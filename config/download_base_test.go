package config

import "testing"

// TestValidateDownloadBase 覆盖下载源校验的各条规则。
//
// 这些字段指向的内容会以 root 身份执行（updater 用 systemd-run 跑 install.sh），
// 因此协议与目标地址都必须受约束。
func TestValidateDownloadBase(t *testing.T) {
	cases := []struct {
		name          string
		raw           string
		allowInsecure bool
		allowInternal bool
		wantErr       bool
	}{
		{"empty-allowed", "", false, false, false},
		{"https-public", "https://github.com/tl0502/runman-agent/releases/latest/download", false, false, false},
		{"https-mirror", "https://mirror.example.com/rel/v0.5.3", false, false, false},
		{"http-rejected", "http://mirror.example.com/rel", false, false, true},
		{"http-allowed-with-flag", "http://mirror.example.com/rel", true, false, false},
		{"file-scheme-rejected", "file:///etc/passwd", false, false, true},
		{"ftp-scheme-rejected", "ftp://example.com/x", false, false, true},
		{"ssh-scheme-rejected", "ssh://example.com/x", false, false, true},
		{"loopback-ip-rejected", "https://127.0.0.1/x", false, false, true},
		{"private-ip-rejected", "https://192.168.1.10/x", false, false, true},
		{"link-local-rejected", "https://169.254.169.254/latest/meta-data", false, false, true},
		{"cgnat-rejected", "https://100.64.0.1/x", false, false, true},
		{"localhost-name-rejected", "https://localhost/x", false, false, true},
		{"internal-allowed-with-flag", "https://192.168.1.10/x", false, true, false},
		{"loopback-allowed-with-flag", "https://127.0.0.1/x", false, true, false},
		{"no-host-rejected", "https:///path", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateDownloadBase(tc.raw, tc.allowInsecure, tc.allowInternal)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.raw)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.raw, err)
			}
		})
	}
}
