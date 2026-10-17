package login_loomy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 本文件封装**讯飞账号端点**（accountBase）的两步登录请求。
//
// 逐段对应参考实现 `dsh-our-free-model/vendor/channel-pack/src/loomy-oauth.ts`：
//   - buildLoomyAccountBody（:55-71）→ buildAccountBody
//   - postLoomyAccount（:78-123）    → postAccount
//   - sendLoomySmsCode（:131-151）   → session.sendCode
//   - loginLoomyBySmsCode（:159-180）→ session.login

// accountEnvelope 是账号端点响应信封 `{code, desc, data}`。
//
// ⚠️ `code` 是**字符串**（实测成功为 "0"，鉴权问题为 "020002" 等）。
// 用 json.RawMessage 承接，避免上游偶尔发数字时整个响应解析失败。
type accountEnvelope struct {
	Code json.RawMessage `json:"code"`
	Desc string          `json:"desc"`
	Data json.RawMessage `json:"data"`
}

// ok 判定信封是否成功（`code` 为 "0" 或 0）。
func (e accountEnvelope) ok() bool {
	c := strings.TrimSpace(string(e.Code))
	c = strings.Trim(c, `"`)
	return c == "0" || c == ""
}

// message 取可展示的错误描述。
func (e accountEnvelope) message() string {
	if s := strings.TrimSpace(e.Desc); s != "" {
		return s
	}
	return "讯飞账号服务返回错误"
}

// buildAccountBody 构建账号端点请求体信封 `{base, param}`。
//
// 依据 account-service.js:441-450：`traceid` 每次调用重新生成（去掉连字符的 uuid，32 位 hex）。
func buildAccountBody(param map[string]any) map[string]any {
	return map[string]any{
		"base": map[string]any{
			"appid":   accessKeyAppID,
			"modelid": accountModelID,
			"version": accountVersion,
			"devid":   accountDeviceID,
			// ⚠️ 硬编码 macOS：客户端在 Windows 上发的也是这个值，照抄不要改。
			"ua":      accountUA,
			"traceid": randomTraceID(),
		},
		"param": param,
	}
}

