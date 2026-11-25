package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/crypto/bcrypt"
)

const (
	DefaultDB             = "/opt/narwhal-agent/agent.db"
	DefaultWeb            = ":8792"
	DefaultMaxPortForward = 20
	DefaultWebUser        = "admin"

	// DefaultContainerPidsLimit 是容器的默认 pids 上限（显式化运行时默认值）。
	DefaultContainerPidsLimit = 2048

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

	// PlatformEnabled 控制是否接入平台。为 false 时 Agent 完全不建立平台连接，
	// 仅通过本地面板管理（独立模式）。nil 视为 true——存量配置没有这个字段，
	// 必须保持原有的连接行为，升级不能静默改变已有部署。
	PlatformEnabled *bool `json:"platform_enabled"`

	// 平台接入地址（gRPC host:port）。为空时回落到 DefaultPlatformAddr。
	// 改动后 Agent 会自动断开并按新地址重连，无需重启进程。
	PlatformAddr string `json:"platform_addr"`

	// 自更新来源。UpdateRepo 形如 "owner/repo"，为空时回落到 DefaultUpdateRepo；
	// UpdateBaseURL 可指向镜像/反向代理的 releases 下载基址（含 tag），
	// UpdateAPIURL 可指向同结构的版本查询接口（JSON: tag_name / target_commitish）。
	// 后两者为空时按 GitHub 官方地址派生。
	//
	// 安全：这两个字段指向的内容会以 root 身份执行（updater 用 systemd-run 跑
	// install.sh），因此写入时必须通过 ValidateDownloadBase 校验，
	// 见 web/server.go 的 handleConfig。
	UpdateRepo    string `json:"update_repo"`
	UpdateBaseURL string `json:"update_base_url"`
	UpdateAPIURL  string `json:"update_api_url"`

	// AllowInsecureDownload 允许下载基址使用明文 http://。默认关闭——
	// 明文传输下中间人可直接替换二进制与安装脚本。仅在明确知晓风险时开启。
	AllowInsecureDownload bool `json:"allow_insecure_download"`

	// AllowInternalDownload 允许下载基址指向内网/回环地址（如企业内网镜像站）。
	// 默认关闭——避免把下载指向宿主自身的其它服务或内网元数据服务。
	AllowInternalDownload bool `json:"allow_internal_download"`

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

	// PublicIPv6 是共享 IPv6 的对外地址（snat 模式）：端口映射在该地址上监听，
	// 容器出站流量 SNAT 成它也走它。为空时回落安装脚本探测写入的 IPv6Addr。
	// subnet 模式（容器自带公网 IPv6）不使用该字段。
	PublicIPv6 string `json:"public_ipv6"`

	// ── 容器隔离与安全加固 ────────────────────────────────────────────────────
	// ContainerIsolate 为 true 时通过 runman-hostfw 的 bridge 表 nft 规则实现
	// 容器间隔离（不用 netavark isolate，后者在 br_netfilter 未加载时对同桥流量
	// 不生效）。使容器之间不能互相访问（只能访问网关与出站）。nil 视为 true。
	ContainerIsolate *bool `json:"container_isolate"`

	// HostAllowPorts 是允许容器访问的宿主机 TCP 端口白名单（仅约束新连接，
	// established/related 回程不受限）。为空时容器不能主动访问宿主机任何端口。
	HostAllowPorts []int `json:"host_allow_ports"`

	// ContainerAllowSubnets 是允许容器访问的内网/私有网段白名单（CIDR 列表）。
	// 为空时容器默认不能访问任何私有网段（10/8、172.16/12、192.168/16、169.254/16），
	// 只能出公网。填入的网段会被放行。
	ContainerAllowSubnets []string `json:"container_allow_subnets"`

	// TrustProxy 为 true 时，面板从 X-Forwarded-For / X-Real-IP 取客户端真实 IP。
	//
	// 只在面板确实挂在反向代理（nginx / 1Panel 等）后面时才能打开：直连部署打开它，
	// 任何人都能伪造来源 IP，从而绕过登录封禁。默认 false，保持「只认 RemoteAddr」。
	// 取值时只信 XFF 的最后一项（最近一跳代理追加），前面的项可被客户端伪造。
	TrustProxy bool `json:"trust_proxy"`

	// ContainerNoNewPrivs 为 true 时给容器设置 no_new_privileges，阻断 setuid 提权。nil 视为 true。
	ContainerNoNewPrivs *bool `json:"container_no_new_privs"`

	// ContainerUserns 为 true 时给容器启用 user namespace（userns=auto），
	// 容器内 root 映射成宿主机非特权 uid，逃逸后拿不到宿主 root。nil 视为 true。
	// 依赖宿主机 /etc/subuid、/etc/subgid 为 root 配置了足够的 id 范围。
	ContainerUserns *bool `json:"container_userns"`

	// ContainerPidsLimit 是容器的 pids 上限（cgroup pids.max）。<=0 时使用默认 2048。
	ContainerPidsLimit int64 `json:"container_pids_limit"`

	// ContainerDropCaps / ContainerAddCaps 是创建容器时额外 drop/add 的 capabilities
	// （逗号分隔，如 "SYS_MODULE,SYS_RAWIO"）。留空则不改变运行时默认 capability 集。
	// 注意：systemd + sshd 镜像对默认 capability 集依赖较强，激进 drop 需先在测试机验证。
	ContainerDropCaps string `json:"container_drop_caps"`
	ContainerAddCaps  string `json:"container_add_caps"`

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

	// 流量重置模式："monthly" = 每月固定日（TrafficResetDay）；"rental" = 每 VM 按创建时间 + 30 天。
	// 为空或非法值回落 "monthly"。
	TrafficResetMode string `json:"traffic_reset_mode"`

	// 每月自动重置流量的日期（1-28，仅 monthly 模式生效）
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

	// 自动巡查（巡检）：周期探测 VM 的网络带宽/活跃连接、CPU/内存/磁盘、容器内进程，
	// 命中规则后限速/停止/重启/杀进程并记录事件、发 Telegram 通知。仅 Podman 后端生效。
	Patrol PatrolConfig `json:"patrol"`

	// Telegram 通知：巡查与其它告警的推送通道。
	Telegram TelegramConfig `json:"telegram"`
}

