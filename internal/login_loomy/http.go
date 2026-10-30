package login_loomy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
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
// ⚠️ `code` 是**字符串**（鉴权问题为 "020002" 等）。
// 用 json.RawMessage 承接，避免上游偶尔发数字时整个响应解析失败。
type accountEnvelope struct {
	Code json.RawMessage `json:"code"`
	Desc string          `json:"desc"`
	Data json.RawMessage `json:"data"`
}

// accountReply 是账号端点一次响应的完整内容（信封 + data + 原文）。
//
// 旧实现是「解析 → 立刻按 code 白名单 {"0",""} 判成功 → 只回 data」。
// 实测上游对**已真实下发**的短信回 `desc="成功"` 而 `code` 不在该白名单里，
// 于是整次下发被判失败：msgid 不落库、页面反而显示红色「成功」、用户点登录时
// 后端查不到任何下发记录。
//
// 现在的判据改成**响应里有没有 msgid**（业务上唯一可信的「真的发出去了」证据），
// `code` 只在「拿不到 msgid」时用来挑错误文案。Raw 留着是因为 msgid 也可能
// 发在响应顶层而不是 data 里，需要在整包里再找一次。
type accountReply struct {
	Code string
	Desc string
	Data json.RawMessage
	Raw  []byte
}

// ok 判定信封是否**声明**成功：未带 code / 全零 code 视为成功。
//
// 注意它只是「信封自己怎么说」，不等于业务成功 —— 见 accountReply 的注释。
func (r accountReply) ok() bool {
	c := strings.TrimSpace(r.Code)
	if c == "" {
		return true
	}
	// "0" / "000000" 等全零形态都算成功（数字 0 在 codeText 里已归一为 "0"）。
	return strings.Trim(c, "0") == ""
}

// codeText 把信封 code 归一成可打印文本（JSON null / 缺字段 → 空串）。
func codeText(raw json.RawMessage) string {
	c := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if c == "null" {
		return ""
	}
	return c
}

