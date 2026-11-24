//go:build containers_image_openpgp

package podman

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runman-agent/manager/podman/cpualloc"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/containers/podman/v5/libpod/define"
	"github.com/containers/podman/v5/pkg/api/handlers"
	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/bindings/images"
	entTypes "github.com/containers/podman/v5/pkg/domain/entities/types"
	"github.com/containers/podman/v5/pkg/specgen"
	"github.com/containers/podman/v5/pkg/util"
	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/opencontainers/runtime-spec/specs-go"
	netTypes "go.podman.io/common/libnetwork/types"
	"go.podman.io/image/v5/manifest"
	stypes "go.podman.io/storage/types"

	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/proto/agent"
)

const (
	SocketPath  = "unix:///run/podman/podman.sock"
	NetworkName = "narwhal-net"
)

// netdevErrLoggedAt 记录上次 /proc/<pid>/net/dev 读取失败的时间（unix 秒）。
// ListVMs 每次列表请求都会尝试采样，失败时限频记录，避免刷屏。
var netdevErrLoggedAt atomic.Int64

func ptr[T any](v T) *T { return &v }

type Manager struct {
	ctx   context.Context
	db    *db.DB
	cfg   *config.Manager
	alloc *cpualloc.Allocator
	mu    sync.Mutex // 全局锁，序列化所有容器生命周期操作，防止 IP/Cpuset 分配冲突
}

func New(database *db.DB, cfg *config.Manager) (*Manager, error) {
	alloc := cpualloc.New(runtime.NumCPU())
	if vmConfigs, err2 := database.ListPodmanConfigs(); err2 == nil {
		for _, c := range vmConfigs {
			alloc.Restore(c.Cpuset)
		}
	}
	// 用 Background context 建立长期连接，避免 ping 超时取消后续所有操作
	ctx, err := bindings.NewConnection(context.Background(), SocketPath)
	if err != nil {
		return nil, err
	}
	// 用独立超时 context 验证连通性
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err = bindings.GetClient(pingCtx); err != nil {
		return nil, err
	}

	return &Manager{ctx: ctx, db: database, cfg: cfg, alloc: alloc}, nil
}

func (m *Manager) timeoutCtx() context.Context {
	ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
	_ = cancel // timer goroutine exits when timeout fires; acceptable for short-lived ops
	return ctx
}

// LockNetworkMigration keeps IP allocation, start/stop, deletion and cleanup out
// of a network snapshot/rebuild. Read-only IP lookup remains available to rebind
// forwarding rules after the containers have reconnected.
func (m *Manager) LockNetworkMigration() func() {
	m.mu.Lock()
	return m.mu.Unlock
}

func buildResourceConfig(cpu int64, ramMb int64, cpuset string, pidsLimit int64) specgen.ContainerResourceConfig {
	res := &specs.LinuxResources{}
	if ramMb > 0 {
		res.Memory = &specs.LinuxMemory{
			Limit:            ptr(ramMb * 1024 * 1024),
			DisableOOMKiller: ptr(false),
		}
	}
	if cpu > 0 {
		res.CPU = &specs.LinuxCPU{
			Quota:  ptr(cpu * 100000),
			Period: ptr(uint64(100000)),
			Cpus:   cpuset,
		}
	}
	if pidsLimit > 0 {
		res.Pids = &specs.LinuxPids{Limit: pidsLimit}
	}
	return specgen.ContainerResourceConfig{
		ResourceLimits: res,
		Rlimits: []specs.POSIXRlimit{
			{Type: "nofile", Soft: 65535, Hard: 65535},
			{Type: "nproc", Soft: 65535, Hard: 65535},
		},
	}
}

func lxcfsMounts() []specs.Mount {
	paths := []struct{ dst, src string }{
		{"/proc/cpuinfo", "/var/lib/lxcfs/proc/cpuinfo"},
		{"/proc/meminfo", "/var/lib/lxcfs/proc/meminfo"},
		{"/proc/diskstats", "/var/lib/lxcfs/proc/diskstats"},
		{"/proc/stat", "/var/lib/lxcfs/proc/stat"},
		{"/proc/swaps", "/var/lib/lxcfs/proc/swaps"},
		{"/proc/uptime", "/var/lib/lxcfs/proc/uptime"},
		{"/proc/loadavg", "/var/lib/lxcfs/proc/loadavg"},
		{"/sys/devices/system/cpu", "/var/lib/lxcfs/sys/devices/system/cpu"},
		{"/proc/slabinfo", "/var/lib/lxcfs/proc/slabinfo"},
	}
	mounts := make([]specs.Mount, 0, len(paths))
	for _, p := range paths {
		mounts = append(mounts, specs.Mount{
			Destination: p.dst,
			Type:        "bind",
			Source:      p.src,
			Options:     []string{"rw", "bind"},
		})
	}
	return mounts
}

// userNamespace 根据配置返回容器的 user namespace 设置。
// 开启时用 userns=auto（rootful 自动分配 subuid 映射，容器内 root ≠ 宿主 root）；
// 关闭时返回零值（默认 host，容器 root == 宿主 root）。
func userNamespace(cfg config.Config) specgen.Namespace {
	if cfg.UsernsEnabled() {
		return specgen.Namespace{NSMode: specgen.Auto}
	}
	return specgen.Namespace{}
}

// userNamespaceIDMapping 返回 auto userns 所需的 ID 映射配置。
// specgen 的 UserNS=auto 在 bindings 创建路径下不会自动生成 uid/gid 映射
// （只有 CLI 的 FillOutSpecGen 才会补 IDMappings），必须显式设置 AutoUserNs，
// libpod 才会从 containers 用户的 subuid/subgid 池分配映射。
func userNamespaceIDMapping(cfg config.Config) *stypes.IDMappingOptions {
	if !cfg.UsernsEnabled() {
		return nil
	}
	opts, err := util.ParseIDMapping("auto", nil, nil, "", "")
	if err != nil {
		log.Printf("[Podman] parse userns=auto idmapping failed: %v", err)
		return nil
	}
	return opts
}

// createWithRetry 创建容器；若名字被残留容器占用（包括仅存在于 storage 层、
// libpod DB 不认识的 "external" 容器，重装/创建中途失败后可能残留），
// 强制清理同名容器后重试一次。
func (m *Manager) createWithRetry(spec *specgen.SpecGenerator) (entTypes.ContainerCreateResponse, error) {
	res, err := containers.CreateWithSpec(m.timeoutCtx(), spec, nil)
	if err != nil && (strings.Contains(err.Error(), "already in use") || strings.Contains(err.Error(), "exists") || strings.Contains(err.Error(), "conflict")) {
		log.Printf("[Podman] create %s: name in use by stale container, force removing and retrying: %v", spec.Name, err)
		if rmErr := m.deleteByID(spec.Name); rmErr != nil {
			log.Printf("[Podman] create %s: stale container removal failed: %v", spec.Name, rmErr)
		}
		res, err = containers.CreateWithSpec(m.timeoutCtx(), spec, nil)
	}
	return res, err
}

