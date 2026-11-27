package web

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runman-agent/config"
	"runman-agent/db"
	"runman-agent/manager"
	"runman-agent/manager/cloudhv"
	"runman-agent/manager/egress"
	"runman-agent/manager/netmigrate"
	"runman-agent/manager/podman"
	"runman-agent/manager/portforward"
	"runman-agent/manager/wgbind"
	"runman-agent/monitor"
	"runman-agent/notify"
	"runman-agent/patrol"
	"runman-agent/proto/agent"
	"runman-agent/updater"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/shirou/gopsutil/v4/disk"
	psnet "github.com/shirou/gopsutil/v4/net"
	"golang.org/x/crypto/bcrypt"
)

//go:embed static/*
var staticFiles embed.FS

var wsUpgrader = websocket.Upgrader{
	// 同源校验：浏览器发起的 WS 一定带 Origin；空 Origin 放行非浏览器客户端
	// （脚本/测试）。未带有效 session cookie 的握手在 authMiddleware 已被 401。
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		return origin == "" || origin == "https://"+r.Host
	},
}

type vmNetSnapshot struct {
	inBytes   int64
	outBytes  int64
	timestamp time.Time
}

type Server struct {
	db           *db.DB
	cfg          *config.Manager
	mgr          manager.VMManager
	cloudHVMgr   *cloudhv.Manager // 如果使用 CloudHV，保存直接引用以便内存上报
	podmanMgr    *podman.Manager
	hostMon      *monitor.HostMonitor
	pf           *portforward.Manager
	wg           *wgbind.Manager
	egress       interface{ Apply(context.Context) error }
	agent        interface{}
	sessions     *sessionStore
	vmSnapshots  map[string]*vmNetSnapshot
	vmSnapshotMu sync.Mutex
	version      string
	commit       string
	updater      *updater.Service
	patrol       *patrol.Service
	notifier     *notify.Notifier
	speedTester  SpeedTester

	monitorHst  map[string][]vmListItem
	monitorMu   sync.RWMutex
	shareIPv6Mu sync.Mutex // serialize network shape, config commit, and application

	// 实例活跃连接快照（conntrack 全表解析 + 短 TTL 缓存）。
	// 面板实例列表每几秒轮询一次，而 conntrack 可能有几万行，不能每次请求都读。
	connMu      sync.Mutex
	connCache   map[string]patrol.ActiveConns
	connCacheAt time.Time
}

// NewServer 接收两个 manager：
// - mgr (svc) 是 VMService 包装层
// - rawMgr 是真正的驱动实现（CloudHV/Podman），用于内存上报等功能
func NewServer(database *db.DB, mgr manager.VMManager, hostMon *monitor.HostMonitor, cfg *config.Manager, pf *portforward.Manager, wg *wgbind.Manager, agent interface{}, rawMgr manager.VMManager, version, commit string, upd *updater.Service) *Server {
	var cloudHVMgr *cloudhv.Manager
	if ch, ok := rawMgr.(*cloudhv.Manager); ok {
		cloudHVMgr = ch
	}
	podmanMgr, _ := rawMgr.(*podman.Manager)

	return &Server{
		db:          database,
		cfg:         cfg,
		mgr:         mgr,
		cloudHVMgr:  cloudHVMgr,
		podmanMgr:   podmanMgr,
		hostMon:     hostMon,
		pf:          pf,
		wg:          wg,
		agent:       agent,
		sessions:    newSessionStore(database),
		version:     version,
		commit:      commit,
		updater:     upd,
		vmSnapshots: make(map[string]*vmNetSnapshot),
		monitorHst:  make(map[string][]vmListItem),
	}
}

// SetPatrol 注入巡查服务引用（非 Podman 后端时为 nil，相关端点返回未启用）。
func (s *Server) SetPatrol(p *patrol.Service) {
	s.patrol = p
}

// SetEgress 注入出站地址管理器，供共享 IPv6 开关在配置变更后立即生效。
func (s *Server) SetEgress(a *egress.Applier) {
	s.egress = a
}

// SetNotifier 注入全局通知器（与巡查共享同一实例，配置实时生效）。
func (s *Server) SetNotifier(n *notify.Notifier) {
	s.notifier = n
}

// SpeedTester 是测速模块的调度入口，由 main 包实现（避免 web 反向依赖 main）。
// Start 返回 false 表示已有测速在进行。
type SpeedTester interface {
	Start() bool
	Status() (running bool, lastErr string)
}

// SetSpeedTester 注入测速调度器；未注入时手动测速端点返回错误。
func (s *Server) SetSpeedTester(t SpeedTester) {
	s.speedTester = t
}

// authMiddleware 校验 cookie session；未登录返回 401（无 WWW-Authenticate，
// 前端收到 401 后显示登录页）。静态资源（登录页本体）与状态上报端点公开。
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 静态资源（index.html/app.js/app.css/vendor）必须公开，否则登录页无法加载；
		// /rfw 反代是防火墙管理面，需要鉴权。
		if !strings.HasPrefix(r.URL.Path, "/api/") && !strings.HasPrefix(r.URL.Path, "/rfw/") {
			next.ServeHTTP(w, r)
			return
		}
		// 状态上报（VM 用）与登录/登出端点公开
		// 公开资源：VM 状态上报、登录/登出、品牌信息与图标（登录页也要展示）。
		// 图标仅放行 GET；POST/DELETE（上传/重置）仍需登录。
		if r.URL.Path == "/api/vm-status" ||
			r.URL.Path == "/api/login" ||
			r.URL.Path == "/api/logout" ||
			r.URL.Path == "/api/brand" ||
			(r.URL.Path == "/api/brand/icon" && r.Method == http.MethodGet) {
			next.ServeHTTP(w, r)
			return
		}

		conf := s.cfg.Get()
		if conf.WebUser == "" {
			// 未配置用户名：面板无鉴权模式（保持旧行为）。
			// 注意：hash 为空但 WebUser 非空属异常态（如配置损坏），此时 bcrypt
			// 对空 hash 恒失败 → 走下面 401（fail-closed），绝不放行。
			next.ServeHTTP(w, r)
			return
		}

		c, err := r.Cookie(sessionCookie)
		if err == nil {
			if user, ok := s.sessions.validate(c.Value); ok {
				// 滑动续期：服务端 lastSeen 已在 validate 内更新，这里重发 cookie
				// 重置浏览器侧的 7 天窗口，活跃用户不会被绝对过期登出。
				setSessionCookie(w, c.Value, r.TLS != nil)
				r = r.WithContext(context.WithValue(r.Context(), sessionUserKey, user))
				next.ServeHTTP(w, r)
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	})
}

func (s *Server) ListenAndServe(addr string) error {
	mux := http.NewServeMux()

	// 系统
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("GET /api/brand", s.handleBrandPublic)
	// 统一事件流：巡查事件 + 安全登录日志按时间归并
	mux.HandleFunc("GET /api/events/feed", s.handleEventsFeed)
	// 安全：登录日志与封禁
	mux.HandleFunc("GET /api/security/log", s.handleSecurityLog)
	mux.HandleFunc("GET /api/security/bans", s.handleSecurityBans)
	mux.HandleFunc("POST /api/security/ban", s.handleSecurityBanCreate)
	mux.HandleFunc("DELETE /api/security/ban/{ip}", s.handleSecurityBanDelete)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	// GET/POST 分开注册：不限定方法时 PUT /api/config 会落进 GET 分支返回 200，
	// 调用方误以为写入生效（Q8）。Go 1.22 的 ServeMux 允许同路径多方法模式共存。
	mux.HandleFunc("GET /api/config", s.handleConfig)
	mux.HandleFunc("POST /api/config", s.handleConfig)
	mux.HandleFunc("POST /api/ipv6/share", s.handleShareIPv6)
	mux.HandleFunc("GET /api/network/addresses", s.handleNetworkAddresses)
	mux.HandleFunc("GET /api/brand/icon", s.handleBrandIcon)
	mux.HandleFunc("POST /api/brand/icon", s.handleBrandIcon)
	mux.HandleFunc("DELETE /api/brand/icon", s.handleBrandIcon)
	// 自定义图标同时充当 tab 图标；没有配置时回落到内置 favicon。
	mux.HandleFunc("GET /favicon.ico", s.handleFavicon)
	mux.HandleFunc("GET /api/system/info", s.handleSystemInfo)
	mux.HandleFunc("GET /api/podman/limits", s.handlePodmanLimits)
	mux.HandleFunc("POST /api/podman/limits", s.handlePodmanLimitsSave)
	mux.HandleFunc("GET /api/connection", s.handleConnection)
	mux.HandleFunc("GET /api/hostfw/status", s.handleHostFWStatus)
	// 清除平台凭据（显式动作，等同退出平台接入）
	mux.HandleFunc("DELETE /api/platform/token", s.handleClearPlatformToken)
	mux.HandleFunc("GET /api/version", s.handleVersion)
	mux.HandleFunc("GET /api/update-check", s.handleUpdateCheck)
	mux.HandleFunc("POST /api/upgrade", s.handleUpgrade)

	// 镜像列表
	mux.HandleFunc("GET /api/images", s.handleImages)
	mux.HandleFunc("GET /api/images/config", s.handleGetImagesConfig)
	mux.HandleFunc("POST /api/images/config", s.handleSaveImagesConfig)

	// 自定义镜像（仅 podman）
	mux.HandleFunc("GET /api/images/custom", s.handleListCustomImages)
	mux.HandleFunc("POST /api/images/custom", s.handleAddCustomImage)
	mux.HandleFunc("DELETE /api/images/custom", s.handleDeleteCustomImage)

	// VM 集合
	mux.HandleFunc("GET /api/vms", s.handleListVMs)
	mux.HandleFunc("POST /api/vms", s.handleCreateVM)

	// VM 单机操作
	mux.HandleFunc("GET /api/vms/{id}", s.handleGetVM)
	mux.HandleFunc("POST /api/vms/{id}/start", s.handleStartVM)
	mux.HandleFunc("POST /api/vms/{id}/stop", s.handleStopVM)
	mux.HandleFunc("POST /api/vms/{id}/restart", s.handleRestartVM)
	mux.HandleFunc("DELETE /api/vms/{id}", s.handleDeleteVM)
	mux.HandleFunc("POST /api/vms/{id}/reinstall", s.handleReinstallVM)
	mux.HandleFunc("POST /api/vms/{id}/reset-password", s.handleResetPassword)
	mux.HandleFunc("POST /api/vms/{id}/reset-traffic", s.handleResetTraffic)

	// 端口转发
	mux.HandleFunc("GET /api/vms/{id}/portfwds", s.handleListPortFwds)
	mux.HandleFunc("POST /api/vms/{id}/portfwds", s.handleAddPortFwd)
	mux.HandleFunc("PUT /api/vms/{id}/portfwds", s.handleSyncPortFwds)
	mux.HandleFunc("DELETE /api/vms/{id}/portfwds/{protocol}/{hostPort}", s.handleDelPortFwd)

	// WireGuard 绑定（纯用户态隧道，全端口转发到 VM）
	mux.HandleFunc("GET /api/vms/{id}/wg", s.handleListWG)
	mux.HandleFunc("POST /api/vms/{id}/wg", s.handleAddWG)
	mux.HandleFunc("PUT /api/vms/{id}/wg/{bindID}", s.handleUpdateWG)
	mux.HandleFunc("DELETE /api/vms/{id}/wg/{bindID}", s.handleDelWG)
	mux.HandleFunc("POST /api/wg/keypair", s.handleWGKeypair)

	// 端口转发唯一来源 IP 数限制
	mux.HandleFunc("GET /api/vms/{id}/iplimit", s.handleGetIPLimit)
	mux.HandleFunc("PUT /api/vms/{id}/iplimit", s.handleSetIPLimit)

	// 控制台 TTY（WebSocket）
	mux.HandleFunc("GET /api/vms/{id}/tty", s.handleVMTTY)

	// 虚拟机状态上报
	mux.HandleFunc("POST /api/vm-status", s.handleVMStatus)

	// rfw 在线状态检测
	mux.HandleFunc("GET /api/rfw/online", s.handleRfwOnline)

	// rfw 反向代理：/rfw/* → http://rfw_addr/*（去掉 /rfw 前缀）
	mux.Handle("/rfw/", s.rfwReverseProxy())

	// 大盘监控
	mux.HandleFunc("GET /api/monitor/latest", s.handleMonitorLatest)
	mux.HandleFunc("GET /api/monitor/history", s.handleMonitorHistory)

	// 自动巡查
	mux.HandleFunc("GET /api/patrol/events", s.handlePatrolEvents)
	mux.HandleFunc("GET /api/patrol/anomalies", s.handlePatrolAnomalies)
	mux.HandleFunc("GET /api/patrol/throttles", s.handlePatrolThrottles)
	mux.HandleFunc("POST /api/patrol/throttles/{vmID}/release", s.handlePatrolRelease)
	mux.HandleFunc("GET /api/patrol/config", s.handlePatrolConfig)
	mux.HandleFunc("PUT /api/patrol/config", s.handlePatrolConfig)
	mux.HandleFunc("POST /api/patrol/config", s.handlePatrolConfig)

	// 通知（Agent 侧全局能力，当前通道 Telegram）
	mux.HandleFunc("POST /api/notify/test", s.handleNotifyTest)
	// 手动测速：异步触发，结果通过 /api/status 的 speed 字段回传
	mux.HandleFunc("POST /api/speedtest", s.handleSpeedTest)

	// 启动后台监控收集
	go s.startMonitorLoop()

	// 静态文件
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			data, err := staticFiles.ReadFile("static/index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			// 首帧按登录态渲染：session 有效则去掉挡板（否则未登录用户会闪登录页、
			// 已登录用户会闪登录面板——静态 HTML 自己无法知道登录态，由服务端注入）。
			// HTML 源文件的静态默认是"未登录挡板态"，探测失败时 JS 的 401 逻辑仍兜底。
			if s.authedFromCookie(r) {
				data = bytes.Replace(data, []byte(`<body class="logged-out">`), []byte(`<body>`), 1)
				data = bytes.Replace(data,
					[]byte(`<section class="view" id="view-login">`),
					[]byte(`<section class="view" id="view-login" hidden>`), 1)
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// 首帧内容依赖 cookie 状态，绝不能缓存，否则登出后刷新仍显示内部页。
			w.Header().Set("Cache-Control", "no-store")
			if _, err := w.Write(data); err != nil {
				log.Printf("write response error: %v", err)
			}
			return
		}
		filePath := "static" + r.URL.Path
		data, err := staticFiles.ReadFile(filePath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", staticContentType(filePath))
		// 面板随二进制一起升级，缓存必须每次回源，避免升级后仍用旧 CSS/JS。
		w.Header().Set("Cache-Control", "no-cache")
		if _, err := w.Write(data); err != nil {
			log.Printf("write response error: %v", err)
		}
	})

	// TLS：面板原地升级为 HTTPS（自签证书，首次启动自动生成并复用）
	certPath, keyPath := s.tlsPaths()
	cert, err := ensureSelfSignedCert(certPath, keyPath)
	if err != nil {
		return fmt.Errorf("ensure TLS certificate: %w", err)
	}
	ln, err := tls.Listen("tcp", addr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	if err != nil {
		return err
	}
	// ReadHeaderTimeout 挡住「只发半个请求头然后挂住」的连接。
	// ReadTimeout 继续兜住「头发完了但 body 不发」——只设头超时时，这种连接
	// 可以一直占着 fd 与 goroutine（实测 20 条就能稳定占住 20 个 fd）。
	// 不设 WriteTimeout：SSE 事件流与 Web 终端都是长连接，写超时会误杀。
	// IdleTimeout 回收 keep-alive 空闲连接。
	srv := &http.Server{
		Handler:           s.authMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return srv.Serve(ln)
}

// tlsPaths 返回自签证书的存放路径（与 agent 数据目录同级）。
func (s *Server) tlsPaths() (string, string) {
	dir := filepath.Dir(s.cfg.Get().DB)
	if dir == "" || dir == "." {
		dir = "/opt/narwhal-agent"
	}
	return filepath.Join(dir, "web-cert.pem"), filepath.Join(dir, "web-key.pem")
}

// ─── 登录会话 ──────────────────────────────────────────────────────────────────

// handleLogin 校验用户名/密码并颁发会话 cookie。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}
	// 免鉴权端点必须限制 body 大小，防止解码前的内存放大 DoS
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		jsonErr(w, fmt.Errorf("invalid request body"), http.StatusBadRequest)
		return
	}

	ip := s.clientIP(r)

	// 手动封禁检查（先于限速）：被封 IP 的登录请求直接拒绝并记录事件——
	// 这是"攻击者被封后仍在活动"的观测点。响应与密码错误一致，不泄漏封禁状态。
	// 事件 60s 节流：被封 IP 高频重试时不刷爆时间线/写放大 DB。
	if _, banned := s.db.GetLoginBan(ip); banned {
		if s.sessions.recordBlocked(ip) {
			s.db.InsertLoginLog("login_blocked", ip, req.User, "封禁中的 IP 尝试登录")
			s.notifier.Send(notify.KindLoginBlocked,
				"登录告警 ["+req.User+"] ("+ip+")\n封禁中的 IP 仍在尝试登录")
		}
		jsonErr(w, fmt.Errorf("用户名或密码错误"), http.StatusUnauthorized)
		return
	}

	if s.sessions.loginLocked(ip) {
		// 自动锁定中：429 响应但不记事件（爆破工具高频敲击会刷爆时间线），
		// 锁定状态本身已在封禁列表中可见。
		jsonErr(w, fmt.Errorf("尝试次数过多，请 %d 分钟后再试", int(lockoutDuration.Minutes())), http.StatusTooManyRequests)
		return
	}

	conf := s.cfg.Get()
	match := req.User == conf.WebUser &&
		bcrypt.CompareHashAndPassword([]byte(conf.WebPassHash), []byte(req.Password)) == nil
	if !match {
		// 用户名不匹配时短路跳过了 bcrypt（~100ms），用固定 dummy hash 补一次，
		// 拉平"用户名错"与"密码错"的响应耗时，阻断用户名枚举时序侧信道。
		if req.User != conf.WebUser {
			_ = bcrypt.CompareHashAndPassword([]byte(dummyBcryptHash), []byte(req.Password))
		}
		if until, locked := s.sessions.recordFail(ip); locked {
			detail := fmt.Sprintf("连续 %d 次失败，锁定至 %s", maxLoginFails, until.Format("15:04:05"))
			s.db.InsertLoginLog("auto_ban", ip, req.User, detail)
			s.notifier.Send(notify.KindAutoBan,
				"登录告警 ["+req.User+"] ("+ip+")\n"+detail)
		} else {
			s.db.InsertLoginLog("login_fail", ip, req.User, "")
		}
		jsonErr(w, fmt.Errorf("用户名或密码错误"), http.StatusUnauthorized)
		return
	}
	s.sessions.recordSuccess(ip)
	s.db.InsertLoginLog("login_ok", ip, req.User, "")
	s.notifier.Send(notify.KindLoginOk,
		"登录通知 ["+req.User+"] ("+ip+")\n面板登录成功")

	token, err := s.sessions.create(req.User)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	// 复核：bcrypt 期间密码被改（会话已全部被踢），本次颁发的 token 必须作废，
	// 否则旧密码可在踢人窗口后拿到有效会话。
	if cur := s.cfg.Get(); cur.WebPassHash != conf.WebPassHash {
		s.sessions.destroy(token)
		jsonErr(w, fmt.Errorf("用户名或密码错误"), http.StatusUnauthorized)
		return
	}
	setSessionCookie(w, token, r.TLS != nil)
	jsonOK(w, map[string]string{"user": req.User})
}

