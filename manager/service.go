package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"runman-agent/db"
	"runman-agent/proto/agent"
)

// VMService 是 VMManager 的服务层包装，位于 gRPC 处理层与底层虚拟化驱动之间，负责：
//  1. 将 platform 下发的 serverVmID 转换为底层虚拟化驱动实际使用的 localID
//  2. ListVMs 只返回本节点 DB 中已登记的 VM，过滤用户自行创建的容器/虚拟机
//  3. 业务编排：调用驱动执行生命周期管理，并补全 DB 配置
type VMService struct {
	mgr VMManager
	db  *db.DB

	// OnCreated 在 VM 成功创建并写入 DB 后被调用，可用于自动添加端口转发等。
	// bandwidthMbps 为 0 时表示不限速。
	OnCreated func(ctx context.Context, vmID string, bandwidthMbps int)
}

func NewVMService(mgr VMManager, database *db.DB) *VMService {
	return &VMService{mgr: mgr, db: database}
}

// --- 生命周期管理 ---

func (s *VMService) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	log.Printf("[CreateVM] start: vmID=%q, image=%q", req.VmId, req.OsImage)
	if err := s.mgr.CreateVM(ctx, req); err != nil {
		return err
	}

	// 驱动层已经负责将驱动特定配置（如 Podman 的 IP/MAC/Cpuset）存入数据库。
	// 这里补全业务层的通用配置。
	conf, _ := s.db.GetVMConfig(req.VmId)
	if conf == nil {
		conf = &db.VMConfig{VMID: req.VmId}
	}
	conf.BandwidthMbps = int(req.BandwidthMbps)
	conf.CPU = int(req.Cpu)
	conf.MemoryMB = req.RamMb
	conf.DiskGB = req.DiskGb
	conf.Image = req.OsImage
	conf.Status = "running"
	// CreatedAt 锚定首次创建时间（订阅起点）。重建走驱动层 ReinstallVM，不经过
	// 这里，因此 DB 里已有的 CreatedAt 会原样保留，rental 流量周期不被重置。
	if conf.CreatedAt.IsZero() {
		conf.CreatedAt = time.Now()
	}
	log.Printf("[CreateVM] saving VMConfig: vmID=%s, image=%s", conf.VMID, conf.Image)
	if err := s.db.SaveVMConfig(conf); err != nil {
		log.Printf("[CreateVM] SaveVMConfig error: %v", err)
		return err
	}

	if s.OnCreated != nil {
		s.OnCreated(ctx, req.VmId, int(req.BandwidthMbps))
	}

	// 出方向套餐上限：netavark 只装下载方向（宿主 veth root tbf），
	// 容器内 egress 在此补齐，使「带宽上限」真实约束双向流量。
	if conf.BandwidthMbps > 0 {
		s.ensureEgressCap(ctx, req.VmId, int64(conf.BandwidthMbps)*1000000)
	}
	return nil
}

func (s *VMService) StartVM(ctx context.Context, vmID string) error {
	log.Printf("[StartVM] vmID=%s", vmID)
	err := s.mgr.StartVM(ctx, vmID)
	if err != nil {
		log.Printf("[StartVM] error: vmID=%s, err=%v", vmID, err)
	} else {
		log.Printf("[StartVM] success: vmID=%s", vmID)
		// 容器 netns 随重启重建，容器内出方向限速丢失，按套餐重装
		// （下载方向由 netavark 在 start 时自动恢复，无需处理）。
		if conf, cfgErr := s.db.GetVMConfig(vmID); cfgErr == nil && conf != nil && conf.BandwidthMbps > 0 {
			s.ensureEgressCap(ctx, vmID, int64(conf.BandwidthMbps)*1000000)
		}
	}
	return err
}

func (s *VMService) StopVM(ctx context.Context, vmID string, force bool) error {
	forceStr := "graceful"
	if force {
		forceStr = "force"
	}
	log.Printf("[StopVM] vmID=%s, mode=%s", vmID, forceStr)
	err := s.mgr.StopVM(ctx, vmID, force)
	if err != nil {
		log.Printf("[StopVM] error: vmID=%s, mode=%s, err=%v", vmID, forceStr, err)
	} else {
		log.Printf("[StopVM] success: vmID=%s, mode=%s", vmID, forceStr)
	}
	return err
}

