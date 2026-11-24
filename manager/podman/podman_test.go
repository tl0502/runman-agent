//go:build containers_image_openpgp

package podman

import (
	"net"
	"testing"
)

// 容器内进程的 status 样本：NSpid 第 2 项才是容器命名空间视角的 PID。
func TestParseProcStatus(t *testing.T) {
	data := "Name:\txmrig\nUmask:\t0022\nState:\tS (sleeping)\nTgid:\t12345\nPid:\t12345\nNSpid:\t12345\t42\n"
	state, comm, nsPID, err := parseProcStatus(data)
	if err != nil {
		t.Fatal(err)
	}
	if state != 'S' {
		t.Fatalf("state=%q want S", state)
	}
	if comm != "xmrig" {
		t.Fatalf("comm=%q want xmrig", comm)
	}
	if nsPID != 42 {
		t.Fatalf("nsPID=%d want 42", nsPID)
	}
}

// 僵尸进程必须能识别出来（调用方据此跳过，避免杀了没用还刷事件）。
func TestParseProcStatusZombie(t *testing.T) {
	state, _, _, err := parseProcStatus("Name:\tdefunct\nState:\tZ (zombie)\n")
	if err != nil {
		t.Fatal(err)
	}
	if state != 'Z' {
		t.Fatalf("state=%q want Z", state)
	}
}

// 缺少 Name 的异常输入要报错，避免把空进程名当成正常进程。
func TestParseProcStatusNoName(t *testing.T) {
	if _, _, _, err := parseProcStatus("State:\tS (sleeping)\n"); err == nil {
		t.Fatal("缺少 Name 应报错")
	}
}

// comm 可被进程改写并注入伪造行；解析必须只认第一处 NSpid/State。
func TestParseProcStatusIgnoresInjectedLines(t *testing.T) {
	data := "Name:\tfake\nState:\tS (sleeping)\nNSpid:\t4411\t17\n" +
		"State:\tZ (zombie)\nNSpid:\t4411\t1\n"
	state, comm, nsPID, err := parseProcStatus(data)
	if err != nil {
		t.Fatal(err)
	}
	if state != 'S' || nsPID != 17 {
		t.Fatalf("应使用第一处真实值，got state=%q nsPID=%d", state, nsPID)
	}
	if comm != "fake" {
		t.Fatalf("comm=%q", comm)
	}
}

// 共享 IPv6 模式下容器只有 ULA，不能作为公网地址上报给平台。
func TestIsPublicIPv6(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"", false},
		{"10.91.0.2", false},
		{"fd91:cafe:cafe:10::2", false}, // 共享模式 ULA
		{"fc00::1", false},              // ULA 另一端
		{"fe80::1", false},              // 链路本地
		{"::1", false},                  // 回环
		{"ff02::1", false},              // 组播
		{"2001:db8::1", true},           // subnet 模式公网段
		{"2408:8000::1", true},
	}
	for _, c := range cases {
		if got := isPublicIPv6(net.ParseIP(c.ip)); got != c.want {
			t.Fatalf("isPublicIPv6(%q)=%v want %v", c.ip, got, c.want)
		}
	}
}

// 上报前过滤：ULA / 链路本地 / 非法值剔除，v4 与公网 v6 保留。
func TestFilterReportableIPs(t *testing.T) {
	in := []string{"10.91.0.2", "fd91:cafe:cafe:10::2", "2001:db8::1", "fe80::1", "bogus"}
	got := FilterReportableIPs(in)
	want := []string{"10.91.0.2", "2001:db8::1"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
