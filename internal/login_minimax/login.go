// Package login_minimax 实现 MiniMax Code（中国版）的 OAuth **设备码 + PKCE** 登录。
//
// 形态（协议事实来源：docs/ref-ofm-渠道-lobsterai-minimax.md §3，389–451 行）：
//  1. Start：生成 PKCE → `POST {AccountHost}/oauth2/device/code` 申请设备码 →
//     把 device_code / code_verifier / interval / 轮询截止时间写进 state 文件 →
//     **立即**返回 `verification_uri_complete`（前端 window.open 只在用户点击后的
//     瞬态激活窗口内有效，约 5 秒，故 Start 不能阻塞在轮询上）。
//  2. 用户在自己的浏览器里完成授权（点链接或手输 user_code）。
//  3. Poll：由 `app.pollLogin` 每 2s 调一次；本包按上游下发的 interval（**单位秒**）
//     节流，到点才 `POST {AccountHost}/oauth2/token` 换 token。**不起本地回调端口**。
//  4. 成功后删除 state 文件并返回 Result，由调用方 SaveAuth 落盘
//     `auths/minimax-<uid>.json`。
//
// 上游纪律（AGENTS.md §0.1）：本包只在用户主动发起登录后才请求上游，且每次轮询都受
// interval 节流；一旦得到**终态**结论（拒绝/过期/其它硬失败）就把错误固化进 state 文件，
// 后续 Poll 直接短路返回，不再重复打上游。
//
// 本包只负责登录。续期（`grant_type=refresh_token`，§3.7）属于 internal/minimax 的
// Upstream.RefreshToken —— 那里同样受**不变量 20** 约束：MiniMax 的 refresh token 会轮换，
// 刷新后必须紧跟 `SaveAtomic` 落盘，否则下次启动拿旧 refresh token 直接死锁。
package login_minimax

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/minimax"
)

// 上游常量：主机与 OAuth 客户端常量的**单一来源在 internal/minimax**（渠道包），
// 这里一律引用而非另写一份——两处各写一份的话，将来改端点必然漂移（登录包与渠道包
// 指向不同主机，且编译期毫无感觉）。只属于登录流程的 Domain / 路径 / 间隔留在这里。
const (
	// AccountHost 账号 / OAuth 主机（设备码申请 + 令牌轮询 + 续期共用）。
	AccountHost = minimax.AccountHost
	// ApiHost 业务主机（模型目录 / 签到 / 积分 / Anthropic Messages 推理）。
	// 登录流程本身不用它，但必须写进 auth 文件——业务侧（internal/minimax）从那里取。
	ApiHost = minimax.APIHost
	// ClientID / Audience / Scope 是公开的 OAuth 客户端常量（非机密，不是密钥）。
	ClientID = minimax.OAuthClientID
	Audience = minimax.OAuthAudience
	Scope    = minimax.OAuthScope

	// Domain 写进 auth 文件的 domain 字段（与 trae-"trae.cn" / qwenwork-"qwenwork.cn" 同口径）。
	Domain = "minimax.cn"

	deviceCodePath = "/oauth2/device/code"
	tokenPath      = "/oauth2/token"

	// deviceCodeGrant 是设备码换 token 的 grant_type（RFC 8628 标准值）。
	deviceCodeGrant = "urn:ietf:params:oauth:grant-type:device_code"

	// defaultIntervalSec 是上游未下发 interval 时的缺省轮询间隔（**秒**，§3.3）。
	defaultIntervalSec = 5
	// slowDownStepSec 是 slow_down 时的退避步长：interval +5s（§3.4；上游内部是 +5000ms）。
	slowDownStepSec = 5
)

// endpointHost 是账号/OAuth 主机。独立成变量（默认等于 AccountHost，而非直接引用常量）
// 的唯一目的是让回归测试能把它指向**本机 httptest 服务**。
// ⚠️ 严禁把它指向生产上游做探测（AGENTS.md §0.1：验证上游行为必须最小化请求数）。
var endpointHost = AccountHost

