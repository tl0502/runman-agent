package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

const (
	DefaultDB             = "/opt/narwhal-agent/agent.db"
	DefaultWeb            = ":8792"
	DefaultMaxPortForward = 20
	DefaultWebUser        = "admin"

	// DefaultSiteName 是面板顶栏显示的品牌名；可在设置页改成任意名称（白标）。
	DefaultSiteName = "Run"

	// DefaultPlatformAddr 是官方平台的 gRPC 接入地址；配置为空时使用。
	DefaultPlatformAddr = "hosting.fuckip.me:443"
	// DefaultUpdateRepo 是本 fork 的自更新仓库；配置为空时使用。
	// 上游原值 narwhal-cloud/runman-agent 不再作为兜底，避免误拉官方构建。
	DefaultUpdateRepo = "tl0502/runman-agent"
	// DefaultNetavarkBase 是自定义 netavark（支持 snat_ipv6 / bandwidth_*）的下载基址。
	DefaultNetavarkBase = "https://github.com/tl0502/netavark-Runman/releases/latest/download"
)

// Config 是 agent 的全局配置，从 JSON 配置文件读取，通过 Web API 可修改。
type Config struct {
	DB  string `json:"db"`
	Web string `json:"web"`

	// 虚拟化类型（podman/cloudhv），首次安装时由配置文件写入，不通过 Web API 修改
	VirtType string `json:"virt_type"`

	// 平台认证
	Token string `json:"token"`

	// 平台接入地址（gRPC host:port）。为空时回落到 DefaultPlatformAddr。
	// 改动后 Agent 会自动断开并按新地址重连，无需重启进程。
	PlatformAddr string `json:"platform_addr"`

	// 自更新来源。UpdateRepo 形如 "owner/repo"，为空时回落到 DefaultUpdateRepo；
	// UpdateBaseURL 可指向镜像/反向代理的 releases 下载基址（含 tag），
	// UpdateAPIURL 可指向同结构的版本查询接口（JSON: tag_name / target_commitish）。
	// 后两者为空时按 GitHub 官方地址派生。
	UpdateRepo    string `json:"update_repo"`
	UpdateBaseURL string `json:"update_base_url"`
	UpdateAPIURL  string `json:"update_api_url"`

	// PlatformInsecure 为 true 时跳过平台 gRPC 的证书校验，
	// 仅用于接入使用 IP 地址或自签证书的第三方平台。
	PlatformInsecure bool `json:"platform_insecure"`

	// NetavarkBase 是 Podman 后端自定义 netavark 的下载基址，安装/更新时传给 install.sh。
	// 为空时回落到 DefaultNetavarkBase。
	NetavarkBase string `json:"netavark_base"`

	// PublicIPv4 是所有容器共用的对外地址：端口映射只在该地址上监听，
	// 容器出站流量也 SNAT 成该地址。容器本身仍使用 narwhal-net 的内网地址。
	// 为空时保持旧行为（监听 0.0.0.0，出站走 netavark 默认 masquerade）。
	PublicIPv4 string `json:"public_ipv4"`

	// PublicIPv6 为预留字段：当前版本未实现 IPv6 对外地址，仅保留键位，
	// 避免以后新增字段还要再做一次配置迁移。
	PublicIPv6 string `json:"public_ipv6"`

	// 监控配置
	MonitorNIC  string `json:"monitor_nic"`
	MonitorDisk string `json:"monitor_disk"`

	// Web 面板认证
	WebUser     string `json:"web_user"`
	WebPassHash string `json:"web_pass_hash"` // bcrypt hash

	// 面板品牌名，显示在顶栏与浏览器标题；空值回落 DefaultSiteName。
	SiteName string `json:"site_name"`

	// 公网入口地址（空则由服务端自取）
	Host string `json:"host"`

	// 每个 VM 最大端口转发数
	MaxPortForward int32 `json:"max_port_forward"`

	// 每个 VM 端口转发允许的最大唯一来源 IP 数（全局默认值，0 表示不限制，可按 VM 覆盖）
	MaxForwardIPs int32 `json:"max_forward_ips"`

	// 每月自动重置流量的日期（1-28）
	TrafficResetDay int `json:"traffic_reset_day"`

	// IPv6 配置（cloud-hypervisor / incus 公用）
	IPv6Mode   string `json:"ipv6_mode"`   // "none"/"subnet"/"snat"
	IPv6Subnet string `json:"ipv6_subnet"` // 可用子网，如 "2001:db8:1234::/64"
	IPv6Addr   string `json:"ipv6_addr"`   // 单个公网 IPv6（snat 模式）
	IPv6Iface  string `json:"ipv6_iface"`  // IPv6 所在网卡

	// 供 WireGuard 隧道分配的 IPv6 池（CIDR，如 "2001:db8:1234::/64"）。
	// 探测到本机/上游 IPv6 后由安装脚本按 /64 或更小前缀切分写入，
	// wgbind 在创建隧道时可从中取一个地址分配给隧道接口。
	IPv6WGSubnet string `json:"ipv6_wg_subnet"`

	// IPv6 配置备份目录（backup_ipv6_config / restore_ipv6_config 使用）
	IPv6BackupDir string `json:"ipv6_backup_dir"`

	// NDP 应答器配置（cloud-hypervisor 公网 IPv6 场景）
	NdpIface   string `json:"ndp_iface"`   // 上行网卡
	NdpSubnets string `json:"ndp_subnets"` // 响应的 IPv6 CIDR，逗号分隔
	NdpNetwork string `json:"ndp_network"` // Podman 网络名（Podman NDP 场景）

	// rfw 防火墙 API 本地监听地址（agent 反代用）
	RfwAddr string `json:"rfw_addr"`

	// ── Incus 定制能力 ──────────────────────────────────────────────────────────
	// 自定义欢迎页 / SSH 登录横幅
	IncusBannerPreset string `json:"incus_banner_preset"` // "none" / "default" / "project" / "custom"
	IncusBannerText   string `json:"incus_banner_text"`   // preset="custom" 时的完整文本内容

	// 每个容器分配的 IPv6 数量（非 /64 网段精细化分配，默认 1）
	IncusIPv6Alloc int32 `json:"incus_ipv6_alloc"`

	// 自定义 alpine 基础镜像别名（结合 custom/alpine-base 等定制镜像）。
	// 非空时 OsImage=="alpine" 将使用该别名而非内置的 alpine/3.23/cloud。
	IncusAlpineBase string `json:"incus_alpine_base"`

	// 本地/内网 incus 镜像服务基址（覆盖 GitHub releases）。
	// 可填 http(s) URL（如 http://192.168.10.9:8080）或本地目录路径。
	IncusImageMirror string `json:"incus_image_mirror"`

	// 纯 IPv6 容器开关：开启后新建 incus 容器不分配 IPv4，仅配置静态 IPv6
	// （依赖 IPv6 模式 subnet/snat）。要求宿主机已具备可用 IPv6 子网。
	IncusIPv6Only bool `json:"incus_ipv6_only"`
}

