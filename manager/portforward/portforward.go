package portforward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// IPCount 记录单个来源 IP 的连接情况。
type IPCount struct {
	IP          string `json:"ip"`
	ActiveCount int64  `json:"active_count"` // 当前活跃连接数
	Count       int64  `json:"count"`        // 历史总连接数
}

const (
	MinPort = 1024
	MaxPort = 65535
)

// 端口映射的地址族。0 为存量默认值（仅 IPv4），落库前不做迁移。
const (
	BindV4   = 0
	BindV6   = 1
	BindBoth = 2
)

// ValidBindFamily 校验地址族取值。
func ValidBindFamily(f int) bool { return f >= BindV4 && f <= BindBoth }

// --- 转发管理实现 ---

type Entry struct {
	VMID        string
	Protocol    string // "tcp" or "udp"
	HostPort    int
	GuestPort   int
	TargetAddr  string // resolved "ip:port" at setup time, for diagnostics
	Description string
	BindFamily  int // 0 = v4 only, 1 = v6 only, 2 = both

	// Stats — populated by GetReport(), zero in live entries.
	ActiveConns int64
	TotalConns  int64
	TopIPs      []IPCount

	// Internal live state — do not copy after first use.
	cancel        context.CancelFunc
	lns           []io.Closer  // one listener per enabled address family
	binds         []listenSpec // actual bind addresses, retained for failed replacements
	connActive    int64        // atomic: current active connections
	connTotal     int64        // atomic: all-time connections since rule added
	connIPMu      sync.Mutex
	connIPs       map[string]int64 // total connections per IP
	connActiveIPs map[string]int64 // active connections per IP
}

func (e *Entry) trackConnect(ip string) {
	atomic.AddInt64(&e.connActive, 1)
	atomic.AddInt64(&e.connTotal, 1)
	e.connIPMu.Lock()
	e.connIPs[ip]++
	e.connActiveIPs[ip]++
	e.connIPMu.Unlock()
}

func (e *Entry) trackDisconnect(ip string) {
	atomic.AddInt64(&e.connActive, -1)
	e.connIPMu.Lock()
	if e.connActiveIPs[ip] > 1 {
		e.connActiveIPs[ip]--
	} else {
		delete(e.connActiveIPs, ip)
	}
	e.connIPMu.Unlock()
}

func (e *Entry) getTopIPs() []IPCount {
	e.connIPMu.Lock()
	result := make([]IPCount, 0, len(e.connIPs))
	for ip, c := range e.connIPs {
		result = append(result, IPCount{
			IP:          ip,
			Count:       c,
			ActiveCount: e.connActiveIPs[ip],
		})
	}
	e.connIPMu.Unlock()
	sort.Slice(result, func(i, j int) bool {
		if result[i].ActiveCount != result[j].ActiveCount {
			return result[i].ActiveCount > result[j].ActiveCount
		}
		return result[i].IP < result[j].IP
	})
	return result
}

// DesiredRule 用于 SyncForVM 全量同步时描述期望状态
type DesiredRule struct {
	Protocol    string
	HostPort    int
	GuestPort   int
	Description string
	BindFamily  int
}

type Manager struct {
	mu       sync.RWMutex
	mappings map[string][]*Entry
	mgr      manager.VMManager
	db       *db.DB
	cfg      *config.Manager

	// 唯一来源 IP 数限制：ipLimits 缓存各 VM 的覆盖值（DB 为准），
	// vmActiveIPs 跟踪各 VM 当前活跃的来源 IP 及其连接数。
	ipLimitMu   sync.RWMutex
	ipLimits    map[string]int
	vmIPMu      sync.Mutex
	vmActiveIPs map[string]map[string]int64
}

func New(mgr manager.VMManager, database *db.DB, cfg *config.Manager) *Manager {
	m := &Manager{
		mappings:    make(map[string][]*Entry),
		mgr:         mgr,
		db:          database,
		cfg:         cfg,
		ipLimits:    make(map[string]int),
		vmActiveIPs: make(map[string]map[string]int64),
	}
	if limits, err := database.ListVMIPLimits(); err == nil {
		for _, l := range limits {
			m.ipLimits[l.VMID] = l.MaxIPs
		}
	}
	return m
}

