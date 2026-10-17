package login_loomy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// 本文件是登录引导页（本机 loopback）与其 API。
//
// 页面只做三件事：
//  1. 说明「本工具要用你的手机号完成 Loomy 短信验证码登录」；
//  2. 收手机号 → POST /api/loomy/send_code（本工具用签名协议真实下发短信）；
//  3. 收验证码 → POST /api/loomy/login（换 session，成功后由 app.pollLogin 落盘）。
//
// ⚠️ 页面**不内嵌**二维码、不依赖任何外部资源：断网也能显示表单（除提交那两次请求外）。

// contextWithTimeout 是 context.WithTimeout 的薄封装（让 login.go 不必 import context）。
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// sessionLikePattern 匹配「像 Loomy session 的串」（32~64 位连续 hex），用于日志脱敏。
// 见 redact。
var sessionLikePattern = regexp.MustCompile(`\b[0-9a-fA-F]{32,64}\b`)

// maxBodyBytes 表单请求体上限（防止被塞爆；与 ofm 的 64KB 同口径）。
const maxBodyBytes = 64 << 10

// writeJSON 写一个 JSON 响应。
func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(payload)
}

// readJSONBody 读取并解析请求体。
func readJSONBody(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// handleGuide 渲染引导页。
func (s *session) handleGuide(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != guidePath {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, guidePage)
}

// handleSendCode 下发短信验证码。
//
// 失败**置终态**：下发不出验证码，整条流程就没有意义（重试只会放大上游风控风险，
// 也让用户看不出「没收到短信」的真实原因）。
func (s *session) handleSendCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := readJSONBody(r)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": "请求体不是合法 JSON"})
		return
	}
	phone, _ := body["phone"].(string)
	phone = strings.TrimSpace(phone)

	msgid, err := s.sendCode(s.guideClient(), phone)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	// msgid 回给页面（页面提交时原样带回）。它不是凭据，但也没必要广播到别的窗口。
	writeJSON(w, map[string]any{"ok": true, "msgid": msgid, "message": "验证码已发送至 " + maskPhone(phone)})
}

// handleLogin 用验证码换 session。
func (s *session) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, err := readJSONBody(r)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": "请求体不是合法 JSON"})
		return
	}
	phone, _ := body["phone"].(string)
	code, _ := body["code"].(string)
	msgid, _ := body["msgid"].(string)

	if _, err := s.login(s.guideClient(), strings.TrimSpace(phone), strings.TrimSpace(code), strings.TrimSpace(msgid)); err != nil {
		// 就地报错让用户重试（不关页面、不置终态）。
		writeJSON(w, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "登录成功，可以关闭此窗口了"})
}

// handleStatus 供页面在提交后确认状态（也便于用户手动刷新排查）。
func (s *session) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := s.status()
	writeJSON(w, map[string]any{
		"phase":       string(st.Phase),
		"has_session": st.HasSession,
		"phone":       st.Phone,
		"error":       st.Err,
	})
}

