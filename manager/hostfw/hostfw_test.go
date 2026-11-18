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
	rules := inet6Ruleset([]int{8792}, []string{"fd91:cafe:cafe:10::/64"}, "fd91:cafe:cafe:10::/64")
	for _, want := range []string{
		"table inet6 runman-hostfw",
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
	rules := inetRuleset([]int{}, nil, "10.91.0.0/20")
	if !strings.Contains(rules, "udp dport 53 accept") || !strings.Contains(rules, "tcp dport 53 accept") {
		t.Fatalf("inet ruleset should allow DNS:\n%s", rules)
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
