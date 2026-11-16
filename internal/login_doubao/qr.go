// Package login_doubao 实现豆包的「扫码登录」。
//
// # 为什么是扫码而不是拉浏览器
//
// 本仓其它网页渠道（glm/raccoon/comate/lingxi）走 CDP 拉起独立 profile 的浏览器，
// 但那条路要求**能启动浏览器**：WSL / 无桌面环境（NAS）下起不来（用户实测：
// 面板点了登录，Windows 侧浏览器不弹）。豆包的扫码链路是**纯 HTTP**——
// 服务端申请二维码、返回 base64 图，面板用 <img> 直接显示，用户手机扫码确认，
// 全程不需要本机浏览器，也不需要桌面客户端。与 WPS Comate 的二维码登录同范式（R132）。
//
// # 协议流程（2026-11-15 按 wangchuxiaoji-oss/doubao2api 的 326★ 实现重写）
//
//	① GET www.doubao.com/                        → 拿会话 Cookie（ttwid 等）
//	② GET /passport/safe/csrf_token/?aid=497858  → passport_csrf_token
//	③ GET /passport/web/get_qrcode/?next=…&aid=497858
//	   带头 x-tt-passport-csrf-token                → token + 二维码图
//	④ 轮询 /passport/web/check_qrconnect/?next=…&token=…&aid=497858
//	   状态 new → scanned → confirmed / expired
//	⑤ confirmed 后**跟 redirect_url 走一遍重定向链**才拿得到 sessionid
//
// # ⚠️ 为什么不用另一套（yyydsz/doubao2api，Go/MIT）
//
// 那套走 `accounts.doubao.com` + `aid=582478`（桌面客户端号）+ 无 CSRF 头。
// 本仓初版照抄它，**实测 9 秒后即被上游拒绝**（error_code 2156
// 「服务繁忙，请重启客户端或重装后重试」，见 data/app.log 2026-11-15 16:41）。
// 而 326★ 那套（www.doubao.com + aid=497858 + CSRF 头 + redirect 收 sessionid）
// 是被大规模使用验证过的。注意**登录用网页端 aid、对话用桌面端 aid**并不矛盾：
// 扫码只建立账号级会话（sessionid Cookie），Cookie 与客户端身份无关。
//
// # 与参考实现的差异（本仓补强）
//
//   - 二维码过期自动换新（重新走一遍 CSRF + 申请，参考实现直接报过期）；
//   - 拿到 Cookie 后**必须过额度接口校验**才认（R128 同款纪律）；
//   - 会话状态结构化上报；中间态不得 cancel context（见 setState 注释）。
package login_doubao

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"wild-work/internal/doubao"
)

// 会话状态（与面板 comate 二维码弹层的状态机一致）。
const (
	StateIdle      = "idle"
	StatePending   = "pending"
	StateScanned   = "scanned"
	StateSuccess   = "success"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
)

const (
	// qrTimeout 整体超时。
	qrTimeout = 5 * time.Minute
	// pollInterval 轮询基础间隔。
	//
	// 为什么比参考实现的 1.5s 更保守：2026-11-15 实测，连续 1.5s 轮询在扫码后
	// 立刻被上游判「访问太频繁」（error_code 7），导致永远等不到 confirmed。
	// 2s 起跳 + 错误退避，宁可慢一点也要能走完。
	pollInterval = 2 * time.Second
	// maxPollBackoff 出错时的轮询退避上限。
	maxPollBackoff = 10 * time.Second
	// maxQRRound 最多换几张码。
	maxQRRound = 3
)

// passport 流程常量（网页端身份）。
//
// passportBase 做成 var：测试要把整个流程指到 httptest，否则扫码登录会打到
// 真上游（§0.1：绝不能用真上游做实验，哪怕是登录探测）。
var (
	passportBase = "https://www.doubao.com"
	// passportAID 扫码/登录用**网页端** aid（与对话的桌面端 aid 582478 不同）。
	passportAID = "497858"
	// csrfHeader CSRF 头名。
	csrfHeader = "x-tt-passport-csrf-token"
	// nextParam 登录成功后的回跳地址（上游要求带）。
	nextParam = "https://www.doubao.com"
	// webUserAgent 扫码流程用**纯浏览器** UA（不带 SamanthaDoubao 桌面端标识）。
	webUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
)

// Result 登录成功后的凭据。
type Result struct {
	Cookie string
	UID    string
}

// QRSession 一次扫码登录会话。
type QRSession struct {
	cancel context.CancelFunc

	mu     sync.Mutex
	state  string
	qr     string // data:image/png;base64,…（面板直接 <img src>）
	err    error
	hint   string
	result Result
}

