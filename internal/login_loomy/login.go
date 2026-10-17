// Package login_loomy 实现 Loomy（讯飞）的「浏览器登录」。
//
// # 与其它渠道的根本差异：Loomy 没有 OAuth 授权页
//
// 其余渠道都是「返回 loginUrl → 用户在浏览器里授权 → 本机回调 / 轮询收 code」。
// Loomy 的登录入口是**讯飞 CAccount 账号服务**，只有两条路：
//
//	① 短信验证码：POST /login/phone/sendMsgCode → 用户输码 → POST /login/phone/checkCode
//	② 微信扫码：微信长轮询拿 code → /login/thirdAccount/bind/* 四步换 session
//
// 两条路都靠 **HMAC-SHA1 签名**（`Authorization: account {ak}:{sig}`）鉴权，
// 而签名密钥是随客户端分发的公开值（见 constants.go 的 accessKeySecret）。
// 因此「浏览器登录」在本渠道的落地形态是：
//
//	Start()     → 返回一个**真实可达的登录引导页**（本机 loopback），并置会话为 pending
//	              该页只做两件事：展示「在手机上完成 Loomy 登录」的说明 + 接收手机号并请求验证码
//	              手机号里的验证码**由页面转发给本机后端**发起 SendCode（浏览器里没有短信到达）
//	SendCode()  → 向讯飞账号服务下发短信验证码（真实验证码，发到用户手机）
//	Login()     → 用「手机号 + 验证码 + msgid」换 session（这就是登录引导页表单提交的动作）
//	Poll()      → 登录成功返回 Result（ErrPending 表示还在等）
//	SaveAuth()  → 落盘 auths/loomy-<uid>.json（与「从本机客户端导入」同一形态）
//
// # 为什么不照抄 ofm 的做法
//
// 参考实现（dsh-our-free-model）起一个含**内联微信二维码**的本地服务器。本工具不这么做，
// 因为：微信 `redirect_uri` 必须是官方地址（`https://loomy.xunfei.cn/oauth/wechat/callback`，
// 微信校验域名白名单），而这个回调页**实测 404**；官方 Electron 客户端是靠 `BrowserWindow`
// 的 `will-redirect` 在导航前截获 code 的，普通浏览器做不到。
// 故本实现**只走短信验证码**这条官方真实可用的路（ofm 的短信路径同样是它的主推荐路径），
// 并保留「从本机客户端导入」作为替代路径（internal/app/import_local.go）。
//
// # 为什么登录引导页是 loopback 而不是直接开 loomy 官网
//
// 短信验证码只能在本工具内发起（签名密钥与协议都在本工具里），官网登录页的验证码
// 不会交给本工具。故引导页是**本工具的** loopback 页面：它承载手机号输入 + 验证码转发，
// 用户实际收短信、填验证码的动作发生在手机上或页面里。
//
// 与调用方（internal/app）的契约（与 login_trae 同形）：
//
//	NewClient()                    → 登录用 HTTP client
//	Start(client, statePath)       → 起引导页并返回地址
//	SendCode(client, phone)        → 下发短信验证码，返回 msgid
//	Login(client, phone, code, id) → 换 session（成功即进入 success）
//	Poll(client, statePath)        → ErrPending / Result / 终态 error
//	Shutdown()                     → 关引导页（幂等）
//	SaveAuth(authDir, r)           → 落盘
package login_loomy

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
)

// ErrPending 表示登录尚未完成（用户还没提交验证码，或验证码还没通过）。
// 与 login_trae / login_lobsterai / login_raccoon 同名同义，app.pollLogin 用 errors.Is 判定。
var ErrPending = errors.New("login pending")

// flowTTL 是一条登录流程的最长寿命。
//
// ⚠️ 必须**小于** app 的 loginTimeout（5 分钟，app.go:63）：否则 app 会先以
// 「登录超时」收尾，而本包的引导页还在监听，用户此时提交验证码会得到
// 「流程已结束」的困惑提示（登录其实已成功但没人收）。
const flowTTL = 4 * time.Minute

// Poll 的结果只取一次：取走后再 Poll 回到 ErrPending，避免同一账号被重复落盘。

