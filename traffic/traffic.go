package traffic

import (
	"context"
	"fmt"
	"log"
	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"sync"
	"time"
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
	monthlyCycle := monthlyCycleKey(now, resetDay)

	for _, vm := range vms {
		// 该 VM 当前所处的计费周期键（monthly 全局；rental 逐 VM 计算）。
		cycle := monthlyCycle
		if mode == "rental" {
			cycle = s.rentalCycleKey(ctx, vm.VmId, now, monthlyCycle)
		}

		// 从驱动获取 VM 的当前流量值（累计字节数）
		netStats, err := s.mgr.GetVMNetStats(ctx, vm.VmId)
		if err != nil {
			log.Printf("failed to get net stats for VM %s: %v", vm.VmId, err)
			continue
		}

		// 从数据库获取或创建 Traffic 记录
		traffic, err := s.db.GetTraffic(vm.VmId)
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

			// 检查周期是否变化
			if traffic.Month != cycle {
				// 周期切换，重置月度数据
				traffic.Month = cycle
				traffic.MonthIn = 0
				traffic.MonthOut = 0
			}

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

// monthlyCycleKey 计算「每月 resetDay」的周期键（如 "2024-03-15"）。
// 当前日期在 resetDay 之前时归属上一个月的周期。
func monthlyCycleKey(now time.Time, resetDay int) string {
	y, m, d := now.Date()
	if d < resetDay {
		m -= 1
		if m == 0 {
			m = 12
			y -= 1
		}
	}
	return fmt.Sprintf("%04d-%02d-%02d", y, m, resetDay)
}

// rentalCycleKey 计算「VM 首次创建时间 + 30 天」的当前周期键（返回周期开始日）。
// 创建时间由 VMService.GetVMCreatedAt 提供稳定锚点（DB.CreatedAt），重建不会重置；
// 拿不到时回落到 monthly 的周期键，避免流量被误归零。
//
// 锚点稳定后不再需要本地缓存：GetVMCreatedAt 优先走本地 SQLite，成本可忽略，
// 且消除了旧 24h 缓存导致「重建后周期延迟最多一天才刷新」的漂移。
func (s *Service) rentalCycleKey(ctx context.Context, vmID string, now time.Time, fallback string) string {
	created, err := s.mgr.GetVMCreatedAt(ctx, vmID)
	if err != nil || created.IsZero() {
		return fallback
	}
	return rentalCycleKeyFrom(created, now, fallback)
}

// rentalCycleKeyFrom 是 rentalCycleKey 的纯计算部分（便于单测）。
// 以创建时间为起点，每 30 天一个周期；当前周期开始日即周期键。
func rentalCycleKeyFrom(created, now time.Time, fallback string) string {
	if created.IsZero() {
		return fallback
	}
	elapsed := now.Sub(created)
	if elapsed < 0 {
		elapsed = 0
	}
	n := int64(elapsed / (30 * 24 * time.Hour))
	cycleStart := created.Add(time.Duration(n) * 30 * 24 * time.Hour)
	return cycleStart.Format("2006-01-02")
}

// GetVMTraffic 从数据库获取 VM 的累计流量（用于 API）
func (s *Service) GetVMTraffic(vmID string) (*db.Traffic, error) {
	return s.db.GetTraffic(vmID)
}
