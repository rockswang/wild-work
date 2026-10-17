// client.go MiniMax Code（中国版）渠道客户端，实现 provider.Upstream。
//
// 渠道形态：**正常多账号 OAuth 渠道**（设备码 + PKCE 登录，凭据落 auths/minimax-*.json）。
// 因此**不置 SingleAccount**（main 侧装配已如此）：账号级错误按正常分类走池内轮换
// （R24/R29），而 402 权益不足这类「账号健康、只是没钱」的语义仍由 ErrHardCredit 表达。
//
// 三条与其它渠道不同的协议事实（协议 §8.3 / §5）：
//   - 对话路径**不做换号轮转**：本渠道只做「凭据过期 → 刷新一次」与「其余 HTTP 失败 →
//     抛错」，多账号轮换是**上层 pool** 的职责（靠 Classify 的分类驱动）；
//   - access_token **不是 JWT**（前缀 mmoat_，60 字符），因此 expires_at 只能由
//     `now + expires_in` 写入，不能像 trae/qwenwork 那样从 JWT exp 反推（协议 §3.6）；
//   - 上游 HTTP 402 一律映射为「余额/权益不足」（协议 §5 末条）。
package minimax

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// Client 上游 HTTP 客户端。Base / Account 可覆盖以便测试。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 专供对话流式请求：**无总超时**（R35）。
	//
	// 为什么必须分开：http.Client.Timeout 是整请求上限（计时器在 Do() 返回后继续跑，
	// 直到 body 读完），而 SSE 整个生成期都在读 body —— 长思考请求会被从流中间掐断，
	// 客户端表现为「突然无响应」。空闲兜底改由 provider.IdleReader 承担。
	StreamHTTP *http.Client
	// IdleTimeout 流式空闲超时：连续该时长读不到任何字节即判上游卡死。
	// 0 = 用 DefaultIdleTimeout（本包没有配置项，main 也不注入）。
	IdleTimeout time.Duration
	// Base API 主机（目录 / 推理 / 签到 / 积分），默认 APIHost。
	Base string
	// Account 账号 / OAuth 主机，默认 AccountHost。
	Account string

	// catMu 保护 dynamic（最近一次成功拉取的真实远端目录）。
	// 兜底表不缓存：一次瞬时失败不得污染整个进程生命周期（协议 §6 缓存纪律）。
	catMu   sync.RWMutex
	dynamic []catalogEntry
}

// New 生产默认客户端。
func New() *Client {
	tr := newTransport()
	return &Client{
		HTTP:       &http.Client{Timeout: requestTimeout, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr}, // 共用 Transport，不设 Timeout
		Base:       APIHost,
		Account:    AccountHost,
	}
}

// NewWithBase 测试用：覆盖 API 主机（账号主机仍为默认，需要时另设 c.Account）。
func NewWithBase(base string) *Client {
	c := New()
	c.Base = base
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
	}
}

// apiHost 返回 API 主机基址。
func (c *Client) apiHost() string {
	if s := strings.TrimRight(strings.TrimSpace(c.Base), "/"); s != "" {
		return s
	}
	return APIHost
}

// accountHost 返回账号 / OAuth 主机基址。
func (c *Client) accountHost() string {
	if s := strings.TrimRight(strings.TrimSpace(c.Account), "/"); s != "" {
		return s
	}
	return AccountHost
}

// streamClient 返回流式调用用的客户端（无总超时，回落新建保证 nil 安全）。
func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP == nil {
		c.StreamHTTP = &http.Client{Transport: newTransport()}
	}
	return c.StreamHTTP
}

// idleTimeout 生效的空闲超时（未注入时用默认值）。
func (c *Client) idleTimeout() time.Duration {
	if c.IdleTimeout <= 0 {
		return DefaultIdleTimeout
	}
	return c.IdleTimeout
}

// ---------------------------------------------------------------------------
// provider.Upstream：对话
// ---------------------------------------------------------------------------

