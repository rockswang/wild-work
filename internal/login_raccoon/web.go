// web.go 小浣熊「网页版」自动登录。
//
// 流程（2026-10-10 实盘验证）：
//
//	① 拉起一个**独立 profile** 的 Edge/Chrome（带 CDP 调试端口），打开小浣熊网页版登录页
//	② 用户在真实浏览器里正常登录（扫码 / 手机号 / 微信均可）
//	③ 登录成功后前端 JS 会把会话写进 Cookie `xhx_token`（域 xiaohuanxiong.com）
//	④ 本工具经 CDP 轮询 Cookie，捕获到**真实账号**（owner_type=users）的 refresh_token
//	⑤ 用该 refresh_token 调 /api/web/auth/v1/refresh 换取新 access+refresh（同时完成有效性校验）
//	⑥ 落盘 auths/raccoon-<uid>.json → 关闭浏览器、清理临时 profile
//
// 为什么改用网页版（替代原「桌面协议回调」）：
//   - 原方式要改写 HKCU 的 office-raccoon 注册表，**仅 Windows 可用**，且要求本机装有官方客户端；
//     网页版流程纯 HTTP + 浏览器，Windows/macOS/Linux 通用。
//   - 网页版登录签发**独立会话**（新 sid），与日常浏览器 / 官方客户端各持一份凭据。
//
// ⚠️ 互踢提示（面板弹窗已告知用户）：上游 refresh 会**轮换 refresh_token**。若日常浏览器
// 与本工具**共用同一份凭据**（例如把浏览器 Cookie 里的 refresh_token 手工粘进来），
// 本工具刷新后那个浏览器会话即失效。走本流程登录会新建独立会话；但若上游对账号限制
// 单会话，仍可能导致日常浏览器的登录态失效。
//
// 为什么用独立 profile：日常 profile 被浏览器独占锁、且会污染用户登录态；独立 profile
// 天然完全隔离，多账号逐个添加互不干扰（与智谱清言同一套做法，见 internal/login_glm/auto.go）。
package login_raccoon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"wild-work/internal/cdp"
	"wild-work/internal/raccoon"
)

// WebLoginURL 小浣熊网页版登录入口（2026-10-15 上游迁站后）。
//
// 旧入口 office.xiaohuanxiong.com/home?loginModal=true 已 302 到
// xiaohuanxiong.com/work/home?loginModal=true，而该参数在新版前端会引发
// 「/work/home?loginModal=true ↔ /login?redirect_url=…loginModal=true」重定向死循环，
// Cookie 永远写不进来（表现为面板窗口「登录成功那一刻起一直跳转」）。
// 故直接开登录页：product_code=work 定位网页版产品，redirect_url 指向工作台。
// **redirect_url 不得带 loginModal=true**。
const WebLoginURL = "https://xiaohuanxiong.com/login?product_code=work&redirect_url=https%3A%2F%2Fxiaohuanxiong.com%2Fwork%2Fhome"

// CookieName 网页版凭据所在的 Cookie 名。
//
// 实测（2026-10-15 CDP 探针）：新版把**登录态**凭据写在 `xhx_token`（refresh_token，
// ≈30 天），而旧的 `raccoon_refresh_token` 在**访客态**仍会下发 —— 故监听旧名会一打开
// 就抓到访客凭据（R28 同款教训）。`xhx_token` 仅登录后出现，是更干净的判据。
const CookieName = "xhx_token"

// CookieDomain 匹配用域名片段。
const CookieDomain = "xiaohuanxiong.com"

// LoginTimeout 等待用户完成登录的最长时间。
const LoginTimeout = 10 * time.Minute

// ownerTypeUsers 真实登录账号的 owner_type（访客为 visitors）。
const ownerTypeUsers = "users"

// Result 登录成功后拿到的凭据（与「从本机客户端导入」落盘形态一致）。
type Result struct {
	AccessToken    string
	RefreshToken   string
	OfficeIdentity string
	OfficeOrgName  string
	ExpiresAt      int64
}

// WebSession 一次网页版自动登录会话。
type WebSession struct {
	browser *cdp.Browser
	cancel  context.CancelFunc
	mu      sync.Mutex
	status  string // pending / success / failed / cancelled
	err     error
	result  Result
}

// Status 当前状态。
func (s *WebSession) Status() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.err
}

// Result 成功时返回的账号信息。
func (s *WebSession) Result() (Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == "success" {
		return s.result, true
	}
	return Result{}, false
}

// setStatus 内部状态更新。
func (s *WebSession) setStatus(status string, err error) {
	s.mu.Lock()
	s.status = status
	s.err = err
	s.mu.Unlock()
}

// Cancel 取消自动登录并关闭浏览器。
func (s *WebSession) Cancel() {
	s.setStatus("cancelled", nil)
	s.cancel()
}

