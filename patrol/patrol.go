// Package patrol 实现自动巡查：周期探测 VM 的带宽/活跃连接、CPU/内存/磁盘、
// 容器内进程黑名单，命中规则后执行限速/停止/重启/杀进程，并记录事件、发 Telegram 通知。
package patrol

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/notify"
	"runman-agent/proto/agent"
)

// VMProber 是巡查所需的、由具体驱动（目前仅 podman）提供的探测能力。
type VMProber interface {
	ListProcesses(ctx context.Context, vmID string) ([]manager.Process, error)
	KillProcess(ctx context.Context, vmID string, pid int) error
	GetVMDiskUsage(ctx context.Context, vmID string) (int64, error)
	GetVMVeth(ctx context.Context, vmID string) (string, error)
	GetVMIP(ctx context.Context, vmID string) (string, error)
	// EnsureEgressTbf 施加/清除容器内出方向限速（rateBps<=0 表示清除），
	// 与 GetVMVeth 上的下载方向限速合成双向控制。
	EnsureEgressTbf(ctx context.Context, vmID string, rateBps int64) error
}

// Service 是巡查引擎：ticker 周期巡检，判定 + 动作 + 落库都在本进程内完成，
// 不依赖平台。面板与 API 只读它的结果（事件日志 / 限速状态）。
type Service struct {
	mgr      manager.VMManager
	prober   VMProber
	db       *db.DB
	cfg      *config.Manager
	notifier *notify.Notifier

	mu        sync.Mutex
	states    map[string]*vmState
	throttles map[string]db.PatrolThrottle
	// sources 是巡查数据源的自检状态：连续失败达到阈值告警一次，恢复时再报。
	// 没有它，「没收到告警」无法区分「没问题」和「巡查瞎了」。
	sources map[string]*sourceHealth

	done chan struct{}
}

// sourceHealth 是单个数据源的连续失败计数与告警态。
type sourceHealth struct {
	fails   int
	alerted bool
}

// selfCheckVMID 是自检事件在事件日志里挂的虚拟 VM 标识（自检是宿主级视角）。
const selfCheckVMID = "host"

// vmState 保存单个 VM 跨巡检周期的阈值判定状态（用于「持续 N 分钟」与冷却）。
type vmState struct {
	lastIn, lastOut int64 // 上次采样的累计流量（字节），用于计算速率
	lastNetAt       time.Time

	bwOverSince   time.Time // 带宽开始超阈值的时间
	connOverSince time.Time // 连接数开始超阈值的时间
	udpOverSince  time.Time // 滥用识别：UDP 连接数
	scanOverSince time.Time // 滥用识别：不同目标 IP 数
	smtpOverSince time.Time // 滥用识别：SMTP 端口命中
	cpuOverSince  time.Time
	memOverSince  time.Time
	// lastDiskAlertAt 上次磁盘告警时间。同一 VM 的磁盘告警按配置间隔限频
	// （默认 6 小时），避免持续超限/在阈值附近抖动时反复刷日志。
	lastDiskAlertAt time.Time
	// procErrAt 进程枚举失败最近一次记日志的时间。容器镜像可能没有 ps
	// （top 依赖容器内 ps），限频记日志让「进程巡查跳过」可见而不是静默。
	procErrAt    time.Time
	lastActionAt time.Time // 最近一次自动动作时间（重启/关机冷却）
}

