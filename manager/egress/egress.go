// Package egress 维护 Podman 网络的出站源地址（SNAT）。
//
// 依赖 netavark 的网络级选项 outbound_addr4 / outbound_addr6：写了它，
// netavark 生成的规则是 `snat to <ip>` 而不是默认的 masquerade。容器本身
// 仍在 narwhal-net 的内网段里，对外只体现为这两个地址。
//
// IPv6 只在 snat 模式（共享 IPv6）下接管：此时容器只有 ULA 内网地址，
// 出站需要 SNAT 成配置的单个公网 IPv6。subnet 模式下容器自带公网地址、
// netavark 也不做 v6 NAT（snat_ipv6=false），这里不写 outbound_addr6。
//
// 一个必须注意的行为：netavark 在规则链已存在时会跳过重建（日志里的
// "Found chain ..."），因此改完配置只执行 podman network reload 不会生效，
// 必须先把规则表删掉再重建。
package egress

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"runman-agent/config"
)

const (
	networkName  = "narwhal-net"
	networkFile  = "/etc/containers/networks/" + networkName + ".json"
	subnetCIDR   = "10.91.0.0/20"
	nftTable     = "netavark"
	option4Key   = "outbound_addr4"
	option6Key   = "outbound_addr6"
	networkPerm  = 0o644
	subnetFormat = `%s saddr %s jump (nv_[A-Za-z0-9_-]+)`
)

// Applier 负责把配置里的对外地址落到网络配置和防火墙规则上。
type Applier struct {
	cfg *config.Manager
	mu  sync.Mutex

	// last4 / last6 用于避免每次轮询都重复执行破坏性重建。
	last4, last6 string
	lastKnown    bool
}

func New(cfg *config.Manager) *Applier {
	return &Applier{cfg: cfg}
}

// Apply 让网络配置与防火墙规则和 cfg 的对外地址保持一致。幂等。
func (a *Applier) Apply(ctx context.Context) error {
	conf := a.cfg.Get()
	if conf.VirtType != "podman" {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	want4 := conf.EgressIPv4()
	// IPv6 出口只在容器网络真的带 v6 段时才接管：没有 v6 段的网络里
	// netavark 不会生成 v6 NAT 规则，写 outbound_addr6 无意义且会让
	// 规则一致性校验永远失败（每 10 秒重试刷日志）。
	want6 := ""
	if conf.SharedIPv6Enabled() {
		if _, v6Subnet := networkSubnets(); v6Subnet != "" {
			want6 = conf.EgressIPv6()
		}
	}

	changed4, err := writeNetworkOption(option4Key, want4)
	if err != nil {
		return err
	}
	// 改动两次写文件之间只隔毫秒级，且 Apply 有互斥；随后统一 rebuild。
	changed6, err := writeNetworkOption(option6Key, want6)
	if err != nil {
		return err
	}
	changed := changed4 || changed6

	if !changed && a.lastKnown && a.last4 == want4 && a.last6 == want6 {
		return nil
	}
	if !changed && a.rulesMatch(ctx, want4, want6) {
		a.last4, a.last6, a.lastKnown = want4, want6, true
		return nil
	}

	if err := a.rebuild(ctx); err != nil {
		a.lastKnown = false
		return err
	}
	if !a.rulesMatch(ctx, want4, want6) {
		a.lastKnown = false
		return fmt.Errorf("nft rules did not converge to v4=%q v6=%q", describe(want4), describe(want6))
	}

	a.last4, a.last6, a.lastKnown = want4, want6, true
	log.Printf("[Egress] container egress set to IPv4=%s IPv6=%s", describe(want4), describe(want6))
	return nil
}

func describe(ip string) string {
	if ip == "" {
		return "<default masquerade>"
	}
	return ip
}

// writeNetworkOption 把某个字符串选项写进（或从）网络配置里移除，返回是否有改动。
// want 为空表示删除该选项。
func writeNetworkOption(key, want string) (bool, error) {
	raw, err := os.ReadFile(networkFile)
	if err != nil {
		if os.IsNotExist(err) {
			return false, fmt.Errorf("network %q not found at %s", networkName, networkFile)
		}
		return false, err
	}

	// 用 RawMessage 保留未知字段，避免重写时丢 podman 自己的数据。
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false, fmt.Errorf("parse %s: %w", networkFile, err)
	}

	options := map[string]string{}
	if v, ok := doc["options"]; ok && len(v) > 0 && string(v) != "null" {
		if err := json.Unmarshal(v, &options); err != nil {
			return false, fmt.Errorf("parse options in %s: %w", networkFile, err)
		}
	}

	current, has := options[key]
	if want == "" {
		if !has {
			return false, nil
		}
		delete(options, key)
	} else {
		if has && current == want {
			return false, nil
		}
		options[key] = want
	}

	if len(options) == 0 {
		delete(doc, "options")
	} else {
		encoded, err := json.Marshal(options)
		if err != nil {
			return false, err
		}
		doc["options"] = encoded
	}

	out, err := json.MarshalIndent(doc, "", "     ")
	if err != nil {
		return false, err
	}
	out = append(out, '\n')

	tmp := networkFile + ".tmp"
	if err := os.WriteFile(tmp, out, networkPerm); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, networkFile); err != nil {
		_ = os.Remove(tmp)
		return false, err
	}
	return true, nil
}

