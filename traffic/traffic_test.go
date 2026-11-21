package traffic

import (
	"testing"
	"time"
)

func TestMonthlyCycle(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 12, 0, 0, 0, time.UTC) }

	cases := []struct {
		name      string
		now       time.Time
		resetDay  int
		wantKey   string
		wantStart time.Time
	}{
		{"当天即周期开始", day(2026, 3, 15), 15, "2026-03-15", time.Date(2026, 3, 15, 0, 0, 0, 0, time.Local)},
		{"重置日前一天归属上月", day(2026, 3, 14), 15, "2026-02-15", time.Date(2026, 2, 15, 0, 0, 0, 0, time.Local)},
		{"重置日当天下午", day(2026, 3, 15), 15, "2026-03-15", time.Date(2026, 3, 15, 0, 0, 0, 0, time.Local)},
		{"跨年", day(2026, 1, 5), 15, "2025-12-15", time.Date(2025, 12, 15, 0, 0, 0, 0, time.Local)},
		{"月初 1 号", day(2026, 3, 15), 1, "2026-03-01", time.Date(2026, 3, 1, 0, 0, 0, 0, time.Local)},
		{"28 号边界", day(2026, 3, 28), 28, "2026-03-28", time.Date(2026, 3, 28, 0, 0, 0, 0, time.Local)},
	}
	for _, c := range cases {
		key, start := monthlyCycle(c.now, c.resetDay)
		if key != c.wantKey {
			t.Errorf("%s: key got %s want %s", c.name, key, c.wantKey)
		}
		if !start.Equal(c.wantStart) {
			t.Errorf("%s: start got %v want %v", c.name, start, c.wantStart)
		}
	}
}

func TestRentalCycleFrom(t *testing.T) {
	base := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		created   time.Time
		now       time.Time
		wantKey   string
		wantStart time.Time
	}{
		{"创建当天", base, base, "2026-11-18", base},
		{"第 29 天仍在第 0 周期", base, base.Add(29 * 24 * time.Hour), "2026-11-18", base},
		{"第 30 天进入第 1 周期", base, base.Add(30 * 24 * time.Hour), "2026-12-18", base.Add(30 * 24 * time.Hour)},
		{"第 30 天 + 1 秒", base, base.Add(30*24*time.Hour + time.Second), "2026-12-18", base.Add(30 * 24 * time.Hour)},
		{"第 60 天进入第 2 周期", base, base.Add(60 * 24 * time.Hour), "2027-01-17", base.Add(60 * 24 * time.Hour)},
		// 周期起点必须精确到锚点时分秒，而非格式化日期的 00:00：
		// 周期切换当天、锚点时刻之前的同步仍要能判定为「新周期」。
		{"起点保留锚点时分秒", base, base.Add(30*24*time.Hour - time.Hour), "2026-11-18", base},
	}
	for _, c := range cases {
		key, start := rentalCycleFrom(c.created, c.now)
		if key != c.wantKey {
			t.Errorf("%s: key got %s want %s", c.name, key, c.wantKey)
		}
		if !start.Equal(c.wantStart) {
			t.Errorf("%s: start got %v want %v", c.name, start, c.wantStart)
		}
	}
}

// TestRentalCycleAfterRebuild 验证重建后周期不重置：锚点固定为首次创建时间，
// 即使中间发生重装（容器 Created 变化），周期仍按原起点推进。
func TestRentalCycleAfterRebuild(t *testing.T) {
	anchor := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		now     time.Time
		wantKey string
	}{
		{"重建后第 20 天仍属第 0 周期", anchor.Add(20 * 24 * time.Hour), "2026-11-01"},
		{"重建后第 35 天进入第 1 周期", anchor.Add(35 * 24 * time.Hour), "2026-12-01"},
	}
	for _, c := range cases {
		key, _ := rentalCycleFrom(anchor, c.now)
		if key != c.wantKey {
			t.Errorf("%s: got %s want %s", c.name, key, c.wantKey)
		}
	}
	// 反证：若错误地用「重建时刻」当锚点，第 35 天会落回新周期起点，用户白嫖流量。
	rebuiltAt := anchor.Add(20 * 24 * time.Hour)
	if wrong, _ := rentalCycleFrom(rebuiltAt, anchor.Add(35*24*time.Hour)); wrong == "2026-12-01" {
		t.Fatal("rebuild-as-anchor would not reset cycle; test is meaningless")
	}
}

