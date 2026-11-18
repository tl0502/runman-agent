package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// podmanLimitsDropIn 是 rootful podman 所有容器合计资源预算的唯一配置源。
// machine.slice 是 rootful podman 容器的默认父 cgroup（systemd 管理），
// 因此对 machine.slice 设置 MemoryMax / CPUQuota 即可约束全部容器的合计用量，
// 而非单容器硬墙。仅 rootful + cgroup v2 + systemd 生效。
const podmanLimitsDropIn = "/etc/systemd/system/machine.slice.d/50-podman-limits.conf"

var (
	// systemd 尺寸语法：数字 + 可选单位（K/M/G/T/P/E，可带 i/B），如 3G / 2560M / 1.5GiB。
	limSizeRe  = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?[KMGTPE]?i?B?$`)
	limQuotaRe = regexp.MustCompile(`^[0-9]+$`)
)

// podmanLimitsView 是 GET /api/podman/limits 的返回结构。
type podmanLimitsView struct {
	Supported  bool   `json:"supported"`
	MemoryMax  string `json:"memory_max"`   // 配置值（systemd 尺寸，如 "3G"），空=不限
	MemoryHigh string `json:"memory_high"`  // 软上限（systemd 尺寸），空=不限
	SwapMax    string `json:"swap_max"`     // MemorySwapMax（内存+swap 合计），空=不限，"0"=禁用
	CPUQuota   string `json:"cpu_quota"`    // 配置值（百分比，如 "150" = 1.5 核），空=不限
	EffMemory  string `json:"eff_memory"`   // 生效值（人可读，如 "3.0G" / "不限"）
	EffMemHigh string `json:"eff_mem_high"` // 软上限生效值
	EffSwap    string `json:"eff_swap"`     // 生效值（人可读，如 "512M" / "禁用" / "不限"）
	EffCPU     string `json:"eff_cpu"`      // 生效值（人可读，如 "1.5 核" / "不限"）
}

type podmanLimitsRequest struct {
	MemoryMax  string `json:"memory_max"`
	MemoryHigh string `json:"memory_high"`
	SwapMax    string `json:"swap_max"`
	CPUQuota   string `json:"cpu_quota"`
}

func (s *Server) handlePodmanLimits(w http.ResponseWriter, _ *http.Request) {
	v := podmanLimitsView{Supported: s.podmanLimitsSupported()}
	if !v.Supported {
		jsonOK(w, v)
		return
	}
	v.MemoryMax, v.MemoryHigh, v.SwapMax, v.CPUQuota = readPodmanLimitsDropIn()
	show := showSliceProps("MemoryMax", "MemoryHigh", "MemorySwapMax", "CPUQuotaPerSecUSec")
	v.EffMemory = formatMemSize(show["MemoryMax"])
	v.EffMemHigh = formatMemSize(show["MemoryHigh"])
	v.EffSwap = formatSwap(show["MemorySwapMax"])
	v.EffCPU = formatCPUQuota(show["CPUQuotaPerSecUSec"])
	jsonOK(w, v)
}

func (s *Server) handlePodmanLimitsSave(w http.ResponseWriter, r *http.Request) {
	if !s.podmanLimitsSupported() {
		jsonErr(w, fmt.Errorf("当前后端不支持容器资源上限（需 Podman + cgroup v2）"), http.StatusBadRequest)
		return
	}
	var req podmanLimitsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	req.MemoryMax = strings.TrimSpace(req.MemoryMax)
	req.MemoryHigh = strings.TrimSpace(req.MemoryHigh)
	req.SwapMax = strings.TrimSpace(req.SwapMax)
	req.CPUQuota = strings.TrimSpace(req.CPUQuota)
	if req.MemoryMax != "" && !limSizeRe.MatchString(req.MemoryMax) {
		jsonErr(w, fmt.Errorf("内存上限格式无效（如 3G / 2560M）"), http.StatusBadRequest)
		return
	}
	if req.MemoryHigh != "" && !limSizeRe.MatchString(req.MemoryHigh) {
		jsonErr(w, fmt.Errorf("内存软上限格式无效（如 2560M）"), http.StatusBadRequest)
		return
	}
	if req.SwapMax != "" && !limSizeRe.MatchString(req.SwapMax) {
		jsonErr(w, fmt.Errorf("Swap 上限格式无效（如 512M，0=禁用）"), http.StatusBadRequest)
		return
	}
	if req.CPUQuota != "" {
		if !limQuotaRe.MatchString(req.CPUQuota) {
			jsonErr(w, fmt.Errorf("CPU 配额格式无效"), http.StatusBadRequest)
			return
		}
		if n, _ := strconv.Atoi(req.CPUQuota); n < 1 || n > 1000000 {
			jsonErr(w, fmt.Errorf("CPU 配额需在 0.01–10000 核之间"), http.StatusBadRequest)
			return
		}
	}
	if err := applyPodmanLimits(req.MemoryMax, req.MemoryHigh, req.SwapMax, req.CPUQuota); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

// podmanLimitsSupported 判断当前部署是否可用该能力：仅 Podman 后端 + cgroup v2。
func (s *Server) podmanLimitsSupported() bool {
	return s.cfg.Get().VirtType == "podman" && cgroupV2()
}

// cgroupV2 通过 /sys/fs/cgroup/cgroup.controllers 是否存在判断（v2 特有）。
func cgroupV2() bool {
	b, err := os.ReadFile("/sys/fs/cgroup/cgroup.controllers")
	return err == nil && len(strings.TrimSpace(string(b))) > 0
}

// readPodmanLimitsDropIn 从 drop-in 文件解析出配置的四个值（无则空）。
func readPodmanLimitsDropIn() (memoryMax, memoryHigh, swapMax, cpuQuota string) {
	b, err := os.ReadFile(podmanLimitsDropIn)
	if err != nil {
		return "", "", "", ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "MemoryMax="):
			memoryMax = strings.TrimPrefix(line, "MemoryMax=")
		case strings.HasPrefix(line, "MemoryHigh="):
			memoryHigh = strings.TrimPrefix(line, "MemoryHigh=")
		case strings.HasPrefix(line, "MemorySwapMax="):
			swapMax = strings.TrimPrefix(line, "MemorySwapMax=")
		case strings.HasPrefix(line, "CPUQuota="):
			cpuQuota = strings.TrimSuffix(strings.TrimPrefix(line, "CPUQuota="), "%")
		}
	}
	return memoryMax, memoryHigh, swapMax, cpuQuota
}

// applyPodmanLimits 三步齐全才影响运行中的容器：写 drop-in → daemon-reload →
// set-property 下发。四项都为空视为「不限」，删除 drop-in 并恢复默认。
func applyPodmanLimits(memoryMax, memoryHigh, swapMax, cpuQuota string) error {
	if memoryMax == "" && memoryHigh == "" && swapMax == "" && cpuQuota == "" {
		if err := os.Remove(podmanLimitsDropIn); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(podmanLimitsDropIn), 0o755); err != nil {
			return err
		}
		var b strings.Builder
		b.WriteString("[Slice]\n")
		if memoryMax != "" {
			b.WriteString("MemoryMax=" + memoryMax + "\n")
		}
		if memoryHigh != "" {
			b.WriteString("MemoryHigh=" + memoryHigh + "\n")
		}
		if swapMax != "" {
			b.WriteString("MemorySwapMax=" + swapMax + "\n")
		}
		if cpuQuota != "" {
			b.WriteString("CPUQuota=" + cpuQuota + "%\n")
		}
		if err := os.WriteFile(podmanLimitsDropIn, []byte(b.String()), 0o644); err != nil {
			return err
		}
	}

	if out, err := systemctlRun(15*time.Second, "systemctl", "daemon-reload"); err != nil {
		return fmt.Errorf("daemon-reload 失败: %v: %s", err, out)
	}

	args := []string{"systemctl", "set-property", "machine.slice"}
	if memoryMax != "" {
		args = append(args, "MemoryMax="+memoryMax)
	} else {
		// 空值 = 重置为默认（不限）。systemd 257 的 CPUQuota 不接受 infinity，
		// 统一用空值更干净，且能显式清掉上一次残留的配额。
		args = append(args, "MemoryMax=")
	}
	if memoryHigh != "" {
		args = append(args, "MemoryHigh="+memoryHigh)
	} else {
		args = append(args, "MemoryHigh=")
	}
	if swapMax != "" {
		args = append(args, "MemorySwapMax="+swapMax)
	} else {
		args = append(args, "MemorySwapMax=")
	}
	if cpuQuota != "" {
		args = append(args, "CPUQuota="+cpuQuota+"%")
	} else {
		args = append(args, "CPUQuota=")
	}
	if out, err := systemctlRun(15*time.Second, args...); err != nil {
		return fmt.Errorf("下发资源上限失败: %v: %s", err, out)
	}
	return nil
}

// showSliceProps 读取 machine.slice 的指定 systemd 属性，返回 map[属性名]值。
func showSliceProps(props ...string) map[string]string {
	out := map[string]string{}
	args := []string{"systemctl", "show", "machine.slice"}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	b, err := systemctlRun(5*time.Second, args...)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(b, "\n") {
		if i := strings.Index(line, "="); i > 0 {
			out[line[:i]] = strings.TrimSpace(line[i+1:])
		}
	}
	return out
}

// systemctlRun 执行 systemctl 并返回输出（去除首尾空白）。
func systemctlRun(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// formatMemSize 把 systemd 的字节数（或 infinity）转成人可读字符串。
func formatMemSize(v string) string {
	if v == "" || v == "infinity" {
		return "不限"
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return v
	}
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// formatSwap 把 MemorySwapMax 的字节数（或 infinity/0）转成人可读字符串。
// "0" 是 systemd 的禁用语义，单独标注，避免显示成 "0B" 让人误解为无 swap。
func formatSwap(v string) string {
	if v == "" || v == "infinity" {
		return "不限"
	}
	if v == "0" {
		return "禁用"
	}
	return formatMemSize(v)
}

// formatCPUQuota 把 CPUQuotaPerSecUSec（systemd 时间串或裸微秒）转成人可读核数。
func formatCPUQuota(v string) string {
	sec, ok := parseSystemdTime(v)
	if !ok {
		return "不限"
	}
	// 1 秒 CPU 时间/秒 = 100% = 1 核
	return strconv.FormatFloat(sec, 'f', -1, 64) + " 核"
}

// parseSystemdTime 解析 systemd 时间字符串（"1.500000s" / "1500ms" / 裸微秒）为秒。
func parseSystemdTime(v string) (float64, bool) {
	v = strings.TrimSpace(v)
	if v == "" || v == "infinity" {
		return 0, false
	}
	unit := 1e-6 // systemd USec 属性的裸值单位是微秒
	switch {
	case strings.HasSuffix(v, "us"):
		v = strings.TrimSuffix(v, "us")
		unit = 1e-6
	case strings.HasSuffix(v, "ms"):
		v = strings.TrimSuffix(v, "ms")
		unit = 1e-3
	case strings.HasSuffix(v, "min"):
		v = strings.TrimSuffix(v, "min")
		unit = 60
	case strings.HasSuffix(v, "h"):
		v = strings.TrimSuffix(v, "h")
		unit = 3600
	case strings.HasSuffix(v, "s"):
		v = strings.TrimSuffix(v, "s")
		unit = 1
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return 0, false
	}
	return n * unit, true
}
