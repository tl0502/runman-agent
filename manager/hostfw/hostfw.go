// Package hostfw 维护 nft 规则，限制 narwhal-net 容器的网络访问：
//
//   - 容器间：bridge 家族表，拦同桥二层转发（meta ibrname == obrname）。
//   - 容器 → 宿主机本机：inet INPUT，端口白名单。
//   - 容器 → 内网私有段：inet FORWARD，拦 RFC1918/CGNAT/链路本地，放行公网。
//
// 为什么不把规则写进 netavark 表：egress 在改出口 SNAT 时会执行
// `nft delete table inet netavark` 再 reload，写进去的自定义规则会被冲掉。
// 独立表互不影响，且与 1Panel 的 ip filter/ip6 filter 表解耦。
//
// 为什么容器间隔离用 bridge 家族而不是 inet FORWARD：现代系统默认不加载
// br_netfilter（/proc/sys/net/bridge 不存在），同桥容器间流量是纯二层交换，
// 根本不经过 ip/inet 家族的 FORWARD hook。只有 bridge 家族的 forward hook
// 能拦到它。
//
// 当前仅约束 IPv4；IPv6 方向待后续补齐。
package hostfw

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"runman-agent/config"
)

const (
	tableName   = "runman-hostfw" // inet/ip6/bridge 不同家族，可同名
	networkFile = "/etc/containers/networks/narwhal-net.json"

	// defaultSubnetCIDR 是读不到 narwhal-net.json 时回落的 IPv4 网段。
	// 正常情况下网段从配置文件动态读取（install.sh 固定用 10.91.0.0/20），
	// 回落值保证非标准部署下规则仍能生成。
	defaultSubnetCIDR = "10.91.0.0/20"

	// existenceCheckEvery 是规则存在性校验的周期（以 Apply 调用次数计）。
	// watchHostFirewall 每 10 秒调一次 Apply，10 次 = 约 100 秒校验一次，
	// 兼顾自愈及时性与 `nft list` 的开销。
	existenceCheckEvery = 10
)

// privateCIDRs 是默认拦截的私有/保留网段（容器不该主动访问的内网）。
var privateCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"100.64.0.0/10",
}

// privateIPv6CIDRs 是默认拦截的 IPv6 内网网段：ULA 与链路本地。
// 容器 IPv6 是 ULA（fd91:cafe:cafe:10::/64），不应访问其它 ULA 段或链路本地服务。
var privateIPv6CIDRs = []string{
	"fc00::/7",  // ULA
	"fe80::/10", // link-local
}

// Applier 让 nft 表与配置保持一致。幂等：仅在有变化时重建。
type Applier struct {
	cfg     *config.Manager
	mu      sync.Mutex
	last    string // 上次应用的规则指纹
	ticks   int    // 指纹未变的连续调用次数，用于周期性存在性校验
	lastErr string // 最近一次 Apply 失败原因（空=正常），供面板展示隔离失效状态
	applied bool   // 是否至少成功应用过一次

	// fwdPorts 返回当前端口转发规则的宿主端口（按协议分组）。
	// 这些是 NAT 映射端口：容器做代理时需要回环访问宿主公网 IP 上的转发端口，
	// 即使不在 host_allow_ports 白名单里也应放行。未注入时为 nil，不生成额外规则。
	fwdPorts func() map[string][]int
}

func New(cfg *config.Manager) *Applier {
	return &Applier{cfg: cfg}
}

// SetForwardPortsProvider 注入端口转发宿主端口来源（NAT 映射端口）。
// 由 main.go 传入 portforward.Manager.ForwardPorts，随转发规则增删自动同步。
func (a *Applier) SetForwardPortsProvider(fn func() map[string][]int) {
	a.fwdPorts = fn
}

// Healthy 返回隔离规则是否处于已生效状态。
// 用于面板展示：隔离失效（应用失败或规则被外部删除）时提示用户，而非静默 fail-open。
func (a *Applier) Healthy() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.applied {
		return false, "隔离规则尚未成功应用"
	}
	if a.lastErr != "" {
		return false, a.lastErr
	}
	return true, ""
}