// PatrolConfig 是自动巡查的全部配置。所有百分比与时长均可配置，不留写死的阈值。
type PatrolConfig struct {
	// Enabled 为 nil 时视为 true（默认开启巡查）。false 显式关闭整个巡查。
	Enabled *bool `json:"enabled"`
	// IntervalSec 巡查周期（秒），<=0 时回落默认 60。
	IntervalSec int `json:"interval_sec"`

	Bandwidth   PatrolBandwidth   `json:"bandwidth"`
	Connections PatrolConnections `json:"connections"`
	Resources   PatrolResources   `json:"resources"`
	Processes   PatrolProcesses   `json:"processes"`
	Abuse       PatrolAbuse       `json:"abuse"`

	// SelfCheckFailLimit 巡查数据源（容器列表/连接表）连续失败多少次后告警，
	// <=0 回落 3。把「巡查静默失效」变成「瞎了会喊」，恢复时同样上报。
	SelfCheckFailLimit int `json:"self_check_fail_limit"`
}

// PatrolBandwidth 单容器带宽占用巡查。ThresholdPct 是「占分配上限的百分比」。
type PatrolBandwidth struct {
	Enabled *bool `json:"enabled"` // nil 视为 true
	// ThresholdPct 占用超过分配上限的该百分比即判定为高（0-100）。
	ThresholdPct int `json:"threshold_pct"`
	// DurationMin 连续超过阈值的分钟数，达到后触发限速。
	DurationMin int `json:"duration_min"`
	// ThrottleMbps 限速动作把带宽压到的绝对速率（Mbps），默认 10。
	// 按绝对值而不是分配上限的百分比：VM 未设带宽上限时同样可以限速。
	ThrottleMbps int `json:"throttle_mbps"`
	// ReleaseMin 限速持续时间（分钟），到期自动恢复原上限，期间可人工恢复。
	ReleaseMin int `json:"release_min"`
}