// postAccount 发一次账号端点请求并拆信封。
//
// ⚠️ body **只序列化一次**，签名与发送共用同一个 []byte —— 这是签名能通过的前提
// （二次序列化会改变字节：键序/空格）。
func postAccount(client *http.Client, path string, body map[string]any) (json.RawMessage, error) {
	if client == nil {
		client = NewClient()
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	serialized := string(raw)

	now := time.Now().UTC()
	// HTTP Date 用 UTC RFC1123（`http.TimeFormat`），与客户端 `new Date().toUTCString()` 等价。
	headers := authHeaders(http.MethodPost, path, "", serialized, "application/json",
		now.Format(http.TimeFormat), randomUUID())

	req, err := http.NewRequest(http.MethodPost, accountBase+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// 传输层失败：不把底层 error 原文原样透出（可能含 URL/请求细节，且对用户没帮助）。
		return nil, fmt.Errorf("无法连接讯飞账号服务（%s）：请检查网络后重试", path)
	}
	defer resp.Body.Close()

	respRaw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var env accountEnvelope
	if err := json.Unmarshal(respRaw, &env); err != nil {
		return nil, fmt.Errorf("讯飞账号服务响应不是 JSON（%s，HTTP %d）", path, resp.StatusCode)
	}
	if !env.ok() {
		// 账号端点的鉴权/业务错误码一律透传 desc：上层据文案判断
		// 「验证码错误」「手机号格式不对」「频率限制」（ofm loomy-oauth.ts:117-120 同款）。
		//
		// ⚠️ 只透传上游声明的 desc，**不透传响应体原文**：这些文案会经 app.pollLogin 进日志，
		// 且 desc 里若意外回显了 token 也必须被吃掉（见 redact）。
		return nil, errors.New(redact(env.message()))
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("讯飞账号服务返回 HTTP %d（%s）", resp.StatusCode, path)
	}
	return env.Data, nil
}

// sendCode 下发短信验证码，返回 msgid。
func (s *session) sendCode(client *http.Client, phone string) (string, error) {
	body := buildAccountBody(map[string]any{
		"ccode":  "86",
		"phone":  phone,
		"expire": smsCodeTTLSeconds,
	})
	data, err := postAccount(client, epSendCode, body)
	if err != nil {
		s.fail("下发短信验证码失败：" + err.Error())
		return "", err
	}
	var m struct {
		MsgID string `json:"msgid"`
	}
	_ = json.Unmarshal(data, &m)
	if strings.TrimSpace(m.MsgID) == "" {
		// 不返回空串：上层会拿它去登录，服务端必然报「msgid 无效」，
		// 用户看到的将是一个与真实原因无关的错误（ofm loomy-oauth.ts:145-149 原话）。
		msg := "短信验证码响应缺少 msgid（上游协议可能已变化）"
		s.fail(msg)
		return "", errors.New(msg)
	}

	s.mu.Lock()
	s.phone = maskPhone(phone)
	s.sendDone = true
	s.mu.Unlock()
	return m.MsgID, nil
}

// login 用验证码换 session；成功即置 success 等 Poll 取走。
func (s *session) login(client *http.Client, phone, code, msgid string) (Result, error) {
	body := buildAccountBody(map[string]any{
		"ccode":  "86",
		"phone":  phone,
		"mcode":  code,
		"msgid":  msgid,
		"expire": sessionTTLSeconds,
	})
	data, err := postAccount(client, epCheckCode, body)
	if err != nil {
		// ⚠️ 不置终态失败：验证码输错是常态，用户要能就地重试（ofm 同款）。
		return Result{}, err
	}
	var m struct {
		Session string `json:"session"`
		UserID  string `json:"userid"`
	}
	_ = json.Unmarshal(data, &m)
	sess := strings.TrimSpace(m.Session)
	if sess == "" {
		return Result{}, errors.New(redact("登录响应缺少 session（上游协议可能已变化）"))
	}
	uid := strings.TrimSpace(m.UserID)
	if uid == "" {
		// userid 缺失时用 session 派生稳定 UID：账号池需要一个去重标识，
		// 而「每次登录换一个新随机名」会让同一账号重复入库、池里出现幽灵账号。
		uid = deriveUID(sess)
	}

	r := Result{
		AccessToken: sess,
		ExpiresAt:   time.Now().Add(sessionTTLSeconds * time.Second).Unix(),
		Domain:      apiDomain,
		ApiHost:     apiHost,
		UID:         uid,
		Phone:       phone,
		Nickname:    maskPhone(phone),
	}

	s.mu.Lock()
	if s.ph == PhasePending || s.ph == PhaseSuccess {
		s.result, s.hasRes, s.ph, s.errMsg = r, true, PhaseSuccess, ""
		s.phone = maskPhone(phone)
	}
	s.mu.Unlock()
	return r, nil
}

// fail 置终态失败。
func (s *session) fail(msg string) { s.setFailed(redact(msg)) }

// deriveUID 从 session 派生稳定 UID（userid 缺失时才用）。
//
// 取 session 的前 12 位 hex：同一份凭据总是得到同一个 UID（重复登录覆盖同一文件，
// 不产生重复账号）；不同账号的 session 自带随机性，前缀碰撞概率可忽略。
// ⚠️ 只取前缀而不是整串：**完整 session 等于 token**，不该作为「账号标识」到处传播
// （它会进文件名、日志、面板），前缀足够区分且不构成可用凭据。
func deriveUID(session string) string {
	const n = 12
	if len(session) >= n {
		return "loomy_" + session[:n]
	}
	return "loomy_" + session
}

// redact 从文案里抹掉任何 32 位 hex 串（Loomy session 的形态）。
//
// 为什么需要：错误文案会经 app.pollLogin 进日志，而 AGENTS.md §0.2 要求**零 token 落盘**。
// 上游 desc 若意外回显凭据，这里必须吃掉 —— 宁可少一条诊断信息，也不留泄露面。
func redact(msg string) string {
	return sessionLikePattern.ReplaceAllString(msg, "[已隐去]")
}

// randomTraceID 生成 32 位小写 hex（去掉连字符的 uuid，对齐客户端 traceid）。
func randomTraceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// randomUUID 生成 v4 格式 UUID（对齐客户端 `Date`+`Nonce` 里的 Nonce 形态）。
func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// atoiSafe 宽松解析整数（信封 code 可能是字符串或数字）。
func atoiSafe(raw json.RawMessage) int {
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}
