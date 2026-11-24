package hostfw

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBridgeRulesetIsolateOff 验证隔离关闭时不生成 drop 规则，且不引用桥名。
func TestBridgeRulesetIsolateOff(t *testing.T) {
	rules := bridgeRuleset("", false)
	if strings.Contains(rules, "counter drop") {
		t.Fatalf("isolate=false 不应生成 drop 规则:\n%s", rules)
	}
	if strings.Contains(rules, "ibrname") {
		t.Fatalf("isolate=false 不应引用桥名:\n%s", rules)
	}
}

// TestBridgeRulesetIsolateOn 验证隔离开启时生成带桥名的 drop 规则。
func TestBridgeRulesetIsolateOn(t *testing.T) {
	rules := bridgeRuleset("podman1", true)
	if !strings.Contains(rules, `meta ibrname "podman1" meta obrname "podman1" counter drop`) {
		t.Fatalf("isolate=true 应生成带桥名的 drop 规则:\n%s", rules)
	}
}

// TestBridgeNameFromFileMissing 验证配置文件读不到时返回空串，
// 而不是猜一个默认名——空串会由 Apply 判定为错误，避免规则落到错误的桥上。
func TestBridgeNameFromFileMissing(t *testing.T) {
	if got := bridgeNameFromFile(filepath.Join(t.TempDir(), "does-not-exist.json")); got != "" {
		t.Fatalf("missing file should return empty, got %q", got)
	}
}