func (m *Manager) CreateVM(ctx context.Context, req *agent.CmdCreateVM) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 平台断流后可能重发同一条创建命令。不拦住的话会走到 createWithRetry 的
	// 名称冲突分支，把正在运行的同名实例连同数据一起删掉。
	if exists, err := containers.Exists(m.timeoutCtx(), req.VmId, nil); err != nil {
		return fmt.Errorf("check existing container %s: %w", req.VmId, err)
	} else if exists {
		return fmt.Errorf("VM %s already exists", req.VmId)
	}
	return m.createVM(ctx, req)
}

func (m *Manager) createVM(_ context.Context, req *agent.CmdCreateVM) error {
	// 限制最低配置：CPU 1核，内存 64MB，磁盘 1GB
	if req.Cpu < 1 {
		req.Cpu = 1
	}
	if req.RamMb < 64 {
		req.RamMb = 64
	}
	if req.DiskGb < 1 {
		req.DiskGb = 1
	}

	// 安全/隔离配置快照（cap、pids、no-new-privs）
	sec := config.Config{}
	if m.cfg != nil {
		sec = m.cfg.Get()
	}

	// 1. 拉取镜像（大镜像首次拉取可能很慢，使用独立的长超时）
	if err := m.PullImage(context.Background(), req.OsImage); err != nil {
		return err
	}

	// 2. 确定 IP 和 MAC
	var macAddr, cpuSet string
	var ipv4, ipv6 net.IP
	if conf, err := m.db.GetPodmanConfig(req.VmId); err == nil && conf.IPv4 != "" {
		// 复用已有配置
		ipv4 = net.ParseIP(conf.IPv4)
		if conf.IPv6 != "" {
			ipv6 = net.ParseIP(conf.IPv6)
		}
		macAddr = conf.MAC
		cpuSet = conf.Cpuset
		log.Printf("[CreateVM] %s: reuse stored config ipv4=%s ipv6=%s mac=%s cpuset=%s", req.VmId, conf.IPv4, conf.IPv6, conf.MAC, conf.Cpuset)
	} else {
		log.Printf("[CreateVM] %s: no stored config (err=%v), creating temp container to get IP", req.VmId, err)
		// 创建临时容器获取 IPAM 分配的地址
		tempID := "tmp-" + req.VmId
		netOpts := map[string]netTypes.PerNetworkOptions{
			NetworkName: {},
		}
		res, err := m.createWithRetry(&specgen.SpecGenerator{
			ContainerBasicConfig: specgen.ContainerBasicConfig{
				Name:     tempID,
				Hostname: tempID,
				Remove:   ptr(true),
			},
			ContainerStorageConfig: specgen.ContainerStorageConfig{
				Image: req.OsImage,
			},
			ContainerNetworkConfig: specgen.ContainerNetworkConfig{
				Networks: netOpts,
			},
			ContainerHealthCheckConfig: specgen.ContainerHealthCheckConfig{
				HealthConfig:         &manifest.Schema2HealthConfig{Test: []string{"NONE"}},
				HealthLogDestination: "/tmp",
			},
		})
		if err != nil {
			log.Printf("[CreateVM] %s: temp container create failed: %v", req.VmId, err)
		} else {
			log.Printf("[CreateVM] %s: temp container created id=%s", req.VmId, res.ID)
			// 启动并获取信息
			if startErr := containers.Start(m.timeoutCtx(), res.ID, nil); startErr != nil {
				log.Printf("[CreateVM] %s: temp container start failed: %v", req.VmId, startErr)
			} else {
				inspect, _ := containers.Inspect(m.timeoutCtx(), res.ID, nil)
				ipv4, ipv6, err = m.getIPsFromInspect(inspect)
				if err != nil {
					log.Printf("[CreateVM] %s: getIPsFromInspect failed: %v", req.VmId, err)
				} else {
					log.Printf("[CreateVM] %s: got ipv4=%s ipv6=%s from temp container", req.VmId, ipv4, ipv6)
					for _, ins := range inspect.NetworkSettings.Networks {
						if macAddr == "" {
							macAddr = ins.MacAddress
						}
					}
					log.Printf("[CreateVM] %s: got mac=%s from temp container", req.VmId, macAddr)
				}
			}
			// 删除临时容器
			_ = m.deleteByID(res.ID)
			log.Printf("[CreateVM] %s: temp container deleted", req.VmId)
		}
	}

	// 3. 分配 cpuset
	alloc := false
	if cpuSet == "" && req.Cpu > 0 {
		cpuSet, _ = m.alloc.Allocate(int(req.Cpu))
		alloc = true
	}

	// 如果最终拿到了地址，则注入到创建参数中
	netOpt := netTypes.PerNetworkOptions{}
	if ipv4 != nil {
		netOpt.StaticIPs = append(netOpt.StaticIPs, ipv4)
		log.Printf("[CreateVM] %s: will use static ipv4=%s", req.VmId, ipv4)
	} else {
		log.Printf("[CreateVM] %s: WARNING no static IPv4 will be set, IP may change on restart", req.VmId)
	}
	if ipv6 != nil {
		netOpt.StaticIPs = append(netOpt.StaticIPs, ipv6)
		log.Printf("[CreateVM] %s: will use static ipv6=%s", req.VmId, ipv6)
	}
	if macAddr != "" {
		if hwAddr, err := net.ParseMAC(macAddr); err == nil {
			netOpt.StaticMAC = netTypes.HardwareAddr(hwAddr)
		}
	}

	// 限速选项
	if req.BandwidthMbps > 0 {
		rate := int64(req.BandwidthMbps) * 1000000
		burst := rate / 10
		if burst < 1000000 {
			burst = 1000000
		}
		netOpt.Options = map[string]string{
			"bandwidth_rate":    fmt.Sprintf("%d", rate),
			"bandwidth_burst":   fmt.Sprintf("%d", burst),
			"bandwidth_latency": "50",
		}
	}

	netOpts := map[string]netTypes.PerNetworkOptions{
		NetworkName: netOpt,
	}

	res, err := m.createWithRetry(&specgen.SpecGenerator{
		ContainerBasicConfig: specgen.ContainerBasicConfig{
			Name:          req.VmId,
			Terminal:      ptr(true),
			Stdin:         ptr(true),
			RestartPolicy: "unless-stopped",
			Hostname:      req.VmId,
			Systemd:       "true",
		},
		ContainerStorageConfig: specgen.ContainerStorageConfig{
			Image: req.OsImage,
			StorageOpts: map[string]string{
				"size": fmt.Sprintf("%dG", req.DiskGb),
			},
			Mounts: lxcfsMounts(),
		},
		ContainerSecurityConfig: specgen.ContainerSecurityConfig{
			CapDrop:         sec.DropCaps(),
			CapAdd:          sec.AddCaps(),
			NoNewPrivileges: ptr(sec.NoNewPrivsEnabled()),
			UserNS:          userNamespace(sec),
			IDMappings:      userNamespaceIDMapping(sec),
		},
		ContainerNetworkConfig: specgen.ContainerNetworkConfig{
			Networks:   netOpts,
			DNSServers: []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2606:4700:4700::1111")},
		},
		ContainerResourceConfig: buildResourceConfig(int64(req.Cpu), req.RamMb, cpuSet, sec.PidsLimit()),
		ContainerHealthCheckConfig: specgen.ContainerHealthCheckConfig{
			HealthConfig: &manifest.Schema2HealthConfig{
				Test:    []string{"NONE"},
				Timeout: 30 * time.Second,
			},
			HealthLogDestination: "/tmp",
		},
	})

	if err != nil {
		if alloc {
			m.alloc.Release(cpuSet)
		}
		return err
	}
	defer func() {
		if err != nil {
			_ = m.deleteByID(res.ID)
		}
	}()

	if err = containers.Start(m.timeoutCtx(), res.ID, nil); err != nil {
		return err
	}

	// 在正式容器启动后，如果 ipAdds 或 macAddr 为空（例如临时容器获取失败），从正式容器重新获取并更新到数据库
	if ipv4 == nil || macAddr == "" {
		inspect, _ := containers.Inspect(m.timeoutCtx(), res.ID, nil)
		if ipv4 == nil {
			ipv4, ipv6, _ = m.getIPsFromInspect(inspect)
		}
		if macAddr == "" {
			for _, netw := range inspect.NetworkSettings.Networks {
				if netw.MacAddress != "" {
					macAddr = netw.MacAddress
					break
				}
			}
		}
	}

	// 成功后确保保存配置到数据库
	bizConf, err := m.db.GetVMConfig(req.VmId)
	if err != nil {
		bizConf = &db.VMConfig{VMID: req.VmId}
	}
	bizConf.CPU = int(req.Cpu)
	bizConf.MemoryMB = req.RamMb
	bizConf.DiskGB = req.DiskGb
	bizConf.BandwidthMbps = int(req.BandwidthMbps)
	bizConf.Image = req.OsImage
	_ = m.db.SaveVMConfig(bizConf)

	// Podman 驱动特定配置
	pConf, err := m.db.GetPodmanConfig(req.VmId)
	if err != nil {
		pConf = &db.PodmanVMConfig{VMID: req.VmId}
	}
	pConf.Container = req.VmId
	pConf.IPv4 = ipv4.String()
	if ipv6 != nil {
		pConf.IPv6 = ipv6.String()
	}
	pConf.MAC = macAddr
	pConf.Cpuset = cpuSet
	_ = m.db.SavePodmanConfig(pConf)

	// 设置 root 密码
	return m.setRootPassword(res.ID, req.RootPassword)
}

