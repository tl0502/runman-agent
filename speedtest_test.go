//go:build containers_image_openpgp

package main

import (
	"path/filepath"
	"runman-agent/db"
	"runman-agent/monitor"
	"testing"
	"time"
)

// TestSpeedResultPersistsAcrossRestart 验证测速结果落 System 表后，
// 新进程（新建 runner）能回填到 HostMonitor——这样重启后首轮测速完成前，
// 心跳上报的是上次的值而不是 0。
func TestSpeedResultPersistsAcrossRestart(t *testing.T) {
	database, err := db.Init(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db init: %v", err)
	}

	first := monitor.NewHostMonitor()
	r := newHostSpeedRunner(database, first)
	r.apply(monitor.SpeedResult{
		DownMbps: 940, UpMbps: 120, BandwidthMbps: 120,
		TestedAt: time.Now().Truncate(time.Second), DurationMs: 9100,
	})

	// 模拟重启
	second := monitor.NewHostMonitor()
	_ = newHostSpeedRunner(database, second)

	got, ok := second.Speed()
	if !ok {
		t.Fatal("重启后应回填上次测速结果")
	}
	if got.BandwidthMbps != 120 || got.DownMbps != 940 || got.UpMbps != 120 {
		t.Fatalf("回填结果不一致: %+v", got)
	}
}

// TestSpeedResultCorruptedRowIgnored 验证损坏的历史记录不影响启动，
// 只是没有可用的带宽值。
func TestSpeedResultCorruptedRowIgnored(t *testing.T) {
	database, err := db.Init(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db init: %v", err)
	}
	if err := database.SetSystem(speedTestSystemKey, "{not json"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mon := monitor.NewHostMonitor()
	_ = newHostSpeedRunner(database, mon)
	if _, ok := mon.Speed(); ok {
		t.Fatal("损坏记录不应被采用")
	}
}

// TestStartupSkipOnlyWhenFresh 验证崩溃重启（结果很新）跳过启动测速，
// 而陈旧结果仍会重测——单次测速在高速机上要吃掉几百 MB，不能每次重启都跑。
func TestStartupSkipOnlyWhenFresh(t *testing.T) {
	database, err := db.Init(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db init: %v", err)
	}

	mon := monitor.NewHostMonitor()
	r := newHostSpeedRunner(database, mon)

	if _, skip := r.startupSkip(); skip {
		t.Fatal("没有任何结果时不应跳过")
	}

	r.apply(monitor.SpeedResult{DownMbps: 900, UpMbps: 900, BandwidthMbps: 900, TestedAt: time.Now()})
	if _, skip := r.startupSkip(); !skip {
		t.Fatal("刚测过的结果应跳过启动测速")
	}

	mon.SetSpeed(monitor.SpeedResult{
		DownMbps: 900, UpMbps: 900, BandwidthMbps: 900,
		TestedAt: time.Now().Add(-2 * startupSpeedSkipWindow),
	})
	if _, skip := r.startupSkip(); skip {
		t.Fatal("陈旧结果不应跳过启动测速")
	}
}