// ChatStream 把 OpenAI Chat 请求体翻成 Anthropic Messages 方言后转发。
//
// 返回约定（与其它渠道一致，handler 依赖它做分类与透传）：
//   - 传输错误            → (nil, 0, nil, err)
//   - HTTP ≥ 400          → (nil, status, 上游原始响应体, nil)   ← 原文透传，不包装
//   - 200                 → (带空闲看门狗的 body, 200, nil, nil)
//
// 出站体不可构造（请求 JSON 非法 / 消息为空 / 图片读不到 / 模型不支持图片）时返回
// **本地 400 + 错误体**而不是 err：err 会被 handler 当成传输层错误而去
// `Pool.NoteError` 并换号重试——那是在为客户端的问题惩罚健康账号；
// 本地 400 走 status≥400 分支，Classify 把它归到 ErrBadParams（请求级、不罚号、不轮转）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	model := upstreamModelName(modelOf(body))
	entry, known := c.catalogEntry(model)
	out, berr := buildMessagesBody(body, entry, known)
	if berr != nil {
		return nil, http.StatusBadRequest, badParamsBody(berr), nil
	}
	req, err := http.NewRequest(http.MethodPost, c.apiHost()+EpMessages, bytes.NewReader(out))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tokenOf(a))
	req.Header.Set("Content-Type", "application/json")
	// 只需要这三个头：**不需要 anthropic-version，也不需要 machine 头或签名**
	// （协议 §2 明确列出，实测只带上述头即 HTTP 200）。
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.streamClient().Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, resp.StatusCode, raw, nil
	}
	// 空闲看门狗兜底：连续 idleTimeout 读不到字节即判上游卡死并关掉 body。
	// 流式与 aggregate 路径共用这一层（聚合时由「总时长上限」变为「空闲上限」）。
	return provider.NewIdleReader(resp.Body, c.idleTimeout()), resp.StatusCode, nil, nil
}

// Stream 把上游流（Anthropic SSE）转成 OpenAI SSE 后交给统一规范化层。
// model 是客户端请求的原始模型名，用于回填每个 chunk 的 model 字段。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	err := upstream.StreamCapture(w, newAnthropicStream(r, model), func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 把上游流收敛成单个 OpenAI chat.completion 响应（非流式客户端走这里）。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	return upstream.Aggregate(newAnthropicStream(r, model))
}

// ---------------------------------------------------------------------------
// provider.Upstream：续期
// ---------------------------------------------------------------------------