// Result 登录成功后拿到的凭据与账号信息（与「从本机客户端导入」落盘形态一致）。
//
// Loomy 的凭据只有 `session`（32 位 hex，向服务端声明 14 天有效期），
// **没有任何 refresh 端点**，故 RefreshToken 恒为空、ExpiresAt 由本地按 14 天推算。
type Result struct {
	// AccessToken 即 Loomy 的 session（推理与积分接口都用它）。
	AccessToken string
	// RefreshToken 恒为空：Loomy 无续期机制（留空还能让 scheduler 跳过无意义的保活）。
	RefreshToken string
	// ExpiresAt 是 session 的到期时刻（**Unix 秒**，本地按 14 天推算）。
	ExpiresAt int64
	// Domain / ApiHost 写进 auth 文件，供 internal/loomy 组装出站请求。
	Domain  string
	ApiHost string
	// UID 是账号池去重/排序标识。上游只在登录响应里给 userid；
	// 缺省时由 SessionUID 从 session 派生（见 deriveUID 的说明）。
	UID string
	// Phone 是登录手机号（展示用，可为空：微信路径拿不到手机号）。
	Phone string
	// Nickname 展示名（上游不下发时用手机号/UID 兜底）。
	Nickname string
}

// LogFields 返回可安全写进日志的摘要（**绝不含 token**，AGENTS.md §0.2 零 token 落盘）。
func (r Result) LogFields() string {
	return fmt.Sprintf("uid=%s phone=%s expires_at=%d", r.UID, maskPhone(r.Phone), r.ExpiresAt)
}

// SendCode 向讯飞账号服务下发短信验证码，返回 msgid（验证码登录时必须原样带回）。
//
// 这是「浏览器登录」链路里**唯一触达上游**的写操作之一 —— 故：
//   - 手机号先在本地校验（非法号码不发请求，不浪费用户的上游配额/风控额度）；
//   - 失败是**终态**：网络抖动/限频重试只会放大风控风险，也让用户看不出「没收到短信」的原因。
func SendCode(client *http.Client, phone string) (string, error) {
	s := currentSession()
	if s == nil {
		return "", errors.New("没有进行中的 Loomy 登录流程，请先在面板点「登录 Loomy」")
	}
	phone = strings.TrimSpace(phone)
	if !phonePattern.MatchString(phone) {
		return "", errors.New("手机号格式不正确：请填 11 位中国大陆手机号")
	}
	return s.sendCode(client, phone)
}

// Login 用「手机号 + 验证码 + msgid」换 session；成功后会话置为 success，由 Poll 取走。
//
// 这是登录引导页表单提交的动作。失败**不终止**流程（用户可能只是验证码输错，
// 见 ofm loomy-wechat-login.ts:288-292 同款处理）：把原因回给页面让用户就地重试。
func Login(client *http.Client, phone, code, msgid string) (Result, error) {
	s := currentSession()
	if s == nil {
		return Result{}, errors.New("没有进行中的 Loomy 登录流程，请先在面板点「登录 Loomy」")
	}
	phone = strings.TrimSpace(phone)
	code = strings.TrimSpace(code)
	msgid = strings.TrimSpace(msgid)
	if !phonePattern.MatchString(phone) {
		return Result{}, errors.New("手机号格式不正确：请填 11 位中国大陆手机号")
	}
	if code == "" {
		return Result{}, errors.New("请填写短信验证码")
	}
	if msgid == "" {
		return Result{}, errors.New("请先点「获取验证码」（缺少 msgid）")
	}
	return s.login(client, phone, code, msgid)
}

// ===========================================================================
// 会话编排
// ===========================================================================

// phase 是登录流程的阶段。
type phase string

const (
	// PhasePending 已 Start，等待用户提交验证码。
	PhasePending phase = "pending"
	// PhaseSuccess 已换到 session，等待 Poll 取走。
	PhaseSuccess phase = "success"
	// PhaseFailed 终态失败（下发验证码就失败了 / 引导页起不来）。
	PhaseFailed phase = "failed"
	// PhaseCancelled 被 Shutdown 取消。
	PhaseCancelled phase = "cancelled"
)

// Status 是供面板展示的只读状态（**不含任何 token**）。
type Status struct {
	Phase phase
	// Phone 已请求过验证码的手机号（脱敏）。
	Phone string
	// HasSession 是否已登录成功（仅布尔，不给 token）。
	HasSession bool
	// Err 终态失败原因。
	Err string
}

// String 便于日志与测试断言。
func (s Status) String() string {
	if s.Err != "" {
		return fmt.Sprintf("phase=%s phone=%s has_session=%t err=%s", s.Phase, s.Phone, s.HasSession, s.Err)
	}
	return fmt.Sprintf("phase=%s phone=%s has_session=%t", s.Phase, s.Phone, s.HasSession)
}

// session 是一条登录流程的运行时状态。
// 全部字段由 mu 保护：引导页 handler 在 server goroutine 里写，Poll / Shutdown 在主流程读。
type session struct {
	mu sync.Mutex

	ph       phase
	errMsg   string
	phone    string // 已脱敏
	result   Result
	hasRes   bool // result 是否可被取走
	sendDone bool // 本次流程是否已成功下发过验证码

	srv *http.Server
	ln  net.Listener
	// loginURL 是引导页地址。
	loginURL string
	// createdAt 用于 flowTTL 判定。
	createdAt time.Time
}