// Snapshot 供 HTTP 状态接口序列化（不含任何凭据）。
func (s *QRSession) Snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]any{"state": s.state, "qr": s.qr}
	if s.err != nil {
		out["error"] = s.err.Error()
	}
	if s.hint != "" {
		out["hint"] = s.hint
	}
	if s.state == StateSuccess {
		out["uid"] = s.result.UID
	}
	return out
}

// State 当前状态。
func (s *QRSession) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Cancel 取消会话。
func (s *QRSession) Cancel() { s.set(StateCancelled, nil, "") }

// set 终态切换：**会 cancel context**（终止后台轮询 goroutine）。
// 仅用于 success / failed / cancelled —— 中间态绝不能用它。
func (s *QRSession) set(state string, err error, hint string) {
	s.mu.Lock()
	s.state, s.err, s.hint = state, err, hint
	s.mu.Unlock()
	s.cancel()
}

// setState 中间态切换：**不 cancel**（轮询 goroutine 还要继续跑）。
//
// 为什么必须分开：早期版本图省事让中间态也走 set()，结果第一次轮询返回
// 「已扫码」就把 context cancel 掉，后台循环下一步就退出，会话永远停在
// scanned —— 用户等到超时也等不到 confirmed（由单测 12s 不落地抓到）。
func (s *QRSession) setState(state, hint string) {
	s.mu.Lock()
	s.state, s.hint = state, hint
	s.mu.Unlock()
}

func qrSessionSetQR(s *QRSession, qr, hint string) {
	s.mu.Lock()
	s.qr, s.state, s.hint = qr, StatePending, hint
	s.mu.Unlock()
}

// succeed 成功终态。
func (s *QRSession) succeed(r Result) {
	s.mu.Lock()
	s.result, s.state, s.err, s.hint = r, StateSuccess, nil, ""
	s.mu.Unlock()
}

// qrHTTPClient 扫码/轮询用的 HTTP client（测试可替换）。
var qrHTTPClient = &http.Client{Timeout: 20 * time.Second}

// verifyCookie 登录态校验接缝（测试可替换，避免打到真上游额度接口）。
var verifyCookie = doubao.VerifyCookie

// 包级当前会话（App 层 loginBusy 保证串行）。
var (
	mu      sync.Mutex
	current *QRSession
)

// Start 发起扫码登录。
//
// onFound 在凭据校验通过后回调，由调用方落盘 + 重载账号池；
// 返回 error 表示这个凭据不可用（此时会话转 failed）。
func Start(onFound func(Result) error) (*QRSession, error) {
	mu.Lock()
	if current != nil {
		if st := current.State(); st == StatePending || st == StateScanned {
			mu.Unlock()
			return nil, fmt.Errorf("已有豆包扫码登录进行中，请先完成或取消")
		}
	}
	mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), qrTimeout+30*time.Second)
	s := &QRSession{cancel: cancel, state: StatePending, hint: "请用豆包 App 扫码登录"}
	mu.Lock()
	current = s
	mu.Unlock()

	flow, err := newPassportFlow(ctx)
	if err != nil {
		cancel()
		s.set(StateFailed, err, "")
		return nil, err
	}
	token, qr, err := flow.requestQR(ctx)
	if err != nil {
		cancel()
		s.set(StateFailed, err, "")
		return nil, err
	}
	qrSessionSetQR(s, qr, "请用豆包 App 扫码登录")
	log.Printf("doubao 扫码登录：二维码已生成 token_len=%d qr_len=%d csrf=%t",
		len(token), len(qr), flow.csrf != "")

	go s.run(ctx, flow, token, onFound)
	return s, nil
}

// Shutdown 取消当前会话（幂等）。
func Shutdown() {
	mu.Lock()
	s := current
	mu.Unlock()
	if s != nil {
		s.Cancel()
	}
}