// Manager 持有配置文件路径和内存副本，提供并发安全的读写。
type Manager struct {
	path string
	mu   sync.RWMutex
	cfg  Config
}

// Load 从 JSON 配置文件加载配置到内存。
func Load(path string) (*Manager, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	applyDefaults(&cfg)

	m := &Manager{path: path, cfg: cfg}

	// 如果密码不是加密的（即首次安装由安装脚本写入的明文），自动进行 bcrypt 加密并写回
	if cfg.WebPassHash != "" && !strings.HasPrefix(cfg.WebPassHash, "$2") {
		err = m.Update(func(c *Config) {
			hash, _ := bcrypt.GenerateFromPassword([]byte(c.WebPassHash), bcrypt.DefaultCost)
			c.WebPassHash = string(hash)
		})
		if err != nil {
			return nil, err
		}
	}

	return m, nil
}

// Get 返回当前配置的快照副本（并发安全）。
func (m *Manager) Get() Config {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg
}

// Dir 返回配置文件所在目录，面板用它存放自定义品牌图标等运行期文件。
func (m *Manager) Dir() string {
	return filepath.Dir(m.path)
}

// Update 原子更新配置内存值并持久化到文件。
// fn 接收指针并在锁内修改，修改完成后写文件。
func (m *Manager) Update(fn func(*Config)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(&m.cfg)
	return m.save()
}

// save 将当前内存配置写回文件（调用方持有写锁）。
func (m *Manager) save() error {
	data, err := json.MarshalIndent(m.cfg, "", "    ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, data, 0600)
}

// applyDefaults 在字段为空时填充默认值。
func applyDefaults(cfg *Config) {
	if cfg.DB == "" {
		cfg.DB = DefaultDB
	}
	if cfg.Web == "" {
		cfg.Web = DefaultWeb
	}
	if cfg.WebUser == "" {
		cfg.WebUser = DefaultWebUser
	}
	if cfg.MaxPortForward <= 0 {
		cfg.MaxPortForward = DefaultMaxPortForward
	}
	if cfg.SiteName == "" {
		cfg.SiteName = DefaultSiteName
	}
	if cfg.TrafficResetDay <= 0 || cfg.TrafficResetDay > 28 {
		cfg.TrafficResetDay = 1
	}
	if cfg.IPv6Mode == "" {
		cfg.IPv6Mode = "none"
	}
	if cfg.RfwAddr == "" {
		cfg.RfwAddr = "127.0.0.1:7734"
	}
	if cfg.IncusIPv6Alloc <= 0 {
		cfg.IncusIPv6Alloc = 1
	}
	if cfg.IncusBannerPreset == "" {
		cfg.IncusBannerPreset = "none"
	}
	if cfg.IPv6BackupDir == "" {
		cfg.IPv6BackupDir = "/var/lib/narwhal-agent/backups"
	}
}

// PlatformAddress 返回实际生效的平台接入地址（host:port）。
func (c Config) PlatformAddress() string {
	if v := strings.TrimSpace(c.PlatformAddr); v != "" {
		return v
	}
	return DefaultPlatformAddr
}

// UpdateRepository 返回实际生效的自更新仓库（owner/repo）。
func (c Config) UpdateRepository() string {
	v := strings.TrimSpace(c.UpdateRepo)
	if v == "" {
		return DefaultUpdateRepo
	}
	return strings.TrimPrefix(strings.TrimSuffix(v, "/"), "https://github.com/")
}

// UpdateDownloadBase 返回自更新下载基址的覆盖值；空表示由仓库地址派生。
func (c Config) UpdateDownloadBase() string {
	return strings.TrimRight(strings.TrimSpace(c.UpdateBaseURL), "/")
}

// UpdateAPIBase 返回版本查询接口的覆盖值；空表示由仓库地址派生。
func (c Config) UpdateAPIBase() string {
	return strings.TrimRight(strings.TrimSpace(c.UpdateAPIURL), "/")
}

// NetavarkDownloadBase 返回实际生效的 netavark 下载基址。
func (c Config) NetavarkDownloadBase() string {
	if v := strings.TrimRight(strings.TrimSpace(c.NetavarkBase), "/"); v != "" {
		return v
	}
	return DefaultNetavarkBase
}

// EgressIPv4 返回对外 IPv4（去掉空白）。空表示不指定，走默认行为。
func (c Config) EgressIPv4() string {
	return strings.TrimSpace(c.PublicIPv4)
}
