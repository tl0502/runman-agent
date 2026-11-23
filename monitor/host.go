package monitor

import (
	"context"
	"runman-agent/proto/agent"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
)

// MonitorNICKey / MonitorDiskKey 是 context 中的监控配置 key
type contextKey string

const (
	NICKey  contextKey = "monitor_nic"
	DiskKey contextKey = "monitor_disk"
)

type HostStats struct {
	*agent.Heartbeat
	NetInTotal  int64  `json:"net_in_total"`
	NetOutTotal int64  `json:"net_out_total"`
	Os          string `json:"os,omitempty"`
	Kernel      string `json:"kernel,omitempty"`
}

var (
	osInfoOnce sync.Once
	osName     string
	osKernel   string
)

// hostOSKernel 返回宿主机操作系统与内核版本；进程生命周期内取一次即可。
func hostOSKernel() (string, string) {
	osInfoOnce.Do(func() {
		if i, err := host.Info(); err == nil {
			osKernel = i.KernelVersion
			osName = strings.TrimSpace(i.Platform + " " + i.PlatformVersion)
		}
	})
	return osName, osKernel
}

type HostMonitor struct {
	mu         sync.Mutex
	lastNetIn  int64
	lastNetOut int64
	lastTime   time.Time

	// 出口带宽取自最近一次测速（见 speedtest.go）。心跳只有一个带宽字段，
	// 取上下行的较小值；面板通过 /api/status 拿完整的 down/up 与测速时间。
	speed   SpeedResult
	speedOK bool
}

func NewHostMonitor() *HostMonitor {
	return &HostMonitor{
		lastTime: time.Now(),
	}
}

// SetSpeed 更新最近一次测速结果：进程启动时从 DB 回填，之后由测速流程写入。
func (h *HostMonitor) SetSpeed(r SpeedResult) {
	h.mu.Lock()
	h.speed = r
	h.speedOK = r.BandwidthMbps > 0
	h.mu.Unlock()
}

// Speed 返回最近一次测速结果；从未测出过结果时 ok=false。
func (h *HostMonitor) Speed() (SpeedResult, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.speed, h.speedOK
}

func (h *HostMonitor) GetStats(ctx context.Context) (*HostStats, error) {
	targetNIC, _ := ctx.Value(NICKey).(string)
	targetDisk, _ := ctx.Value(DiskKey).(string)

	v, _ := mem.VirtualMemory()
	c, _ := cpu.Percent(0, false)
	cpuCount, _ := cpu.Counts(false)

	diskPath := "/"
	if targetDisk != "" {
		diskPath = targetDisk
	}
	d, _ := disk.Usage(diskPath)

	loadAvg, _ := load.Avg()
	uptime, _ := host.Uptime()

	now := time.Now()
	h.mu.Lock()
	elapsed := now.Sub(h.lastTime).Seconds()
	if elapsed < 0.1 {
		elapsed = 1.0
	}

	netStats, _ := psnet.IOCounters(true)
	var currentIn, currentOut int64
	for _, ns := range netStats {
		if targetNIC != "" {
			if ns.Name == targetNIC {
				currentIn = int64(ns.BytesRecv)
				currentOut = int64(ns.BytesSent)
				break
			}
		} else {
			name := ns.Name
			if name == "lo" ||
				strings.HasPrefix(name, "veth") ||
				strings.HasPrefix(name, "br-") ||
				strings.HasPrefix(name, "docker") ||
				strings.HasPrefix(name, "podman") ||
				strings.HasPrefix(name, "cni-") {
				continue
			}
			currentIn += int64(ns.BytesRecv)
			currentOut += int64(ns.BytesSent)
		}
	}

	rateIn := int64(float64(currentIn-h.lastNetIn) / elapsed)
	rateOut := int64(float64(currentOut-h.lastNetOut) / elapsed)

	// 首次采样或计数器重置时速率无意义，归零
	if h.lastNetIn == 0 || rateIn < 0 {
		rateIn = 0
	}
	if h.lastNetOut == 0 || rateOut < 0 {
		rateOut = 0
	}

	bwMbps := h.speed.BandwidthMbps
	h.lastNetIn, h.lastNetOut, h.lastTime = currentIn, currentOut, now
	h.mu.Unlock()

	hb := &agent.Heartbeat{
		Timestamp:     now.Unix(),
		NetInBps:      rateIn,
		NetOutBps:     rateOut,
		Uptime:        int64(uptime),
		Cpus:          int32(cpuCount),
		BandwidthMbps: bwMbps,
	}

	if v != nil {
		hb.RamUsedMb = int64(v.Used / 1024 / 1024)
		hb.RamTotalMb = int64(v.Total / 1024 / 1024)
	}
	if len(c) > 0 {
		hb.CpuPct = float32(c[0])
	}
	if d != nil {
		hb.DiskUsedGb = int64(d.Used / 1024 / 1024 / 1024)
		hb.DiskTotalGb = int64(d.Total / 1024 / 1024 / 1024)
	}
	if loadAvg != nil {
		hb.Load1 = float32(loadAvg.Load1)
		hb.Load5 = float32(loadAvg.Load5)
		hb.Load15 = float32(loadAvg.Load15)
	}

	osName, osKernel := hostOSKernel()
	return &HostStats{
		Heartbeat:   hb,
		NetInTotal:  currentIn,
		NetOutTotal: currentOut,
		Os:          osName,
		Kernel:      osKernel,
	}, nil
}
