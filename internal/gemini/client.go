// client.go Gemini（Google Code Assist / Cloud Code）渠道客户端，实现 provider.Upstream。
//
// 渠道形态：**正常多账号 OAuth 渠道**（浏览器授权 + 本机回调登录，凭据落
// `auths/gemini-*.json`，可 refresh_token 续期），故**不置** SingleAccount：
// 账号级错误按正常分类走池内轮换（R24/R29）。上游文档明确「换端点是零成本无害、
// 但**不是** quota 的有效解法，主救场手段是**换账号**」（协议 §2.1）。
//
// 四条必须原样保留的协议事实（协议 §2.2 / §2.5 / §2.7）：
//  1. 鉴权 = `Bearer` + **逐字身份五头**；**不带** `x-goog-api-key`；
//     流式请求**刻意不带** `Accept`（抓包一致）；
//  2. 信封**逐层字母序序列化**（见 envelope.go 文件头）；
//  3. `loadCodeAssist` / `retrieveUserQuotaSummary` **固定走 sandbox**，其余（含推理）走轮换端点；
//  4. 配额是**窗口剩余百分比**、**没有签到**（DailyCheckin 返回明确的「不支持」错误）。
package gemini

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// Client 上游 HTTP 客户端。导出 Base / Daily / Sandbox / TokenURL 便于测试注入假上游。
type Client struct {
	// HTTP 用于非流式请求（配额 / 探测 / 续期），带总超时。
	HTTP *http.Client
	// StreamHTTP 用于对话 SSE：**不设**整体 Timeout，避免长回答在超时点被掐断且无终止帧
	// （R35）。卡死由空闲看门狗（IdleTimeout）兜底。
	StreamHTTP *http.Client
	// IdleTimeout 流中空闲上限；<=0 时回落到 DefaultIdleTimeout。
	IdleTimeout time.Duration

	// Base 覆盖**全部**端点（测试注入假上游用，见 NewWithBase）。
	Base string
	// Daily / Sandbox 分别覆盖主端点与沙箱端点；空 = 各自默认常量。
	Daily   string
	Sandbox string
	// TokenURL 覆盖 OAuth 令牌端点（仅续期用；空 = TokenEndpoint）。
	TokenURL string

	mu sync.Mutex
	// projCache 进程内 project 缓存：键 = 账号稳定标识（uid / access_token 兜底）。
	projCache map[string]string
	// quotaCache 配额查询缓存（TTL = QuotaCacheTTL），键同上。
	quotaCache map[string]quotaCacheEntry
}

// quotaCacheEntry 一次成功配额查询的结果与时刻。
type quotaCacheEntry struct {
	at    time.Time
	items []provider.ResourceItem
}

// New 生产默认客户端。
func New() *Client {
	tr := newTransport()
	return &Client{
		HTTP:        &http.Client{Timeout: requestTimeout, Transport: tr},
		StreamHTTP:  &http.Client{Transport: tr}, // 共用 Transport，不设 Timeout
		IdleTimeout: DefaultIdleTimeout,
	}
}

// NewWithBase 测试用：把**全部**端点（daily / sandbox / 探测 / 配额）都指向 base。
func NewWithBase(base string) *Client {
	c := New()
	c.Base = strings.TrimRight(strings.TrimSpace(base), "/")
	return c
}

// newTransport 出厂 Transport：禁 h2 + 连接池 + ResponseHeaderTimeout 兜底。
//
// StreamHTTP 无总超时后**必须**有 ResponseHeaderTimeout，否则连响应头都等不到就会无限挂住。
func newTransport() *http.Transport {
	return &http.Transport{
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // 强制 HTTP/1.1
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 15 * time.Second,
		}).DialContext,
	}
}

// endpoints 返回推理用的端点轮换顺序：`[daily, sandbox]`（协议 §2.1）。
func (c *Client) endpoints() []string {
	if b := strings.TrimRight(strings.TrimSpace(c.Base), "/"); b != "" {
		return []string{b}
	}
	daily := strings.TrimRight(strings.TrimSpace(c.Daily), "/")
	if daily == "" {
		daily = DailyHost
	}
	sandbox := strings.TrimRight(strings.TrimSpace(c.Sandbox), "/")
	if sandbox == "" {
		sandbox = SandboxHost
	}
	return []string{daily, sandbox}
}

