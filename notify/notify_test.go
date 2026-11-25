package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"runman-agent/config"
)

func boolPtr(b bool) *bool { return &b }

// newTestNotifier 构造指向本地 httptest 服务的 Notifier（替换 Telegram API 地址），
// 预置「通道开启 + 凭据齐全」的配置，mutate 可再覆写字段。
// 返回通知器与请求命中计数。
func newTestNotifier(t *testing.T, mutate func(c *config.Config)) (*Notifier, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"web_user":""}`), 0600); err != nil {
		t.Fatal(err)
	}
	mgr, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.UpdateChecked(func(c *config.Config) error {
		c.Telegram = config.TelegramConfig{
			Enabled:  boolPtr(true),
			BotToken: "test-token",
			ChatID:   "test-chat",
		}
		if mutate != nil {
			mutate(c)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	n := New(mgr, nil)
	n.apiBase = srv.URL
	return n, &hits
}

// waitHits 等待异步发送落地（Send 是 fire-and-forget goroutine）。
func waitHits(t *testing.T, hits *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hits.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected >= %d telegram calls, got %d", want, hits.Load())
}

// TestSendGating 验证 Send 的门控：通道关/凭据缺/逐事件关 均不外发；
// login_enabled 只约束登录类三个事件，不影响巡查类。
func TestSendGating(t *testing.T) {
	cases := []struct {
		name   string
		kind   Kind
		mutate func(c *config.Config)
		want   int32
	}{
		{"通道关闭不发", KindPatrol, func(c *config.Config) { c.Telegram.Enabled = boolPtr(false) }, 0},
		{"缺chat_id不发", KindPatrol, func(c *config.Config) { c.Telegram.ChatID = "" }, 0},
		{"巡查类默认推", KindPatrol, nil, 1},
		{"巡查开关关不发", KindPatrol, func(c *config.Config) { c.Telegram.PatrolNotify = boolPtr(false) }, 0},
		{"登录成功默认推", KindLoginOk, nil, 1},
		{"登录总开关关全不发", KindLoginOk, func(c *config.Config) { c.Telegram.LoginEnabled = boolPtr(false) }, 0},
		{"登录成功关不发", KindLoginOk, func(c *config.Config) { c.Telegram.LoginOkNotify = boolPtr(false) }, 0},
		{"自动封禁默认推", KindAutoBan, nil, 1},
		{"自动封禁关不发", KindAutoBan, func(c *config.Config) { c.Telegram.AutoBanNotify = boolPtr(false) }, 0},
		{"自动封禁关不影响登录成功", KindLoginOk, func(c *config.Config) { c.Telegram.AutoBanNotify = boolPtr(false) }, 1},
		{"登录被拦截默认推", KindLoginBlocked, nil, 1},
		{"登录被拦截关不发", KindLoginBlocked, func(c *config.Config) { c.Telegram.LoginBlockedNotify = boolPtr(false) }, 0},
		{"登录关不影响巡查", KindPatrol, func(c *config.Config) { c.Telegram.LoginEnabled = boolPtr(false) }, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n, hits := newTestNotifier(t, tc.mutate)
			n.Send(tc.kind, "测试文本")
			waitHits(t, hits, tc.want)
		})
	}
}

// TestNilNotifierSafe 验证未注入通知器时调用不 panic（Server/Service 可能为零值）。
func TestNilNotifierSafe(t *testing.T) {
	var n *Notifier
	n.Send(KindPatrol, "x")
	n.Send(KindLoginOk, "x")
	if err := n.SendTest(); err == nil {
		t.Fatal("expected error on nil notifier")
	}
}

// TestSendTestRequiresCredentials 验证测试发送在凭据缺失时直接报错、不发请求。
func TestSendTestRequiresCredentials(t *testing.T) {
	n, hits := newTestNotifier(t, func(c *config.Config) { c.Telegram.ChatID = "" })
	if err := n.SendTest(); err == nil {
		t.Fatal("expected error when chat_id missing")
	}
	if hits.Load() != 0 {
		t.Fatalf("no request expected, got %d", hits.Load())
	}
}

// TestSendTestSuccessAndHTTPError 验证测试发送真实走 HTTP：2xx 成功，非 2xx 转为错误。
func TestSendTestSuccessAndHTTPError(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer okSrv.Close()

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer badSrv.Close()

	n, _ := newTestNotifier(t, nil)
	n.apiBase = okSrv.URL
	if err := n.SendTest(); err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	n.apiBase = badSrv.URL
	if err := n.SendTest(); err == nil {
		t.Fatal("expected error on HTTP 401")
	}
}

// TestSendTestSurfacesTelegramDescription 验证非 2xx 时把 Telegram 的 description
// 带进错误（Q19），否则排障时无法区分 token 错、chat_id 错还是被限流。
func TestSendTestSurfacesTelegramDescription(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"ok":false,"description":"Unauthorized"}`))
	}))
	defer srv.Close()

	n, _ := newTestNotifier(t, nil)
	n.apiBase = srv.URL
	err := n.SendTest()
	if err == nil {
		t.Fatal("expected error on HTTP 401")
	}
	if !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("error 缺少 Telegram description: %v", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error 缺少状态码: %v", err)
	}
}

