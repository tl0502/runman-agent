package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"runman-agent/config"
	"runman-agent/manager"
)

// ── Q23：负数上限不能返回 200 后静默忽略 ────────────────────────────────────

func TestConfigRejectsNegativeLimits(t *testing.T) {
	s := newConfigTestServer(t)
	for _, payload := range []map[string]any{
		{"max_forward_ips": -3},
		{"max_port_forward": -1},
		{"web_user": "qa:admin"}, // Q5：Basic Auth 按首个冒号切分，降级后无法登录
	} {
		rec := postConfig(t, s, payload)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%v: got %d, want 400 (%s)", payload, rec.Code, rec.Body.String())
		}
	}
	// 0 是「不限制」的合法值，不能被误拒绝。
	rec := postConfig(t, s, map[string]any{"max_forward_ips": 0})
	if rec.Code != http.StatusOK {
		t.Errorf("max_forward_ips=0 got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
}

// ── Q11：trust_proxy 只在显式开启时采信 XFF 最右一跳 ────────────────────────

func TestClientIPTrustProxy(t *testing.T) {
	s := newConfigTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.1, 198.51.100.7")
	req.Header.Set("X-Real-IP", "192.0.2.9")

	// 默认直连语义：完全忽略反代头，避免任何人伪造来源 IP 绕过登录封禁。
	if got := s.clientIP(req); got != "192.0.2.1" {
		t.Fatalf("direct clientIP = %q, want RemoteAddr host", got)
	}

	if err := s.cfg.UpdateChecked(func(c *config.Config) error {
		c.TrustProxy = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 最左一跳可由客户端随意预置，只信最近一跳代理追加的最后一项。
	if got := s.clientIP(req); got != "198.51.100.7" {
		t.Fatalf("trusted clientIP = %q, want last XFF hop", got)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req2.Header.Set("X-Real-IP", "192.0.2.9")
	if got := s.clientIP(req2); got != "192.0.2.9" {
		t.Fatalf("X-Real-IP fallback = %q", got)
	}

	// 反代头里是垃圾时回落到 RemoteAddr，不能返回空串把封禁键污染成 ""。
	req3 := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	req3.Header.Set("X-Forwarded-For", "not-an-ip")
	if got := s.clientIP(req3); got != "192.0.2.1" {
		t.Fatalf("invalid XFF fallback = %q", got)
	}
}

// ── Q11：配置项的 GET / POST 往返，确保前端开关真的能落盘 ───────────────────

func TestConfigTrustProxyRoundTrip(t *testing.T) {
	s := newConfigTestServer(t)
	if rec := postConfig(t, s, map[string]any{"trust_proxy": true}); rec.Code != http.StatusOK {
		t.Fatalf("save trust_proxy: %d %s", rec.Code, rec.Body.String())
	}
	if !s.cfg.Get().TrustProxy {
		t.Fatal("trust_proxy 未落盘")
	}
	var body map[string]any
	rec := httptest.NewRecorder()
	s.handleConfig(rec, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/config: %d", rec.Code)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if v, _ := body["trust_proxy"].(bool); !v {
		t.Fatalf("GET /api/config trust_proxy = %v, want true", body["trust_proxy"])
	}
}

// ── Q8：哨兵错误映射成 4xx，而不是一律 500 ──────────────────────────────────

func TestVMErrStatus(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, http.StatusOK},
		{fmt.Errorf("wrapped: %w", manager.ErrVMNotFound), http.StatusNotFound},
		{fmt.Errorf("wrapped: %w", manager.ErrVMExists), http.StatusConflict},
		{fmt.Errorf("wrapped: %w", manager.ErrInvalidArgument), http.StatusBadRequest},
		{errors.New("disk on fire"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		if got := vmErrStatus(c.err); got != c.want {
			t.Errorf("vmErrStatus(%v) = %d, want %d", c.err, got, c.want)
		}
	}
}

// ── Q8：非法 vm_id 在进入驱动前被拦截（否则会 500 + crun 原始报错） ─────────

func TestValidateVMID(t *testing.T) {
	valid := []string{"abc", "vm-01", "a_b.c", "QA-123"}
	invalid := []string{"", "-lead", ".lead", "a;b", "../etc", "a b", "a'b", "a\"b", "a\\b", longID(129)}
	for _, id := range valid {
		if err := validateVMID(id); err != nil {
			t.Errorf("validateVMID(%q) rejected: %v", id, err)
		}
	}
	for _, id := range invalid {
		if err := validateVMID(id); err == nil {
			t.Errorf("validateVMID(%q) should be rejected", id)
		}
	}
}

func longID(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a'
	}
	return string(b)
}