// sandboxHost 返回沙箱端点基址（配额与探测**固定**走它，协议 §2.1）。
func (c *Client) sandboxHost() string {
	if b := strings.TrimRight(strings.TrimSpace(c.Base), "/"); b != "" {
		return b
	}
	if s := strings.TrimRight(strings.TrimSpace(c.Sandbox), "/"); s != "" {
		return s
	}
	return SandboxHost
}

// tokenURL 返回 OAuth 令牌端点。
func (c *Client) tokenURL() string {
	if s := strings.TrimSpace(c.TokenURL); s != "" {
		return s
	}
	return TokenEndpoint
}

// streamClient 返回流式调用用的客户端（nil 安全）。
func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP == nil {
		c.StreamHTTP = &http.Client{Transport: newTransport()}
	}
	return c.StreamHTTP
}

// idleTimeout 生效的空闲超时。
func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout <= 0 {
		return DefaultIdleTimeout
	}
	return c.IdleTimeout
}

// ---------------------------------------------------------------------------
// 请求头
// ---------------------------------------------------------------------------

// identityHeaders 注入鉴权与**逐字身份五头**（协议 §2.2）。
//
// ⚠️ 刻意**不设** `Accept`：流式请求抓包一致地不带它（geminiHeaders 的 includeAccept
// 默认 false）。⚠️ 刻意**不设** `x-goog-api-key` / `x-goog-api-client`：原版不带，
// 多一个头就多一个「非官方客户端」的识别特征。
func identityHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", IdentityUserAgent)
	req.Header.Set("x-client-name", IdentityClientName)
	req.Header.Set("x-client-version", IdentityClientVersion)
	req.Header.Set("x-machine-id", IdentityMachineID)
	req.Header.Set("x-vscode-sessionid", IdentitySessionID)
}

// ---------------------------------------------------------------------------
// provider.Upstream：对话
// ---------------------------------------------------------------------------

// ChatStream 把内层 OpenAI Chat 请求体翻成 v1internal 信封后发往流式端点。
//
// 返回约定（与其它渠道一致，handler 依赖它做分类与透传）：
//   - 传输错误   → (nil, 0, nil, err)（仅在**所有**端点都失败时）；
//   - HTTP ≥ 400 → (nil, status, 上游原始响应体, nil)；原文透传，不包装（不变量 8）；
//   - 200        → (带空闲看门狗的 body, 200, nil, nil)。
//
// 出站体不可构造（请求 JSON 非法 / 模型不在目录 / 消息为空 / 图片读不到）时返回
// **本地 400 + 错误体**而不是 err：err 会被 handler 当成传输层错误去惩罚健康账号。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	token := tokenOf(a)
	if token == "" {
		return nil, http.StatusUnauthorized,
			localErrorBody(http.StatusUnauthorized, "session_dead", "gemini：账号没有 access_token"), nil
	}

	// project：凭据真值 → 进程内缓存 → 现探（探测失败**不发推理**，协议 §2.7）。
	project, err := c.resolveProject(a)
	if err != nil {
		status := http.StatusBadGateway
		var ue *provider.Error
		if errors.As(err, &ue) && ue.Status > 0 {
			status = ue.Status
		}
		return nil, status,
			localErrorBody(status, "upstream_error", "gemini：project 探测失败，未发推理（"+err.Error()+"）"), nil
	}
	out, code, err := buildInternalBody(body, project)
	if err != nil {
		return nil, http.StatusBadRequest, localErrorBody(http.StatusBadRequest, code, "gemini："+err.Error()), nil
	}

	eps := c.endpoints()
	var lastErr error
	for i, ep := range eps {
		req, err := http.NewRequest(http.MethodPost, ep+EpStreamGenerate+StreamQuery, bytes.NewReader(out))
		if err != nil {
			return nil, 0, nil, err
		}
		identityHeaders(req, token)
		resp, err := c.streamClient().Do(req)
		if err != nil {
			lastErr = err
			if i < len(eps)-1 {
				continue // 传输层失败 → 换下一个端点（协议 §2.1 的轮换顺序）
			}
			return nil, 0, nil, err
		}
		if resp.StatusCode >= 400 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			// 5xx 属上游故障：换下一个端点再试一次；其余状态原样透传给 Classify。
			if resp.StatusCode >= 500 && i < len(eps)-1 {
				lastErr = fmt.Errorf("gemini：端点 %s 返回 %d", ep, resp.StatusCode)
				continue
			}
			return nil, resp.StatusCode, raw, nil
		}
		return provider.NewIdleReader(resp.Body, c.idleTimeout()), resp.StatusCode, nil, nil
	}
	if lastErr == nil {
		lastErr = errors.New("gemini：没有可用端点")
	}
	return nil, 0, nil, lastErr
}

