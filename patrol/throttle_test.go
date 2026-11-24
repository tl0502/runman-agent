package patrol

import (
	"context"
	"errors"
	"testing"
	"time"

	"runman-agent/db"
	"runman-agent/proto/agent"
)

type vethFailProber struct{ fakeProber }

func (vethFailProber) GetVMVeth(context.Context, string) (string, error) {
	return "", errors.New("veth not ready")
}

type statusMgr struct {
	fakeMgr
	status agent.VMStatus
}

func (m *statusMgr) GetVMInfo(context.Context, string) (*agent.VMSummary, error) {
	return &agent.VMSummary{VmId: "vm-1", Status: m.status}, nil
}

func newThrottledService(t *testing.T, status agent.VMStatus, releaseAt time.Time) *Service {
	s := newTestService(t)
	s.prober = &vethFailProber{}
	s.mgr = &statusMgr{status: status}
	if err := s.db.SaveVMConfig(&db.VMConfig{VMID: "vm-1", BandwidthMbps: 100}); err != nil {
		t.Fatal(err)
	}
	s.throttles["vm-1"] = db.PatrolThrottle{VMID: "vm-1", RateMbps: 10, ReleaseAt: releaseAt, Reason: "bandwidth"}
	_ = s.db.SavePatrolThrottle("vm-1", 10, releaseAt, "bandwidth")
	return s
}

// 恢复失败且 VM 在运行：记录必须保留并标为已到期，不能报「已解除」。
func TestReleaseFailureKeepsRecordForRetry(t *testing.T) {
	s := newThrottledService(t, agent.VMStatus_VM_STATUS_RUNNING, time.Now().Add(time.Hour))
	if err := s.ReleaseThrottle("vm-1"); err == nil {
		t.Fatal("恢复失败应返回错误")
	}
	got := s.db.ListPatrolThrottles()
	if len(got) != 1 || !s.isThrottled("vm-1") {
		t.Fatalf("记录应保留以便重试，db=%d", len(got))
	}
	if !time.Now().After(got[0].ReleaseAt) {
		t.Fatal("人工解除失败后应标记为已到期，避免下一轮把限速重放回去")
	}
	if n := countEvents(t, s, "release"); n != 0 {
		t.Fatalf("不应记录解除成功事件，got %d", n)
	}
}

// VM 已停止：没有 qdisc 可恢复，重启时按套餐重装，记录直接清理。
func TestReleaseOnStoppedVMDropsRecord(t *testing.T) {
	s := newThrottledService(t, agent.VMStatus_VM_STATUS_STOPPED, time.Now().Add(-time.Minute))
	s.reconcileThrottles()
	if len(s.db.ListPatrolThrottles()) != 0 || s.isThrottled("vm-1") {
		t.Fatal("已停止 VM 的到期限速应被清理")
	}
}
