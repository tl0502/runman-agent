package db

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type VMConfig struct {
	VMID          string `gorm:"primaryKey"`
	CPU           int
	MemoryMB      int64
	DiskGB        int64
	BandwidthMbps int
	Image         string
	Status        string
	// CreatedAt 是 VM 首次创建时间（订阅起点），用于 rental 流量重置周期。
	// GORM 约定字段名：INSERT 自动填当前时间，UPDATE 保留原值，因此重建不会重置。
	CreatedAt time.Time
}

// PodmanVMConfig 存储 Podman 驱动特有的数据
type PodmanVMConfig struct {
	VMID      string `gorm:"primaryKey"`
	Container string // 容器名
	Cpuset    string // 分配的 cpuset
	MAC       string // 固定 MAC
	IPv4      string // 本地ipv4一定存在
	IPv6      string // 公网ipv6可能存在
}

// CloudHVVMConfig 存储 cloud-hypervisor 驱动特有的数据
type CloudHVVMConfig struct {
	VMID string `gorm:"primaryKey"`
	Idx  int    // IP/TAP 索引，范围 2-4094
	MAC  string // 固定 MAC 地址
	IPv4 string // 静态 IPv4，来自 10.91.0.0/20 段
	IPv6 string // 静态 IPv6（ULA 或公网 /112），可能为空
}

// IncusVMConfig 存储 Incus 驱动特有的数据
type IncusVMConfig struct {
	VMID      string `gorm:"primaryKey"`
	Idx       int    // IP 索引，用于计算静态 IP
	Container string // 容器名
	Image     string // 镜像名
	IPv4      string // 静态 IPv4（纯 IPv6 容器时为空）
	IPv6      string // 静态 IPv6（多地址时取首个，便于兼容旧逻辑/NDP 主地址）
	IPv6s     string // 静态 IPv6 列表，逗号分隔（多地址精细化分配时 >1 个）
	IPv6Only  bool   `gorm:"default:false"` // 是否为纯 IPv6 容器（不分配 IPv4）
}

// VMIPLimit 按 VM 覆盖端口转发唯一来源 IP 数上限。
// 无记录时使用全局配置 MaxForwardIPs；MaxIPs 为 0 表示该 VM 不限制。
type VMIPLimit struct {
	VMID   string `gorm:"primaryKey"`
	MaxIPs int
}

// WGBinding 持久化一条 WireGuard 绑定。每条绑定 = 一个纯用户态 WG 隧道，
// 隧道地址（单个 IPv4 或 IPv6）上收到的全部 TCP/UDP 流量按原端口号转发到该 VM 的内网 IPv4。
// 密钥以 base64 存储，与 wg 配置文件的书写形式一致。
type WGBinding struct {
	ID            string `gorm:"primaryKey"` // uuid
	VMID          string `gorm:"index"`
	Name          string // 面向用户的备注名
	Enabled       bool
	PrivateKey    string // 本端私钥
	Address       string // 隧道地址，单个 IPv4/IPv6
	ListenPort    int    // 本端 WG UDP 监听端口，0 = 随机（仅在配了 Endpoint 时允许）
	MTU           int    // 0 = DefaultMTU
	PeerPublicKey string
	PresharedKey  string // 可空
	Endpoint      string // 对端 host:port，可空表示被动等待对端连入
	AllowedIPs    string // 逗号分隔，空 = 0.0.0.0/0, ::/0
	Keepalive     int    // PersistentKeepalive 秒数，0 = 不发
	CreatedAt     time.Time
}

// PortForward 持久化端口转发规则。(Protocol, HostPort) 联合主键保证宿主机端口唯一。
// BindFamily 决定监听哪些地址族的对外地址：0 = 仅 IPv4（默认，存量行为）、
// 1 = 仅 IPv6（共享 IPv6）、2 = 双栈。
type PortForward struct {
	Protocol    string `gorm:"primaryKey"`
	HostPort    int    `gorm:"primaryKey"`
	VMID        string `gorm:"index"`
	GuestPort   int
	Description string
	BindFamily  int `gorm:"default:0"`
}