// Stream 把上游流（v1internal SSE）转成 OpenAI SSE 后交给统一规范化层。
// model 是客户端请求的原始模型名，用于回填每个 chunk 的 model 字段。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	err := upstream.StreamCapture(w, newGeminiStream(r, model), func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 把上游流收敛成单个 OpenAI chat.completion 响应（非流式客户端走这里）。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	return upstream.Aggregate(newGeminiStream(r, model))
}

// ---------------------------------------------------------------------------
// provider.Upstream：续期
// ---------------------------------------------------------------------------

// RefreshToken 用 refresh_token 续期（协议 §2.3）。
//
// 表单**必须**带 client_secret（协议 §2.2 末段：token 端点强制校验客户端身份，
// 只发 client_id 会回 `invalid_request: client_secret is missing`；症状是「浏览器显示
// 授权成功但账号一直不出现」）。
//
// ⚠️ Google **偶尔轮换 refresh_token**：响应给了新的就必须回写；调用方**必须**紧跟
// `SaveAtomic`（不变量 20），否则下次启动用旧 refresh token → invalid_grant → 好账号被误标死。
//
// 终态判定返回 *provider.Error{Kind: ErrSessionDead}（上层 errors.As 依赖，不变量 19/23）。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()

	old := strings.TrimSpace(a.RefreshToken)
	if old == "" {
		return &provider.Error{Kind: provider.ErrSessionDead, Msg: "gemini：账号没有 refresh_token"}
	}
	form := url.Values{}
	form.Set("client_id", ClientID())
	form.Set("client_secret", ClientSecret())
	form.Set("refresh_token", old)
	form.Set("grant_type", "refresh_token")

	status, raw, err := c.postForm(c.tokenURL(), form)
	if err != nil {
		return err
	}
	if status >= 400 {
		msg := oauthErrorMessage(raw)
		if hitsTerminal(msg) || hitsTerminal(string(raw)) {
			return &provider.Error{Kind: provider.ErrSessionDead, Status: status,
				Msg: "gemini：refresh_token 已失效，请重新登录（" + msg + "）"}
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return &provider.Error{Kind: provider.ErrSessionDead, Status: status,
				Msg: "gemini：续期被拒（" + msg + "）"}
		}
		return &provider.Error{Kind: Classify(status, string(raw)), Status: status,
			Msg: "gemini：续期失败（" + msg + "）"}
	}

	grant, err := ParseTokenGrant(raw, old)
	if err != nil {
		return err
	}
	a.AccessToken = grant.AccessToken
	a.RefreshToken = grant.RefreshToken
	a.ExpiresAt = time.Now().Add(time.Duration(grant.ExpiresIn) * time.Second).Unix()
	return nil
}

// TokenGrant 令牌端点的解析结果（**登录包与续期共用同一口径**，故导出）。
type TokenGrant struct {
	AccessToken  string
	RefreshToken string // 轮换后的新值；响应未给时回退调用方传入的旧值
	ExpiresIn    int64  // **秒**（auth.Auth.ExpiresAt 的口径是秒，换算只在本包内做）
	IDToken      string
}

