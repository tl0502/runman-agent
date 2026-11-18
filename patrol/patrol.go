// Package patrol 实现自动巡查：周期探测 VM 的带宽/活跃连接、CPU/内存/磁盘、
// 容器内进程黑名单，命中规则后执行限速/停止/重启/杀进程，并记录事件、发 Telegram 通知。
package patrol

import (
	"context"
	"log"
	"sync"
	"time"

	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/proto/agent"
)

// VMProber 是巡查所需的、由具体驱动（目前仅 podman）提供的探测能力。
type VMProber interface {
	ListProcesses(ctx context.Context, vmID string) ([]manager.Process, error)
	KillProcess(ctx context.Context, vmID string, pid int) error
	GetVMDiskUsage(ctx context.Context, vmID string) (int64, error)
	GetVMVeth(ctx context.Context, vmID string) (string, error)
	GetVMIP(ctx context.Context, vmID string) (string, error)
}

// Service 是巡查引擎：ticker 周期巡检，判定 + 动作 + 落库都在本进程内完成，
// 不依赖平台。面板与 API 只读它的结果（事件日志 / 限速状态）。
type Service struct {
	mgr    manager.VMManager
	prober VMProber
	db     *db.DB
	cfg    *config.Manager

	mu        sync.Mutex
	states    map[string]*vmState
	throttles map[string]db.PatrolThrottle

	done chan struct{}
}

// vmState 保存单个 VM 跨巡检周期的阈值判定状态（用于「持续 N 分钟」与冷却）。
type vmState struct {
	lastIn, lastOut int64 // 上次采样的累计流量（字节），用于计算速率
	lastNetAt       time.Time

	bwOverSince   time.Time // 带宽开始超阈值的时间
	connOverSince time.Time // 连接数开始超阈值的时间
	cpuOverSince  time.Time
	memOverSince  time.Time
	diskAlerted   bool      // 磁盘告警去重（只在 ok→bad 转换时告警一次）
	lastActionAt  time.Time // 最近一次自动动作时间（重启/关机冷却）
}

func NewService(mgr manager.VMManager, prober VMProber, database *db.DB, cfg *config.Manager) *Service {
	s := &Service{
		mgr:       mgr,
		prober:    prober,
		db:        database,
		cfg:       cfg,
		states:    make(map[string]*vmState),
		throttles: make(map[string]db.PatrolThrottle),
		done:      make(chan struct{}),
	}
	// 启动时恢复上次未到期的限速（重启后继续生效）。
	for _, t := range database.ListPatrolThrottles() {
		s.throttles[t.VMID] = t
	}
	return s
}

// Start 启动巡查循环。首次调用前先立即巡检一轮。
func (s *Service) Start(ctx context.Context) {
	go func() {
		s.tick()
		for {
			interval := time.Duration(s.cfg.Get().Patrol.IntervalSec) * time.Second
			if interval < 5*time.Second {
				interval = 5 * time.Second
			}
			select {
			case <-s.done:
				return
			case <-ctx.Done():
				return
			case <-time.After(interval):
				s.tick()
			}
		}
	}()
}

// Stop 停止巡查循环。
func (s *Service) Stop() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

func (s *Service) getState(vmID string) *vmState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[vmID]
	if !ok {
		st = &vmState{}
		s.states[vmID] = st
	}
	return st
}

// isThrottled 判断该 VM 当前是否处于限速状态。
func (s *Service) isThrottled(vmID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.throttles[vmID]
	return ok
}

// ActiveThrottles 返回当前生效中的限速状态（供 API / 面板展示）。
func (s *Service) ActiveThrottles() []db.PatrolThrottle {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]db.PatrolThrottle, 0, len(s.throttles))
	for _, t := range s.throttles {
		out = append(out, t)
	}
	return out
}

// ActiveStops 返回当前「被巡查停机」的 VM 列表。
func (s *Service) ActiveStops() []db.PatrolStop {
	return s.db.ListPatrolStops()
}

// ReleaseThrottle 人工解除某个 VM 的限速（立即恢复原上限）。
func (s *Service) ReleaseThrottle(vmID string) {
	s.releaseThrottle(vmID)
}

func (s *Service) tick() {
	conf := s.cfg.Get()
	// 先做限速的到期释放 / 幂等重放（无论巡查总开关是否关闭，限速都应按时恢复）。
	s.reconcileThrottles()

	if !conf.PatrolEnabled() {
		return
	}

	ctx := context.Background()
	vms, err := s.mgr.ListVMs(ctx)
	if err != nil {
		log.Printf("[Patrol] list VMs: %v", err)
		return
	}

	// 停机异常的恢复检测：VM 被用户重新启动（或已被删除）后清除「被停机」状态。
	s.reconcileStops(vms)

	// 一次读取 conntrack，为所有 VM 统计 in/out 活跃连接，避免每个 VM 重复解析。
	connMap := countAllConnections()

	now := time.Now()
	for _, vm := range vms {
		if vm.Status != agent.VMStatus_VM_STATUS_RUNNING {
			continue
		}
		st := s.getState(vm.VmId)
		s.checkBandwidth(conf, vm, st, now)
		s.checkConnections(conf, vm, connMap, st, now)
		s.checkResources(conf, vm, st, now)
		s.checkProcesses(conf, vm)
	}
}

// reconcileStops 清除已恢复的「被停机」状态：VM 被用户重新启动 → 解除异常并记恢复事件；
// VM 已不存在（被删除）→ 直接清理残留记录。
func (s *Service) reconcileStops(vms []*agent.VMSummary) {
	running := make(map[string]bool, len(vms))
	existing := make(map[string]bool, len(vms))
	for _, vm := range vms {
		existing[vm.VmId] = true
		if vm.Status == agent.VMStatus_VM_STATUS_RUNNING {
			running[vm.VmId] = true
		}
	}
	for _, st := range s.db.ListPatrolStops() {
		if running[st.VMID] {
			_ = s.db.DeletePatrolStop(st.VMID)
			s.emit(st.VMID, st.Reason, "recovered", "VM 已恢复运行，停机异常解除", true)
		} else if !existing[st.VMID] {
			_ = s.db.DeletePatrolStop(st.VMID)
		}
	}
}

// emit 记录一条巡查事件，notify 为 true 时同步发 Telegram。
func (s *Service) emit(vmID, check, action, detail string, notify bool) {
	s.db.InsertPatrolEvent(vmID, check, action, detail)
	if notify {
		s.notify(vmID, check, detail)
	}
}

// notify 发 Telegram 通知（仅当配置了 token/chat_id 且开关开启）。
// 异步发送：Telegram 网络延迟不应阻塞巡检循环。
func (s *Service) notify(vmID, check, detail string) {
	conf := s.cfg.Get()
	if !conf.TelegramEnabled() {
		return
	}
	token, chatID := conf.Telegram.BotToken, conf.Telegram.ChatID
	go sendTelegram(token, chatID, "巡查告警 ["+vmID+"] ("+check+")\n"+detail)
}
