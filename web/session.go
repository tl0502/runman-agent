package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"runman-agent/db"
)

const (
	sessionCookie   = "narwhal_session"
	sessionLifetime = 7 * 24 * time.Hour
	maxLoginFails   = 5
	lockoutDuration = 10 * time.Minute
)

// sessionUserKey 用于向 context 注入当前登录用户。
type sessionUserKeyType struct{}

var sessionUserKey = sessionUserKeyType{}

// sessionStore 保存已登录会话与登录失败记录。
// 会话同时落库（只存 token 的 SHA-256），agent 重启/自动更新后从库恢复，
// 不会把已登录用户踢出去；失败计数仍是内存态（重启清零可接受）。
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*sessionEntry // tokenHash -> entry
	fails    map[string]*failEntry    // ip -> 失败计数
	blocked  map[string]time.Time     // ip -> 上次 blocked 事件记录时刻（节流）
	db       *db.DB
}

type sessionEntry struct {
	user     string
	lastSeen time.Time
	syncedAt time.Time // 上次写库时间（节流，避免每次请求都落盘）
}

type failEntry struct {
	count       int
	lockedUntil time.Time
	lastFail    time.Time
}

// tokenHash 返回会话 token 的 SHA-256（十六进制），内存与 DB 都用它作 key。
func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func newSessionStore(database *db.DB) *sessionStore {
	s := &sessionStore{
		sessions: make(map[string]*sessionEntry),
		fails:    make(map[string]*failEntry),
		blocked:  make(map[string]time.Time),
		db:       database,
	}
	// 从库恢复未过期会话：进程重启（含自动更新）后保持登录状态。
	if database != nil {
		cutoff := time.Now().Add(-sessionLifetime)
		for _, row := range database.ListWebSessions() {
			if row.LastSeen.Before(cutoff) {
				continue
			}
			s.sessions[row.TokenHash] = &sessionEntry{
				user:     row.User,
				lastSeen: row.LastSeen,
				syncedAt: row.LastSeen,
			}
		}
	}
	startSessionJanitor(s)
	return s
}

// startSessionJanitor 周期清扫两个 map：sessions/fails 目前只在条目被再次
// 访问时惰性删除，不再出现的条目（登录后不登出、失败几次就走）会永久驻留。
// agent 是常驻进程，必须后台兜底，否则慢性内存增长（且 fails 可被海量源 IP 注水）。
func startSessionJanitor(s *sessionStore) {
	go func() {
		for {
			time.Sleep(time.Hour)
			s.sweep()
		}
	}()
}

// sweep 删除过期会话与已失效的失败记录（持锁批量删除），
// 锁外清理库中的过期会话行。
func (s *sessionStore) sweep() {
	now := time.Now()
	s.mu.Lock()
	for h, e := range s.sessions {
		if now.Sub(e.lastSeen) > sessionLifetime {
			delete(s.sessions, h)
		}
	}
	for ip, e := range s.fails {
		if e.lockedUntil.Before(now) && now.Sub(e.lastFail) > lockoutDuration {
			delete(s.fails, ip)
		}
	}
	for ip, t := range s.blocked {
		if now.Sub(t) > time.Minute {
			delete(s.blocked, ip)
		}
	}
	s.mu.Unlock()

	if s.db != nil {
		if err := s.db.DeleteExpiredWebSessions(now.Add(-sessionLifetime)); err != nil {
			log.Printf("[Web] purge expired sessions failed: %v", err)
		}
	}
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// create 新建会话并返回 token。
func (s *sessionStore) create(user string) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	h := tokenHash(token)
	now := time.Now()
	s.mu.Lock()
	s.sessions[h] = &sessionEntry{user: user, lastSeen: now, syncedAt: now}
	s.mu.Unlock()

	// 落库失败不阻断登录（最坏情况是重启后需要重新登录）。
	if s.db != nil {
		if err := s.db.SaveWebSession(h, user, now); err != nil {
			log.Printf("[Web] save session failed: %v", err)
		}
	}
	return token, nil
}

