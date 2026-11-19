package traffic

import (
	"testing"
	"time"
)

func TestMonthlyCycleKey(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 12, 0, 0, 0, time.UTC) }

	cases := []struct {
		name     string
		now      time.Time
		resetDay int
		want     string
	}{
		{"当天即周期开始", day(2026, 3, 15), 15, "2026-03-15"},
		{"重置日前一天归属上月", day(2026, 3, 14), 15, "2026-02-15"},
		{"重置日当天下午", day(2026, 3, 15), 15, "2026-03-15"},
		{"跨年", day(2026, 1, 5), 15, "2025-12-15"},
		{"月初 1 号", day(2026, 3, 15), 1, "2026-03-01"},
		{"28 号边界", day(2026, 3, 28), 28, "2026-03-28"},
	}
	for _, c := range cases {
		if got := monthlyCycleKey(c.now, c.resetDay); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

func TestRentalCycleKeyFrom(t *testing.T) {
	base := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		created time.Time
		now     time.Time
		want    string
	}{
		{"创建当天", base, base, "2026-11-18"},
		{"第 29 天仍在第 0 周期", base, base.Add(29 * 24 * time.Hour), "2026-11-18"},
		{"第 30 天进入第 1 周期", base, base.Add(30 * 24 * time.Hour), "2026-12-18"},
		{"第 30 天 + 1 秒", base, base.Add(30*24*time.Hour + time.Second), "2026-12-18"},
		{"第 60 天进入第 2 周期", base, base.Add(60 * 24 * time.Hour), "2027-01-17"},
		{"零值创建时间回落到 fallback", time.Time{}, base, "fallback"},
	}
	for _, c := range cases {
		if got := rentalCycleKeyFrom(c.created, c.now, "fallback"); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
}

// TestRentalCycleAfterRebuild 验证重建后周期不重置：锚点固定为首次创建时间，
// 即使中间发生重装（容器 Created 变化），周期仍按原起点推进。
func TestRentalCycleAfterRebuild(t *testing.T) {
	anchor := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		now  time.Time
		want string
	}{
		{"重建后第 20 天仍属第 0 周期", anchor.Add(20 * 24 * time.Hour), "2026-11-01"},
		{"重建后第 35 天进入第 1 周期", anchor.Add(35 * 24 * time.Hour), "2026-12-01"},
	}
	for _, c := range cases {
		if got := rentalCycleKeyFrom(anchor, c.now, "fallback"); got != c.want {
			t.Errorf("%s: got %s want %s", c.name, got, c.want)
		}
	}
	// 反证：若错误地用「重建时刻」当锚点，第 35 天会落回新周期起点，用户白嫖流量。
	rebuiltAt := anchor.Add(20 * 24 * time.Hour)
	if wrong := rentalCycleKeyFrom(rebuiltAt, anchor.Add(35*24*time.Hour), "fallback"); wrong == "2026-12-01" {
		t.Fatal("rebuild-as-anchor would not reset cycle; test is meaningless")
	}
}
