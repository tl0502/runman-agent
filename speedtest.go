//go:build containers_image_openpgp

package main

import (
	"context"
	"encoding/json"
	"log"
	"runman-agent/db"
	"runman-agent/monitor"
	"sync"
	"time"
)

// speedTestSystemKey 是 System 表里保存最近一次测速结果的键。
const speedTestSystemKey = "host_speedtest"

// startupSpeedRetryDelays 是启动测速失败后的退避重试间隔。只覆盖「开机时
// DNS/网络还没就绪」这类一次性失败，不做周期性重测——之后的恢复入口是
// 面板上的手动测速按钮。
var startupSpeedRetryDelays = []time.Duration{time.Minute, 5 * time.Minute}

// startupSpeedSkipWindow：上次结果足够新就跳过启动测速。单次测速在千兆机上
// 要跑几百 MB，崩溃重启（几秒一次）不该每次都吃一遍流量。
const startupSpeedSkipWindow = 30 * time.Minute

// hostSpeedRunner 负责测速的调度与持久化：同一时刻只允许一次测速，
// 结果写 System 表，并在进程启动时回填到 HostMonitor——这样首轮测速完成前
// 心跳也有上一轮的值，而不是上报 0。
type hostSpeedRunner struct {
	db      *db.DB
	hostMon *monitor.HostMonitor

	mu      sync.Mutex
	running bool
	lastErr string
}

func newHostSpeedRunner(database *db.DB, mon *monitor.HostMonitor) *hostSpeedRunner {
	r := &hostSpeedRunner{db: database, hostMon: mon}
	r.restore()
	return r
}

// restore 回填上次结果。没有记录或记录损坏都不影响启动。
func (r *hostSpeedRunner) restore() {
	raw, err := r.db.GetSystem(speedTestSystemKey)
	if err != nil || raw == "" {
		return
	}
	var res monitor.SpeedResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		log.Printf("[Speed] 忽略损坏的历史记录: %v", err)
		return
	}
	if res.BandwidthMbps <= 0 {
		return
	}
	r.hostMon.SetSpeed(res)
	log.Printf("[Speed] 沿用上次测速结果 %d Mbps（%s）",
		res.BandwidthMbps, res.TestedAt.Format("2006-01-02 15:04"))
}

// Start 异步触发一次测速；已在测速时返回 false（面板据此回 409）。
func (r *hostSpeedRunner) Start() bool {
	if !r.begin() {
		return false
	}
	go r.runOnce()
	return true
}

// runOnce 跑一次完整测速。调用方必须已经持有 running 标记（begin）。
func (r *hostSpeedRunner) runOnce() {
	log.Printf("[Speed] 开始测速（4 条并发流，上下行各约 4 秒）...")
	res, err := monitor.MeasureSpeed(context.Background())
	if err == nil {
		r.apply(res)
	}
	r.finish(err)
}

// Status 返回当前是否在测速，以及最近一次失败原因。
func (r *hostSpeedRunner) Status() (running bool, lastErr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running, r.lastErr
}

// startupSkip 判断上次结果是否新鲜到可以跳过启动测速；返回结果年龄。
func (r *hostSpeedRunner) startupSkip() (time.Duration, bool) {
	res, ok := r.hostMon.Speed()
	if !ok {
		return 0, false
	}
	age := time.Since(res.TestedAt)
	return age, age < startupSpeedSkipWindow
}

// errState 返回最近一次失败原因；成功时为空串。
func (r *hostSpeedRunner) errState() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

// StartAtStartup 启动时测一次；失败按 startupSpeedRetryDelays 退避重试。
func (r *hostSpeedRunner) StartAtStartup() {
	go func() {
		if age, skip := r.startupSkip(); skip {
			log.Printf("[Speed] 上次测速在 %d 分钟前，跳过启动测速", int(age.Minutes()))
			return
		}
		for attempt := 0; attempt <= len(startupSpeedRetryDelays); attempt++ {
			if attempt > 0 {
				time.Sleep(startupSpeedRetryDelays[attempt-1])
			}
			// 面板抢先触发了测速：等它跑完，这一轮不计入失败次数。
			for !r.begin() {
				time.Sleep(15 * time.Second)
			}
			r.runOnce()
			if r.errState() == "" {
				return
			}
		}
		log.Printf("[Speed] 启动测速连续失败，改为等待面板手动触发")
	}()
}

// apply 写入内存缓存并落盘。持久化失败只影响重启后能否沿用，不影响本轮结果。
func (r *hostSpeedRunner) apply(res monitor.SpeedResult) {
	r.hostMon.SetSpeed(res)
	log.Printf("[Speed] 下行 %d Mbps / 上行 %d Mbps → 带宽 %d Mbps（%.1fs%s）",
		res.DownMbps, res.UpMbps, res.BandwidthMbps, float64(res.DurationMs)/1000, partialNote(res))

	b, err := json.Marshal(res)
	if err != nil {
		log.Printf("[Speed] 序列化结果失败: %v", err)
		return
	}
	if err := r.db.SetSystem(speedTestSystemKey, string(b)); err != nil {
		log.Printf("[Speed] 保存结果失败: %v", err)
	}
}

func partialNote(res monitor.SpeedResult) string {
	if res.Partial {
		return "，单方向失败"
	}
	return ""
}

func (r *hostSpeedRunner) begin() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	return true
}

func (r *hostSpeedRunner) finish(err error) {
	r.mu.Lock()
	r.running = false
	r.lastErr = ""
	if err != nil {
		r.lastErr = err.Error()
	}
	r.mu.Unlock()
	if err != nil {
		log.Printf("[Speed] 测速失败: %v", err)
	}
}