// RefreshToken 用 refresh_token 续期（协议 §3.7）。
//
// 请求是 form-urlencoded：grant_type=refresh_token + refresh_token + client_id + scope + audience。
// 终态判定：错误文本命中 invalid_grant / expired / revoked → ErrSessionDead
// **语义的 \*provider.Error**（上层 errors.As 判定自愈与禁用；裸 fmt.Errorf 会让自愈失效，
// 见 AGENTS.md 不变式 19）。
//
// 注意 expires_at 只能由 `now + expires_in` 写入：access_token **不是 JWT**
// （前缀 mmoat_、60 字符、0 个点），没有 exp 可读（协议 §3.6）。
// 同步轮换 refresh_token（上游会轮换），因此调用方**必须**紧跟 SaveAtomic
// ——不落盘 = 下次启动用旧 refresh token（不变式 20）。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()

	old := strings.TrimSpace(a.RefreshToken)
	if old == "" {
		return &provider.Error{Kind: provider.ErrSessionDead, Msg: "minimax：账号没有 refresh_token"}
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", old)
	form.Set("client_id", OAuthClientID)
	form.Set("scope", OAuthScope)
	form.Set("audience", OAuthAudience)

	status, raw, err := c.postForm(c.accountHost()+EpToken, form)
	if err != nil {
		return err
	}
	if status >= 400 {
		// 非 2xx：先看错误文本是否命中终态判据（协议 §3.7），再退到通用状态码分类。
		msg := oauthErrorMessage(raw)
		if hitsTerminal(msg) || hitsTerminal(string(raw)) {
			return &provider.Error{Kind: provider.ErrSessionDead, Status: status, Msg: "minimax：refresh_token 已失效（" + msg + "）"}
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return &provider.Error{Kind: provider.ErrSessionDead, Status: status, Msg: "minimax：续期被拒（" + msg + "）"}
		}
		return &provider.Error{Kind: Classify(status, string(raw)), Status: status, Msg: "minimax：续期失败（" + msg + "）"}
	}

	var g struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &g); err != nil {
		return fmt.Errorf("minimax：续期响应解析失败：%w", err)
	}
	// 令牌响应硬校验（协议 §3.5，设备码与续期共用同一个解析器）。
	if strings.TrimSpace(g.AccessToken) == "" {
		return fmt.Errorf("minimax：续期响应缺 access_token")
	}
	tokenType := strings.ToLower(strings.TrimSpace(g.TokenType))
	if tokenType != "" && tokenType != "bearer" {
		return fmt.Errorf("minimax：续期响应 token_type=%q（期望 bearer）", g.TokenType)
	}
	// scope：**存在但缺产品声明的 scope** 即失败（协议 §3.5 第 5 条）。
	// 字段整体缺失时放行——文档没有钉死续期响应一定回 scope，为此拒绝一个可能完全
	// 正常的新令牌，代价是账号被误判为会话失效，得不偿失（此处是与文档的有意容差）。
	if s := strings.TrimSpace(g.Scope); s != "" && !hasScope(s, OAuthScope) {
		return fmt.Errorf("minimax：续期响应 scope=%q 不含 %s", s, OAuthScope)
	}
	// expires_in 必须是正数：access_token 不是 JWT，没有第二个到期来源
	// （协议 §3.6），拿不到就只能报错，绝不能留一个"看起来还有效"的假到期时刻
	// ——那会让 NeedsRefresh 恒假、永远不再刷新。
	if g.ExpiresIn <= 0 {
		return fmt.Errorf("minimax：续期响应 expires_in=%d（必须为正数）", g.ExpiresIn)
	}
	newRT := strings.TrimSpace(g.RefreshToken)
	if newRT == "" {
		// 回退到上一个 refresh token（协议 §3.5 第 2 条：缺失时回退调用方传入的那个）。
		newRT = old
	}
	a.AccessToken = strings.TrimSpace(g.AccessToken)
	a.RefreshToken = newRT
	a.ExpiresAt = time.Now().Add(time.Duration(g.ExpiresIn) * time.Second).Unix()
	return nil
}

// ---------------------------------------------------------------------------
// provider.Upstream：积分 / 签到
// ---------------------------------------------------------------------------

// UserResource 查询可消耗积分（= Σ details[].remaining_amount，协议 §7.2）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	remain, _, err := c.creditDetail(a)
	return remain, err
}

// UserResourceDetail 返回余额与明细条目。
//
// 明细只给**一条**汇总条目：`details[]` 里没有区分「本周期有效」的标志，`credit_type`
// 语义未实测，参考实现也把 expiredTotal 恒置 0、packages 恒空（协议 §7.2 末条）
// ——「不凭猜测分类」比「给一堆看不出含义的行」更有用。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return c.creditDetail(a)
}