// Apply 校验配置并（必要时）重建 inet、ip6 与 bridge 三张表。
func (a *Applier) Apply(ctx context.Context) error {
	if a.cfg.Get().VirtType != "podman" {
		return nil
	}

	conf := a.cfg.Get()
	ports := conf.AllowedHostPorts()
	subnets := conf.AllowedSubnets()
	isolate := conf.IsolateEnabled()

	// 端口转发宿主端口（NAT 映射端口）：容器回环访问这些端口也应放行。
	var fwdPorts map[string][]int
	if a.fwdPorts != nil {
		fwdPorts = a.fwdPorts()
	}

	// 网段从 narwhal-net.json 读（单一真源），而不是硬编码 10.91.0.0/20。
	// 读不到 IPv4 网段时回落到默认值，保证非标准部署下规则仍能生成。
	info := readNetworkFile(networkFile)
	ipv4 := info.ipv4
	if ipv4 == "" {
		ipv4 = defaultSubnetCIDR
	}

	// 隔离开启时必须拿到真实网桥名，否则规则会落到猜的名字上、静默失效。
	// 拿不到就返回错误，交由 watchHostFirewall 的轮询在网络恢复后重试。
	bridge := ""
	if isolate {
		bridge = info.bridge
		if bridge == "" {
			bridge = bridgeNameFromPodman()
		}
		if bridge == "" {
			a.setErr("container isolation enabled but bridge name unavailable (narwhal-net missing?)")
			return fmt.Errorf("%s", a.lastErr)
		}
	}
	fp := fingerprint(ports, subnets, isolate, bridge, ipv4, info.ipv6, fwdPorts)

	a.mu.Lock()
	defer a.mu.Unlock()

	// 指纹一致通常意味着无需重建，但规则可能被外部删除（1Panel 重载、
	// 手动 nft delete、系统重启后规则未持久化）。因此每隔若干次调用做一次
	// 轻量的存在性校验，发现表不在了就强制重建——把「静默失效」变成自愈。
	if a.last == fp {
		a.ticks++
		if a.ticks%existenceCheckEvery != 0 || a.rulesPresent(info.ipv6 != "") {
			return nil
		}
		log.Printf("[HostFW] ruleset missing despite unchanged config; rebuilding")
		// 规则丢失：清空指纹以强制走重建路径。
		a.last = ""
	}

	if err := a.applyRuleset(ctx, "inet", inetRuleset(ports, subnets, ipv4, fwdPorts)); err != nil {
		a.setErrLocked(err.Error())
		return err
	}
	if info.ipv6 != "" {
		if err := a.applyRuleset(ctx, "ip6", inet6Ruleset(ports, subnets, info.ipv6, fwdPorts)); err != nil {
			a.setErrLocked(err.Error())
			return err
		}
	}
	if err := a.applyRuleset(ctx, "bridge", bridgeRuleset(bridge, isolate)); err != nil {
		a.setErrLocked(err.Error())
		return err
	}

	a.last = fp
	a.ticks = 0
	a.lastErr = ""
	a.applied = true
	log.Printf("[HostFW] applied: isolate=%v host_ports=%v allow_subnets=%v ipv4=%s ipv6=%s", isolate, ports, subnets, ipv4, info.ipv6)
	return nil
}

// setErr 记录失败原因（调用方尚未持锁）。
func (a *Applier) setErr(msg string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastErr = msg
}

// setErrLocked 记录失败原因（调用方已持锁）。
func (a *Applier) setErrLocked(msg string) {
	a.lastErr = msg
}

// rulesPresent 检查规则表是否都还在。轻量：只探测表存在性，不比对规则内容。
// hasIPv6 表示是否应存在 ip6 表（配置了 IPv6 网段时）。
func (a *Applier) rulesPresent(hasIPv6 bool) bool {
	families := []string{"inet", "bridge"}
	if hasIPv6 {
		families = append(families, "ip6")
	}
	for _, family := range families {
		cmd := exec.Command("nft", "list", "table", family, tableName)
		if err := cmd.Run(); err != nil {
			return false
		}
	}
	return true
}

