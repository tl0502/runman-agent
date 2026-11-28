//go:build containers_image_openpgp

package podman

import (
	"testing"

	"runman-agent/config"
)

// TestContainerSecurityConfigApparmor 守住 2026-11-28 的回归：
//
// 只开 no_new_privileges 而不显式 unconfined 时，Ubuntu 上容器进程标签会变成
// "<profile>//&crun" 堆叠标签，AppArmor 的 base 抽象放行不了对端信号，导致
// VM 内 kill / systemctl stop 全部失败（kill -0 却能过，很容易被单测漏掉）。
// 这里断言的是「最终交给运行时的字段」，改回只设 NNP 会立刻失败。
func TestContainerSecurityConfigApparmor(t *testing.T) {
	orig := apparmorAvailable
	t.Cleanup(func() { apparmorAvailable = orig })

	on := true
	off := false

	tests := []struct {
		name        string
		apparmor    bool
		nnp         *bool
		wantProfile string
		wantNNP     bool
	}{
		// 默认配置（NNP nil → true）+ AppArmor 宿主：必须 unconfined，
		// 否则容器内无法发信号。这是唯一会「坏用户功能」的组合。
		{"nnp default on apparmor host", true, nil, "unconfined", true},
		{"nnp explicit on apparmor host", true, &on, "unconfined", true},
		// 关掉 NNP 时标签干净，默认 profile 工作正常，不该顺手削弱它。
		{"nnp off apparmor host", true, &off, "", false},
		// 宿主没有 AppArmor（RHEL 系 / 精简镜像）：不传 profile，避免运行时报错。
		{"nnp on without apparmor", false, nil, "", true},
		{"nnp off without apparmor", false, &off, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apparmorAvailable = func() bool { return tt.apparmor }

			got := containerSecurityConfig(config.Config{ContainerNoNewPrivs: tt.nnp})

			if got.ApparmorProfile != tt.wantProfile {
				t.Fatalf("ApparmorProfile = %q, want %q（容器内 kill 会失败）", got.ApparmorProfile, tt.wantProfile)
			}
			if got.NoNewPrivileges == nil || *got.NoNewPrivileges != tt.wantNNP {
				t.Fatalf("NoNewPrivileges = %v, want %v", got.NoNewPrivileges, tt.wantNNP)
			}
		})
	}
}