// creditDetail 单次请求同时产出总额与明细（口径同源，避免两处各算一遍）。
func (c *Client) creditDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	tok := tokenOf(a)
	if tok == "" {
		return 0, nil, &provider.Error{Kind: provider.ErrSessionDead, Msg: "minimax：账号没有 access_token"}
	}
	status, raw, err := c.getJSON(c.apiHost()+EpCreditDetails, tok)
	if err != nil {
		return 0, nil, err
	}
	if status >= 400 {
		return 0, nil, classifyErr(status, raw)
	}
	top, perr := decodeObject(raw)
	if perr != nil {
		return 0, nil, fmt.Errorf("minimax：积分响应解析失败：%w", perr)
	}
	// credit/details 是**平铺**结构：total_count 与 base_resp 同级，**没有 data 键**
	// （协议 §7.3）。这里仍按「有 data 就用它，否则用顶层」的通用策略取值，兼容未来形态。
	body := businessObject(top)
	if code, msg, ok := baseResp(body, top); ok && code != 0 {
		return 0, nil, &provider.Error{Kind: provider.ErrClient, Status: status,
			Msg: fmt.Sprintf("minimax：积分接口业务码 %d %s", code, msg)}
	}
	// ⚠️ total_count **不是余额**，它是 details[] 的记录条数（协议 §7.2 实测：
	// 领取 800 积分后 total_count=1 而 remaining_amount="800.00"）。
	var sum int64
	for _, d := range detailsArray(body) {
		sum += amountOf(d["remaining_amount"])
	}
	items := []provider.ResourceItem{{
		Name: "积分余额", Total: sum, Remain: sum, Usable: true, Key: "credits",
	}}
	return sum, items, nil
}

// DailyCheckin 实现 provider.Upstream：每日签到（协议 §7.1）。
//
// 「今日已领取」返回 nil（幂等语义：重复领取同样 HTTP 200，判据是 claim_result）。
// 需要区分「已领取 / 本次领取 / 未知结果」的调度器请用 DailyCheckinReport。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.DailyCheckinReport(a)
	return err
}

// DailyCheckinReport 实现 provider.CheckinReporter：额外返回结构化状态，
// 让调度器能把「本次领到」与「今天已领过」分开记（两者都不是 error，但语义不同）。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	tok := tokenOf(a)
	if tok == "" {
		return provider.CheckinReport{Status: provider.CheckinNoToken, Msg: "无 access_token"}, nil
	}
	// timezone_id 必须走 **query 参数**（实测：请求头无效、不带参数或非法时区名都会失败；
	// 协议 §7.1）。请求体是空 JSON 对象 `{}`。
	endpoint := c.apiHost() + EpSigninClaim + "?timezone_id=" + url.QueryEscape(timezoneID())
	status, raw, err := c.postJSON(endpoint, tok, []byte("{}"))
	if err != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: err.Error()}, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		// 类型化的会话失效：调度器用 errors.As 判定「刷新一次再重试」（不变式 19）。
		return provider.CheckinReport{Status: provider.CheckinError},
			&provider.Error{Kind: provider.ErrSessionDead, Status: status, Msg: snippet(raw)}
	}
	if status >= 400 {
		e := classifyErr(status, raw)
		return provider.CheckinReport{Status: provider.CheckinError, Msg: e.Error()}, e
	}
	top, perr := decodeObject(raw)
	if perr != nil {
		return provider.CheckinReport{Status: provider.CheckinError, Msg: "签到响应解析失败"}, nil
	}
	// signin/claim 是**信封**结构：业务字段在 data 下（协议 §7.3）。
	body := businessObject(top)
	if code, msg, ok := baseResp(body, top); ok && code != 0 {
		// ⚠️ 「invalid timezone_id」也是 HTTP 200：只看状态码会把它当成签到成功
		// （协议 §7.3 实测）。
		return provider.CheckinReport{Status: provider.CheckinError,
			Msg: fmt.Sprintf("签到失败：业务码 %d %s", code, msg)}, nil
	}
	switch result := intOfRaw(body["claim_result"]); result {
	case SigninClaimAlreadyClaimed:
		// 幂等：**判据是响应里的 claim_result，不是 HTTP 状态码**（协议 §7.1）。
		return provider.CheckinReport{Status: provider.CheckinAlready, Msg: "今日已领取"}, nil
	case SigninClaimClaimed:
		points := intOfRaw(body["points"])
		return provider.CheckinReport{Status: provider.CheckinClaimed, Amount: points,
			Msg: fmt.Sprintf("签到成功 +%d", points)}, nil
	default:
		return provider.CheckinReport{Status: provider.CheckinError,
			Msg: fmt.Sprintf("未知领取结果 claim_result=%d", result)}, nil
	}
}

