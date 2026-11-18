package web

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"sync"
	"time"
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

// sessionStore 保存已登录会话与登录失败记录；进程重启即全部失效（强制重新登录）。
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*sessionEntry // token -> entry
	fails    map[string]*failEntry    // ip -> 失败计数
}

type sessionEntry struct {
	user     string
	lastSeen time.Time
}

type failEntry struct {
	count       int
	lockedUntil time.Time
	lastFail    time.Time
}

func newSessionStore() *sessionStore {
	s := &sessionStore{
		sessions: make(map[string]*sessionEntry),
		fails:    make(map[string]*failEntry),
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

// sweep 删除过期会话与已失效的失败记录（持锁批量删除）。
func (s *sessionStore) sweep() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for t, e := range s.sessions {
		if now.Sub(e.lastSeen) > sessionLifetime {
			delete(s.sessions, t)
		}
	}
	for ip, e := range s.fails {
		if e.lockedUntil.Before(now) && now.Sub(e.lastFail) > lockoutDuration {
			delete(s.fails, ip)
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
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[token] = &sessionEntry{user: user, lastSeen: time.Now()}
	return token, nil
}

// validate 校验 token 并滑动续期；无效返回 false。
func (s *sessionStore) validate(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.sessions[token]
	if !ok {
		return "", false
	}
	if time.Since(e.lastSeen) > sessionLifetime {
		delete(s.sessions, token)
		return "", false
	}
	e.lastSeen = time.Now()
	return e.user, true
}

// destroy 删除单个会话（登出）。
func (s *sessionStore) destroy(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// destroyAll 清空所有会话（修改密码后踢掉全部登录）。
func (s *sessionStore) destroyAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = make(map[string]*sessionEntry)
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
