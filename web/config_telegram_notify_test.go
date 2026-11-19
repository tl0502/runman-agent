package web

import (
	"encoding/json"
	"net/http"
	"testing"
)

// postTelegram 提交一次 telegram 段（POST /api/config）并断言成功。
func postTelegram(t *testing.T, s *Server, tg map[string]any) {
	t.Helper()
	rec := postConfig(t, s, map[string]any{"telegram": tg})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// boolAt 断言 telegram 段里某开关的值与指针状态。
func boolAt(t *testing.T, tg map[string]any, key string, want bool) {
	t.Helper()
	raw, ok := tg[key]
	if !ok {
		t.Fatalf("telegram.%s missing in GET response", key)
	}
	v, ok := raw.(bool)
	if !ok {
		t.Fatalf("telegram.%s not a bool: %v", key, raw)
	}
	if v != want {
		t.Fatalf("telegram.%s = %v, want %v", key, v, want)
	}
}

// TestTelegramNotifySwitchesPatch 验证逐事件推送开关的提交语义：
//
// 1. 显式提交生效（新前端总是全量提交布尔值）；
// 2. 字段缺失（旧版缓存页不认识这些字段）= 未提交，保留现有值——
// 否则升级后旧页一次保存会把用户已按事件关掉的推送全部重开；
// 3. GET 响应带全部逐事件字段，前端据此渲染勾选态。
func TestTelegramNotifySwitchesPatch(t *testing.T) {
	s := newConfigTestServer(t)

	// 预置：用户通过新前端关掉「自动封禁」推送。
	postTelegram(t, s, map[string]any{
		"enabled": true, "bot_token": "tks-7f2b91-mark", "chat_id": "c1",
		"patrol_notify": true, "login_ok_notify": true,
		"auto_ban_notify": false, "login_blocked_notify": true,
	})
	if got := s.cfg.Get().Telegram.AutoBanNotify; got == nil || *got {
		t.Fatalf("explicit false auto_ban_notify not applied: %v", got)
	}

	// 旧版前端的提交体：只有它认识的老字段，逐事件字段缺失 → 保留现有值。
	postTelegram(t, s, map[string]any{
		"enabled": true, "bot_token": "", "chat_id": "c1", "login_enabled": true,
	})
	tg := s.cfg.Get().Telegram
	if got := tg.AutoBanNotify; got == nil || *got {
		t.Fatalf("missing auto_ban_notify must keep existing false, got %v", got)
	}
	if got := tg.PatrolNotify; got == nil || !*got {
		t.Fatalf("missing patrol_notify must keep existing true, got %v", got)
	}
	if tg.BotToken != "tks-7f2b91-mark" {
		t.Fatalf("empty bot_token must keep existing value, got %q", tg.BotToken)
	}

	// 显式重开：新前端提交 true 生效。
	postTelegram(t, s, map[string]any{
		"enabled": true, "chat_id": "c1", "auto_ban_notify": true,
	})
	if got := s.cfg.Get().Telegram.AutoBanNotify; got == nil || !*got {
		t.Fatalf("explicit true auto_ban_notify not applied: %v", got)
	}

	// GET 响应带全部逐事件字段，bot_token 恒为空。
	body := getConfigBody(t, s, `"tks-7f2b91-mark"`)
	var resp map[string]any
	if err := json.Unmarshal(body["telegram"], &resp); err != nil {
		t.Fatalf("invalid telegram section: %v", err)
	}
	boolAt(t, resp, "patrol_notify", true)
	boolAt(t, resp, "login_ok_notify", true)
	boolAt(t, resp, "auto_ban_notify", true)
	boolAt(t, resp, "login_blocked_notify", true)
}
