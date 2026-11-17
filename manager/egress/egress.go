// Package egress 维护 Podman 网络的出站源地址（SNAT）。
//
// 依赖 netavark 的网络级选项 outbound_addr4：写了它，netavark 生成的规则是
// `snat to <ip>` 而不是默认的 masquerade。容器本身仍在 narwhal-net 的内网段里，
// 对外只体现为这一个地址。
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
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"runman-agent/config"
)

const (
	networkName   = "narwhal-net"
	networkFile   = "/etc/containers/networks/" + networkName + ".json"
	subnetCIDR    = "10.91.0.0/20"
	nftTable      = "netavark"
	optionKey     = "outbound_addr4"
	networkPerm   = 0o644
	subnetPattern = `ip saddr ` + subnetCIDR + ` jump (nv_[A-Za-z0-9_]+)`
)

var subnetRe = regexp.MustCompile(subnetPattern)

// Applier 负责把配置里的对外 IPv4 落到网络配置和防火墙规则上。
type Applier struct {
	cfg *config.Manager
	mu  sync.Mutex

	// lastApplied / lastKnown 用于避免每次轮询都重复执行破坏性重建。
	lastApplied string
	lastKnown   bool
}

func New(cfg *config.Manager) *Applier {
	return &Applier{cfg: cfg}
}

// Apply 让网络配置与防火墙规则和 cfg.PublicIPv4 保持一致。幂等。
func (a *Applier) Apply(ctx context.Context) error {
	if a.cfg.Get().VirtType != "podman" {
		return nil
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	want := a.cfg.Get().EgressIPv4()

	changed, err := writeNetworkOption(optionKey, want)
	if err != nil {
		return err
	}
	if !changed && a.lastKnown && a.lastApplied == want {
		return nil
	}
	if !changed && a.rulesMatch(ctx, want) {
		a.lastApplied, a.lastKnown = want, true
		return nil
	}

	if err := a.rebuild(ctx); err != nil {
		a.lastKnown = false
		return err
	}
	if !a.rulesMatch(ctx, want) {
		a.lastKnown = false
		return fmt.Errorf("nft rules did not converge to %q", describe(want))
	}

	a.lastApplied, a.lastKnown = want, true
	log.Printf("[Egress] container egress IPv4 set to %s", describe(want))
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
func (a *Applier) rulesMatch(ctx context.Context, want string) bool {
	out, err := exec.CommandContext(ctx, "nft", "list", "ruleset").Output()
	if err != nil {
		return false
	}
	ruleset := string(out)

	chain := subnetChain(ruleset)
	if chain == "" {
		return false
	}
	body := chainBody(ruleset, chain)
	if body == "" {
		return false
	}

	if want == "" {
		return strings.Contains(body, "masquerade") && !strings.Contains(body, "snat ")
	}
	return strings.Contains(body, "snat ip to "+want)
}

// subnetChain 从规则集里找出该子网对应的链名。
func subnetChain(ruleset string) string {
	m := subnetRe.FindStringSubmatch(ruleset)
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
