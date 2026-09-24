// Package app 管理面板会话鉴权：cookie 会话 + 管理员密码。
//
// 设计（见 AGENTS.md R4 的修订）：
//   - config.admin_password 为空 = 面板不鉴权（仅允许监听环回地址时为空）；
//   - 监听非环回地址时强制要求该字段（Save 层 + 启动层双重把关）；
//   - 会话 token 为随机串，服务端只存哈希；重启即失效（需重新登录）。
package app

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work/internal/config"
)

const (
	// sessionCookie 管理面板会话 cookie 名。
	sessionCookie = "ww_admin"
	// sessionTTL 会话有效期；面板每次成功请求续期（滑动过期）。
	sessionTTL = 7 * 24 * time.Hour
	// sessionSweep 过期会话清理间隔（仅在有请求时顺带触发）。
	sessionSweep = time.Hour
	// loginMaxFail 连续失败次数达到上限后临时拒绝该来源 IP 的登录尝试。
	loginMaxFail = 5
	// loginFailWindow 失败计数窗口 / 锁定时长。
	loginFailWindow = 5 * time.Minute
)

// panelSession 一条已签发的会话：map 以 token 的 SHA-256（hex）为键，只存哈希与过期时间。
type panelSession struct {
	exp   time.Time
	label string // 密码指纹：改密码后旧会话自然失配
}

// loginAttempt 单个来源 IP 的登录失败计数。
type loginAttempt struct {
	fails   int
	firstAt time.Time
	until   time.Time // 非零且在未来 = 锁定中
}

// authState 面板鉴权状态（进程内存，重启即失效；Cookie 只存 token 本身）。
type authState struct {
	mu       sync.Mutex
	sessions map[string]*panelSession // tokenHash → session
	attempts map[string]*loginAttempt // 客户端 IP → 登录失败计数
	sweptAt  time.Time
}

func newAuthState() *authState {
	return &authState{sessions: map[string]*panelSession{}, attempts: map[string]*loginAttempt{}}
}

// panelAuthEnabled 当前是否启用面板鉴权（密码非空）。
func (a *App) panelAuthEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.TrimSpace(a.cfg.AdminPass) != ""
}

// requireRemoteAuth 判断某个监听地址是否必须配置管理员密码：
// 非环回监听（0.0.0.0 / 具体网卡 IP / 空主机名）一律强制。
func requireRemoteAuth(l config.Listen) bool { return !l.IsLoopback() }

// panelLogin 校验管理员密码并签发会话。
// 返回的 brief 为「口令指纹」：前端只持有它判断登录态，无需读取 HttpOnly cookie。
func (a *App) panelLogin(pass, clientIP string) (token, brief string, ok bool, msg string) {
	valid := a.adminPass()
	if strings.TrimSpace(valid) == "" {
		return "", "", true, "未启用管理密码"
	}
	if !a.auth.allowLogin(clientIP) {
		return "", "", false, "尝试次数过多，请 5 分钟后再试"
	}
	if subtle.ConstantTimeCompare([]byte(pass), []byte(valid)) != 1 {
		a.auth.noteFail(clientIP)
		log.Printf("管理面板登录失败 ip=%s", clientIP)
		return "", "", false, "密码错误"
	}
	a.auth.noteSuccess(clientIP)
	brief = passBrief(valid)
	token = a.auth.issue(brief)
	log.Printf("管理面板登录成功 ip=%s", clientIP)
	return token, brief, true, ""
}

// panelLogout 注销当前会话。
func (a *App) panelLogout(r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.auth.revoke(c.Value)
	}
}

// adminPass 读当前管理员密码快照。
func (a *App) adminPass() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.AdminPass
}

// passBrief 口令指纹：改密码后旧 cookie 立即失效（prefix = SHA-256(密码) 前 8 字节的 hex）。
func passBrief(pass string) string {
	sum := sha256.Sum256([]byte("wildwork-admin:" + pass))
	return hex.EncodeToString(sum[:8])
}

// newSessionToken 生成 32 字节随机 token（hex 编码）。
func newSessionToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil { // crypto/rand 失败极罕见：宁可直接失败也不签弱 token
		panic("crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf)
}

// issue 签发会话并返回 token。
func (s *authState) issue(brief string) string {
	tok := newSessionToken()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.sessions[tokHash(tok)] = &panelSession{exp: time.Now().Add(sessionTTL), label: brief}
	return tok
}

// valid 校验 token 并续期（滚动过期）。token 原文只存在于客户端 cookie。
func (s *authState) valid(tok, brief string) bool {
	if tok == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	key := tokHash(tok)
	sess := s.sessions[key]
	if sess == nil || sess.label != brief {
		return false
	}
	if time.Now().After(sess.exp) {
		delete(s.sessions, key)
		return false
	}
	sess.exp = time.Now().Add(sessionTTL) // 滑动续期：常用面板不会中途掉线
	return true
}

// revoke 注销单个会话。
func (s *authState) revoke(tok string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, tokHash(tok))
}

// tokHash 会话 token → 存储键（SHA-256 hex）。
func tokHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

// dropAll 清空所有会话（改密码时调用）。
func (s *authState) dropAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]*panelSession{}
}

// sweepLocked 顺带清理过期会话（最多每小时一次）。调用方需持锁。
func (s *authState) sweepLocked() {
	now := time.Now()
	if now.Sub(s.sweptAt) < sessionSweep {
		return
	}
	s.sweptAt = now
	for k, v := range s.sessions {
		if now.After(v.exp) {
			delete(s.sessions, k)
		}
	}
	for k, v := range s.attempts {
		if now.After(v.until) && now.Sub(v.firstAt) > loginFailWindow {
			delete(s.attempts, k)
		}
	}
}

// allowLogin 该来源 IP 是否允许尝试登录（失败过多则临时锁定）。
func (s *authState) allowLogin(ip string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at := s.attempts[ip]
	return at == nil || time.Now().After(at.until)
}

// noteFail 记录一次登录失败；窗口内累计到上限则锁定。
func (s *authState) noteFail(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	at := s.attempts[ip]
	if at == nil || now.Sub(at.firstAt) > loginFailWindow {
		at = &loginAttempt{firstAt: now}
		s.attempts[ip] = at
	}
	at.fails++
	if at.fails >= loginMaxFail {
		at.until = now.Add(loginFailWindow)
		at.fails = 0
		at.firstAt = now
	}
}

// noteSuccess 登录成功后清空失败计数。
func (s *authState) noteSuccess(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.attempts, ip)
}
