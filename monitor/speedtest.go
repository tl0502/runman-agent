package monitor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 测速参数。取值理由：
//   - streams：单连接吞吐受接收窗口/RTT 限制（默认 tcp_rmem 上限 6MB，200ms RTT 时
//     天花板约 240Mbps），必须多流并发才能测出一千兆以上的链路。
//   - warmup：把 TCP 慢启动爬坡段丢掉，只统计稳定段。
//   - window：稳定段取样时长；太短噪声大，太长无谓消耗流量。
//   - dirCap：单方向累计流量上限。只有高速链路才撞得到（百兆机跑完 warmup+window
//     约 47MB 就主动收尾），千兆机 256MB 对应约 2 秒，仍能取到完整取样窗口。
const (
	speedStreams      = 4
	speedDirCap       = int64(256 << 20)
	speedDirBudget    = 25 * time.Second
	speedReadyTimeout = 8 * time.Second
	// speedTotalBudget 是一次测速的总时长上限：主源不可用时最多还要试两个备用源，
	// 没有它时面板上的手动测速可能要转一两分钟才显示失败。
	speedTotalBudget = 40 * time.Second
	// speedMinBody 是下载响应的最小可接受长度：低于它的响应基本都是错误页/
	// 劫持页/透明代理落地页，拿来算带宽只会得到一个假值。
	speedMinBody = int64(8 << 20)
)

// warmup / window 与测速源在测试里会被替换，因此是变量而非常量。
var (
	speedWarmup = 1200 * time.Millisecond
	speedWindow = 2600 * time.Millisecond
	// speedUpBody 是单条上传请求声明的长度（实测 8MB 可正常接受）。请求体发完会再起
	// 一个请求续上，因此单次大小与服务端限制无关，累计量由 speedDirCap 兜住。
	speedUpBody = int64(24 << 20)
)

// speedSource 是一个测速源。down 必需；up 为空表示该源只有下载镜像，
// 选到它时只能测下行（结果标记 Partial）。
// 顺序即优先级：主源失败才换下一个——备用源只解决「测不到」，不解决「测得慢」。
var speedSources = []speedSource{
	{
		name: "cloudflare",
		// Cloudflare 的 bytes 参数超过 ~92MB 直接回 403（实测 1e8 也是 403），
		// 取这个已验证可用的值；请求结束会续发，不影响长窗口取样。
		down: "https://speed.cloudflare.com/__down?bytes=92160000",
		up:   "https://speed.cloudflare.com/__up",
	},
	{name: "ovh", down: "https://proof.ovh.net/files/100Mb.dat"},
	{name: "cachefly", down: "https://cachefly.cachefly.net/100mb.test"},
}

type speedSource struct {
	name string
	down string
	up   string
}

// SpeedResult 是一次测速的结果。数值为十进制 Mbps，取稳定段平均值，
// 不含 DNS/TCP/TLS 握手与慢启动。
type SpeedResult struct {
	DownMbps      int32     `json:"down_mbps"`
	UpMbps        int32     `json:"up_mbps"`
	BandwidthMbps int32     `json:"bandwidth_mbps"` // min(下行, 上行)：非对称线路按窄的一侧算
	TestedAt      time.Time `json:"tested_at"`
	DurationMs    int64     `json:"duration_ms"`
	Source        string    `json:"source,omitempty"` // 实际生效的测速源
	// Partial 表示只有一个方向测成功，BandwidthMbps 取的是那一个方向的值。
	Partial bool `json:"partial,omitempty"`
}

// MeasureSpeed 依次尝试测速源，直到某个源的下行测成功为止。
// 两个方向串行测：并发会互相抢带宽，各自只能测到一半。
func MeasureSpeed(ctx context.Context) (SpeedResult, error) {
	ctx, cancel := context.WithTimeout(ctx, speedTotalBudget)
	defer cancel()

	start := time.Now()
	client := speedClient()
	res := SpeedResult{}
	var failures []string

	for _, src := range speedSources {
		down, err := measureDirection(ctx, client, src.down, false)
		if err != nil {
			failures = append(failures, src.name+" 下行: "+err.Error())
			continue
		}

		res.Source = src.name
		res.DownMbps = mbpsInt(down)
		switch {
		case src.up == "":
			// 下载镜像没有上行端点，只能给出下行带宽。
			res.Partial = true
		default:
			up, uerr := measureDirection(ctx, client, src.up, true)
			if uerr != nil {
				// 上行失败不影响下行结果：上行被挡、下行正常的机器很常见。
				res.Partial = true
				failures = append(failures, src.name+" 上行: "+uerr.Error())
			} else {
				res.UpMbps = mbpsInt(up)
			}
		}

		res.BandwidthMbps = minPositive(res.DownMbps, res.UpMbps)
		res.TestedAt = time.Now()
		res.DurationMs = time.Since(start).Milliseconds()
		if len(failures) > 0 {
			log.Printf("[Speed] 部分失败: %s", strings.Join(failures, "；"))
		}
		return res, nil
	}

	return res, fmt.Errorf("%s", strings.Join(failures, "；"))
}

// speedClient 用独立 Transport：测的是本机出口带宽，必须绕过 HTTPS_PROXY，
// 否则测到的是代理链路。连接池按并发流数放大，避免复用同一条连接。
func speedClient() *http.Client {
	var rt *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		rt = base.Clone()
	} else {
		rt = &http.Transport{}
	}
	rt.Proxy = nil
	rt.MaxIdleConnsPerHost = speedStreams
	rt.ResponseHeaderTimeout = speedReadyTimeout
	return &http.Client{Transport: rt}
}

