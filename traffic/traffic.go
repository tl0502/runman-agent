package traffic

import (
	"context"
	"errors"
	"log"
	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"sync"
	"time"

	"gorm.io/gorm"
)

// VMNetStats 表示 VM 的网络流量统计
type VMNetStats struct {
	VMID      string
	InBytes   int64
	OutBytes  int64
	Timestamp time.Time
}

// Service 负责定期收集 VM 流量数据并同步到数据库
type Service struct {
	mgr    manager.VMManager
	db     *db.DB
	cfg    *config.Manager
	mu     sync.Mutex
	ticker *time.Ticker
	done   chan struct{}
}

// NewService 创建新的流量统计服务
func NewService(mgr manager.VMManager, database *db.DB, cfg *config.Manager) *Service {
	return &Service{
		mgr:  mgr,
		db:   database,
		cfg:  cfg,
		done: make(chan struct{}),
	}
}

// Start 启动后台定时同步任务
func (s *Service) Start(ctx context.Context, interval time.Duration) {
	s.mu.Lock()
	if s.ticker != nil {
		s.mu.Unlock()
		return
	}
	s.ticker = time.NewTicker(interval)
	s.mu.Unlock()

	go s.run(ctx)
	log.Printf("Traffic service started with interval %v", interval)
}

// Stop 停止服务
func (s *Service) Stop() {
	s.mu.Lock()
	if s.ticker != nil {
		s.ticker.Stop()
		s.ticker = nil
	}
	s.mu.Unlock()
	close(s.done)
}

func (s *Service) run(ctx context.Context) {
	// 启动时立即同步一次
	s.syncOnce(ctx)

	for {
		select {
		case <-s.done:
			return
		case <-s.ticker.C:
			s.syncOnce(ctx)
		}
	}
}

// syncOnce 执行一次流量同步
func (s *Service) syncOnce(ctx context.Context) {
	vms, err := s.mgr.ListVMs(ctx)
	if err != nil {
		log.Printf("failed to list VMs: %v", err)
		return
	}

	now := time.Now()
	cfg := s.cfg.Get()

	// 重置模式：monthly（每月固定日，全局统一）/ rental（每 VM 按创建时间 + 30 天）。
	mode := cfg.TrafficResetMode
	if mode != "rental" {
		mode = "monthly"
	}
	resetDay := cfg.TrafficResetDay
	if resetDay <= 0 || resetDay > 28 {
		resetDay = 1
	}
	// monthly 模式下所有 VM 共用一个周期键。
	monthlyKey, monthlyStart := monthlyCycle(now, resetDay)

	for _, vm := range vms {
		// 该 VM 当前所处的计费周期键（monthly 全局；rental 逐 VM 计算）。
		// rental 拿不到创建时间锚点时回落 monthly 周期，避免误用零值。
		cycle, cycleStart := monthlyKey, monthlyStart
		if mode == "rental" {
			if created := s.cycleAnchor(ctx, vm.VmId); !created.IsZero() {
				cycle, cycleStart = rentalCycleFrom(created, now)
			}
		}

		// 从驱动获取 VM 的当前流量值（累计字节数）
		netStats, err := s.mgr.GetVMNetStats(ctx, vm.VmId)
		if err != nil {
			log.Printf("failed to get net stats for VM %s: %v", vm.VmId, err)
			continue
		}

		// 从数据库获取或创建 Traffic 记录
		traffic, err := s.db.GetTraffic(vm.VmId)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			// 读库失败不等于新 VM：按新 VM 初始化会把已有累计流量清零。
			log.Printf("failed to read traffic record for VM %s, skipping this round: %v", vm.VmId, err)
			continue
		}
		if err != nil {
			// 新 VM，初始化记录
			traffic = &db.Traffic{
				VMID:      vm.VmId,
				RawIn:     netStats.InBytes,
				RawOut:    netStats.OutBytes,
				TotalIn:   0,
				TotalOut:  0,
				Month:     cycle,
				MonthIn:   0,
				MonthOut:  0,
				UpdatedAt: now,
			}
		} else {
			// 已有记录，计算增量
			deltaIn := netStats.InBytes - traffic.RawIn
			deltaOut := netStats.OutBytes - traffic.RawOut

			// 计数器重置（如容器/宿主机重启），将当前 raw 值全量计入增量
			if deltaIn < 0 {
				deltaIn = netStats.InBytes
			}
			if deltaOut < 0 {
				deltaOut = netStats.OutBytes
			}

			// 累计到总数
			traffic.TotalIn += deltaIn
			traffic.TotalOut += deltaOut

			// 周期键变化时，仅在「时间自然推进到新周期」时才清零。
			// 模式切换 / 改重置日 / 锚点抖动造成的键跳变属于「回溯到过去的周期」，
			// 保留累计值只更新键，否则切一次模式就全员清零（2026-11 实际事故）。
			if shouldResetCycle(traffic.Month, cycle, cycleStart, traffic.UpdatedAt) {
				traffic.MonthIn = 0
				traffic.MonthOut = 0
			}
			traffic.Month = cycle

			// 累计月度数据
			traffic.MonthIn += deltaIn
			traffic.MonthOut += deltaOut

			// 更新原始计数器值和时间戳
			traffic.RawIn = netStats.InBytes
			traffic.RawOut = netStats.OutBytes
			traffic.UpdatedAt = now
		}

		// 保存到数据库
		if err := s.db.SaveTraffic(traffic); err != nil {
			log.Printf("failed to save traffic for VM %s: %v", vm.VmId, err)
		}
	}
}