// dummyBcryptHash 为一串合法但无对应意义的 bcrypt 摘要，仅用于登录时序拉平。
const dummyBcryptHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// handleLogout 销毁当前会话（幂等）。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.sessions.destroy(c.Value)
	}
	clearSessionCookie(w)
	w.WriteHeader(http.StatusNoContent)
}

// ─── 安全：登录日志与封禁 ──────────────────────────────────────────────────────

// handleSecurityLog 返回登录安全事件时间线（最新在前）。
func (s *Server) handleSecurityLog(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	jsonOK(w, s.db.ListLoginLog(limit))
}

// ─── 自动巡查 ──────────────────────────────────────────────────────────────────

// handlePatrolEvents 返回巡查事件日志，支持筛选/排序/分页：
//
//	severity: bad(严重) / warn(警告) / info(信息)
//	check:    bandwidth / connections / resources_cpu / resources_mem / resources_disk / process
//	vm_id:    模糊匹配 VM ID
//	sort:     asc（时间正序）；缺省最新在前
//	page / page_size: 分页（page 从 1 起）
func (s *Server) handlePatrolEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	page := 1
	if v := q.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	pageSize := 20
	if v := q.Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			pageSize = n
		}
	}

	query := db.PatrolEventQuery{
		Check: q.Get("check"),
		VMID:  q.Get("vm_id"),
		Sort:  q.Get("sort"),
	}
	switch q.Get("severity") {
	case "bad":
		query.Actions = []string{"stop_vm", "kill_process"}
	case "warn":
		query.Actions = []string{"throttle", "restart_vm", "alert"}
	case "info":
		query.Actions = []string{"release", "recovered"}
	}

	items, total, err := s.db.QueryPatrolEvents(query, (page-1)*pageSize, pageSize)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]interface{}{
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"items":     items,
	})
}

// ─── 统一事件流 ──────────────────────────────────────────────────────────────

// feedItem 统一事件流条目：两源字段归一到同一个形状。
type feedItem struct {
	TS     time.Time `json:"ts"`
	Source string    `json:"source"` // patrol / security
	Type   string    `json:"type"`   // 巡查=检查项（bandwidth…）；安全=动作（login_ok…）
	Action string    `json:"action"`
	Sev    string    `json:"sev"`            // bad / warn / info
	Actor  string    `json:"actor"`          // 巡查=vm_id；安全=IP
	User   string    `json:"user,omitempty"` // 安全侧的登录用户名
	Detail string    `json:"detail"`
}

// patrolSevOf 巡查动作 → 严重度（与 handlePatrolEvents 的过滤组保持一致）。
func patrolSevOf(action string) string {
	switch action {
	case "stop_vm", "kill_process":
		return "bad"
	case "throttle", "restart_vm", "alert":
		return "warn"
	default:
		return "info"
	}
}

// securitySevOf 登录动作 → 严重度：封禁类最重，登录失败次之，其余是流水。
func securitySevOf(action string) string {
	switch action {
	case "auto_ban", "login_blocked", "manual_ban":
		return "bad"
	case "login_fail":
		return "warn"
	default:
		return "info"
	}
}

// severityActionGroups 把严重度翻译成两源各自的动作过滤组。
func severityActionGroups(sev string) (patrol, security []string) {
	switch sev {
	case "bad":
		return []string{"stop_vm", "kill_process"}, []string{"auto_ban", "login_blocked", "manual_ban"}
	case "warn":
		return []string{"throttle", "restart_vm", "alert"}, []string{"login_fail"}
	case "info":
		return []string{"release", "recovered"}, []string{"login_ok", "unban"}
	}
	return nil, nil
}

// handleEventsFeed 返回统一事件流：GET /api/events/feed
//
//	source:   空=全部 / patrol / security
//	severity: bad / warn / info（空=全部）
//	check:    巡查侧检查项（bandwidth…），来源含巡查时生效
//	sec_type: 安全侧类型组 ok / fail / ban，来源含安全时覆盖 severity 的安全侧分组
//	since:    unix 秒，只返回该时刻之后的事件（两源统一语义）
//	q:        内容搜索（巡查 detail/vm_id，安全 detail/ip/user）
//	page / page_size: 分页（page 从 1 起）
//
// 两源各自按条件取 offset+limit 条，按时间倒序归并后切窗口；
// total 为两源过滤后的总数之和。
func (s *Server) handleEventsFeed(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := 1
	if v := q.Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	pageSize := 20
	if v := q.Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			pageSize = n
		}
	}
	offset := (page - 1) * pageSize

	source := q.Get("source")
	if source != "" && source != "patrol" && source != "security" {
		jsonErr(w, fmt.Errorf("source 取值为 patrol / security"), http.StatusBadRequest)
		return
	}
	patrolActions, secActions := severityActionGroups(q.Get("severity"))
	// 登录侧独立筛选维度（登录 Tab 专用）：类型组语义与旧安全页的筛选一致。
	if v := q.Get("sec_type"); v != "" {
		switch v {
		case "ok":
			secActions = []string{"login_ok"}
		case "fail":
			secActions = []string{"login_fail"}
		case "ban":
			secActions = []string{"auto_ban", "login_blocked", "manual_ban", "unban"}
		}
	}
	// 时间筛选（unix 秒）：两源统一语义，只返回该时刻之后的事件。
	var since int64
	if v := q.Get("since"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			since = n
		}
	}
	// 内容搜索：巡查匹配 detail/vm_id，安全匹配 detail/ip/user。
	search := strings.TrimSpace(q.Get("q"))
	fetchPatrol := source == "" || source == "patrol"
	fetchSecurity := source == "" || source == "security"

	items := []feedItem{}
	var total int64
	// 每侧取各自最新的 offset+pageSize 条（offset=0）：两源最新段合并起来
	// 必然覆盖全局窗口 [offset, offset+pageSize)，归并后再切片。
	if fetchPatrol {
		pItems, pTotal, err := s.db.QueryPatrolEvents(db.PatrolEventQuery{Actions: patrolActions, Check: q.Get("check"), Since: since, Q: search}, 0, offset+pageSize)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		total += pTotal
		for _, e := range pItems {
			items = append(items, feedItem{
				TS: e.TS, Source: "patrol", Type: e.Check, Action: e.Action,
				Sev: patrolSevOf(e.Action), Actor: e.VMID, Detail: e.Detail,
			})
		}
	}
	if fetchSecurity {
		lItems, lTotal, err := s.db.QueryLoginLogs(db.LoginLogQuery{Actions: secActions, Since: since, Q: search}, 0, offset+pageSize)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		total += lTotal
		for _, e := range lItems {
			items = append(items, feedItem{
				TS: e.TS, Source: "security", Type: e.Action, Action: e.Action,
				Sev: securitySevOf(e.Action), Actor: e.IP, User: e.User, Detail: e.Detail,
			})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].TS.After(items[j].TS) })
	if offset >= len(items) {
		items = items[:0]
	} else if end := offset + pageSize; end < len(items) {
		items = items[offset:end]
	} else {
		items = items[offset:]
	}
	jsonOK(w, map[string]interface{}{
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"items":     items,
	})
}

type patrolThrottleView struct {
	VMID      string `json:"vm_id"`
	RateMbps  int    `json:"rate_mbps"`
	ReleaseAt int64  `json:"release_at"` // unix 秒
	Reason    string `json:"reason"`
}

// handlePatrolThrottles 返回当前生效中的限速状态。
func (s *Server) handlePatrolThrottles(w http.ResponseWriter, _ *http.Request) {
	out := []patrolThrottleView{}
	if s.patrol == nil {
		jsonOK(w, out)
		return
	}
	for _, t := range s.patrol.ActiveThrottles() {
		out = append(out, patrolThrottleView{VMID: t.VMID, RateMbps: t.RateMbps, ReleaseAt: t.ReleaseAt.Unix(), Reason: t.Reason})
	}
	jsonOK(w, out)
}

// patrolAnomalyView 是「异常 VM」面板的条目：限速中 / 被停机两类当前未解决的异常。
type patrolAnomalyView struct {
	VMID      string `json:"vm_id"`
	Type      string `json:"type"`   // throttle | stopped
	Reason    string `json:"reason"` // bandwidth | connections | resources | process
	RateMbps  int    `json:"rate_mbps,omitempty"`
	ReleaseAt int64  `json:"release_at,omitempty"`
	StoppedAt int64  `json:"stopped_at,omitempty"`
}

// handlePatrolAnomalies 返回当前未解决的异常（限速 + 被停机）合并列表。
func (s *Server) handlePatrolAnomalies(w http.ResponseWriter, _ *http.Request) {
	out := []patrolAnomalyView{}
	if s.patrol == nil {
		jsonOK(w, out)
		return
	}
	for _, t := range s.patrol.ActiveThrottles() {
		out = append(out, patrolAnomalyView{VMID: t.VMID, Type: "throttle", Reason: t.Reason, RateMbps: t.RateMbps, ReleaseAt: t.ReleaseAt.Unix()})
	}
	for _, st := range s.patrol.ActiveStops() {
		out = append(out, patrolAnomalyView{VMID: st.VMID, Type: "stopped", Reason: st.Reason, StoppedAt: st.StoppedAt.Unix()})
	}
	jsonOK(w, out)
}

// handlePatrolRelease 人工解除某个 VM 的限速。
func (s *Server) handlePatrolRelease(w http.ResponseWriter, r *http.Request) {
	if s.patrol == nil {
		jsonErr(w, fmt.Errorf("巡查未启用"), http.StatusBadRequest)
		return
	}
	if err := s.patrol.ReleaseThrottle(r.PathValue("vmID")); err != nil {
		jsonErr(w, fmt.Errorf("带宽恢复未完成，将自动重试：%w", err), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]bool{"ok": true})
}