// guideClient 给引导页 handler 用的 HTTP client。
// 单独一个 30s 超时的 client：引导页请求不该被登录流程的生命周期牵连。
func (s *session) guideClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// guidePage 是登录引导页（单文件、零外部资源）。
const guidePage = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>登录 Loomy</title>
<style>
  :root { color-scheme: light dark; }
  body { margin:0; min-height:100vh; display:flex; align-items:center; justify-content:center;
         font-family:-apple-system,BlinkMacSystemFont,"Segoe UI","Microsoft YaHei",sans-serif;
         background:#f5f6f8; color:#1f2329; }
  .card { background:#fff; border-radius:12px; padding:28px 32px; box-shadow:0 4px 24px rgba(0,0,0,.08);
          width:340px; }
  h1 { font-size:17px; margin:0 0 4px; font-weight:600; }
  .sub { font-size:13px; color:#8a9099; margin-bottom:18px; line-height:1.6; }
  label { display:block; font-size:12px; color:#8a9099; margin:0 0 4px; }
  input { width:100%; box-sizing:border-box; padding:9px 10px; font-size:14px; margin-bottom:12px;
          border:1px solid #d9dde3; border-radius:6px; }
  button { width:100%; padding:10px; font-size:14px; border:0; border-radius:6px;
           background:#2f6bff; color:#fff; cursor:pointer; }
  button:disabled { background:#c9cdd4; cursor:not-allowed; }
  button.ghost { background:#f2f3f5; color:#1f2329; }
  .row { display:flex; gap:8px; }
  .row input { margin-bottom:0; }
  .row button { width:auto; white-space:nowrap; padding:9px 12px; }
  .gap { margin-bottom:12px; }
  .status { margin-top:14px; font-size:13px; color:#4e5969; min-height:20px; line-height:1.5; }
  .status[data-tone="ok"] { color:#0f9d58; font-weight:600; }
  .status[data-tone="err"] { color:#d93026; }
</style>
</head>
<body>
  <div class="card">
    <h1>登录 Loomy</h1>
    <div class="sub">用你的手机号接收短信验证码完成登录。凭据只会保存在本机 wild-work 里，
      到期后（约 14 天）需要重新登录。</div>

    <label for="phone">手机号</label>
    <div class="row gap">
      <input id="phone" type="tel" inputmode="numeric" maxlength="11" placeholder="11 位手机号">
      <button id="sendBtn" type="button">获取验证码</button>
    </div>

    <label for="code">短信验证码</label>
    <input id="code" type="text" inputmode="numeric" maxlength="6" placeholder="6 位验证码">

    <button id="loginBtn" type="button">登录</button>
    <div class="status" id="status"></div>
  </div>

<script>
(function () {
  var phoneEl = document.getElementById('phone');
  var codeEl = document.getElementById('code');
  var sendBtn = document.getElementById('sendBtn');
  var loginBtn = document.getElementById('loginBtn');
  var statusEl = document.getElementById('status');
  var msgid = '';

  function setStatus(text, tone) {
    statusEl.textContent = text;
    if (tone) statusEl.setAttribute('data-tone', tone);
    else statusEl.removeAttribute('data-tone');
  }
  function digits(el) { return (el.value || '').replace(/\D/g, ''); }
  function post(path, payload) {
    return fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload)
    }).then(function (r) { return r.json(); });
  }

  sendBtn.addEventListener('click', function () {
    var phone = digits(phoneEl);
    if (phone.length !== 11) { setStatus('请输入 11 位手机号', 'err'); return; }
    sendBtn.disabled = true;
    setStatus('正在发送验证码…');
    post('` + apiSendCodePath + `', { phone: phone })
      .then(function (res) {
        if (res && res.ok) {
          msgid = res.msgid || '';
          setStatus(res.message || '验证码已发送', 'ok');
          codeEl.focus();
        } else {
          setStatus((res && res.message) || '发送失败', 'err');
        }
      })
      .catch(function () { setStatus('发送失败，请重试', 'err'); })
      .finally(function () { sendBtn.disabled = false; });
  });

  loginBtn.addEventListener('click', function () {
    var phone = digits(phoneEl);
    var code = digits(codeEl);
    if (phone.length !== 11) { setStatus('请输入 11 位手机号', 'err'); return; }
    if (!code) { setStatus('请输入短信验证码', 'err'); return; }
    if (!msgid) { setStatus('请先点「获取验证码」', 'err'); return; }
    loginBtn.disabled = true;
    setStatus('正在登录…');
    post('` + apiLoginPath + `', { phone: phone, code: code, msgid: msgid })
      .then(function (res) {
        if (res && res.ok) setStatus(res.message || '登录成功，可以关闭此窗口了', 'ok');
        else setStatus((res && res.message) || '登录失败，请重试', 'err');
      })
      .catch(function () { setStatus('登录失败，请重试', 'err'); })
      .finally(function () { loginBtn.disabled = false; });
  });
})();
</script>
</body>
</html>`