func (s *VMService) RestartVM(ctx context.Context, vmID string) error {
	log.Printf("[RestartVM] vmID=%s", vmID)
	err := s.mgr.RestartVM(ctx, vmID)
	if err != nil {
		log.Printf("[RestartVM] error: vmID=%s, err=%v", vmID, err)
	} else {
		log.Printf("[RestartVM] success: vmID=%s", vmID)
		// 重启同样重建 netns，出方向限速按套餐重装（巡查 throttle 未到期时
		// 由 patrol 的限速重放覆盖回来）。
		if conf, cfgErr := s.db.GetVMConfig(vmID); cfgErr == nil && conf != nil && conf.BandwidthMbps > 0 {
			s.ensureEgressCap(ctx, vmID, int64(conf.BandwidthMbps)*1000000)
		}
	}
	return err
}

func (s *VMService) DeleteVM(ctx context.Context, vmID string) error {
	log.Printf("[DeleteVM] vmID=%s", vmID)
	err := s.mgr.DeleteVM(ctx, vmID)
	if err != nil {
		log.Printf("[DeleteVM] error: vmID=%s, err=%v", vmID, err)
	} else {
		log.Printf("[DeleteVM] success: vmID=%s", vmID)
	}
	return err
}

// --- 配置与维护 ---

func (s *VMService) ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error {
	// 驱动层会处理特定驱动配置的复用与重新分配，并负责保存 VMConfig
	if err := s.mgr.ReinstallVM(ctx, req); err != nil {
		return err
	}
	// 重建后的容器 netns 是新的，按套餐重装出方向限速
	if conf, err := s.db.GetVMConfig(req.VmId); err == nil && conf != nil && conf.BandwidthMbps > 0 {
		s.ensureEgressCap(ctx, req.VmId, int64(conf.BandwidthMbps)*1000000)
	}
	return nil
}

// EnsureEgressTbf 转发到驱动的出方向限速能力。仅 podman 实现 EgressShaper；
// 其他驱动没有 tbf 语义，静默跳过（行为与历史一致，只依赖 netavark/systemd 的下载侧）。
func (s *VMService) EnsureEgressTbf(ctx context.Context, vmID string, rateBps int64) error {
	if es, ok := s.mgr.(EgressShaper); ok {
		return es.EnsureEgressTbf(ctx, vmID, rateBps)
	}
	return nil
}

// ensureEgressCap 按套餐在容器内补装出方向限速。失败只记日志不阻塞生命周期操作：
// 下载方向不受影响，上传缺口由启动期全量补装兜底。
func (s *VMService) ensureEgressCap(ctx context.Context, vmID string, rateBps int64) {
	if err := s.EnsureEgressTbf(ctx, vmID, rateBps); err != nil {
		log.Printf("[EgressTbf] %s: %v", vmID, err)
	}
}

func (s *VMService) ResetPassword(ctx context.Context, vmID, password string) error {
	return s.mgr.ResetPassword(ctx, vmID, password)
}

// --- 信息查询 ---

func (s *VMService) GetVMInfo(ctx context.Context, vmID string) (*agent.VMSummary, error) {
	info, err := s.mgr.GetVMInfo(ctx, vmID)
	if err != nil {
		return nil, err
	}
	info.VmId = vmID
	return info, nil
}

// GetVMCreatedAt 返回 VM 的首次创建时间（订阅起点），用于面板展示与 rental 流量周期。
// 优先读 DB 的稳定锚点（重建不会改）；存量 VM 在字段引入前没有该值，回落驱动的
// inspect.Created 并写回 DB 完成一次性迁移，之后不再漂移。
func (s *VMService) GetVMCreatedAt(ctx context.Context, vmID string) (time.Time, error) {
	conf, err := s.db.GetVMConfig(vmID)
	if err == nil && conf != nil && !conf.CreatedAt.IsZero() {
		return conf.CreatedAt, nil
	}

	created, err := s.mgr.GetVMCreatedAt(ctx, vmID)
	if err != nil || created.IsZero() {
		return created, err
	}

	// 存量迁移：把当前容器创建时间固化为锚点。用 Save 前重新读取，避免覆盖
	// 期间被别的流程更新的业务字段。
	if cur, e := s.db.GetVMConfig(vmID); e == nil && cur != nil && cur.CreatedAt.IsZero() {
		cur.CreatedAt = created
		if e := s.db.SaveVMConfig(cur); e != nil {
			log.Printf("[GetVMCreatedAt] persist CreatedAt for %s failed: %v", vmID, e)
		}
	}
	return created, nil
}

