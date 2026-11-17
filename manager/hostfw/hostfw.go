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
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"

	"runman-agent/config"
)

const (
	tableName     = "runman-hostfw" // inet 与 bridge 不同家族，可同名
	subnetCIDR    = "10.91.0.0/20"
	networkFile   = "/etc/containers/networks/narwhal-net.json"
	defaultBridge = "podman1"
)

// privateCIDRs 是默认拦截的私有/保留网段（容器不该主动访问的内网）。
var privateCIDRs = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"100.64.0.0/10",
}

// Applier 让 nft 表与配置保持一致。幂等：仅在有变化时重建。
type Applier struct {
	cfg  *config.Manager
	mu   sync.Mutex
	last string // 上次应用的规则指纹
}

func New(cfg *config.Manager) *Applier {
	return &Applier{cfg: cfg}
}

// Apply 校验配置并（必要时）重建 inet 与 bridge 两张表。
func (a *Applier) Apply(ctx context.Context) error {
	if a.cfg.Get().VirtType != "podman" {
		return nil
	}

	conf := a.cfg.Get()
	ports := conf.AllowedHostPorts()
	subnets := conf.AllowedSubnets()
	isolate := conf.IsolateEnabled()
	bridge := bridgeName()
	fp := fingerprint(ports, subnets, isolate, bridge)

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.last == fp {
		return nil
	}

	if err := a.applyRuleset(ctx, "inet", inetRuleset(ports, subnets)); err != nil {
		return err
	}
	if err := a.applyRuleset(ctx, "bridge", bridgeRuleset(bridge, isolate)); err != nil {
		return err
	}

	a.last = fp
	log.Printf("[HostFW] applied: isolate=%v host_ports=%v allow_subnets=%v", isolate, ports, subnets)
	return nil
}

// applyRuleset 先删旧表（不存在则忽略），再用 stdin 原子灌入完整规则集。
func (a *Applier) applyRuleset(ctx context.Context, family, ruleset string) error {
	del := exec.CommandContext(ctx, "nft", "delete", "table", family, tableName)
	if out, err := del.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if !strings.Contains(msg, "No such file or directory") && !strings.Contains(msg, "does not exist") {
			log.Printf("[HostFW] delete table %s %s: %v (%s)", family, tableName, err, msg)
		}
	}

	cmd := exec.CommandContext(ctx, "nft", "-f", "-")
	cmd.Stdin = strings.NewReader(ruleset)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply %s %s ruleset: %v (%s)", family, tableName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// inetRuleset 生成 inet 家族规则集（INPUT 端口白名单 + FORWARD 内网拦截）。
func inetRuleset(ports []int, subnets []string) string {
	var b strings.Builder
	b.WriteString("table inet " + tableName + " {\n")

	b.WriteString("\tchain input {\n")
	b.WriteString("\t\ttype filter hook input priority -10; policy accept;\n")
	b.WriteString("\t\tip saddr " + subnetCIDR + " jump allow\n")
	b.WriteString("\t}\n")
	b.WriteString("\tchain allow {\n")
	b.WriteString("\t\tct state established,related accept\n")
	if len(ports) > 0 {
		strs := make([]string, len(ports))
		for i, p := range ports {
			strs[i] = strconv.Itoa(p)
		}
		b.WriteString("\t\ttcp dport { " + strings.Join(strs, ", ") + " } accept\n")
	}
	b.WriteString("\t\tcounter drop\n")
	b.WriteString("\t}\n")

	b.WriteString("\tchain forward {\n")
	b.WriteString("\t\ttype filter hook forward priority -10; policy accept;\n")
	b.WriteString("\t\tip saddr " + subnetCIDR + " jump fwd_allow\n")
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

// bridgeName 从 narwhal-net.json 读取桥接口名，失败回落默认值。
func bridgeName() string {
	data, err := os.ReadFile(networkFile)
	if err != nil {
		return defaultBridge
	}
	var doc struct {
		NetworkInterface string `json:"network_interface"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return defaultBridge
	}
	if doc.NetworkInterface != "" {
		return doc.NetworkInterface
	}
	return defaultBridge
}

// fingerprint 生成与配置一一对应的指纹，用于判断是否需要重建。
func fingerprint(ports []int, subnets []string, isolate bool, bridge string) string {
	cp := append([]int(nil), ports...)
	sort.Ints(cp)
	ps := make([]string, len(cp))
	for i, p := range cp {
		ps[i] = strconv.Itoa(p)
	}

	cs := append([]string(nil), subnets...)
	sort.Strings(cs)

	return fmt.Sprintf("iso=%v|bridge=%s|ports=%s|subnets=%s",
		isolate, bridge, strings.Join(ps, ","), strings.Join(cs, ","))
}