// rebuild 删掉 netavark 的规则表再让 podman 重建。
//
// 直接 reload 不够：netavark 发现链已存在就只做增量更新，SNAT 规则不会重建。
func (a *Applier) rebuild(ctx context.Context) error {
	if out, err := exec.CommandContext(ctx, "nft", "delete", "table", "inet", nftTable).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		// 表本来就不存在时不算失败。
		if !strings.Contains(msg, "No such file or directory") {
			log.Printf("[Egress] delete nft table %s: %v (%s)", nftTable, err, msg)
		}
	}

	out, err := exec.CommandContext(ctx, "podman", "network", "reload", "--all").CombinedOutput()
	if err != nil {
		return fmt.Errorf("podman network reload --all: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// rulesMatch 检查当前 nft 规则是否已经是期望形态。
func (a *Applier) rulesMatch(ctx context.Context, want4, want6 string) bool {
	out, err := exec.CommandContext(ctx, "nft", "list", "ruleset").Output()
	if err != nil {
		return false
	}
	ruleset := string(out)

	// 网络里还没有容器时 netavark 根本不会创建规则表（podman network reload
	// 对空网络不产生任何规则）。这种状态下没有任何可校验的目标，直接视为一致，
	// 否则新装机会每 10 秒报一次「未收敛」并反复重建。
	if !strings.Contains(ruleset, "table inet "+nftTable) {
		return true
	}

	v4Subnet, v6Subnet := networkSubnets()

	// IPv4：子网链里的 snat/masquerade 形态。
	if v4Subnet == "" {
		v4Subnet = subnetCIDR
	}
	chain4 := subnetChain(ruleset, "ip", v4Subnet)
	if chain4 == "" {
		return false
	}
	body4 := chainBody(ruleset, chain4)
	if body4 == "" {
		return false
	}
	if want4 == "" {
		if !strings.Contains(body4, "masquerade") || strings.Contains(body4, "snat ") {
			return false
		}
	} else if !strings.Contains(body4, "snat ip to "+want4) && !strings.Contains(body4, "snat to "+want4) {
		return false
	}

	// IPv6：网络没有 v6 段时不参与；subnet 模式（want6 为空）下 netavark
	// 不做 v6 NAT，链可能不存在，只要没有残留的 snat 规则即可。
	if v6Subnet == "" {
		return want6 == ""
	}
	chain6 := subnetChain(ruleset, "ip6", v6Subnet)
	if want6 == "" {
		if chain6 == "" {
			return true
		}
		body6 := chainBody(ruleset, chain6)
		return !strings.Contains(body6, "snat ip6 to") && !strings.Contains(body6, "snat ip to")
	}
	if chain6 == "" {
		return false
	}
	body6 := chainBody(ruleset, chain6)
	return strings.Contains(body6, "snat ip6 to "+want6) || strings.Contains(body6, "snat ip to "+want6)
}

// networkSubnets 从 narwhal-net.json 读取容器网络的 v4/v6 子网。
// 读取失败时返回空串，调用方回落到默认值或不校验。
func networkSubnets() (v4, v6 string) {
	raw, err := os.ReadFile(networkFile)
	if err != nil {
		return "", ""
	}
	var doc struct {
		Subnets []struct {
			Subnet string `json:"subnet"`
		} `json:"subnets"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", ""
	}
	for _, s := range doc.Subnets {
		if ip, _, err := net.ParseCIDR(s.Subnet); err == nil {
			if ip.To4() != nil {
				v4 = s.Subnet
			} else {
				v6 = s.Subnet
			}
		}
	}
	return v4, v6
}

// subnetChain 从规则集里找出指定协议族子网对应的链名。
// family 是 nft 的匹配关键字："ip" 或 "ip6"。
func subnetChain(ruleset, family, subnet string) string {
	if subnet == "" {
		return ""
	}
	re := regexp.MustCompile(fmt.Sprintf(subnetFormat, regexp.QuoteMeta(family), regexp.QuoteMeta(subnet)))
	m := re.FindStringSubmatch(ruleset)
	if len(m) != 2 {
		return ""
	}
	return m[1]
}

// chainBody 返回指定链的内容（不含链名那一行）。
func chainBody(ruleset, chain string) string {
	lines := strings.Split(ruleset, "\n")
	var body []string
	inChain := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !inChain {
			if strings.HasPrefix(trimmed, "chain "+chain+" {") {
				inChain = true
			}
			continue
		}
		if trimmed == "}" {
			break
		}
		body = append(body, trimmed)
	}
	return strings.Join(body, "\n")
}