// message 取可展示的错误描述。
func (r accountReply) message() string {
	if s := strings.TrimSpace(r.Desc); s != "" {
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
//
// ⚠️ 本函数**不再**用 code 白名单判业务成败 —— 只报传输层/协议层失败
// （连不上、非 JSON、HTTP>=400）。业务判定交给调用方按**证据**做
// （发码看有没有 msgid，登录看有没有 session），理由见 accountReply 注释。
func postAccount(client *http.Client, path string, body map[string]any) (accountReply, error) {
	if client == nil {
		client = NewClient()
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return accountReply{}, err
	}
	serialized := string(raw)

	now := time.Now().UTC()
	// HTTP Date 用 UTC RFC1123（`http.TimeFormat`），与客户端 `new Date().toUTCString()` 等价。
	headers := authHeaders(http.MethodPost, path, "", serialized, "application/json",
		now.Format(http.TimeFormat), randomUUID())

	req, err := http.NewRequest(http.MethodPost, accountBase+path, bytes.NewReader(raw))
	if err != nil {
		return accountReply{}, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		// 传输层失败：不把底层 error 原文原样透出（可能含 URL/请求细节，且对用户没帮助）。
		return accountReply{}, fmt.Errorf("无法连接讯飞账号服务（%s）：请检查网络后重试", path)
	}
	defer resp.Body.Close()

	respRaw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var env accountEnvelope
	if err := json.Unmarshal(respRaw, &env); err != nil {
		return accountReply{}, fmt.Errorf("讯飞账号服务响应不是 JSON（%s，HTTP %d）", path, resp.StatusCode)
	}
	reply := accountReply{
		Code: codeText(env.Code),
		Desc: env.Desc,
		Data: env.Data,
		Raw:  respRaw,
	}
	if resp.StatusCode >= 400 {
		return reply, fmt.Errorf("讯飞账号服务返回 HTTP %d（%s）", resp.StatusCode, path)
	}
	return reply, nil
}

// sendCode 下发短信验证码，返回 msgid。
func (s *session) sendCode(client *http.Client, phone string) (string, error) {
	body := buildAccountBody(map[string]any{
		"ccode":  "86",
		"phone":  phone,
		"expire": smsCodeTTLSeconds,
	})
	reply, err := postAccount(client, epSendCode, body)
	if err != nil {
		// 不置终态：见 `noteSendFail` 注释（#173 缺陷 A）。
		s.noteSendFail(redact("下发短信验证码失败：" + err.Error()))
		return "", err
	}
	// 成功判据是「响应里真的带回了 msgid」，不是信封 code。
	// data 段优先，其次整个响应体（少数形态把 msgid 放在顶层）。
	msgid, field := extractMsgID(reply.Data)
	if strings.TrimSpace(msgid) == "" {
		if v, f := extractMsgID(reply.Raw); v != "" {
			msgid, field = v, "top:"+f
		}
	}
	if strings.TrimSpace(msgid) == "" {
		// 拿不到 msgid 才是失败：短信有没有真的发出去本机无法确认，
		// 但**没有 msgid 就换不到 session**，故必须报错并留诊断。
		// 一次记全 code / desc / data 顶层键 / 整包顶层键（只记键名，不记值），
		// 复现一次就能看清真实响应结构，不必再来一轮「猜字段在哪一层」。
		log.Printf("loomy 发码：上游响应未取到 msgid phone=%s code=%q desc=%q data_keys=%v raw_keys=%v",
			maskPhone(phone), reply.Code, redact(reply.Desc), topLevelKeys(reply.Data), topLevelKeys(reply.Raw))
		msg := "短信验证码下发失败：" + redact(reply.message())
		if reply.ok() {
			// 信封说成功却没有任何 msgid：上游协议确实变了（不是用户侧问题）。
			msg = "短信验证码响应缺少 msgid（上游协议可能已变化）"
		}
		s.noteSendFail(msg)
		return "", errors.New(msg)
	}

	s.mu.Lock()
	s.phone = maskPhone(phone)
	s.sendDone = true
	s.msgid = msgid
	s.sendErr = "" // 一次成功下发就把上次的失败提示洗掉
	s.mu.Unlock()
	// 跨会话留一份：session 会比验证码先死（取消 / 重开 / 4 分钟 flowTTL），
	// 而用户手机里的验证码仍在有效期内（见 rememberMsgID 的注释）。
	rememberMsgID(phone, msgid)
	// code/desc 照记：上游成功码长什么样只能靠这条现场记录确认；
	// 判据一旦写死在 code 的取值上，上游一改就会连带把方向带偏。
	log.Printf("loomy 发码：成功 phone=%s field=%s msgid_len=%d code=%q desc=%q",
		maskPhone(phone), field, len(msgid), reply.Code, redact(reply.Desc))
	return msgid, nil
}

// msgidField 是 msgid 字段名的候选，按官方客户端实际写法排在首位。
//
// 兼容多种写法的理由：取不到 msgid 时，短信**已经发出去了**，用户手机上收到码却被告知
// 发送失败，只能再发一条（还会撞上发送间隔限制）。而每补一个字段名都要发一版内核，
// 故一次覆盖常见变体，并把**实际命中的字段名**写进日志，便于判断上游是否真的改了协议。
var msgidField = []string{"msgid", "msgId", "msg_id", "MsgID", "MsgId"}

// extractMsgID 从 sendMsgCode 的 data 段取短信业务 id，返回命中的字段名（未命中为空）。
func extractMsgID(data json.RawMessage) (msgid, field string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return "", ""
	}
	for _, k := range msgidField {
		raw, ok := m[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			// 少数服务端会把 id 发成数字：取原始文本并去掉可能的引号。
			s = strings.Trim(strings.TrimSpace(string(raw)), `"`)
		}
		if v := strings.TrimSpace(s); v != "" {
			return v, k
		}
	}
	return "", ""
}

// topLevelKeys 取 JSON 对象的顶层键名（用于诊断，不泄露值）。
func topLevelKeys(data json.RawMessage) []string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
	reply, err := postAccount(client, epCheckCode, body)
	if err != nil {
		// ⚠️ 不置终态失败：验证码输错是常态，用户要能就地重试（ofm 同款）。
		return Result{}, err
	}
	var m struct {
		Session string `json:"session"`
		UserID  string `json:"userid"`
	}
	_ = json.Unmarshal(reply.Data, &m)
	sess := strings.TrimSpace(m.Session)
	if sess == "" {
		// 业务成功同样不看 code，只看有没有 session。上游在验证码错误 /
		// msgid 失效时 desc 里给了人话（「验证码错误」「msgid 无效」），优先透传；
		// 信封说成功却仍没 session，才是协议问题。
		if !reply.ok() {
			return Result{}, errors.New(redact(reply.message()))
		}
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
		// Loomy 无 refresh 端点、上游响应不带到期时间戳，本地只能估算 —— 而估算值一旦
		// 到期即触发必然失败的刷新（见 internal/loomy.RefreshToken）。故写**远期哨兵**，
		// 表示「本凭据不由 expiresAt 驱动刷新」，与导入路径/auth 侧哨兵口径一致。
		ExpiresAt: time.Now().AddDate(50, 0, 0).Unix(),
		Domain:    apiDomain,
		ApiHost:   apiHost,
		UID:       uid,
		Phone:     phone,
		Nickname:  maskPhone(phone),
	}

	s.mu.Lock()
	// 已取消（Shutdown / 面板已放弃本流程）不得复活；除此之外的一律接受。
	//
	// 旧条件是 `ph == PhasePending || ph == PhaseSuccess`，它把 PhaseFailed 也算「不可再写」，
	// 于是一次下发失败后的**真实登录成功**会被静默丢弃（#173 缺陷 A）：
	// 页面回「登录成功」、面板 Poll 恒报旧的「下发失败」，两边说法相反且无法自愈。
	if s.ph != PhaseCancelled {
		s.result, s.hasRes, s.ph, s.errMsg, s.sendErr = r, true, PhaseSuccess, "", ""
		s.phone = maskPhone(phone)
	}
	s.mu.Unlock()
	return r, nil
}

// 注：曾有的 `func (s *session) fail()` （下发失败直接置终态）已删除——
// 它正是 #173 缺陷 A 的入口，终态失败现在只由超时/硬失败走 session.setFailed。

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