// TestBridgeNameFromFileValid 验证能正确读出 network_interface 字段。
func TestBridgeNameFromFileValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "narwhal-net.json")
	if err := os.WriteFile(path, []byte(`{"name":"narwhal-net","network_interface":"podman2"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := bridgeNameFromFile(path); got != "podman2" {
		t.Fatalf("expected podman2, got %q", got)
	}
}

// TestBridgeNameFromFileMalformed 验证 JSON 损坏时返回空串而非 panic。
func TestBridgeNameFromFileMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "narwhal-net.json")
	if err := os.WriteFile(path, []byte(`{broken json`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := bridgeNameFromFile(path); got != "" {
		t.Fatalf("malformed json should return empty, got %q", got)
	}
}

// TestReadNetworkFile 验证能同时读出网桥名与 IPv4/IPv6 网段。
func TestReadNetworkFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "narwhal-net.json")
	doc := `{
		"name": "narwhal-net",
		"network_interface": "podman1",
		"subnets": [
			{"subnet": "10.91.0.0/20", "gateway": "10.91.0.1"},
			{"subnet": "fd91:cafe:cafe:10::/64", "gateway": "fd91:cafe:cafe:10::1"}
		]
	}`
	if err := os.WriteFile(path, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	info := readNetworkFile(path)
	if info.bridge != "podman1" {
		t.Fatalf("bridge = %q, want podman1", info.bridge)
	}
	if info.ipv4 != "10.91.0.0/20" {
		t.Fatalf("ipv4 = %q, want 10.91.0.0/20", info.ipv4)
	}
	if info.ipv6 != "fd91:cafe:cafe:10::/64" {
		t.Fatalf("ipv6 = %q, want fd91:cafe:cafe:10::/64", info.ipv6)
	}
}

// TestReadNetworkFileNoIPv6 验证无 IPv6 网段时 ipv6 为空。
func TestReadNetworkFileNoIPv6(t *testing.T) {
	path := filepath.Join(t.TempDir(), "narwhal-net.json")
	doc := `{"network_interface":"podman0","subnets":[{"subnet":"10.91.0.0/20"}]}`
	if err := os.WriteFile(path, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}
	info := readNetworkFile(path)
	if info.ipv4 != "10.91.0.0/20" || info.ipv6 != "" {
		t.Fatalf("unexpected: ipv4=%q ipv6=%q", info.ipv4, info.ipv6)
	}
}

// TestInet6Ruleset 验证 IPv6 规则集含容器网段与 ULA/链路本地拦截。
func TestInet6Ruleset(t *testing.T) {
	rules := inet6Ruleset([]int{8792}, []string{"fd91:cafe:cafe:10::/64"}, "fd91:cafe:cafe:10::/64", nil)
	for _, want := range []string{
		"table ip6 runman-hostfw",
		"ip6 saddr fd91:cafe:cafe:10::/64 jump allow6",
		"ip6 daddr fc00::/7 drop",
		"ip6 daddr fe80::/10 drop",
		"udp dport 53 accept",
	} {
		if !strings.Contains(rules, want) {
			t.Fatalf("inet6 ruleset missing %q:\n%s", want, rules)
		}
	}
}

// TestInetRulesetHasDNSAllow 验证 inet 表补了 DNS 放行（P2-7）。
func TestInetRulesetHasDNSAllow(t *testing.T) {
	rules := inetRuleset([]int{}, nil, "10.91.0.0/20", nil)
	if !strings.Contains(rules, "udp dport 53 accept") || !strings.Contains(rules, "tcp dport 53 accept") {
		t.Fatalf("inet ruleset should allow DNS:\n%s", rules)
	}
}

// TestInetRulesetForwardPorts 验证端口转发宿主端口（NAT 映射端口）被放行：
// TCP/UDP 分别生成 accept，且不依赖 host_allow_ports 白名单。
func TestInetRulesetForwardPorts(t *testing.T) {
	fwd := map[string][]int{"tcp": {22001, 49624}, "udp": {51820}}
	rules := inetRuleset(nil, nil, "10.91.0.0/20", fwd)
	for _, want := range []string{
		"tcp dport { 22001, 49624 } accept",
		"udp dport { 51820 } accept",
	} {
		if !strings.Contains(rules, want) {
			t.Fatalf("inet ruleset missing forward-port allow %q:\n%s", want, rules)
		}
	}
}

// TestInetRulesetForwardPortsNoDup 验证转发端口与白名单重叠时不去重崩溃、
// 且空转发端口不产生空集合（`{ }`）。
func TestInetRulesetForwardPortsNoDup(t *testing.T) {
	rules := inetRuleset([]int{22001}, nil, "10.91.0.0/20", map[string][]int{"tcp": {22001}})
	if strings.Contains(rules, "{ }") {
		t.Fatalf("empty port set should not appear:\n%s", rules)
	}
	// 白名单 22001 + 转发 22001，规则里至少应有一条 tcp 放行（重复无害）。
	if !strings.Contains(rules, "22001") {
		t.Fatalf("22001 should still be accepted:\n%s", rules)
	}
}

// TestInet6RulesetForwardPorts 验证 IPv6 规则集与 inet 表对称，同样放行转发端口。
func TestInet6RulesetForwardPorts(t *testing.T) {
	fwd := map[string][]int{"tcp": {22001}, "udp": {51820}}
	rules := inet6Ruleset(nil, nil, "fd91:cafe:cafe:10::/64", fwd)
	for _, want := range []string{
		"tcp dport { 22001 } accept",
		"udp dport { 51820 } accept",
	} {
		if !strings.Contains(rules, want) {
			t.Fatalf("inet6 ruleset missing forward-port allow %q:\n%s", want, rules)
		}
	}
}

// TestFingerprintIncludesForwardPorts 验证指纹纳入转发端口：增删转发规则会改变指纹。
func TestFingerprintIncludesForwardPorts(t *testing.T) {
	a := fingerprint(nil, nil, false, "", "10.91.0.0/20", "", map[string][]int{"tcp": {22001}})
	b := fingerprint(nil, nil, false, "", "10.91.0.0/20", "", map[string][]int{"tcp": {22001, 22002}})
	if a == b {
		t.Fatalf("fingerprint should change when forward ports change: %q", a)
	}
	// 遍历顺序不影响指纹稳定性。
	c := fingerprint(nil, nil, false, "", "10.91.0.0/20", "", map[string][]int{"tcp": {22002, 22001}})
	if c != b {
		t.Fatalf("fingerprint should be order-independent: %q vs %q", c, b)
	}
}

// TestIsIPv6CIDR 验证 CIDR 版本判断。
func TestIsIPv6CIDR(t *testing.T) {
	if isIPv6CIDR("10.91.0.0/20") {
		t.Fatal("10.91.0.0/20 should be IPv4")
	}
	if !isIPv6CIDR("fd91:cafe:cafe:10::/64") {
		t.Fatal("fd91:cafe:cafe:10::/64 should be IPv6")
	}
	if isIPv6CIDR("not-a-cidr") {
		t.Fatal("invalid cidr should be false")
	}
}