// CurrentStatus 供面板展示的只读状态（**不含任何 token**）。
//
// 命名带 Current 前缀而不是 `Status()`：`Status` 已是类型名，Go 不允许
// 同名类型与函数并存。
func CurrentStatus() Status {
	s := currentSession()
	if s == nil {
		return Status{}
	}
	return s.status()
}

// status 读一次状态。
func (s *session) status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Status{Phase: s.ph, Phone: s.phone, HasSession: s.hasRes, Err: s.errMsg}
}

// setFailed 置终态失败（已 success 时不降级）。
func (s *session) setFailed(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ph == PhaseSuccess {
		return
	}
	s.ph, s.errMsg = PhaseFailed, msg
}

// close 关掉引导页（幂等）。
func (s *session) close() {
	if s == nil {
		return
	}
	if s.srv != nil {
		ctx, cancel := contextWithTimeout(time.Second)
		defer cancel()
		_ = s.srv.Shutdown(ctx)
	}
	if s.ln != nil {
		_ = s.ln.Close()
	}
}

var (
	// curMu 保护 cur（仅 Start / Shutdown / Poll 的入口临界区）。
	curMu sync.Mutex
	cur   *session
)

// currentSession 取当前会话。
func currentSession() *session {
	curMu.Lock()
	defer curMu.Unlock()
	return cur
}

