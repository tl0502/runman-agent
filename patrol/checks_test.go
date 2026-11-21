package patrol

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/proto/agent"
)

// 基础 conntrack 样本：涵盖 TCP ESTABLISHED / TIME_WAIT、UDP ASSURED / UNREPLIED、ipv6。
const sampleConntrack = `ipv4     2 tcp      6 431999 ESTABLISHED src=10.91.0.2 dst=1.2.3.4 sport=60000 dport=443 src=1.2.3.4 dst=10.91.0.2 sport=443 dport=60000 [ASSURED] mark=0 zone=0 use=2
ipv4     2 tcp      6 299 ESTABLISHED src=5.6.7.8 dst=10.91.0.2 sport=22 dport=50000 src=10.91.0.2 dst=5.6.7.8 sport=50000 dport=22 [ASSURED] mark=0 zone=0 use=2
ipv4     2 tcp      6 120 TIME_WAIT src=10.91.0.2 dst=9.9.9.9 sport=40000 dport=80 src=9.9.9.9 dst=10.91.0.2 sport=80 dport=40000 [ASSURED] mark=0 zone=0 use=2
ipv4     2 udp     17 29 src=10.91.0.2 dst=8.8.8.8 sport=53 dport=53 [UNREPLIED] src=8.8.8.8 dst=10.91.0.2 sport=53 dport=53 mark=0 zone=0 use=2
ipv4     2 udp     17 29 src=10.91.0.3 dst=10.91.0.2 sport=60001 dport=60001 [ASSURED] src=10.91.0.2 dst=10.91.0.3 sport=60001 dport=60001 mark=0 zone=0 use=2
ipv6     2 tcp      6 100 ESTABLISHED src=2001:db8::2 dst=2001:db8::3 sport=1 dport=2 src=2001:db8::3 dst=2001:db8::2 sport=2 dport=1 [ASSURED] mark=0 zone=0 use=2
`

func TestCountConnections(t *testing.T) {
	// watch=nil 表示对所有 IP 做完整画像（与旧测试口径一致）。
	m := countConnections(sampleConntrack, nil, 100, nil)

	if m["10.91.0.2"].out != 1 {
		t.Fatalf("10.91.0.2 out = %d, want 1 (TIME_WAIT 与 UDP UNREPLIED 不应计入活跃)", m["10.91.0.2"].out)
	}
	if m["10.91.0.2"].in != 2 {
		t.Fatalf("10.91.0.2 in = %d, want 2 (TCP ESTABLISHED + UDP ASSURED)", m["10.91.0.2"].in)
	}
	if m["1.2.3.4"].in != 1 || m["1.2.3.4"].out != 0 {
		t.Fatalf("1.2.3.4 = %+v, want in=1 out=0", m["1.2.3.4"])
	}
	if m["5.6.7.8"].out != 1 || m["5.6.7.8"].in != 0 {
		t.Fatalf("5.6.7.8 = %+v, want in=0 out=1", m["5.6.7.8"])
	}
	if m["9.9.9.9"].in != 0 || m["9.9.9.9"].out != 0 {
		t.Fatalf("9.9.9.9 = %+v, want 0/0（TIME_WAIT 不计活跃）", m["9.9.9.9"])
	}
	// 纯 ipv6 行按自身 src/dst 统计（既有行为）
	if m["2001:db8::2"].out != 1 {
		t.Fatalf("2001:db8::2 = %+v, want out=1（ipv6 条目自身应被统计）", m["2001:db8::2"])
	}
}

func TestFirstKV(t *testing.T) {
	line := "ipv4 2 tcp 6 431999 ESTABLISHED src=10.91.0.2 dst=1.2.3.4 sport=1 src=1.2.3.4 dst=10.91.0.2"
	if got := firstKV(line, "src="); got != "10.91.0.2" {
		t.Fatalf("firstKV src = %q, want 10.91.0.2", got)
	}
	if got := firstKV(line, "dst="); got != "1.2.3.4" {
		t.Fatalf("firstKV dst = %q, want 1.2.3.4", got)
	}
}