// setRootPassword 设置容器 root 密码。密码按 shell 单引号规则转义后传入：
// 直接拼接时，含单引号的合法密码会破坏命令语法。
func (m *Manager) setRootPassword(id, password string) error {
	if strings.ContainsAny(password, "\n\r\x00") {
		return fmt.Errorf("password must not contain line breaks")
	}
	quoted := "'" + strings.ReplaceAll("root:"+password, "'", `'\''`) + "'"
	return m.execInContainer(id, []string{"bash", "-c", "printf '%s\\n' " + quoted + " | chpasswd"})
}

func (m *Manager) execInContainer(id string, cmd []string) error {
	execID, err := containers.ExecCreate(m.timeoutCtx(), id, &handlers.ExecCreateConfig{
		ExecOptions: dockerContainer.ExecOptions{
			AttachStdout: true,
			AttachStderr: true,
			Cmd:          cmd,
		},
	})
	if err != nil {
		return err
	}
	return containers.ExecStart(m.timeoutCtx(), execID, nil)
}

func emptyDir(dir string) {
	if dir == "" {
		return
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

func (m *Manager) deleteByID(id string) error {
	name := id
	var upperDir string

	// 1. 获取容器名称、状态及 UpperDir（可写层路径）
	if info, err := containers.Inspect(m.timeoutCtx(), id, nil); err == nil {
		name = info.Name
		if info.GraphDriver != nil && info.GraphDriver.Data != nil {
			upperDir = info.GraphDriver.Data["UpperDir"]
		}
		if info.State != nil && info.State.Running {
			_ = containers.Kill(m.timeoutCtx(), id, &containers.KillOptions{Signal: ptr("SIGKILL")})
		}
	} else {
		log.Printf("[Podman] deleteByID %s: inspect failed (%v), removing anyway in case of storage-only container", id, err)
	}

	// 2. 删除前尝试从 DB 获取 cpuset 并释放
	if conf, err := m.db.GetPodmanConfig(name); err == nil && conf.Cpuset != "" {
		m.alloc.Release(conf.Cpuset)
		_ = m.db.DeletePodmanConfig(name)
	} else if name != id {
		if conf, err := m.db.GetPodmanConfig(id); err == nil && conf.Cpuset != "" {
			m.alloc.Release(conf.Cpuset)
			_ = m.db.DeletePodmanConfig(id)
		}
	}

	// 3. 尝试常规删除
	_, err := containers.Remove(m.timeoutCtx(), name, &containers.RemoveOptions{Force: ptr(true), Ignore: ptr(true)})
	if err != nil && upperDir != "" {
		// 4. 若删除失败（如 XFS project quota 写满导致 ENOSPC），清空 diff 目录释放配额并重试
		log.Printf("[Podman] deleteByID %s: remove failed (%v), emptying diff to release quota: %s", name, err, upperDir)
		emptyDir(upperDir)
		_, err = containers.Remove(m.timeoutCtx(), name, &containers.RemoveOptions{Force: ptr(true), Ignore: ptr(true)})
	}

	// 5. 若 bindings 仍报错，使用命令行强制清除 storage 兜底
	if err != nil {
		log.Printf("[Podman] deleteByID %s: fallback to podman rm --storage --force", name)
		if fbErr := exec.Command("podman", "rm", "-f", "--storage", name).Run(); fbErr != nil {
			// 兜底也失败且容器确实还在：必须报错，不能让调用方以为已删除。
			if exists, _ := containers.Exists(m.timeoutCtx(), name, nil); exists {
				return fmt.Errorf("remove container %s: %w", name, err)
			}
		}
	}
	return nil
}

func (m *Manager) StartVM(_ context.Context, vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.startVM(vmID)
}

func (m *Manager) startVM(vmID string) error {
	return containers.Start(m.timeoutCtx(), vmID, nil)
}

func (m *Manager) StopVM(_ context.Context, vmID string, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopVM(vmID, force)
}

func (m *Manager) stopVM(vmID string, force bool) error {
	if force {
		return containers.Kill(m.timeoutCtx(), vmID, &containers.KillOptions{Signal: ptr("SIGKILL")})
	}
	return containers.Stop(m.timeoutCtx(), vmID, nil)
}

func (m *Manager) RestartVM(_ context.Context, vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return containers.Restart(m.timeoutCtx(), vmID, nil)
}

func (m *Manager) DeleteVM(ctx context.Context, vmID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.deleteVM(ctx, vmID)
}

func (m *Manager) deleteVM(ctx context.Context, vmID string) error {
	// 先删容器再删 DB 记录：容器删除失败时记录保留，平台重试即可；
	// 反过来的话容器还在、记录已丢，实例就成了没人管的残留。
	if err := m.deleteByID(vmID); err != nil {
		return err
	}
	if err := m.db.DeleteVMConfig(vmID); err != nil {
		log.Printf("[Podman][DeleteVM] error: failed to delete VMConfig for %s: %v", vmID, err)
		return fmt.Errorf("delete VMConfig: %w", err)
	}
	return nil
}

func (m *Manager) ReinstallVM(ctx context.Context, req *agent.CmdReinstallVM) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	log.Printf("[Podman][Reinstall] start: vmID=%s image=%s diskGb=%d", req.VmId, req.OsImage, req.DiskGb)

	// 1. 强制停止旧容器
	// 先拉镜像再动旧实例：镜像不存在或下载失败时旧系统必须原样保留。
	if err := m.PullImage(ctx, req.OsImage); err != nil {
		return fmt.Errorf("pull image %s: %w", req.OsImage, err)
	}
	if err := m.stopVM(req.VmId, true); err != nil {
		log.Printf("[Podman][Reinstall] %s: stop failed (continuing): %v", req.VmId, err)
	}

	// 2. 备份原有的 PodmanConfig 配置（如 IPv4, IPv6, MAC, Cpuset），重装时复用，避免产生临时容器和 IP 变动
	savedPodmanConf, _ := m.db.GetPodmanConfig(req.VmId)

	// 3. 删除旧容器实例与存储（使用具备自动解死锁的 deleteByID 进行清理）
	if err := m.deleteByID(req.VmId); err != nil {
		log.Printf("[Podman][Reinstall] %s: deleteByID failed (continuing, create will retry on name conflict): %v", req.VmId, err)
	}

	// 4. 如果之前有配置，恢复到 DB，确保 createVM 直接复用，无需走 tmp- 容器
	if savedPodmanConf != nil {
		_ = m.db.SavePodmanConfig(savedPodmanConf)
		if savedPodmanConf.Cpuset != "" {
			m.alloc.Restore(savedPodmanConf.Cpuset)
		}
	}

	// 5. 调用 createVM 创建全新的容器实例
	return m.createVM(ctx, &agent.CmdCreateVM{
		VmId:          req.VmId,
		OsImage:       req.OsImage,
		RootPassword:  req.RootPassword,
		Cpu:           req.Cpu,
		RamMb:         req.RamMb,
		DiskGb:        req.DiskGb,
		BandwidthMbps: req.BandwidthMbps,
	})
}

func (m *Manager) ResetPassword(_ context.Context, vmID, password string) error {
	// 密码重设通常涉及 exec，并不会导致并发资源冲突，但为了严谨也可以加锁或不加
	return m.setRootPassword(vmID, password)
}

func (m *Manager) GetVMLocalIP(_ context.Context, vmID string) (string, error) {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, nil)
	if err != nil {
		return "", err
	}
	ipv4, _, err := m.getIPsFromInspect(inspect)
	if err != nil {
		return "", err
	}
	return ipv4.String(), nil
}

