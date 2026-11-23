package monitor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestThroughputMbps(t *testing.T) {
	// 12.5 MB/s = 100 Mbps（十进制）
	if got := throughputMbps(12_500_000, time.Second); got < 99.9 || got > 100.1 {
		t.Fatalf("throughputMbps = %v, want 100", got)
	}
	if got := throughputMbps(0, time.Second); got != 0 {
		t.Fatalf("零字节应为 0，得到 %v", got)
	}
	if got := throughputMbps(1024, 0); got != 0 {
		t.Fatalf("零时长应为 0，得到 %v", got)
	}
}

func TestMinPositive(t *testing.T) {
	cases := []struct {
		a, b, want int32
	}{
		{940, 120, 120}, // 非对称：取窄的一侧
		{100, 100, 100},
		{0, 300, 300}, // 上行测失败时用下行
		{300, 0, 300},
		{0, 0, 0},
	}
	for _, c := range cases {
		if got := minPositive(c.a, c.b); got != c.want {
			t.Fatalf("minPositive(%d, %d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// 关键回归：错误页（403 + 1 字节）不能被算成带宽。
// 旧实现只做 io.Copy 计数，会把这个 1 字节换算成一个假速率。
func TestMeasureDirectionRejectsErrorPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()
	useTestTimings(t)

	if _, err := measureDirection(context.Background(), speedClient(), srv.URL, false); err == nil {
		t.Fatal("403 响应应当返回错误，而不是把错误页字节当带宽")
	}
}

// 200 但内容很小（劫持页/落地页）同样不能当成带宽。
func TestMeasureDirectionRejectsTinyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>portal</html>"))
	}))
	defer srv.Close()
	useTestTimings(t)

	if _, err := measureDirection(context.Background(), speedClient(), srv.URL, false); err == nil {
		t.Fatal("小体积的 200 响应应被拒绝（低于 speedMinBody）")
	}
}

// 端到端：本地服务持续吐数据时，两个方向都应测出正值，带宽取较小值。
func TestMeasureSpeedLocal(t *testing.T) {
	srv := newSpeedTestServer(t)
	useTestSources(t, speedSource{name: "local", down: srv.URL + "/down", up: srv.URL + "/up"})

	res, err := MeasureSpeed(context.Background())
	if err != nil {
		t.Fatalf("MeasureSpeed: %v", err)
	}
	if res.DownMbps <= 0 || res.UpMbps <= 0 {
		t.Fatalf("两个方向都应测出正值，得到 down=%d up=%d", res.DownMbps, res.UpMbps)
	}
	if res.BandwidthMbps != minPositive(res.DownMbps, res.UpMbps) {
		t.Fatalf("带宽应取较小值：down=%d up=%d bw=%d", res.DownMbps, res.UpMbps, res.BandwidthMbps)
	}
	if res.Partial {
		t.Fatal("两个方向都成功时不应标记 partial")
	}
	if res.Source != "local" {
		t.Fatalf("应记录生效的测速源，得到 %q", res.Source)
	}
}

// 主源失败必须自动落到备用源，并把结果标记为单方向（备用源只有下载镜像）。
func TestMeasureSpeedFallsBack(t *testing.T) {
	srv := newSpeedTestServer(t)
	useTestSources(t,
		speedSource{name: "bad", down: srv.URL + "/bad"},
		speedSource{name: "good", down: srv.URL + "/down"},
	)

	res, err := MeasureSpeed(context.Background())
	if err != nil {
		t.Fatalf("备用源可用时不应失败: %v", err)
	}
	if res.Source != "good" {
		t.Fatalf("应落到备用源，得到 %q", res.Source)
	}
	if res.DownMbps <= 0 || res.UpMbps != 0 {
		t.Fatalf("备用源只测下行，得到 down=%d up=%d", res.DownMbps, res.UpMbps)
	}
	if !res.Partial {
		t.Fatal("只有下行时须标记 partial")
	}
}

// 全部源都失败时必须返回错误，而不是一个 0 值结果。
func TestMeasureSpeedAllSourcesFail(t *testing.T) {
	srv := newSpeedTestServer(t)
	useTestSources(t,
		speedSource{name: "a", down: srv.URL + "/bad"},
		speedSource{name: "b", down: srv.URL + "/bad"},
	)

	if _, err := MeasureSpeed(context.Background()); err == nil {
		t.Fatal("所有源都 403 时应返回错误")
	}
}

// newSpeedTestServer 起一个本地测速服务：/down 持续吐数据，/up 收流，/bad 返回 403。
func newSpeedTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	buf := make([]byte, 32<<10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bad":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("x"))
		case "/up":
			_, _ = io.Copy(io.Discard, r.Body)
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			// 长度给足（>= speedMinBody），持续写到客户端取消
			w.Header().Set("Content-Length", "104857600")
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if _, err := w.Write(buf); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// useTestSources 替换测速源表并压缩预热/取样时长，测试结束后恢复。
func useTestSources(t *testing.T, sources ...speedSource) {
	t.Helper()
	old := speedSources
	speedSources = sources
	t.Cleanup(func() { speedSources = old })
	useTestTimings(t)
}

func useTestTimings(t *testing.T) {
	t.Helper()
	oldWarmup, oldWindow := speedWarmup, speedWindow
	speedWarmup, speedWindow = 300*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { speedWarmup, speedWindow = oldWarmup, oldWindow })
}