// PatrolConnections 单容器活跃连接数巡查（in/out 两方向，抓发包/PT 类滥用）。
type PatrolConnections struct {
	Enabled     *bool `json:"enabled"`      // nil 视为 true
	InLimit     int   `json:"in_limit"`     // 入方向活跃连接数阈值
	OutLimit    int   `json:"out_limit"`    // 出方向活跃连接数阈值
	DurationMin int   `json:"duration_min"` // 连续超限分钟数
	// ThrottleMbps 停止 VM 时同步施加的限速（Mbps），ReleaseMin 分钟后自动解除。
	ThrottleMbps int `json:"throttle_mbps"`
	ReleaseMin   int `json:"release_min"`
}

// PatrolResources VM 级 CPU/内存/磁盘巡查。Action 取值 restart/stop/alert。
type PatrolResources struct {
	Enabled *bool `json:"enabled"` // nil 视为 true
	CpuPct  int   `json:"cpu_pct"`
	CpuMin  int   `json:"cpu_min"`
	MemPct  int   `json:"mem_pct"`
	MemMin  int   `json:"mem_min"`
	DiskPct int   `json:"disk_pct"`
	// DiskAlertIntervalMin 是同一 VM 磁盘告警的最小间隔（分钟），默认 360（6 小时）。
	// 磁盘超阈值只记面板日志、不发 Telegram。
	DiskAlertIntervalMin int `json:"disk_alert_interval_min"`
	// Action 是 CPU/内存超限后的动作：restart（自动重启，默认）/ stop（关机）/ alert（仅告警）。
	Action string `json:"action"`
	// RestartCooldownMin 两次自动重启动作的最小间隔（分钟），防重启风暴。
	RestartCooldownMin int `json:"restart_cooldown_min"`
}

// PatrolProcesses 容器内进程黑名单巡查：命中即杀。Blacklist 为全局一份。
type PatrolProcesses struct {
	Enabled   *bool    `json:"enabled"` // nil 视为 true
	Blacklist []string `json:"blacklist"`
	// KillStopLimit 连杀升级停机阈值：1 小时窗口内命中次数达到该值，视为
	// 有守护脚本反复拉起，光杀进程治标不治本，自动停容器。0=关闭（默认）。
	KillStopLimit int `json:"kill_stop_limit"`
}

// PatrolAbuse 滥用识别巡查：从 conntrack 原始条目里识别「天生不活跃」的滥用流量。
// 现有活跃连接巡查只认 TCP ESTABLISHED / UDP ASSURED，而滥用行为（单向 UDP 发包、
// 端口扫描的半截握手、群发邮件的短命连接）恰恰是非活跃条目，活跃口径下不可见。
type PatrolAbuse struct {
	// Enabled 为 nil 时视为 false（默认关）：新功能上线不应在阈值未调好前自动开始告警。
	Enabled *bool `json:"enabled"`
	// UdpConnLimit 单容器 UDP 条目数阈值（in/out 任一方向达到即算超限，含 UNREPLIED）。
	UdpConnLimit int `json:"udp_conn_limit"`
	// ScanDstLimit 出方向不同目标 IP 数阈值（达到即算超限，解析侧饱和封顶）。
	ScanDstLimit int `json:"scan_dst_limit"`
	// SmtpPorts 出方向连接视为发信行为的目标端口黑名单（空回落 [25]）。
	SmtpPorts []int `json:"smtp_ports"`
	// SmtpLimit 出方向命中黑名单端口的 TCP 条目数阈值。
	SmtpLimit int `json:"smtp_limit"`
	// DurationMin 持续超限多少分钟后执行动作；动作后计时清零，天然形成告警节奏。
	DurationMin int `json:"duration_min"`
	// 三个子项的动作均为 alert / throttle / stop，各自独立可配。
	UdpAction  string `json:"udp_action"`
	ScanAction string `json:"scan_action"`
	SmtpAction string `json:"smtp_action"`
	// ThrottleMbps / ReleaseMin 是 throttle 动作共用的限速值与到期恢复时长。
	ThrottleMbps int `json:"throttle_mbps"`
	ReleaseMin   int `json:"release_min"`
}