// cycleAnchor 返回 rental 周期的锚点（实例首次创建时间）。
//
// 必须优先用 DB 里的 vm_configs.created_at：重装是「删旧建新」，容器自身的
// Created 每次都会变成重装那一刻，拿它当锚点会让周期键跳到今天、且新起点
// 晚于 lastSync，被 shouldResetCycle 判定为「自然进入新周期」——于是每次重装
// 都把本月流量清零（实测：11-01 起算的实例，当日重装后 month_in/out 归零，
// 客户白拿一份配额）。DB 锚点由 CreateVM 写入且重装不会覆盖，正是为此保留的。
//
// DB 记录缺失，或早期版本创建的实例 CreatedAt 为零时，回落驱动时间，并把
// 它回填进 DB：只锚定一次，之后重装不再影响周期。回填失败不影响本轮统计。
func (s *Service) cycleAnchor(ctx context.Context, vmID string) time.Time {
	conf, err := s.db.GetVMConfig(vmID)
	if err == nil && conf != nil && !conf.CreatedAt.IsZero() {
		return conf.CreatedAt
	}
	created, err := s.mgr.GetVMCreatedAt(ctx, vmID)
	if err != nil || created.IsZero() {
		return time.Time{}
	}
	if conf != nil {
		conf.CreatedAt = created
		if err := s.db.SaveVMConfig(conf); err != nil {
			log.Printf("traffic: backfill created_at for VM %s failed: %v", vmID, err)
		} else {
			log.Printf("traffic: backfilled created_at for VM %s = %s", vmID, created.Format(time.RFC3339))
		}
	}
	return created
}

// monthlyCycle 计算「每月 resetDay」的周期，返回周期键与周期起点。
// 当前日期在 resetDay 之前时归属上一个月的周期。
func monthlyCycle(now time.Time, resetDay int) (string, time.Time) {
	y, m, d := now.Date()
	if d < resetDay {
		m -= 1
		if m == 0 {
			m = 12
			y -= 1
		}
	}
	start := time.Date(y, m, resetDay, 0, 0, 0, 0, time.Local)
	return start.Format("2006-01-02"), start
}

// rentalCycleFrom 计算「VM 首次创建时间 + 30 天」的当前周期，返回周期键与周期起点。
// 周期起点精确到锚点时分秒（而非格式化日期的 00:00）：周期切换当天、
// 锚点时刻之前的同步窗口仍必须能判定为「新周期」，否则会漏清零。
func rentalCycleFrom(created, now time.Time) (string, time.Time) {
	elapsed := now.Sub(created)
	if elapsed < 0 {
		elapsed = 0
	}
	n := int64(elapsed / (30 * 24 * time.Hour))
	start := created.Add(time.Duration(n) * 30 * 24 * time.Hour)
	return start.Format("2006-01-02"), start
}

// shouldResetCycle 判定周期键变化是否属于「自然周期推进」（需要清零月度计数）。
// 仅当新周期起点晚于上次同步时间才清零；键因模式切换 / 改重置日 / 锚点
// 回落抖动而「跳到过去」时，新周期起点早于上次同步时间，不清零。
func shouldResetCycle(oldKey, newKey string, newCycleStart, lastSync time.Time) bool {
	if oldKey == newKey {
		return false
	}
	return newCycleStart.After(lastSync)
}

// GetVMTraffic 从数据库获取 VM 的累计流量（用于 API）
func (s *Service) GetVMTraffic(vmID string) (*db.Traffic, error) {
	return s.db.GetTraffic(vmID)
}