// handlePatrolConfig 读/写巡查 + Telegram 配置（旧端点，前端已改走 /api/config）。
// bot_token 口径与主配置一致：GET 脱敏回传 + telegram_has_token，PUT 留空 = 保留。
func (s *Server) handlePatrolConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		var req struct {
			Patrol   config.PatrolConfig   `json:"patrol"`
			Telegram config.TelegramConfig `json:"telegram"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		normalizePatrol(&req.Patrol)
		if err := validatePatrol(&req.Patrol); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		if err := s.cfg.Update(func(c *config.Config) {
			c.Patrol = req.Patrol
			tg := req.Telegram
			if tg.BotToken == "" {
				tg.BotToken = c.Telegram.BotToken
			}
			c.Telegram = tg
		}); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	conf := s.cfg.Get()
	tg := conf.Telegram
	tg.BotToken = ""
	jsonOK(w, map[string]interface{}{
		"patrol":             conf.Patrol,
		"telegram":           tg,
		"telegram_has_token": conf.Telegram.BotToken != "",
	})
}

// handleNotifyTest 用当前 Telegram 配置同步发送测试消息，当场验证通道可用性。
// 不受 enabled 开关限制：配了凭据即可测，否则「开关开着但没收到」与
// 「根本发不出去」无法区分排查。
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil {
		jsonErr(w, fmt.Errorf("通知器未初始化"), http.StatusInternalServerError)
		return
	}
	if err := s.notifier.SendTest(); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	jsonOK(w, map[string]bool{"ok": true})
}

// handleSpeedTest 异步触发一次出口带宽测速。整个过程约 10 秒，
// 结果通过 /api/status 的 speed 字段回传，面板轮询即可看到变化。
func (s *Server) handleSpeedTest(w http.ResponseWriter, r *http.Request) {
	if s.speedTester == nil {
		jsonErr(w, fmt.Errorf("测速模块未初始化"), http.StatusInternalServerError)
		return
	}
	if !s.speedTester.Start() {
		jsonErr(w, fmt.Errorf("测速进行中，请稍候"), http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]bool{"started": true})
}

// normalizePatrol 把面板传来的值夹取到合法区间（所有阈值/时长可配置，但须在安全范围）。
func normalizePatrol(p *config.PatrolConfig) {
	clamp := func(v int, lo, hi int) int {
		if v < lo {
			return lo
		}
		if v > hi {
			return hi
		}
		return v
	}
	if p.IntervalSec < 5 {
		p.IntervalSec = 60
	}
	p.Bandwidth.ThresholdPct = clamp(p.Bandwidth.ThresholdPct, 1, 100)
	p.Bandwidth.DurationMin = clamp(p.Bandwidth.DurationMin, 1, 10080)
	p.Bandwidth.ThrottleMbps = clamp(p.Bandwidth.ThrottleMbps, 1, 100000)
	p.Bandwidth.ReleaseMin = clamp(p.Bandwidth.ReleaseMin, 1, 10080)
	p.Connections.InLimit = clamp(p.Connections.InLimit, 1, 1000000)
	p.Connections.OutLimit = clamp(p.Connections.OutLimit, 1, 1000000)
	p.Connections.DurationMin = clamp(p.Connections.DurationMin, 1, 10080)
	p.Connections.ThrottleMbps = clamp(p.Connections.ThrottleMbps, 1, 100000)
	p.Connections.ReleaseMin = clamp(p.Connections.ReleaseMin, 1, 10080)
	p.Resources.CpuPct = clamp(p.Resources.CpuPct, 1, 100)
	p.Resources.CpuMin = clamp(p.Resources.CpuMin, 1, 10080)
	p.Resources.MemPct = clamp(p.Resources.MemPct, 1, 100)
	p.Resources.MemMin = clamp(p.Resources.MemMin, 1, 10080)
	p.Resources.DiskPct = clamp(p.Resources.DiskPct, 1, 100)
	p.Resources.DiskAlertIntervalMin = clamp(p.Resources.DiskAlertIntervalMin, 10, 10080)
	p.Resources.RestartCooldownMin = clamp(p.Resources.RestartCooldownMin, 1, 10080)
	// 0 是合法值（关闭连杀停机），clamp 下限为 0。
	p.Processes.KillStopLimit = clamp(p.Processes.KillStopLimit, 0, 10000)
	if p.Resources.Action != "restart" && p.Resources.Action != "stop" && p.Resources.Action != "alert" {
		p.Resources.Action = "restart"
	}
	p.Abuse.UdpConnLimit = clamp(p.Abuse.UdpConnLimit, 1, 1000000)
	p.Abuse.ScanDstLimit = clamp(p.Abuse.ScanDstLimit, 1, 4096)
	p.Abuse.SmtpLimit = clamp(p.Abuse.SmtpLimit, 1, 1000000)
	p.Abuse.DurationMin = clamp(p.Abuse.DurationMin, 1, 10080)
	p.Abuse.ThrottleMbps = clamp(p.Abuse.ThrottleMbps, 1, 100000)
	p.Abuse.ReleaseMin = clamp(p.Abuse.ReleaseMin, 1, 10080)
	p.Abuse.UdpAction = normAbuseAction(p.Abuse.UdpAction, "throttle")
	p.Abuse.ScanAction = normAbuseAction(p.Abuse.ScanAction, "alert")
	p.Abuse.SmtpAction = normAbuseAction(p.Abuse.SmtpAction, "alert")
	ports := make([]int, 0, len(p.Abuse.SmtpPorts))
	for _, port := range p.Abuse.SmtpPorts {
		if port >= 1 && port <= 65535 {
			ports = append(ports, port)
		}
	}
	if len(ports) == 0 {
		ports = []int{25} // 与 applyPatrolDefaults 一致：空黑名单回落 SMTP 缺省端口
	}
	p.Abuse.SmtpPorts = ports
	p.SelfCheckFailLimit = clamp(p.SelfCheckFailLimit, 1, 100)
}

// normAbuseAction 归一滥用识别动作：非法值回落 def（alert/throttle/stop 三档）。
func normAbuseAction(v, def string) string {
	switch v {
	case "alert", "throttle", "stop":
		return v
	}
	return def
}

// validatePatrol 校验面板提交的巡查配置（补充 normalize 未覆盖的硬约束）。
func validatePatrol(p *config.PatrolConfig) error {
	if p.IntervalSec < 5 || p.IntervalSec > 86400 {
		return fmt.Errorf("巡查周期须在 5-86400 秒之间")
	}
	return nil
}

// securityBanView 封禁列表条目：合并 DB 手动封禁与内存自动锁。
type securityBanView struct {
	IP     string `json:"ip"`
	Source string `json:"source"` // manual | auto
	Until  int64  `json:"until"`  // unix 秒；0 = 永久
	Reason string `json:"reason"`
}

// handleSecurityBans 返回当前生效中的全部封禁。
func (s *Server) handleSecurityBans(w http.ResponseWriter, _ *http.Request) {
	out := []securityBanView{}
	for _, b := range s.db.ListActiveLoginBans() {
		u := int64(0)
		if !b.Until.IsZero() {
			u = b.Until.Unix()
		}
		out = append(out, securityBanView{IP: b.IP, Source: b.Source, Until: u, Reason: b.Reason})
	}
	for _, l := range s.sessions.lockedIPs() {
		out = append(out, securityBanView{
			IP: l.IP, Source: "auto", Until: l.Until.Unix(),
			Reason: fmt.Sprintf("连续 %d 次登录失败", maxLoginFails),
		})
	}
	jsonOK(w, out)
}

// handleSecurityBanCreate 手动封禁一个 IP（hours<=0 表示永久）。
func (s *Server) handleSecurityBanCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IP     string `json:"ip"`
		Hours  int    `json:"hours"`
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		jsonErr(w, fmt.Errorf("请求体无法解析"), http.StatusBadRequest)
		return
	}
	p := net.ParseIP(strings.TrimSpace(req.IP))
	if p == nil {
		jsonErr(w, fmt.Errorf("IP 格式不正确"), http.StatusBadRequest)
		return
	}
	ip := p.String()
	// 自锁保护：禁止封禁当前请求的来源 IP（防止把自己挡在面板外）
	if ip == s.clientIP(r) {
		jsonErr(w, fmt.Errorf("不能封禁当前来源 IP"), http.StatusBadRequest)
		return
	}
	// hours 边界：负数语义不明（会意外变永久），过大值经 time.Duration 乘法会
	// int64 溢出回绕成过去时间（封禁立即失效）。上限 87600h = 100 年。
	if req.Hours < 0 || req.Hours > 87600 {
		jsonErr(w, fmt.Errorf("时长不合法（0=永久，1~87600 小时）"), http.StatusBadRequest)
		return
	}
	var until time.Time
	if req.Hours > 0 {
		until = time.Now().Add(time.Duration(req.Hours) * time.Hour)
	}
	// reason 是攻击者/管理员都可能写长的一面，限制长度防 DB 膨胀
	if len(req.Reason) > 200 {
		jsonErr(w, fmt.Errorf("原因最长 200 字符"), http.StatusBadRequest)
		return
	}
	if err := s.db.InsertLoginBan(ip, strings.TrimSpace(req.Reason), until); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	desc := "永久"
	if req.Hours > 0 {
		desc = fmt.Sprintf("%d 小时", req.Hours)
	}
	s.db.InsertLoginLog("manual_ban", ip, "", "手动封禁（"+desc+"）")
	jsonOK(w, map[string]bool{"ok": true})
}

// handleSecurityBanDelete 解除封禁：同时清掉手动封禁记录与内存自动锁。
func (s *Server) handleSecurityBanDelete(w http.ResponseWriter, r *http.Request) {
	p := net.ParseIP(r.PathValue("ip"))
	if p == nil {
		jsonErr(w, fmt.Errorf("IP 格式不正确"), http.StatusBadRequest)
		return
	}
	// 归一化后再删：写入端存的是 ParseIP().String() 的规范形式，
	// 非规范输入（如 2001:DB8::1）不归一化会删不到行，造成解封假成功。
	ip := p.String()
	s.db.DeleteLoginBan(ip)
	s.sessions.clearLock(ip)
	s.db.InsertLoginLog("unban", ip, "", "管理员解封")
	w.WriteHeader(http.StatusNoContent)
}

// handleSession 返回当前登录用户（请求已经 authMiddleware 校验）。
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	user, _ := r.Context().Value(sessionUserKey).(string)
	jsonOK(w, map[string]string{"user": user})
}

// authedFromCookie 判断请求是否携带有效登录会话（供首帧 HTML 渲染分支用）。
// 未配置登录用户名时面板无鉴权，等同已登录；hash 异常为空时 validate 必然失败 → 未登录。
func (s *Server) authedFromCookie(r *http.Request) bool {
	conf := s.cfg.Get()
	if conf.WebUser == "" {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	_, ok := s.sessions.validate(c.Value)
	return ok
}

// handleBrandPublic 公开返回站点品牌信息（名称 + 图标版本），供登录页在
// 未登录状态下展示真实品牌。不含任何敏感字段。
func (s *Server) handleBrandPublic(w http.ResponseWriter, _ *http.Request) {
	conf := s.cfg.Get()
	_, iconMtime := s.brandIconFile()
	jsonOK(w, map[string]any{
		"site_name": normalizeSiteName(conf.SiteName),
		"site_icon": iconMtime,
	})
}

// ─── 辅助 ──────────────────────────────────────────────────────────────────────

// staticContentType 按扩展名返回 Content-Type。
// 面板的 CSS/JS 若被当作 text/plain 返回，浏览器会拒绝应用样式（严格 MIME 校验）。
func staticContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	default:
		return "application/octet-stream"
	}
}

// normalizeSiteName 归一化面板品牌名：去空白、限长 24 字符、空值回落默认。
func normalizeSiteName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return config.DefaultSiteName
	}
	if r := []rune(name); len(r) > 24 {
		name = string(r[:24])
	}
	return name
}

// ─── 品牌图标 ──────────────────────────────────────────────────────────────────

const maxBrandIconSize = 256 << 10 // 256 KB

// 固定文件名：换图标时删掉旧的，避免同扩展名残留两份。
var brandIconNames = []string{"brand-icon.png", "brand-icon.jpg", "brand-icon.webp", "brand-icon.svg", "brand-icon.ico"}

// brandIconFile 返回当前自定义图标的路径与其修改时间（无图标返回空串、0）。
// 修改时间用作前端缓存键，换图标后 URL 自动变化。
func (s *Server) brandIconFile() (string, int64) {
	dir := s.cfg.Dir()
	if dir == "" {
		return "", 0
	}
	for _, name := range brandIconNames {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, st.ModTime().Unix()
		}
	}
	return "", 0
}

func brandIconContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png":
		return "image/png"
	case ".jpg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

// sniffBrandIcon 用文件头判断类型，不信任客户端声明的 MIME。
func sniffBrandIcon(b []byte) (string, bool) {
	switch {
	case len(b) > 8 && bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return ".png", true
	case len(b) > 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return ".jpg", true
	case len(b) > 12 && bytes.HasPrefix(b, []byte("RIFF")) && string(b[8:12]) == "WEBP":
		return ".webp", true
	case len(b) > 4 && b[0] == 0x00 && b[1] == 0x00 && b[2] == 0x01 && b[3] == 0x00:
		return ".ico", true
	}
	head := strings.ToLower(string(bytes.TrimLeft(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf}), " \t\r\n")))
	if strings.HasPrefix(head, "<svg") || (strings.HasPrefix(head, "<?xml") && strings.Contains(head, "<svg")) {
		return ".svg", true
	}
	return "", false
}

// writeBrandIcon 把自定义图标写进响应；无图标或读取失败时返回 false，由调用方决定回落。
func (s *Server) writeBrandIcon(w http.ResponseWriter, r *http.Request) bool {
	file, mtime := s.brandIconFile()
	if file == "" {
		return false
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	etag := fmt.Sprintf(`"%x-%x"`, mtime, len(data))
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	w.Header().Set("Content-Type", brandIconContentType(file))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// 图标被单独打开时（SVG 可带脚本），用 CSP 掐断一切外部与脚本能力。
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", etag)
	_, _ = w.Write(data)
	return true
}

// handleFavicon 让浏览器标签图标跟随自定义站点图标；未配置时回落到内置 favicon。
func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	if s.writeBrandIcon(w, r) {
		return
	}
	data, err := staticFiles.ReadFile("static/favicon.ico")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/x-icon")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}

// handleBrandIcon 负责自定义站点图标的读取、上传与重置。
func (s *Server) handleBrandIcon(w http.ResponseWriter, r *http.Request) {
	dir := s.cfg.Dir()
	if dir == "" {
		jsonErr(w, fmt.Errorf("配置目录不可用"), 500)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if !s.writeBrandIcon(w, r) {
			http.NotFound(w, r)
		}

	case http.MethodPost:
		var req struct {
			Data string `json:"data"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBrandIconSize*2)).Decode(&req); err != nil {
			jsonErr(w, fmt.Errorf("请求体无法解析: %v", err), 400)
			return
		}
		raw := req.Data
		if i := strings.Index(raw, "base64,"); i >= 0 {
			raw = raw[i+len("base64,"):]
		}
		raw = strings.TrimSpace(raw)
		if raw == "" {
			jsonErr(w, fmt.Errorf("图标数据为空"), 400)
			return
		}
		data, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			jsonErr(w, fmt.Errorf("图标数据不是合法 base64"), 400)
			return
		}
		if len(data) == 0 || len(data) > maxBrandIconSize {
			jsonErr(w, fmt.Errorf("图标需小于 256 KB"), 400)
			return
		}
		ext, ok := sniffBrandIcon(data)
		if !ok {
			jsonErr(w, fmt.Errorf("只支持 PNG / JPEG / WebP / SVG / ICO"), 400)
			return
		}
		for _, name := range brandIconNames {
			_ = os.Remove(filepath.Join(dir, name))
		}
		path := filepath.Join(dir, "brand-icon"+ext)
		if err := os.WriteFile(path, data, 0644); err != nil {
			jsonErr(w, fmt.Errorf("写入图标失败: %v", err), 500)
			return
		}
		jsonOK(w, map[string]any{"ok": true, "site_icon": time.Now().Unix()})

	case http.MethodDelete:
		for _, name := range brandIconNames {
			_ = os.Remove(filepath.Join(dir, name))
		}
		jsonOK(w, map[string]any{"ok": true, "site_icon": 0})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, err error, code int) {
	http.Error(w, err.Error(), code)
}