func (s *VMService) ListVMs(ctx context.Context) ([]*agent.VMSummary, error) {
	all, err := s.mgr.ListVMs(ctx)
	if err != nil {
		return nil, err
	}

	// 仅返回 DB 中存在的 VM，排除非平台管理的实例。
	// DB 出错时返回错误而非空列表，避免心跳上报 0 个 VM（会触发后端误判逻辑）。
	configs, err := s.db.ListVMConfigs()
	if err != nil {
		return nil, fmt.Errorf("list VM configs: %w", err)
	}
	registered := make(map[string]bool, len(configs))
	for _, c := range configs {
		registered[c.VMID] = true
	}

	var result []*agent.VMSummary
	for _, vm := range all {
		if registered[vm.VmId] {
			result = append(result, vm)
		}
	}
	return result, nil
}

// AttachTTY 将终端附接请求转发给底层驱动。
func (s *VMService) AttachTTY(ctx context.Context, vmID string, stdin io.Reader, stdout io.Writer, resize <-chan ResizeEvent) error {
	return s.mgr.AttachTTY(ctx, vmID, stdin, stdout, resize)
}

// --- 其他 ---

func (s *VMService) GetVMLocalIP(ctx context.Context, vmID string) (string, error) {
	return s.mgr.GetVMLocalIP(ctx, vmID)
}

func (s *VMService) GetSupportedImages(ctx context.Context) ([]*agent.OSImageInfo, error) {
	return s.mgr.GetSupportedImages(ctx)
}

// SupportsCustomImages 返回底层驱动是否支持自定义镜像（即实现了 ImagePuller）。
func (s *VMService) SupportsCustomImages() bool {
	_, ok := s.mgr.(ImagePuller)
	return ok
}

// StartCustomImagePull 后台拉取自定义镜像并更新其在 DB 中的状态。
func (s *VMService) StartCustomImagePull(ref string) {
	puller, ok := s.mgr.(ImagePuller)
	if !ok {
		_ = s.db.UpdateCustomImageStatus(ref, db.CustomImageError, "driver does not support custom images")
		return
	}
	go func() {
		log.Printf("[CustomImage] pulling %s ...", ref)
		if err := puller.PullImage(context.Background(), ref); err != nil {
			log.Printf("[CustomImage] pull %s failed: %v", ref, err)
			_ = s.db.UpdateCustomImageStatus(ref, db.CustomImageError, err.Error())
			return
		}
		log.Printf("[CustomImage] pull %s done", ref)
		_ = s.db.UpdateCustomImageStatus(ref, db.CustomImageReady, "")
	}()
}

// ResumeCustomImagePulls 恢复因 agent 重启而中断的拉取任务（启动时调用一次）。
func (s *VMService) ResumeCustomImagePulls() {
	for _, img := range s.db.ListCustomImages() {
		if img.Status == db.CustomImagePulling {
			s.StartCustomImagePull(img.ID)
		}
	}
}

// GetVMNetStats 获取 VM 的网络流量统计（用于流量统计服务）
func (s *VMService) GetVMNetStats(ctx context.Context, vmID string) (*VMNetStats, error) {
	return s.mgr.GetVMNetStats(ctx, vmID)
}

// Autostart 启动所有状态为 running 的 VM（通常在 agent 启动时调用）
func (s *VMService) Autostart(ctx context.Context) {
	configs, err := s.db.ListVMConfigs()
	if err != nil {
		log.Printf("[Autostart] failed to list VM configs: %v", err)
		return
	}
	for _, c := range configs {
		if c.Status == "running" {
			if err := s.mgr.StartVM(ctx, c.VMID); err != nil {
				log.Printf("[Autostart] failed to start VM %s: %v", c.VMID, err)
			} else {
				log.Printf("[Autostart] started VM %s", c.VMID)
			}
		}
	}
}

// Cleanup 执行幽灵实例清理
func (s *VMService) Cleanup(ctx context.Context) error {
	return s.mgr.Cleanup(ctx)
}

// ─── 镜像清单（上架管理）────────────────────────────────────────────────────
//
// 内置镜像与自定义镜像统一成一张清单：每项有上架开关与顺序，配置存
// System KV「os_images_config」（JSON 数组，顺序即展示顺序）。对租户/
// 平台可见的镜像 = 上架 且 镜像就绪（自定义镜像需 ready）。旧格式
// （"debian,alpine" 类别名列表）在读取时惰性迁移为镜像 ID 列表。

const osImagesConfigKey = "os_images_config"