// ErrPending 表示授权尚未完成（用户还没点确认），调用方应继续轮询。
// 与 login_trae/login_qwenwork 同名同义，app.pollLogin 用 errors.Is 判定。
var ErrPending = errors.New("login pending")

// Result 登录成功后拿到的凭据与账号信息。
type Result struct {
	AccessToken  string
	RefreshToken string
	// ExpiresAt 是 access token 的到期时刻（Unix 秒），由 `now + expires_in` 写入。
	// ⚠️ MiniMax 的 access token **不是 JWT**（前缀 mmoat_），没有 exp 可解，见 parseTokenGrant。
	ExpiresAt int64
	// Scope 是上游下发的 scope 原文（空格分隔），硬校验要求含 agent.default。
	Scope string
	// Domain / ApiHost 写进 auth 文件，供 internal/minimax 组装出站请求。
	Domain  string
	ApiHost string
	// UID 是账号在**本工具**里的稳定短标识（派生规则见 deriveUID），也是 auth 文件名的一部分。
	UID string
	// Nickname 是展示名；上游不下发时留空，由 SaveAuth 用 UID 兜底（面板至少有个可读标识）。
	Nickname string
}

// state 是落在 login-state.json 里的一次性登录状态。
//
// 为什么状态要落盘而不是只放内存：与 login_trae/login_qwenwork 保持同形——app 的
// Start / Poll / CancelLogin 是三个独立入口（CancelLogin 直接删 state 文件即等于放弃），
// 且进程重启后仍能看清上一轮登录卡在哪一步。
type state struct {
	DeviceCode   string `json:"deviceCode"`
	UserCode     string `json:"userCode,omitempty"` // 仅诊断用（用户可手输；前端不展示）
	CodeVerifier string `json:"codeVerifier"`
	// IntervalSec 是当前有效的轮询间隔（**秒**）。缺省 5，slow_down 时 +5。
	IntervalSec int `json:"intervalSec"`
	// ExpiresAt 是设备码的轮询截止时刻（Unix 秒）= Start 时刻 + expires_in。
	ExpiresAt int64 `json:"expiresAt"`
	// NextPollAt 是下一次**允许**向 /oauth2/token 发请求的时刻（Unix 秒）。
	// app.pollLogin 固定每 2s 调一次 Poll，比 interval 密，必须本地拦掉多余请求。
	NextPollAt int64 `json:"nextPollAt"`
	// Err 是固化的终态错误：非空时后续 Poll 直接返回它，不再触碰上游（§0.1）。
	Err string `json:"err,omitempty"`
}

// NewClient 返回登录用 HTTP 客户端（与 login_trae 同形；30s 总超时兜底，
// 上游 OAuth 自身声明 20s 超时，留一点本机网络余量）。
func NewClient() *http.Client { return &http.Client{Timeout: 30 * time.Second} }