// EnabledOn 返回滥用识别开关，nil 视为 false（默认关）。
func (a PatrolAbuse) EnabledOn() bool {
	return a.Enabled != nil && *a.Enabled
}

// TelegramConfig 是 Telegram 通知配置。Enabled 为 nil 时视为 false（默认不推送）。
// BotToken 属于凭据：API 响应不回传明文（只回 has_token），提交时空串 = 保留现有值。
type TelegramConfig struct {
	Enabled  *bool  `json:"enabled"`
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
	// LoginEnabled 登录安全事件总开关（nil 视为 true）。细粒度开关见下方逐事件字段，
	// 保留它是为了兼容存量配置里显式 false 的语义（整体关闭登录类推送）。
	LoginEnabled *bool `json:"login_enabled"`
	// 逐事件推送开关，nil 均视为 true（升级前行为：巡查告警与登录事件默认推送）。
	PatrolNotify       *bool `json:"patrol_notify"`
	LoginOkNotify      *bool `json:"login_ok_notify"`
	AutoBanNotify      *bool `json:"auto_ban_notify"`
	LoginBlockedNotify *bool `json:"login_blocked_notify"`
}

// LoginNotifyOn 返回登录安全事件推送开关，nil 视为 true。
func (c TelegramConfig) LoginNotifyOn() bool {
	return c.LoginEnabled == nil || *c.LoginEnabled
}

// Manager 持有配置文件路径和内存副本，提供并发安全的读写。
type Manager struct {
	path string
	mu   sync.RWMutex
	cfg  Config
}

// HashPassword 生成 bcrypt 哈希，供 CLI / 安装器直接写盘使用。
// 直接存哈希可以避免明文密码短暂落在配置文件里，也不必依赖二次 Load 触发自动加密。
// bcrypt 只接受 72 字节以内的密码，超长会返回错误——不能静默忽略，
// 否则会把密码写成空哈希，等于把面板密码清空。
func HashPassword(plain string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
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
			hash, herr := HashPassword(c.WebPassHash)
			if herr != nil {
				// 哈希失败时保留原值：写成空串等于把密码清空。
				// 但必须让运维看见——明文会一直留在配置文件里，每次 Load 都重试。
				log.Printf("[config] web_pass_hash 自动加密失败（明文超过 72 字节？），本次保留明文: %v", herr)
				return
			}
			c.WebPassHash = hash
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
	// 在副本上修改，落盘成功后才替换内存值：写盘失败时不能留下
	// 「接口报失败、内存却已变」的半生效状态。
	next := m.cfg
	fn(&next)
	if err := m.save(next); err != nil {
		return err
	}
	m.cfg = next
	return nil
}

// UpdateChecked 与 Update 相同，但 fn 可以返回错误；fn 返回错误时不落盘、
// 不修改内存值。用于下载源这类必须先校验才能写入的字段——校验失败若被
// 静默忽略，会让面板显示「已保存」而实际未生效，或让非法值短暂落盘。
func (m *Manager) UpdateChecked(fn func(*Config) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 先在副本上试跑，fn 成功后才提交，避免校验中途失败留下半更新状态。
	next := m.cfg
	if err := fn(&next); err != nil {
		return err
	}
	if err := m.save(next); err != nil {
		return err
	}
	m.cfg = next
	return nil
}

// save 将配置写回文件（调用方持有写锁）。先写同目录临时文件再 rename：
// 直接覆盖写时，磁盘满或断电会留下截断的 JSON，Agent 下次无法启动。
func (m *Manager) save(cfg Config) error {
	data, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, m.path)
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
	if cfg.TrafficResetMode != "rental" {
		cfg.TrafficResetMode = "monthly"
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
	applyPatrolDefaults(&cfg.Patrol)
}