type Traffic struct {
	VMID      string    `gorm:"primaryKey"`
	RawIn     int64     // 最后一次采集的网卡计数器值（入站）
	RawOut    int64     // 最后一次采集的网卡计数器值（出站）
	TotalIn   int64     // 累计使用量（字节）
	TotalOut  int64     // 累计使用量（字节）
	Month     string    `gorm:"index"` // YYYY-MM
	MonthIn   int64     // 当月使用量（字节）
	MonthOut  int64     // 当月使用量（字节）
	UpdatedAt time.Time // 最后同步时间
}

// System 存储 Agent 系统级别的元数据，如检测到新版本的时间
type System struct {
	Key       string `gorm:"primaryKey"`
	Value     string
	UpdatedAt time.Time
}

// LoginLog 登录安全事件时间线（统一事件流）。Action 取值：
// login_ok / login_fail / auto_ban / login_blocked / manual_ban / unban。
// Detail 存补充信息（如封禁时长、触发原因），可为空。
type LoginLog struct {
	ID     int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	TS     time.Time `gorm:"index" json:"ts"`
	Action string    `gorm:"index" json:"action"`
	IP     string    `gorm:"index" json:"ip"`
	User   string    `json:"user"` // 尝试的用户名（login_fail 时可能是错误值）
	Detail string    `json:"detail"`
}

// LoginBan 手动封禁记录（应用层：封禁期间该 IP 的登录请求直接被拒）。
// Until 为零值表示永久。自动锁定（内存限速）不入此表，由面板合并展示。
type LoginBan struct {
	IP        string    `gorm:"primaryKey" json:"ip"`
	Source    string    `json:"source"` // manual（当前仅手动封禁入库）
	Until     time.Time `json:"until"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// PatrolEvent 巡查事件日志。Check 取值：
// bandwidth / connections / resources_cpu / resources_mem / resources_disk / process。
// Action 取值：throttle / release / stop_vm / restart_vm / kill_process / alert。
type PatrolEvent struct {
	ID     int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	TS     time.Time `gorm:"index" json:"ts"`
	VMID   string    `gorm:"index" json:"vm_id"`
	Check  string    `json:"check"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
}

// PatrolThrottle 记录某个 VM 当前生效的限速（幂等重放，重启后仍生效）。
// ReleaseAt 到期后自动恢复原上限。VM 停止期间 veth 不存在，tc 无法打上去，
// 由巡查每轮重放，VM 重新拉起后自动补上。
type PatrolThrottle struct {
	VMID      string    `gorm:"primaryKey"`
	RateMbps  int       // 当前限速的绝对速率（Mbps）
	ReleaseAt time.Time // 到期时间
	Reason    string    // 触发原因（bandwidth / connections）
}

// PatrolStop 记录某个 VM 被巡查主动停机（连接超限 / 资源超限关机动作）。
// 这是「当前未解决」状态：VM 恢复运行（用户手动启动）后由巡查每轮清除。
type PatrolStop struct {
	VMID      string `gorm:"primaryKey"`
	Reason    string // bandwidth / connections / resources / process / abuse_udp / abuse_scan / abuse_smtp
	StoppedAt time.Time
}

type DB struct {
	orm *gorm.DB
	// ciMu 序列化自定义镜像列表的读改写，防止并发拉取状态更新互相覆盖
	ciMu sync.Mutex
}

