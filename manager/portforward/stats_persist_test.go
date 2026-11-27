package portforward

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"runman-agent/config"
	"runman-agent/db"
)

// newStatsFixture 造一份可跨 Manager 复用的 cfg + db（同一文件），
// 用来模拟「进程重启」：第二个 Manager 打开同一个库恢复规则和统计。
func newStatsFixture(t *testing.T) (*config.Manager, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"virt_type":"podman"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Update(func(c *config.Config) { c.PublicIPv4 = "127.0.0.1" }); err != nil {
		t.Fatal(err)
	}
	database, err := db.Init(filepath.Join(dir, "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg, database
}

// killListeners 只关闭监听、保留内存与 DB 记录，模拟 agent 进程被 SIGTERM 杀掉：
// 旧连接全断（active 归零），但累计值应该还在。
func killListeners(m *Manager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, entries := range m.mappings {
		for _, e := range entries {
			e.cancel()
			closeAll(e.lns)
		}
	}
}

func liveEntry(t *testing.T, m *Manager, vmID, protocol string, hostPort int) *Entry {
	t.Helper()
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.mappings[vmID] {
		if e.Protocol == protocol && e.HostPort == hostPort {
			return e
		}
	}
	t.Fatalf("entry %s/%d not found for %s", protocol, hostPort, vmID)
	return nil
}

// TestStatsSurviveRestart 复现「更新后端口连接统计被清零」：
// 累计连接数与 per-IP 计数落库，重启（Restore）后必须读回；活跃数归零。
func TestStatsSurviveRestart(t *testing.T) {
	cfg, database := newStatsFixture(t)
	ctx := context.Background()

	m1 := New(socketVM{}, database, cfg)
	hostPort := availablePort(t, "tcp")
	guest := echoTarget(t, "tcp")
	if err := m1.AddMapping(ctx, "test", "tcp", hostPort, guest, "ssh", BindV4); err != nil {
		t.Fatal(err)
	}

	e1 := liveEntry(t, m1, "test", "tcp", hostPort)
	e1.trackConnect("1.1.1.1")
	e1.trackConnect("1.1.1.1")
	e1.trackConnect("2.2.2.2")
	m1.FlushStats()

	killListeners(m1)

	// 重启：新 Manager 打开同一个库，Restore 重新绑定端口。
	m2 := New(socketVM{}, database, cfg)
	rctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m2.Restore(rctx)
	defer m2.DeleteVM(context.Background(), "test")

	report := m2.GetReport()
	var got *Entry
	for i := range report {
		if report[i].Protocol == "tcp" && report[i].HostPort == hostPort {
			got = &report[i]
		}
	}
	if got == nil {
		t.Fatal("rule not restored")
	}
	if got.TotalConns != 3 {
		t.Fatalf("total after restart = %d, want 3", got.TotalConns)
	}
	if got.ActiveConns != 0 {
		t.Fatalf("active after restart = %d, want 0（旧连接已断，不能伪造）", got.ActiveConns)
	}
	perIP := map[string]int64{}
	for _, ip := range got.TopIPs {
		perIP[ip.IP] = ip.Count
	}
	if perIP["1.1.1.1"] != 2 || perIP["2.2.2.2"] != 1 {
		t.Fatalf("per-IP counts after restart = %v, want 1.1.1.1:2 2.2.2.2:1", perIP)
	}

	// 恢复后继续累加，再次落库。
	e2 := liveEntry(t, m2, "test", "tcp", hostPort)
	e2.trackConnect("3.3.3.3")
	m2.FlushStats()
	if err := m2.AddMapping(ctx, "test", "tcp", hostPort, guest, "ssh", BindV4); err != nil {
		t.Fatal(err)
	}
	pf, err := database.GetPortForward("tcp", hostPort)
	if err != nil {
		t.Fatal(err)
	}
	if pf == nil || pf.TotalConns != 4 {
		t.Fatalf("flushed stats = %+v, want total 4", pf)
	}
}

// TestStatsSurviveRuleRebuild 规则被重建（改描述 / 改内网端口 / 容器换 IP）
// 时沿用同一 (protocol, hostPort) 的累计值，不能清零。
func TestStatsSurviveRuleRebuild(t *testing.T) {
	cfg, database := newStatsFixture(t)
	ctx := context.Background()
	m := New(socketVM{}, database, cfg)
	defer m.DeleteVM(context.Background(), "test")

	hostPort := availablePort(t, "tcp")
	guest := echoTarget(t, "tcp")
	if err := m.AddMapping(ctx, "test", "tcp", hostPort, guest, "ssh", BindV4); err != nil {
		t.Fatal(err)
	}
	e := liveEntry(t, m, "test", "tcp", hostPort)
	e.trackConnect("9.9.9.9")
	e.trackConnect("9.9.9.9")

	// 改描述 + 改内网端口 → addMapping 走「替换 Entry」分支。
	if err := m.AddMapping(ctx, "test", "tcp", hostPort, echoTarget(t, "tcp"), "sshd", BindV4); err != nil {
		t.Fatal(err)
	}

	rebuilt := liveEntry(t, m, "test", "tcp", hostPort)
	report := m.GetReport()
	if len(report) != 1 {
		t.Fatalf("report size = %d, want 1", len(report))
	}
	r := &report[0]
	if r.HostPort != hostPort {
		t.Fatalf("report host port = %d, want %d", r.HostPort, hostPort)
	}
	if r.TotalConns != 2 {
		t.Fatalf("total after rebuild = %d, want 2", r.TotalConns)
	}
	if len(r.TopIPs) != 1 || r.TopIPs[0].IP != "9.9.9.9" || r.TopIPs[0].Count != 2 {
		t.Fatalf("per-IP after rebuild = %+v, want 9.9.9.9:2", r.TopIPs)
	}
	// 新 Entry 的 lastFlushTotal 继承自旧 Entry，避免下一次 flush 重写一遍。
	if got := rebuilt.lastFlushTotal; got != 0 {
		t.Fatalf("lastFlushTotal = %d, want inherited 0", got)
	}

	// 重建后新增连接继续累加到同一计数上。
	rebuilt.trackConnect("9.9.9.9")
	m.FlushStats()
	pf, err := database.GetPortForward("tcp", hostPort)
	if err != nil {
		t.Fatal(err)
	}
	if pf == nil || pf.TotalConns != 3 {
		t.Fatalf("flushed total = %+v, want 3", pf)
	}
}

// TestStatsResetWhenPortChangesOwner 端口从旧 VM 回收给新 VM 时不继承旧统计，
// 否则新实例会凭空背上「累计 N 次连接」。
func TestStatsResetWhenPortChangesOwner(t *testing.T) {
	cfg, database := newStatsFixture(t)
	ctx := context.Background()
	hostPort := availablePort(t, "tcp")

	// 库里残留一条属于 vm-old 的规则（内存里没有监听），历史统计 99。
	if err := database.SavePortForward(&db.PortForward{
		Protocol: "tcp", HostPort: hostPort, VMID: "vm-old", GuestPort: 22,
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdatePortForwardStats("tcp", hostPort, 99, `{"6.6.6.6":99}`); err != nil {
		t.Fatal(err)
	}

	m := New(socketVM{}, database, cfg)
	defer m.DeleteVM(context.Background(), "vm-new")
	if err := m.AddMapping(ctx, "vm-new", "tcp", hostPort, echoTarget(t, "tcp"), "", BindV4); err != nil {
		t.Fatal(err)
	}

	report := m.GetReport()
	if len(report) != 1 {
		t.Fatalf("report size = %d, want 1", len(report))
	}
	if report[0].TotalConns != 0 || len(report[0].TopIPs) != 0 {
		t.Fatalf("new owner inherited stats: total=%d top=%+v", report[0].TotalConns, report[0].TopIPs)
	}
	pf, err := database.GetPortForward("tcp", hostPort)
	if err != nil {
		t.Fatal(err)
	}
	if pf == nil || pf.VMID != "vm-new" || pf.TotalConns != 0 || pf.ConnsByIP != "" {
		t.Fatalf("db row after owner change = %+v, want vm-new with zero stats", pf)
	}
}

// TestFlushStatsIdempotent flush 不会把没变化的规则反复写库（lastFlushTotal 生效）。
func TestFlushStatsIdempotent(t *testing.T) {
	cfg, database := newStatsFixture(t)
	ctx := context.Background()
	m := New(socketVM{}, database, cfg)
	defer m.DeleteVM(context.Background(), "test")

	hostPort := availablePort(t, "tcp")
	if err := m.AddMapping(ctx, "test", "tcp", hostPort, echoTarget(t, "tcp"), "", BindV4); err != nil {
		t.Fatal(err)
	}
	e := liveEntry(t, m, "test", "tcp", hostPort)
	e.trackConnect("5.5.5.5")
	m.FlushStats()
	if got := e.lastFlushTotal; got != 1 {
		t.Fatalf("lastFlushTotal = %d, want 1", got)
	}
	// 手工把 DB 统计改成异常值，再次空转 flush 不应覆盖回 1。
	if err := database.UpdatePortForwardStats("tcp", hostPort, 999, ""); err != nil {
		t.Fatal(err)
	}
	m.FlushStats()
	pf, err := database.GetPortForward("tcp", hostPort)
	if err != nil {
		t.Fatal(err)
	}
	if pf.TotalConns != 999 {
		t.Fatalf("idle flush rewrote stats: total = %d, want untouched 999", pf.TotalConns)
	}
}