// applyPatrolDefaults 为巡查各数值字段填充默认值（所有阈值/时长可配置，缺省用默认）。
func applyPatrolDefaults(p *PatrolConfig) {
	if p.IntervalSec <= 0 {
		p.IntervalSec = 60
	}
	if p.Bandwidth.ThresholdPct <= 0 {
		p.Bandwidth.ThresholdPct = 90
	}
	if p.Bandwidth.DurationMin <= 0 {
		p.Bandwidth.DurationMin = 6
	}
	if p.Bandwidth.ThrottleMbps <= 0 {
		p.Bandwidth.ThrottleMbps = 10
	}
	if p.Bandwidth.ReleaseMin <= 0 {
		p.Bandwidth.ReleaseMin = 60
	}
	if p.Connections.InLimit <= 0 {
		p.Connections.InLimit = 200
	}
	if p.Connections.OutLimit <= 0 {
		p.Connections.OutLimit = 300
	}
	if p.Connections.DurationMin <= 0 {
		p.Connections.DurationMin = 4
	}
	if p.Connections.ThrottleMbps <= 0 {
		p.Connections.ThrottleMbps = 10
	}
	if p.Connections.ReleaseMin <= 0 {
		p.Connections.ReleaseMin = 60
	}
	if p.Resources.CpuPct <= 0 {
		p.Resources.CpuPct = 90
	}
	if p.Resources.CpuMin <= 0 {
		p.Resources.CpuMin = 5
	}
	if p.Resources.MemPct <= 0 {
		p.Resources.MemPct = 90
	}
	if p.Resources.MemMin <= 0 {
		p.Resources.MemMin = 5
	}
	if p.Resources.DiskPct <= 0 {
		p.Resources.DiskPct = 90
	}
	if p.Resources.DiskAlertIntervalMin <= 0 {
		p.Resources.DiskAlertIntervalMin = 360
	}
	if p.Resources.Action == "" {
		p.Resources.Action = "restart"
	}
	if p.Resources.RestartCooldownMin <= 0 {
		p.Resources.RestartCooldownMin = 60
	}
	if p.Processes.Blacklist == nil {
		// 匹配语义：进程名精确（词尾 * 通配），命令行按词边界匹配。
		// 挖矿：二进制名 + 协议特征（改名伪装也能从池地址抓到）；
		// BT/PT、发包工具同理。代理类（v2ray/xray/sing-box 等）不禁用。
		p.Processes.Blacklist = []string{
			"xmrig", "minerd", "cpuminer", "xmr-stak", "cnrig",
			"kdevtmpfsi", "kinsing", "kthreaddi", "monerod",
			"stratum+tcp", "stratum+ssl",
			"transmission", "qbittorrent", "aria2c", "deluge*", "rtorrent", "biglybt",
			"hping*", "xerxes", "torshammer", "slowloris",
		}
	}
	if p.Abuse.UdpConnLimit <= 0 {
		p.Abuse.UdpConnLimit = 200
	}
	if p.Abuse.ScanDstLimit <= 0 {
		p.Abuse.ScanDstLimit = 200
	}
	if len(p.Abuse.SmtpPorts) == 0 {
		p.Abuse.SmtpPorts = []int{25}
	}
	if p.Abuse.SmtpLimit <= 0 {
		p.Abuse.SmtpLimit = 20
	}
	if p.Abuse.DurationMin <= 0 {
		p.Abuse.DurationMin = 5
	}
	if p.Abuse.UdpAction == "" {
		p.Abuse.UdpAction = "throttle"
	}
	if p.Abuse.ScanAction == "" {
		p.Abuse.ScanAction = "alert"
	}
	if p.Abuse.SmtpAction == "" {
		p.Abuse.SmtpAction = "alert"
	}
	if p.Abuse.ThrottleMbps <= 0 {
		p.Abuse.ThrottleMbps = 10
	}
	if p.Abuse.ReleaseMin <= 0 {
		p.Abuse.ReleaseMin = 60
	}
	if p.SelfCheckFailLimit <= 0 {
		p.SelfCheckFailLimit = 3
	}
}

// PlatformEnabledOn 返回是否接入平台。nil 视为 true：存量配置里没有该字段，
// 升级后必须沿用它原本的行为（接平台），否则所有已接入的机器会静默断连。
// 独立模式（不接平台）只能由用户显式关闭。
func (c Config) PlatformEnabledOn() bool {
	return c.PlatformEnabled == nil || *c.PlatformEnabled
}