func TestCountConnectionsUDPBothDirections(t *testing.T) {
	// UNREPLIED（单向发包的主要形态）也必须计入 UDP 画像——活跃口径看不见它。
	m := countConnections(sampleConntrack, nil, 0, nil)
	c := m["10.91.0.2"]
	if c.udpOut != 1 || c.udpIn != 1 {
		t.Fatalf("10.91.0.2 udp = out=%d in=%d, want out=1 in=1（UNREPLIED 出方向 + ASSURED 入方向）", c.udpOut, c.udpIn)
	}
	if m["8.8.8.8"].udpIn != 1 {
		t.Fatalf("8.8.8.8 udpIn = %d, want 1", m["8.8.8.8"].udpIn)
	}
}

func TestCountConnectionsSMTPPorts(t *testing.T) {
	data := `ipv4     2 tcp      6 60 SYN_SENT src=10.91.0.5 dst=198.51.100.20 sport=53001 dport=25 [UNREPLIED] mark=0 use=1
ipv4     2 tcp      6 60 SYN_SENT src=10.91.0.5 dst=198.51.100.21 sport=53002 dport=465 [UNREPLIED] mark=0 use=1
ipv4     2 tcp      6 60 SYN_SENT src=10.91.0.5 dst=198.51.100.22 sport=53003 dport=587 [UNREPLIED] mark=0 use=1
ipv4     2 tcp      6 60 SYN_SENT src=10.91.0.5 dst=198.51.100.23 sport=53004 dport=80 [UNREPLIED] mark=0 use=1
ipv4     2 tcp      6 60 SYN_SENT src=198.51.100.30 dst=10.91.0.5 sport=40000 dport=25 [UNREPLIED] mark=0 use=1
`
	ports := map[int]bool{25: true, 465: true, 587: true}
	m := countConnections(data, nil, 0, ports)
	// 入方向的 dport=25（别人连我们 25）不算发信：只数出方向
	if got := m["10.91.0.5"].smtpOut; got != 3 {
		t.Fatalf("smtpOut = %d, want 3（25/465/587 出方向命中；入方向与 dport=80 不计）", got)
	}
}

func TestCountConnectionsDistinctSaturation(t *testing.T) {
	var data string
	for i := 0; i < 50; i++ {
		data += "ipv4     2 tcp      6 60 SYN_SENT src=10.91.0.9 dst=198.51.100." + strconv.Itoa(i) +
			" sport=53000 dport=22 [UNREPLIED] mark=0 use=1\n"
	}
	m := countConnections(data, map[string]bool{"10.91.0.9": true}, 10, nil)
	if got := len(m["10.91.0.9"].distinct); got != 10 {
		t.Fatalf("distinct = %d, want 10（饱和封顶在 scanCap）", got)
	}
	// 非关注 IP 不收集 distinct（watch 生效）
	m2 := countConnections(data, map[string]bool{"10.91.0.99": true}, 10, nil)
	if m2["10.91.0.9"].distinct != nil {
		t.Fatalf("非 watch IP 不应收集 distinct，got %d", len(m2["10.91.0.9"].distinct))
	}
}

// newTestService 构造仅带 DB 的巡查服务（notifier 为 nil，Send 对 nil 安全）。
func newTestService(t *testing.T) *Service {
	t.Helper()
	database, err := db.Init(filepath.Join(t.TempDir(), "patrol.db"))
	if err != nil {
		t.Fatal(err)
	}
	return &Service{
		db:        database,
		states:    make(map[string]*vmState),
		throttles: make(map[string]db.PatrolThrottle),
		sources:   make(map[string]*sourceHealth),
	}
}

func testVM() *agent.VMSummary {
	return &agent.VMSummary{VmId: "vm-1", Ips: []string{"10.91.0.5"}}
}

// checkAbuse 的持续判定：第一轮只记起点，第二轮达到时长才动作（alert）。
func TestAbuseGateAlertAfterDuration(t *testing.T) {
	s := newTestService(t)
	vm := testVM()
	st := &vmState{}
	ac := config.PatrolAbuse{UdpConnLimit: 100, DurationMin: 5, UdpAction: "alert"}
	now := time.Now()

	s.abuseGate(vm, st, now, &st.udpOverSince, true, 5*time.Minute, "alert",
		"UDP 超限", "abuse_udp", ac)
	if st.udpOverSince.IsZero() {
		t.Fatal("第一轮应只记起点")
	}
	if _, n, _ := s.db.QueryPatrolEvents(db.PatrolEventQuery{}, 0, 10); n != 0 {
		t.Fatalf("第一轮不应产生事件，got %d", n)
	}

	s.abuseGate(vm, st, now.Add(6*time.Minute), &st.udpOverSince, true, 5*time.Minute, "alert",
		"UDP 超限", "abuse_udp", ac)
	if !st.udpOverSince.IsZero() {
		t.Fatal("动作后计时应清零")
	}
	events, n, _ := s.db.QueryPatrolEvents(db.PatrolEventQuery{}, 0, 10)
	if n != 1 || len(events) != 1 || events[0].Action != "alert" || events[0].VMID != "vm-1" {
		t.Fatalf("应产生 1 条 vm-1 的 alert 事件，got n=%d events=%+v", n, events)
	}
}