// Start 申请设备码并返回用户需要打开的授权链接（verification_uri_complete）。
//
// 注意两点：
//   - code_verifier 必须随 state 落盘：轮询换 token 时要回传（PKCE public client）。
//   - 返回的是 `verification_uri_complete`；该字段缺失时回退 `verification_uri`（§3.3）。
func Start(client *http.Client, statePath string) (string, error) {
	verifier, challenge := newPKCE()

	form := url.Values{
		"client_id":             {ClientID},
		"scope":                 {Scope},
		"audience":              {Audience},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	raw, status, err := postForm(client, deviceCodePath, form)
	if err != nil {
		return "", err
	}
	if status >= 400 {
		return "", fmt.Errorf("申请设备码失败 http %d: %s", status, structuredErr(raw, 200))
	}

	var dc struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURL         string `json:"verification_url"` // 别名，缺失上游可能只给这个
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int64  `json:"expires_in"` // 单位：秒，必须是正数
		Interval                int64  `json:"interval"`   // 单位：秒，缺省 5
		Error                   string `json:"error"`
		ErrorDescription        string `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &dc); err != nil {
		return "", fmt.Errorf("申请设备码失败：响应不是 JSON（%v）", err)
	}
	if dc.Error != "" {
		return "", fmt.Errorf("申请设备码失败: %s %s", dc.Error, truncate(dc.ErrorDescription, 200))
	}
	if strings.TrimSpace(dc.DeviceCode) == "" {
		return "", errors.New("申请设备码失败：响应缺 device_code")
	}
	if dc.ExpiresIn <= 0 {
		// 没有正数 expires_in 就算不出轮询截止时间，宁可立即失败也不用零值兜底
		// （零值会让轮询立刻"过期"，用户看到的是莫名其妙的失败）。
		return "", fmt.Errorf("申请设备码失败：expires_in 非正数（%d）", dc.ExpiresIn)
	}
	uri := strings.TrimSpace(dc.VerificationURI)
	if uri == "" {
		uri = strings.TrimSpace(dc.VerificationURL)
	}
	open := strings.TrimSpace(dc.VerificationURIComplete)
	if open == "" {
		open = uri // 缺失回退 verification_uri（§3.3）
	}
	if open == "" {
		return "", errors.New("申请设备码失败：响应缺 verification_uri / verification_uri_complete")
	}
	interval := int(dc.Interval)
	if interval <= 0 {
		interval = defaultIntervalSec
	}

	now := time.Now()
	st := state{
		DeviceCode:   dc.DeviceCode,
		UserCode:     dc.UserCode,
		CodeVerifier: verifier,
		IntervalSec:  interval,
		ExpiresAt:    now.Add(time.Duration(dc.ExpiresIn) * time.Second).Unix(),
		// 首发也要等一个 interval：RFC 8628 要求两次轮询之间至少间隔 interval，
		// 而"申请设备码"本身算第 0 次请求。
		NextPollAt: now.Add(time.Duration(interval) * time.Second).Unix(),
	}
	if err := writeState(statePath, st); err != nil {
		return "", err
	}
	return open, nil
}

// Poll 轮询一次授权结果：未完成返回 ErrPending；成功返回 Result 并删除 state 文件；
// 终态失败返回可行动的错误（同时把错误固化进 state，避免反复打上游）。
func Poll(client *http.Client, statePath string) (Result, error) {
	st, err := readState(statePath)
	if err != nil {
		return Result{}, fmt.Errorf("read state: %w", err)
	}
	// 终态短路：错误已固化，本轮登录不会再有别的结果（也不该再问上游）。
	if st.Err != "" {
		return Result{}, errors.New(st.Err)
	}
	if st.DeviceCode == "" || st.CodeVerifier == "" {
		return Result{}, errors.New("login state 不完整（缺 deviceCode/codeVerifier），请重新发起登录")
	}
	now := time.Now()
	if st.ExpiresAt > 0 && now.Unix() >= st.ExpiresAt {
		return Result{}, st.fail(statePath, "设备码已过期，请重新发起登录")
	}
	// 本地节流：没到 NextPollAt 就什么都不做（上游 interval 是秒级的，app 是 2s 一跳）。
	if st.NextPollAt > 0 && now.Unix() < st.NextPollAt {
		return Result{}, ErrPending
	}

	form := url.Values{
		"grant_type":    {deviceCodeGrant},
		"device_code":   {st.DeviceCode},
		"client_id":     {ClientID},
		"code_verifier": {st.CodeVerifier},
	}
	raw, status, err := postForm(client, tokenPath, form)
	if err != nil {
		// 传输层错误：不改 state（不是终态），下一跳再试。
		return Result{}, err
	}

	var tr tokenResponse
	parseErr := json.Unmarshal(raw, &tr)
	// ⚠️⚠️ 本渠道最大的坑：`pending` 有**两种信封**，必须都认（§3.4）——
	//   ① HTTP 200 + {"status":"pending"}
	//   ② 非 2xx + {"error":"authorization_pending"}
	// 只认一种的后果：把 ① 当"成功"去解析令牌（报"缺 access_token"），或把 ② 当
	// "授权失败"直接终止登录（用户明明还没点确认，却被判失败）。
	// 故这里把 status / error 两个字段一起取，按同一个判别表处理。
	code := strings.ToLower(strings.TrimSpace(firstNonEmpty(tr.Error, tr.Status)))
	switch code {
	case "authorization_pending", "pending":
		return Result{}, st.backoff(statePath, now, st.intervalOr(), ErrPending)
	case "slow_down":
		// slow_down：interval +5s 后继续（上游内部同样 +5000ms）。
		return Result{}, st.backoff(statePath, now, st.intervalOr()+slowDownStepSec, ErrPending)
	case "access_denied", "denied":
		return Result{}, st.fail(statePath, "用户取消了授权")
	case "expired_token", "expired":
		return Result{}, st.fail(statePath, "设备码已过期，请重新发起登录")
	}
	if status >= 400 || tr.Error != "" {
		// 其他非 2xx + error：授权失败。零 token 纪律（AGENTS.md §0.2）：
		// **不**回显响应体原文（2xx 形状的失败体可能带 access/refresh token），
		// 只透出上游自己声明的结构化错误码与描述。
		return Result{}, st.fail(statePath, fmt.Sprintf("授权失败 http %d: %s", status, structuredErr(raw, 200)))
	}
	if parseErr != nil {
		// 2xx 但 body 不是 JSON：多半是代理/网关插进来的页面，属可重试的异常，
		// 但仍退避一个 interval，免得 2s 一跳把上游打满。
		_ = writeState(statePath, st.withNextPoll(now, st.intervalOr()))
		return Result{}, fmt.Errorf("令牌响应不是 JSON（%v）", parseErr)
	}
	// 2xx 且 status/error 都不在上述判别表里 ⇒ 按成功解析令牌。
	res, err := parseTokenGrant(tr)
	if err != nil {
		return Result{}, err
	}
	_ = os.Remove(statePath) // 一次性状态用完即删（同 login_trae）
	return res, nil
}

// SaveAuth 以嵌套形原子写盘 `auths/minimax-<uid>.json`。
//
// 这里刻意**不手写 map**，而是构造 auth.Auth 后调 auth.SaveAtomic()：
// 这样落盘字节形态与 `auth.Parse` / LoadMinimaxDir / 将来刷新路径的写回**完全同源**，
// 不会再出现"登录写的字段名和刷新写的字段名不一致"这类漂移。
func SaveAuth(authDir string, r Result) (string, error) {
	if strings.TrimSpace(r.UID) == "" {
		return "", fmt.Errorf("missing uid in result")
	}
	if r.ExpiresAt <= 0 {
		// 非 JWT 渠道没有"事后校正"的机会：expiresAt 只能来自 expires_in。
		// 落成 0 会让 auth.NeedsRefresh 恒为真 → 每次请求都刷 token（不变式 19 的坑）。
		return "", fmt.Errorf("missing expiresAt in result（MiniMax 非 JWT，过期时间只能由 expires_in 算出）")
	}
	nick := strings.TrimSpace(r.Nickname)
	if nick == "" {
		nick = r.UID // 上游不下发 nickname 时用 uid 兜底，面板至少有可读标识（同 raccoon）
	}
	a := &auth.Auth{
		Kind:         "minimax",
		AccessToken:  r.AccessToken,
		RefreshToken: r.RefreshToken,
		ExpiresAt:    r.ExpiresAt,
		Domain:       Domain,
		ApiHost:      ApiHost,
		UID:          r.UID,
		Nickname:     nick,
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return "", err
	}
	a.FilePath = filepath.Join(authDir, "minimax-"+r.UID+".json")
	if err := a.SaveAtomic(); err != nil {
		return "", err
	}
	return a.FilePath, nil
}

// ---------------------------------------------------------------------------
// 内部实现
// ---------------------------------------------------------------------------

// tokenResponse 是 /oauth2/token 的**宽松**解析结果：成功字段与错误字段放在一起，
// 因为本渠道同一个端点既回令牌、也回 pending/slow_down/denied/expired 两种信封。
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"` // 单位：秒
	Scope        string `json:"scope"`
	Status       string `json:"status"` // 200 信封：pending / slow_down / denied / expired
	Error        string `json:"error"`  // 非 200 信封：authorization_pending / slow_down / …
	ErrorDesc    string `json:"error_description"`
	Nickname     string `json:"nickname"` // 上游"可能"下发（§8.1 凭据有 nickname 字段）；缺失就走 SaveAuth 兜底
}

// parseTokenGrant 解析并**硬校验**令牌响应（§3.5，任一条不满足即抛）。
//
// 关于 expires_at 的坑（§3.6，必须照抄）：MiniMax 的 access_token **不是 JWT**
// （实测前缀 mmoat_、60 字符、0 个点；refresh_token 前缀 mmort_ 同样非 JWT），
// 所以 `expiresAt` **只能**由 `now + expires_in` 写入，绝不能去解 JWT `exp`；
// 同理 `account_id`（JWT sub）对真实凭据恒为 undefined，这里也就没有"账号 ID"可读，
// UID 只能由 access_token 派生（见 deriveUID）。
//
// 错误文案刻意**不带响应体原文**：令牌响应里就是 access/refresh token，
// 而这些错误会经 app.pollLogin 进日志（AGENTS.md §0.2 零 token 落盘）。
func parseTokenGrant(tr tokenResponse) (Result, error) {
	if !hasScope(tr.Scope, Scope) {
		return Result{}, fmt.Errorf("令牌响应 scope 不含 %s（scope=%q）", Scope, tr.Scope)
	}
	if strings.TrimSpace(tr.AccessToken) == "" {
		return Result{}, errors.New("令牌响应缺 access_token")
	}
	// 登录路径没有"上一个 refresh token"可回退（§3.5 第 2 条的回退分支只适用于续期），
	// 故此处必须非空。
	if strings.TrimSpace(tr.RefreshToken) == "" {
		return Result{}, errors.New("令牌响应缺 refresh_token")
	}
	if !strings.EqualFold(strings.TrimSpace(tr.TokenType), "bearer") {
		return Result{}, fmt.Errorf("令牌响应 token_type 不是 bearer（got %q）", tr.TokenType)
	}
	if tr.ExpiresIn <= 0 {
		return Result{}, fmt.Errorf("令牌响应 expires_in 非正数（%d）", tr.ExpiresIn)
	}
	expiresAt := time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	return Result{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresAt:    expiresAt,
		Scope:        strings.TrimSpace(tr.Scope),
		Domain:       Domain,
		ApiHost:      ApiHost,
		UID:          deriveUID(tr.AccessToken),
		Nickname:     strings.TrimSpace(tr.Nickname),
	}, nil
}

// deriveUID 由 access_token 派生本工具内的账号短标识（文件名安全）。
//
// 为什么以 access_token 为身份来源：上游账号池的身份标识就是 access_token
// （§8.1「账号池身份标识用的就是它」、§8.2 `identityOf = access_token`），
// 且文档给出的 uid 派生先例就是 `sha256(accessToken)` 的 hex 前 16 字符（同文档 155 行）。
//
// ⚠️ 已知代价（**未解决，如实标注**）：access_token 每次登录都是新签发的，
// 因此"同一账号重复登录"会得到**新的** uid、写成**新文件**，而不是覆盖旧文件。
// 文档中唯一可能的稳定账号标识 `account_id` 来自 JWT `sub`，对真实凭据**恒 undefined**
// （§3.6/§8.1），故当前没有稳定标识可用；是否另有稳定字段**未确认**。
// 多出来的旧文件可由面板「删除账号」清理，不影响正确性（旧 refresh token 轮换后本已失效）。
func deriveUID(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return hex.EncodeToString(sum[:])[:16]
}

// newPKCE 生成 PKCE 对（§3.2）：
//
//	code_verifier  = randomBytes(32) 的 base64url（无填充）
//	code_challenge = sha256(code_verifier) 的 base64url —— **verifier 按 ASCII 编码**
//
// 注意 challenge 的输入是 verifier 字符串本身（不是 32 字节原文）。
func newPKCE() (verifier, challenge string) {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

// hasScope 按空格切分 scope 字符串判断是否含目标值（§3.5 第 5 条的判据）。
func hasScope(scope, want string) bool {
	for _, f := range strings.Fields(scope) {
		if f == want {
			return true
		}
	}
	return false
}

// postForm 发一个 form-urlencoded 的 POST，返回原始 body 与状态码。
// 非 2xx 也要读 body：本渠道的错误信封（authorization_pending 等）就在 body 里。
func postForm(client *http.Client, path string, form url.Values) ([]byte, int, error) {
	if client == nil {
		client = NewClient()
	}
	req, err := http.NewRequest(http.MethodPost, endpointHost+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return raw, resp.StatusCode, nil
}

// intervalOr 返回当前有效 interval（秒），非法值回退缺省。
func (st state) intervalOr() int {
	if st.IntervalSec <= 0 {
		return defaultIntervalSec
	}
	return st.IntervalSec
}

// withNextPoll 返回把 NextPollAt 推到 now+interval 后的状态副本（不改 interval）。
func (st state) withNextPoll(now time.Time, intervalSec int) state {
	st.NextPollAt = now.Add(time.Duration(intervalSec) * time.Second).Unix()
	return st
}

// backoff 按给定 interval 退避并落盘，返回 ret（正常是 ErrPending）。
func (st state) backoff(statePath string, now time.Time, intervalSec int, ret error) error {
	st.IntervalSec = intervalSec
	st.NextPollAt = now.Add(time.Duration(intervalSec) * time.Second).Unix()
	_ = writeState(statePath, st)
	return ret
}

// fail 把终态错误固化进 state 并返回它。
// 固化是刻意的：app.pollLogin 对非 ErrPending 错误是"记日志继续轮询"（同 trae），
// 不固化就会每 2s 重复问上游同一个必然失败的请求（§0.1 最小请求数）。
func (st state) fail(statePath, msg string) error {
	st.Err = msg
	_ = writeState(statePath, st)
	return errors.New(msg)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// structuredErr 从响应体里只取结构化错误字段（error / error_description）用于错误文案。
//
// 为什么不直接回显响应体：设备码响应含 device_code，令牌响应含 access/refresh token，
// 而这些错误会进日志与面板（AGENTS.md §0.2「零 token 落盘」）。取不到结构化字段时
// 就明确说"响应体未记录"，不退回原文。
func structuredErr(raw []byte, maxDesc int) string {
	var e struct {
		Error     string `json:"error"`
		ErrorDesc string `json:"error_description"`
		Status    string `json:"status"`
		StatusMsg string `json:"status_msg"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &e) != nil {
		return "（响应体非 JSON，未记录：避免凭据进入日志）"
	}
	parts := make([]string, 0, 3)
	for _, v := range []string{e.Error, e.Status, e.StatusMsg} {
		if strings.TrimSpace(v) != "" {
			parts = append(parts, strings.TrimSpace(v))
		}
	}
	if desc := truncate(e.ErrorDesc, maxDesc); desc != "" {
		parts = append(parts, desc)
	}
	if len(parts) == 0 {
		return "（上游未给出结构化错误字段，响应体未记录：避免凭据进入日志）"
	}
	return strings.Join(parts, " | ")
}

func readState(path string) (state, error) {
	var st state
	raw, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(raw, &st)
	return st, err
}

func writeState(path string, st state) error {
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	raw, _ := json.MarshalIndent(st, "", "  ")
	return os.WriteFile(path, raw, 0o600)
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
