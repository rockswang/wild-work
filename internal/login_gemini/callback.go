// callback.go 本机回调 server：收 `code` 前**强制校验 state**。
//
// 安全边界（必须在注释里写清，别以为它有更多保护）：
//   - 只监听 `127.0.0.1` 与 `[::1]`（协议 §2.3：loopback 回调，局域网不可达）；
//   - 唯一防伪手段是 16 字节随机 `state`（回调里逐字比对，不匹配回 400 并拒绝）；
//   - 回调页是**同一台机器上的任何进程**都能访问的（这是所有本机回调 OAuth 的固有性质），
//     本包不额外加"一次性本地 token"之类的自造机制：那会偏离参考实现，
//     且 attacker 若已能在本机起进程，直接读 auths/*.json 更省事。
package login_gemini

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
)

// handleCallback 处理 `GET /oauth-callback?code=…&state=…`。
//
// 顺序刻意如此（参考实现 gemini-oauth.ts:482-508 同序）：
//  1. 先校验 state —— 不匹配**立即 400 并拒绝**，绝不把 code 交给后续流程；
//  2. 再看 `error` 参数（用户在授权页拒绝授权）；
//  3. 最后取 code。
func (s *loginSession) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// 1. state 逐字比对。**区分"缺失"与"不匹配"**只为日志可读，处置相同：400 + 拒绝。
	got := q.Get("state")
	if got == "" {
		s.reject(w, "回调缺少 state 参数，已拒绝")
		return
	}
	if got != s.sessionState() {
		s.reject(w, "state 不匹配，已拒绝该回调")
		return
	}

	// 2. 用户取消 / 上游报错：state 正确时才算"我们这条流程"的失败，可以回显 error 文案。
	if e := q.Get("error"); e != "" {
		desc := q.Get("error_description")
		if desc == "" {
			desc = e
		} else {
			desc = desc + " (" + e + ")"
		}
		s.fail(desc)
		writePage(w, http.StatusOK, callbackPage(false, desc))
		return
	}

	// 3. 取 code 并置入内存，由 Poll 立即兑换。
	code := q.Get("code")
	if code == "" {
		s.fail("回调缺少 code 参数")
		writePage(w, http.StatusOK, callbackPage(false, "回调缺少 code 参数"))
		return
	}
	if !s.set(code) {
		// 同一条流程收到第二个 code：保留第一个（已被 Poll 取走或即将取走），
		// 避免后到的把先到的覆盖掉。浏览器刷新/预取可能触发，属正常现象。
		writePage(w, http.StatusOK, callbackPage(true, ""))
		return
	}
	// 注意：此刻**尚未**完成兑换（Poll 随后立即做），故页面文案是「已收到授权」
	// 而不是「登录成功」——避免兑换失败时误导用户。
	writePage(w, http.StatusOK, callbackPage(true, ""))
}

// sessionState 读一次会话里的 state（回调 goroutine 与 Start 不同 goroutine，需加锁）。
func (s *loginSession) sessionState() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.State
}

// parseCallbackURL 从用户粘贴的内容中提取授权码、state 与错误。
//
// 兼容支持：
//  1. 完整回调 URL（如 https://antigravity.google/oauth-callback?state=...&code=4/0AX...）
//  2. 查询串（如 ?code=...&state=...）
//  3. 官方授权完成页面上展示并提供一键复制的裸授权码（如 4/0AX...）
func parseCallbackURL(raw string) (code, state, errName, errDesc string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", "", ""
	}

	// 1. 尝试作为完整 URL 解析
	if u, err := url.Parse(trimmed); err == nil && (u.Scheme != "" || u.Host != "" || strings.Contains(trimmed, "?") || strings.Contains(trimmed, "#")) {
		q := u.Query()
		// 如果 query 中没有 code，但 hash 中有，尝试从 hash 中解析参数
		if q.Get("code") == "" && u.Fragment != "" {
			frag := strings.TrimPrefix(u.Fragment, "#")
			if fq, ferr := url.ParseQuery(frag); ferr == nil {
				for k, vs := range fq {
					if len(vs) > 0 && q.Get(k) == "" {
						q[k] = vs
					}
				}
			}
		}
		if q.Get("code") != "" || q.Get("error") != "" {
			return q.Get("code"), q.Get("state"), q.Get("error"), q.Get("error_description")
		}
	}

	// 2. 尝试作为 url.Values 解析 (如直接粘贴 code=...&state=...)
	if strings.Contains(trimmed, "=") {
		if q, err := url.ParseQuery(strings.TrimPrefix(trimmed, "?")); err == nil {
			if q.Get("code") != "" || q.Get("error") != "" {
				return q.Get("code"), q.Get("state"), q.Get("error"), q.Get("error_description")
			}
		}
	}

	// 3. 裸授权码（官方 antigravity 页面复制的 code 或直接复制的 authorization code）
	return trimmed, "", "", ""
}