func TestAbuseGateResetWhenUnder(t *testing.T) {
	s := newTestService(t)
	vm := testVM()
	st := &vmState{}
	ac := config.PatrolAbuse{UdpConnLimit: 100, DurationMin: 5, UdpAction: "alert"}
	now := time.Now()

	s.abuseGate(vm, st, now, &st.udpOverSince, true, 5*time.Minute, "alert", "超限", "abuse_udp", ac)
	// 未到时长就回落：起点清零，之后重新累计
	s.abuseGate(vm, st, now.Add(time.Minute), &st.udpOverSince, false, 5*time.Minute, "alert", "超限", "abuse_udp", ac)
	if !st.udpOverSince.IsZero() {
		t.Fatal("回落后起点应清零")
	}
	s.abuseGate(vm, st, now.Add(2*time.Minute), &st.udpOverSince, true, 5*time.Minute, "alert", "超限", "abuse_udp", ac)
	if !st.udpOverSince.Equal(now.Add(2 * time.Minute)) {
		t.Fatalf("回落超限应重新记起点，got %v", st.udpOverSince)
	}
}

func TestAbuseDisabledByDefault(t *testing.T) {
	var a config.PatrolAbuse
	if a.EnabledOn() {
		t.Fatal("Abuse.Enabled 为 nil 时应视为关闭（默认关）")
	}
	on := true
	a.Enabled = &on
	if !a.EnabledOn() {
		t.Fatal("显式开启后 EnabledOn 应为 true")
	}
}

// 自检：连续失败达到阈值告警一次，不重复；恢复后补报并清零。
func TestSourceHealthAlertAndRecover(t *testing.T) {
	s := newTestService(t)
	conf := config.Config{} // SelfCheckFailLimit 零值 → 回落 3

	for i := 0; i < 2; i++ {
		s.sourceFailed(conf, "conntrack", errFake)
	}
	if _, n, _ := s.db.QueryPatrolEvents(db.PatrolEventQuery{}, 0, 10); n != 0 {
		t.Fatalf("未达阈值不应告警，got %d 条事件", n)
	}
	s.sourceFailed(conf, "conntrack", errFake) // 第 3 次 → 告警
	s.sourceFailed(conf, "conntrack", errFake) // 第 4 次 → 不重复
	events, n, _ := s.db.QueryPatrolEvents(db.PatrolEventQuery{Check: "self_check"}, 0, 10)
	if n != 1 || events[0].Action != "alert" || events[0].VMID != selfCheckVMID {
		t.Fatalf("应恰好 1 条 host 的 alert 事件，got n=%d %+v", n, events)
	}

	s.sourceHealthy(conf, "conntrack", "连接跟踪表")
	s.sourceHealthy(conf, "conntrack", "连接跟踪表") // 二次恢复不再报
	_, n, _ = s.db.QueryPatrolEvents(db.PatrolEventQuery{Check: "self_check"}, 0, 10)
	if n != 2 {
		t.Fatalf("告警+恢复应共 2 条事件，got %d", n)
	}
}

type fakeErr struct{}

func (fakeErr) Error() string { return "fake failure" }

var errFake = fakeErr{}

