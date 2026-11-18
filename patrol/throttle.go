package patrol

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"time"

	"runman-agent/db"
)

// errThrottleNoEffect 表示限速值不低于容器自身的带宽上限，施加后不会更严格。
var errThrottleNoEffect = errors.New("throttle rate not below allocated cap")

// applyThrottle 对 VM 施加限速：落库 + 内存 + 立即打 tc。
// 返回 tc 是否立即生效。VM 已停止 / veth 尚未就绪时 tc 打不上去，
// 此时状态照样落库，由 reconcile 每轮幂等重放，VM 重新拉起后自动补上；
// 调用方据此在事件文案里区分「已生效」与「待 VM 运行后生效」，避免假限速。
func (s *Service) applyThrottle(vmID string, mbps int, releaseAt time.Time, reason string) bool {
	s.mu.Lock()
	s.throttles[vmID] = db.PatrolThrottle{VMID: vmID, RateMbps: mbps, ReleaseAt: releaseAt, Reason: reason}
	s.mu.Unlock()
	if err := s.db.SavePatrolThrottle(vmID, mbps, releaseAt, reason); err != nil {
		log.Printf("[Patrol] save throttle %s: %v", vmID, err)
	}
	if err := s.tcApply(vmID, mbps); err != nil {
		if errors.Is(err, errThrottleNoEffect) {
			log.Printf("[Patrol] throttle %s skipped: %v", vmID, err)
		} else {
			log.Printf("[Patrol] tc throttle %s failed (will retry): %v", vmID, err)
		}
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
		if err := s.tcApply(t.VMID, t.RateMbps); err != nil {
			log.Printf("[Patrol] tc replay %s failed: %v", t.VMID, err)
		}
	}
}

// tcApply 用 tc 把指定 VM 的 veth 限到绝对速率 mbps（Mbps）。
// 与分配上限无关：未设带宽上限的容器同样可以限速。
// 但若容器自身上限更低，限速不会更严格，此时返回 errThrottleNoEffect 跳过。
func (s *Service) tcApply(vmID string, mbps int) error {
	if mbps <= 0 {
		return fmt.Errorf("invalid throttle rate for %s (mbps=%d)", vmID, mbps)
	}
	rate := int64(mbps) * 1000000
	if capBit, err := s.vmCapBps(vmID); err == nil && capBit > 0 && rate >= capBit {
		return fmt.Errorf("%w (%dMbps >= allocated %dMbps)", errThrottleNoEffect, mbps, capBit/1000000)
	}
	veth, err := s.prober.GetVMVeth(context.Background(), vmID)
	if err != nil {
		return err
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
