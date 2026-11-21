//go:build containers_image_openpgp

package podman

import "testing"

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