func Init(path string) (*DB, error) {
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{
		Logger: logger.New(log.New(os.Stdout, "\r\n", log.LstdFlags), logger.Config{
			SlowThreshold:             200 * time.Millisecond,
			LogLevel:                  logger.Error,
			IgnoreRecordNotFoundError: true,
			Colorful:                  true,
		}),
	})
	if err != nil {
		return nil, err
	}

	// SQLite 并发安全配置：
	// WAL 模式让读操作不会被写操作阻塞（写时仍可读）；
	// busy_timeout 避免并发写时立即返回 SQLITE_BUSY 错误（等待 5s 再报错）；
	// synchronous=NORMAL 在 WAL 模式下已足够安全，比 FULL 快；
	// SetMaxOpenConns(1) 保证所有 goroutine 串行使用同一个连接，彻底消除并发写冲突。
	db.Exec("PRAGMA journal_mode = WAL")
	db.Exec("PRAGMA busy_timeout = 5000")
	db.Exec("PRAGMA synchronous = NORMAL")
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}

	if err := db.AutoMigrate(&Traffic{}, &VMConfig{}, &PodmanVMConfig{}, &CloudHVVMConfig{}, &IncusVMConfig{}, &PortForward{}, &VMIPLimit{}, &WGBinding{}, &System{}, &LoginLog{}, &LoginBan{}, &PatrolEvent{}, &PatrolThrottle{}, &PatrolStop{}, &WebSession{}); err != nil {
		return nil, fmt.Errorf("automigrate: %w", err)
	}

	// 数据库内含平台 Token、VM 记录与端口转发规则，须收紧为仅属主可读写。
	// 新建时按 0600 落盘；文件已存在（升级场景）也一并校正，避免历史 0644。
	// 注意：SQLite 的 WAL 模式会生成 -wal / -shm 伴生文件，这里一并收紧。
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0600); err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("chmod %s: %w", p, err)
		}
	}

	return &DB{orm: db}, nil
}

// System 元数据

func (d *DB) GetSystem(key string) (string, error) {
	var s System
	err := d.orm.First(&s, "key = ?", key).Error
	if err != nil {
		return "", err
	}
	return s.Value, nil
}

func (d *DB) SetSystem(key, value string) error {
	return d.orm.Save(&System{Key: key, Value: value, UpdatedAt: time.Now()}).Error
}

func (d *DB) GetSystemTime(key string) (time.Time, error) {
	var s System
	err := d.orm.First(&s, "key = ?", key).Error
	if err != nil {
		return time.Time{}, err
	}
	return s.UpdatedAt, nil
}

// 登录安全（事件时间线 + 手动封禁）

const loginLogKeep = 2000 // 事件保留条数，超出自动轮转

// InsertLoginLog 记录一条安全事件并轮转超量历史。
func (d *DB) InsertLoginLog(action, ip, user, detail string) {
	d.orm.Create(&LoginLog{TS: time.Now(), Action: action, IP: ip, User: user, Detail: detail})
	// 轮转：只保留最新 loginLogKeep 条（插入后清理一次，量小成本低）
	d.orm.Where("id NOT IN (?)",
		d.orm.Model(&LoginLog{}).Select("id").Order("id DESC").Limit(loginLogKeep),
	).Delete(&LoginLog{})
}

// ListLoginLog 返回最近的事件，最新在前。
func (d *DB) ListLoginLog(limit int) []LoginLog {
	if limit <= 0 || limit > loginLogKeep {
		limit = 100
	}
	out := []LoginLog{}
	d.orm.Order("id DESC").Limit(limit).Find(&out)
	return out
}

// InsertLoginBan 写入/刷新一条手动封禁。until 为零值表示永久。
func (d *DB) InsertLoginBan(ip, reason string, until time.Time) error {
	return d.orm.Save(&LoginBan{
		IP: ip, Source: "manual", Until: until, Reason: reason, CreatedAt: time.Now(),
	}).Error
}

// GetLoginBan 返回该 IP 当前生效的手动封禁（无或已过期返回 false）。
func (d *DB) GetLoginBan(ip string) (LoginBan, bool) {
	var b LoginBan
	if err := d.orm.First(&b, "ip = ?", ip).Error; err != nil {
		return b, false
	}
	if !b.Until.IsZero() && b.Until.Before(time.Now()) {
		return b, false
	}
	return b, true
}