// PlatformAddress 返回实际生效的平台接入地址（host:port）。
func (c Config) PlatformAddress() string {
	if v := strings.TrimSpace(c.PlatformAddr); v != "" {
		return v
	}
	return DefaultPlatformAddr
}

// updateRepoPattern 限定 update_repo 必须是 "owner/repo" 形式，
// 避免填入任意字符串后拼出的 GitHub API 地址无法解析、或误导使用者以为能配镜像。
var updateRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// UpdateRepository 返回实际生效的自更新仓库（owner/repo）。
// 非法格式回落到默认仓库——宁可更新失败也不拼出不可预期的地址。
func (c Config) UpdateRepository() string {
	v := strings.TrimSpace(c.UpdateRepo)
	if v == "" {
		return DefaultUpdateRepo
	}
	v = strings.TrimPrefix(strings.TrimSuffix(v, "/"), "https://github.com/")
	if !updateRepoPattern.MatchString(v) {
		return DefaultUpdateRepo
	}
	return v
}

// UpdateDownloadBase 返回自更新下载基址的覆盖值；空表示由仓库地址派生。
func (c Config) UpdateDownloadBase() string {
	return strings.TrimRight(strings.TrimSpace(c.UpdateBaseURL), "/")
}

// UpdateAPIBase 返回版本查询接口的覆盖值；空表示由仓库地址派生。
func (c Config) UpdateAPIBase() string {
	return strings.TrimRight(strings.TrimSpace(c.UpdateAPIURL), "/")
}

// ErrInvalidDownloadBase 表示下载源校验失败。调用方（如 web handler）可用
// errors.Is 识别并返回 400，而不是笼统的 500。
var ErrInvalidDownloadBase = errors.New("下载地址校验失败")

// cgnatBlock 是运营商级 NAT 网段（RFC 6598，100.64.0.0/10）。
// Go 的 net.IP.IsPrivate() 不覆盖这一段，但它在云厂商环境里属于内网，
// 同样是 SSRF / 劫持的目标，需单独拒绝。
var cgnatBlock = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// isBlockedHost 判断主机是否落在不应作为下载源的地址段。
func isBlockedHost(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsUnspecified() ||
		ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		cgnatBlock.Contains(ip)
}

