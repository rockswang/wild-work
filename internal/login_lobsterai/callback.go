// callback.go 本机回调 server：收 `code` 并**原样比对 state**。
//
// 安全边界（必须在注释里写清，别以为它有更多保护）：
//   - 只监听 `127.0.0.1`（备忘 104 行），局域网不可达；
//   - 唯一防伪手段是 `state` 随机串（备忘 108 行：「回调时原样比对；不匹配回 400 并拒绝」）；
//   - 回调页是**同一台机器上的任何进程**都能访问的（这是所有本机回调 OAuth 的固有性质），
//     本包不额外加"一次性本地 token"之类的自造机制：那会偏离参考实现，
//     且 attacker 若已能在本机起进程，直接读 auths/*.json 更省事。
package login_lobsterai

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html"
	"net/http"
	"net/url"
)

// handleCallback 处理 `GET /auth/callback?code=...&state=...`。
//
// 顺序刻意如此（与备忘 108 行一致）：
//  1. 先校验 state —— 不匹配**立即 400 并拒绝**，绝不把 code 交给后续流程；
//  2. 再看 `error` 参数（用户在登录页拒绝授权）；
//  3. 最后取 code。
func (s *loginSession) handleCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// 1. state 原样比对。**区分"缺失"与"不匹配"**只为日志可读，处置相同：400 + 拒绝。
	got := q.Get("state")
	if got == "" {
		s.reject(w, "回调缺少 state 参数，已拒绝")
		return
	}
	if got != s.state.State {
		s.reject(w, "state 不匹配，已拒绝该回调")
		return
	}

	// 2. 用户取消 / 上游报错：state 正确时才是"我们这条流程"的失败，可以回显 error 文案。
	if e := q.Get("error"); e != "" {
		desc := q.Get("error_description")
		if desc == "" {
			desc = e
		} else {
			desc = desc + " (" + e + ")"
		}
		s.fail(desc)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(callbackPage(false, desc)))
		return
	}

	// 3. 取 code 并置入内存，由 Poll 立即 exchange。
	code := q.Get("code")
	if code == "" {
		s.fail("回调缺少 code 参数")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(callbackPage(false, "回调缺少 code 参数")))
		return
	}
	if !s.set(code) {
		// 同一条流程收到第二个 code：保留第一个（已被 Poll 取走或即将取走），
		// 避免后到的把先到的覆盖掉。浏览器刷新/预取可能触发，属正常现象。
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(callbackPage(true, "")))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 注意：此刻**尚未**完成 exchange（Poll 随后立即做），故页面文案是「已收到授权」
	// 而不是「登录成功」——避免兑换失败时误导用户（同 login_qwenwork 的取舍）。
	_, _ = w.Write([]byte(callbackPage(true, "")))
}

// reject 对 state 校验失败的回调回 400 并拒绝（不写入任何 pending 状态）。
func (s *loginSession) reject(w http.ResponseWriter, msg string) {
	s.fail(msg) // 记入 pending 让 Poll 报出可行动的错误，但不写入 code
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(callbackPage(false, msg)))
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

// snapshot 读出当前 pending（code / 错误 / 是否已有结论）。
func (s *loginSession) snapshot() (code, errMsg string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending.code, s.pending.errMsg, s.pending.received
}

// close 关闭回调 server 与监听口（幂等）。
func (s *loginSession) close() {
	s.mu.Lock()
	if s.shutdown {
		s.mu.Unlock()
		return
	}
	s.shutdown = true
	srv, ln := s.srv, s.ln
	s.mu.Unlock()
	if srv != nil {
		_ = srv.Close()
	}
	if ln != nil {
		_ = ln.Close()
	}
}

// replaceSession 安装新会话并关掉旧会话（同一时刻只跑一条登录流程）。
func replaceSession(sess *loginSession) {
	curMu.Lock()
	old := cur
	cur = sess
	curMu.Unlock()
	if old != nil {
		old.close()
	}
}

// currentSession 返回当前会话（无则 nil）。
func currentSession() *loginSession {
	curMu.Lock()
	defer curMu.Unlock()
	return cur
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// randomUUID 生成随机 UUID v4 字符串（与 login_qwenwork.uuid4Hex 同实现）。
// 用途有两处且**互不相干**：state（回调比对）与登录会话的安装 UUID（凭据载荷）。
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// urlQueryEscape 对 query 值做转义。
// 用 url.QueryEscape 而非 PathEscape：redirect_uri 里的 `:` `/` 必须被转义，
// 否则登录页解析 query 时会把 `http://127.0.0.1:port/auth/callback` 拆错。
func urlQueryEscape(s string) string { return url.QueryEscape(s) }

// base64URLDecode 解 base64url（容忍标准表：JWT 段可能是两种变体），
// 两种都失败时再按"无填充 → 补填充"重试一次。
func base64URLDecode(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, nil
		}
	}
	// 极少数实现用标准 base64 表（+/ 而非 -_）：容错一次，解不出就返回错误。
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

// callbackPage 渲染本机回调响应页（成功/失败两态）。
// 与 login_qwenwork/login_trae 共用同一套视觉约定（青蓝 #0e7490、成功 1.5s 自动关），
// 但各自内联一份：工具的包边界原则是"渠道间零耦合"，不为一段 HTML 建共享包。
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
// window.close 对非脚本打开的页面常被浏览器拦截：拦截时提示手动关闭
setTimeout(function () {
  var b = document.querySelector('.close-btn');
  if (!window.closed && b) { b.textContent = '关闭此标签页'; }
}, %d);
</script>
</body>
</html>`