// DeleteLoginBan 解除手动封禁。
func (d *DB) DeleteLoginBan(ip string) {
	d.orm.Delete(&LoginBan{}, "ip = ?", ip)
}

// ListActiveLoginBans 返回全部未过期的手动封禁。
func (d *DB) ListActiveLoginBans() []LoginBan {
	now := time.Now()
	out := []LoginBan{}
	d.orm.Where("until IS NULL OR until = ? OR until > ?", time.Time{}, now).Order("created_at DESC").Find(&out)
	return out
}

// 巡查事件 + 限速状态

const patrolEventKeep = 2000 // 事件保留条数，超出自动轮转

// InsertPatrolEvent 记录一条巡查事件并轮转超量历史。
func (d *DB) InsertPatrolEvent(vmID, check, action, detail string) {
	d.orm.Create(&PatrolEvent{TS: time.Now(), VMID: vmID, Check: check, Action: action, Detail: detail})
	d.orm.Where("id NOT IN (?)",
		d.orm.Model(&PatrolEvent{}).Select("id").Order("id DESC").Limit(patrolEventKeep),
	).Delete(&PatrolEvent{})
}

// PatrolEventQuery 是巡查事件的筛选/排序条件。
type PatrolEventQuery struct {
	Check   string   // 精确匹配 check（空 = 全部）
	Actions []string // 匹配 action IN (...)；空 = 全部
	VMID    string   // vm_id LIKE 模糊匹配；空 = 全部
	Sort    string   // "asc" 按时间正序，其余（含空）按最新在前
	Since   int64    // unix 秒；>0 时只返回该时间之后的事件（统一事件流的时间筛选）
	Q       string   // 内容搜索：detail / vm_id 模糊匹配；空 = 不筛
}