// 匹配语义：名字层精确/尾通配，命令行层整词边界。
func TestMatchKeyword(t *testing.T) {
	cases := []struct {
		name string
		kw   string
		comm string
		exe  string
		args string
		want bool
	}{
		{"名字精确命中", "x-ui", "x-ui", "/usr/local/x-ui/x-ui", "", true},
		{"名字不误杀 nginx-ui", "x-ui", "nginx-ui", "/usr/local/nginx-ui/nginx-ui", "", false},
		{"命令行边界不误杀 nginx-ui", "x-ui", "nginx-ui", "", "/usr/local/nginx-ui/nginx-ui", false},
		{"命令行路径命中", "x-ui", "x-ui-linux-amd64", "", "/usr/local/x-ui/x-ui-linux-amd64", true},
		{"后缀变体靠 args 边界", "transmission", "transmission-daemon", "", "/usr/bin/transmission-daemon --foreground", true},
		{"名字后缀不匹配精确", "deluge", "deluged", "", "", false},
		{"尾通配匹配 deluged", "deluge*", "deluged", "", "", true},
		{"尾通配匹配 hping3", "hping*", "hping3", "", "", true},
		{"池地址命中", "stratum+tcp", "bash", "", "bash -c /tmp/x -o stratum+tcp://pool.example:3333", true},
		{"exe 文件名命中", "xmrig", "bash", "/tmp/xmrig", "", true},
		{"右侧字母不算边界", "xmrig", "", "", "/tmp/xmrig2", false},
		{"大小写不敏感", "XMRIG", "XMRIG", "", "", true},
		{"空关键词", "", "xmrig", "", "", false},
		{"只有通配", "*", "xmrig", "", "", false},
	}
	for _, c := range cases {
		if got := matchKeyword(c.kw, c.comm, c.exe, c.args); got != c.want {
			t.Errorf("%s: matchKeyword(%q,%q,%q,%q)=%v want %v", c.name, c.kw, c.comm, c.exe, c.args, got, c.want)
		}
	}
}

// 连杀窗口：窗口外的记录要被丢弃，窗口内保留。
func TestPruneKillTimes(t *testing.T) {
	now := time.Now()
	st := &vmState{killTimes: []time.Time{
		now.Add(-61 * time.Minute),
		now.Add(-30 * time.Minute),
		now.Add(-time.Minute),
	}}
	pruneKillTimes(st, now)
	if len(st.killTimes) != 2 {
		t.Fatalf("窗口外记录应被丢弃，got %d 条: %v", len(st.killTimes), st.killTimes)
	}
	if !st.killTimes[0].Equal(now.Add(-30 * time.Minute)) {
		t.Fatalf("保留顺序错误: %v", st.killTimes)
	}
}

// fakeProber 只实现巡查用到的探测能力，其余按零值返回。
type fakeProber struct {
	procs   []manager.Process
	listErr error
	killErr error
	killed  []int
}

func (f *fakeProber) ListProcesses(context.Context, string) ([]manager.Process, error) {
	return f.procs, f.listErr
}
func (f *fakeProber) KillProcess(_ context.Context, p manager.Process) error {
	if f.killErr != nil {
		return f.killErr
	}
	f.killed = append(f.killed, p.HostPID)
	return nil
}
func (f *fakeProber) GetVMDiskUsage(context.Context, string) (int64, error) { return 0, nil }
func (f *fakeProber) GetVMVeth(context.Context, string) (string, error)     { return "", nil }
func (f *fakeProber) GetVMIP(context.Context, string) (string, error)       { return "", nil }
func (f *fakeProber) EnsureEgressTbf(context.Context, string, int64) error  { return nil }

// fakeMgr 只覆盖 StopVM；嵌入接口让未用到的几十个方法不必逐个实现。
type fakeMgr struct {
	manager.VMManager
	stopped []string
	stopErr error
}

func (f *fakeMgr) StopVM(_ context.Context, vmID string, _ bool) error {
	if f.stopErr != nil {
		return f.stopErr
	}
	f.stopped = append(f.stopped, vmID)
	return nil
}

func processConf(limit int) config.Config {
	return config.Config{Patrol: config.PatrolConfig{Processes: config.PatrolProcesses{
		Blacklist: []string{"xmrig"}, KillStopLimit: limit,
	}}}
}

func newProcessService(t *testing.T, p *fakeProber, m *fakeMgr) *Service {
	s := newTestService(t)
	s.prober = p
	s.mgr = m
	return s
}

