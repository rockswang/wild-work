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
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// handleCallback 处理 `GET /auth/callback?code=...&state=...`。
//
// 顺序刻意如此（与备忘 108 行一致）：
//  1. 先校验 state —— 不匹配**立即 400 并拒绝**，绝不把 code 交给后续流程；
//  2. 再看 `error` 参数（用户在登录页拒绝授权）；
//  3. 最后取 code。
func (s *loginSession) handleCallback(w http.ResponseWriter, r *http.Request) {
	// 取证日志（零 token：只记形态与结论，不记 code/state 的值）。
	// 为什么必须有：回调「没到」「到了但 state 不符」「到了但路径不对」在面板上都只表现成
	// 「一直登录中」，没有这行就无法区分——2026-10-18 局域网排查就卡在这里。
	q := r.URL.Query()
	ok, msg, stateRejected := s.applyCallback(q)
	switch {
	case ok:
		log.Printf("lobsterai 回调到达并已收码 remote=%s（等待 exchange）", r.RemoteAddr)
	case stateRejected:
		log.Printf("lobsterai 回调被拒（state 不符/缺失）remote=%s 期望端口=%d", r.RemoteAddr, s.state.CallbackPort)
	default:
		log.Printf("lobsterai 回调到达但无法收码 remote=%s", r.RemoteAddr)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if stateRejected {
		w.WriteHeader(http.StatusBadRequest)
	}
	// 注意：此刻**尚未**完成 exchange（Poll 随后立即做），故页面文案是「已收到授权」
	// 而不是「登录成功」——避免兑换失败时误导用户（同 login_qwenwork 的取舍）。
	_, _ = w.Write([]byte(callbackPage(ok, msg)))
}

// applyCallback 解析回调参数并把结论写进会话。
//
// HTTP 回调（handleCallback）与手动粘贴（SubmitCallbackURL）**共用这一条结论通道**：
// 两条入口写进同一个 pending 字段，Poll / exchange 路径因此零分叉（不变量：一处判定）。
// stateRejected 只用于让自动回调页回 400；手动粘贴只需把 msg 当错误抛出。
func (s *loginSession) applyCallback(q url.Values) (ok bool, msg string, stateRejected bool) {
	// 1. state 原样比对。**区分"缺失"与"不匹配"**只为文案可读，处置相同：拒绝。
	got := q.Get("state")
	if got == "" {
		const m = "回调缺少 state 参数，已拒绝"
		s.fail(m)
		return false, m, true
	}
	if got != s.state.State {
		const m = "state 不匹配，已拒绝该回调（多半是上一次登录遗留的地址，请用本次登录后跳转的那条）"
		s.fail(m)
		return false, m, true
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
		return false, desc, false
	}

	// 3. 取 code 并置入内存，由 Poll 立即 exchange。
	code := q.Get("code")
	if code == "" {
		// 粘贴场景下用户最常见的错是只复制了 `…/auth/callback` 而丢了查询串，
		// 光说「缺少 code 参数」他不知道自己该复制什么，故文案要可执行。
		const m = "回调缺少 code 参数：请复制登录完成后浏览器地址栏里含 code=… 的整条 URL" +
			"（若地址栏里只有 …/auth/callback 而没有 code=，说明授权没完成，请重新登录）"
		s.fail(m)
		return false, m, false
	}
	// 同一条流程收到第二个 code：保留第一个（已被 Poll 取走或即将取走），
	// 避免后到的把先到的覆盖掉。浏览器刷新/预取可能触发，属正常现象。
	s.set(code)
	return true, "", false
}

// SubmitCallbackURL 手动提交浏览器跳转后的回调地址（远程/局域网场景的兜底入口）。
//
// 为什么必须有它：本渠道的回调地址是 portal 按 `redirect_uri` 参数**原样跳回**的，
// 而本工具只能给出 `http://127.0.0.1:<port>/auth/callback`（不变量：回调口绝不监听
// 0.0.0.0，监听非环回等于把「收 code 即换 token」的口暴露到局域网）。这隐含了
// 「浏览器与网关同一台机器」的前提（Termux 手机、本机桌面均满足）。用户从局域网另一台
// 电脑打开面板时，浏览器跳到的是**他自己机器**的 127.0.0.1:<port> ⇒ 连接被拒、
// 授权码到不了网关，流程只能枯等到超时（用户侧现象：「登录完成后跳到一个打不开的
// 127.0.0.1 页面」）。
//
// 本函数不改 portal 侧约定（改不了），只补一条入口：让用户把地址栏里的完整 URL
// 粘回来，解析出 code 后走与 HTTP 回调完全相同的结论/兑换路径（state 仍原样比对）。
//
// raw 接受三种形态：完整 URL / `?code=…&state=…` 查询串 / 裸授权码
// （裸码缺少 state，会按 state 缺失拒绝——本渠道 state 是唯一防伪手段，不得放宽）。
func SubmitCallbackURL(raw string) error {
	q, port, err := parseCallbackInput(raw)
	if err != nil {
		return err
	}
	sess := currentSession()
	if sess == nil {
		return errors.New("没有进行中的登录流程（可能已完成或已超时），请重新发起登录")
	}
	// 端口自检：多开/重试场景下很容易粘到**上一次**登录的回调地址。放过去只会得到
	// 一个语焉不详的 exchange 失败，故在这里拦下并说清原因。
	if port != 0 && sess.state.CallbackPort != 0 && port != sess.state.CallbackPort {
		return fmt.Errorf("回调地址的端口 %d 与本次登录的回调口 %d 不一致——多半粘贴的是上一次登录的地址，请用本次登录后跳转的那条 URL",
			port, sess.state.CallbackPort)
	}
	ok, msg, _ := sess.applyCallback(q)
	if !ok {
		return fmt.Errorf("登录失败: %s", msg)
	}
	return nil
}

// parseCallbackInput 把用户粘贴的内容解析成回调参数，并尽力取出回调端口。
// 端口取不到（裸 code / 无端口的查询串）时返回 0，调用方跳过端口自检。
func parseCallbackInput(raw string) (url.Values, int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, 0, errors.New("回调地址为空")
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("回调地址无法解析: %w", err)
		}
		port := 0
		if p, err := strconv.Atoi(u.Port()); err == nil {
			port = p
		}
		return u.Query(), port, nil
	}
	if strings.Contains(raw, "=") {
		q, err := url.ParseQuery(strings.TrimPrefix(raw, "?"))
		if err != nil && len(q) == 0 {
			return nil, 0, fmt.Errorf("回调地址无法解析: %w", err)
		}
		return q, 0, nil
	}
	// 只贴了裸授权码（部分用户会只复制 code= 后面的值）。state 缺失会被 applyCallback 拒绝。
	return url.Values{"code": {raw}}, 0, nil
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
