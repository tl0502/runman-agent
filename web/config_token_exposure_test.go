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

// ── Telegram Bot Token：与平台 Token 同基线 ──────────────────────────────────

// setTelegramToken 直接把 bot_token 写进配置，模拟已配置通知通道的机器。
func setTelegramToken(t *testing.T, s *Server, token string) {
	t.Helper()
	if err := s.cfg.UpdateChecked(func(c *config.Config) error {
		c.Telegram.BotToken = token
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestConfigResponseOmitsTelegramBotToken 验证 GET /api/config 不回传
// telegram.bot_token 明文，只回 telegram_has_token（凭据与平台 Token 同口径）。
func TestConfigResponseOmitsTelegramBotToken(t *testing.T) {
	const tgToken = "secret-bot-token-987654321"
	s := newConfigTestServer(t)
	setTelegramToken(t, s, tgToken)

	body := getConfigBody(t, s, tgToken)

	var tg struct {
		BotToken string `json:"bot_token"`
	}
	if err := json.Unmarshal(body["telegram"], &tg); err != nil {
		t.Fatalf("telegram segment invalid: %v", err)
	}
	if tg.BotToken != "" {
		t.Fatalf("bot_token should be masked, got %q", tg.BotToken)
	}
	var has bool
	if err := json.Unmarshal(body["telegram_has_token"], &has); err != nil {
		t.Fatalf("telegram_has_token missing: %v", err)
	}
	if !has {
		t.Fatal("telegram_has_token should be true when a token is configured")
	}
}

// TestConfigTelegramTokenEmptyPreserves 验证 POST /api/config 的
// bot_token 留空 = 保留现有值（整体替换语义下防止每次保存清掉 Token）。
func TestConfigTelegramTokenEmptyPreserves(t *testing.T) {
	s := newConfigTestServer(t)
	setTelegramToken(t, s, "original-bot-token")

	postConfig(t, s, map[string]any{
		"telegram": map[string]any{"enabled": true, "bot_token": "", "chat_id": "-100123"},
	})

	tg := s.cfg.Get().Telegram
	if tg.BotToken != "original-bot-token" {
		t.Fatalf("bot_token should be preserved, got %q", tg.BotToken)
	}
	if tg.ChatID != "-100123" {
		t.Fatalf("chat_id should be updated, got %q", tg.ChatID)
	}
}

// TestConfigTelegramLoginNotifyDefaultOn 验证 login_enabled 缺省为开（nil→true）：
// 通道本身是显式开启的，打开即同意接收各类告警，不应有第二层静默关闭。
func TestConfigTelegramLoginNotifyDefaultOn(t *testing.T) {
	s := newConfigTestServer(t)
	postConfig(t, s, map[string]any{
		"telegram": map[string]any{"enabled": true, "bot_token": "t", "chat_id": "c"},
	})
	if !s.cfg.Get().Telegram.LoginNotifyOn() {
		t.Fatal("login notifications should default to on when login_enabled omitted")
	}

	postConfig(t, s, map[string]any{
		"telegram": map[string]any{"enabled": true, "bot_token": "", "chat_id": "c", "login_enabled": false},
	})
	if s.cfg.Get().Telegram.LoginNotifyOn() {
		t.Fatal("login notifications should be off when login_enabled=false")
	}
}
