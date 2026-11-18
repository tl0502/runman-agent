package patrol

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"

	"runman-agent/db"
)

// applyThrottle 对 VM 施加限速：落库 + 内存 + 立即打 tc。
// 返回 tc 是否立即生效。VM 已停止 / veth 尚未就绪时 tc 打不上去，
// 此时状态照样落库，由 reconcile 每轮幂等重放，VM 重新拉起后自动补上；
// 调用方据此在事件文案里区分「已生效」与「待 VM 运行后生效」，避免假限速。
func (s *Service) applyThrottle(vmID string, pct int, releaseAt time.Time, reason string) bool {
	s.mu.Lock()
	s.throttles[vmID] = db.PatrolThrottle{VMID: vmID, RatePct: pct, ReleaseAt: releaseAt, Reason: reason}
	s.mu.Unlock()
	if err := s.db.SavePatrolThrottle(vmID, pct, releaseAt, reason); err != nil {
		log.Printf("[Patrol] save throttle %s: %v", vmID, err)
	}
	if err := s.tcApply(vmID, pct); err != nil {
		log.Printf("[Patrol] tc throttle %s failed (will retry): %v", vmID, err)
		return false
	}
	return true
}

// releaseThrottle 解除限速：恢复原上限 + 落库删除 + 内存删除 + 记录事件 + 通知。
func (s *Service) releaseThrottle(vmID string) {
	s.mu.Lock()
	t, ok := s.throttles[vmID]
	if ok {
		delete(s.throttles, vmID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = s.db.DeletePatrolThrottle(vmID)
	if err := s.tcRestore(vmID); err != nil {
		log.Printf("[Patrol] tc restore %s failed: %v", vmID, err)
	}
	s.emit(vmID, t.Reason, "release", fmt.Sprintf("限速解除，恢复原带宽上限"), true)
}

// reconcileThrottles 每轮巡检执行：到期释放 + 未到期幂等重放（兜住 veth 重建）。
func (s *Service) reconcileThrottles() {
	now := time.Now()
	s.mu.Lock()
	var expired []string
	var active []db.PatrolThrottle
	for id, t := range s.throttles {
		if !t.ReleaseAt.IsZero() && now.After(t.ReleaseAt) {
			expired = append(expired, id)
		} else {
			active = append(active, t)
		}
	}
	s.mu.Unlock()

	for _, id := range expired {
		s.releaseThrottle(id)
	}
	for _, t := range active {
		if err := s.tcApply(t.VMID, t.RatePct); err != nil {
			log.Printf("[Patrol] tc replay %s failed: %v", t.VMID, err)
		}
	}
}

// tcApply 用 tc 把指定 VM 的 veth 限到 cap*pct/100。
func (s *Service) tcApply(vmID string, pct int) error {
	veth, err := s.prober.GetVMVeth(context.Background(), vmID)
	if err != nil {
		return err
	}
	capBit, err := s.vmCapBps(vmID)
	if err != nil {
		return err
	}
	rate := capBit * int64(pct) / 100
	if rate <= 0 {
		return fmt.Errorf("invalid throttle rate for %s (pct=%d)", vmID, pct)
	}
	return runTC(veth, rate)
}

// tcRestore 恢复 VM 的原带宽上限：有上限则 tc 改回原上限，无上限则删掉 qdisc。
func (s *Service) tcRestore(vmID string) error {
	veth, err := s.prober.GetVMVeth(context.Background(), vmID)
	if err != nil {
		return err
	}
	capBps, err := s.vmCapBps(vmID)
	if err != nil {
		return err
	}
	if capBps <= 0 {
		// 原本没限速：直接删掉覆盖上去的 qdisc。
		return exec.Command("tc", "qdisc", "del", "dev", veth, "root").Run()
	}
	return runTC(veth, capBps)
}

// vmCapBps 返回 VM 分配带宽上限的数值（bit/s），与 tc 的 "rate <n>bit" 直接对应
// （netavark 创建容器时同样用 Mbps×1e6 作为 bandwidth_rate）。
// 无上限（BandwidthMbps<=0）返回 0。
func (s *Service) vmCapBps(vmID string) (int64, error) {
	cfg, err := s.db.GetVMConfig(vmID)
	if err != nil {
		return 0, err
	}
	if cfg.BandwidthMbps <= 0 {
		return 0, nil
	}
	return int64(cfg.BandwidthMbps) * 1000000, nil
}

// runTC 在 veth 上打一条 TBF 限速 qdisc（与 netavark 同款命令）。
func runTC(veth string, rateBps int64) error {
	burst := rateBps / 10
	if burst < 1000000 {
		burst = 1000000
	}
	return exec.Command("tc", "qdisc", "replace", "dev", veth, "root", "tbf",
		"rate", fmt.Sprintf("%dbit", rateBps),
		"burst", fmt.Sprintf("%db", burst),
		"latency", "50ms").Run()
}