// validate 校验 token 并滑动续期；无效返回 false。
// 续期写库按 10 分钟节流，避免每个请求都落一次盘。
func (s *sessionStore) validate(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	h := tokenHash(token)
	s.mu.Lock()
	e, ok := s.sessions[h]
	if !ok {
		s.mu.Unlock()
		return "", false
	}
	now := time.Now()
	if now.Sub(e.lastSeen) > sessionLifetime {
		delete(s.sessions, h)
		s.mu.Unlock()
		if s.db != nil {
			_ = s.db.DeleteWebSession(h)
		}
		return "", false
	}
	e.lastSeen = now
	needSync := s.db != nil && now.Sub(e.syncedAt) > 10*time.Minute
	if needSync {
		e.syncedAt = now
	}
	user := e.user
	s.mu.Unlock()

	if needSync {
		go func() {
			if err := s.db.SaveWebSession(h, user, now); err != nil {
				log.Printf("[Web] refresh session failed: %v", err)
			}
		}()
	}
	return user, true
}

// destroy 删除单个会话（登出）。
func (s *sessionStore) destroy(token string) {
	h := tokenHash(token)
	s.mu.Lock()
	delete(s.sessions, h)
	s.mu.Unlock()
	if s.db != nil {
		if err := s.db.DeleteWebSession(h); err != nil {
			log.Printf("[Web] delete session failed: %v", err)
		}
	}
}

// destroyAll 清空所有会话（修改密码后踢掉全部登录）。
func (s *sessionStore) destroyAll() {
	s.mu.Lock()
	s.sessions = make(map[string]*sessionEntry)
	s.mu.Unlock()
	if s.db != nil {
		if err := s.db.DeleteAllWebSessions(); err != nil {
			log.Printf("[Web] clear sessions failed: %v", err)
		}
	}
}

// loginLocked 判断该 IP 是否处于登录失败锁定中。
func (s *sessionStore) loginLocked(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.fails[ip]
	if !ok {
		return false
	}
	if e.lockedUntil.After(time.Now()) {
		return true
	}
	// 锁定已过期，清理旧记录
	if time.Since(e.lastFail) > lockoutDuration {
		delete(s.fails, ip)
	}
	return false
}

// recordFail 记录一次登录失败，连续达到上限则锁定该 IP。
// 返回值：locked 为 true 表示本次失败触发了新锁定（until 为锁定截止时间），
// 调用方可据此记录 auto_ban 事件。
func (s *sessionStore) recordFail(ip string) (until time.Time, locked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.fails[ip]
	if !ok {
		e = &failEntry{}
		s.fails[ip] = e
	}
	e.count++
	e.lastFail = time.Now()
	if e.count >= maxLoginFails {
		until = time.Now().Add(lockoutDuration)
		e.lockedUntil = until
		e.count = 0
		locked = true
	}
	return until, locked
}

// recordSuccess 登录成功后清空该 IP 的失败记录。
func (s *sessionStore) recordSuccess(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.fails, ip)
}

// lockInfo 描述一条生效中的自动锁定。
type lockInfo struct {
	IP    string
	Until time.Time
}

// lockedIPs 返回当前全部生效中的自动锁定（供面板封禁列表合并展示）。
func (s *sessionStore) lockedIPs() []lockInfo {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]lockInfo, 0, len(s.fails))
	for ip, e := range s.fails {
		if e.lockedUntil.After(now) {
			out = append(out, lockInfo{IP: ip, Until: e.lockedUntil})
		}
	}
	return out
}

// clearLock 清除该 IP 的失败记录与自动锁定（解封操作）。
func (s *sessionStore) clearLock(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.fails, ip)
	delete(s.blocked, ip)
}

// recordBlocked 对被封 IP 的登录尝试做事件节流：60 秒内同 IP 只记一次。
// 被封后仍高频重试的攻击者若每次都写库，会刷爆时间线并放大 DB 写入。
// 返回 true 表示本次应记录事件。
func (s *sessionStore) recordBlocked(ip string) bool {
	const blockedThrottle = time.Minute
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if last, ok := s.blocked[ip]; ok && now.Sub(last) < blockedThrottle {
		return false
	}
	s.blocked[ip] = now
	return true
}

// clientIP 提取来源 IP（RemoteAddr 去端口）。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// setSessionCookie 写登录 cookie；secure 按 TLS 由调用方传入。
func setSessionCookie(w http.ResponseWriter, token string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   int(sessionLifetime.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true, // 与 setSessionCookie 保持一致（服务恒为 TLS）
		SameSite: http.SameSiteLaxMode,
	})
}
