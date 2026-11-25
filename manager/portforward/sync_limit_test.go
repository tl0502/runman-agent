package portforward

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"runman-agent/config"
)

// livePorts 返回当前内存里的转发键（tcp/端口），排序后便于比较。
func livePorts(m *Manager, vmID string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0, len(m.mappings[vmID]))
	for _, e := range m.mappings[vmID] {
		keys = append(keys, fmt.Sprintf("%s/%d", e.Protocol, e.HostPort))
	}
	sort.Strings(keys)
	return keys
}

// TestSyncRejectsNewRulesBeyondShrunkenLimit 复现隔离测试问题 1：
// 上限调小后，一次失败的批量同步曾把回滚也卡死在 limit 检查上，
// 导致原规则只恢复一部分（丢规则）。
//
// 修复后的两条语义：
//  1. 含新增、总数超上限的提交在预检阶段整体拒绝，原有规则分毫不动；
//  2. 回滚是"恢复原状"，不受当前上限约束。
func TestSyncRejectsNewRulesBeyondShrunkenLimit(t *testing.T) {
	m, cfg, _ := socketManager(t, "127.0.0.1")
	ctx := context.Background()
	guest := echoTarget(t, "tcp")

	original := make([]DesiredRule, 0, 3)
	var origKeys []string
	for i := 0; i < 3; i++ {
		hp := availablePort(t, "tcp")
		if err := m.AddMapping(ctx, "test", "tcp", hp, guest, "", BindBoth); err != nil {
			t.Fatalf("setup rule %d: %v", i, err)
		}
		original = append(original, DesiredRule{Protocol: "tcp", HostPort: hp, GuestPort: guest, BindFamily: BindBoth})
		origKeys = append(origKeys, fmt.Sprintf("tcp/%d", hp))
	}

	// 把上限调小到低于存量数：存量 3 条、上限 2。
	if err := cfg.Update(func(c *config.Config) { c.MaxPortForward = 2 }); err != nil {
		t.Fatal(err)
	}

	// 1) 全量换成 3 条全新端口（含新增、超上限）→ 必须整体拒绝且原 3 条完好。
	fresh := make([]DesiredRule, 0, 3)
	for i := 0; i < 3; i++ {
		fresh = append(fresh, DesiredRule{Protocol: "tcp", HostPort: availablePort(t, "tcp"), GuestPort: guest, BindFamily: BindBoth})
	}
	if err := m.SyncForVM(ctx, "test", fresh); err == nil {
		t.Fatal("expected rejection: new rules beyond shrunken limit")
	}
	if got, want := livePorts(m, "test"), sorted(origKeys); !equal(got, want) {
		t.Fatalf("rules damaged after failed sync: got %v, want %v", got, want)
	}

	// 2) 存量幂等更新（原 3 条原样提交）→ 必须放行，不能被调小的上限卡死。
	if err := m.SyncForVM(ctx, "test", original); err != nil {
		t.Fatalf("idempotent re-sync of existing rules must pass: %v", err)
	}
	if got, want := livePorts(m, "test"), sorted(origKeys); !equal(got, want) {
		t.Fatalf("idempotent sync changed rules: got %v, want %v", got, want)
	}

	// 3) 正常范围内的新增仍可用（上限恢复到 20）。
	if err := cfg.Update(func(c *config.Config) { c.MaxPortForward = 20 }); err != nil {
		t.Fatal(err)
	}
	extra := DesiredRule{Protocol: "tcp", HostPort: availablePort(t, "tcp"), GuestPort: guest, BindFamily: BindBoth}
	if err := m.SyncForVM(ctx, "test", append(original, extra)); err != nil {
		t.Fatalf("add within limit failed: %v", err)
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