// run 轮询扫码状态；过期自动换码。
func (s *QRSession) run(ctx context.Context, flow *passportFlow, token string, onFound func(Result) error) {
	defer s.cancel()

	deadline := time.Now().Add(qrTimeout)
	round := 0
	backoff := time.Duration(0)
	for {
		if s.State() == StateCancelled {
			return
		}
		if time.Now().After(deadline) {
			s.set(StateFailed, fmt.Errorf("等待扫码超时（%v）", qrTimeout), "")
			return
		}
		status, cookie, raw, err := flow.pollQR(ctx, token)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("doubao 扫码登录：轮询出错 err=%v", err)
			s.set(StateFailed, err, "")
			return
		}
		// 每轮都记：未映射的状态值 + 原始响应片段（截断）是定位「扫码没反应」的唯一依据。
		log.Printf("doubao 扫码登录：轮询 status=%q raw=%s", status, truncate(raw, 200))
		// 上游回错误信封（如 error_code 7「访问太频繁」）时退避，**不**按原间隔继续撞
		// —— 2026-11-15 实测：不退避会把会话钉死在限流态，永远等不到 confirmed。
		if isUpstreamErrorEnvelope(raw) {
			if backoff == 0 {
				backoff = 2 * pollInterval
			} else if backoff < maxPollBackoff {
				backoff *= 2
			}
			log.Printf("doubao 扫码登录：上游返回错误信封，退避 %v", backoff)
		} else if backoff > 0 {
			backoff = 0 // 恢复正常后立刻回到基础间隔
		}
		switch status {
		case "confirmed":
			if cookie == "" {
				s.set(StateFailed, fmt.Errorf("扫码已确认但上游未下发 Cookie"), "")
				return
			}
			s.setState(StateScanned, "扫码成功，正在校验登录态…")
			log.Printf("doubao 扫码登录：已确认，Cookie 长度=%d sessionid=%t，开始校验登录态",
				len(cookie), strings.Contains(cookie, "sessionid="))
			if verr := verifyCookie(cookie); verr != nil {
				log.Printf("doubao 扫码登录：登录态校验未通过 err=%v", verr)
				s.set(StateFailed, fmt.Errorf("登录态校验未通过：%v", verr), "")
				return
			}
			log.Printf("doubao 扫码登录：登录态校验通过")
			res := Result{Cookie: cookie, UID: doubao.UIDOfCookie(cookie)}
			if ferr := onFound(res); ferr != nil {
				s.set(StateFailed, ferr, "")
				return
			}
			s.succeed(res)
			return
		case "scanning", "scanned":
			s.setState(StateScanned, "已扫码，请在手机上确认")
		case "expired":
			round++
			if round > maxQRRound {
				s.set(StateFailed, fmt.Errorf("二维码多次过期，请重新发起登录"), "")
				return
			}
			nt, nqr, nerr := flow.requestQR(ctx)
			if nerr != nil {
				s.set(StateFailed, nerr, "")
				return
			}
			token = nt
			qrSessionSetQR(s, nqr, "二维码已过期，已自动更换，请重新扫码")
		}
		wait := pollInterval + backoff
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// isUpstreamErrorEnvelope 判断一轮轮询响应是不是「错误信封」而非正常状态帧。
//
// 判据：message != "success" 或 data.error_code != 0。正常帧长这样：
// {"data":{"status":"new","error_code":0,…},"message":"success"}
func isUpstreamErrorEnvelope(raw string) bool {
	var env struct {
		Message string `json:"message"`
		Data    struct {
			ErrorCode int `json:"error_code"`
		} `json:"data"`
	}
	if json.Unmarshal([]byte(raw), &env) != nil {
		return false // 非 JSON：不猜，交给状态映射
	}
	return env.Message != "success" || env.Data.ErrorCode != 0
}

// ---------- passport 流程 ----------

// passportFlow 一次扫码流程的会话（Cookie jar + CSRF token 跨请求复用）。
type passportFlow struct {
	cookies map[string]string
	csrf    string
}

// newPassportFlow 建会话 + 取 CSRF token。
func newPassportFlow(ctx context.Context) (*passportFlow, error) {
	f := &passportFlow{cookies: map[string]string{}}
	// ① 主页：建立会话 Cookie（ttwid 等）。失败不致命，继续尝试拿 CSRF。
	if _, _, err := f.get(ctx, passportBase+"/"); err != nil {
		log.Printf("doubao 扫码登录：访问主页失败（继续尝试）err=%v", err)
	}
	// ② CSRF token：JSON 的 data.passport_csrf_token 优先，回落 Cookie。
	body, _, err := f.get(ctx, fmt.Sprintf("%s/passport/safe/csrf_token/?aid=%s", passportBase, passportAID))
	if err != nil {
		return nil, fmt.Errorf("获取 CSRF token 失败: %w", err)
	}
	var payload struct {
		Data struct {
			PassportCSRFToken string `json:"passport_csrf_token"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &payload)
	f.csrf = payload.Data.PassportCSRFToken
	if f.csrf == "" {
		f.csrf = f.cookies["passport_csrf_token"]
	}
	if f.csrf == "" {
		return nil, fmt.Errorf("上游未下发 passport_csrf_token")
	}
	return f, nil
}

// requestQR 申请一张二维码，返回 (token, dataURL)。
func (f *passportFlow) requestQR(ctx context.Context) (string, string, error) {
	u := fmt.Sprintf("%s/passport/web/get_qrcode/?next=%s&aid=%s",
		passportBase, url.QueryEscape(nextParam), passportAID)
	body, _, err := f.get(ctx, u)
	if err != nil {
		return "", "", fmt.Errorf("请求二维码失败: %w", err)
	}
	var payload struct {
		Message string `json:"message"`
		Data    struct {
			ErrorCode   int    `json:"error_code"`
			Description string `json:"description"`
			Token       string `json:"token"`
			Qrcode      string `json:"qrcode"`
			QrcodeURL   string `json:"qrcode_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return "", "", fmt.Errorf("二维码响应解析失败: %s", truncate(body, 200))
	}
	if payload.Data.ErrorCode != 0 || payload.Data.Token == "" {
		msg := payload.Data.Description
		if msg == "" {
			msg = payload.Message
		}
		if msg == "" {
			msg = truncate(body, 200)
		}
		return "", "", fmt.Errorf("上游拒绝生成二维码: %s", msg)
	}
	img := payload.Data.Qrcode
	if img == "" {
		img = payload.Data.QrcodeURL
	}
	// 实测：qrcode 字段是**裸 base64**（不是 data URI），qrcode_url 是 http 链接。
	switch {
	case strings.HasPrefix(img, "data:"):
	case strings.HasPrefix(img, "http"):
		raw, _, derr := f.get(ctx, img)
		if derr != nil {
			return "", "", fmt.Errorf("下载二维码图片失败: %w", derr)
		}
		img = "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(raw))
	default:
		img = "data:image/png;base64," + img
	}
	return payload.Data.Token, img, nil
}

// pollQR 轮询一次扫码状态。confirmed 时同时返回 Cookie 与原始响应（供日志定位）。
func (f *passportFlow) pollQR(ctx context.Context, token string) (status, cookie, raw string, err error) {
	u := fmt.Sprintf("%s/passport/web/check_qrconnect/?next=%s&token=%s&aid=%s",
		passportBase, url.QueryEscape(nextParam), url.QueryEscape(token), passportAID)
	body, _, err := f.get(ctx, u)
	if err != nil {
		return "", "", "", fmt.Errorf("轮询扫码状态失败: %w", err)
	}
	var payload struct {
		Message string `json:"message"`
		Data    struct {
			ErrorCode   int    `json:"error_code"`
			Description string `json:"description"`
			Status      string `json:"status"`
			RedirectURL string `json:"redirect_url"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(body), &payload)
	st := normalizeStatus(payload.Data.Status)
	if st == "confirmed" {
		// ⑤ 跟 redirect 走一遍，sessionid 是在这条链上才下发的。
		if payload.Data.RedirectURL != "" {
			if _, _, rerr := f.get(ctx, payload.Data.RedirectURL); rerr != nil {
				log.Printf("doubao 扫码登录：跟随登录重定向失败（用已收 Cookie 继续）err=%v", rerr)
			}
		}
		return "confirmed", f.cookieHeader(), body, nil
	}
	if st == "" && payload.Data.Description != "" {
		return "waiting", "", body, nil
	}
	return st, "", body, nil
}

// get 带会话 Cookie 与 CSRF 头发一次 GET，并把新 Set-Cookie 并回 jar。
func (f *passportFlow) get(ctx context.Context, rawURL string) (string, map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("User-Agent", webUserAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", passportBase+"/chat/login")
	req.Header.Set("Origin", passportBase)
	if f.csrf != "" {
		req.Header.Set(csrfHeader, f.csrf)
	}
	f.muCookies(func() { req.Header.Set("Cookie", cookieHeader(f.cookies)) })

	resp, err := qrHTTPClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", nil, err
	}
	newCK := map[string]string{}
	for _, c := range resp.Cookies() {
		if c.Name == "" || c.Value == "" {
			continue
		}
		newCK[c.Name] = c.Value
	}
	f.muCookies(func() {
		for k, v := range newCK {
			f.cookies[k] = v
		}
	})
	return string(body), newCK, nil
}

// muCookies 串行化 cookie jar 的读写（轮询 goroutine 与换码路径都可能碰它）。
func (f *passportFlow) muCookies(fn func()) {
	cookieMu.Lock()
	defer cookieMu.Unlock()
	fn()
}

var cookieMu sync.Mutex

// cookieHeader 把 jar 拼成请求头格式。
func cookieHeader(jar map[string]string) string {
	if len(jar) == 0 {
		return ""
	}
	parts := make([]string, 0, len(jar))
	for k, v := range jar {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "; ")
}

// cookieHeaderOf 当前 jar 的请求头形态（confirmed 时取用）。
func (f *passportFlow) cookieHeader() string {
	return cookieHeader(f.cookies)
}

// collectCookies 把响应里的 Set-Cookie 拼成请求头格式（保留供测试使用）。
func collectCookies(resp *http.Response) string {
	parts := make([]string, 0, 8)
	for _, c := range resp.Cookies() {
		if c.Name == "" {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// normalizeStatus 把 passport 返回的状态值归一化。
func normalizeStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "new", "1", "0", "waiting":
		return "waiting"
	case "scanning", "scanned", "2", "5":
		return "scanning"
	case "confirmed", "success", "3", "6":
		return "confirmed"
	case "expired", "4", "7":
		return "expired"
	case "canceled", "cancelled", "8":
		return "cancelled"
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