// GetVMCreatedAt 返回容器的真实创建时间（来自 podman inspect 的 Created 字段）。
func (m *Manager) GetVMCreatedAt(_ context.Context, vmID string) (time.Time, error) {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, nil)
	if err != nil {
		return time.Time{}, err
	}
	return inspect.Created, nil
}

func (m *Manager) GetSupportedImages(_ context.Context) ([]*agent.OSImageInfo, error) {
	return []*agent.OSImageInfo{
		{Id: "docker.io/narwhalcloud/debian:podman", Name: "Debian (Podman)"},
		{Id: "docker.io/narwhalcloud/alpine:podman", Name: "Alpine (Podman)"},
	}, nil
}

// PullImage 拉取镜像。使用独立的 15 分钟超时（默认 timeoutCtx 只有 1 分钟，
// 自定义大镜像首次拉取必然超时）。调用方 ctx 为 nil 时仅使用内部超时。
func (m *Manager) PullImage(_ context.Context, ref string) error {
	pullCtx, cancel := context.WithTimeout(m.ctx, 15*time.Minute)
	defer cancel()
	_, err := images.Pull(pullCtx, ref, &images.PullOptions{
		Policy: ptr("newer"),
	})
	return err
}

func (m *Manager) GetVMInfo(ctx context.Context, vmID string) (*agent.VMSummary, error) {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, nil)
	if err != nil {
		return nil, err
	}
	var ips []string
	ipv4, ipv6, _ := m.getIPsFromInspect(inspect)
	if ipv4 != nil {
		ips = append(ips, ipv4.String())
	}
	if ipv6 != nil {
		ips = append(ips, ipv6.String())
	}
	cpuPct, memUsed, netIn, netOut, _ := m.getUsage(ctx, vmID)
	return &agent.VMSummary{
		VmId:            vmID,
		Status:          mapStatus(inspect.State.Status),
		CpuPct:          cpuPct,
		RamUsedMb:       memUsed / 1024 / 1024,
		TrafficInBytes:  netIn,
		TrafficOutBytes: netOut,
		Ips:             ips,
	}, nil
}

func (m *Manager) getUsage(_ context.Context, vmID string) (cpuPct float32, memUsed, netIn, netOut int64, err error) {
	statsCtx, cancel := context.WithCancel(m.timeoutCtx())
	defer cancel()

	statsCh, err := containers.Stats(statsCtx, []string{vmID}, &containers.StatsOptions{
		All:      ptr(false),
		Stream:   ptr(true),
		Interval: ptr(1),
	})
	if err != nil {
		return
	}

	// 跳过第一个采样（基准值），取第二个计算速率。
	// 注意：podman 5.4.x 的 stats 流实测可能一个样本都不返回、也不关闭
	// channel，`for range` 会永久阻塞并拖死整个巡查循环（ListVMs 卡在
	// WaitGroup 上，其他 VM 一起停摆）。逐次接收加超时，拿不到就放弃本轮。
	samples := 0
	timeout := time.After(5 * time.Second)
	for {
		select {
		case report, ok := <-statsCh:
			if !ok {
				goto netdevFallback
			}
			if samples == 0 {
				samples++
				continue
			}
			for _, s := range report.Stats {
				for name, n := range s.Network {
					if name == "lo" {
						continue
					}
					netIn += int64(n.RxBytes)
					netOut += int64(n.TxBytes)
				}
				cpuPct = float32(s.CPU)
				memUsed = int64(s.MemUsage)
				cancel()
				go func() {
					for range statsCh {
					}
				}()
				goto netdevFallback
			}
			goto netdevFallback
		case <-timeout:
			goto netdevFallback
		}
	}

netdevFallback:
	// 兼容 Podman 4.x (如 Debian 12 官方源默认版本)：
	// Podman 4.x 的 stats 响应中网络统计仅存在于顶层 NetInput/NetOutput 字段，无 Network 字典，
	// 导致 s.Network 为空，netIn 与 netOut 仍为 0。
	// 此时通过读取容器 network namespace 对应的 /proc/<pid>/net/dev 获取真实累计网络流量。
	if netIn == 0 && netOut == 0 {
		if in, out, pErr := m.getProcNetDev(vmID); pErr == nil {
			netIn = in
			netOut = out
		}
	}

	return
}