// measureDirection 用多条并发流测一个方向：
//  1. 等到至少一条流真正开始传输才开始计时（握手与 TTFB 不计入传输时间）；
//  2. 丢掉 warmup 段，让慢启动爬完；
//  3. 取 window 段内的字节增量算速率；
//  4. 单方向累计流量撞到 speedDirCap 时提前收尾。
func measureDirection(ctx context.Context, client *http.Client, url string, up bool) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, speedDirBudget)
	defer cancel()

	var total atomic.Int64
	ready := make(chan struct{}, speedStreams)
	errs := make(chan error, speedStreams)

	var wg sync.WaitGroup
	for i := 0; i < speedStreams; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := streamLoop(ctx, client, url, up, &total, ready); err != nil {
				errs <- err
			}
		}()
	}

	select {
	case <-ready:
	case err := <-errs:
		cancel()
		wg.Wait()
		return 0, err
	case <-time.After(speedReadyTimeout):
		cancel()
		wg.Wait()
		return 0, errors.New("连接超时")
	case <-ctx.Done():
		cancel()
		wg.Wait()
		return 0, ctx.Err()
	}

	base := total.Load()
	t0 := time.Now()
	drain(&total, base, speedWarmup)
	b1, t1 := total.Load(), time.Now()
	drain(&total, base, speedWindow)
	b2, t2 := total.Load(), time.Now()

	cancel()
	wg.Wait()

	var rate float64
	if span := t2.Sub(t1); span >= 400*time.Millisecond {
		rate = throughputMbps(b2-b1, span)
	} else {
		// 流量上限在预热段就吃满了（万兆链路）：退化为「首字节→封顶」的整段平均。
		// 这类链路上慢启动只占几十毫秒，误差可接受，总比不给结果强。
		rate = throughputMbps(b2-base, t2.Sub(t0))
	}
	if rate <= 0 {
		// 一个字节都没动：网络不通或被中间设备挡住，不能报 0 当结果。
		return 0, errors.New("未测得有效流量")
	}
	return rate, nil
}

// drain 等待 d 时长，或单方向累计量达到 speedDirCap 后立即返回。
func drain(total *atomic.Int64, base int64, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if total.Load()-base >= speedDirCap {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// streamLoop 让一条连接持续传输：单次请求的体量受服务端限制（Cloudflare 92MB、
// 镜像文件大小），读完/发完就续一个请求，直到 context 结束或撞到单方向流量上限。
func streamLoop(ctx context.Context, client *http.Client, url string, up bool, total *atomic.Int64, ready chan<- struct{}) error {
	var once sync.Once
	markReady := func() { once.Do(func() { ready <- struct{}{} }) }
	buf := make([]byte, 32<<10)

	for {
		if ctx.Err() != nil || total.Load() >= speedDirCap {
			return nil
		}
		var err error
		if up {
			err = uploadOnce(ctx, client, url, total, markReady)
		} else {
			err = downloadOnce(ctx, client, url, total, markReady, buf)
		}
		if err != nil {
			return err
		}
	}
}

func downloadOnce(ctx context.Context, client *http.Client, url string, total *atomic.Int64, markReady func(), buf []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// 403/429/劫持之类必须显式失败：否则错误页的字节数会被算成带宽。
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 声明长度已知时先挡一道；ContentLength == -1（chunked）留给读取量兜底。
	if resp.ContentLength >= 0 && resp.ContentLength < speedMinBody {
		return fmt.Errorf("响应过小（%d 字节）", resp.ContentLength)
	}
	// 响应头到达即视为开始传输，TTFB 不计入速率。
	markReady()

	// got 是本次请求实际收到的字节数：读完却不到 speedMinBody，说明拿到的是
	// 落地页之类的小响应。必须报错，否则 streamLoop 会立刻再发一个请求空转。
	var got int64
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			got += int64(n)
			total.Add(int64(n))
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil // 测速正常收尾时取消
			}
			if err == io.EOF {
				if got < speedMinBody {
					return fmt.Errorf("响应过小（%d 字节）", got)
				}
				return nil
			}
			return err
		}
	}
}

func uploadOnce(ctx context.Context, client *http.Client, url string, total *atomic.Int64, markReady func()) error {
	body := &zeroReader{total: total, ready: markReady, left: speedUpBody}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return err
	}
	req.ContentLength = body.left
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil // 测速正常收尾时取消，连接被中断是预期内的
		}
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// 服务端可能在收完 body 前就拒绝（413/429）：同样不能当成有效数据。
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// zeroReader 产生零字节流用于上行测速，每读一次累加已发送量。
// 稳定段里 TCP 发送缓冲始终占用，此处的读取速率等于实际发送速率。
type zeroReader struct {
	total *atomic.Int64
	ready func()
	left  int64
}

func (r *zeroReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > r.left {
		n = int(r.left)
	}
	for i := 0; i < n; i++ {
		p[i] = 0
	}
	r.left -= int64(n)
	r.ready()
	r.total.Add(int64(n))
	return n, nil
}

func throughputMbps(bytes int64, d time.Duration) float64 {
	if bytes <= 0 || d <= 0 {
		return 0
	}
	return float64(bytes) * 8 / d.Seconds() / 1e6
}

func mbpsInt(v float64) int32 {
	if v <= 0 {
		return 0
	}
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(math.Round(v))
}

// minPositive 取两个方向中较小的正值；某一方向不可用时返回另一方向。
func minPositive(a, b int32) int32 {
	switch {
	case a <= 0:
		return b
	case b <= 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}