// EffectiveIPLimit 返回某 VM 生效的唯一来源 IP 数上限（0 表示不限制）。
func (m *Manager) EffectiveIPLimit(vmID string) int {
	m.ipLimitMu.RLock()
	v, ok := m.ipLimits[vmID]
	m.ipLimitMu.RUnlock()
	if ok {
		return v
	}
	return int(m.cfg.Get().MaxForwardIPs)
}

// GetVMIPLimit 返回某 VM 的覆盖值（nil 表示未覆盖，使用全局）。
func (m *Manager) GetVMIPLimit(vmID string) *int {
	m.ipLimitMu.RLock()
	defer m.ipLimitMu.RUnlock()
	if v, ok := m.ipLimits[vmID]; ok {
		return &v
	}
	return nil
}

// SetVMIPLimit 设置某 VM 的覆盖值；limit 为 nil 时清除覆盖（回退到全局配置）。
func (m *Manager) SetVMIPLimit(vmID string, limit *int) error {
	if limit == nil {
		if err := m.db.DeleteVMIPLimit(vmID); err != nil {
			return err
		}
		m.ipLimitMu.Lock()
		delete(m.ipLimits, vmID)
		m.ipLimitMu.Unlock()
		return nil
	}
	if *limit < 0 {
		return fmt.Errorf("ip limit must be >= 0")
	}
	if err := m.db.SaveVMIPLimit(vmID, *limit); err != nil {
		return err
	}
	m.ipLimitMu.Lock()
	m.ipLimits[vmID] = *limit
	m.ipLimitMu.Unlock()
	return nil
}

// ActiveIPCount 返回某 VM 当前活跃的唯一来源 IP 数。
func (m *Manager) ActiveIPCount(vmID string) int {
	m.vmIPMu.Lock()
	defer m.vmIPMu.Unlock()
	return len(m.vmActiveIPs[vmID])
}

// tryAcquireIP 尝试为一条新连接登记来源 IP。
// 已在活跃集合中的 IP 总是放行（一个 IP 允许多条连接）；
// 新 IP 在唯一 IP 数达到上限时被拒绝。返回 false 表示应拒绝连接。
func (m *Manager) tryAcquireIP(vmID, ip string) bool {
	limit := m.EffectiveIPLimit(vmID)
	m.vmIPMu.Lock()
	defer m.vmIPMu.Unlock()
	set := m.vmActiveIPs[vmID]
	if set == nil {
		set = make(map[string]int64)
		m.vmActiveIPs[vmID] = set
	}
	if _, exists := set[ip]; !exists && limit > 0 && len(set) >= limit {
		return false
	}
	set[ip]++
	return true
}

// releaseIP 在连接结束时释放来源 IP 的占用。
func (m *Manager) releaseIP(vmID, ip string) {
	m.vmIPMu.Lock()
	defer m.vmIPMu.Unlock()
	set := m.vmActiveIPs[vmID]
	if set == nil {
		return
	}
	if set[ip] > 1 {
		set[ip]--
	} else {
		delete(set, ip)
		if len(set) == 0 {
			delete(m.vmActiveIPs, vmID)
		}
	}
}

// Restore 启动时从 DB 恢复所有持久化的端口转发规则。
// 母鸡重启后 agent 可能先于容器就绪启动，此时 GetVMLocalIP 会失败；
// 后台协程持续对齐 DB 与内存中的规则，容器起来后自动恢复转发。
func (m *Manager) Restore(ctx context.Context) {
	all, err := m.db.ListAllPortForwards()
	if err != nil {
		log.Printf("[PortForward] restore: list rules failed: %v", err)
	}
	for _, pf := range all {
		if err := m.addMapping(ctx, pf.VMID, pf.Protocol, pf.HostPort, pf.GuestPort, pf.Description, pf.BindFamily, false); err != nil {
			log.Printf("[PortForward] restore %s %s :%d->%d failed (will retry): %v", pf.VMID, pf.Protocol, pf.HostPort, pf.GuestPort, err)
		}
	}
	go m.reconcileLoop(ctx)
}