// QueryPatrolEvents 按条件分页查询巡查事件，返回 items 与总数。
func (d *DB) QueryPatrolEvents(q PatrolEventQuery, offset, limit int) ([]PatrolEvent, int64, error) {
	tx := d.orm.Model(&PatrolEvent{})
	if q.Check != "" {
		// check 是 SQLite 保留字，裸写会语法错误（面板按 check 筛选事件时 500）。
		tx = tx.Where(`"check" = ?`, q.Check)
	}
	if len(q.Actions) > 0 {
		tx = tx.Where("action IN ?", q.Actions)
	}
	if q.VMID != "" {
		tx = tx.Where("vm_id LIKE ?", "%"+q.VMID+"%")
	}
	if q.Since > 0 {
		tx = tx.Where("ts >= ?", time.Unix(q.Since, 0))
	}
	if q.Q != "" {
		like := "%" + q.Q + "%"
		tx = tx.Where("(detail LIKE ? OR vm_id LIKE ?)", like, like)
	}
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	order := "id DESC"
	if q.Sort == "asc" {
		order = "id ASC"
	}
	out := []PatrolEvent{}
	if err := tx.Order(order).Offset(offset).Limit(limit).Find(&out).Error; err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// LoginLogQuery 登录日志查询条件。
type LoginLogQuery struct {
	Actions []string // 匹配 action IN (...)；空 = 全部
	Since   int64    // unix 秒；>0 时只返回该时间之后的事件（统一事件流的时间筛选）
	Q       string   // 内容搜索：detail / ip / user 模糊匹配；空 = 不筛
}

// QueryLoginLogs 按条件分页查询登录日志，返回 items 与总数。
// 与 QueryPatrolEvents 同款风格，供统一事件流归并分页使用。
func (d *DB) QueryLoginLogs(q LoginLogQuery, offset, limit int) ([]LoginLog, int64, error) {
	tx := d.orm.Model(&LoginLog{})
	if len(q.Actions) > 0 {
		tx = tx.Where("action IN ?", q.Actions)
	}
	if q.Since > 0 {
		tx = tx.Where("ts >= ?", time.Unix(q.Since, 0))
	}
	if q.Q != "" {
		like := "%" + q.Q + "%"
		tx = tx.Where("(detail LIKE ? OR ip LIKE ? OR \"user\" LIKE ?)", like, like, like)
	}
	var total int64
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	out := []LoginLog{}
	if err := tx.Order("id DESC").Offset(offset).Limit(limit).Find(&out).Error; err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// SavePatrolThrottle 写入/刷新一条限速状态。
func (d *DB) SavePatrolThrottle(vmID string, rateMbps int, releaseAt time.Time, reason string) error {
	return d.orm.Save(&PatrolThrottle{VMID: vmID, RateMbps: rateMbps, ReleaseAt: releaseAt, Reason: reason}).Error
}

// DeletePatrolThrottle 解除限速状态。
func (d *DB) DeletePatrolThrottle(vmID string) error {
	return d.orm.Delete(&PatrolThrottle{}, "vm_id = ?", vmID).Error
}

// ListPatrolThrottles 返回全部生效中的限速状态。
func (d *DB) ListPatrolThrottles() []PatrolThrottle {
	out := []PatrolThrottle{}
	d.orm.Find(&out)
	return out
}

// WebSession 持久化面板登录会话，使 agent 重启/自动更新后仍保持登录。
// 只保存 token 的 SHA-256（不存明文），LastSeen 用于滑动过期与清理。
type WebSession struct {
	TokenHash string    `gorm:"primaryKey"`
	User      string    `gorm:"index"`
	LastSeen  time.Time `gorm:"index"`
}

// SaveWebSession 写入/刷新一条会话记录。
func (d *DB) SaveWebSession(tokenHash, user string, lastSeen time.Time) error {
	return d.orm.Save(&WebSession{TokenHash: tokenHash, User: user, LastSeen: lastSeen}).Error
}

// TouchWebSession 刷新已存在会话的 last_seen。只 UPDATE 不插入：
// 续期是异步的，可能晚于登出/改密码的删除执行，用 Save 会把已撤销的会话写回去。
func (d *DB) TouchWebSession(tokenHash string, lastSeen time.Time) error {
	return d.orm.Model(&WebSession{}).Where("token_hash = ?", tokenHash).Update("last_seen", lastSeen).Error
}

// DeleteWebSession 删除单个会话（登出）。
func (d *DB) DeleteWebSession(tokenHash string) error {
	return d.orm.Delete(&WebSession{}, "token_hash = ?", tokenHash).Error
}

// DeleteAllWebSessions 清空全部会话（修改密码后强制所有端重新登录）。
func (d *DB) DeleteAllWebSessions() error {
	return d.orm.Where("1 = 1").Delete(&WebSession{}).Error
}

// ListWebSessions 返回全部会话（agent 启动时载入内存）。
func (d *DB) ListWebSessions() []WebSession {
	out := []WebSession{}
	d.orm.Find(&out)
	return out
}

// DeleteExpiredWebSessions 删除 last_seen 早于 before 的会话。
func (d *DB) DeleteExpiredWebSessions(before time.Time) error {
	return d.orm.Where("last_seen < ?", before).Delete(&WebSession{}).Error
}

// SavePatrolStop 写入一条「被巡查停机」状态。
func (d *DB) SavePatrolStop(vmID, reason string) error {
	return d.orm.Save(&PatrolStop{VMID: vmID, Reason: reason, StoppedAt: time.Now()}).Error
}

// DeletePatrolStop 清除「被停机」状态。
func (d *DB) DeletePatrolStop(vmID string) error {
	return d.orm.Delete(&PatrolStop{}, "vm_id = ?", vmID).Error
}

// ListPatrolStops 返回全部「被巡查停机」状态。
func (d *DB) ListPatrolStops() []PatrolStop {
	out := []PatrolStop{}
	d.orm.Find(&out)
	return out
}

// 自定义镜像（仅 podman 驱动使用），JSON 序列化后存于 System KV

const customImagesKey = "custom_images"

// CustomImage 拉取状态
const (
	CustomImagePulling = "pulling"
	CustomImageReady   = "ready"
	CustomImageError   = "error"
)

type CustomImage struct {
	ID      string `json:"id"`   // 完整镜像引用，如 docker.io/user/image:tag
	Name    string `json:"name"` // 面向用户的显示名
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
	AddedAt int64  `json:"added_at"`
}

func (d *DB) loadCustomImagesLocked() []CustomImage {
	raw, err := d.GetSystem(customImagesKey)
	if err != nil || raw == "" {
		return nil
	}
	var list []CustomImage
	if json.Unmarshal([]byte(raw), &list) != nil {
		return nil
	}
	return list
}

func (d *DB) saveCustomImagesLocked(list []CustomImage) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return d.SetSystem(customImagesKey, string(raw))
}

func (d *DB) ListCustomImages() []CustomImage {
	d.ciMu.Lock()
	defer d.ciMu.Unlock()
	return d.loadCustomImagesLocked()
}

// UpsertCustomImage 新增或更新一条自定义镜像记录（按 ID 匹配）。
func (d *DB) UpsertCustomImage(img CustomImage) error {
	d.ciMu.Lock()
	defer d.ciMu.Unlock()
	list := d.loadCustomImagesLocked()
	for i := range list {
		if list[i].ID == img.ID {
			list[i] = img
			return d.saveCustomImagesLocked(list)
		}
	}
	return d.saveCustomImagesLocked(append(list, img))
}

func (d *DB) DeleteCustomImage(id string) error {
	d.ciMu.Lock()
	defer d.ciMu.Unlock()
	list := d.loadCustomImagesLocked()
	out := list[:0]
	for _, img := range list {
		if img.ID != id {
			out = append(out, img)
		}
	}
	return d.saveCustomImagesLocked(out)
}

// UpdateCustomImageStatus 更新指定镜像的拉取状态；记录不存在时（如拉取期间被删除）静默忽略。
func (d *DB) UpdateCustomImageStatus(id, status, errMsg string) error {
	d.ciMu.Lock()
	defer d.ciMu.Unlock()
	list := d.loadCustomImagesLocked()
	for i := range list {
		if list[i].ID == id {
			list[i].Status = status
			list[i].Error = errMsg
			return d.saveCustomImagesLocked(list)
		}
	}
	return nil
}

// VM 核心业务配置

func (d *DB) SaveVMConfig(v *VMConfig) error {
	return d.orm.Save(v).Error
}

func (d *DB) GetVMConfig(vmId string) (*VMConfig, error) {
	var conf VMConfig
	err := d.orm.First(&conf, "vm_id = ?", vmId).Error
	if err != nil {
		return nil, err
	}
	return &conf, nil
}

func (d *DB) ListVMConfigs() ([]*VMConfig, error) {
	var list []*VMConfig
	err := d.orm.Find(&list).Error
	return list, err
}

func (d *DB) DeleteVMConfig(vmId string) error {
	return d.orm.Delete(&VMConfig{}, "vm_id = ?", vmId).Error
}

// 端口转发

func (d *DB) SavePortForward(pf *PortForward) error {
	return d.orm.Save(pf).Error
}

func (d *DB) DeletePortForward(protocol string, hostPort int) error {
	return d.orm.Delete(&PortForward{}, "protocol = ? AND host_port = ?", protocol, hostPort).Error
}

func (d *DB) ListPortForwards(vmId string) ([]*PortForward, error) {
	var list []*PortForward
	err := d.orm.Where("vm_id = ?", vmId).Find(&list).Error
	return list, err
}

func (d *DB) ListAllPortForwards() ([]*PortForward, error) {
	var list []*PortForward
	err := d.orm.Find(&list).Error
	return list, err
}

func (d *DB) DeletePortForwardsForVM(vmId string) error {
	return d.orm.Delete(&PortForward{}, "vm_id = ?", vmId).Error
}

// WireGuard 绑定

func (d *DB) SaveWGBinding(b *WGBinding) error {
	return d.orm.Save(b).Error
}

func (d *DB) GetWGBinding(id string) (*WGBinding, error) {
	var b WGBinding
	err := d.orm.First(&b, "id = ?", id).Error
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (d *DB) DeleteWGBinding(id string) error {
	return d.orm.Delete(&WGBinding{}, "id = ?", id).Error
}

func (d *DB) ListWGBindings(vmId string) ([]*WGBinding, error) {
	var list []*WGBinding
	err := d.orm.Where("vm_id = ?", vmId).Order("created_at").Find(&list).Error
	return list, err
}

func (d *DB) ListAllWGBindings() ([]*WGBinding, error) {
	var list []*WGBinding
	err := d.orm.Order("created_at").Find(&list).Error
	return list, err
}

func (d *DB) DeleteWGBindingsForVM(vmId string) error {
	return d.orm.Delete(&WGBinding{}, "vm_id = ?", vmId).Error
}

// 端口转发唯一来源 IP 数限制（按 VM 覆盖）

func (d *DB) SaveVMIPLimit(vmId string, maxIPs int) error {
	return d.orm.Save(&VMIPLimit{VMID: vmId, MaxIPs: maxIPs}).Error
}

func (d *DB) DeleteVMIPLimit(vmId string) error {
	return d.orm.Delete(&VMIPLimit{}, "vm_id = ?", vmId).Error
}

func (d *DB) ListVMIPLimits() ([]*VMIPLimit, error) {
	var list []*VMIPLimit
	err := d.orm.Find(&list).Error
	return list, err
}

// 流量

func (d *DB) DeleteTraffic(vmId string) error {
	return d.orm.Delete(&Traffic{}, "vm_id = ?", vmId).Error
}

func (d *DB) GetTraffic(vmId string) (*Traffic, error) {
	var t Traffic
	err := d.orm.First(&t, "vm_id = ?", vmId).Error
	return &t, err
}

func (d *DB) UpdateTraffic(vmId string, rawIn, rawOut int64, month string) (totalIn, totalOut, monthIn, monthOut int64, err error) {
	var t Traffic
	err = d.orm.First(&t, "vm_id = ?", vmId).Error
	if err != nil {
		// 首次看到该 VM，将当前流量作为初始值计入统计
		t = Traffic{
			VMID:     vmId,
			RawIn:    rawIn,
			RawOut:   rawOut,
			TotalIn:  rawIn,
			TotalOut: rawOut,
			Month:    month,
			MonthIn:  rawIn,
			MonthOut: rawOut,
		}
		d.orm.Create(&t)
		return t.TotalIn, t.TotalOut, t.MonthIn, t.MonthOut, nil
	}

	// 计算增量
	diffIn := rawIn - t.RawIn
	if diffIn < 0 {
		// 计数器重置（如容器/宿主机重启），将当前 raw 值全量计入增量
		diffIn = rawIn
	}
	diffOut := rawOut - t.RawOut
	if diffOut < 0 {
		diffOut = rawOut
	}

	t.RawIn = rawIn
	t.RawOut = rawOut
	t.TotalIn += diffIn
	t.TotalOut += diffOut

	if t.Month != month {
		// 跨月重置
		t.Month = month
		t.MonthIn = diffIn
		t.MonthOut = diffOut
	} else {
		t.MonthIn += diffIn
		t.MonthOut += diffOut
	}

	d.orm.Save(&t)
	return t.TotalIn, t.TotalOut, t.MonthIn, t.MonthOut, nil
}

// SaveTraffic 直接保存或更新 Traffic 记录（用于流量统计服务）
func (d *DB) SaveTraffic(t *Traffic) error {
	return d.orm.Save(t).Error
}

// ResetTrafficMonth 重置虚拟机的当月流量为0
func (d *DB) ResetTrafficMonth(vmId string) error {
	return d.orm.Model(&Traffic{}).Where("vm_id = ?", vmId).Updates(map[string]interface{}{
		"month_in":  0,
		"month_out": 0,
	}).Error
}

// Podman数据结构

func (d *DB) SavePodmanConfig(v *PodmanVMConfig) error {
	return d.orm.Save(v).Error
}

func (d *DB) GetPodmanConfig(vmId string) (*PodmanVMConfig, error) {
	var conf PodmanVMConfig
	err := d.orm.First(&conf, "vm_id = ?", vmId).Error
	if err != nil {
		return nil, err
	}
	return &conf, nil
}

func (d *DB) DeletePodmanConfig(vmId string) error {
	return d.orm.Delete(&PodmanVMConfig{}, "vm_id = ?", vmId).Error
}
func (d *DB) ListPodmanConfigs() ([]*PodmanVMConfig, error) {
	var list []*PodmanVMConfig
	err := d.orm.Find(&list).Error
	return list, err
}

// cloud-hypervisor 数据结构

func (d *DB) SaveCloudHVConfig(v *CloudHVVMConfig) error {
	return d.orm.Save(v).Error
}

func (d *DB) GetCloudHVConfig(vmId string) (*CloudHVVMConfig, error) {
	var conf CloudHVVMConfig
	err := d.orm.First(&conf, "vm_id = ?", vmId).Error
	if err != nil {
		return nil, err
	}
	return &conf, nil
}

func (d *DB) DeleteCloudHVConfig(vmId string) error {
	return d.orm.Delete(&CloudHVVMConfig{}, "vm_id = ?", vmId).Error
}

func (d *DB) ListCloudHVConfigs() ([]*CloudHVVMConfig, error) {
	var list []*CloudHVVMConfig
	err := d.orm.Find(&list).Error
	return list, err
}

// Incus 数据结构

func (d *DB) SaveIncusConfig(v *IncusVMConfig) error {
	return d.orm.Save(v).Error
}

func (d *DB) GetIncusConfig(vmId string) (*IncusVMConfig, error) {
	var conf IncusVMConfig
	err := d.orm.First(&conf, "vm_id = ?", vmId).Error
	if err != nil {
		return nil, err
	}
	return &conf, nil
}

func (d *DB) DeleteIncusConfig(vmId string) error {
	return d.orm.Delete(&IncusVMConfig{}, "vm_id = ?", vmId).Error
}

func (d *DB) ListIncusConfigs() ([]*IncusVMConfig, error) {
	var list []*IncusVMConfig
	err := d.orm.Find(&list).Error
	return list, err
}

// NextIncusIdx 返回最小可用的 idx（范围 2-4094）
func (d *DB) NextIncusIdx() (int, error) {
	var configs []*IncusVMConfig
	if err := d.orm.Find(&configs).Error; err != nil {
		return 2, err
	}

	used := make(map[int]bool, len(configs))
	for _, c := range configs {
		used[c.Idx] = true
	}

	for idx := 2; idx <= 4094; idx++ {
		if !used[idx] {
			return idx, nil
		}
	}
	return -1, gorm.ErrRecordNotFound
}

// NextCloudHVIdx 返回最小可用的 idx（范围 2-4094）
func (d *DB) NextCloudHVIdx() (int, error) {
	var configs []*CloudHVVMConfig
	if err := d.orm.Find(&configs).Error; err != nil {
		return 2, err
	}

	used := make(map[int]bool, len(configs))
	for _, c := range configs {
		used[c.Idx] = true
	}

	for idx := 2; idx <= 4094; idx++ {
		if !used[idx] {
			return idx, nil
		}
	}
	return -1, gorm.ErrRecordNotFound // 没有可用的 idx
}
