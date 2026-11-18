package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"runman-agent/config"
)

// newConfigTestServer 造一个只带 config 的最小 Server，用于打 handleConfig。
// handleConfig 只依赖 s.cfg，不触碰数据库或 manager。
func newConfigTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"web_user":""}`), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: mgr}
}

// postConfig 提交一次 /api/config POST。
func postConfig(t *testing.T, s *Server, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/config", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleConfig(rec, req)
	return rec
}

// TestConfigRejectsUnsafeDownloadBase 验证恶意下载源被拒绝且不落盘。
//
// 这些字段指向的内容会以 root 身份执行（updater 用 systemd-run 跑 install.sh），
// 因此校验失败必须让请求失败、配置保持原值——静默忽略会让面板显示「已保存」
// 而实际未生效。
func TestConfigRejectsUnsafeDownloadBase(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		wantOK  bool
	}{
		{"http-download-base", map[string]any{"update_base_url": "http://attacker.example.com/x"}, false},
		{"http-api-url", map[string]any{"update_api_url": "http://attacker.example.com/api"}, false},
		{"file-scheme", map[string]any{"update_base_url": "file:///etc/passwd"}, false},
		{"loopback", map[string]any{"update_base_url": "https://127.0.0.1/x"}, false},
		{"private-ip", map[string]any{"update_base_url": "https://192.168.1.10/x"}, false},
		{"cloud-metadata", map[string]any{"update_api_url": "https://169.254.169.254/latest/meta-data"}, false},
		{"netavark-loopback", map[string]any{"netavark_base": "https://127.0.0.1/x"}, false},
		{"https-public-ok", map[string]any{"update_base_url": "https://mirror.example.com/rel/v0.5.3"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newConfigTestServer(t)
			before := s.cfg.Get().UpdateBaseURL

			rec := postConfig(t, s, tc.payload)
			if tc.wantOK {
				if rec.Code != http.StatusOK {
					t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
				}
				return
			}
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
			}
			// 校验失败必须不落盘。
			after := s.cfg.Get().UpdateBaseURL
			if after != before {
				t.Fatalf("rejected value was persisted: %q", after)
			}
		})
	}
}

// TestConfigAllowsInsecureWithOptIn 验证显式开启后可以配置内网/明文源。
//
// 企业内网镜像站是合法需求，不能一刀切禁止；开关存在即可。
func TestConfigAllowsInsecureWithOptIn(t *testing.T) {
	s := newConfigTestServer(t)

	rec := postConfig(t, s, map[string]any{
		"allow_internal_download": true,
		"update_base_url":         "https://192.168.1.10/rel",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with allow_internal_download, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.cfg.Get().UpdateBaseURL; got != "https://192.168.1.10/rel" {
		t.Fatalf("value not persisted: %q", got)
	}

	rec = postConfig(t, s, map[string]any{
		"allow_insecure_download": true,
		"update_base_url":         "http://mirror.example.com/rel",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with allow_insecure_download, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestConfigRejectionLeavesOtherFieldsIntact 验证下载源被拒时，
// 同一次提交里的其它字段也不会落盘（UpdateChecked 的副本语义）。
func TestConfigRejectionLeavesOtherFieldsIntact(t *testing.T) {
	s := newConfigTestServer(t)

	rec := postConfig(t, s, map[string]any{
		"site_name":       "changed",
		"update_base_url": "http://attacker.example.com/x",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if got := s.cfg.Get().SiteName; got == "changed" {
		t.Fatalf("unrelated field was persisted despite rejection: %q", got)
	}
}