func NewService(mgr manager.VMManager, prober VMProber, database *db.DB, cfg *config.Manager, notifier *notify.Notifier) *Service {
	s := &Service{
		mgr:       mgr,
		prober:    prober,
		db:        database,
		cfg:       cfg,
		notifier:  notifier,
		states:    make(map[string]*vmState),
		throttles: make(map[string]db.PatrolThrottle),
		sources:   make(map[string]*sourceHealth),
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
		s.sourceFailed(conf, "podman", err)
		return
	}
	s.sourceHealthy(conf, "podman", "容器列表")

	// 停机异常的恢复检测：VM 被用户重新启动（或已被删除）后清除「被停机」状态。
	s.reconcileStops(vms)

	// 一次读取 conntrack，为所有 VM 统计连接画像（活跃 in/out + 滥用识别三件套），
	// 避免每个 VM 重复解析。watch 限定容器 IP：滥用画像只对容器有意义，
	// 也把不同目标集合的内存开销约束在「容器数 × 饱和上限」内。
	watch := make(map[string]bool, len(vms))
	for _, vm := range vms {
		if ip := vmIPv4(vm); ip != "" {
			watch[ip] = true
		}
	}
	scanCap, smtpPorts := abuseParseParams(conf)
	connMap, err := countAllConnections(watch, scanCap, smtpPorts)
	if err != nil {
		log.Printf("[Patrol] read conntrack: %v", err)
		s.sourceFailed(conf, "conntrack", err)
		connMap = nil // 本轮失明：连接类判定清零，其他检查照常
	} else {
		s.sourceHealthy(conf, "conntrack", "连接跟踪表")
	}

	now := time.Now()
	for _, vm := range vms {
		if vm.Status != agent.VMStatus_VM_STATUS_RUNNING {
			continue
		}
		st := s.getState(vm.VmId)
		s.checkBandwidth(conf, vm, st, now)
		s.checkConnections(conf, vm, connMap, st, now)
		s.checkAbuse(conf, vm, connMap, st, now)
		s.checkResources(conf, vm, st, now)
		s.checkProcesses(conf, vm, st)
	}
}

// abuseParseParams 从配置提取 conntrack 解析所需的滥用识别参数：
// scanCap 是不同目标数的饱和上限（0=不收集），smtpPorts 是发信端口黑名单。
// 滥用识别关闭时两者皆空，解析循环不承担画像开销。
func abuseParseParams(conf config.Config) (int, map[int]bool) {
	a := conf.Patrol.Abuse
	if !a.EnabledOn() {
		return 0, nil
	}
	ports := make(map[int]bool, len(a.SmtpPorts))
	for _, p := range a.SmtpPorts {
		if p > 0 && p < 65536 {
			ports[p] = true
		}
	}
	var scanCap int
	if a.ScanDstLimit > 0 {
		scanCap = a.ScanDstLimit
	}
	return scanCap, ports
}

// sourceFailed 记录数据源一次失败：连续失败达到阈值时告警一次（已告警不重复），
// 恢复由 sourceHealthy 上报。自检跟随巡查总开关（tick 只在开启时执行）。
func (s *Service) sourceFailed(conf config.Config, name string, err error) {
	limit := conf.Patrol.SelfCheckFailLimit
	if limit <= 0 {
		limit = 3
	}
	s.mu.Lock()
	h := s.sources[name]
	if h == nil {
		h = &sourceHealth{}
		s.sources[name] = h
	}
	h.fails++
	fire := h.fails >= limit && !h.alerted
	if fire {
		h.alerted = true
	}
	s.mu.Unlock()
	if fire {
		s.emit(selfCheckVMID, "self_check", "alert",
			fmt.Sprintf("巡查数据源 %s 连续 %d 次读取失败，相关检查暂停：%v", name, h.fails, err), true)
	}
}

// sourceHealthy 记录数据源恢复正常：清零计数；此前处于告警态时补报恢复。
func (s *Service) sourceHealthy(conf config.Config, name, label string) {
	s.mu.Lock()
	h := s.sources[name]
	wasAlerted := h != nil && h.alerted
	if h != nil {
		h.fails = 0
		h.alerted = false
	}
	s.mu.Unlock()
	if wasAlerted {
		s.emit(selfCheckVMID, "self_check", "recovered", label+"已恢复，相关检查继续", true)
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
// 通道能力在 notify 包，这里只提交文本；实际发送异步，不阻塞巡检循环。
func (s *Service) notify(vmID, check, detail string) {
	s.notifier.Send(notify.KindPatrol, "巡查告警 ["+vmID+"] ("+check+")\n"+detail)
}