// SubmitCallbackURL 手动提交浏览器跳转后的回调地址或复制的授权码。
//
// 兼顾两种交互形态：
//  - 若粘贴完整 URL 且携带 state，进行严格逐字比对（防串号与跨请求错乱）
//  - 若直接粘贴授权码页面上复制的 code（如 4/0AX...），直接提取并进入兑换流程
func SubmitCallbackURL(raw string) error {
	sess := currentSession()
	if sess == nil {
		return errors.New("没有进行中的登录流程（可能已完成或已超时），请重新发起登录")
	}

	code, gotState, errName, errDesc := parseCallbackURL(raw)

	// 1. 若携带了 state，做严格逐字比对（防串号或误贴历史地址）
	if gotState != "" && gotState != sess.sessionState() {
		m := "state 不匹配，已拒绝该回调（多半是上一次登录遗留的地址，请用本次登录后跳转的那条完整 URL）"
		sess.fail(m)
		return errors.New(m)
	}

	// 2. 用户取消 / 上游报错
	if errName != "" {
		desc := firstNonEmpty(errDesc, errName)
		sess.fail(desc)
		return fmt.Errorf("登录失败: %s", desc)
	}

	// 3. 提取 code
	if code == "" {
		const m = "未从输入中识别出授权码：请粘贴授权码或含 code=… 的完整 URL"
		sess.fail(m)
		return errors.New(m)
	}
	sess.set(code)
	return nil
}

// firstNonEmpty 取第一个非空串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// reject 对 state 校验失败的回调回 400 并拒绝（不写入任何 pending code）。
func (s *loginSession) reject(w http.ResponseWriter, msg string) {
	s.fail(msg) // 记入 pending 让 Poll 报出可行动的错误，但不写入 code
	writePage(w, http.StatusBadRequest, callbackPage(false, msg))
}

// set 写入授权码；已有一个码时返回 false（不覆盖）。
func (s *loginSession) set(code string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending.code != "" {
		return false
	}
	s.pending.code, s.pending.received = code, true
	return true
}

// fail 记录失败文案（state 校验失败或用户拒绝）。
func (s *loginSession) fail(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending.code != "" {
		return // 已经拿到码，失败文案不该覆盖成功路径
	}
	s.pending.errMsg, s.pending.received = msg, true
}

// writePage 输出回调页。
func writePage(w http.ResponseWriter, status int, htmlBody string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprint(w, htmlBody)
}

// callbackPage 渲染本机回调响应页（成功/失败两态）。
func callbackPage(ok bool, msg string) string {
	if ok {
		return fmt.Sprintf(callbackPageBase, "ok", "✓", "已收到授权",
			"正在换取凭据…请回到 wild-work 面板查看账号。<br>本页面即将自动关闭…",
			"true", 1500, 2500)
	}
	return fmt.Sprintf(callbackPageBase, "err", "✕", "登录失败",
		`<span class="msg">`+html.EscapeString(msg)+`</span>`,
		"false", 0, 800)
}

// callbackPageBase 统一模板（%% 是 fmt 占位符的转义）。
const callbackPageBase = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>wild-work 登录回调</title>
<style>
  :root { color-scheme: light; }
  * { margin: 0; padding: 0; box-sizing: border-box; }
  html, body { height: 100%%; }
  body {
    display: flex; align-items: center; justify-content: center;
    font-family: -apple-system, "Segoe UI", "PingFang SC", "Microsoft YaHei", sans-serif;
    background: linear-gradient(160deg, #f0f9ff 0%%, #e0f2fe 100%%);
  }
  .card {
    width: 360px; padding: 36px 32px 28px; text-align: center;
    background: #fff; border-radius: 16px;
    box-shadow: 0 10px 40px rgba(14, 116, 144, .14);
  }
  .icon {
    width: 64px; height: 64px; margin: 0 auto 18px; border-radius: 50%%;
    display: flex; align-items: center; justify-content: center;
    font-size: 34px; color: #fff;
  }
  .ok .icon { background: #0e7490; }
  .err .icon { background: #dc2626; }
  h1 { font-size: 20px; color: #0f172a; margin-bottom: 10px; font-weight: 600; }
  p { font-size: 14px; color: #64748b; line-height: 1.6; margin-bottom: 22px; word-break: break-all; }
  .msg { font-size: 13px; color: #475569; }
  .close-btn {
    display: inline-block; padding: 10px 36px; border: none; border-radius: 8px;
    font-size: 14px; color: #fff; cursor: pointer; transition: opacity .15s;
  }
  .ok .close-btn { background: #0e7490; }
  .err .close-btn { background: #dc2626; }
  .close-btn:hover { opacity: .88; }
  .brand { margin-top: 18px; font-size: 12px; color: #94a3b8; }
</style>
</head>
<body>
  <div class="card %s">
    <div class="icon">%s</div>
    <h1>%s</h1>
    <p>%s</p>
    <button class="close-btn" onclick="tryClose()">立即关闭</button>
    <div class="brand">wild-work · 本页面由本机回调服务生成，可安全关闭</div>
  </div>
<script>
var autoClose = %s;
function tryClose() { window.close(); }
if (autoClose) { setTimeout(tryClose, %d); }
setTimeout(function () {
  var b = document.querySelector('.close-btn');
  if (!window.closed && b) { b.textContent = '关闭此标签页'; }
}, %d);
</script>
</body>
</html>`
