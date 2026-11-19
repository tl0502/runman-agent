package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"runman-agent/config"
	"runman-agent/db"
)

// newFeedTestServer 构造带 db 的 Server。巡查事件与登录日志由各用例自行插入。
func newFeedTestServer(t *testing.T) *Server {
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
	database, err := db.Init(filepath.Join(dir, "panel.db"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: mgr, db: database}
}

func feedGet(t *testing.T, s *Server, query string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handleEventsFeed(rec, httptest.NewRequest(http.MethodGet, "/api/events/feed?"+query, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	return body
}

func feedSources(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items missing or not a list")
	}
	out := make([]string, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("item not an object: %v", it)
		}
		out = append(out, m["source"].(string))
	}
	return out
}

// TestEventsFeedMerge 验证统一事件流：归并排序、total 合计、来源过滤、严重度过滤。
func TestEventsFeedMerge(t *testing.T) {
	s := newFeedTestServer(t)
	// 交错插入 4 条巡查 + 3 条安全（时间随插入递增），验证归并排序
	patrols := []struct{ check, action, detail string }{
		{"bandwidth", "throttle", "带宽超限"},
		{"resources_cpu", "stop_vm", "CPU 超限停机"},
		{"process", "kill_process", "黑名单命中"},
		{"bandwidth", "alert", "仅告警"},
	}
	for i, p := range patrols {
		s.db.InsertPatrolEvent("vm-"+string(rune('A'+i)), p.check, p.action, p.detail)
	}
	logs := []struct{ action, ip, user, detail string }{
		{"login_ok", "203.0.113.9", "admin", ""},
		{"auto_ban", "198.51.100.7", "admin", "连续失败锁定"},
		{"login_fail", "198.51.100.7", "admin", "密码错误"},
	}
	for _, l := range logs {
		s.db.InsertLoginLog(l.action, l.ip, l.user, l.detail)
	}

	// 全量：7 条，按时间倒序——最后插入的是 login_fail（最新），最早的是第一条巡查
	body := feedGet(t, s, "")
	if got := int(body["total"].(float64)); got != 7 {
		t.Fatalf("total = %d, want 7", got)
	}
	srcs := feedSources(t, body)
	if len(srcs) != 7 {
		t.Fatalf("items len = %d, want 7", len(srcs))
	}
	if srcs[0] != "security" {
		t.Fatalf("newest item should be security (last insert), got %s", srcs[0])
	}

	// 来源过滤
	body = feedGet(t, s, "source=patrol")
	if got := int(body["total"].(float64)); got != 4 {
		t.Fatalf("patrol total = %d, want 4", got)
	}
	for _, src := range feedSources(t, body) {
		if src != "patrol" {
			t.Fatalf("source filter leaked: %s", src)
		}
	}

	// 严重度过滤：bad = stop_vm/kill_process（2 条巡查）+ auto_ban（1 条安全）
	body = feedGet(t, s, "severity=bad")
	if got := int(body["total"].(float64)); got != 3 {
		t.Fatalf("bad total = %d, want 3", got)
	}

	// 非法 source → 400
	rec := httptest.NewRecorder()
	s.handleEventsFeed(rec, httptest.NewRequest(http.MethodGet, "/api/events/feed?source=weird", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid source should 400, got %d", rec.Code)
	}

	// 分页窗口：page_size=3 page=2 → 第 4~6 新的条目
	body = feedGet(t, s, "page=2&page_size=3")
	raw := body["items"].([]any)
	if len(raw) != 3 {
		t.Fatalf("page2 items len = %d, want 3", len(raw))
	}
}

// TestEventsFeedPerSourceFilters 验证各来源的独立筛选维度：检查项、登录类型组、时间。
func TestEventsFeedPerSourceFilters(t *testing.T) {
	s := newFeedTestServer(t)
	s.db.InsertPatrolEvent("vm-a", "bandwidth", "alert", "带宽告警")
	s.db.InsertPatrolEvent("vm-b", "process", "kill_process", "黑名单命中")
	s.db.InsertLoginLog("login_ok", "203.0.113.9", "admin", "")
	s.db.InsertLoginLog("login_fail", "198.51.100.7", "admin", "密码错误")

	// 巡查检查项筛选：只影响巡查侧，安全侧不受牵连
	body := feedGet(t, s, "source=patrol&check=process")
	if got := int(body["total"].(float64)); got != 1 {
		t.Fatalf("check=process total = %d, want 1", got)
	}

	// 登录类型组：sec_type=ban 应命中 manual_ban/auto_ban/login_blocked/unban——这里只有 fail/ok，为 0
	body = feedGet(t, s, "source=security&sec_type=ban")
	if got := int(body["total"].(float64)); got != 0 {
		t.Fatalf("sec_type=ban total = %d, want 0", got)
	}
	body = feedGet(t, s, "source=security&sec_type=ok")
	if got := int(body["total"].(float64)); got != 1 {
		t.Fatalf("sec_type=ok total = %d, want 1", got)
	}

	// 时间筛选：以「现在」为界，历史事件全部出局
	body = feedGet(t, s, "since="+fmt.Sprint(time.Now().Add(time.Minute).Unix()))
	if got := int(body["total"].(float64)); got != 0 {
		t.Fatalf("future since total = %d, want 0", got)
	}
	// 以 1 小时前为界：全部保留
	body = feedGet(t, s, "since="+fmt.Sprint(time.Now().Add(-time.Hour).Unix()))
	if got := int(body["total"].(float64)); got != 4 {
		t.Fatalf("past since total = %d, want 4", got)
	}
}

// TestEventsFeedSearch 验证内容搜索：巡查匹配 detail/vm_id，安全匹配 detail/ip/user。
func TestEventsFeedSearch(t *testing.T) {
	s := newFeedTestServer(t)
	s.db.InsertPatrolEvent("vm-alpha", "bandwidth", "alert", "带宽占用 6% 超阈值")
	s.db.InsertPatrolEvent("vm-beta", "process", "kill_process", "黑名单命中")
	s.db.InsertLoginLog("login_ok", "203.0.113.9", "admin", "")
	s.db.InsertLoginLog("auto_ban", "198.51.100.7", "root", "连续失败锁定")

	cases := []struct {
		name string
		q    string
		want int
	}{{
		"按事件详情（巡查）", "超阈值", 1,
	}, {
		"按 vm_id（巡查）", "alpha", 1,
	}, {
		"按 IP（安全）", "198.51.100", 1,
	}, {
		"按用户名（安全）", "root", 1,
	}, {
		"跨源命中同一关键词", "黑名单", 1,
	}, {
		"无命中", "不存在的关键词", 0,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := feedGet(t, s, "q="+url.QueryEscape(tc.q))
			if got := int(body["total"].(float64)); got != tc.want {
				t.Fatalf("q=%q total = %d, want %d", tc.q, got, tc.want)
			}
		})
	}
}

// TestEventsFeedSevLabels 验证条目 sev 与 actor 形状归一正确。
func TestEventsFeedSevLabels(t *testing.T) {
	s := newFeedTestServer(t)
	s.db.InsertPatrolEvent("vm-x", "bandwidth", "stop_vm", "停机")
	s.db.InsertLoginLog("manual_ban", "10.0.0.1", "", "手动封禁")
	body := feedGet(t, s, "")
	raw := body["items"].([]any)
	if len(raw) != 2 {
		t.Fatalf("items len = %d, want 2", len(raw))
	}
	for _, it := range raw {
		m := it.(map[string]any)
		if m["sev"] != "bad" {
			t.Fatalf("sev = %v, want bad", m["sev"])
		}
	}
	// actor 形状：巡查=vm_id，安全=IP；安全条目应带 detail
	seen := map[string]string{}
	for _, it := range raw {
		m := it.(map[string]any)
		seen[m["source"].(string)] = m["actor"].(string)
	}
	if seen["patrol"] != "vm-x" {
		t.Fatalf("patrol actor = %q, want vm-x", seen["patrol"])
	}
	if seen["security"] != "10.0.0.1" {
		t.Fatalf("security actor = %q, want 10.0.0.1", seen["security"])
	}
}