func countEvents(t *testing.T, s *Service, action string) int {
	t.Helper()
	events, n, err := s.db.QueryPatrolEvents(db.PatrolEventQuery{}, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	c := 0
	for i := 0; i < int(n) && i < len(events); i++ {
		if events[i].Action == action {
			c++
		}
	}
	return c
}

// 普通进程命中：杀掉并记事件，不停机。
func TestCheckProcessesKillOnly(t *testing.T) {
	p := &fakeProber{procs: []manager.Process{{PID: 7, HostPID: 107, Comm: "xmrig", Args: "/tmp/xmrig 60"}}}
	m := &fakeMgr{}
	s := newProcessService(t, p, m)
	st := &vmState{}
	s.checkProcesses(processConf(0), testVM(), st)
	if len(p.killed) != 1 || p.killed[0] != 107 {
		t.Fatalf("应杀掉 host pid 107，got %v", p.killed)
	}
	if len(m.stopped) != 0 {
		t.Fatalf("阈值为 0 不应停机，got %v", m.stopped)
	}
	if len(st.killTimes) != 1 {
		t.Fatalf("应记录 1 次命中，got %d", len(st.killTimes))
	}
	if c := countEvents(t, s, "kill_process"); c != 1 {
		t.Fatalf("应产生 1 条 kill_process 事件，got %d", c)
	}
}

// 连杀达到阈值：停机 + 记录停机原因 + 清空计数。
func TestCheckProcessesEscalates(t *testing.T) {
	p := &fakeProber{procs: []manager.Process{{PID: 7, HostPID: 107, Comm: "xmrig"}}}
	m := &fakeMgr{}
	s := newProcessService(t, p, m)
	st := &vmState{}
	conf := processConf(2)
	s.checkProcesses(conf, testVM(), st)
	if len(m.stopped) != 0 {
		t.Fatal("第一次命中不应停机")
	}
	s.checkProcesses(conf, testVM(), st)
	if len(m.stopped) != 1 || m.stopped[0] != "vm-1" {
		t.Fatalf("第二次命中应停机，got %v", m.stopped)
	}
	if len(st.killTimes) != 0 {
		t.Fatalf("停机后计数应清空，got %d", len(st.killTimes))
	}
	if c := countEvents(t, s, "stop_vm"); c != 1 {
		t.Fatalf("应产生 1 条 stop_vm 事件，got %d", c)
	}
	stops := s.db.ListPatrolStops()
	if len(stops) != 1 || stops[0].Reason != "process" {
		t.Fatalf("应记录 process 原因停机，got %+v", stops)
	}
}

// 容器主进程命中：不 kill，直接停机。
func TestCheckProcessesInitStops(t *testing.T) {
	p := &fakeProber{procs: []manager.Process{{PID: 1, HostPID: 101, Comm: "xmrig", Init: true}}}
	m := &fakeMgr{}
	s := newProcessService(t, p, m)
	st := &vmState{}
	s.checkProcesses(processConf(0), testVM(), st)
	if len(p.killed) != 0 {
		t.Fatalf("主进程不应被 kill，got %v", p.killed)
	}
	if len(m.stopped) != 1 {
		t.Fatalf("主进程命中应停机，got %v", m.stopped)
	}
	if len(st.killTimes) != 0 {
		t.Fatalf("主进程停机不应计入连杀阈值")
	}
}

// 边界匹配：nginx-ui 不得被 x-ui 误杀。
func TestCheckProcessesBoundaryNoFalsePositive(t *testing.T) {
	p := &fakeProber{procs: []manager.Process{{
		PID: 9, HostPID: 109, Comm: "nginx-ui",
		Exe: "/usr/local/nginx-ui/nginx-ui", Args: "/usr/local/nginx-ui/nginx-ui",
	}}}
	m := &fakeMgr{}
	s := newProcessService(t, p, m)
	s.checkProcesses(processConf(0), testVM(), &vmState{})
	if len(p.killed) != 0 {
		t.Fatalf("nginx-ui 不应被误杀，got %v", p.killed)
	}
}

// 停机失败：只告警一次（10 分钟限频），不记录停机、不清空计数。
func TestCheckProcessesStopFailureRateLimited(t *testing.T) {
	p := &fakeProber{procs: []manager.Process{{PID: 1, HostPID: 101, Comm: "xmrig", Init: true}}}
	m := &fakeMgr{stopErr: errors.New("boom")}
	s := newProcessService(t, p, m)
	st := &vmState{}
	s.checkProcesses(processConf(0), testVM(), st)
	s.checkProcesses(processConf(0), testVM(), st)
	if c := countEvents(t, s, "alert"); c != 1 {
		t.Fatalf("停机失败应限频告警，got %d 条", c)
	}
	if stops := s.db.ListPatrolStops(); len(stops) != 0 {
		t.Fatalf("停机失败不应记录停机，got %+v", stops)
	}
}
