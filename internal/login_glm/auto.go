// auto.go 智谱清言「自动获取登录态」流程。
//
// 背景：清言的凭据是浏览器 Cookie 里的 chatglm_refresh_token，官方没有可编程登录接口。
// 手工路径（F12 → Application → Cookies → 复制粘贴）繁琐且容易出错，故提供自动路径：
//
//	① 拉起一个**独立 profile** 的 Edge/Chrome（带 CDP 调试端口）
//	② 浏览器打开 chatglm.cn，用户在真实浏览器里正常登录
//	③ wild-work 经 CDP 轮询 Cookie，一旦 chatglm_refresh_token 出现即捕获
//	④ 验证凭据有效 → 落盘 → 关闭浏览器、清理临时 profile
//
// 为什么用独立 profile 而不是用户日常 profile / InPrivate：
//   - 日常 profile 被浏览器独占锁（实测读不到 Cookie 库），且会污染用户登录态；
//   - 独立 profile 天然完全隔离，**多账号逐个添加互不干扰**——每次添加都是全新环境，
//     不需要用户手动开无痕窗口。这比 InPrivate 更适合本场景。
package loginglm

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"wild-work/internal/cdp"
)

// CookieName 清言凭据所在的 Cookie 名。
const CookieName = "chatglm_refresh_token"

// CookieDomain 匹配用域名片段。
const CookieDomain = "chatglm.cn"

// LoginTimeout 等待用户完成登录的最长时间。
const LoginTimeout = 10 * time.Minute

// AutoSession 一次自动登录会话。
type AutoSession struct {
	browser *cdp.Browser
	cancel  context.CancelFunc
	mu      sync.Mutex
	status  string // pending / success / failed / cancelled
	err     error
	result  Result
}

// Status 当前状态。
func (s *AutoSession) Status() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, s.err
}

// Result 成功时返回的账号信息。
func (s *AutoSession) Result() (Result, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == "success" {
		return s.result, true
	}
	return Result{}, false
}

// setStatus 内部状态更新。
func (s *AutoSession) setStatus(status string, err error) {
	s.mu.Lock()
	s.status = status
	s.err = err
	s.mu.Unlock()
}

// AutoLogin 拉起浏览器并在后台等待登录完成。
//
// 返回的 AutoSession 立即可用（状态 pending）；调用方轮询 Status() 或 Result()。
// 登录成功后**自动落盘并关闭浏览器**。
//
// authDir 为凭证目录；onSaved 在落盘成功后回调（用于触发账号池重载），可为 nil。
//
// ⚠️ 关键设计：清言对新访客会自动下发**访客凭据**，所以「Cookie 出现」不等于
// 「用户登录完成」。若一见到 Cookie 就收工，会抓到一个不可用的访客 token
// （实测：新 profile 打开 chatglm.cn 立刻就有 chatglm_refresh_token）。
// 因此这里以**「能换出真实账号的 access_token」**为完成判据：
// 轮询 Cookie → 尝试验证 → 只有验证通过（非访客）才算成功。
func AutoLogin(authDir string, onSaved func(Result)) (*AutoSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), LoginTimeout+2*time.Minute)

	browser, err := cdp.Launch(ctx, cdp.Options{StartURL: "https://chatglm.cn"})
	if err != nil {
		cancel()
		return nil, err
	}

	s := &AutoSession{browser: browser, cancel: cancel, status: "pending"}

	go func() {
		defer cancel()
		defer func() {
			if cerr := browser.Close(); cerr != nil {
				log.Printf("glm 自动登录：关闭浏览器失败: %v", cerr)
			}
		}()

		log.Printf("glm 自动登录：已拉起浏览器（端口 %d），等待用户在浏览器中登录…", browser.Port())

		deadline := time.Now().Add(LoginTimeout)
		var lastToken string
		var lastErr error
		sawGuest := false

		for time.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				s.setStatus("cancelled", nil)
				log.Printf("glm 自动登录：已取消")
				return
			default:
			}

			// 取当前 Cookie
			token, werr := currentRefreshToken(ctx, browser)
			if werr != nil {
				lastErr = werr
			} else if token != "" && token != lastToken {
				// Cookie 变了（首次出现或刷新）→ 尝试验证
				lastToken = token
				r, verr := Validate(token)
				if verr == nil {
					// 验证通过 = 真实账号，落盘并收工
					if _, serr := SaveAuth(authDir, r); serr != nil {
						s.setStatus("failed", fmt.Errorf("保存凭证失败：%w", serr))
						log.Printf("glm 自动登录：保存凭证失败: %v", serr)
						return
					}
					log.Printf("glm 自动登录：账号已添加 uid=%s nickname=%s", r.UID, r.Nickname)
					s.mu.Lock()
					s.result = r
					s.status = "success"
					s.mu.Unlock()
					if onSaved != nil {
						onSaved(r)
					}
					return
				}
				// 验证失败：多为访客凭据，继续等用户登录
				lastErr = verr
				if !sawGuest {
					sawGuest = true
					log.Printf("glm 自动登录：当前为访客凭据（用户尚未登录），继续等待…")
				}
			}

			select {
			case <-ctx.Done():
				s.setStatus("cancelled", nil)
				return
			case <-time.After(2 * time.Second):
			}
		}

		// 超时：给出可读原因
		msg := "等待登录超时"
		if sawGuest {
			msg = "等待登录超时：检测到访客凭据，但未检测到已登录的真实账号。请确认已在弹出的浏览器窗口中完成登录（扫码/手机号/微信）"
		} else if lastErr != nil {
			msg = fmt.Sprintf("等待登录超时：%v", lastErr)
		}
		s.setStatus("failed", errors.New(msg))
		log.Printf("glm 自动登录：%s", msg)
	}()

	return s, nil
}

// currentRefreshToken 读一次当前 profile 的 chatglm_refresh_token（无则空串）。
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

// Cancel 取消自动登录并关闭浏览器。
func (s *AutoSession) Cancel() {
	s.setStatus("cancelled", nil)
	s.cancel()
}

// ValidateAndSave 手工路径：验证用户粘贴的 refresh_token 并落盘。
// 自动路径失败或用户偏好手工时使用。
func ValidateAndSave(authDir, refreshToken string) (Result, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return Result{}, fmt.Errorf("refresh_token 不能为空")
	}
	r, err := Validate(refreshToken)
	if err != nil {
		return Result{}, err
	}
	if _, err := SaveAuth(authDir, r); err != nil {
		return Result{}, err
	}
	return r, nil
}
