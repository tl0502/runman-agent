package patrol

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"runman-agent/config"
	"runman-agent/manager"
	"runman-agent/proto/agent"
)

// connCount 是某个 IP 的活跃连接计数（in=外部主动连入，out=该 IP 主动连出）。
// 滥用识别追加的画像字段只在 watch 集合（容器 IP）上累计，避免为全表外部 IP
// 分配无用结构。
type ipConnStats struct {
	in, out int // 活跃连接：TCP ESTABLISHED / UDP ASSURED（与旧口径一致）
	// udpIn/udpOut 是 UDP 条目数（含 UNREPLIED，分方向）。单向海量 UDP 是
	// 发包攻击的硬特征——现有活跃口径（ASSURED）对单向洪水不可见。
	udpIn, udpOut int
	// smtpOut 是出方向、目标端口命中黑名单的 TCP 条目数（不看状态，
	// 群发邮件的连接大多短命，活跃口径下同样不可见）。
	smtpOut int
	// distinct 是出方向出现过的不同目标 TCP IP（不看状态，扫描器的大多数
	// 握手完不成）。饱和封顶：数量达到 scanCap 后不再收集，内存有界。
	distinct map[string]struct{}
}

// countAllConnections 一次性解析 /proc/net/nf_conntrack，返回 IP → 连接画像。
// watch 为容器 IP 集合（nil = 全部 IP 都做完整画像）；scanCap 是不同目标数的
// 饱和上限（<=0 不收集）；smtpPorts 是发信端口黑名单（空则不统计）。
func countAllConnections(watch map[string]bool, scanCap int, smtpPorts map[int]bool) (map[string]*ipConnStats, error) {
	data, err := os.ReadFile("/proc/net/nf_conntrack")
	if err != nil {
		data, err = os.ReadFile("/proc/net/ip_conntrack") // 旧内核路径
		if err != nil {
			return nil, err
		}
	}
	return countConnections(string(data), watch, scanCap, smtpPorts), nil
}

// countConnections 解析 conntrack 文本，统计每个 IP 的连接画像。
// 活跃判定：TCP 状态 ESTABLISHED、UDP 标记 [ASSURED]（双向都见过）。
// 滥用画像（udp/smtp/distinct）不看活跃状态——滥用条目天生不活跃。
func countConnections(data string, watch map[string]bool, scanCap int, smtpPorts map[int]bool) map[string]*ipConnStats {
	m := map[string]*ipConnStats{}
	for _, line := range strings.Split(data, "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		proto := fields[2]
		src := normalizeIP(firstKV(line, "src="))
		dst := normalizeIP(firstKV(line, "dst="))
		if src == "" || dst == "" || src == dst {
			continue
		}
		c := m[src]
		if c == nil {
			c = &ipConnStats{}
			m[src] = c
		}
		d := m[dst]
		if d == nil {
			d = &ipConnStats{}
			m[dst] = d
		}
		if isActiveConn(line, proto) {
			c.out++
			d.in++
		}
		if proto == "udp" {
			c.udpOut++
			d.udpIn++
			continue // UDP 不参与扫描/发信画像
		}
		if proto != "tcp" || (watch != nil && !watch[src]) {
			continue
		}
		// 以下为出方向 TCP 的滥用画像（仅容器 IP）
		if len(smtpPorts) > 0 {
			if port, err := strconv.Atoi(firstKV(line, "dport=")); err == nil && smtpPorts[port] {
				c.smtpOut++
			}
		}
		if scanCap > 0 {
			if c.distinct == nil {
				c.distinct = make(map[string]struct{}, 8)
			}
			if len(c.distinct) < scanCap {
				c.distinct[dst] = struct{}{}
			}
		}
	}
	return m
}

