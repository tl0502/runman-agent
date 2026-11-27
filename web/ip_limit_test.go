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
	"runman-agent/db"
	"runman-agent/manager/portforward"
)

// newIPLimitTestServer 造一个只挂了端口转发管理器的面板，用于验证
// PUT /api/vms/{id}/iplimit 的字段解析——这条链路上没有任何 podman 依赖。
func newIPLimitTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"max_forward_ips":3}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Init(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: cfg, pf: portforward.New(nil, database, cfg)}, database
}

func putIPLimit(t *testing.T, s *Server, vmID string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/vms/"+vmID+"/iplimit", bytes.NewReader(body))
	req.SetPathValue("id", vmID)
	rec := httptest.NewRecorder()
	s.handleSetIPLimit(rec, req)
	return rec
}

// TestSetIPLimitAcceptsPanelPayload 覆盖面板实际发出的字段名。
// 前端曾发 {max_ips, use_global} 而服务端只认 {limit}，导致「设置」被解析成
// 「清除覆盖」：返回 204、toast 显示成功，实际生效值回落到全局——功能全废。
func TestSetIPLimitAcceptsPanelPayload(t *testing.T) {
	s, database := newIPLimitTestServer(t)

	if rec := putIPLimit(t, s, "vm-1", map[string]any{"limit": 7}); rec.Code != http.StatusNoContent {
		t.Fatalf("set limit: %d %s", rec.Code, rec.Body.String())
	}
	if got := s.pf.EffectiveIPLimit("vm-1"); got != 7 {
		t.Fatalf("effective = %d, want 7", got)
	}
	if got := s.pf.GetVMIPLimit("vm-1"); got == nil || *got != 7 {
		t.Fatalf("override = %v, want 7", got)
	}

	// 0 是「该 VM 不限制」，不能被当成清除覆盖。
	if rec := putIPLimit(t, s, "vm-1", map[string]any{"limit": 0}); rec.Code != http.StatusNoContent {
		t.Fatalf("set zero: %d %s", rec.Code, rec.Body.String())
	}
	if got := s.pf.GetVMIPLimit("vm-1"); got == nil || *got != 0 {
		t.Fatalf("override after 0 = %v, want 0", got)
	}

	// null 显式清除覆盖，回落到全局 3。
	if rec := putIPLimit(t, s, "vm-1", map[string]any{"limit": nil}); rec.Code != http.StatusNoContent {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	if got := s.pf.GetVMIPLimit("vm-1"); got != nil {
		t.Fatalf("override not cleared: %v", got)
	}
	if got := s.pf.EffectiveIPLimit("vm-1"); got != 3 {
		t.Fatalf("effective after clear = %d, want global 3", got)
	}
	// DB 也应同步删除，重启后才不会复活旧覆盖。
	if list, err := database.ListVMIPLimits(); err != nil || len(list) != 0 {
		t.Fatalf("db rows = %v (err %v), want empty", list, err)
	}
}

// TestSetIPLimitAcceptsLegacyPayload 保证浏览器里缓存的旧 app.js（发
// max_ips/use_global）在升级后仍然可用，不会把「设置」变成「清除」。
func TestSetIPLimitAcceptsLegacyPayload(t *testing.T) {
	s, _ := newIPLimitTestServer(t)

	if rec := putIPLimit(t, s, "vm-legacy", map[string]any{"max_ips": 9, "use_global": false}); rec.Code != http.StatusNoContent {
		t.Fatalf("legacy set: %d %s", rec.Code, rec.Body.String())
	}
	if got := s.pf.EffectiveIPLimit("vm-legacy"); got != 9 {
		t.Fatalf("legacy effective = %d, want 9", got)
	}

	if rec := putIPLimit(t, s, "vm-legacy", map[string]any{"use_global": true}); rec.Code != http.StatusNoContent {
		t.Fatalf("legacy global: %d %s", rec.Code, rec.Body.String())
	}
	if got := s.pf.GetVMIPLimit("vm-legacy"); got != nil {
		t.Fatalf("legacy override not cleared: %v", got)
	}
}

// TestSetIPLimitRejectsBadValues 越界值必须 400，不能静默写入。
func TestSetIPLimitRejectsBadValues(t *testing.T) {
	s, _ := newIPLimitTestServer(t)
	for _, payload := range []map[string]any{
		{"limit": -1},
		{"limit": maxVMLimitIPs + 1},
		{"max_ips": -5},
	} {
		if rec := putIPLimit(t, s, "vm-bad", payload); rec.Code != http.StatusBadRequest {
			t.Errorf("%v: got %d, want 400 (%s)", payload, rec.Code, rec.Body.String())
		}
	}
	if got := s.pf.GetVMIPLimit("vm-bad"); got != nil {
		t.Fatalf("rejected payload wrote override: %v", got)
	}
}

// TestSetIPLimitPersists 覆盖值落库后，新进程（重启）应能读回。
func TestSetIPLimitPersists(t *testing.T) {
	s, database := newIPLimitTestServer(t)
	if rec := putIPLimit(t, s, "vm-p", map[string]any{"limit": 11}); rec.Code != http.StatusNoContent {
		t.Fatalf("set: %d %s", rec.Code, rec.Body.String())
	}
	fresh := portforward.New(nil, database, s.cfg)
	if got := fresh.EffectiveIPLimit("vm-p"); got != 11 {
		t.Fatalf("reloaded effective = %d, want 11", got)
	}
}