// TestShouldResetCycle 验证清零判据：只有「时间自然推进到新周期」才清零。
// 2026-11 实际事故：monthly 切 rental 时键空间不同（2026-11-01 vs 创建时间
// 对齐日），旧判据「键不同即清零」导致全部容器周期流量被清零。
func TestShouldResetCycle(t *testing.T) {
	cases := []struct {
		name     string
		oldKey   string
		newKey   string
		newStart time.Time
		lastSync time.Time
		want     bool
	}{
		{
			// 事故场景：已运行一段时间（上次同步 11-21）后切到 rental，
			// rental 周期起点（10-30）早于上次同步 → 不清零，计数平移保留。
			name:   "monthly 切 rental 不清零",
			oldKey: "2026-11-01", newKey: "2026-10-30",
			newStart: time.Date(2026, 10, 30, 9, 0, 0, 0, time.Local),
			lastSync: time.Date(2026, 11, 21, 8, 0, 0, 0, time.Local),
			want:     false,
		},
		{
			name:   "rental 切回 monthly 不清零",
			oldKey: "2026-11-18", newKey: "2026-11-01",
			newStart: time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local),
			lastSync: time.Date(2026, 11, 21, 8, 0, 0, 0, time.Local),
			want:     false,
		},
		{
			// monthly 下改重置日（1 → 15）：新周期起点在过去 → 不清零。
			name:   "改重置日不清零",
			oldKey: "2026-11-01", newKey: "2026-11-15",
			newStart: time.Date(2026, 11, 15, 0, 0, 0, 0, time.Local),
			lastSync: time.Date(2026, 11, 21, 8, 0, 0, 0, time.Local),
			want:     false,
		},
		{
			// 锚点偶发失败回落 monthly 键（rental → monthly 键抖动）→ 不清零。
			name:   "锚点抖动回落不清零",
			oldKey: "2026-11-18", newKey: "2026-11-01",
			newStart: time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local),
			lastSync: time.Date(2026, 11, 21, 8, 30, 0, 0, time.Local),
			want:     false,
		},
		{
			// rental 自然到期：上次同步在周期切换时刻（锚点时分秒）之前 → 清零。
			// 覆盖「切换当天、锚点时刻之前同步过」的窗口（起点若取日期 00:00 会漏清零）。
			name:   "rental 自然到期清零",
			oldKey: "2026-11-18", newKey: "2026-12-18",
			newStart: time.Date(2026, 12, 18, 10, 0, 0, 0, time.Local),
			lastSync: time.Date(2026, 12, 18, 9, 55, 0, 0, time.Local),
			want:     true,
		},
		{
			// monthly 自然跨月：上次同步在月末，新周期起点 00:00 晚于它 → 清零。
			name:   "monthly 自然跨月清零",
			oldKey: "2026-10-01", newKey: "2026-11-01",
			newStart: time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local),
			lastSync: time.Date(2026, 10, 31, 23, 59, 59, 0, time.Local),
			want:     true,
		},
		{
			name:   "键未变化不清零",
			oldKey: "2026-11-18", newKey: "2026-11-18",
			newStart: time.Date(2026, 11, 18, 10, 0, 0, 0, time.Local),
			lastSync: time.Date(2026, 11, 20, 8, 0, 0, 0, time.Local),
			want:     false,
		},
	}
	for _, c := range cases {
		if got := shouldResetCycle(c.oldKey, c.newKey, c.newStart, c.lastSync); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}