// vmErrStatus 把驱动层哨兵错误映射成合适的 HTTP 状态码。
// 之前所有失败一律 500，导致「实例不存在」「同名重复」「参数越界」都无法与真正的
// 服务端故障区分；调用方无法据此决定重试还是改参数。
func vmErrStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, manager.ErrVMNotFound):
		return http.StatusNotFound
	case errors.Is(err, manager.ErrVMExists):
		return http.StatusConflict
	case errors.Is(err, manager.ErrInvalidArgument):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// ─── 监控 TSDB ─────────────────────────────────────────────────────────────────

// vmCreatedAtUnix 返回 VM 的真实创建时间（Unix 秒）；无法获取时返回 0，前端显示「—」。
func (s *Server) vmCreatedAtUnix(vmID string) int64 {
	t, err := s.mgr.GetVMCreatedAt(context.Background(), vmID)
	if err != nil || t.IsZero() {
		return 0
	}
	return t.Unix()
}

func (s *Server) startMonitorLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		vms, err := s.mgr.ListVMs(ctx)
		if err != nil {
			cancel()
			continue
		}

		s.vmSnapshotMu.Lock()
		now := time.Now()
		var items []vmListItem

		for _, vm := range vms {
			traffic, _ := s.db.GetTraffic(vm.VmId)

			item := vmListItem{
				VmId:      vm.VmId,
				Status:    int32(vm.Status),
				CpuPct:    vm.CpuPct,
				RamUsedMb: vm.RamUsedMb,
				Ips:       vm.Ips,
				CreatedAt: s.vmCreatedAtUnix(vm.VmId),
			}

			if traffic != nil {
				item.MonthlyTrafficIn = traffic.MonthIn
				item.MonthlyTrafficOut = traffic.MonthOut
			}

			if lastSnapshot, exists := s.vmSnapshots[vm.VmId]; exists {
				elapsed := now.Sub(lastSnapshot.timestamp).Seconds()
				if elapsed > 0.1 {
					deltaIn := vm.TrafficInBytes - lastSnapshot.inBytes
					deltaOut := vm.TrafficOutBytes - lastSnapshot.outBytes
					if deltaIn < 0 {
						deltaIn = 0
					}
					if deltaOut < 0 {
						deltaOut = 0
					}
					item.NetInBps = int64(float64(deltaIn) / elapsed)
					item.NetOutBps = int64(float64(deltaOut) / elapsed)
				}
			}

			s.vmSnapshots[vm.VmId] = &vmNetSnapshot{
				inBytes:   vm.TrafficInBytes,
				outBytes:  vm.TrafficOutBytes,
				timestamp: now,
			}

			if conf, _ := s.db.GetVMConfig(vm.VmId); conf != nil {
				item.RamTotalMb = conf.MemoryMB
			}
			items = append(items, item)
		}
		s.vmSnapshotMu.Unlock()
		cancel()

		s.monitorMu.Lock()
		// 获取最新存活的 VMID 以便清理僵尸数据
		alive := make(map[string]bool)
		for _, item := range items {
			alive[item.VmId] = true
			hst := s.monitorHst[item.VmId]
			hst = append(hst, item)
			if len(hst) > 80 {
				hst = hst[len(hst)-80:] // 保留最近 80 个点
			}
			s.monitorHst[item.VmId] = hst
		}
		// 清理已删除的 VM 历史数据
		for k := range s.monitorHst {
			if !alive[k] {
				delete(s.monitorHst, k)
			}
		}
		s.monitorMu.Unlock()
	}
}

func (s *Server) handleMonitorLatest(w http.ResponseWriter, r *http.Request) {
	s.monitorMu.RLock()
	defer s.monitorMu.RUnlock()

	var latest []vmListItem
	for _, history := range s.monitorHst {
		if len(history) > 0 {
			latest = append(latest, history[len(history)-1])
		}
	}
	jsonOK(w, latest)
}

func (s *Server) handleMonitorHistory(w http.ResponseWriter, r *http.Request) {
	vmID := r.URL.Query().Get("vm_id")
	if vmID == "" {
		http.Error(w, "vm_id required", 400)
		return
	}

	s.monitorMu.RLock()
	defer s.monitorMu.RUnlock()

	history := s.monitorHst[vmID]
	if history == nil {
		history = []vmListItem{}
	}
	jsonOK(w, history)
}

// ─── 系统 ──────────────────────────────────────────────────────────────────────