// readProcNetDev 从指定 PID 的 /proc/<pid>/net/dev 汇总非 lo 接口的累计字节。
// PID 必须是容器主进程在宿主 PID 命名空间里的 PID。
func readProcNetDev(pid int) (in, out int64, err error) {
	if pid <= 0 {
		return 0, 0, fmt.Errorf("invalid pid %d", pid)
	}
	file, err := os.Open(fmt.Sprintf("/proc/%d/net/dev", pid))
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.Contains(line, ":") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		ifName := strings.TrimSpace(parts[0])
		if ifName == "lo" {
			continue
		}

		fields := strings.Fields(parts[1])
		// fields[0]: rx_bytes (容器入站 InBytes)
		// fields[8]: tx_bytes (容器出站 OutBytes)
		if len(fields) >= 9 {
			if rx, e := strconv.ParseInt(fields[0], 10, 64); e == nil {
				in += rx
			}
			if tx, e := strconv.ParseInt(fields[8], 10, 64); e == nil {
				out += tx
			}
		}
	}
	return in, out, scanner.Err()
}

// getProcNetDev 按 container id 取进程 PID 后读取 /proc/<pid>/net/dev（Podman 4.x 兼容回退）。
func (m *Manager) getProcNetDev(vmID string) (in, out int64, err error) {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, nil)
	if err != nil {
		return 0, 0, err
	}
	if inspect.State == nil || inspect.State.Pid <= 0 {
		return 0, 0, fmt.Errorf("container %s is not running", vmID)
	}
	return readProcNetDev(inspect.State.Pid)
}

func (m *Manager) ListVMs(ctx context.Context) ([]*agent.VMSummary, error) {
	list, err := containers.List(m.timeoutCtx(), &containers.ListOptions{All: ptr(true)})
	if err != nil {
		return nil, err
	}

	summaries := make([]*agent.VMSummary, len(list))
	inspectErrs := make([]error, len(list))
	var wg sync.WaitGroup
	for i, c := range list {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			inspect, err2 := containers.Inspect(m.timeoutCtx(), id, nil)
			if err2 != nil {
				// 容器在 List 之后被删属正常竞态，跳过；其余失败不能静默丢掉这台 VM，
				// 否则调用方会把「没读到」当成「已删除」。
				if !strings.Contains(err2.Error(), "no such container") {
					inspectErrs[i] = fmt.Errorf("inspect %s: %w", id, err2)
				}
				return
			}
			var ips []string
			ipv4, ipv6, _ := m.getIPsFromInspect(inspect)
			if ipv4 != nil {
				ips = append(ips, ipv4.String())
			}
			if ipv6 != nil {
				ips = append(ips, ipv6.String())
			}
			s := &agent.VMSummary{
				VmId:   inspect.Name,
				Status: mapStatus(inspect.State.Status),
				Ips:    ips,
			}
			if inspect.State.Running {
				cpuPct, memUsed, _, _, _ := m.getUsage(ctx, inspect.Name)
				s.CpuPct = cpuPct
				s.RamUsedMb = memUsed / 1024 / 1024
				// 网络累计字节直接读 /proc/<pid>/net/dev，不重复 Inspect：
				// 这里已经拿到了 inspect，直接用它带的 PID。
				if netIn, netOut, pErr := readProcNetDev(inspect.State.Pid); pErr == nil {
					s.TrafficInBytes = netIn
					s.TrafficOutBytes = netOut
				} else if now := time.Now().Unix(); now-netdevErrLoggedAt.Load() > 300 {
					// 限频 5 分钟，避免每轮列表请求刷屏
					netdevErrLoggedAt.Store(now)
					log.Printf("[Podman] read netdev for %s (pid %d) failed: %v", inspect.Name, inspect.State.Pid, pErr)
				}
			}
			summaries[i] = s
		}(i, c.ID)
	}
	wg.Wait()
	for _, e := range inspectErrs {
		if e != nil {
			return nil, e
		}
	}

	var result []*agent.VMSummary
	for _, s := range summaries {
		if s != nil {
			result = append(result, s)
		}
	}
	return result, nil
}

func (m *Manager) AttachTTY(ctx context.Context, vmID string, stdin io.Reader, stdout io.Writer, resize <-chan manager.ResizeEvent) error {
	execID, err := containers.ExecCreate(m.timeoutCtx(), vmID, &handlers.ExecCreateConfig{
		ExecOptions: dockerContainer.ExecOptions{
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			Tty:          true,
			Cmd:          []string{"/bin/bash"},
		},
	})
	if err != nil {
		return err
	}

	// 合并 podman 连接 context 与请求取消信号：
	// 请求关闭时取消 attach，但保持底层 podman 连接可用。
	attachCtx, cancel := context.WithCancel(m.ctx)
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-attachCtx.Done():
		}
	}()
	defer cancel()

	// 处理终端尺寸变更
	go func() {
		for rs := range resize {
			h := int(rs.Rows)
			w := int(rs.Cols)
			_ = containers.ResizeExecTTY(m.ctx, execID,
				new(containers.ResizeExecTTYOptions).WithHeight(h).WithWidth(w))
		}
	}()

	return containers.ExecStartAndAttach(attachCtx, execID,
		new(containers.ExecStartAndAttachOptions).
			WithAttachInput(true).
			WithAttachOutput(true).
			WithAttachError(true).
			WithInputStream(*bufio.NewReader(stdin)).
			WithOutputStream(stdout).
			WithErrorStream(stdout),
	)
}

// isPublicIPv6 判断地址是否可作为公网 IPv6 上报给平台。
// 共享 IPv6（snat）下容器只有 ULA（fd91:...），公网不可路由，不应进入
// VMSummary.ips；subnet 模式下容器持有公网 IPv6，正常上报。
func isPublicIPv6(ip net.IP) bool {
	if ip == nil || ip.To4() != nil {
		return false
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	addr = addr.Unmap()
	if !addr.Is6() || addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() {
		return false
	}
	b := addr.As16()
	return b[0]&0xfe != 0xfc // 排除 ULA fc00::/7
}

// FilterReportableIPs 去掉不能作为公网入口上报给平台的地址（ULA / 链路本地 / 回环）。
// 共享 IPv6（snat）下容器只持有 ULA，平台展示会给客户「可用」的错觉；
// subnet 模式下容器持有公网 IPv6，会被保留。
// 只在心跳上报前调用——巡查等内部逻辑需要完整地址（含 ULA）。
func FilterReportableIPs(ips []string) []string {
	out := make([]string, 0, len(ips))
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		if ip.To4() != nil || isPublicIPv6(ip) {
			out = append(out, s)
		}
	}
	return out
}