// imageSetting 是镜像清单中一项的上架配置。
type imageSetting struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// ImageConfigItem 是镜像清单的管理视图：包含已下架/拉取中/拉取失败的项，
// 供面板展示与编辑；对租户可见的列表请用 BuildImageList。
type ImageConfigItem struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`             // builtin | custom
	Status  string `json:"status,omitempty"` // 自定义镜像拉取状态
	Enabled bool   `json:"enabled"`
}

// loadImageSettings 读取上架配置。旧格式（类别名逗号分隔）按类别匹配到
// 具体内置镜像 ID 后迁移并回写，避免每次读取都走迁移路径。
func loadImageSettings(d *db.DB, builtin []*agent.OSImageInfo) []imageSetting {
	raw, err := d.GetSystem(osImagesConfigKey)
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil
	}
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "[") {
		var list []imageSetting
		if json.Unmarshal([]byte(raw), &list) == nil {
			return list
		}
		return nil
	}
	// 旧格式："debian,alpine"（类别名）→ 匹配具体镜像 ID（保持原顺序）
	var migrated []imageSetting
	for _, part := range strings.Split(raw, ",") {
		cat := strings.ToLower(strings.TrimSpace(part))
		if cat == "" {
			continue
		}
		for _, img := range builtin {
			if strings.Contains(strings.ToLower(img.Id), cat) {
				migrated = append(migrated, imageSetting{ID: img.Id, Enabled: true})
				break
			}
		}
	}
	if b, mErr := json.Marshal(migrated); mErr == nil {
		_ = d.SetSystem(osImagesConfigKey, string(b))
	}
	return migrated
}

// ListImageConfigs 返回完整镜像清单（管理视图）：内置镜像 + 全部自定义镜像
// （含 pulling/error），按配置顺序排列，未配置的镜像默认上架追加末尾。
func ListImageConfigs(d *db.DB, builtin []*agent.OSImageInfo) []ImageConfigItem {
	settings := loadImageSettings(d, builtin)
	builtinByID := make(map[string]*agent.OSImageInfo, len(builtin))
	for _, img := range builtin {
		builtinByID[img.Id] = img
	}
	customByID := make(map[string]db.CustomImage)
	for _, ci := range d.ListCustomImages() {
		customByID[ci.ID] = ci
	}

	var items []ImageConfigItem
	seen := make(map[string]bool)
	add := func(id string, s *imageSetting) {
		if seen[id] {
			return
		}
		if b, ok := builtinByID[id]; ok {
			seen[id] = true
			enabled := true
			if s != nil {
				enabled = s.Enabled
			}
			items = append(items, ImageConfigItem{ID: id, Name: b.Name, Kind: "builtin", Enabled: enabled})
			return
		}
		if ci, ok := customByID[id]; ok {
			seen[id] = true
			name := ci.Name
			if name == "" {
				name = ci.ID
			}
			enabled := true
			if s != nil {
				enabled = s.Enabled
			}
			items = append(items, ImageConfigItem{ID: id, Name: name, Kind: "custom", Status: ci.Status, Enabled: enabled})
			return
		}
		// 配置里残留的、已不存在的镜像：忽略
	}
	for i := range settings {
		add(settings[i].ID, &settings[i])
	}
	// 未配置的镜像默认上架（新增内置镜像 / 新加自定义镜像自动可用）
	for _, img := range builtin {
		add(img.Id, nil)
	}
	for _, ci := range d.ListCustomImages() {
		add(ci.ID, nil)
	}
	return items
}

// SaveImageConfigs 全量保存镜像清单（顺序即数组顺序）。
func SaveImageConfigs(d *db.DB, items []ImageConfigItem) error {
	seen := make(map[string]bool, len(items))
	settings := make([]imageSetting, 0, len(items))
	for _, it := range items {
		id := strings.TrimSpace(it.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		settings = append(settings, imageSetting{ID: id, Enabled: it.Enabled})
	}
	b, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	return d.SetSystem(osImagesConfigKey, string(b))
}

// RemoveImageSetting 从清单移除一个镜像（自定义镜像被删除时调用），
// 配置中不存在时静默返回。
func RemoveImageSetting(d *db.DB, builtin []*agent.OSImageInfo, id string) error {
	settings := loadImageSettings(d, builtin)
	out := settings[:0]
	for _, s := range settings {
		if s.ID != id {
			out = append(out, s)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return d.SetSystem(osImagesConfigKey, string(b))
}

// BuildImageList 返回对租户/平台可见的镜像列表：按配置顺序输出「上架且就绪」
// 的镜像；自定义镜像仅在 ready 时进入。
func BuildImageList(d *db.DB, builtin []*agent.OSImageInfo) []*agent.OSImageInfo {
	var out []*agent.OSImageInfo
	for _, it := range ListImageConfigs(d, builtin) {
		if !it.Enabled {
			continue
		}
		if it.Kind == "custom" && it.Status != db.CustomImageReady {
			continue
		}
		out = append(out, &agent.OSImageInfo{Id: it.ID, Name: it.Name})
	}
	return out
}