// ValidateDownloadBase 校验一个下载基址（update_base_url / update_api_url /
// netavark_base）是否可以安全用于拉取并执行的内容。
//
// 背景：这些字段可由面板写入，而下载到的 install.sh 与二进制会以 root 身份执行
// （updater 用 systemd-run 跑 install.sh）。若允许任意来源，等于把「改配置」
// 放大成「以 root 执行任意代码」。因此这里做三层限制：
//
//  1. 必须是 http/https 绝对 URL——拒绝 file://、ftp:// 等其它 scheme；
//  2. 默认必须 https——明文 HTTP 会让中间人直接替换二进制；
//     仅当显式开启 AllowInsecureDownload 时才放行 http://；
//  3. 目标主机不得是回环/内网地址——避免把下载指向宿主自身的其它服务，
//     也避免 SSRF 打到内网元数据服务。
//
// 内网自建镜像站是合法需求（如企业内网 Nexus），因此第 3 条只在
// AllowInternalDownload 显式开启时豁免。
func ValidateDownloadBase(raw string, allowInsecure, allowInternal bool) error {
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil
	}
	u, err := url.Parse(v)
	if err != nil {
		return fmt.Errorf("%w: 地址无效: %v", ErrInvalidDownloadBase, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !allowInsecure {
			return fmt.Errorf("%w: 必须使用 https（当前为 http）：%s", ErrInvalidDownloadBase, v)
		}
	default:
		return fmt.Errorf("%w: 只支持 http/https：%s", ErrInvalidDownloadBase, v)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: 缺少主机名：%s", ErrInvalidDownloadBase, v)
	}
	if allowInternal {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedHost(ip) {
			return fmt.Errorf("%w: 指向内网/回环地址，已拒绝：%s", ErrInvalidDownloadBase, host)
		}
		return nil
	}
	// 主机名：解析一次，若解析到内网 IP 同样拒绝。
	if addrs, err := net.LookupIP(host); err == nil {
		for _, a := range addrs {
			if isBlockedHost(a) {
				return fmt.Errorf("%w: %s 解析到内网地址，已拒绝", ErrInvalidDownloadBase, host)
			}
		}
	}
	return nil
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

// IsPrivateIPv4 判断一个 IPv4 字面量是否属于无法直接对外通告的地址段
// （RFC1918 私网、CGNAT 100.64/10、回环、未指定、链路本地）。
// 非 IPv4 字面量（含域名、IPv6、空串）一律返回 false，交由调用方按原值处理。
func IsPrivateIPv4(raw string) bool {
	ip := net.ParseIP(strings.TrimSpace(raw))
	if ip == nil || ip.To4() == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsUnspecified() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || cgnatBlock.Contains(ip)
}

// EntryHost 返回对外的入口地址，供心跳 entry_host、面板展示、通知身份行共用同一口径：
// host（手动，支持 DDNS）> public_ipv4（仅当它是公网地址）> 自动探测的公网 IPv4。
//
// 为什么 public_ipv4 要判公网：云厂商 1:1 NAT（内网网卡 + 弹性公网 IP，如 AWS
// Lightsail）场景下，这个字段只能填网卡上的内网地址用于端口监听，套在入口地址上
// 上报会让平台和面板把 172.16/10/192.168 之类地址当成用户可访问地址。
// detected 为空时保留 public_ipv4 原值：纯内网部署里它就是唯一入口，
// 宁可上报内网地址，也不要留空让平台无从连接。
func (c Config) EntryHost(detectedIPv4 string) string {
	v, _ := c.entryHostResolve(detectedIPv4)
	return v
}

// EntryHostSource 与 EntryHost 同口径，额外返回取值来源，供面板展示
// 「当前生效值 · 来源」，避免再用文字解释一遍优先级：
//
//	"manual"      —— 手动填写的 host（含 DDNS 域名）
//	"public_ipv4" —— 面板选定的对外 IPv4（本身是公网地址）
//	"auto"        —— 自动探测到的公网 IPv4
//	""            —— 三者都没有（尚未探测到，或探测被网络阻断）
func (c Config) EntryHostSource(detectedIPv4 string) string {
	_, src := c.entryHostResolve(detectedIPv4)
	return src
}

// EntryIPv6 返回上报/展示用的入口 IPv6 与来源，与 EntryHost 同一思路：
//   - 未启用共享 IPv6（非 snat 模式，或 snat 但没拿到地址）→ 自动探测值，来源 auto；
//   - 已启用 → 配置地址，来源 config；
//   - 配置地址已不在本机网卡上（实例重建、云厂商地址漂移）且探测到了新地址
//     → 回落探测值，来源 stale（此时端口监听仍绑旧地址，需要人工修正配置）。
//
// onHost 可为 nil（不做网卡校验，直接信配置）。
func (c Config) EntryIPv6(detectedIPv6 string, onHost func(string) bool) (string, string) {
	detected := strings.TrimSpace(detectedIPv6)
	if !c.SharedIPv6Enabled() {
		if detected != "" {
			return detected, "auto"
		}
		return "", ""
	}
	configured := c.EgressIPv6()
	if detected != "" && onHost != nil && !onHost(configured) {
		return detected, "stale"
	}
	return configured, "config"
}

func (c Config) entryHostResolve(detectedIPv4 string) (string, string) {
	if h := strings.TrimSpace(c.Host); h != "" {
		return h, "manual"
	}
	pub := strings.TrimSpace(c.PublicIPv4)
	detected := strings.TrimSpace(detectedIPv4)
	if pub == "" {
		if detected != "" {
			return detected, "auto"
		}
		return "", ""
	}
	if IsPrivateIPv4(pub) && detected != "" {
		return detected, "auto"
	}
	return pub, "public_ipv4"
}

// EgressIPv6 返回共享 IPv6 的对外地址：public_ipv6 优先，回落安装脚本
// 探测写入的 ipv6_addr。仅在 snat 模式下有意义。
func (c Config) EgressIPv6() string {
	if v := strings.TrimSpace(c.PublicIPv6); v != "" {
		return v
	}
	return strings.TrimSpace(c.IPv6Addr)
}

// SharedIPv6Enabled 表示共享 IPv6 是否启用：snat 模式且拿到可用地址。
// subnet 模式下容器自带公网地址，不需要共享出口，也不做端口映射。
func (c Config) SharedIPv6Enabled() bool {
	return c.IPv6Mode == "snat" && c.EgressIPv6() != ""
}

// IsolateEnabled 返回是否启用容器间网络隔离（runman-hostfw bridge 表 nft 规则）。
// nil 视为 true。
func (c Config) IsolateEnabled() bool {
	if c.ContainerIsolate == nil {
		return true
	}
	return *c.ContainerIsolate
}

// NoNewPrivsEnabled 返回是否为容器开启 no_new_privileges。nil 视为 true。
func (c Config) NoNewPrivsEnabled() bool {
	if c.ContainerNoNewPrivs == nil {
		return true
	}
	return *c.ContainerNoNewPrivs
}

// UsernsEnabled 返回是否为容器开启 user namespace。nil 视为 true。
func (c Config) UsernsEnabled() bool {
	if c.ContainerUserns == nil {
		return true
	}
	return *c.ContainerUserns
}

// PidsLimit 返回容器的 pids 上限，<=0 时回落默认值。
func (c Config) PidsLimit() int64 {
	if c.ContainerPidsLimit <= 0 {
		return DefaultContainerPidsLimit
	}
	return c.ContainerPidsLimit
}

// PatrolEnabled 返回巡查总开关，nil 视为 true。
func (c Config) PatrolEnabled() bool {
	return c.Patrol.Enabled == nil || *c.Patrol.Enabled
}

// TelegramEnabled 返回 Telegram 通知开关，nil 视为 false（默认不推送）。
func (c Config) TelegramEnabled() bool {
	return c.Telegram.Enabled != nil && *c.Telegram.Enabled
}

// DropCaps 返回创建容器时额外 drop 的 capability 列表（去空）。
func (c Config) DropCaps() []string { return splitCSV(c.ContainerDropCaps) }

// AddCaps 返回创建容器时额外 add 的 capability 列表（去空）。
func (c Config) AddCaps() []string { return splitCSV(c.ContainerAddCaps) }

// AllowedHostPorts 返回容器可访问的宿主机 TCP 端口白名单：去重、过滤非法值、升序。
func (c Config) AllowedHostPorts() []int {
	seen := map[int]bool{}
	out := make([]int, 0, len(c.HostAllowPorts))
	for _, p := range c.HostAllowPorts {
		if p < 1 || p > 65535 || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

// AllowedSubnets 返回允许容器访问的内网/私有网段白名单：去空、去重、保持顺序。
func (c Config) AllowedSubnets() []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(c.ContainerAllowSubnets))
	for _, s := range c.ContainerAllowSubnets {
		v := strings.TrimSpace(s)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// ParsePortList 把端口列表字符串解析成 []int（去重、升序），非法值（非数字、越界）忽略。
// 分隔符兼容中文逗号/顿号/分号与空白：用户常从聊天、文档里粘「779，22818」，
// 旧实现只按 ASCII 逗号切分，中文逗号会让整串 Atoi 失败 → 静默变成空列表，
// 导致白名单全丢且无任何提示。
func ParsePortList(s string) []int {
	parts := strings.FieldsFunc(s, func(r rune) bool {
		switch r {
		case ',', '，', '、', ';', '；', '\t', '\n', '\r', ' ':
			return true
		}
		return false
	})
	seen := make(map[int]bool, len(parts))
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// ParseSubnetList 把逗号分隔的 CIDR 字符串解析成 []string，去空去重。
func ParseSubnetList(s string) []string {
	seen := map[string]bool{}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// splitCSV 把逗号分隔的字符串切成去空白的片段。
func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