func (m *Manager) getIPsFromInspect(inspect *define.InspectContainerData) (ipv4, ipv6 net.IP, err error) {
	for _, ins := range inspect.NetworkSettings.Networks {
		ipv4 = net.ParseIP(ins.IPAddress)
		ipv6 = net.ParseIP(ins.GlobalIPv6Address)
	}
	if ipv4 == nil && ipv6 == nil {
		return nil, nil, fmt.Errorf("no ipv4 or ipv6 network found")
	}
	return ipv4, ipv6, nil
}

// ListProcesses 枚举容器内进程（供巡查黑名单匹配）。
// 全程在宿主机侧读 /proc，不依赖容器内 ps/kill——精简镜像（distroless）与
// 容器内 rootkit（ld.so.preload 劫持 ps）都不影响结果。归属判定：与容器
// init 进程同 PID 命名空间即属该容器，因此也不受 cgroup v1/v2 布局差异影响。
func (m *Manager) ListProcesses(_ context.Context, vmID string) ([]manager.Process, error) {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, nil)
	if err != nil {
		return nil, err
	}
	if inspect.State == nil {
		return nil, fmt.Errorf("container %s has no runtime state", vmID)
	}
	if inspect.State.Paused {
		// 冻结中的容器 SIGKILL 不会立即生效，枚举出来只会产生「杀了没死」的假事件。
		// 必须在 Running 判断之前：paused 时 State.Running 为 false，先判 Running
		// 会把它误报成「未运行」，排查时看不到真实原因。
		return nil, fmt.Errorf("container %s is paused", vmID)
	}
	if !inspect.State.Running || inspect.State.Pid <= 0 {
		return nil, fmt.Errorf("container %s is not running", vmID)
	}
	initPID := inspect.State.Pid
	ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", initPID))
	if err != nil {
		return nil, err
	}
	// 容器与宿主共享 PID 命名空间时（--pid=host），归属判定失效，会把宿主
	// 所有进程都当成容器进程。宁可报错跳过，也不能误枚举、误杀。
	if hostNS, e := os.Readlink("/proc/1/ns/pid"); e == nil && hostNS == ns {
		return nil, fmt.Errorf("container %s shares host PID namespace", vmID)
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	procs := make([]manager.Process, 0, 32)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		// 先比命名空间：绝大多数宿主进程在这一步被排除。
		link, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", pid))
		if err != nil || link != ns {
			continue
		}
		// 同命名空间不等于是本容器：--pid=container:X 的两个容器共享 PID
		// 命名空间但 cgroup 不同，必须再用 cgroup 子树排除，否则会杀到
		// 别的容器的进程。init 的 cgroup 读不到时退回纯 ns 判定。
		if !inSameCgroup(pid, initPID) {
			continue
		}
		state, comm, nsPID, err := readProcStatus(pid)
		if err != nil || state == 'Z' || state == 'X' || state == 'x' {
			continue // 已退出待回收 / 正在销毁的进程杀之无益
		}
		if nsPID <= 0 {
			continue
		}
		procs = append(procs, manager.Process{
			PID:     nsPID,
			HostPID: pid,
			Comm:    comm,
			Exe:     readProcExe(pid),
			Args:    readProcCmdline(pid),
			Init:    nsPID == 1,
			Ns:      ns,
		})
	}
	if len(procs) == 0 {
		// 运行中的容器至少有一个 init 进程；空结果说明命名空间/cgroup
		// 判定失效，必须报错让调用方记日志，而不是当作「没有进程」静默跳过。
		return nil, fmt.Errorf("no process found in container %s (pid namespace/cgroup mismatch?)", vmID)
	}
	return procs, nil
}

// cgroupPaths 读取 /proc/<pid>/cgroup 里的层级路径，跳过根 cgroup（"/"）。
func cgroupPaths(pid int) []string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		// v1: hierarchy-ID:controllers:path；v2: 0::path
		i := strings.LastIndex(line, ":")
		if i < 0 {
			continue
		}
		p := strings.TrimSpace(line[i+1:])
		if p == "" || p == "/" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// inSameCgroup 判断 pid 是否位于容器 init 的 cgroup 子树内。
// 这是 PID 命名空间判定的补充：共享命名空间（--pid=container:X）的容器
// 必须靠 cgroup 区分；嵌套命名空间里的进程仍会被漏掉（需要 CAP_SYS_ADMIN，
// 当前容器创建参数不授予），属已知边界。
func inSameCgroup(pid, initPID int) bool {
	parents := cgroupPaths(initPID)
	if len(parents) == 0 {
		return true // init 的 cgroup 不可读（罕见），退回 ns 判定避免巡查整体失效
	}
	for _, p := range cgroupPaths(pid) {
		for _, parent := range parents {
			if p == parent || strings.HasPrefix(p, parent+"/") {
				return true
			}
		}
	}
	return false
}

// readProcStatus 解析 /proc/<pid>/status，返回进程状态、comm 与容器内 PID。
// NSpid 是「宿主 → 各层命名空间」的 PID 链，第 2 项即容器内 PID。
func readProcStatus(pid int) (state byte, comm string, nsPID int, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, "", 0, err
	}
	return parseProcStatus(string(data))
}

// parseProcStatus 是 readProcStatus 的纯解析部分，便于单测。
func parseProcStatus(data string) (state byte, comm string, nsPID int, err error) {
	for _, line := range strings.Split(data, "\n") {
		switch {
		case strings.HasPrefix(line, "Name:"):
			// comm 可以被进程自己改写，甚至塞入换行伪造后续行；只认第一处，
			// 后出现的同名行一律忽略。
			if comm == "" {
				comm = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			}
		case strings.HasPrefix(line, "State:") && state == 0:
			if f := strings.Fields(line); len(f) >= 2 && f[1] != "" {
				state = f[1][0]
			}
		case strings.HasPrefix(line, "NSpid:") && nsPID == 0:
			fields := strings.Fields(strings.TrimPrefix(line, "NSpid:"))
			if len(fields) >= 2 {
				nsPID, _ = strconv.Atoi(fields[1]) // 第 2 项 = 容器命名空间视角
			} else if len(fields) == 1 {
				nsPID, _ = strconv.Atoi(fields[0])
			}
		}
	}
	if comm == "" {
		return 0, "", 0, fmt.Errorf("no Name in status")
	}
	return state, comm, nsPID, nil
}