// reconcileLoop 周期性检查 DB 中登记但内存中缺失的规则并尝试重建。
// 覆盖场景：重启时容器尚未就绪、VM 停机后再次启动、RefreshVM 期间重建失败。
//
// 同时检查已存在规则的目标地址是否过期：容器若换了 IP（网络重建、IPAM 重排、
// 手工重连），targetAddr 会一直指向旧地址，连接全部失败，而 hasMapping 仍为
// true 使 reconcile 永远不会重建它。
func (m *Manager) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	failCount := make(map[string]int) // key: protocol/hostPort，用于降低重复失败的日志频率
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		all, err := m.db.ListAllPortForwards()
		if err != nil {
			continue
		}
		active := make(map[string]bool)
		staleVMs := make(map[string]bool) // 目标地址过期的 VM，循环结束后统一重建
		for _, pf := range all {
			key := fmt.Sprintf("%s/%d", pf.Protocol, pf.HostPort)
			active[key] = true
			if m.hasMapping(pf.VMID, pf.Protocol, pf.HostPort) {
				if _, stale := m.staleTarget(ctx, pf.VMID, pf.Protocol, pf.HostPort, pf.GuestPort); stale {
					staleVMs[pf.VMID] = true
				}
				delete(failCount, key)
				continue
			}
			if err := m.addMapping(ctx, pf.VMID, pf.Protocol, pf.HostPort, pf.GuestPort, pf.Description, pf.BindFamily, false); err != nil {
				failCount[key]++
				if failCount[key] == 1 || failCount[key]%20 == 0 {
					log.Printf("[PortForward] reconcile %s %s :%d->%d failed %d time(s): %v", pf.VMID, pf.Protocol, pf.HostPort, pf.GuestPort, failCount[key], err)
				}
			} else {
				log.Printf("[PortForward] reconcile: recovered %s %s :%d->%d", pf.VMID, pf.Protocol, pf.HostPort, pf.GuestPort)
				delete(failCount, key)
			}
		}
		for vmID := range staleVMs {
			log.Printf("[PortForward] target address changed for VM %s, rebinding all its rules", vmID)
			m.RefreshVM(ctx, vmID)
		}
		for key := range failCount {
			if !active[key] {
				delete(failCount, key)
			}
		}
	}
}

// staleTarget 判断某条已有规则的目标地址是否与容器当前 IP 不一致。
// 第二个返回值表示"能否判定"：容器 IP 取不到（停机/查询失败）时无法判断，
// 此时不处理，避免把正常规则误判为过期。
func (m *Manager) staleTarget(ctx context.Context, vmID, protocol string, hostPort, guestPort int) (string, bool) {
	m.mu.RLock()
	cur := ""
	bindFamily := BindV4
	var binds []listenSpec
	for _, e := range m.mappings[vmID] {
		if e.Protocol == protocol && e.HostPort == hostPort {
			cur = e.TargetAddr
			bindFamily = e.BindFamily
			binds = append(binds, e.binds...)
			break
		}
	}
	m.mu.RUnlock()
	if cur == "" {
		return "", false
	}
	if ips, err := m.bindAddresses(bindFamily); err != nil {
		return "configured bind address unavailable: " + err.Error(), true
	} else if !sameListenSpecs(binds, makeListenSpecs(protocol, ips)) {
		return "configured bind addresses changed", true
	}
	ip, err := m.mgr.GetVMLocalIP(ctx, vmID)
	if err != nil || ip == "" {
		return "", false
	}
	want := formatTargetAddress(ip, guestPort)
	if want == cur {
		return "", false
	}
	return cur + " -> " + want, true
}

func (m *Manager) hasMapping(vmID, protocol string, hostPort int) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, e := range m.mappings[vmID] {
		if e.Protocol == protocol && e.HostPort == hostPort {
			return true
		}
	}
	return false
}