type hostStatusResponse struct {
	*monitor.HostStats
	MonthInTotal  int64 `json:"month_in_total"`
	MonthOutTotal int64 `json:"month_out_total"`
	// Speed 是最近一次测速结果；从未测出过为 null。
	// 心跳里的 bandwidth_mbps 是它的 min(down, up)，这里给出明细供面板展示。
	Speed        *monitor.SpeedResult `json:"speed"`
	SpeedTesting bool                 `json:"speed_testing"`
	SpeedError   string               `json:"speed_error,omitempty"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	conf := s.cfg.Get()
	ctx := context.WithValue(r.Context(), monitor.NICKey, conf.MonitorNIC)
	ctx = context.WithValue(ctx, monitor.DiskKey, conf.MonitorDisk)

	stats, err := s.hostMon.GetStats(ctx)
	if err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}

	// 汇总所有 VM 的月度流量
	vms, _ := s.mgr.ListVMs(ctx)
	var mIn, mOut int64
	for _, vm := range vms {
		if t, err := s.db.GetTraffic(vm.VmId); err == nil {
			mIn += t.MonthIn
			mOut += t.MonthOut
		}
	}

	resp := hostStatusResponse{
		HostStats:     stats,
		MonthInTotal:  mIn,
		MonthOutTotal: mOut,
	}
	if speed, ok := s.hostMon.Speed(); ok {
		resp.Speed = &speed
	}
	if s.speedTester != nil {
		var lastErr string
		resp.SpeedTesting, lastErr = s.speedTester.Status()
		if !resp.SpeedTesting {
			resp.SpeedError = lastErr
		}
	}

	jsonOK(w, resp)
}

func (s *Server) handleSystemInfo(w http.ResponseWriter, _ *http.Request) {
	nics, _ := psnet.Interfaces()
	parts, _ := disk.Partitions(false)

	var nicNames []string
	for _, n := range nics {
		nicNames = append(nicNames, n.Name)
	}
	var mountPoints []string
	for _, p := range parts {
		mountPoints = append(mountPoints, p.Mountpoint)
	}
	jsonOK(w, map[string]any{
		"nics":         nicNames,
		"disks":        mountPoints,
		"primary_nic":  defaultRouteIface(),
		"primary_disk": "/",
	})
}

// defaultRouteIface 读取 /proc/net/route 里 Destination=00000000 的默认路由网卡名。
func defaultRouteIface() string {
	f, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if sc.Scan() { /* 跳过表头 */
	}
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 8 && fields[1] == "00000000" {
			return fields[0]
		}
	}
	return ""
}

type configRequest struct {
	Token            string  `json:"token"`
	MonitorNIC       string  `json:"monitor_nic"`
	MonitorDisk      string  `json:"monitor_disk"`
	WebUser          string  `json:"web_user"`
	WebPass          string  `json:"web_pass"` // plaintext，服务端 bcrypt 后存储
	SiteName         *string `json:"site_name"`
	Host             string  `json:"host"`
	MaxPortForward   int32   `json:"max_port_forward"`
	MaxForwardIPs    *int32  `json:"max_forward_ips"` // 指针以区分"未提交"与"设为 0（不限制）"
	TrafficResetDay  int     `json:"traffic_reset_day"`
	TrafficResetMode string  `json:"traffic_reset_mode"` // "monthly" / "rental"

	// 接入点配置。指针用于区分"未提交"（保持原值）与"提交空串"（回落到内置默认），
	// 保证不认识这些字段的旧客户端调用 POST /api/config 时行为完全不变。
	PlatformAddr *string `json:"platform_addr"`
	// nil = 未提交（保持原值）；false = 显式关闭平台接入（独立模式）。
	PlatformEnabled *bool   `json:"platform_enabled"`
	UpdateRepo      *string `json:"update_repo"`
	UpdateBaseURL   *string `json:"update_base_url"`
	UpdateAPIURL    *string `json:"update_api_url"`
	// 下载源安全开关：默认关闭，放行明文 http 与内网地址需显式开启。
	AllowInsecureDownload *bool `json:"allow_insecure_download"`
	AllowInternalDownload *bool `json:"allow_internal_download"`
	// nil = 未提交；false = 显式关闭，因此用指针。
	PlatformInsecure *bool   `json:"platform_insecure"`
	NetavarkBase     *string `json:"netavark_base"`
	// 容器对外 IPv4：端口映射监听该地址，出站 SNAT 也用它。空串表示恢复默认。
	PublicIPv4 *string `json:"public_ipv4"`

	// 上报给平台的入口 IPv6：纯对外宣称，不参与监听/SNAT，改了零中断。
	// 空串 = 跟随自动取值（共享 IPv6 出口地址或探测值）；指针用于区分"未提交"。
	HostIPv6 *string `json:"host_ipv6"`

	// 共享 IPv6 的对外地址（snat 模式）：端口映射监听它、出站 SNAT 也用它。
	// 空串 = 回落安装脚本探测的 ipv6_addr；指针用于区分"未提交"。
	PublicIPv6 *string `json:"public_ipv6"`

	// 容器网络隔离。指针用于区分"未提交"（保持原值）与"显式 false"。
	ContainerIsolate      *bool   `json:"container_isolate"`
	TrustProxy            *bool   `json:"trust_proxy"`
	HostAllowPorts        *string `json:"host_allow_ports"`        // 逗号分隔端口，空串 = 清空
	ContainerAllowSubnets *string `json:"container_allow_subnets"` // 逗号分隔 CIDR，空串 = 清空

	// 巡查与通知渠道配置。指针区分"未提交"（保持原值，兼容旧客户端）与提交；
	// 提交即整体替换，面板每次都带全量字段。服务端 normalize+validate 后写入。
	Patrol   *config.PatrolConfig   `json:"patrol"`
	Telegram *config.TelegramConfig `json:"telegram"`
}

type configResponse struct {
	// 只回传"是否已配置"，不回传 Token 本身。凭据一旦进入响应就会落到浏览器
	// 内存与开发者工具里，面板并不需要它的明文——留空即"不修改"。
	HasToken         bool   `json:"has_token"`
	MonitorNIC       string `json:"monitor_nic"`
	MonitorDisk      string `json:"monitor_disk"`
	WebUser          string `json:"web_user"`
	SiteName         string `json:"site_name"`
	SiteIcon         int64  `json:"site_icon"` // 自定义图标的修改时间，0 表示用内置图标
	VirtType         string `json:"virt_type"`
	Host             string `json:"host"`
	MaxPortForward   int32  `json:"max_port_forward"`
	MaxForwardIPs    int32  `json:"max_forward_ips"`
	TrafficResetDay  int    `json:"traffic_reset_day"`
	TrafficResetMode string `json:"traffic_reset_mode"`
	// 空串表示走内置默认，面板据此显示 placeholder。
	PlatformAddr     string `json:"platform_addr"`
	PlatformEnabled  bool   `json:"platform_enabled"`
	UpdateRepo       string `json:"update_repo"`
	UpdateBaseURL    string `json:"update_base_url"`
	UpdateAPIURL     string `json:"update_api_url"`
	PlatformInsecure bool   `json:"platform_insecure"`
	NetavarkBase     string `json:"netavark_base"`
	PublicIPv4       string `json:"public_ipv4"`
	// DetectedIPv4 是后端自动探测到的公网 IPv4（只读，不可配置）。
	// 前端「入口地址」在 host / public_ipv4 都为空时用它，避免回落到浏览器
	// 地址栏（SSH 隧道访问时会显示成 127.0.0.1）。
	DetectedIPv4 string `json:"detected_ipv4"`
	// HostIPv6 是用户手填的入口 IPv6（空 = 跟随自动取值），用于回填输入框。
	// 与下面 entry_ipv6（最终生效值）不同，二者都要回传：
	// 前者是配置，后者是算出来的结果，混用会让「自动」被固化成「手动」。
	// DetectedIPv6 在下方「共享 IPv6」段声明。
	HostIPv6 string `json:"host_ipv6"`
	// EntryAddr 是最终生效的入口地址（host > 公网性质的 public_ipv4 > 自动探测），
	// 与心跳 entry_host 同口径。前端直接展示它，避免把 1:1 NAT 机器上的内网
	// 监听地址当成用户可访问的入口。
	EntryAddr string `json:"entry_addr"`
	// EntrySource 说明 EntryAddr 的来源：manual / public_ipv4 / auto / ""。
	// 面板用它渲染「当前生效值 · 来源」，就不必再用文字解释优先级顺序。
	EntrySource string `json:"entry_source"`
	// EntryIPv6 是上报/展示用的入口 IPv6，EntryIPv6Source 说明来源：
	// config（配置的共享 IPv6）/ auto（自动探测）/ stale（配置地址已不在网卡上，
	// 改用探测值）/ ""。
	EntryIPv6       string `json:"entry_ipv6"`
	EntryIPv6Source string `json:"entry_ipv6_source"`

	// 共享 IPv6（仅 snat 模式有意义）：PublicIPv6 是可配置的对外地址，
	// IPv6Addr 是安装脚本探测写入的地址（只读），SharedIPv6 是最终生效值
	// （public_ipv6 优先，回落 ipv6_addr），供前端展示与映射表单判断。
	PublicIPv6   string `json:"public_ipv6"`
	IPv6Mode     string `json:"ipv6_mode"`
	IPv6Addr     string `json:"ipv6_addr"`
	SharedIPv6   string `json:"shared_ipv6"`
	DetectedIPv6 string `json:"detected_ipv6"`

	AllowInsecureDownload bool `json:"allow_insecure_download"`
	AllowInternalDownload bool `json:"allow_internal_download"`

	ContainerIsolate      bool     `json:"container_isolate"`
	HostAllowPorts        []int    `json:"host_allow_ports"`
	ContainerAllowSubnets []string `json:"container_allow_subnets"`
	TrustProxy            bool     `json:"trust_proxy"`

	// 巡查与通知渠道：并入主配置响应，前端单次提交即可保存全部。
	// BotToken 不回传明文（与主 Token 的 has_token 基线一致）：Telegram 里
	// bot_token 恒为空串，是否已配置看 TelegramHasToken；提交留空即"不修改"。
	Patrol           config.PatrolConfig   `json:"patrol"`
	Telegram         config.TelegramConfig `json:"telegram"`
	TelegramHasToken bool                  `json:"telegram_has_token"`
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var req configRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// 负数没有「不限制」的语义（0 才是），静默忽略会让调用方以为保存成功。
		if req.MaxForwardIPs != nil && *req.MaxForwardIPs < 0 {
			jsonErr(w, fmt.Errorf("max_forward_ips 不能为负数（0 表示不限制）"), http.StatusBadRequest)
			return
		}
		if req.MaxPortForward < 0 {
			jsonErr(w, fmt.Errorf("max_port_forward 不能为负数"), http.StatusBadRequest)
			return
		}
		// v0.4.1 用 Basic Auth，按首个冒号切分 "user:pass"：用户名里含冒号
		// 会让降级后永远登录不上（Q5）。新版是 cookie 会话不受影响，但配置是
		// 共用的，这里直接拒绝生成这种用户名，避免回滚时踩坑。
		if req.WebUser != "" && strings.Contains(req.WebUser, ":") {
			jsonErr(w, fmt.Errorf("web_user 不能包含冒号"), http.StatusBadRequest)
			return
		}
		// 密码在改配置前预生成：bcrypt 对超 72 字节密码报错，若在 Update 回调里
		// 吞掉错误会把空 hash 写进配置 → 面板 fail-open 全裸。这里失败直接 400。
		var newPassHash string
		if req.WebPass != "" {
			if len(req.WebPass) > 72 {
				jsonErr(w, fmt.Errorf("密码最长 72 个字符"), http.StatusBadRequest)
				return
			}
			h, err := bcrypt.GenerateFromPassword([]byte(req.WebPass), bcrypt.DefaultCost)
			if err != nil {
				jsonErr(w, fmt.Errorf("密码无法使用: %v", err), http.StatusBadRequest)
				return
			}
			newPassHash = string(h)
		}
		// 巡查段先于 UpdateChecked 校验：normalize+validate 失败直接 400，
		// 不落盘、不改内存值；校验通过后回调里纯写入。
		if req.Patrol != nil {
			normalizePatrol(req.Patrol)
			if err := validatePatrol(req.Patrol); err != nil {
				jsonErr(w, err, http.StatusBadRequest)
				return
			}
		}
		// 原子更新配置文件
		credChanged := false
		// UpdateChecked：fn 可返回错误，校验失败时不落盘、不改内存值。
		err := s.cfg.UpdateChecked(func(cfg *config.Config) error {
			if req.Token != "" {
				cfg.Token = req.Token
			}
			if req.MonitorNIC != "" {
				cfg.MonitorNIC = req.MonitorNIC
			}
			if req.MonitorDisk != "" {
				cfg.MonitorDisk = req.MonitorDisk
			}
			// Host 允许清空（空字符串 = 恢复自动检测公网 IP），因此无论是否为空都写入。
			cfg.Host = req.Host
			// 入口 IPv6 同样是「空 = 跟随自动取值」，纯上报字段，不做网卡校验。
			if req.HostIPv6 != nil {
				cfg.HostIPv6 = strings.TrimSpace(*req.HostIPv6)
			}
			if req.MaxPortForward > 0 {
				cfg.MaxPortForward = req.MaxPortForward
			}
			if req.MaxForwardIPs != nil && *req.MaxForwardIPs >= 0 {
				cfg.MaxForwardIPs = *req.MaxForwardIPs
			}
			if req.WebUser != "" && req.WebUser != cfg.WebUser {
				cfg.WebUser = req.WebUser
				credChanged = true
			}
			if req.SiteName != nil {
				cfg.SiteName = normalizeSiteName(*req.SiteName)
			}
			if newPassHash != "" {
				cfg.WebPassHash = newPassHash
				credChanged = true
			}
			if req.TrafficResetDay > 0 && req.TrafficResetDay <= 28 {
				cfg.TrafficResetDay = req.TrafficResetDay
			}
			if req.TrafficResetMode == "monthly" || req.TrafficResetMode == "rental" {
				cfg.TrafficResetMode = req.TrafficResetMode
			}
			if req.PlatformAddr != nil {
				cfg.PlatformAddr = strings.TrimSpace(*req.PlatformAddr)
			}
			if req.PlatformEnabled != nil {
				enabled := *req.PlatformEnabled
				cfg.PlatformEnabled = &enabled
			}
			if req.UpdateRepo != nil {
				cfg.UpdateRepo = strings.TrimSpace(*req.UpdateRepo)
			}
			// 下载源校验：这几个字段指向的内容会以 root 身份执行
			// （updater 用 systemd-run 跑 install.sh，netavark 二进制直接落地），
			// 校验失败必须让整个更新失败，不能静默写入。
			if req.AllowInsecureDownload != nil {
				cfg.AllowInsecureDownload = *req.AllowInsecureDownload
			}
			if req.AllowInternalDownload != nil {
				cfg.AllowInternalDownload = *req.AllowInternalDownload
			}
			if req.UpdateBaseURL != nil {
				v := strings.TrimSpace(*req.UpdateBaseURL)
				if err := config.ValidateDownloadBase(v, cfg.AllowInsecureDownload, cfg.AllowInternalDownload); err != nil {
					return err
				}
				cfg.UpdateBaseURL = v
			}
			if req.UpdateAPIURL != nil {
				v := strings.TrimSpace(*req.UpdateAPIURL)
				if err := config.ValidateDownloadBase(v, cfg.AllowInsecureDownload, cfg.AllowInternalDownload); err != nil {
					return err
				}
				cfg.UpdateAPIURL = v
			}
			if req.PlatformInsecure != nil {
				cfg.PlatformInsecure = *req.PlatformInsecure
			}
			if req.NetavarkBase != nil {
				v := strings.TrimSpace(*req.NetavarkBase)
				if err := config.ValidateDownloadBase(v, cfg.AllowInsecureDownload, cfg.AllowInternalDownload); err != nil {
					return err
				}
				cfg.NetavarkBase = v
			}
			if req.PublicIPv4 != nil {
				v := strings.TrimSpace(*req.PublicIPv4)
				// 端口转发直接 bind 这个地址：不在本机网卡上时 bind 会 EADDRNOTAVAIL，
				// 该机器的映射全部失败。云厂商 1:1 NAT 的弹性 IP 属于这种地址，
				// 它只用于入口展示（由 entry_addr 自动得出），不该填在这里。
				// 已存量的旧值放行，否则这类机器连其它字段都改不了，只能先清空。
				if v != "" && v != strings.TrimSpace(cfg.PublicIPv4) && !hostHasIPv4(v) {
					return fmt.Errorf("public_ipv4 %s 不在本机网卡上，无法用于端口监听；云厂商 1:1 NAT 的弹性 IP 请留空", v)
				}
				cfg.PublicIPv4 = v
			}
			if req.PublicIPv6 != nil {
				v := strings.TrimSpace(*req.PublicIPv6)
				if v != "" {
					ip := net.ParseIP(v)
					if ip == nil || ip.To4() != nil {
						return fmt.Errorf("public_ipv6 必须是合法的 IPv6 地址")
					}
					v = ip.String()
					// 端口映射直接 bind 这个地址：地址不在本机时该 VM 的所有 v6
					// 映射都会 bind 失败并整体回滚（连双栈的 v4 侧一起没了），
					// 而 egress 侧不报错，面板上看不出根因。保存前就拦下来。
					if !hostHasIPv6(v) {
						return fmt.Errorf("public_ipv6 %s 不在本机网卡上，请先在系统中配置该地址，或留空以回落到安装时探测的地址", v)
					}
				}
				cfg.PublicIPv6 = v
			}
			if req.ContainerIsolate != nil {
				cfg.ContainerIsolate = req.ContainerIsolate
			}
			if req.TrustProxy != nil {
				cfg.TrustProxy = *req.TrustProxy
			}
			if req.HostAllowPorts != nil {
				cfg.HostAllowPorts = config.ParsePortList(*req.HostAllowPorts)
			}
			if req.ContainerAllowSubnets != nil {
				cfg.ContainerAllowSubnets = config.ParseSubnetList(*req.ContainerAllowSubnets)
			}
			if req.Patrol != nil {
				cfg.Patrol = *req.Patrol
			}
			if req.Telegram != nil {
				tg := *req.Telegram
				// bot_token 留空 = 不修改：GET 从不回传明文，前端输入框恒为空，
				// 整体替换语义下空值必须回落到原值，否则每次保存都会清掉 Token。
				// 停用通知走 enabled 开关，没有"清空 token"的合法场景。
				if tg.BotToken == "" {
					tg.BotToken = cfg.Telegram.BotToken
				}
				// 逐事件开关 nil = 未提交（旧版缓存页不认识这些字段，提交体里没有）：
				// 保留现有值，否则旧页一次保存会把用户已按事件关掉的推送全部重开。
				if tg.PatrolNotify == nil {
					tg.PatrolNotify = cfg.Telegram.PatrolNotify
				}
				if tg.LoginOkNotify == nil {
					tg.LoginOkNotify = cfg.Telegram.LoginOkNotify
				}
				if tg.AutoBanNotify == nil {
					tg.AutoBanNotify = cfg.Telegram.AutoBanNotify
				}
				if tg.LoginBlockedNotify == nil {
					tg.LoginBlockedNotify = cfg.Telegram.LoginBlockedNotify
				}
				cfg.Telegram = tg
			}
			return nil
		})
		if err != nil {
			// 下载源校验失败属于请求参数错误（400），不是服务端故障。
			if errors.Is(err, config.ErrInvalidDownloadBase) {
				jsonErr(w, err, http.StatusBadRequest)
				return
			}
			http.Error(w, "failed to save config: "+err.Error(), 500)
			return
		}
		// 凭证变更且持久化成功后才踢掉全部已登录会话，强制重新登录；
		// 放在 Update 之外，避免落盘失败时误踢（内存态可能未生效）。
		if credChanged {
			s.sessions.destroyAll()
		}
		w.WriteHeader(200)
		return
	}
	conf := s.cfg.Get()
	_, iconMtime := s.brandIconFile()
	// 入口 IPv6 与心跳同口径：手动填写的入口优先，其次共享 IPv6 出口地址，
	// 配置地址已不在网卡上时回落探测值。非 podman 驱动不支持共享 IPv6 语义，
	// 只允许手动覆盖，其余保持原有「上报探测值」行为。
	detected6 := s.detectedIPv6()
	entryV6, entryV6Source := detected6, "auto"
	if entryV6 == "" {
		entryV6Source = ""
	}
	if conf.VirtType == "podman" {
		entryV6, entryV6Source = conf.EntryIPv6(detected6, config.HostHasIPv6)
	} else if h := strings.TrimSpace(conf.HostIPv6); h != "" {
		entryV6, entryV6Source = h, "manual"
	}
	// BotToken 在副本上抹掉明文再回传：响应体、浏览器内存、抓包日志都不落明文。
	tg := conf.Telegram
	tg.BotToken = ""
	jsonOK(w, configResponse{
		HasToken:         conf.Token != "",
		MonitorNIC:       conf.MonitorNIC,
		MonitorDisk:      conf.MonitorDisk,
		WebUser:          conf.WebUser,
		SiteName:         conf.SiteName,
		SiteIcon:         iconMtime,
		VirtType:         conf.VirtType,
		Host:             conf.Host,
		MaxPortForward:   conf.MaxPortForward,
		MaxForwardIPs:    conf.MaxForwardIPs,
		TrafficResetDay:  conf.TrafficResetDay,
		TrafficResetMode: conf.TrafficResetMode,
		PlatformAddr:     conf.PlatformAddr,
		PlatformEnabled:  conf.PlatformEnabledOn(),
		UpdateRepo:       conf.UpdateRepo,
		UpdateBaseURL:    conf.UpdateBaseURL,
		UpdateAPIURL:     conf.UpdateAPIURL,
		PlatformInsecure: conf.PlatformInsecure,
		NetavarkBase:     conf.NetavarkBase,
		PublicIPv4:       conf.PublicIPv4,
		DetectedIPv4:     s.detectedIPv4(),
		HostIPv6:         conf.HostIPv6,
		EntryAddr:        conf.EntryHost(s.detectedIPv4()),
		EntrySource:      conf.EntryHostSource(s.detectedIPv4()),
		EntryIPv6:        entryV6,
		EntryIPv6Source:  entryV6Source,
		PublicIPv6:       conf.PublicIPv6,
		IPv6Mode:         conf.IPv6Mode,
		IPv6Addr:         conf.IPv6Addr,
		SharedIPv6:       conf.EgressIPv6(),
		DetectedIPv6:     s.detectedIPv6(),

		AllowInsecureDownload: conf.AllowInsecureDownload,
		AllowInternalDownload: conf.AllowInternalDownload,

		ContainerIsolate:      conf.IsolateEnabled(),
		HostAllowPorts:        conf.AllowedHostPorts(),
		ContainerAllowSubnets: conf.AllowedSubnets(),
		TrustProxy:            conf.TrustProxy,

		// 巡查与通知渠道并入主配置响应：前端单次提交 /api/config 即可保存全部。
		// BotToken 脱敏回传（副本上抹掉明文），前端输入框恒为空、留空提交即不修改。
		Patrol:           conf.Patrol,
		Telegram:         tg,
		TelegramHasToken: conf.Telegram.BotToken != "",
	})
}

// detectedIPv4 取后端自动探测的公网 IPv4；agent 未提供时返回空。
func (s *Server) detectedIPv4() string {
	if s.agent == nil {
		return ""
	}
	if p, ok := s.agent.(interface{ DetectedIPv4() string }); ok {
		return p.DetectedIPv4()
	}
	return ""
}

// detectedIPv6 取后端自动探测的公网 IPv6；agent 未提供时返回空。
func (s *Server) detectedIPv6() string {
	if s.agent == nil {
		return ""
	}
	if p, ok := s.agent.(interface{ DetectedIPv6() string }); ok {
		return p.DetectedIPv6()
	}
	return ""
}

func (s *Server) handleConnection(w http.ResponseWriter, _ *http.Request) {
	var connected bool
	var errMsg string
	if s.agent != nil {
		if a, ok := s.agent.(interface{ GetConnStatus() (bool, string) }); ok {
			connected, errMsg = a.GetConnStatus()
		}
	}
	// mode 区分四种状态，前端据此渲染胶囊：
	//   standalone   —— 用户显式关闭平台接入，中性显示，不是故障
	//   connected    —— 已连上平台
	//   no_token     —— 开关开着但没有 Token，无法连接
	//   disconnected —— 有 Token 但没连上（网络 / TLS / 认证失败等），属故障
	// 开关优先于实际连接状态：刚关掉的 10 秒窗口内连接可能还在，但用户意图已是独立模式。
	conf := s.cfg.Get()
	mode := "disconnected"
	switch {
	case !conf.PlatformEnabledOn():
		mode = "standalone"
		errMsg = ""
	case connected:
		mode = "connected"
	case conf.Token == "":
		mode = "no_token"
	}
	jsonOK(w, map[string]interface{}{
		"connected": connected,
		"mode":      mode,
		"error":     errMsg,
	})
}

// handleClearPlatformToken 清除平台 Token 并同时关闭平台接入。
//
// 单独开一个接口而不是复用 POST /api/config：那里的 token 字段是"空串 = 未修改"
// 的语义（避免前端没填就抹掉凭据），无法表达"我要清空"。清除是显式动作，
// 语义上等于退出平台接入，因此顺带把平台开关关掉，不留"开着开关但没有凭据"的半配状态。
func (s *Server) handleClearPlatformToken(w http.ResponseWriter, _ *http.Request) {
	err := s.cfg.UpdateChecked(func(cfg *config.Config) error {
		cfg.Token = ""
		disabled := false
		cfg.PlatformEnabled = &disabled
		return nil
	})
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]interface{}{"ok": true, "mode": "standalone"})
}

// handleHostFWStatus 返回容器网络隔离规则的生效状态。
// 前端设置页据此在隔离失效时给出可见提示，避免 fail-open 静默发生。
func (s *Server) handleHostFWStatus(w http.ResponseWriter, _ *http.Request) {
	healthy, errMsg := true, ""
	if s.agent != nil {
		if a, ok := s.agent.(interface{ GetHostFWStatus() (bool, string) }); ok {
			healthy, errMsg = a.GetHostFWStatus()
		}
	}
	jsonOK(w, map[string]interface{}{
		"healthy": healthy,
		"error":   errMsg,
	})
}

type versionResponse struct {
	Current string `json:"current"`
	Commit  string `json:"commit,omitempty"`
	BuildAt string `json:"build_at"`
}

func (s *Server) handleVersion(w http.ResponseWriter, _ *http.Request) {
	jsonOK(w, versionResponse{
		Current: s.version,
		Commit:  s.commit,
		BuildAt: time.Now().UTC().Format(time.RFC3339),
	})
}

type updateCheckResponse struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	HasUpdate bool   `json:"has_update"`
	Error     string `json:"error,omitempty"`
}

func (s *Server) handleUpdateCheck(w http.ResponseWriter, _ *http.Request) {
	resp := updateCheckResponse{Current: s.version}
	if s.updater == nil {
		resp.Error = "updater not initialized"
		jsonOK(w, resp)
		return
	}

	// 复用 updater 的地址派生：面板看到的“最新版本”必须和真正下载的源一致。
	latest, err := s.updater.LatestVersion()
	if err != nil {
		resp.Error = err.Error()
		jsonOK(w, resp)
		return
	}

	resp.Latest = latest
	resp.HasUpdate = updater.VersionNewer(latest, s.version)
	jsonOK(w, resp)
}

type upgradeResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Backup  string `json:"backup,omitempty"`
}

func (s *Server) handleUpgrade(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 启动升级流程
	if err := s.updater.ManualUpdate(); err != nil {
		jsonErr(w, fmt.Errorf("failed to start upgrade script: %v", err), 500)
		return
	}

	jsonOK(w, upgradeResponse{
		Success: true,
		Message: "upgrade script started, agent will restart in 10s",
	})
}

// ─── 镜像 ──────────────────────────────────────────────────────────────────────

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	images, err := s.mgr.GetSupportedImages(r.Context())
	if err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	// 上架且就绪的镜像（内置 + 自定义统一清单）
	jsonOK(w, manager.BuildImageList(s.db, images))
}

// ─── 自定义镜像（仅 podman）─────────────────────────────────────────────────────

const maxCustomImages = 20

// imageRefRe 粗校验镜像引用：registry[:port]/repo[:tag][@digest] 的合法字符集。
// 具体有效性由后台拉取验证，失败会体现在 status=error。
var imageRefRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._\-:/@]{0,255}$`)