// readProcCmdline 返回完整命令行（NUL 分隔转空格）。
func readProcCmdline(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(data), "\x00", " "))
}

// readProcExe 返回可执行文件真实路径。进程启动后文件被删时 readlink 会带
// " (deleted)" 后缀，剥掉才能与关键词正常比对。
func readProcExe(pid int) string {
	exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(exe, " (deleted)")
}

// KillProcess 从宿主机直接对容器内进程发 SIGKILL，不走容器内 kill：
// 精简镜像没有 kill，容器内 rootkit 也可能拦截。杀前复核 PID 命名空间，
// 防止枚举到查杀之间宿主 PID 被复用、误杀别的进程。
func (m *Manager) KillProcess(_ context.Context, p manager.Process) error {
	if p.HostPID <= 0 {
		return fmt.Errorf("invalid host pid %d", p.HostPID)
	}
	ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", p.HostPID))
	if err != nil {
		return err // 进程已退出
	}
	if p.Ns != "" && ns != p.Ns {
		return fmt.Errorf("host pid %d reused by another namespace", p.HostPID)
	}
	return syscall.Kill(p.HostPID, syscall.SIGKILL)
}

// GetVMDiskUsage 返回容器的磁盘占用（overlay 可写层 + 镜像层，字节）。
// 必须显式请求 size：containers.Inspect 不传 InspectOptions 时 podman 不计算体积，
// SizeRootFs 恒为 0，磁盘巡查会永远不触发（实测 --size 下为 287MB、不请求时为 0）。
func (m *Manager) GetVMDiskUsage(_ context.Context, vmID string) (int64, error) {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, new(containers.InspectOptions).WithSize(true))
	if err != nil {
		return 0, err
	}
	return inspect.SizeRootFs, nil
}

// GetVMIP 返回容器的内网 IPv4（优先），无 IPv4 时尝试 IPv6。
func (m *Manager) GetVMIP(_ context.Context, vmID string) (string, error) {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, nil)
	if err != nil {
		return "", err
	}
	ipv4, ipv6, err := m.getIPsFromInspect(inspect)
	if err != nil {
		return "", err
	}
	if ipv4 != nil {
		return ipv4.String(), nil
	}
	return ipv6.String(), nil
}

// GetVMVeth 返回容器在宿主机侧的 veth 接口名，用于 tc 限速。
// 原理：宿主侧 veth 的 /sys/class/net/<veth>/iflink 等于容器内某接口的 ifindex。
// nsenter 只切 network namespace，sh/cat 由宿主机提供，因此不依赖容器镜像内的工具；
// 读取容器 netns 内全部非 lo 接口的 ifindex（不写死 eth0），兼容自定义接口名/IPv6-only。
func (m *Manager) GetVMVeth(_ context.Context, vmID string) (string, error) {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, nil)
	if err != nil {
		return "", err
	}
	if inspect.State == nil || inspect.State.Pid <= 0 {
		return "", fmt.Errorf("container %s is not running", vmID)
	}
	// host / none / container 网络没有独立的 veth pair：这类容器的 netns 里能看到
	// 宿主机（或另一容器）的全部接口，按 ifindex 反查会误中无关 veth 并限错对象，
	// 因此直接拒绝限速（调用方会记为「tc 暂未生效」）。
	if inspect.HostConfig != nil {
		switch inspect.HostConfig.NetworkMode {
		case "host", "none", "container":
			return "", fmt.Errorf("container %s uses %s network, no independent veth to throttle", vmID, inspect.HostConfig.NetworkMode)
		}
	}
	pid := inspect.State.Pid

	nsCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// 必须用 ip(8) 而不是读 /sys/class/net/<iface>/ifindex：nsenter -n 只切换
	// network namespace、不切换 mount namespace，/sys 仍是宿主机的 sysfs，
	// 读到的会是宿主接口而不是容器接口（宿主机往往没有 eth0）。
	// ip 走 netlink，netlink 随 netns 隔离，nsenter 后查询的就是容器内的接口。
	out, err := exec.CommandContext(nsCtx, "nsenter", "-t", strconv.Itoa(pid), "-n",
		"ip", "-o", "link", "show").Output()
	if err != nil {
		return "", fmt.Errorf("nsenter list container links: %w", err)
	}
	// ip -o link show 每行形如 "2: eth0@if26: <BROADCAST,...>" / "1: lo: <LOOPBACK,...>"。
	// "@ifNN" 里的 NN 是**对端**接口在本 netns 的 ifindex —— 对容器 eth0 来说就是
	// 宿主侧 veth 的 ifindex，在宿主 netns 内全局唯一。
	// 不能用容器自己的 ifindex 反查宿主 iflink：每个 netns 的编号都从 1 开始，
	// 所有容器的 eth0 都是 2，宿主上所有 veth 的 iflink 也都是 2，会跨容器误匹配
	// （限速打到别人的网卡上）。实测 veth3/4/5/6 的 iflink 全是 2，对应 peer 26/28/30/32。
	var primaryPeer, fallbackPeer string
	for _, line := range strings.Split(string(out), "\n") {
		colon := strings.Index(line, ": ")
		if colon <= 0 {
			continue
		}
		rest := line[colon+2:]
		end := strings.IndexAny(rest, "@:")
		if end < 0 {
			continue
		}
		name := rest[:end]
		if name == "lo" {
			continue
		}
		peer := ""
		if at := strings.Index(rest, "@if"); at >= 0 {
			tail := rest[at+3:]
			if e := strings.IndexAny(tail, "@:"); e >= 0 {
				tail = tail[:e]
			}
			peer = strings.TrimSpace(tail)
		}
		if name == "eth0" {
			primaryPeer = peer
			break
		}
		if fallbackPeer == "" {
			fallbackPeer = peer
		}
	}
	peerIdx := primaryPeer
	if peerIdx == "" {
		peerIdx = fallbackPeer
	}
	if peerIdx == "" {
		return "", fmt.Errorf("no veth peer ifindex found for %s", vmID)
	}

	// 宿主侧按 ifindex 精确匹配（该值在宿主 netns 内唯一，不会误中其他容器的网卡）。
	entries, err := os.ReadDir("/sys/class/net")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("/sys/class/net", e.Name(), "ifindex"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(b)) == peerIdx {
			return e.Name(), nil
		}
	}
	return "", fmt.Errorf("host veth not found for %s (peer ifindex %s)", vmID, peerIdx)
}

