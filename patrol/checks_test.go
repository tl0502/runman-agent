package patrol

import "testing"

func TestCountConnections(t *testing.T) {
	// 模拟 /proc/net/nf_conntrack 的内容。
	// 每条 ipv4 行：原始方向 src/dst 在前，reply 方向 src/dst 在后。
	data := `ipv4     2 tcp      6 431999 ESTABLISHED src=10.91.0.2 dst=1.2.3.4 sport=60000 dport=443 src=1.2.3.4 dst=10.91.0.2 sport=443 dport=60000 [ASSURED] mark=0 zone=0 use=2
ipv4     2 tcp      6 299 ESTABLISHED src=5.6.7.8 dst=10.91.0.2 sport=22 dport=50000 src=10.91.0.2 dst=5.6.7.8 sport=50000 dport=22 [ASSURED] mark=0 zone=0 use=2
ipv4     2 tcp      6 120 TIME_WAIT src=10.91.0.2 dst=9.9.9.9 sport=40000 dport=80 src=9.9.9.9 dst=10.91.0.2 sport=80 dport=40000 [ASSURED] mark=0 zone=0 use=2
ipv4     2 udp     17 29 src=10.91.0.2 dst=8.8.8.8 sport=53 dport=53 [UNREPLIED] src=8.8.8.8 dst=10.91.0.2 sport=53 dport=53 mark=0 zone=0 use=2
ipv4     2 udp     17 29 src=10.91.0.3 dst=10.91.0.2 sport=60001 dport=60001 [ASSURED] src=10.91.0.2 dst=10.91.0.3 sport=60001 dport=60001 mark=0 zone=0 use=2
ipv6     2 tcp      6 100 ESTABLISHED src=2001:db8::2 dst=2001:db8::3 sport=1 dport=2 src=2001:db8::3 dst=2001:db8::2 sport=2 dport=1 [ASSURED] mark=0 zone=0 use=2
`

	m := countConnections(data)

	if m["10.91.0.2"].out != 1 {
		t.Fatalf("10.91.0.2 out = %d, want 1 (TIME_WAIT 与 UDP UNREPLIED 不应计入)", m["10.91.0.2"].out)
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
		t.Fatalf("9.9.9.9 = %+v, want 0/0（TIME_WAIT 不计）", m["9.9.9.9"])
	}
	// 纯 ipv6 行不应计入 ipv4 IP 计数（src/dst 匹配不到 10.91.0.2）
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