// ---------------------------------------------------------------------------
// provider.Upstream：错误分类
// ---------------------------------------------------------------------------

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Classify 按状态码 + body 语义分类（ErrKind 驱动 pool 的冷却状态机）。
//
// 顺序与判据都有实测依据，改动前请对照协议 §5 的 HTTP 层说明：
//   - **402 → ErrHardCredit**：余额不足必须原样透出，归成 SERVER/AUTH 会让用户看不到
//     「去充值」这一唯一有效动作（协议 §5 末条）；
//   - **429 必须排在硬余额判定之前**：限流 body 高频带 quota exceeded 字样，
//     先判硬余额会把限流误归成余额耗尽 → 长冷却（AGENTS.md 不变式 15）；
//   - 401/403 → ErrSessionDead（凭据失效，需重新登录；上层会 refresh+重试一次）；
//   - 业务码 2013 是**请求形态错误**（requires adaptive thinking / tool call result does not
//     follow tool call / unsupported content type 'image_url' 三种实测形态都带它）⇒
//     ErrBadParams：请求级、不罚号、不轮转。
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)
	switch {
	case status == http.StatusPaymentRequired:
		return provider.ErrHardCredit
	case status == http.StatusTooManyRequests:
		return provider.ErrSoftRate
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return provider.ErrSessionDead
	case status == http.StatusNotFound:
		return provider.ErrNotFound
	case status >= 500:
		return provider.ErrServer
	case status >= 400:
		// 本包自己产的本地 400（出站体不可构造）也走这条判据。
		if provider.CodeMarker(lower, "bad_params") || provider.CodeMarker(lower, "2013") {
			return provider.ErrBadParams
		}
		if strings.Contains(lower, "requires adaptive thinking") ||
			strings.Contains(lower, "does not follow tool call") ||
			strings.Contains(lower, "unsupported content type") {
			return provider.ErrBadParams
		}
		// 其余 4xx：按语义归 ErrClient（可能挂在账号权益上，允许换号一次）。
		return provider.ErrClient
	default:
		return provider.ErrNone
	}
}

// classifyErr 把 HTTP 层失败转成类型化的 *provider.Error。
func classifyErr(status int, raw []byte) error {
	return &provider.Error{Kind: Classify(status, string(raw)), Status: status, Msg: snippet(raw)}
}

// ---------------------------------------------------------------------------
// HTTP 小工具
// ---------------------------------------------------------------------------

// getJSON 发一个 Bearer 认证的 GET（业务端点）。
func (c *Client) getJSON(url, token string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	businessHeaders(req, token, false)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// postJSON 发一个 Bearer 认证的 POST（JSON 体；业务端点）。
func (c *Client) postJSON(url, token string, body []byte) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	businessHeaders(req, token, true)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// businessHeaders 业务端点的请求头（协议 §2）：
// `Authorization: Bearer`、`Accept: application/json`，有请求体时加 `Content-Type: application/json`。
func businessHeaders(req *http.Request, token string, hasBody bool) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
}

// postForm 发一个 form-urlencoded 的 OAuth 请求（协议 §2：OAuth 走 form 体，不是 JSON）。
//
// 超时用**上下文**而不是另建 client：必须复用 c.HTTP.Transport，否则会绕开
// devdump / 备用通道等启动期套在 Transport 上的包装（R57 的接线点只在 Transport）。
func (c *Client) postForm(url string, form url.Values) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), oauthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
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

// modelOf 取请求体里的 model。
func modelOf(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

// badParamsBody 本地出站体拒绝的错误体（OpenAI 风格）。
// code=bad_params 让上层 Classify 归到请求级错误：不罚号、不轮换、原文透传给客户端。
func badParamsBody(err error) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": "minimax：" + err.Error(),
		"type":    "invalid_request_error",
		"code":    "bad_params",
	}})
	return b
}