// RefreshVM 重新解析 VM 的 IP 并重建其所有端口转发规则。
// 用于 VM 重装后 IP 可能发生变化的场景。
func (m *Manager) RefreshVM(ctx context.Context, vmID string) error {
	rules, err := m.db.ListPortForwards(vmID)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rules {
		if err := m.addMapping(ctx, vmID, r.Protocol, r.HostPort, r.GuestPort, r.Description, r.BindFamily, false); err != nil {
			log.Printf("[PortForward] refresh %s %s :%d->%d failed (reconcile will retry): %v", vmID, r.Protocol, r.HostPort, r.GuestPort, err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ReloadAll 按当前配置重建所有监听，用于切换对外 IP。
// 同时重试数据库中尚未成功监听的规则，并向调用方返回实际应用结果。
func (m *Manager) ReloadAll(ctx context.Context) error {
	rules, err := m.db.ListAllPortForwards()
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range rules {
		if err := m.addMapping(ctx, r.VMID, r.Protocol, r.HostPort, r.GuestPort, r.Description, r.BindFamily, false); err != nil {
			errs = append(errs, fmt.Errorf("%s %s :%d: %w", r.VMID, r.Protocol, r.HostPort, err))
		}
	}
	conf := m.cfg.Get()
	if err := errors.Join(errs...); err != nil {
		log.Printf("[PortForward] rebind failed (reconcile will retry): %v", err)
		return err
	}
	log.Printf("[PortForward] applied %d rules to public IPv4 %q IPv6 %q", len(rules), conf.EgressIPv4(), conf.EgressIPv6())
	return nil
}

// listenAddress 返回监听地址。bindIP 为空时监听所有本地地址（旧行为），
// 否则只监听该地址，容器对外仅在这一个 IP 上可达。
func listenAddress(bindIP string, port int) string {
	if bindIP == "" {
		return fmt.Sprintf(":%d", port)
	}
	// JoinHostPort 而不是字符串拼接：IPv6 地址需要 [ ] 包裹。
	return net.JoinHostPort(bindIP, strconv.Itoa(port))
}

// AddMapping 添加转发规则；替换失败时恢复旧监听，旧数据库记录保留到替换成功。
// bindFamily 取值见 BindV4 / BindV6 / BindBoth。
func (m *Manager) AddMapping(ctx context.Context, vmId string, protocol string, hostPort, guestPort int, description string, bindFamily int) error {
	return m.addMapping(ctx, vmId, protocol, hostPort, guestPort, description, bindFamily, true)
}

func (m *Manager) addMapping(ctx context.Context, vmId string, protocol string, hostPort, guestPort int, description string, bindFamily int, checkLimit bool) error {
	if protocol != "tcp" && protocol != "udp" {
		return fmt.Errorf("invalid protocol %q", protocol)
	}
	if guestPort < 1 || guestPort > MaxPort {
		return fmt.Errorf("invalid guest port %d", guestPort)
	}
	if hostPort < MinPort || hostPort > MaxPort {
		return fmt.Errorf("host port %d out of allowed range [%d, %d]", hostPort, MinPort, MaxPort)
	}
	if !ValidBindFamily(bindFamily) {
		return fmt.Errorf("invalid bind family %d", bindFamily)
	}

	// 先解析目标地址和监听地址：这两步都可能失败（容器未就绪、共享 IPv6 未配置）。
	// 必须在删除旧规则之前完成——否则一次失败的操作会把已有映射连同 DB 记录一起删掉。
	ip, err := m.mgr.GetVMLocalIP(ctx, vmId)
	if err != nil {
		return err
	}
	if ip == "" {
		return fmt.Errorf("VM %s has no IP address (is it running?)", vmId)
	}
	targetAddr := formatTargetAddress(ip, guestPort)

	bindIPs, err := m.bindAddresses(bindFamily)
	if err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	binds := makeListenSpecs(protocol, bindIPs)
	var old *Entry
	oldIndex := -1
	for i, e := range m.mappings[vmId] {
		if e.Protocol == protocol && e.HostPort == hostPort {
			if e.GuestPort == guestPort && e.Description == description && e.BindFamily == bindFamily && e.TargetAddr == targetAddr && sameListenSpecs(e.binds, binds) {
				return nil
			}
			old, oldIndex = e, i
			break
		}
	}

	if checkLimit && old == nil {
		maxPF := int(m.cfg.Get().MaxPortForward)
		if len(m.mappings[vmId]) >= maxPF {
			return fmt.Errorf("port forward limit reached (%d/%d)", len(m.mappings[vmId]), maxPF)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	entry := &Entry{
		VMID:          vmId,
		Protocol:      protocol,
		HostPort:      hostPort,
		GuestPort:     guestPort,
		TargetAddr:    targetAddr,
		Description:   description,
		BindFamily:    bindFamily,
		binds:         binds,
		connIPs:       make(map[string]int64),
		connActiveIPs: make(map[string]int64),
	}

	if old != nil {
		old.cancel()
		closeAll(old.lns)
	}
	restoreOld := func(cause error) error {
		if old == nil {
			return cause
		}
		if err := m.startEntry(old); err != nil {
			// Preserve its DB row so reconciliation can retry after a bind conflict.
			m.mappings[vmId] = append(m.mappings[vmId][:oldIndex], m.mappings[vmId][oldIndex+1:]...)
			return errors.Join(cause, fmt.Errorf("restore old mapping (DB rule retained): %w", err))
		}
		return cause
	}
	if err := m.startEntry(entry); err != nil {
		return restoreOld(err)
	}
	if err := m.db.SavePortForward(&db.PortForward{
		Protocol:    protocol,
		HostPort:    hostPort,
		VMID:        vmId,
		GuestPort:   guestPort,
		Description: description,
		BindFamily:  bindFamily,
	}); err != nil {
		entry.cancel()
		closeAll(entry.lns)
		return restoreOld(fmt.Errorf("save port forward: %w", err))
	}
	if old != nil {
		m.mappings[vmId][oldIndex] = entry
	} else {
		m.mappings[vmId] = append(m.mappings[vmId], entry)
	}
	return nil
}

type listenSpec struct{ network, ip string }

func makeListenSpecs(protocol string, ips []string) []listenSpec {
	out := make([]listenSpec, 0, len(ips))
	for _, ip := range ips {
		network := protocol
		// Keep the historical wildcard behavior of bind_family=0 with an empty
		// public IPv4. Explicit IPv4 and all new dual-stack sockets are family-specific.
		if ip != "" {
			if parsed := net.ParseIP(ip); parsed != nil && parsed.To4() == nil {
				network += "6"
			} else {
				network += "4"
			}
		}
		out = append(out, listenSpec{network: network, ip: ip})
	}
	return out
}

func sameListenSpecs(a, b []listenSpec) bool {
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

// Bind every socket before starting forwarding. No partial rule is published.
func (m *Manager) startEntry(e *Entry) error {
	if len(e.binds) == 0 {
		return fmt.Errorf("no bind address for family %d", e.BindFamily)
	}
	var sockets []io.Closer
	for _, bind := range e.binds {
		var socket io.Closer
		var err error
		if e.Protocol == "tcp" {
			socket, err = net.Listen(bind.network, listenAddress(bind.ip, e.HostPort))
		} else {
			socket, err = net.ListenPacket(bind.network, listenAddress(bind.ip, e.HostPort))
		}
		if err != nil {
			closeAll(sockets)
			return err
		}
		sockets = append(sockets, socket)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	e.cancel, e.lns = cancel, sockets
	for _, socket := range sockets {
		if e.Protocol == "tcp" {
			go m.runTCP(runCtx, socket.(net.Listener), e.TargetAddr, e)
		} else {
			go m.runUDP(runCtx, socket.(net.PacketConn), e.TargetAddr, e)
		}
	}
	return nil
}

// bindAddresses 返回该地址族对应的监听地址列表。
// v4 沿用旧行为：对外 IPv4 为空时监听所有本地地址（":port"）。
// 双栈时 v4 显式绑 0.0.0.0，并由 makeListenSpecs 使用 tcp4/udp4。
func (m *Manager) bindAddresses(bindFamily int) ([]string, error) {
	conf := m.cfg.Get()
	var out []string
	if bindFamily == BindV4 || bindFamily == BindBoth {
		ip := conf.EgressIPv4()
		if ip == "" && bindFamily == BindBoth {
			ip = "0.0.0.0"
		}
		out = append(out, ip)
	}
	if bindFamily == BindV6 || bindFamily == BindBoth {
		ip := conf.EgressIPv6()
		if ip == "" {
			return nil, fmt.Errorf("shared IPv6 address is not configured")
		}
		out = append(out, ip)
	}
	return out, nil
}

func closeAll(closers []io.Closer) {
	for _, c := range closers {
		_ = c.Close()
	}
}

func formatTargetAddress(ip string, port int) string {
	return net.JoinHostPort(ip, strconv.Itoa(port))
}

// removeEntryLocked removes the entry matching (vmId, protocol, hostPort) from
// the in-memory map, and from the database when deleteDB is true.
// Caller must hold m.mu (write lock).
func (m *Manager) removeEntryLocked(vmId, protocol string, hostPort int, deleteDB bool) {
	entries := m.mappings[vmId]
	for i, e := range entries {
		if e.Protocol == protocol && e.HostPort == hostPort {
			e.cancel()
			closeAll(e.lns)
			m.mappings[vmId] = append(entries[:i], entries[i+1:]...)
			if deleteDB {
				_ = m.db.DeletePortForward(protocol, hostPort)
			}
			return
		}
	}
}

func (m *Manager) RemoveMapping(_ context.Context, vmId string, protocol string, hostPort int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeEntryLocked(vmId, protocol, hostPort, true)
	return nil
}

// SyncForVM 以 desired 为准全量对齐某个 VM 的端口转发规则
func (m *Manager) SyncForVM(ctx context.Context, vmId string, desired []DesiredRule) error {
	m.mu.RLock()
	current := make([]*Entry, len(m.mappings[vmId]))
	copy(current, m.mappings[vmId])
	m.mu.RUnlock()

	// 删除不在期望列表中的规则
	for _, c := range current {
		found := false
		for _, d := range desired {
			if c.Protocol == d.Protocol && c.HostPort == d.HostPort {
				found = true
				break
			}
		}
		if !found {
			_ = m.RemoveMapping(ctx, vmId, c.Protocol, c.HostPort)
		}
	}

	// 添加或更新期望的规则
	for _, d := range desired {
		_ = m.AddMapping(ctx, vmId, d.Protocol, d.HostPort, d.GuestPort, d.Description, d.BindFamily)
	}

	return nil
}

// DeleteVM 删除某 VM 的所有转发规则（内存 + DB）
func (m *Manager) DeleteVM(ctx context.Context, vmId string) {
	m.mu.RLock()
	entries := make([]*Entry, len(m.mappings[vmId]))
	copy(entries, m.mappings[vmId])
	m.mu.RUnlock()
	for _, e := range entries {
		_ = m.RemoveMapping(ctx, vmId, e.Protocol, e.HostPort)
	}
	_ = m.db.DeletePortForwardsForVM(vmId)
	_ = m.db.DeleteVMIPLimit(vmId)
	m.ipLimitMu.Lock()
	delete(m.ipLimits, vmId)
	m.ipLimitMu.Unlock()
}

// ForwardPorts 返回当前所有端口转发规则的宿主端口，按协议分组（去重、升序）。
// 供 hostfw 把 NAT 映射端口纳入容器 INPUT 放行集合：容器做代理时回环访问
// 宿主公网 IP 上的转发端口，即使不在 host_allow_ports 里也应放行。
func (m *Manager) ForwardPorts() map[string][]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	seen := make(map[string]map[int]bool)
	for _, entries := range m.mappings {
		for _, e := range entries {
			if seen[e.Protocol] == nil {
				seen[e.Protocol] = make(map[int]bool)
			}
			seen[e.Protocol][e.HostPort] = true
		}
	}
	out := make(map[string][]int, len(seen))
	for proto, set := range seen {
		list := make([]int, 0, len(set))
		for p := range set {
			list = append(list, p)
		}
		sort.Ints(list)
		out[proto] = list
	}
	return out
}

func (m *Manager) GetReport() []Entry {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var all []Entry
	for _, entries := range m.mappings {
		for _, e := range entries {
			all = append(all, Entry{
				VMID:        e.VMID,
				Protocol:    e.Protocol,
				HostPort:    e.HostPort,
				GuestPort:   e.GuestPort,
				TargetAddr:  e.TargetAddr,
				Description: e.Description,
				BindFamily:  e.BindFamily,
				ActiveConns: atomic.LoadInt64(&e.connActive),
				TotalConns:  atomic.LoadInt64(&e.connTotal),
				TopIPs:      e.getTopIPs(),
			})
		}
	}
	return all
}

// --- 内部转发逻辑 ---

func (m *Manager) runTCP(ctx context.Context, ln net.Listener, target string, e *Entry) {
	defer func() { _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				time.Sleep(time.Second)
				continue
			}
		}
		go m.handleTCP(ctx, conn, target, e)
	}
}

func (m *Manager) handleTCP(ctx context.Context, src net.Conn, target string, e *Entry) {
	defer func() { _ = src.Close() }()

	host, _, _ := net.SplitHostPort(src.RemoteAddr().String())
	if !m.tryAcquireIP(e.VMID, host) {
		// 唯一来源 IP 数已达上限，拒绝新 IP 的连接
		return
	}
	defer m.releaseIP(e.VMID, host)
	e.trackConnect(host)
	defer e.trackDisconnect(host)

	dst, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		return
	}
	defer func() { _ = dst.Close() }()

	done := make(chan struct{}, 2)
	go func() {
		m.proxy(dst, src)
		done <- struct{}{}
	}()
	go func() {
		m.proxy(src, dst)
		done <- struct{}{}
	}()

	select {
	case <-ctx.Done():
	case <-done:
	}
}

func (m *Manager) runUDP(ctx context.Context, pc net.PacketConn, target string, e *Entry) {
	defer func() { _ = pc.Close() }()
	targetAddr, _ := net.ResolveUDPAddr("udp", target)
	sessions := make(map[string]net.Conn)
	var smu sync.Mutex

	buf := make([]byte, 64*1024)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}

		smu.Lock()
		client, ok := sessions[addr.String()]
		if !ok {
			host, _, _ := net.SplitHostPort(addr.String())
			if !m.tryAcquireIP(e.VMID, host) {
				// 唯一来源 IP 数已达上限，丢弃新 IP 的数据包
				smu.Unlock()
				continue
			}
			var dialErr error
			client, dialErr = net.Dial("udp", targetAddr.String())
			if dialErr != nil {
				m.releaseIP(e.VMID, host)
				smu.Unlock()
				continue
			}
			sessions[addr.String()] = client
			e.trackConnect(host)
			go func(a net.Addr, c net.Conn, h string) {
				defer func() {
					smu.Lock()
					delete(sessions, a.String())
					smu.Unlock()
					_ = c.Close()
					e.trackDisconnect(h)
					m.releaseIP(e.VMID, h)
				}()
				rBuf := make([]byte, 64*1024)
				for {
					_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
					rn, err := c.Read(rBuf)
					if err != nil {
						return
					}
					_, _ = pc.WriteTo(rBuf[:rn], a)
				}
			}(addr, client, host)
		}
		smu.Unlock()

		_, _ = client.Write(buf[:n])
	}
}

func (m *Manager) proxy(dst io.Writer, src io.Reader) {
	_, _ = io.Copy(dst, src)
}