// StartWebLogin 拉起独立 profile 的浏览器并在后台等待网页版登录完成。
//
// 返回的 WebSession 立即可用（状态 pending）；调用方轮询 Status()/Result()。
// onSaved 在凭据获取成功后回调（用于落盘与账号池重载），可为 nil。
//
// ⚠️ 与智谱清言同理：小浣熊对**未登录访客**也会下发凭据（旧名 `raccoon_refresh_token`，
// owner_type=visitors）。若监听旧名并以「Cookie 出现」为完成判据，会抓到一个不可用的访客凭据。
// 新版（2026-10-15）登录态凭据改名为 `xhx_token`，访客态不写它，故监听 `xhx_token` 即为
// 干净判据；同时保留 owner_type 兜底——上游若显式回 visitors 仍拒绝。
func StartWebLogin(onSaved func(Result)) (*WebSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), LoginTimeout+2*time.Minute)

	browser, err := cdp.Launch(ctx, cdp.Options{StartURL: WebLoginURL})
	if err != nil {
		cancel()
		return nil, err
	}

	s := &WebSession{browser: browser, cancel: cancel, status: "pending"}

	go func() {
		defer cancel()
		defer func() {
			if cerr := browser.Close(); cerr != nil {
				log.Printf("raccoon 网页版登录：关闭浏览器失败: %v", cerr)
			}
		}()

		log.Printf("raccoon 网页版登录：已拉起浏览器（端口 %d），等待用户登录…", browser.Port())

		deadline := time.Now().Add(LoginTimeout)
		var lastTok string
		var lastErr error
		sawVisitor := false

		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				s.setStatus("cancelled", nil)
				log.Printf("raccoon 网页版登录：已取消")
				return
			default:
			}

			tok, werr := currentRefreshToken(ctx, browser)
			if werr != nil {
				lastErr = werr
			} else if tok != "" && tok != lastTok {
				lastTok = tok
				// 登录判据：新版访客态**不写** `xhx_token`（2026-10-15 实测），故 cookie 出现
				// 即视为登录完成。旧版靠 JWT `owner_type==users` 区分访客，但新版 JWT 已是标准
				// OIDC（sid/ver/iss/sub/aud/exp/nbf/iat/jti，**无 owner_type**）——故仅当上游
				// **显式**声明 visitors 时才拒绝，缺省即放行。
				if ot := jwtOwnerType(tok); ot != "" && ot != ownerTypeUsers {
					// 访客凭据：用户尚未登录，继续等
					if !sawVisitor {
						sawVisitor = true
						log.Printf("raccoon 网页版登录：当前为访客凭据（用户尚未登录），继续等待…")
					}
				} else {
					// 真实账号：用 refresh 换取新令牌（同时校验有效）
					access, refresh, verr := raccoon.New().RefreshWithToken(tok)
					if verr != nil {
						lastErr = verr
						log.Printf("raccoon 网页版登录：换取令牌失败: %v", verr)
					} else {
						r := Result{
							AccessToken:  access,
							RefreshToken: refresh,
							ExpiresAt:    jwtExp(access),
						}
						log.Printf("raccoon 网页版登录：已捕获账号 uid=%s", jwtName(access))
						s.mu.Lock()
						s.result, s.status = r, "success"
						s.mu.Unlock()
						if onSaved != nil {
							onSaved(r)
						}
						return
					}
				}
			}

			select {
			case <-ctx.Done():
				s.setStatus("cancelled", nil)
				return
			case <-time.After(2 * time.Second):
			}
		}

		msg := "等待登录超时"
		switch {
		case sawVisitor:
			msg = "等待登录超时：检测到访客凭据，但未检测到已登录的真实账号。请确认已在弹出的浏览器窗口中完成登录（扫码/手机号/微信）"
		case lastErr != nil:
			msg = fmt.Sprintf("等待登录超时：%v", lastErr)
		}
		s.setStatus("failed", errors.New(msg))
		log.Printf("raccoon 网页版登录：%s", msg)
	}()

	return s, nil
}

// currentRefreshToken 读一次当前 profile 的 xhx_token（无则空串）。
func currentRefreshToken(ctx context.Context, browser *cdp.Browser) (string, error) {
	cookies, err := browser.AllCookies(ctx, CookieDomain)
	if err != nil {
		return "", err
	}
	for _, c := range cookies {
		if c.Name == CookieName && strings.TrimSpace(c.Value) != "" &&
			strings.Contains(c.Domain, CookieDomain) {
			return c.Value, nil
		}
	}
	return "", nil
}

// jwtClaims 解出 JWT 的 payload（失败返回 nil）。
func jwtClaims(tok string) map[string]any {
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) < 2 {
		return nil
	}
	seg := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
	if m := len(seg) % 4; m != 0 {
		seg += strings.Repeat("=", 4-m)
	}
	raw, err := base64.StdEncoding.DecodeString(seg)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

// jwtOwnerType 取 JWT 的 owner_type（users / visitors）。
func jwtOwnerType(tok string) string {
	if s, ok := jwtClaims(tok)["owner_type"].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// jwtName 取 JWT 里的 uid：委托给 raccoon.JWTUID（唯一取数点）——
// 旧版取 name claim，新版标准 OIDC（2026-10-15 上游迁站后）无 name、回落 sub。
// 本处保留同名包装只是为了调用点可读，不再各自重复解析逻辑。
func jwtName(tok string) string { return raccoon.JWTUID(tok) }

// jwtExp 取 JWT 的 exp（Unix 秒）；解析失败返回 0。
func jwtExp(tok string) int64 {
	if f, ok := jwtClaims(tok)["exp"].(float64); ok {
		return int64(f)
	}
	return 0
}
