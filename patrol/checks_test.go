package patrol

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"runman-agent/config"
	"runman-agent/db"
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
