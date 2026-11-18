package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"runman-agent/config"
)

const testAgentToken = "secret-agent-token-0123456789"

// setToken 直接把 Token 写进配置，模拟"已接入平台"的机器。
func setToken(t *testing.T, s *Server, token string) {
	t.Helper()
	if err := s.cfg.UpdateChecked(func(c *config.Config) error {
		c.Token = token
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// getConfigBody 请求一次 GET /api/config，返回原始 JSON；
// forbidden 非空时断言响应里不含该字符串。
func getConfigBody(t *testing.T, s *Server, forbidden string) map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleConfig(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if forbidden != "" && strings.Contains(rec.Body.String(), forbidden) {
		t.Fatalf("response leaked the secret: %s", rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	return body
}

func hasTokenField(t *testing.T, body map[string]json.RawMessage) bool {
	t.Helper()
	var v bool
	if err := json.Unmarshal(body["has_token"], &v); err != nil {
		t.Fatalf("has_token missing or invalid: %v", err)
	}
	return v
}

// TestConfigResponseOmitsToken 验证面板配置接口不回传平台凭据。
//
// 凭据一旦进入响应就会落到浏览器内存、开发者工具和任何前端日志里；面板本身
// 不需要明文（输入框留空 = 不修改），因此只暴露 has_token。
func TestConfigResponseOmitsToken(t *testing.T) {
	s := newConfigTestServer(t)
	setToken(t, s, testAgentToken)

	body := getConfigBody(t, s, testAgentToken)

	if _, ok := body["token"]; ok {
		t.Fatal("response still contains a token field")
	}
	if !hasTokenField(t, body) {
		t.Fatal("has_token should be true when a token is configured")
	}
}

// TestConfigResponseHasTokenFalseWhenUnset 验证未配置时只回传 false。
func TestConfigResponseHasTokenFalseWhenUnset(t *testing.T) {
	s := newConfigTestServer(t)

	body := getConfigBody(t, s, "")

	if hasTokenField(t, body) {
		t.Fatal("has_token should be false when no token is configured")
	}
}

// TestClearPlatformTokenUpdatesHasToken 验证清除凭据后 has_token 归位。
func TestClearPlatformTokenUpdatesHasToken(t *testing.T) {
	s := newConfigTestServer(t)
	setToken(t, s, testAgentToken)

	rec := httptest.NewRecorder()
	s.handleClearPlatformToken(rec, httptest.NewRequest(http.MethodDelete, "/api/platform/token", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.cfg.Get().Token; got != "" {
		t.Fatalf("token not cleared: %q", got)
	}
	if s.cfg.Get().PlatformEnabledOn() {
		t.Fatal("platform access should be disabled after clearing the token")
	}

	body := getConfigBody(t, s, testAgentToken)
	if hasTokenField(t, body) {
		t.Fatal("has_token should be false after clearing the token")
	}
}
