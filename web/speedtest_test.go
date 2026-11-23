package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runman-agent/monitor"
	"runman-agent/proto/agent"
	"testing"
)

type fakeSpeedTester struct {
	canStart bool
	running  bool
	lastErr  string
	starts   int
}

func (f *fakeSpeedTester) Start() bool {
	if !f.canStart {
		return false
	}
	f.starts++
	f.running = true
	return true
}

func (f *fakeSpeedTester) Status() (bool, string) { return f.running, f.lastErr }

// TestSpeedTestEndpoint 验证手动测速端点的三种结果：
// 未注入调度器 → 500；首次触发 → 202；已有测速在跑 → 409。
func TestSpeedTestEndpoint(t *testing.T) {
	s := newConfigTestServer(t)

	rec := httptest.NewRecorder()
	s.handleSpeedTest(rec, httptest.NewRequest(http.MethodPost, "/api/speedtest", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("未注入调度器应回 500，得到 %d", rec.Code)
	}

	f := &fakeSpeedTester{canStart: true}
	s.SetSpeedTester(f)

	rec = httptest.NewRecorder()
	s.handleSpeedTest(rec, httptest.NewRequest(http.MethodPost, "/api/speedtest", nil))
	if rec.Code != http.StatusAccepted || f.starts != 1 {
		t.Fatalf("首次触发应回 202 并调用 Start，得到 %d（调用 %d 次）", rec.Code, f.starts)
	}
	if got := rec.Body.String(); got != "{\"started\":true}\n" {
		t.Fatalf("响应体异常: %q", got)
	}

	f.canStart = false
	rec = httptest.NewRecorder()
	s.handleSpeedTest(rec, httptest.NewRequest(http.MethodPost, "/api/speedtest", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("测速进行中应回 409，得到 %d", rec.Code)
	}
}

// TestHostStatusCarriesSpeed 锁定 /api/status 的测速字段名：
// 前端按 bandwidth_mbps（= min(down, up)）显示，speed 里是明细。
func TestHostStatusCarriesSpeed(t *testing.T) {
	resp := hostStatusResponse{
		HostStats: &monitor.HostStats{Heartbeat: &agent.Heartbeat{BandwidthMbps: 118}},
		Speed: &monitor.SpeedResult{
			DownMbps: 562, UpMbps: 118, BandwidthMbps: 118,
		},
		SpeedTesting: true,
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got["bandwidth_mbps"] != float64(118) {
		t.Fatalf("心跳带宽字段异常: %v", got["bandwidth_mbps"])
	}
	speed, ok := got["speed"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 speed 字段: %s", b)
	}
	if speed["down_mbps"] != float64(562) || speed["up_mbps"] != float64(118) {
		t.Fatalf("speed 明细异常: %v", speed)
	}
	if got["speed_testing"] != true {
		t.Fatalf("speed_testing 应为 true: %v", got["speed_testing"])
	}
}