// EnsureEgressTbf 在容器 netns 内对出方向接口施加/清除 tbf 限速（上传方向）。
// netavark 的 bandwidth_rate 装在宿主 veth root，只覆盖下载方向；出方向在此补齐，
// 与套餐上限、巡查限速共用一个入口。rateBps<=0 表示清除（对应套餐「不限」）。
// 幂等可反复调用；容器未运行返回错误，由调用方决定是否重试。
func (m *Manager) EnsureEgressTbf(_ context.Context, vmID string, rateBps int64) error {
	inspect, err := containers.Inspect(m.timeoutCtx(), vmID, nil)
	if err != nil {
		return err
	}
	if inspect.State == nil || inspect.State.Pid <= 0 {
		return fmt.Errorf("container %s is not running", vmID)
	}
	// host/none/container 网络没有独立 netns 边界，往里打 tbf 会限到宿主机
	// 或别的容器，与 GetVMVeth 的防护口径一致：直接拒绝。
	if inspect.HostConfig != nil {
		switch inspect.HostConfig.NetworkMode {
		case "host", "none", "container":
			return fmt.Errorf("container %s uses %s network, egress shaping not applicable", vmID, inspect.HostConfig.NetworkMode)
		}
	}
	pid := inspect.State.Pid
	iface, err := containerPrimaryIface(pid)
	if err != nil {
		return err
	}

	nsArgs := []string{"-t", strconv.Itoa(pid), "-n", "tc", "qdisc"}
	var args []string
	if rateBps > 0 {
		burst := rateBps / 10
		if burst < 1000000 {
			burst = 1000000
		}
		args = append(nsArgs, "replace", "dev", iface, "root", "tbf",
			"rate", fmt.Sprintf("%dbit", rateBps),
			"burst", fmt.Sprintf("%db", burst),
			"latency", "50ms")
	} else {
		args = append(nsArgs, "del", "dev", iface, "root")
	}
	execCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(execCtx, "nsenter", args...).CombinedOutput()
	if err != nil {
		// 清除时 root qdisc 本就不存在属正常状态，不算失败。
		msg := string(out)
		if rateBps <= 0 && (strings.Contains(msg, "No such file") || strings.Contains(msg, "Cannot find")) {
			return nil
		}
		return fmt.Errorf("nsenter tc (pid %d, %s): %w: %s", pid, iface, err, strings.TrimSpace(msg))
	}
	return nil
}

// containerPrimaryIface 返回容器 netns 内的主网络接口名（eth0 优先，其次第一个非 lo）。
// 与 GetVMVeth 同源：nsenter 只切 network namespace，ip(8) 走 netlink，
// netlink 随 netns 隔离，因此查询到的就是容器内的接口。
func containerPrimaryIface(pid int) (string, error) {
	out, err := containerLinkOut(pid)
	if err != nil {
		return "", err
	}
	iface, err := parsePrimaryIface(string(out))
	if err != nil {
		return "", fmt.Errorf("pid %d: %w", pid, err)
	}
	return iface, nil
}

// containerLinkOut 在容器 netns 内执行 ip -o link show 并返回原始输出。
func containerLinkOut(pid int) ([]byte, error) {
	nsCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(nsCtx, "nsenter", "-t", strconv.Itoa(pid), "-n",
		"ip", "-o", "link", "show").Output()
	if err != nil {
		return nil, fmt.Errorf("nsenter list container links: %w", err)
	}
	return out, nil
}

// parsePrimaryIface 从 ip -o link show 输出中选出容器内主接口：
// 每行形如 "2: eth0@if26: <BROADCAST,...>"。选择优先级：
// 1) eth0（podman 默认命名）；
// 2) 任意带 @if 后缀的接口（veth 对端特征，排除租户自建的 tun0/tailscale0 等）；
// 3) 兜底第一个非 lo 接口。
func parsePrimaryIface(out string) (string, error) {
	withPeer, fallback := "", ""
	for _, line := range strings.Split(out, "\n") {
		colon := strings.Index(line, ": ")
		if colon <= 0 {
			continue
		}
		rest := line[colon+2:]
		end := strings.IndexAny(rest, "@:")
		if end < 0 {
			continue
		}
		name := rest[:end]
		if name == "lo" {
			continue
		}
		if name == "eth0" {
			return name, nil
		}
		if withPeer == "" && strings.Contains(rest, "@if") {
			withPeer = name
		}
		if fallback == "" {
			fallback = name
		}
	}
	if withPeer != "" {
		return withPeer, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no usable interface found")
}

// GetVMNetStats 获取 VM 的网络流量统计（用于流量统计服务）。
// 优先读 /proc/<pid>/net/dev：与 ListVMs 保持一致，不依赖可能失败的 stats 流。
func (m *Manager) GetVMNetStats(ctx context.Context, vmID string) (*manager.VMNetStats, error) {
	if in, out, err := m.getProcNetDev(vmID); err == nil {
		return &manager.VMNetStats{VMID: vmID, InBytes: in, OutBytes: out}, nil
	}
	_, _, netIn, netOut, err := m.getUsage(ctx, vmID)
	if err != nil {
		return nil, err
	}
	return &manager.VMNetStats{
		VMID:     vmID,
		InBytes:  netIn,
		OutBytes: netOut,
	}, nil
}

func (m *Manager) Cleanup(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	list, err := containers.List(m.timeoutCtx(), &containers.ListOptions{All: ptr(true)})
	if err != nil {
		return err
	}

	// 获取数据库中登记的所有虚拟机，DB 出错时必须中止：
	// 空列表会把所有容器误判为幽灵实例并删除。
	configs, err := m.db.ListVMConfigs()
	if err != nil {
		log.Printf("[Podman] Cleanup: failed to list VM configs, skipping to avoid false deletions: %v", err)
		return err
	}
	registered := make(map[string]bool, len(configs))
	for _, c := range configs {
		registered[c.VMID] = true
	}

	// 安全检查：DB 为空但驱动有容器，可能是 DB 刚初始化或读取异常，放弃本次清理。
	if len(configs) == 0 && len(list) > 0 {
		log.Printf("[Podman] Cleanup: DB has 0 configs but %d containers exist, skipping to avoid false deletions", len(list))
		return nil
	}

	for _, c := range list {
		if len(c.Names) == 0 {
			continue
		}
		name := strings.TrimPrefix(c.Names[0], "/")

		// 跳过已登记的
		if registered[name] {
			continue
		}
		// 跳过临时容器
		if strings.HasPrefix(name, "tmp-") {
			continue
		}
		// 只清理挂在本项目网络上的容器：宿主上运营者自己跑的其他 Podman
		// 容器不在这个网络里，不属于 Agent，不能碰。
		if !slices.Contains(c.Networks, NetworkName) {
			continue
		}

		// 判定为幽灵实例，调用 deleteVM 确保 VMConfig 也被清理
		log.Printf("[Podman] Found ghost container %s, deleting...", name)
		_ = m.deleteVM(ctx, name)
	}
	return nil
}

func mapStatus(s string) agent.VMStatus {
	switch strings.ToLower(s) {
	case "running":
		return agent.VMStatus_VM_STATUS_RUNNING
	case "exited", "stopped":
		return agent.VMStatus_VM_STATUS_STOPPED
	case "created":
		return agent.VMStatus_VM_STATUS_CREATING
	default:
		return agent.VMStatus_VM_STATUS_RUNNING
	}
}