// oauthErrorMessage 取 OAuth 错误响应里的可读文本（error / error_description）。
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

// hitsTerminal 报告错误文本是否命中续期终态判据（协议 §3.7）。
// 参考实现用同名判据抛 RefreshTokenExpiredError，本仓对应的语义是 ErrSessionDead。
func hitsTerminal(s string) bool {
	l := strings.ToLower(s)
	for _, m := range []string{"invalid_grant", "expired", "revoked"} {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// hasScope 报告空格分隔的 scope 串里是否含 want（协议 §3.5 第 5 条）。
func hasScope(scope, want string) bool {
	for _, s := range strings.Fields(scope) {
		if s == want {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// 业务信封解析（协议 §7.3）
// ---------------------------------------------------------------------------

// decodeObject 把 JSON 解成「键 → 原始值」。
func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("不是 JSON 对象")
	}
	return m, nil
}

// businessObject 取业务字段所在的对象：**有 `data` 对象就用它，否则用顶层**
// （协议 §7.3 实现策略——signin/* 是信封、credit/details 是平铺，不能一刀切）。
func businessObject(top map[string]json.RawMessage) map[string]json.RawMessage {
	if raw, ok := top["data"]; ok {
		if obj, err := decodeObject(raw); err == nil && len(obj) > 0 {
			return obj
		}
	}
	return top
}

// baseResp 取业务码（协议 §7.3：**业务码在 base_resp.status_code，不是 code**）。
// 先在业务对象里找，再退到顶层（平铺结构里 base_resp 与业务字段同级）。
func baseResp(body, top map[string]json.RawMessage) (code int64, msg string, ok bool) {
	for _, m := range []map[string]json.RawMessage{body, top} {
		if m == nil {
			continue
		}
		raw, has := m["base_resp"]
		if !has {
			continue
		}
		obj, err := decodeObject(raw)
		if err != nil {
			continue
		}
		v, has := obj["status_code"]
		if !has {
			continue
		}
		return intOfRaw(v), trimJSONString(obj["status_msg"]), true
	}
	return 0, "", false
}

// detailsArray 取 details 数组：**缺失 / 非数组 ⇒ 空数组 ⇒ 余额 0**（协议 §7.2）。
//
// 这不是查询失败：「真的为 0」与「查不到」必须分开——只有 base_resp.status_code 非 0
// 或响应根本不是对象才算失败。
func detailsArray(body map[string]json.RawMessage) []map[string]json.RawMessage {
	raw, has := body["details"]
	if !has {
		return nil
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	out := make([]map[string]json.RawMessage, 0, len(list))
	for _, it := range list {
		if obj, err := decodeObject(it); err == nil {
			out = append(out, obj)
		}
	}
	return out
}

// amountOf 解析金额：协议 §7.2 实测 remaining_amount 是**字符串**（"800.00"），
// 因此必须同时兼容数字与字符串。小数按截断取整（积分面板是整数口径）。
func amountOf(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		s = strings.TrimSpace(s)
		if s == "" {
			return 0
		}
		if f, err := parseFloat(s); err == nil {
			return int64(f)
		}
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f)
	}
	return 0
}

// intOfRaw 宽松取整数（数字或数字字符串都认）。用于 claim_result / points /
// status_code 这些"上游可能给数字也可能给字符串"的字段。
func intOfRaw(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if f, err := parseFloat(strings.TrimSpace(s)); err == nil {
			return int64(f)
		}
	}
	return 0
}

// trimJSONString 取 JSON 字符串（非字符串时给空串，用于 status_msg）。
func trimJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// parseFloat 解析浮点（strconv 的薄封装，集中一处便于阅读与测试）。
func parseFloat(s string) (float64, error) { return strconv.ParseFloat(strings.TrimSpace(s), 64) }