// isActiveConn 判断一条 conntrack 记录是否为「活跃连接」。
// TCP 条目带状态字段（ESTABLISHED/TIME_WAIT/...），只认 ESTABLISHED；
// UDP 条目无状态字段，靠 [ASSURED]（[UNREPLIED] 是单向未应答，不算活跃）。
// 注意：TCP 条目的 reply 元组也会带 [ASSURED]，不能用它判断 TCP 是否活跃。
func isActiveConn(line, proto string) bool {
	switch proto {
	case "tcp":
		return strings.Contains(line, "ESTABLISHED")
	case "udp":
		return strings.Contains(line, "[ASSURED]")
	}
	return false
}

// firstKV 返回 line 中 key（如 "src="）首次出现后的 token（到下一个空格为止）。
func firstKV(line, key string) string {
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// normalizeIP 把 IPv6 规范成压缩格式，保证与 conntrack 文本里的地址可比。
// 解析失败或 IPv4 原样返回。
func normalizeIP(s string) string {
	if !strings.Contains(s, ":") {
		return s
	}
	if a, err := netip.ParseAddr(s); err == nil {
		return a.String()
	}
	return s
}

// vmIPs 返回 VM 的全部可用地址（v4 + v6），归一化后去重。
// 巡查按地址聚合：共享 IPv6 下容器的 v6 出站条目挂在 ULA 地址上，
// 只查 v4 会漏掉这部分流量。
func vmIPs(vm *agent.VMSummary) []string {
	seen := make(map[string]bool, len(vm.Ips))
	out := make([]string, 0, len(vm.Ips))
	for _, ip := range vm.Ips {
		if net.ParseIP(ip) == nil {
			continue
		}
		n := normalizeIP(ip)
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// sumConnStats 汇总一个 VM 所有地址的连接画像。
// distinct 取并集：扫描判定只看不同目标数是否达阈值，无需保留每地址独立集合。
func sumConnStats(connMap map[string]*ipConnStats, ips []string) *ipConnStats {
	out := &ipConnStats{}
	for _, ip := range ips {
		cs := connMap[ip]
		if cs == nil {
			continue
		}
		out.in += cs.in
		out.out += cs.out
		out.udpIn += cs.udpIn
		out.udpOut += cs.udpOut
		out.smtpOut += cs.smtpOut
		for d := range cs.distinct {
			if out.distinct == nil {
				out.distinct = make(map[string]struct{}, len(cs.distinct))
			}
			out.distinct[d] = struct{}{}
		}
	}
	return out
}

// checkBandwidth 单容器带宽占用巡查：占用 ≥ 上限 thresholdPct% 持续 durationMin 分钟 → 限速。
func (s *Service) checkBandwidth(conf config.Config, vm *agent.VMSummary, st *vmState, now time.Time) {
	bc := conf.Patrol.Bandwidth
	if bc.Enabled != nil && !*bc.Enabled {
		return
	}
	if s.isThrottled(vm.VmId) {
		return // 已限速，等 reconcile 到期恢复
	}
	vmCfg, err := s.db.GetVMConfig(vm.VmId)
	if err != nil || vmCfg == nil || vmCfg.BandwidthMbps <= 0 {
		return // 未分配带宽上限，跳过
	}

	var rateBps int64
	if !st.lastNetAt.IsZero() {
		elapsed := now.Sub(st.lastNetAt).Seconds()
		if elapsed >= 1 {
			dIn := vm.TrafficInBytes - st.lastIn
			dOut := vm.TrafficOutBytes - st.lastOut
			if dIn >= 0 && dOut >= 0 {
				// dIn/dOut 是字节，capBit/threshold 是 bit/s：必须 ×8 统一成 bit/s。
				// 旧实现直接拿 bytes/s 比 bit/s，实际触发门槛被放大 8 倍（灌 6Mbps
				// 过不了 1Mbps 的阈值），带宽巡查形同虚设。
				rateBps = int64(float64(dIn+dOut) * 8 / elapsed)
			}
		}
	}
	st.lastIn, st.lastOut, st.lastNetAt = vm.TrafficInBytes, vm.TrafficOutBytes, now
	if rateBps <= 0 {
		// 零流量或计数器重置：连续超限已中断，不能沿用旧起点。
		st.bwOverSince = time.Time{}
		return
	}

	// capBit 是分配上限的 bit/s 数值（与 tc "rate <n>bit"、netavark bandwidth_rate 一致）。
	capBit := int64(vmCfg.BandwidthMbps) * 1000000
	threshold := capBit * int64(bc.ThresholdPct) / 100
	if rateBps >= threshold {
		if st.bwOverSince.IsZero() {
			st.bwOverSince = now
		}
		if now.Sub(st.bwOverSince) >= time.Duration(bc.DurationMin)*time.Minute {
			st.bwOverSince = time.Time{}
			mbps := bc.ThrottleMbps
			release := now.Add(time.Duration(bc.ReleaseMin) * time.Minute)
			applied, noEffect := s.applyThrottle(vm.VmId, mbps, release, "bandwidth")
			detail := fmt.Sprintf("带宽占用 %d%% 超阈值 %d%% 持续 %d 分钟，限速到 %d Mbps（%d 分钟后恢复）",
				rateBps*100/capBit, bc.ThresholdPct, bc.DurationMin, mbps, bc.ReleaseMin)
			switch {
			case noEffect:
				detail += "；限速值不低于该 VM 的带宽上限，未施加"
			case !applied:
				detail += "；tc 暂未生效（VM 未运行或网卡未就绪），下一轮巡查会重放"
			}
			s.emit(vm.VmId, "bandwidth", "throttle", detail, true)
		}
	} else {
		st.bwOverSince = time.Time{}
	}
}

// checkConnections 活跃连接数巡查：in≥inLimit 或 out≥outLimit 持续 durationMin 分钟 → 停止 VM + 限速。
// connMap 为 nil 表示 conntrack 读取失败（本轮失明）：持续判定清零，下次见到数据重新累计。
func (s *Service) checkConnections(conf config.Config, vm *agent.VMSummary, connMap map[string]*ipConnStats, st *vmState, now time.Time) {
	cc := conf.Patrol.Connections
	if cc.Enabled != nil && !*cc.Enabled {
		return
	}
	if s.isThrottled(vm.VmId) {
		return
	}
	ips := vmIPs(vm)
	if len(ips) == 0 {
		return
	}
	cs := sumConnStats(connMap, ips)
	in, out := cs.in, cs.out
	if in >= cc.InLimit || out >= cc.OutLimit {
		if st.connOverSince.IsZero() {
			st.connOverSince = now
		}
		if now.Sub(st.connOverSince) >= time.Duration(cc.DurationMin)*time.Minute {
			st.connOverSince = time.Time{}
			if err := s.mgr.StopVM(context.Background(), vm.VmId, false); err != nil {
				log.Printf("[Patrol] stop VM %s: %v", vm.VmId, err)
				return
			}
			st.stopped = true
			_ = s.db.SavePatrolStop(vm.VmId, "connections")
			mbps := cc.ThrottleMbps
			release := now.Add(time.Duration(cc.ReleaseMin) * time.Minute)
			applied, noEffect := s.applyThrottle(vm.VmId, mbps, release, "connections")
			detail := fmt.Sprintf("活跃连接 in=%d/out=%d 超限（阈值 in≥%d/out≥%d，持续 %d 分钟），停止 VM 并限速到 %d Mbps（%d 分钟后解除）",
				in, out, cc.InLimit, cc.OutLimit, cc.DurationMin, mbps, cc.ReleaseMin)
			switch {
			case noEffect:
				detail += "；限速值不低于该 VM 的带宽上限，未施加"
			case !applied:
				detail += "；VM 已停止，限速将在重新启动后自动生效"
			}
			s.emit(vm.VmId, "connections", "stop_vm", detail, true)
		}
	} else {
		st.connOverSince = time.Time{}
	}
}

// checkResources VM 级 CPU/内存/磁盘巡查。CPU/内存按配额占比判定，超限后按 action 动作；磁盘只告警。
func (s *Service) checkResources(conf config.Config, vm *agent.VMSummary, st *vmState, now time.Time) {
	rc := conf.Patrol.Resources
	if rc.Enabled != nil && !*rc.Enabled {
		return
	}
	vmCfg, err := s.db.GetVMConfig(vm.VmId)
	if err != nil || vmCfg == nil {
		return
	}

	// CPU：vm.CpuPct 是「占 1 核的百分比」，配额为 vmCfg.CPU 核。
	if vmCfg.CPU > 0 {
		over := vm.CpuPct >= float32(vmCfg.CPU)*float32(rc.CpuPct)
		s.resourceGate(vm, st, now, "resources_cpu", over, &st.cpuOverSince, rc.CpuMin, rc.Action, rc.RestartCooldownMin,
			fmt.Sprintf("CPU 占用 %.0f%%（配额 %d 核）超阈值 %d%%", vm.CpuPct, vmCfg.CPU, rc.CpuPct))
	}
	// 内存
	if vmCfg.MemoryMB > 0 {
		over := vm.RamUsedMb*100 >= vmCfg.MemoryMB*int64(rc.MemPct)
		s.resourceGate(vm, st, now, "resources_mem", over, &st.memOverSince, rc.MemMin, rc.Action, rc.RestartCooldownMin,
			fmt.Sprintf("内存占用 %dMB/%dMB 超阈值 %d%%", vm.RamUsedMb, vmCfg.MemoryMB, rc.MemPct))
	}
	// 磁盘（无持续时长；只记面板日志、不发 Telegram，按间隔限频）
	s.checkDisk(vm, st, vmCfg.DiskGB, rc, now)
}

// resourceGate 处理「持续 durMin 分钟超阈值 → 动作（带冷却）」的通用判定。
func (s *Service) resourceGate(vm *agent.VMSummary, st *vmState, now time.Time, check string,
	over bool, overSince *time.Time, durMin int, action string, cooldownMin int, detail string) {
	if over {
		if overSince.IsZero() {
			*overSince = now
		}
		if now.Sub(*overSince) >= time.Duration(durMin)*time.Minute {
			*overSince = time.Time{}
			if now.Sub(st.lastActionAt) < time.Duration(cooldownMin)*time.Minute {
				return // 冷却中，本轮不动作
			}
			st.lastActionAt = now
			s.doResourceAction(vm.VmId, check, action, detail)
		}
	} else {
		*overSince = time.Time{}
	}
}

func (s *Service) doResourceAction(vmID, check, action, detail string) {
	switch action {
	case "restart":
		if err := s.mgr.RestartVM(context.Background(), vmID); err != nil {
			log.Printf("[Patrol] restart VM %s: %v", vmID, err)
			s.emit(vmID, check, "alert", detail+"，重启失败："+err.Error(), true)
			return
		}
		s.emit(vmID, check, "restart_vm", detail+"，已自动重启", true)
	case "stop":
		if err := s.mgr.StopVM(context.Background(), vmID, false); err != nil {
			log.Printf("[Patrol] stop VM %s: %v", vmID, err)
			s.emit(vmID, check, "alert", detail+"，关机失败："+err.Error(), true)
			return
		}
		_ = s.db.SavePatrolStop(vmID, "resources")
		s.emit(vmID, check, "stop_vm", detail+"，已关机", true)
	default: // alert
		s.emit(vmID, check, "alert", detail, true)
	}
}

// checkDisk 磁盘占用巡查：超阈值只记面板日志，不发 Telegram。
// 同一 VM 的告警按 DiskAlertIntervalMin 限频（默认 6 小时），
// 避免持续超限或在阈值附近抖动时反复刷日志。
func (s *Service) checkDisk(vm *agent.VMSummary, st *vmState, diskGB int64, rc config.PatrolResources, now time.Time) {
	if diskGB <= 0 {
		return
	}
	used, err := s.prober.GetVMDiskUsage(context.Background(), vm.VmId)
	if err != nil {
		return
	}
	limit := diskGB * 1024 * 1024 * 1024
	if used*100 < limit*int64(rc.DiskPct) {
		return
	}
	interval := time.Duration(rc.DiskAlertIntervalMin) * time.Minute
	if interval <= 0 {
		interval = 6 * time.Hour
	}
	if !st.lastDiskAlertAt.IsZero() && now.Sub(st.lastDiskAlertAt) < interval {
		return // 限频窗口内，已告警过
	}
	st.lastDiskAlertAt = now
	s.emit(vm.VmId, "resources_disk", "alert",
		fmt.Sprintf("磁盘占用 %.1fGB/%.1fGB（%d%%）超阈值 %d%%",
			float64(used)/1e9, float64(limit)/1e9, used*100/limit, rc.DiskPct), false)
}

// checkAbuse 滥用识别巡查：UDP 连接数 / 端口扫描 / SMTP 出方向端口。
// 三个子项共用「持续 DurationMin 分钟超限 → 按各自动作处置」的判定框架，
// 动作后计时清零——告警天然形成「每持续超限 N 分钟一次」的节奏，无需额外限频。
// connMap 为 nil（conntrack 读取失败）时本轮失明，持续判定清零。
func (s *Service) checkAbuse(conf config.Config, vm *agent.VMSummary, connMap map[string]*ipConnStats, st *vmState, now time.Time) {
	ac := conf.Patrol.Abuse
	if !ac.EnabledOn() {
		return
	}
	ips := vmIPs(vm)
	if len(ips) == 0 {
		return
	}
	cs := sumConnStats(connMap, ips)
	dur := time.Duration(ac.DurationMin) * time.Minute

	// UDP 发包/被打：任一方向条目数达到阈值（含 UNREPLIED，单向洪水的主要形态）
	overUDP := cs.udpOut >= ac.UdpConnLimit || cs.udpIn >= ac.UdpConnLimit
	s.abuseGate(vm, st, now, &st.udpOverSince, overUDP, dur, ac.UdpAction,
		fmt.Sprintf("UDP 连接 out=%d/in=%d 超阈值 %d", cs.udpOut, cs.udpIn, ac.UdpConnLimit),
		"abuse_udp", ac)

	// 端口扫描：出方向不同目标 IP 数（解析侧饱和封顶 = 阈值，达上限即超限）
	if ac.ScanDstLimit > 0 {
		overScan := len(cs.distinct) >= ac.ScanDstLimit
		s.abuseGate(vm, st, now, &st.scanOverSince, overScan, dur, ac.ScanAction,
			fmt.Sprintf("主动连接 %d 个不同目标 IP（≥%d）", len(cs.distinct), ac.ScanDstLimit),
			"abuse_scan", ac)
	}

	// SMTP 出方向：命中黑名单端口的 TCP 条目数
	if ac.SmtpLimit > 0 {
		overSmtp := cs.smtpOut >= ac.SmtpLimit
		s.abuseGate(vm, st, now, &st.smtpOverSince, overSmtp, dur, ac.SmtpAction,
			fmt.Sprintf("连出端口 %v 共 %d 次（阈值 %d）", ac.SmtpPorts, cs.smtpOut, ac.SmtpLimit),
			"abuse_smtp", ac)
	}
}

// abuseGate 滥用识别的通用判定：持续超限 dur 达到后按 action 处置。
func (s *Service) abuseGate(vm *agent.VMSummary, st *vmState, now time.Time, overSince *time.Time,
	over bool, dur time.Duration, action, detail, check string, ac config.PatrolAbuse) {
	if !over {
		*overSince = time.Time{}
		return
	}
	if overSince.IsZero() {
		*overSince = now
		return
	}
	if now.Sub(*overSince) < dur {
		return
	}
	*overSince = time.Time{}
	switch action {
	case "throttle":
		if s.isThrottled(vm.VmId) {
			return // 已在限速中，等 reconcile 到期恢复
		}
		mbps := ac.ThrottleMbps
		release := now.Add(time.Duration(ac.ReleaseMin) * time.Minute)
		applied, noEffect := s.applyThrottle(vm.VmId, mbps, release, check)
		d := detail + fmt.Sprintf("，限速到 %d Mbps（%d 分钟后恢复）", mbps, ac.ReleaseMin)
		switch {
		case noEffect:
			d += "；限速值不低于该 VM 的带宽上限，未施加"
		case !applied:
			d += "；tc 暂未生效，下一轮巡查会重放"
		}
		s.emit(vm.VmId, check, "throttle", d, true)
	case "stop":
		if err := s.mgr.StopVM(context.Background(), vm.VmId, false); err != nil {
			log.Printf("[Patrol] stop VM %s: %v", vm.VmId, err)
			s.emit(vm.VmId, check, "alert", detail+"，停机失败："+err.Error(), true)
			return
		}
		st.stopped = true
		_ = s.db.SavePatrolStop(vm.VmId, check)
		s.emit(vm.VmId, check, "stop_vm", detail+"，已停机", true)
	default: // alert
		s.emit(vm.VmId, check, "alert", detail, true)
	}
}

// killWindow 是连杀升级停机的统计窗口。
const killWindow = time.Hour

// checkProcesses 容器内进程黑名单巡查：命中即杀；容器主进程命中、或窗口内
// 命中次数达到 kill_stop_limit 时升级为停机——反复被杀说明有守护脚本拉起，
// 光杀进程治标不治本。单次命中只记面板日志，停机属于重大动作，照常推 Telegram。
func (s *Service) checkProcesses(conf config.Config, vm *agent.VMSummary, st *vmState) {
	pc := conf.Patrol.Processes
	if pc.Enabled != nil && !*pc.Enabled {
		return
	}
	if len(pc.Blacklist) == 0 {
		return
	}
	procs, err := s.prober.ListProcesses(context.Background(), vm.VmId)
	if err != nil {
		if st.procErrAt.IsZero() || time.Since(st.procErrAt) >= time.Hour {
			st.procErrAt = time.Now()
			log.Printf("[Patrol] list processes in %s failed: %v", vm.VmId, err)
		}
		return
	}
	now := time.Now()
	pruneKillTimes(st, now)
	for _, p := range procs {
		kw := matchBlacklist(pc.Blacklist, p)
		if kw == "" {
			continue
		}
		if p.Init {
			// 容器主进程就是黑名单本体：kill 它等于容器崩溃退出，没有停机记录、
			// 面板看不到原因。直接按停机处理；整个容器都是滥用，无需观察是否拉起。
			s.stopByProcess(vm.VmId, st, fmt.Sprintf("容器主进程命中黑名单 %q", kw))
			return
		}
		if err := s.prober.KillProcess(context.Background(), p); err != nil {
			log.Printf("[Patrol] kill process %d (%s) in %s: %v", p.PID, p.Comm, vm.VmId, err)
			continue
		}
		st.killTimes = append(st.killTimes, now)
		s.emit(vm.VmId, "process", "kill_process",
			fmt.Sprintf("命中黑名单 %q，已杀进程 %s (pid %d)", kw, p.Comm, p.PID), false)
		if lim := pc.KillStopLimit; lim > 0 && len(st.killTimes) >= lim {
			s.stopByProcess(vm.VmId, st, fmt.Sprintf("1 小时内命中黑名单 %d 次（关键词 %q），疑似被反复拉起",
				len(st.killTimes), kw))
			return
		}
	}
}

// stopByProcess 因进程黑名单升级停机：停容器 + 记录停机原因 + 推 Telegram。
// 停机后本容器的其他进程不必再杀，调用方直接返回即可。停机失败时按 10 分钟
// 限频告警：容器处于异常状态时每一轮都会失败，不限频会把 TG 刷爆。
func (s *Service) stopByProcess(vmID string, st *vmState, detail string) {
	if err := s.mgr.StopVM(context.Background(), vmID, false); err != nil {
		log.Printf("[Patrol] stop VM %s: %v", vmID, err)
		if st.lastStopAlertAt.IsZero() || time.Since(st.lastStopAlertAt) >= 10*time.Minute {
			st.lastStopAlertAt = time.Now()
			s.emit(vmID, "process", "alert", detail+"，停机失败："+err.Error(), true)
		}
		return
	}
	_ = s.db.SavePatrolStop(vmID, "process")
	st.killTimes = nil
	s.emit(vmID, "process", "stop_vm", detail+"，已停机", true)
}

// pruneKillTimes 丢弃 1 小时窗口外的命中记录，窗口随巡查周期滑动。
func pruneKillTimes(st *vmState, now time.Time) {
	cut := now.Add(-killWindow)
	i := 0
	for i < len(st.killTimes) && st.killTimes[i].Before(cut) {
		i++
	}
	if i > 0 {
		st.killTimes = st.killTimes[i:]
	}
}

// matchBlacklist 返回第一个命中的关键词，空串表示未命中。
func matchBlacklist(blacklist []string, p manager.Process) string {
	for _, kw := range blacklist {
		if kw != "" && matchKeyword(kw, p.Comm, p.Exe, p.Args) {
			return kw
		}
	}
	return ""
}

// matchKeyword 判断进程是否命中单个关键词（大小写不敏感）。
// 两层匹配，任一层命中即可：
//   - 名字层：comm 或可执行文件名与关键词完全相等；词尾 * 时按前缀匹配
//     （x-ui 不会命中 nginx-ui，deluge* 能覆盖 deluged）。
//   - 命令行层：关键词以整词形式出现，前后必须是非字母数字（串首尾也算）；
//     词尾 * 时放宽右边界。池地址、/tmp 路径这类片段靠这层抓。
func matchKeyword(kw, comm, exe, args string) bool {
	kw = strings.ToLower(strings.TrimSpace(kw))
	if kw == "" {
		return false
	}
	prefix := strings.HasSuffix(kw, "*")
	kw = strings.TrimSuffix(kw, "*")
	if kw == "" {
		return false
	}
	if nameMatch(kw, prefix, comm) || nameMatch(kw, prefix, exeBasename(exe)) {
		return true
	}
	return containsBoundary(strings.ToLower(args), kw, prefix)
}

func nameMatch(kw string, prefix bool, name string) bool {
	name = strings.ToLower(name)
	if name == "" {
		return false
	}
	if prefix {
		return strings.HasPrefix(name, kw)
	}
	return name == kw
}

// exeBasename 取可执行文件名（/proc/<pid>/exe 可能带 " (deleted)" 后缀，
// 归属 manager 已剥掉，这里只取路径最后一段）。
func exeBasename(exe string) string {
	if exe == "" {
		return ""
	}
	return filepath.Base(exe)
}

// containsBoundary 判断 kw 是否以整词形式出现在 s 中：出现位置的前后必须是
// 非字母数字（串首尾也算边界）。prefix 只放宽右侧——`deluge*` 要匹配 deluged。
func containsBoundary(s, kw string, prefix bool) bool {
	if s == "" || kw == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(s[i:], kw)
		if j < 0 {
			return false
		}
		j += i
		leftOK := j == 0 || !isAlnum(s[j-1])
		end := j + len(kw)
		rightOK := prefix || end == len(s) || !isAlnum(s[end])
		if leftOK && rightOK {
			return true
		}
		i = j + 1
	}
}

// isAlnum 是 ASCII 词边界判断：非 ASCII 字节（中文等）一律视为边界，
// 对进程名的实际取值足够，不引入 unicode 分类的开销。
func isAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}