// ParseTokenGrant 解析令牌端点响应并硬校验（协议 §2.3）。
//
// 三处口径照参考实现（gemini-oauth.ts:147-178）：
//   - `error` 字段存在即失败（错误文案里带上 error_description）；
//   - `access_token` 必需；
//   - `expires_in` 缺失/非正时**回落 3600**（原版同款兜底），因为 access_token 是
//     Google 的 opaque token（`ya29.` 开头，不是 JWT），没有第二个到期来源可读。
//
// ⚠️ 错误文案**只含信封字段**，不含响应体原文（那里就是 token）——上层会把它写进日志。
func ParseTokenGrant(raw []byte, previousRefreshToken string) (TokenGrant, error) {
	var payload struct {
		AccessToken      string      `json:"access_token"`
		RefreshToken     string      `json:"refresh_token"`
		TokenType        string      `json:"token_type"`
		ExpiresIn        json.Number `json:"expires_in"`
		Scope            string      `json:"scope"`
		IDToken          string      `json:"id_token"`
		Error            string      `json:"error"`
		ErrorDescription string      `json:"error_description"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return TokenGrant{}, fmt.Errorf("gemini：令牌端点返回无法解析（%v）", err)
	}
	if payload.Error != "" {
		desc := payload.ErrorDescription
		if desc == "" {
			desc = payload.Error
		} else {
			desc = payload.Error + ": " + desc
		}
		return TokenGrant{}, fmt.Errorf("gemini：令牌端点报错（%s）", desc)
	}
	token := strings.TrimSpace(payload.AccessToken)
	if token == "" {
		return TokenGrant{}, errors.New("gemini：令牌端点未返回 access_token")
	}
	expiresIn := int64(3600)
	if n, err := payload.ExpiresIn.Int64(); err == nil && n > 0 {
		expiresIn = n
	}
	refresh := strings.TrimSpace(payload.RefreshToken)
	if refresh == "" {
		refresh = previousRefreshToken
	}
	return TokenGrant{
		AccessToken:  token,
		RefreshToken: refresh,
		ExpiresIn:    expiresIn,
		IDToken:      payload.IDToken,
	}, nil
}

// ---------------------------------------------------------------------------
// provider.Upstream：模型清单 / 定价
// ---------------------------------------------------------------------------

// FetchModels 实现 provider.Upstream：**纯静态表**，不发任何上游请求
// （协议 §2.6：用户 2026-10-03 拍板静态表，不拉远端目录）。
func (c *Client) FetchModels(_ *auth.Auth) ([]provider.ModelInfo, error) { return StaticModels(), nil }

// FetchModelPricing 本渠道**没有费率端点**（协议 §2.6/§2.7 全篇无倍率/定价字段），
// 故返回空切片——面板显示「未知」，而不是把缺失当成「免费」。
func (c *Client) FetchModelPricing(_ *auth.Auth) ([]provider.ModelPricing, error) {
	return []provider.ModelPricing{}, nil
}

// ---------------------------------------------------------------------------
// provider.Upstream：签到（本渠道没有）
// ---------------------------------------------------------------------------

// DailyCheckin 实现 provider.Upstream：**Gemini 没有签到机制**（协议 §2.7 末行：
// 无签到端点、无每日领取）。这里返回明确的「不支持」错误而不是 nil——
// 返回 nil 会让调度器以为「今日已签到」，把真实缺陷伪装成正常。
//
// 渠道装配时应声明为「无签到」（scheduler.Config 的 CheckinMinutes 置 []int{}，R39），
// 本方法只是防呆：被调用即说明接线漏了。
func (c *Client) DailyCheckin(_ *auth.Auth) error {
	return errors.New("gemini：本渠道没有签到机制（配额为窗口百分比，无每日领取）")
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Classify 按状态码 + body 语义分类（ErrKind 驱动 pool 的冷却状态机）。
//
// 判据顺序与出处（协议 §2.5 的「HTTP 状态 → 错误归类」表）：
//   - **429 必须排在 quota 文案之前**：限流 body 高频带 `quota exceeded` 字样，
//     先判硬余额会把限流误归余额耗尽 → 长冷却（AGENTS.md 不变量 15）；
//   - 上下文超限（`The input token count exceeds the maximum number of tokens` 等）
//     → ErrPromptTooLong：请求级、不罚号、不轮转，但**让上层去压缩重试**
//     （归 ErrBadParams 会让 harness 直接放弃）；
//   - 401 → ErrSessionDead（凭据失效，需重新登录；上层 refresh + 重试一次，不变量 19）；
//   - **403 / 404 → ErrServer**：协议 §2.5 明确「计划 §3.1 如此，不用 httpErrorCode 的
//     既有映射」——403 的典型成因是本账号缺权益（SUBSCRIPTION_REQUIRED），换号有效；
//   - 其余 4xx：含 quota / resource_exhausted / rate limit / exceeded 字样 → ErrHardCredit
//     （余额耗尽/需换号），否则 ErrClient。
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)
	// 本包自产的本地错误：错误码足以定性，先短路。
	if provider.CodeMarker(lower, errCodePromptTooLong) {
		return provider.ErrPromptTooLong
	}
	if provider.CodeMarker(lower, errCodeBadParams) {
		return provider.ErrBadParams
	}
	switch {
	case status == http.StatusTooManyRequests:
		return provider.ErrSoftRate
	case isContextOverflow(lower):
		return provider.ErrPromptTooLong
	case status == http.StatusUnauthorized:
		return provider.ErrSessionDead
	case status == http.StatusForbidden, status == http.StatusNotFound:
		return provider.ErrServer
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		if isQuotaExhausted(lower) {
			return provider.ErrHardCredit
		}
		return provider.ErrClient
	default:
		return provider.ErrNone
	}
}

// isContextOverflow 上下文超限判据（协议 §2.5）。
func isContextOverflow(lower string) bool {
	return strings.Contains(lower, "exceeds the maximum number of tokens") ||
		strings.Contains(lower, "exceeds the maximum") ||
		strings.Contains(lower, "context window exceeded") ||
		strings.Contains(lower, "contextwindowexceeded") ||
		(strings.Contains(lower, "token count") && strings.Contains(lower, "exceed"))
}

// isQuotaExhausted 配额/限流类文案判据（协议 §2.5）。
func isQuotaExhausted(lower string) bool {
	for _, m := range []string{"quota", "resource_exhausted", "rate limit", "exceeded"} {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// HTTP 小工具
// ---------------------------------------------------------------------------

// postJSON 发一个身份头齐全的 JSON POST（配额 / 探测用），返回状态码与响应体。
func (c *Client) postJSON(endpoint, token string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), quotaTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	identityHeaders(req, token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// postForm 发一个 form-urlencoded 的 OAuth 请求。
//
// ⚠️ 刻意**不设** `User-Agent`：`antigravity` 伪装只用在 Cloud Code 的推理/配额端点；
// 令牌端点是标准 Google OAuth，抓包看到的就是 Go 默认 UA，加伪装反而多一个可疑特征
// （gemini-oauth.ts:240-246）。
func (c *Client) postForm(endpoint string, form url.Values) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), oauthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// tokenOf 取账号的 access token 快照（锁内读，防与续期写回竞争）。
func tokenOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.AccessTokenValue())
}

// localErrorBody 构造本地拒绝的 OpenAI 风格错误体。
// code 同时驱动上层 Classify（bad_params / prompt_too_long / session_dead）。
func localErrorBody(status int, code, msg string) []byte {
	if code == "" {
		code = errCodeBadParams
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "invalid_request_error",
		"code":    code,
		"status":  status,
	}})
	return b
}

// oauthErrorMessage 取 OAuth 错误响应里的可读文本（error / error_description / message）。
func oauthErrorMessage(raw []byte) string {
	var e struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
		Message          string `json:"message"`
	}
	if err := json.Unmarshal(raw, &e); err == nil {
		switch {
		case e.Error != "" && e.ErrorDescription != "":
			return e.Error + ": " + e.ErrorDescription
		case e.Error != "":
			return e.Error
		case e.Message != "":
			return e.Message
		}
	}
	return snippet(raw)
}

// hitsTerminal 报告错误文本是否命中续期终态判据（协议 §2.3 的终态错误名
// RefreshTokenExpiredError 对应的判据集，gemini-auth.ts:456）。
func hitsTerminal(s string) bool {
	l := strings.ToLower(s)
	for _, m := range []string{"invalid_grant", "invalid_client", "expired", "revoked", "unauthorized"} {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// snippet 截断响应体用于错误文案（单行化，最长 300 字节）。
func snippet(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len(s) > 300 {
		s = s[:300]
	}
	if s == "" {
		return "（上游未给出正文）"
	}
	return s
}

// ---------------------------------------------------------------------------
// project / 配额缓存（进程内）
// ---------------------------------------------------------------------------

// projectKey 返回账号的稳定缓存键：uid → access_token 兜底。
//
// ⚠️ 优先稳定标识而不是 access_token：后者每次续期都变，用它做键会让缓存恒不命中
// （协议 §2.7 明确「缓存键用 email/sub，不用 access_token」；本工具侧 uid 就是 OIDC 的 sub）。
func projectKey(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	a.RLock()
	defer a.RUnlock()
	if u := strings.TrimSpace(a.UID); u != "" {
		return "uid:" + u
	}
	return "tok:" + strings.TrimSpace(a.AccessToken)
}

// storedProject 取凭据里已落盘的 project 真值（无则空串）。
func storedProject(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	a.RLock()
	defer a.RUnlock()
	if a.Extra == nil {
		return ""
	}
	return strings.TrimSpace(a.Extra[ExtraKeyProject])
}

// cacheProject 记入进程内 project 缓存。
func (c *Client) cacheProject(key, project string) {
	if key == "" {
		return
	}
	c.mu.Lock()
	if c.projCache == nil {
		c.projCache = map[string]string{}
	}
	c.projCache[key] = project
	c.mu.Unlock()
}

// cachedProject 读进程内 project 缓存。
func (c *Client) cachedProject(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.projCache[key]
	return v, ok
}

// resolveProject 解析出站信封要带的 project（协议 §2.7 的三级顺序 + 探测纪律）：
//
//	凭据里的真值（Extra） → 进程内缓存 → 现探 loadCodeAssist
//
// ⚠️ **探测失败时不发推理**（返回 error）：这是原版实测行为（2026-10-05 注入 500 验证）。
// 只有「200 但 project 为空」（免费档常态）才回落 DefaultProject 并照常发推理。
func (c *Client) resolveProject(a *auth.Auth) (string, error) {
	if p := storedProject(a); p != "" {
		return p, nil
	}
	key := projectKey(a)
	if p, ok := c.cachedProject(key); ok && p != "" {
		return p, nil
	}
	info, err := c.loadCodeAssist(a)
	if err != nil {
		return "", err
	}
	project := strings.TrimSpace(info.Project)
	if project == "" {
		project = DefaultProject
	}
	c.cacheProject(key, project)
	// 只回写**探测到的真值**，不回写兜底串（协议 §2.7 末段）。
	if strings.TrimSpace(info.Project) != "" {
		c.persistProject(a, info)
	}
	return project, nil
}

// persistProject 把探测到的 project / 账号档位写回凭据并落盘。
//
// 只写**非空真值**：空 project 不写（否则 Extra 里留一个空串，看起来像「已知为空」，
// 而实际语义是「尚未探测到」）。
//
// 落盘用 auth.Auth.SaveAtomic()（不变量 3：与 auth.Parse 同源格式）。
// 失败只记日志：project 探测成功但写盘失败不该让本次请求失败。
func (c *Client) persistProject(a *auth.Auth, info codeAssistInfo) {
	if a == nil {
		return
	}
	project := strings.TrimSpace(info.Project)
	if project == "" && info.PaidTierID == "" && info.PaidTierName == "" {
		return
	}
	a.Lock()
	if a.Extra == nil {
		a.Extra = map[string]string{}
	}
	if project != "" {
		a.Extra[ExtraKeyProject] = project
	}
	if info.PaidTierID != "" {
		a.Extra[ExtraKeyPaidTier] = info.PaidTierID
	}
	if info.PaidTierName != "" {
		a.Extra[ExtraKeyPaidTierName] = info.PaidTierName
	}
	if label := tierLabel(info.PaidTierID, info.PaidTierName); label != "" {
		a.Extra[ExtraKeyPaidTierLabel] = label
	}
	hasPath := a.FilePath != ""
	a.Unlock()
	if !hasPath {
		return
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("gemini：project/档位写回凭据失败 uid=%s err=%v", a.UID, err)
	}
}