// customImagesSupported 校验当前驱动是否支持自定义镜像，不支持时写入 4xx 响应。
func (s *Server) customImagesSupported(w http.ResponseWriter) bool {
	if s.cfg.Get().VirtType != "podman" {
		http.Error(w, "custom images are only supported on podman hosts", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) handleListCustomImages(w http.ResponseWriter, _ *http.Request) {
	list := s.db.ListCustomImages()
	if list == nil {
		list = []db.CustomImage{}
	}
	jsonOK(w, list)
}

func (s *Server) handleAddCustomImage(w http.ResponseWriter, r *http.Request) {
	if !s.customImagesSupported(w) {
		return
	}
	var req struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.Name = strings.TrimSpace(req.Name)
	if req.ID == "" || !imageRefRe.MatchString(req.ID) {
		http.Error(w, "invalid image reference", 400)
		return
	}
	if req.Name == "" {
		req.Name = req.ID
	}

	list := s.db.ListCustomImages()
	exists := false
	for _, img := range list {
		if img.ID == req.ID {
			exists = true
			break
		}
	}
	if !exists && len(list) >= maxCustomImages {
		http.Error(w, fmt.Sprintf("custom image limit reached (%d)", maxCustomImages), 400)
		return
	}

	if err := s.db.UpsertCustomImage(db.CustomImage{
		ID:      req.ID,
		Name:    req.Name,
		Status:  db.CustomImagePulling,
		AddedAt: time.Now().Unix(),
	}); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}

	// 后台拉取，结果异步更新到状态字段（重复提交同一 ID 即为重试）
	if vs, ok := s.mgr.(*manager.VMService); ok {
		vs.StartCustomImagePull(req.ID)
	}

	jsonOK(w, map[string]string{"status": db.CustomImagePulling})
}

func (s *Server) handleDeleteCustomImage(w http.ResponseWriter, r *http.Request) {
	if !s.customImagesSupported(w) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id query parameter required", 400)
		return
	}
	if err := s.db.DeleteCustomImage(id); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	// 同步清理镜像清单中的残留项（内置镜像不存在于自定义列表，不涉及）
	if builtin, err := s.mgr.GetSupportedImages(r.Context()); err == nil {
		_ = manager.RemoveImageSetting(s.db, builtin, id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetImagesConfig(w http.ResponseWriter, r *http.Request) {
	builtin, err := s.mgr.GetSupportedImages(r.Context())
	if err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	jsonOK(w, map[string]interface{}{
		"items": manager.ListImageConfigs(s.db, builtin),
	})
}

func (s *Server) handleSaveImagesConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		jsonErr(w, err, 400)
		return
	}
	// 新格式：{"items":[{"id":"...","enabled":true},...]}（顺序即展示顺序）
	var req struct {
		Items []manager.ImageConfigItem `json:"items"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Items) == 0 {
		// 兼容旧格式：["debian","alpine"]（类别名）——转换为镜像 ID 清单
		var legacy []string
		if legacyErr := json.Unmarshal(body, &legacy); legacyErr != nil {
			http.Error(w, "invalid request body", 400)
			return
		}
		builtin, bErr := s.mgr.GetSupportedImages(r.Context())
		if bErr != nil {
			jsonErr(w, bErr, 500)
			return
		}
		for _, cat := range legacy {
			cat = strings.ToLower(strings.TrimSpace(cat))
			for _, img := range builtin {
				if cat != "" && strings.Contains(strings.ToLower(img.Id), cat) {
					req.Items = append(req.Items, manager.ImageConfigItem{ID: img.Id, Enabled: true})
					break
				}
			}
		}
	}
	if err := manager.SaveImageConfigs(s.db, req.Items); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	jsonOK(w, map[string]any{"ok": true})
}

// ─── VM 列表 & 创建 ────────────────────────────────────────────────────────────

type vmListItem struct {
	VmId              string   `json:"vm_id"`
	Status            int32    `json:"status"`
	CpuPct            float32  `json:"cpu_pct"`
	RamUsedMb         int64    `json:"ram_used_mb"`
	RamTotalMb        int64    `json:"ram_total_mb"`
	NetInBps          int64    `json:"net_in_bps"`
	NetOutBps         int64    `json:"net_out_bps"`
	MonthlyTrafficIn  int64    `json:"monthly_traffic_in"`
	MonthlyTrafficOut int64    `json:"monthly_traffic_out"`
	Ips               []string `json:"ips"`
	CreatedAt         int64    `json:"created_at"`
	// 活跃连接数（按方向 × 单向/双向），由 conntrack 解析得出。
	// In/Out 是已建立的双向连接；*Uni 是只见到一个方向的（TCP 握手中、UDP 未应答）。
	ConnIn     int `json:"conn_in"`
	ConnInUni  int `json:"conn_in_uni"`
	ConnOut    int `json:"conn_out"`
	ConnOutUni int `json:"conn_out_uni"`
	// Missing 表示 DB 里有记录但容器已不存在（例如在面板外被 podman rm 掉）。
	// 这类记录必须显示出来，否则用户既看不到也清不掉。
	Missing bool `json:"missing,omitempty"`
}

// connCacheTTL 是实例连接快照的复用窗口。面板轮询间隔通常 3-5 秒，
// 取 3 秒既能让同一轮的多处调用共用一次解析，又不会明显滞后。
const connCacheTTL = 3 * time.Second

// vmActiveConns 返回每个实例 IP 的活跃连接画像（入/出 × 单向/双向）。
//
// 读不到 conntrack（内核未加载 nf_conntrack、容器网络未起来）时返回上一次的
// 结果而不是清零：连接数突然全变 0 比略微过期更容易被误读成故障。
func (s *Server) vmActiveConns(vms []*agent.VMSummary) map[string]patrol.ActiveConns {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.connCache != nil && time.Since(s.connCacheAt) < connCacheTTL {
		return s.connCache
	}
	var ips []string
	for _, vm := range vms {
		ips = append(ips, vm.Ips...)
	}
	res, err := patrol.SnapshotActiveConns(ips)
	if err != nil {
		if s.connCache == nil {
			s.connCache = map[string]patrol.ActiveConns{}
		}
		s.connCacheAt = time.Now()
		return s.connCache
	}
	s.connCache = res
	s.connCacheAt = time.Now()
	return res
}

func (s *Server) handleListVMs(w http.ResponseWriter, r *http.Request) {
	vms, _ := s.mgr.ListVMs(r.Context())
	// 一次解析服务所有实例；按 IP 汇总（v4 + v6 都算同一台实例）
	conns := s.vmActiveConns(vms)

	s.vmSnapshotMu.Lock()
	defer s.vmSnapshotMu.Unlock()
	now := time.Now()

	items := make([]vmListItem, len(vms))
	for i, vm := range vms {
		// 从数据库获取流量数据（由 TrafficService 定期更新）
		traffic, _ := s.db.GetTraffic(vm.VmId)

		item := vmListItem{
			VmId:      vm.VmId,
			Status:    int32(vm.Status),
			CpuPct:    vm.CpuPct,
			RamUsedMb: vm.RamUsedMb,
			Ips:       vm.Ips,
			CreatedAt: s.vmCreatedAtUnix(vm.VmId),
		}
		for _, ip := range vm.Ips {
			if c, ok := conns[ip]; ok {
				item.ConnIn += c.InBidi
				item.ConnInUni += c.InUni
				item.ConnOut += c.OutBidi
				item.ConnOutUni += c.OutUni
			}
		}

		// 从 DB 读取累计流量和月度流量
		if traffic != nil {
			item.MonthlyTrafficIn = traffic.MonthIn
			item.MonthlyTrafficOut = traffic.MonthOut
		}

		// 计算实时速率：当前网络计数器 - 上次缓存值，除以时间差
		// VM 的 TrafficInBytes/TrafficOutBytes 是驱动返回的累计计数器值
		if lastSnapshot, exists := s.vmSnapshots[vm.VmId]; exists {
			elapsed := now.Sub(lastSnapshot.timestamp).Seconds()
			if elapsed > 0.1 { // 至少间隔 100ms 才计算
				deltaIn := vm.TrafficInBytes - lastSnapshot.inBytes
				deltaOut := vm.TrafficOutBytes - lastSnapshot.outBytes
				// 防止计数器倒序（如容器重启）
				if deltaIn < 0 {
					deltaIn = 0
				}
				if deltaOut < 0 {
					deltaOut = 0
				}
				item.NetInBps = int64(float64(deltaIn) / elapsed)
				item.NetOutBps = int64(float64(deltaOut) / elapsed)
			}
		}

		// 保存当前快照用于下次计算速率
		s.vmSnapshots[vm.VmId] = &vmNetSnapshot{
			inBytes:   vm.TrafficInBytes,
			outBytes:  vm.TrafficOutBytes,
			timestamp: now,
		}

		if conf, _ := s.db.GetVMConfig(vm.VmId); conf != nil {
			item.RamTotalMb = conf.MemoryMB
		}
		items[i] = item
	}

	// 补上「DB 有记录、容器已不存在」的孤儿实例（例如被 podman rm -f 掉）。
	// 驱动侧的列表以实际容器为基准，不补的话这类实例在面板上凭空消失，
	// 用户既看不到状态也删不掉记录。
	if confs, err := s.db.ListVMConfigs(); err == nil && len(confs) > 0 {
		seen := make(map[string]bool, len(items))
		for _, it := range items {
			if it.VmId != "" {
				seen[it.VmId] = true
			}
		}
		for _, conf := range confs {
			if seen[conf.VMID] {
				continue
			}
			item := vmListItem{
				VmId:       conf.VMID,
				Status:     int32(agent.VMStatus_VM_STATUS_STOPPED),
				RamTotalMb: conf.MemoryMB,
				CreatedAt:  s.vmCreatedAtUnix(conf.VMID),
				Missing:    true,
			}
			if traffic, _ := s.db.GetTraffic(conf.VMID); traffic != nil {
				item.MonthlyTrafficIn = traffic.MonthIn
				item.MonthlyTrafficOut = traffic.MonthOut
			}
			items = append(items, item)
		}
	}
	jsonOK(w, items)
}

// createVMRequest 对应 POST /api/vms 请求体。
// VmId 可选，缺省时自动生成 UUID。
type createVMRequest struct {
	VmId          string `json:"vm_id"`
	Cpu           int32  `json:"cpu"`
	RamMb         int64  `json:"ram_mb"`
	DiskGb        int64  `json:"disk_gb"`
	BandwidthMbps int32  `json:"bandwidth_mbps"`
	OsImage       string `json:"os_image"`
	RootPassword  string `json:"root_password"`
}

// validateVMID 在进入驱动之前校验实例 ID。
//
// 驱动（podman 的容器名、KVM 的域名）本身就带字符集约束，不校验的话请求会一路
// 走到 createVM 深处才被拒绝，返回 500 并把 crun 的原始报错抛给调用方。
// 这里只做「不越界、不注入、不路径穿越」的宽松校验，不影响既有命名习惯。
func validateVMID(id string) error {
	if id == "" {
		return fmt.Errorf("vm_id 不能为空")
	}
	if len(id) > 128 {
		return fmt.Errorf("vm_id 最长 128 个字符")
	}
	if id[0] == '-' || id[0] == '.' {
		return fmt.Errorf("vm_id 不能以 - 或 . 开头")
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.':
		default:
			return fmt.Errorf("vm_id 只能包含字母、数字、下划线、短横线和点")
		}
	}
	return nil
}

func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var req createVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.OsImage == "" || req.RootPassword == "" {
		http.Error(w, "os_image and root_password are required", 400)
		return
	}
	if req.VmId == "" {
		req.VmId = uuid.NewString()
	}
	if err := validateVMID(req.VmId); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	cmd := &agent.CmdCreateVM{
		VmId:          req.VmId,
		Cpu:           req.Cpu,
		RamMb:         req.RamMb,
		DiskGb:        req.DiskGb,
		BandwidthMbps: req.BandwidthMbps,
		OsImage:       req.OsImage,
		RootPassword:  req.RootPassword,
	}

	ctx := r.Context()
	if err := s.mgr.CreateVM(ctx, cmd); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"vm_id": req.VmId})
}

// ─── VM 单机操作 ───────────────────────────────────────────────────────────────

func (s *Server) handleGetVM(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	info, err := s.mgr.GetVMInfo(r.Context(), vmID)
	if err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	jsonOK(w, info)
}

func (s *Server) handleStartVM(w http.ResponseWriter, r *http.Request) {
	if err := s.mgr.StartVM(r.Context(), r.PathValue("id")); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStopVM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := s.mgr.StopVM(r.Context(), r.PathValue("id"), req.Force); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRestartVM(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	if err := s.mgr.RestartVM(r.Context(), vmID); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	s.pf.RefreshVM(r.Context(), vmID)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	if err := s.mgr.DeleteVM(r.Context(), vmID); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	// 与 gRPC 删除路径保持一致：清理该 VM 的端口转发规则及 IP 限制覆盖
	_ = s.db.DeleteTraffic(vmID)
	s.pf.DeleteVM(r.Context(), vmID)
	s.wg.DeleteVM(vmID)
	_ = s.db.DeleteVMConfig(vmID)
	w.WriteHeader(http.StatusNoContent)
}

// reinstallVMRequest 对应 POST /api/vms/{id}/reinstall 请求体。
type reinstallVMRequest struct {
	OsImage       string `json:"os_image"`
	RootPassword  string `json:"root_password"`
	Cpu           int32  `json:"cpu"`
	RamMb         int64  `json:"ram_mb"`
	DiskGb        int64  `json:"disk_gb"`
	BandwidthMbps int32  `json:"bandwidth_mbps"`
}

func (s *Server) handleReinstallVM(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	var req reinstallVMRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.OsImage == "" || req.RootPassword == "" {
		http.Error(w, "os_image and root_password are required", 400)
		return
	}

	cmd := &agent.CmdReinstallVM{
		VmId:          vmID,
		OsImage:       req.OsImage,
		RootPassword:  req.RootPassword,
		Cpu:           req.Cpu,
		RamMb:         req.RamMb,
		DiskGb:        req.DiskGb,
		BandwidthMbps: req.BandwidthMbps,
	}

	if err := s.mgr.ReinstallVM(r.Context(), cmd); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}

	// 重新加载端口转发规则（因为重装可能分配了新的内网 IP）
	s.pf.RefreshVM(r.Context(), vmID)

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleResetPassword(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Password string `json:"password"`
		// RootPassword 是创建/重装用的字段名，这里一并接受：
		// 直连 API 的调用方很容易按创建请求的样子写 root_password，
		// 只认 password 会静默变成空密码（chpasswd 报 "No password has been supplied."）。
		RootPassword string `json:"root_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, err, 400)
		return
	}
	password := req.Password
	if password == "" {
		password = req.RootPassword
	}
	// 空密码必须在进驱动前拦下：chpasswd 会拿 "root:" 去改，报一句
	// "No password has been supplied." 并以 500 返回，看不出是自己传漏了字段。
	if strings.TrimSpace(password) == "" {
		jsonErr(w, fmt.Errorf("password 不能为空"), http.StatusBadRequest)
		return
	}
	v, err := s.db.GetVMConfig(id)
	if err != nil || v == nil {
		http.Error(w, "vm not found", 404)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.mgr.ResetPassword(ctx, id, password); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	w.WriteHeader(204)
}

func (s *Server) handleResetTraffic(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	v, err := s.db.GetVMConfig(id)
	if err != nil || v == nil {
		http.Error(w, "vm not found", 404)
		return
	}
	if err := s.db.ResetTrafficMonth(id); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	w.WriteHeader(204)
}

// ─── 端口转发 ──────────────────────────────────────────────────────────────────

// portFwdEntry 是端口转发规则的 JSON 表示，含实时连接统计。
// bind_family: 0 = 仅 IPv4（默认）、1 = 仅 IPv6、2 = 双栈。
type portFwdEntry struct {
	Protocol    string                `json:"protocol"`
	HostPort    int                   `json:"host_port"`
	GuestPort   int                   `json:"guest_port"`
	BindFamily  int                   `json:"bind_family"`
	TargetAddr  string                `json:"target_addr,omitempty"`
	Description string                `json:"description,omitempty"`
	ActiveConns int64                 `json:"active_conns"`
	TotalConns  int64                 `json:"total_conns"`
	TopIPs      []portforward.IPCount `json:"top_ips"`
}

func (s *Server) handleListPortFwds(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	all := s.pf.GetReport()
	result := make([]portFwdEntry, 0)
	for i := range all {
		e := &all[i]
		if e.VMID == vmID {
			result = append(result, portFwdEntry{
				Protocol:    e.Protocol,
				HostPort:    e.HostPort,
				GuestPort:   e.GuestPort,
				BindFamily:  e.BindFamily,
				TargetAddr:  e.TargetAddr,
				Description: e.Description,
				ActiveConns: e.ActiveConns,
				TotalConns:  e.TotalConns,
				TopIPs:      e.TopIPs,
			})
		}
	}
	jsonOK(w, result)
}

// validatePortFwdBind 校验映射的地址族与当前 IPv6 配置是否匹配。
// 仅 IPv4 映射始终允许；含 IPv6 时要求处于共享 IPv6（snat）模式且地址已配置。
func (s *Server) validatePortFwdBind(bindFamily int) error {
	if !portforward.ValidBindFamily(bindFamily) {
		return fmt.Errorf("bind_family 取值应为 0（IPv4）、1（IPv6）或 2（双栈）")
	}
	if bindFamily == portforward.BindV4 {
		return nil
	}
	conf := s.cfg.Get()
	if conf.IPv6Mode != "snat" {
		return fmt.Errorf("当前 IPv6 模式不支持端口映射，需使用共享 IPv6 模式")
	}
	if conf.EgressIPv6() == "" {
		return fmt.Errorf("共享 IPv6 地址未配置")
	}
	return nil
}

func (s *Server) handleAddPortFwd(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	var req portFwdEntry
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if req.Protocol != "tcp" && req.Protocol != "udp" {
		http.Error(w, "protocol must be tcp or udp", 400)
		return
	}
	if req.HostPort <= 0 || req.HostPort > 65535 {
		http.Error(w, "host_port must be within 1-65535", 400)
		return
	}
	if req.GuestPort <= 0 || req.GuestPort > 65535 {
		http.Error(w, "guest_port must be within 1-65535", 400)
		return
	}
	if err := s.validatePortFwdBind(req.BindFamily); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}

	if err := s.pf.AddMapping(r.Context(), vmID, req.Protocol, req.HostPort, req.GuestPort, req.Description, req.BindFamily); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDelPortFwd(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	proto := r.PathValue("protocol")
	hostPortStr := r.PathValue("hostPort")

	hostPort, err := strconv.Atoi(hostPortStr)
	if err != nil || hostPort <= 0 {
		http.Error(w, "invalid host_port", 400)
		return
	}
	if proto != "tcp" && proto != "udp" {
		http.Error(w, "protocol must be tcp or udp", 400)
		return
	}

	if err := s.pf.RemoveMapping(r.Context(), vmID, proto, hostPort); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSyncPortFwds(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	var rules []portFwdEntry
	if err := json.NewDecoder(r.Body).Decode(&rules); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	desired := make([]portforward.DesiredRule, 0, len(rules))
	for _, rule := range rules {
		if rule.Protocol != "tcp" && rule.Protocol != "udp" {
			jsonErr(w, fmt.Errorf("unsupported protocol %q", rule.Protocol), http.StatusBadRequest)
			return
		}
		if err := s.validatePortFwdBind(rule.BindFamily); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		desired = append(desired, portforward.DesiredRule{
			Protocol:    rule.Protocol,
			HostPort:    rule.HostPort,
			GuestPort:   rule.GuestPort,
			Description: rule.Description,
			BindFamily:  rule.BindFamily,
		})
	}

	if err := s.pf.SyncForVM(r.Context(), vmID, desired); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── WireGuard 绑定 ────────────────────────────────────────────────────────────

// wgRequest 是新建/更新 WG 绑定的请求体。
//
// 所有字段都是指针，语义为"只覆盖请求里出现过的字段"。这样面板上的
// 启用/停用开关只需 PUT {"enabled":false}，不必回传整份配置——
// 也正因如此，前端手里没有私钥（后端从不回显）也能安全地改其它字段。
type wgRequest struct {
	Name          *string `json:"name"`
	Enabled       *bool   `json:"enabled"`
	PrivateKey    *string `json:"private_key"`
	Address       *string `json:"address"`
	ListenPort    *int    `json:"listen_port"`
	MTU           *int    `json:"mtu"`
	PeerPublicKey *string `json:"peer_public_key"`
	PresharedKey  *string `json:"preshared_key"`
	Endpoint      *string `json:"endpoint"`
	AllowedIPs    *string `json:"allowed_ips"`
	Keepalive     *int    `json:"keepalive"`
}

func (req *wgRequest) apply(b *db.WGBinding) {
	setStr := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	setInt := func(dst *int, src *int) {
		if src != nil {
			*dst = *src
		}
	}
	setStr(&b.Name, req.Name)
	setStr(&b.Address, req.Address)
	setStr(&b.PeerPublicKey, req.PeerPublicKey)
	setStr(&b.PresharedKey, req.PresharedKey)
	setStr(&b.Endpoint, req.Endpoint)
	setStr(&b.AllowedIPs, req.AllowedIPs)
	setInt(&b.ListenPort, req.ListenPort)
	setInt(&b.MTU, req.MTU)
	setInt(&b.Keepalive, req.Keepalive)
	// 私钥留空表示沿用原值，否则编辑任何字段都得重新输入私钥。
	if req.PrivateKey != nil && strings.TrimSpace(*req.PrivateKey) != "" {
		b.PrivateKey = strings.TrimSpace(*req.PrivateKey)
	}
	if req.Enabled != nil {
		b.Enabled = *req.Enabled
	}
}

func (s *Server) handleListWG(w http.ResponseWriter, r *http.Request) {
	list, err := s.wg.List(r.PathValue("id"))
	if err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	jsonOK(w, list)
}

func (s *Server) handleAddWG(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	var req wgRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	// 新建时默认启用，前端不传 enabled 也能立刻用起来。
	rec := db.WGBinding{Enabled: true}
	req.apply(&rec)

	created, err := s.wg.Add(vmID, rec)
	if err != nil {
		// created 非 nil 说明记录已落库、只是隧道没起来（比如端口被占）。
		// 这种情况回 200 并带上错误，面板能显示为 error 状态而不是丢失记录。
		if created != nil {
			jsonOK(w, map[string]any{"id": created.ID, "error": err.Error()})
			return
		}
		jsonErr(w, err, 400)
		return
	}
	jsonOK(w, map[string]any{"id": created.ID})
}

func (s *Server) handleUpdateWG(w http.ResponseWriter, r *http.Request) {
	bindID := r.PathValue("bindID")
	existing, err := s.wg.Get(bindID)
	if err != nil {
		http.Error(w, "binding not found", 404)
		return
	}
	if existing.VMID != r.PathValue("id") {
		http.Error(w, "binding does not belong to this VM", 400)
		return
	}

	var req wgRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	req.apply(existing)

	updated, err := s.wg.Update(bindID, *existing)
	if err != nil {
		if updated != nil {
			jsonOK(w, map[string]any{"id": updated.ID, "error": err.Error()})
			return
		}
		jsonErr(w, err, 400)
		return
	}
	jsonOK(w, map[string]any{"id": updated.ID})
}

func (s *Server) handleDelWG(w http.ResponseWriter, r *http.Request) {
	bindID := r.PathValue("bindID")
	existing, err := s.wg.Get(bindID)
	if err != nil {
		w.WriteHeader(http.StatusNoContent) // 已经没了，当作成功
		return
	}
	if existing.VMID != r.PathValue("id") {
		http.Error(w, "binding does not belong to this VM", 400)
		return
	}
	if err := s.wg.Remove(bindID); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleWGKeypair 生成一对新密钥供面板填表，服务端不做任何保存。
func (s *Server) handleWGKeypair(w http.ResponseWriter, _ *http.Request) {
	priv, pub, err := wgbind.GenerateKeyPair()
	if err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	jsonOK(w, map[string]string{"private_key": priv, "public_key": pub})
}

// ─── 端口转发唯一来源 IP 数限制 ────────────────────────────────────────────────

// maxVMLimitIPs 是单 VM 唯一来源 IP 数上限的取值上界，与面板输入框 max 一致。
// 只做量纲保护：真按这个数用会先把 conntrack 撑爆，但拒绝明显错误的输入。
const maxVMLimitIPs = 100000

// ipLimitResponse 描述某 VM 的唯一来源 IP 数限制状态。
// Override 为 nil 表示未按 VM 覆盖（使用全局值）；Effective 为实际生效值（0 = 不限制）。
type ipLimitResponse struct {
	Override  *int `json:"override"`
	Global    int  `json:"global"`
	Effective int  `json:"effective"`
	ActiveIPs int  `json:"active_ips"`
}

func (s *Server) handleGetIPLimit(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	jsonOK(w, ipLimitResponse{
		Override:  s.pf.GetVMIPLimit(vmID),
		Global:    int(s.cfg.Get().MaxForwardIPs),
		Effective: s.pf.EffectiveIPLimit(vmID),
		ActiveIPs: s.pf.ActiveIPCount(vmID),
	})
}

func (s *Server) handleSetIPLimit(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")
	var req struct {
		Limit *int `json:"limit"` // nil = 清除覆盖，0 = 不限制，>0 = 上限
		// 旧版前端发的是 max_ips + use_global，浏览器缓存的 app.js 可能仍在用，
		// 这里一并接受，避免老页面「设置」后静默变成清除覆盖。
		MaxIPs    *int  `json:"max_ips"`
		UseGlobal *bool `json:"use_global"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	limit := req.Limit
	if limit == nil {
		limit = req.MaxIPs
	}
	if req.UseGlobal != nil && *req.UseGlobal {
		limit = nil // 显式要求回退全局：清除覆盖
	}
	if limit != nil && (*limit < 0 || *limit > maxVMLimitIPs) {
		http.Error(w, "limit must be between 0 and 100000", 400)
		return
	}
	if err := s.pf.SetVMIPLimit(vmID, limit); err != nil {
		jsonErr(w, err, vmErrStatus(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ─── TTY WebSocket ─────────────────────────────────────────────────────────────

// wsWriter 封装 WebSocket 连接为线程安全的 io.Writer，将容器输出写回客户端。
type wsWriter struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

func (w *wsWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// handleVMStatus 接收虚拟机上报的状态信息：CPU占用率、内存使用情况（CloudHV only）
func (s *Server) handleVMStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		VmID       string  `json:"vm_id"`
		CpuPercent float32 `json:"cpu_percent"`
		RamUsedMb  int64   `json:"ram_used_mb"`
		RamTotalMb int64   `json:"ram_total_mb"`
	}

	// 免鉴权端点必须限制 body 大小，防止解码前的内存放大 DoS
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		http.Error(w, "Invalid request", http.StatusBadRequest)
		return
	}

	// 将状态信息交给 CloudHV Manager 处理（仅 CloudHV 支持）
	if s.cloudHVMgr != nil {
		s.cloudHVMgr.UpdateVMStatus(req.VmID, req.CpuPercent, req.RamUsedMb, req.RamTotalMb)
		jsonOK(w, map[string]string{"status": "ok"})
		return
	}

	http.Error(w, "Not supported", http.StatusNotImplemented)
}

// ─── rfw 防火墙 ────────────────────────────────────────────────────────────────

// rfwReverseProxy 返回将 /rfw/* 请求转发到本地 rfw API 的反向代理 handler。
func (s *Server) rfwReverseProxy() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conf := s.cfg.Get()
		target, err := url.Parse("http://" + conf.RfwAddr)
		if err != nil {
			http.Error(w, "invalid rfw_addr configuration", http.StatusInternalServerError)
			return
		}
		proxy := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = target.Scheme
				req.URL.Host = target.Host
				req.URL.Path = strings.TrimPrefix(req.URL.Path, "/rfw")
				if req.URL.Path == "" {
					req.URL.Path = "/"
				}
				req.Host = target.Host
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				http.Error(w, "rfw unreachable: "+err.Error(), http.StatusBadGateway)
			},
		}
		proxy.ServeHTTP(w, r)
	})
}

// handleRfwOnline 检测本地 rfw 是否在线，并返回其 /api/status。
func (s *Server) handleRfwOnline(w http.ResponseWriter, r *http.Request) {
	conf := s.cfg.Get()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + conf.RfwAddr + "/api/status")
	if err != nil {
		jsonOK(w, map[string]interface{}{"online": false})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	var status map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&status)
	jsonOK(w, map[string]interface{}{"online": resp.StatusCode == 200, "status": status})
}

// rfwBinaryPath 返回 rfw 二进制的安装路径（与 agent 同目录）。
func rfwBinaryPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "/opt/narwhal-agent/rfw"
	}
	return filepath.Join(filepath.Dir(exe), "rfw")
}

// rfwBinaryExists 检查 rfw 二进制是否已安装。
func rfwBinaryExists() bool {
	_, err := os.Stat(rfwBinaryPath())
	return err == nil
}

// handleVMTTY 升级 HTTP 连接为 WebSocket，并将其与 VM 控制台双向绑定。
//
// 协议：
//   - 客户端 → 服务端：binary 帧为 stdin 数据；text 帧为 JSON 控制消息
//   - 控制消息：{"type":"resize","cols":80,"rows":24}
//   - 服务端 → 客户端：binary 帧为 stdout/stderr 原始字节流
func (s *Server) handleVMTTY(w http.ResponseWriter, r *http.Request) {
	vmID := r.PathValue("id")

	// 只允许连本 Agent 登记过的实例。AttachTTY 直接拿 vmID 当容器名去 exec，
	// 不校验的话可以把终端开进宿主机上任意容器（同机跑的别的业务容器也能进）。
	// 顺带把「实例不存在」从「先 101 再推报错」改成干净的 404。
	if v, err := s.db.GetVMConfig(vmID); err != nil || v == nil {
		http.Error(w, "vm not found", http.StatusNotFound)
		return
	}

	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	stdinPR, stdinPW := io.Pipe()
	resizeCh := make(chan manager.ResizeEvent, 4)

	// 读取 WebSocket 消息：text JSON → resize 事件，其余一律当输入。
	// 早先的实现在 text 帧解析 JSON 失败时直接丢弃，导致任何不发 binary 帧的
	// 客户端（wscat、自研脚本）输入全部无效且没有任何提示。
	go func() {
		defer func() { _ = stdinPW.Close() }()
		defer cancel()
		defer close(resizeCh)
		for {
			mt, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage {
				var ctrl struct {
					Type string `json:"type"`
					Cols uint   `json:"cols"`
					Rows uint   `json:"rows"`
				}
				if json.Unmarshal(msg, &ctrl) == nil && ctrl.Type == "resize" {
					select {
					case resizeCh <- manager.ResizeEvent{Cols: ctrl.Cols, Rows: ctrl.Rows}:
					default:
					}
					continue
				}
				// 不是控制消息：按键盘输入处理
			}
			if _, err := stdinPW.Write(msg); err != nil {
				return
			}
		}
	}()

	wc := &wsWriter{conn: conn}
	if err := s.mgr.AttachTTY(ctx, vmID, stdinPR, wc, resizeCh); err != nil && ctx.Err() == nil {
		_ = conn.WriteMessage(websocket.TextMessage,
			[]byte("\r\n[disconnected: "+err.Error()+"]\r\n"))
	}
}

// netAddressResponse 返回宿主机可选的对外地址，供面板下拉使用。
type netAddressResponse struct {
	IPv4    []string `json:"ipv4"`
	IPv6    []string `json:"ipv6"`
	Primary string   `json:"primary"`
}

// handleNetworkAddresses 枚举本机网卡上的公网地址（IPv4 / IPv6）。
// 过滤掉回环、容器网桥、veth 等虚拟接口，只留下真正可对外使用的地址。
func (s *Server) handleNetworkAddresses(w http.ResponseWriter, _ *http.Request) {
	jsonOK(w, netAddressResponse{
		IPv4:    listHostIPv4(),
		IPv6:    listHostIPv6(),
		Primary: primaryIPv4(),
	})
}

// listHostIPv4 枚举本机网卡上的 IPv4 地址（含内网地址：1:1 NAT 机器的监听地址就是它）。
func listHostIPv4() []string { return config.ListHostIPv4() }

const (
	podmanNetName   = "narwhal-net"
	sharedV6Subnet  = "fd91:cafe:cafe:10::/64"
	sharedV6Gateway = "fd91:cafe:cafe:10::1"
)

type shareIPv6Request struct {
	Enabled *bool  `json:"enabled"`
	Addr    string `json:"addr"`
}

// handleShareIPv6 启用/停用共享 IPv6。
//
// podman 的 IPv6 是网络级属性，容器级没有开关：一台装成纯 IPv4 的机器要加共享
// IPv6，必须重建容器网络。这里把「重建网络 → 写配置 → 刷新出口与监听」包成一个
// 动作，前端二次确认后调用。容器进程不重启，但网络会中断数秒、已建立的连接会断。
func (s *Server) handleShareIPv6(w http.ResponseWriter, r *http.Request) {
	var req shareIPv6Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Enabled == nil {
		http.Error(w, "缺少 enabled", http.StatusBadRequest)
		return
	}
	s.shareIPv6Mu.Lock()
	defer s.shareIPv6Mu.Unlock()
	if err := r.Context().Err(); err != nil {
		jsonErr(w, err, http.StatusRequestTimeout)
		return
	}
	conf := s.cfg.Get()
	if conf.VirtType != "podman" {
		jsonErr(w, fmt.Errorf("仅 Podman 后端支持共享 IPv6"), http.StatusBadRequest)
		return
	}
	if conf.IPv6Mode == "subnet" {
		jsonErr(w, fmt.Errorf("subnet 模式暂不支持通过此接口切换共享 IPv6，未修改网络"), http.StatusBadRequest)
		return
	}
	if s.podmanMgr != nil {
		unlock := s.podmanMgr.LockNetworkMigration()
		defer unlock()
		if err := r.Context().Err(); err != nil {
			jsonErr(w, err, http.StatusRequestTimeout)
			return
		}
	}
	// Finish the entire operation even if the browser disconnects after confirmation.
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Minute)
	defer cancel()

	cur, err := netmigrate.Inspect(opCtx, podmanNetName)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	target := netmigrate.Options{
		Network:   podmanNetName,
		Interface: cur.Interface,
		IPv4:      cur.IPv4,
		Gateway4:  cur.Gateway4,
	}
	if target.IPv4 == "" {
		target.IPv4, target.Gateway4 = "10.91.0.0/20", "10.91.0.1"
	}

	if *req.Enabled {
		addr := strings.TrimSpace(req.Addr)
		if addr == "" {
			addr = conf.EgressIPv6()
		}
		ip := net.ParseIP(addr)
		if ip == nil || ip.To4() != nil {
			jsonErr(w, fmt.Errorf("需要提供合法的 IPv6 地址"), http.StatusBadRequest)
			return
		}
		addr = ip.String()
		if !hostHasIPv6(addr) {
			jsonErr(w, fmt.Errorf("地址 %s 不在本机网卡上，请先在系统中配置该地址", addr), http.StatusBadRequest)
			return
		}
		target.IPv6, target.Gateway6 = sharedV6Subnet, sharedV6Gateway

		res, err := netmigrate.Rebuild(opCtx, target)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if err := s.cfg.Update(func(c *config.Config) {
			c.IPv6Mode = "snat"
			c.IPv6Addr = addr
			c.PublicIPv6 = addr
			if c.IPv6Iface == "" {
				c.IPv6Iface = conf.MonitorNIC
			}
		}); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		s.finishSharedIPv6(opCtx, w, res, true)
		return
	}

	// 停用：v6 映射会失去监听地址，必须先清掉，否则重建后全部 bind 失败。
	all, err := s.db.ListAllPortForwards()
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	v6Rules := 0
	for _, pf := range all {
		if pf.BindFamily != 0 {
			v6Rules++
		}
	}
	if v6Rules > 0 {
		jsonErr(w, fmt.Errorf("还有 %d 条映射绑定了 IPv6，请先删除这些映射再停用", v6Rules), http.StatusBadRequest)
		return
	}

	res, err := netmigrate.Rebuild(opCtx, target)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if err := s.cfg.Update(func(c *config.Config) {
		c.IPv6Mode = "none"
		c.PublicIPv6 = ""
		c.IPv6Addr = ""
	}); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	s.finishSharedIPv6(opCtx, w, res, false)
}

func (s *Server) finishSharedIPv6(ctx context.Context, w http.ResponseWriter, res *netmigrate.Result, enabled bool) {
	if err := s.applySharedIPv6(ctx); err != nil {
		jsonErr(w, fmt.Errorf("网络和配置已更新，出口或监听应用失败（将重试）：%w", err), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{
		"enabled": enabled, "rebuilt": res.Rebuilt,
		"containers": res.Containers, "downtime_ms": res.Downtime.Milliseconds(),
	})
}

// applySharedIPv6 让出站 SNAT 与端口监听跟随新配置。
func (s *Server) applySharedIPv6(ctx context.Context) error {
	var errs []error
	if s.egress != nil {
		if err := s.egress.Apply(ctx); err != nil {
			log.Printf("[IPv6] 应用出站地址失败（轮询会重试）：%v", err)
			errs = append(errs, err)
		}
	}
	if s.pf != nil {
		if err := s.pf.ReloadAll(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// hostHasIPv6 判断地址是否已配置在本机的物理网卡上。
// 共享 IPv6 的端口监听直接 bind 该地址，地址不存在时 bind 会失败。
func hostHasIPv6(addr string) bool { return config.HostHasIPv6(addr) }

// hostHasIPv4 判断地址是否已配置在本机网卡上（空值视为合法，表示不指定）。
// 端口转发直接 bind 这个地址，地址不存在时 bind 会 EADDRNOTAVAIL。
func hostHasIPv4(addr string) bool { return config.HostHasIPv4(addr) }

// listHostIPv6 枚举本机网卡上的全局 IPv6 地址（过滤回环/链路本地/ULA/虚拟接口）。
func listHostIPv6() []string { return config.ListHostIPv6() }

// primaryIPv4 返回默认路由会选中的源地址。UDP 的 Dial 只做路由查找，不发包。
func primaryIPv4() string {
	conn, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return addr.IP.String()
	}
	return ""
}
