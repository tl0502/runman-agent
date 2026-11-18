package patrol

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"runman-agent/config"
	"runman-agent/proto/agent"
)

// connCount 是某个 IP 的活跃连接计数（in=外部主动连入，out=该 IP 主动连出）。
type connCount struct{ in, out int }

// countAllConnections 一次性解析 /proc/net/nf_conntrack，返回 IP → 活跃连接计数。
func countAllConnections() map[string]connCount {
	data, err := os.ReadFile("/proc/net/nf_conntrack")
	if err != nil {
		data, err = os.ReadFile("/proc/net/ip_conntrack") // 旧内核路径
		if err != nil {
			return map[string]connCount{}
		}
	}
	return countConnections(string(data))
}

// countConnections 解析 conntrack 文本，统计每个 IP 的活跃连接数。
// 活跃判定：TCP 状态 ESTABLISHED、UDP 标记 [ASSURED]（双向都见过）。
// 只统计原始方向（每条条目算一条连接）。
func countConnections(data string) map[string]connCount {
	m := map[string]connCount{}
	for _, line := range strings.Split(data, "\n") {
		if line == "" || !isActiveConn(line) {
			continue
		}
		src := firstKV(line, "src=")
		dst := firstKV(line, "dst=")
		if src == "" || dst == "" || src == dst {
			continue
		}
		c := m[src]
		c.out++
		m[src] = c
		d := m[dst]
		d.in++
		m[dst] = d
	}
	return m
}

// isActiveConn 判断一条 conntrack 记录是否为「活跃连接」。
// TCP 条目带状态字段（ESTABLISHED/TIME_WAIT/...），只认 ESTABLISHED；
// UDP 条目无状态字段，靠 [ASSURED]（[UNREPLIED] 是单向未应答，不算活跃）。
// 注意：TCP 条目的 reply 元组也会带 [ASSURED]，不能用它判断 TCP 是否活跃。
func isActiveConn(line string) bool {
	if strings.Contains(line, "tcp") {
		return strings.Contains(line, "ESTABLISHED")
	}
	if strings.Contains(line, "udp") {
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

func vmIPv4(vm *agent.VMSummary) string {
	for _, ip := range vm.Ips {
		if p := net.ParseIP(ip); p != nil && p.To4() != nil {
			return ip
		}
	}
	return ""
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
			applied := s.applyThrottle(vm.VmId, mbps, release, "bandwidth")
			detail := fmt.Sprintf("带宽占用 %d%% 超阈值 %d%% 持续 %d 分钟，限速到 %d Mbps（%d 分钟后恢复）",
				rateBps*100/capBit, bc.ThresholdPct, bc.DurationMin, mbps, bc.ReleaseMin)
			if !applied {
				detail += "；tc 暂未生效（VM 未运行或网卡未就绪），下一轮巡查会重放"
			}
			s.emit(vm.VmId, "bandwidth", "throttle", detail, true)
		}
	} else {
		st.bwOverSince = time.Time{}
	}
}

// checkConnections 活跃连接数巡查：in≥inLimit 或 out≥outLimit 持续 durationMin 分钟 → 停止 VM + 限速。
func (s *Service) checkConnections(conf config.Config, vm *agent.VMSummary, connMap map[string]connCount, st *vmState, now time.Time) {
	cc := conf.Patrol.Connections
	if cc.Enabled != nil && !*cc.Enabled {
		return
	}
	if s.isThrottled(vm.VmId) {
		return
	}
	ip := vmIPv4(vm)
	if ip == "" {
		return
	}
	c := connMap[ip]
	if c.in >= cc.InLimit || c.out >= cc.OutLimit {
		if st.connOverSince.IsZero() {
			st.connOverSince = now
		}
		if now.Sub(st.connOverSince) >= time.Duration(cc.DurationMin)*time.Minute {
			st.connOverSince = time.Time{}
			if err := s.mgr.StopVM(context.Background(), vm.VmId, false); err != nil {
				log.Printf("[Patrol] stop VM %s: %v", vm.VmId, err)
				return
			}
			_ = s.db.SavePatrolStop(vm.VmId, "connections")
			mbps := cc.ThrottleMbps
			release := now.Add(time.Duration(cc.ReleaseMin) * time.Minute)
			applied := s.applyThrottle(vm.VmId, mbps, release, "connections")
			detail := fmt.Sprintf("活跃连接 in=%d/out=%d 超限（阈值 in≥%d/out≥%d，持续 %d 分钟），停止 VM 并限速到 %d Mbps（%d 分钟后解除）",
				c.in, c.out, cc.InLimit, cc.OutLimit, cc.DurationMin, mbps, cc.ReleaseMin)
			if !applied {
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

// checkProcesses 容器内进程黑名单巡查：命中即杀，只记面板日志不通知 TG。
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
		// 镜像可能没有 ps（libpod top 依赖容器内 ps）。限频记日志，
		// 让「该 VM 的进程巡查被跳过」可见，而不是静默失效。
		if st.procErrAt.IsZero() || time.Since(st.procErrAt) >= time.Hour {
			st.procErrAt = time.Now()
			log.Printf("[Patrol] list processes in %s failed (image may lack ps, process check skipped): %v", vm.VmId, err)
		}
		return
	}
	for _, p := range procs {
		haystack := strings.ToLower(p.Comm + " " + p.Args)
		for _, b := range pc.Blacklist {
			if b == "" {
				continue
			}
			if strings.Contains(haystack, strings.ToLower(b)) {
				if err := s.prober.KillProcess(context.Background(), vm.VmId, p.PID); err != nil {
					log.Printf("[Patrol] kill process %d in %s: %v", p.PID, vm.VmId, err)
					continue
				}
				s.emit(vm.VmId, "process", "kill_process",
					fmt.Sprintf("命中黑名单 %q，已杀进程 %s (pid %d)", b, p.Comm, p.PID), false)
				break
			}
		}
	}
}
