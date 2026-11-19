package notify

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	n := New(mgr)
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

// TestSendGating 验证 Send 的门控：通道关/凭据缺/登录类关闭 均不外发；
// login_enabled 只约束登录类，不影响巡查类。
func TestSendGating(t *testing.T) {
	cases := []struct {
		name   string
		kind   Kind
		mutate func(c *config.Config)
		want   int32
	}{
		{"通道关闭不发", KindPatrol, func(c *config.Config) { c.Telegram.Enabled = boolPtr(false) }, 0},
		{"缺chat_id不发", KindPatrol, func(c *config.Config) { c.Telegram.ChatID = "" }, 0},
		{"巡查类推", KindPatrol, nil, 1},
		{"登录类默认推", KindLogin, nil, 1},
		{"登录类显式关不发", KindLogin, func(c *config.Config) { c.Telegram.LoginEnabled = boolPtr(false) }, 0},
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
	n.Send(KindLogin, "x")
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
