package config

import "testing"

func TestIsPrivateIPv4(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"172.26.1.219", true},     // AWS 内网网卡
		{"10.91.0.26", true},       // 容器段
		{"192.168.1.2", true},      // RFC1918
		{"100.64.3.4", true},       // CGNAT
		{"127.0.0.1", true},        // 回环
		{"169.254.1.1", true},      // 链路本地
		{"0.0.0.0", true},          // 未指定
		{"3.112.32.200", false},    // 公网
		{"", false},                // 空串交给调用方
		{"myhost.ddns.net", false}, // 域名不做判断
		{"2406:da14::1", false},    // IPv6 不在此函数范围
	}
	for _, c := range cases {
		if got := IsPrivateIPv4(c.raw); got != c.want {
			t.Errorf("IsPrivateIPv4(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

func TestEntryHost(t *testing.T) {
	cases := []struct {
		name     string
		cfg      Config
		detected string
		want     string
	}{
		{
			name:     "手动 host 最优先",
			cfg:      Config{Host: "jump.example.com", PublicIPv4: "172.26.1.219"},
			detected: "3.112.32.200",
			want:     "jump.example.com",
		},
		{
			name:     "公网 public_ipv4 直接使用",
			cfg:      Config{PublicIPv4: "3.112.32.200"},
			detected: "1.1.1.1",
			want:     "3.112.32.200",
		},
		{
			name:     "1:1 NAT 内网 public_ipv4 不当作入口",
			cfg:      Config{PublicIPv4: "172.26.1.219"},
			detected: "3.112.32.200",
			want:     "3.112.32.200",
		},
		{
			name:     "内网部署无探测值时保留原值",
			cfg:      Config{PublicIPv4: "192.168.10.5"},
			detected: "",
			want:     "192.168.10.5",
		},
		{
			name:     "都为空时用探测值",
			cfg:      Config{},
			detected: "3.112.32.200",
			want:     "3.112.32.200",
		},
		{
			name:     "首尾空白被裁剪",
			cfg:      Config{PublicIPv4: " 172.26.1.219 "},
			detected: " 3.112.32.200 ",
			want:     "3.112.32.200",
		},
	}
	for _, c := range cases {
		if got := c.cfg.EntryHost(c.detected); got != c.want {
			t.Errorf("%s: EntryHost() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestEntryHostSource(t *testing.T) {
	cases := []struct {
		name     string
		cfg      Config
		detected string
		want     string
	}{
		{"手动 host", Config{Host: "my.ddns.net", PublicIPv4: "3.112.32.200"}, "1.1.1.1", "manual"},
		{"公网 public_ipv4", Config{PublicIPv4: "3.112.32.200"}, "1.1.1.1", "public_ipv4"},
		{"内网 public_ipv4 让位探测", Config{PublicIPv4: "172.26.1.219"}, "3.112.32.200", "auto"},
		{"内网 public_ipv4 无探测值", Config{PublicIPv4: "172.26.1.219"}, "", "public_ipv4"},
		{"仅探测值", Config{}, "3.112.32.200", "auto"},
		{"全空", Config{}, "", ""},
	}
	for _, c := range cases {
		if got := c.cfg.EntryHostSource(c.detected); got != c.want {
			t.Errorf("%s: EntryHostSource() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestEntryIPv6(t *testing.T) {
	// 只有 2406:da14::2 在「网卡上」。
	onHost := func(addr string) bool { return addr == "2406:da14::2" }
	cases := []struct {
		name      string
		cfg       Config
		detected  string
		wantValue string
		wantSrc   string
	}{
		{"未启用共享 IPv6 用探测值", Config{}, "2406:da14::1", "2406:da14::1", "auto"},
		{"启用且配置地址在网卡上", Config{IPv6Mode: "snat", IPv6Addr: "2406:da14::2"}, "2406:da14::1", "2406:da14::2", "config"},
		{"启用但配置地址已失效", Config{IPv6Mode: "snat", IPv6Addr: "2406:da14::9"}, "2406:da14::1", "2406:da14::1", "stale"},
		{"失效且无探测值时保留配置", Config{IPv6Mode: "snat", IPv6Addr: "2406:da14::9"}, "", "2406:da14::9", "config"},
		{"全空", Config{}, "", "", ""},
	}
	for _, c := range cases {
		v, src := c.cfg.EntryIPv6(c.detected, onHost)
		if v != c.wantValue || src != c.wantSrc {
			t.Errorf("%s: EntryIPv6() = (%q,%q), want (%q,%q)", c.name, v, src, c.wantValue, c.wantSrc)
		}
	}
}