// applyRuleset 用 stdin 原子替换整张表。
func (a *Applier) applyRuleset(ctx context.Context, family, ruleset string) error {
	// 「声明表（不存在则创建）→ 删表 → 重建」写进同一次 nft -f，由内核作为一个
	// 事务提交。分两条命令时，新规则加载失败会留下旧表已删、新表没建的无隔离窗口。
	replace := fmt.Sprintf("table %s %s\ndelete table %s %s\n", family, tableName, family, tableName)
	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(replace + ruleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply %s %s ruleset: %v (%s)", family, tableName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// inetRuleset 生成 inet 家族规则集（INPUT 端口白名单 + FORWARD 内网拦截）。
func inetRuleset(ports []int, subnets []string, ipv4 string, fwdPorts map[string][]int) string {
	var b strings.Builder
	b.WriteString("table inet " + tableName + " {\n")

	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority -10; policy accept;\n")
	b.WriteString("\t\tip saddr " + ipv4 + " jump allow\n")
	b.WriteString("\t}\n")
	b.WriteString("\tchain allow {\n")
	b.WriteString("\t\tct state established,related accept\n")
	// DNS 放行：netavark 在自己表里也放行了 dport 53（{tcp,udp}），但 hostfw 的
	// INPUT 链 priority 更低、先执行，若不在此显式放行，容器查询网关 DNS 会被
	// counter drop 终结。当前项目 --disable-dns、容器走公网 DNS（FORWARD 链），
	// 此处放行主要消除「将来启用 aardvark-dns 代理即断网」的隐患。
	b.WriteString("\t\tudp dport 53 accept\n")
	b.WriteString("\t\ttcp dport 53 accept\n")
	if len(ports) > 0 {
		b.WriteString("\t\ttcp dport { " + joinPorts(ports) + " } accept\n")
	}
	// 端口转发的宿主端口（NAT 映射端口）：容器做代理时回环访问宿主公网 IP 上的
	// 转发端口，即使不在 host_allow_ports 里也放行。TCP/UDP 分别处理。
	if len(fwdPorts["tcp"]) > 0 {
		b.WriteString("\t\ttcp dport { " + joinPorts(fwdPorts["tcp"]) + " } accept\n")
	}
	if len(fwdPorts["udp"]) > 0 {
		b.WriteString("\t\tudp dport { " + joinPorts(fwdPorts["udp"]) + " } accept\n")
	}
	b.WriteString("\t\tcounter drop\n")
	b.WriteString("\t}\n")

	b.WriteString("\tchain forward {\n")
	b.WriteString("\t\ttype filter hook forward priority -10; policy accept;\n")
	b.WriteString("\t\tip saddr " + ipv4 + " jump fwd_allow\n")
	b.WriteString("\t}\n")
	b.WriteString("\tchain fwd_allow {\n")
	b.WriteString("\t\tct state established,related accept\n")
	for _, s := range subnets {
		b.WriteString("\t\tip daddr " + s + " accept\n")
	}
	for _, c := range privateCIDRs {
		b.WriteString("\t\tip daddr " + c + " drop\n")
	}
	b.WriteString("\t\taccept\n")
	b.WriteString("\t}\n")

	b.WriteString("}\n")
	return b.String()
}

// inet6Ruleset 生成 IPv6（ip6 家族）规则集，与 inetRuleset 对称：
// INPUT 端口白名单 + FORWARD 拦截内网（ULA / 链路本地）。
// 容器 IPv6 网段从 narwhal-net.json 读取（snat 模式 fd91:cafe:cafe:10::/64，
// subnet 模式为 /112），不硬编码。
func inet6Ruleset(ports []int, subnets []string, ipv6 string, fwdPorts map[string][]int) string {
	var b strings.Builder
	b.WriteString("table ip6 " + tableName + " {\n")

	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority -10; policy accept;\n")
	b.WriteString("\t\tip6 saddr " + ipv6 + " jump allow6\n")
	b.WriteString("\t}\n")
	b.WriteString("\tchain allow6 {\n")
	b.WriteString("\t\tct state established,related accept\n")
	// ICMPv6 是 IPv6 的基础协议：NDP 邻居发现/路由器通告都走它。不放行的话
	// 容器连网关 MAC 都解析不到（v4 用 ARP 在二层，不过 ip filter），
	// 所有 v6 流量一个包都发不出去。
	b.WriteString("\t\tmeta l4proto ipv6-icmp accept\n")
	// 与 inet 表一致：放行 DNS，避免启用 IPv6 DNS 代理后解析被拦。
	b.WriteString("\t\tudp dport 53 accept\n")
	b.WriteString("\t\ttcp dport 53 accept\n")
	if len(ports) > 0 {
		b.WriteString("\t\ttcp dport { " + joinPorts(ports) + " } accept\n")
	}
	// 端口转发宿主端口（NAT 映射端口），与 inet 表对称。
	if len(fwdPorts["tcp"]) > 0 {
		b.WriteString("\t\ttcp dport { " + joinPorts(fwdPorts["tcp"]) + " } accept\n")
	}
	if len(fwdPorts["udp"]) > 0 {
		b.WriteString("\t\tudp dport { " + joinPorts(fwdPorts["udp"]) + " } accept\n")
	}
	b.WriteString("\t\tcounter drop\n")
	b.WriteString("\t}\n")

	b.WriteString("\tchain forward {\n")
	b.WriteString("\t\ttype filter hook forward priority -10; policy accept;\n")
	b.WriteString("\t\tip6 saddr " + ipv6 + " jump fwd_allow6\n")
	b.WriteString("\t}\n")
	b.WriteString("\tchain fwd_allow6 {\n")
	b.WriteString("\t\tct state established,related accept\n")
	// IPv6 白名单：支持用户显式放行的 IPv6 CIDR。
	for _, s := range subnets {
		if isIPv6CIDR(s) {
			b.WriteString("\t\tip6 daddr " + s + " accept\n")
		}
	}
	// 默认拦 IPv6 内网：ULA（fc00::/7）与链路本地（fe80::/10）。
	for _, c := range privateIPv6CIDRs {
		b.WriteString("\t\tip6 daddr " + c + " drop\n")
	}
	b.WriteString("\t\taccept\n")
	b.WriteString("\t}\n")

	b.WriteString("}\n")
	return b.String()
}

// isIPv6CIDR 判断 CIDR 是否为 IPv6 网段。
func isIPv6CIDR(cidr string) bool {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return false
	}
	return ip.To4() == nil
}

// bridgeRuleset 生成 bridge 家族规则集（容器间二层隔离）。
func bridgeRuleset(bridge string, isolate bool) string {
	var b strings.Builder
	b.WriteString("table bridge " + tableName + " {\n")
	b.WriteString("\tchain forward {\n")
	b.WriteString("\t\ttype filter hook forward priority 0; policy accept;\n")
	if isolate {
		b.WriteString("\t\tmeta ibrname \"" + bridge + "\" meta obrname \"" + bridge + "\" counter drop\n")
	}
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String()
}

// bridgeNameFromFile 从 podman 的网络配置文件读取接口名。
func bridgeNameFromFile(path string) string {
	info := readNetworkFile(path)
	return info.bridge
}

// netInfo 是 narwhal-net.json 中 hostfw 关心的字段。
type netInfo struct {
	bridge string // 网桥接口名
	ipv4   string // IPv4 网段（CIDR），如 10.91.0.0/20
	ipv6   string // IPv6 网段（CIDR），如 fd91:cafe:cafe:10::/64；无则空
}

// readNetworkFile 从 narwhal-net.json 读取网桥名与网段。
// 任何读取/解析失败都返回零值 netInfo，调用方据此决定降级策略。
func readNetworkFile(path string) netInfo {
	var info netInfo
	data, err := os.ReadFile(path)
	if err != nil {
		return info
	}
	var doc struct {
		NetworkInterface string `json:"network_interface"`
		Subnets          []struct {
			Subnet string `json:"subnet"`
		} `json:"subnets"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return info
	}
	info.bridge = doc.NetworkInterface
	for _, s := range doc.Subnets {
		cidr := s.Subnet
		if ip, _, err := net.ParseCIDR(cidr); err == nil {
			if ip.To4() != nil {
				info.ipv4 = cidr
			} else {
				info.ipv6 = cidr
			}
		}
	}
	return info
}

// bridgeNameFromPodman 用 podman network inspect 动态查询接口名（带超时）。
func bridgeNameFromPodman() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "podman", "network", "inspect", "narwhal-net").Output()
	if err != nil {
		return ""
	}
	var nets []struct {
		NetworkInterface string `json:"network_interface"`
	}
	if err := json.Unmarshal(out, &nets); err != nil || len(nets) == 0 {
		return ""
	}
	return nets[0].NetworkInterface
}

// fingerprint 生成与配置一一对应的指纹，用于判断是否需要重建。
// 网段（ipv4/ipv6）与端口转发宿主端口（fwdPorts）也纳入指纹：
// 网络重建或转发规则增删时都能触发规则重建。
func fingerprint(ports []int, subnets []string, isolate bool, bridge, ipv4, ipv6 string, fwdPorts map[string][]int) string {
	cp := append([]int(nil), ports...)
	sort.Ints(cp)
	ps := make([]string, len(cp))
	for i, p := range cp {
		ps[i] = strconv.Itoa(p)
	}

	cs := append([]string(nil), subnets...)
	sort.Strings(cs)

	// fwdPorts 是 map，遍历顺序不定，需规范化成稳定字符串（协议:端口，升序）。
	fp := make([]string, 0, len(fwdPorts))
	for proto, list := range fwdPorts {
		pl := append([]int(nil), list...)
		sort.Ints(pl)
		for _, p := range pl {
			fp = append(fp, proto+":"+strconv.Itoa(p))
		}
	}
	sort.Strings(fp)

	return fmt.Sprintf("iso=%v|bridge=%s|ports=%s|subnets=%s|ipv4=%s|ipv6=%s|fwd=%s",
		isolate, bridge, strings.Join(ps, ","), strings.Join(cs, ","), ipv4, ipv6, strings.Join(fp, ","))
}

// joinPorts 把端口切片格式化为 nft 集合字面量（升序、去重），如 "80, 443, 8080"。
func joinPorts(ports []int) string {
	cp := append([]int(nil), ports...)
	sort.Ints(cp)
	strs := make([]string, 0, len(cp))
	last := -1
	for _, p := range cp {
		if p == last {
			continue
		}
		strs = append(strs, strconv.Itoa(p))
		last = p
	}
	return strings.Join(strs, ", ")
}