// NewClient 返回登录用 HTTP 客户端（与 login_trae / login_lobsterai 同形：30s 总超时兜底）。
func NewClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Start 发起登录：起本机登录引导页（127.0.0.1 随机端口）并**立即**返回其地址。
//
// ⚠️ 绝不监听 0.0.0.0：引导页会接收手机号与验证码（并转发给上游），
// 暴露到局域网等于把登录口交给同网段任何人。
//
// 重复调用是**幂等**的：已有一条进行中的流程时返回**同一个地址**而不是报错。
// 原因见 TestStartRejectsConcurrent 的注释 —— 前端拿不到 auth_url 就不会开窗口，
// 用户会被困在「有个活着的引导页但再也打不开它」的状态里。
//
// statePath 与其它渠道一致用于落一次性登录状态；但本渠道的协议**不依赖**它
// （没有回调里的 code 要跨进程比对），故只做「清掉上一轮残留」这一件事。
func Start(_ *http.Client, statePath string) (string, error) {
	curMu.Lock()
	if cur != nil {
		u := cur.loginURL
		curMu.Unlock()
		return u, nil
	}
	curMu.Unlock()

	// 清掉上一轮残留的 state（其它渠道的 Poll 会读它；残留会让排查时误判）。
	if statePath != "" {
		_ = os.Remove(statePath)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("无法监听本机登录引导页端口：%w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	s := &session{ph: PhasePending, createdAt: time.Now()}
	s.loginURL = fmt.Sprintf("http://127.0.0.1:%d%s", port, guidePath)
	mux := http.NewServeMux()
	mux.HandleFunc(guidePath, s.handleGuide)
	mux.HandleFunc(apiSendCodePath, s.handleSendCode)
	mux.HandleFunc(apiLoginPath, s.handleLogin)
	mux.HandleFunc(apiStatusPath, s.handleStatus)
	s.ln = ln
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	curMu.Lock()
	cur = s
	curMu.Unlock()

	go func() { _ = s.srv.Serve(ln) }()
	// 到点自关：不让引导页比 app 的 loginTimeout 活得久（见 flowTTL 的注释）。
	go func() {
		t := time.NewTimer(flowTTL)
		defer t.Stop()
		<-t.C
		if currentSession() == s {
			s.setFailed("登录超时（4 分钟内未完成验证码登录），请重新发起")
			Shutdown()
		}
	}()

	log.Printf("loomy 登录：已启动本机登录引导页（端口 %d），等待用户完成短信验证码登录", port)
	return s.loginURL, nil
}

// Poll 取一次登录结果：未完成返回 ErrPending；成功返回 Result（**只返回一次**）；失败返回终态错误。
//
// 取消后返回 ErrPending 而非「已取消」错误 —— 与 login_lobsterai / login_gemini /
// login_codearts 的既有约定一致（它们 Shutdown 后 cur 置 nil，Poll 同样返回 ErrPending）：
// 取消的文案由 app.pollLogin 的 ctx.Done() 分支统一给出（「登录已取消」），
// 这里再报一次只会让用户看到两条重复提示。
func Poll(_ *http.Client, _ string) (Result, error) {
	s := currentSession()
	if s == nil {
		return Result{}, ErrPending
	}
	if s.createdAt.After(time.Time{}) && time.Since(s.createdAt) > flowTTL {
		s.setFailed("登录超时（4 分钟内未完成验证码登录），请重新发起")
	}
	st := s.status()
	switch st.Phase {
	case PhaseSuccess:
		s.mu.Lock()
		r, ok := s.result, s.hasRes
		s.hasRes = false // 只交一次
		s.mu.Unlock()
		if !ok {
			return Result{}, ErrPending
		}
		return r, nil
	case PhaseFailed:
		msg := st.Err
		if msg == "" {
			msg = "Loomy 登录失败"
		}
		return Result{}, errors.New(msg)
	case PhaseCancelled:
		// 见方法注释：取消由 app 层的 ctx.Done() 分支表述。
		return Result{}, ErrPending
	default:
		return Result{}, ErrPending
	}
}

// Shutdown 关掉引导页并作废会话（幂等：无会话、已结束都不报错）。
func Shutdown() {
	curMu.Lock()
	s := cur
	cur = nil
	curMu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	if s.ph == PhasePending {
		s.ph = PhaseCancelled
	}
	s.mu.Unlock()
	s.close()
}

// ===========================================================================
// 落盘
// ===========================================================================

// SaveAuth 以嵌套形原子写盘 `auths/loomy-<uid>.json`。
//
// 刻意**不手写 map**，而是构造 auth.Auth 后调 SaveAtomic()：这样落盘字节形态
// 与 auth.Parse / auth.LoadLoomyDir / 将来刷新路径的写回**完全同源**
// （不变式 3：auth 文件嵌套格式必须与 Parse 一致）。
//
// 前缀必须是 `loomy-`：auth.LoadLoomyDir 用 `loadPrefixed(dir, "loomy")`，
// 即 glob `loomy-*.json`；换个前缀账号就直接不出现在池里。
func SaveAuth(authDir string, r Result) (string, error) {
	uid := strings.TrimSpace(r.UID)
	if uid == "" {
		return "", errors.New("missing uid in result")
	}
	if strings.TrimSpace(r.AccessToken) == "" {
		// 防御：login 已硬校验，这里再挡一次 —— 半成品凭据落盘后不可用且会污染账号池。
		return "", errors.New("missing accessToken in result")
	}
	domain, host := r.Domain, r.ApiHost
	if domain == "" {
		domain = apiDomain
	}
	if host == "" {
		host = apiHost
	}
	nick := strings.TrimSpace(r.Nickname)
	if nick == "" {
		nick = maskPhone(r.Phone)
	}
	if nick == "" {
		nick = uid
	}
	a := &auth.Auth{
		Kind:        "loomy",
		AccessToken: r.AccessToken,
		// Loomy 无 refresh 端点：留空能让 scheduler 跳过必然失败的保活。
		RefreshToken: "",
		ExpiresAt:    r.ExpiresAt,
		Domain:       domain,
		ApiHost:      host,
		UID:          uid,
		Nickname:     nick,
	}
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return "", err
	}
	a.FilePath = filepath.Join(authDir, "loomy-"+sanitizeUID(uid)+".json")
	if err := a.SaveAtomic(); err != nil {
		return "", err
	}
	return a.FilePath, nil
}

// phonePattern 中国大陆手机号（1 开头 11 位）。
var phonePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)

// maskPhone 手机号脱敏（日志与面板只展示这个形态）。
func maskPhone(phone string) string {
	phone = strings.TrimSpace(phone)
	if len(phone) < 7 {
		return ""
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}

// sanitizeUID 过滤文件名里的危险字符。
//
// 比 app.sanitizeUID 更严一档：除了字符白名单，还**折叠连续的 `.`**。
// 原因：`../..` 经字符白名单后仍剩 `....`，虽然它不构成路径穿越（文件名里没有分隔符），
// 但会生成 `loomy-....evil.json` 这种既难读、又容易让人误判「是不是被穿越了」的名字。
// 上游的 userid 本来就不会含点，故折叠不会伤到正常账号。
func sanitizeUID(uid string) string {
	var sb strings.Builder
	for _, r := range uid {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			// 连续点折叠成一个（含跨 rune 的连续，故按已写入的最后一个字符判断）。
			if r == '.' && strings.HasSuffix(sb.String(), ".") {
				continue
			}
			sb.WriteRune(r)
		}
	}
	out := strings.Trim(sb.String(), ".")
	if out == "" {
		return "default"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