// TestSendTestHandlesOKFalse 验证 200 + ok=false 也不会被当成发送成功。
func TestSendTestHandlesOKFalse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":false,"description":"Bad Request: chat not found"}`))
	}))
	defer srv.Close()

	n, _ := newTestNotifier(t, nil)
	n.apiBase = srv.URL
	err := n.SendTest()
	if err == nil || !strings.Contains(err.Error(), "chat not found") {
		t.Fatalf("expected description in error, got %v", err)
	}
}

// TestIdentityPrefix 验证消息首行的身份前缀：站点名 (入口地址)。
// 多面板共用同一个 Bot Token/Chat ID 的场景下，靠首行区分消息来自哪台机器。
func TestIdentityPrefix(t *testing.T) {
	newCapture := func(t *testing.T, mutate func(c *config.Config), entryAddr func() string) (*Notifier, func() string) {
		t.Helper()
		var mu sync.Mutex
		got := ""
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			got = string(b)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(`{"web_user":""}`), 0600); err != nil {
			t.Fatal(err)
		}
		mgr, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := mgr.UpdateChecked(func(c *config.Config) error {
			c.Telegram = config.TelegramConfig{
				Enabled:  boolPtr(true),
				BotToken: "test-token",
				ChatID:   "test-chat",
			}
			if mutate != nil {
				mutate(c)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		n := New(mgr, entryAddr)
		n.apiBase = srv.URL
		return n, func() string {
			mu.Lock()
			defer mu.Unlock()
			return got
		}
	}
	waitText := func(t *testing.T, read func() string) map[string]string {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			raw := read()
			if raw != "" {
				var m map[string]string
				if err := json.Unmarshal([]byte(raw), &m); err != nil {
					t.Fatalf("invalid body: %v", err)
				}
				return m
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("no request captured within deadline")
		return nil
	}

	t.Run("站点名与入口地址都在", func(t *testing.T) {
		n, read := newCapture(t, func(c *config.Config) { c.SiteName = "测试站" }, func() string { return "203.0.113.9" })
		n.Send(KindPatrol, "正文第二行")
		m := waitText(t, read)
		if want := "测试站 (203.0.113.9)\n正文第二行"; m["text"] != want {
			t.Fatalf("text = %q, want %q", m["text"], want)
		}
	})

	t.Run("未改名回落默认品牌名", func(t *testing.T) {
		n, read := newCapture(t, nil, func() string { return "198.51.100.7" })
		n.Send(KindPatrol, "正文")
		m := waitText(t, read)
		if want := config.DefaultSiteName + " (198.51.100.7)\n正文"; m["text"] != want {
			t.Fatalf("text = %q, want %q", m["text"], want)
		}
	})

	t.Run("入口地址缺失仅站点名", func(t *testing.T) {
		n, read := newCapture(t, func(c *config.Config) { c.SiteName = "测试站" }, func() string { return "" })
		n.Send(KindPatrol, "正文")
		m := waitText(t, read)
		if want := "测试站\n正文"; m["text"] != want {
			t.Fatalf("text = %q, want %q", m["text"], want)
		}
	})

	t.Run("回调为nil仅站点名", func(t *testing.T) {
		n, read := newCapture(t, func(c *config.Config) { c.SiteName = "测试站" }, nil)
		n.Send(KindPatrol, "正文")
		m := waitText(t, read)
		if want := "测试站\n正文"; m["text"] != want {
			t.Fatalf("text = %q, want %q", m["text"], want)
		}
	})

	t.Run("测试消息同样带身份", func(t *testing.T) {
		n, read := newCapture(t, func(c *config.Config) { c.SiteName = "测试站" }, func() string { return "203.0.113.9" })
		if err := n.SendTest(); err != nil {
			t.Fatalf("SendTest: %v", err)
		}
		m := waitText(t, read)
		if !strings.HasPrefix(m["text"], "测试站 (203.0.113.9)\n") {
			t.Fatalf("text = %q, want identity prefix", m["text"])
		}
	})
}
