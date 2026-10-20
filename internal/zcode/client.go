// client.go ZCode（智谱 z.ai）渠道客户端，实现 provider.Upstream。
//
// # 渠道形态
//
// **正常多账号渠道**（服务端中介的设备授权流登录，凭据落 `auths/zcode-*.json`），
// 因此**不置 SingleAccount**：账号级错误按正常分类走池内轮换（R24/R29）。
//
// # 通道选路（**模型归属 + 凭据可用性**，本文件的第一个关键决策）
//
//	| 通道          | 端点                                              | 凭据                                      | 额外差异            |
//	|---------------|---------------------------------------------------|-------------------------------------------|---------------------|
//	| `start-plan`  | `https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages` | `Bearer <zcode_jwt>`           | 带 HTTP-Referer     |
//	| `coding-plan` | `https://api.z.ai/api/anthropic/v1/messages`       | `Bearer <coding_plan_key_zai ?? _bigmodel>` | **删掉 HTTP-Referer** |
//
// 选择依据（协议 §1.1 的「通道选路」代码事实 + §1.2.3）：
//
//  1. **`start-plan` 优先**：积分制是白给的额度，不该被订阅额度先消耗掉
//     （参考实现 `resolveChannelFor` 同序，用户 2026-10-03 决定）。
//     它的可用判据是「凭据里有非空 `zcode_jwt`」——即 Auth.AccessToken。
//  2. 只有凭据里存在 `coding_plan_key_zai` / `coding_plan_key_bigmodel`（进 Auth.Extra）
//     时才可能走订阅腿；**两者都没有 → 无条件 start-plan**（协议明确：凭据缺失时保守回退
//     start-plan 而不抛错，因为那是唯一已验证可用的一条腿）。
//  3. 模型归属按**通道白名单**判定（协议 §1.6 第 3 层）：`glm-5.3` 只属订阅腿白名单，
//     故「两条腿凭据都在」时它会走订阅腿；`glm-5.3-flash` 两条腿都有，按规则 1 走 start-plan。
//  4. ⚠ **积分制无论账号是 bigmodel 还是 zai 都发 `zcode.z.ai`**，不要因为账号是
//     国际版就改域名（官方分派表规则 2/5，2026-10-03 实测复核）。
//  5. 订阅腿**不带 HTTP-Referer**：实测该网关只认 Authorization + anthropic-version。
//
// # 与其它渠道的三条关键差异
//
//  1. 请求体必须内嵌官方身份块（identity.go），否则 405 + 3012；
//  2. JWT **不可续期**（RefreshToken 空实现，见那里的注释）；
//  3. 积分领取**不实现**（DailyCheckin 返回明确的「不支持」错误，见那里与包注释）。
package zcode

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// ErrClaimUnsupported 本渠道**不支持自动领取额度**。
//
// 原因（协议 §1.2.5 / §1.9，逐条落实）：`POST /api/v1/zcode-plan/billing/claim`
// 在参考实现里依赖**阿里云验证码网页 SDK**（`AliyunCaptcha.js`，必须在真实 origin
// `https://zcode.z.ai/` 的 **headful chromium**（CDP 驱动）里执行无感验证）、
// 且**每个 plan 都必须新 mint 一个一次性 captcha param**（复用必得 `3007`）。
// 这在 Go 侧无法纯静态移植 ⇒ 本包**不做**这条路径，也**绝不伪造成功**。
//
// 本渠道因此按「无显式签到」渠道对待（接线时进 noExplicitCheckin 名单）。
var ErrClaimUnsupported = errors.New(
	"zcode：本渠道不支持自动领取额度（billing/claim 依赖阿里云验证码网页 SDK + headful 浏览器，" +
		"且每个 plan 需新 mint 一次性 captcha param，无法纯静态移植）；请到官方 ZCode 客户端领取")

// Client 上游 HTTP 客户端。Base / CodingBase / Account 可覆盖以便测试。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 专供对话流式请求：**无总超时**（R35）。
	//
	// 为什么必须分开：http.Client.Timeout 是整请求上限（计时器在 Do() 返回后继续跑，
	// 直到 body 读完），而 SSE 整个生成期都在读 body —— 长思考请求会被从流中间掐断，
	// 客户端表现为「突然无响应」。空闲兜底改由 provider.IdleReader 承担。
	StreamHTTP *http.Client
	// IdleTimeout 流式空闲超时：连续该时长读不到任何字节即判上游卡死。
	// 0 = 用 idleTimeout()（默认 600s，含 DSH_ZCODE_SSE_IDLE_TIMEOUT_MS 覆盖）。
	IdleTimeout time.Duration

	// Base 平台 origin（积分制对话 / 额度 / 目录 / 登录）。
	Base string
	// CodingBase 订阅制网关 origin（只有 coding-plan 腿用它）。
	CodingBase string

	// catMu 保护 dynamic（最近一次成功拉取的真实远端目录）。
	// 兜底表不缓存：一次瞬时失败不得污染整个进程生命周期。
	catMu   sync.RWMutex
	dynamic []catalogEntry
}

// 编译期锁定接线契约：Client 必须实现 provider.Upstream 的全部 10 个方法
// （Lead 的装配依赖它；缺一个就是编译期错误，而不是运行期静默少一项能力）。
var _ provider.Upstream = (*Client)(nil)

// New 生产默认客户端。
func New() *Client {
	tr := newTransport()
	return &Client{
		HTTP:       &http.Client{Timeout: requestTimeout, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr}, // 共用 Transport，不设 Timeout
		Base:       Origin,
		CodingBase: CodingPlanOrigin,
	}
}

// NewWithBase 测试用：把**两个** origin 都指向 base。
//
// 为什么两个都改：测试必须保证「零真实网络」——只要有一个 origin 还指着生产域名，
// 某条分支（例如订阅腿）就可能在测试里真的打上游（AGENTS.md §0.1 明令禁止）。
// 需要分别覆盖时，测试自己设 c.Base / c.CodingBase。
func NewWithBase(base string) *Client {
	c := New()
	c.Base = base
	c.CodingBase = base
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

// origin 返回平台 origin 基址。
func (c *Client) origin() string {
	if s := strings.TrimRight(strings.TrimSpace(c.Base), "/"); s != "" {
		return s
	}
	return Origin
}

// codingOrigin 返回订阅制网关基址。
func (c *Client) codingOrigin() string {
	if s := strings.TrimRight(strings.TrimSpace(c.CodingBase), "/"); s != "" {
		return s
	}
	return CodingPlanOrigin
}

// streamClient 返回流式调用用的客户端（无总超时，回落新建保证 nil 安全）。
func (c *Client) streamClient() *http.Client {
	if c.StreamHTTP == nil {
		c.StreamHTTP = &http.Client{Transport: newTransport()}
	}
	return c.StreamHTTP
}

// businessClient 返回业务端点用的客户端（有总超时）。
func (c *Client) businessClient() *http.Client {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: requestTimeout, Transport: newTransport()}
	}
	return c.HTTP
}

// effectiveIdleTimeout 生效的空闲超时（未注入时用包级默认/环境变量）。
func (c *Client) effectiveIdleTimeout() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return idleTimeout()
}

// ---------------------------------------------------------------------------
// 通道选路
// ---------------------------------------------------------------------------

// channel 本渠道的两条上游腿。
type channel string

const (
	// channelStartPlan 积分制（免费额度）：端点 zcode.z.ai，凭据 zcode_jwt。
	channelStartPlan channel = "start-plan"
	// channelCodingPlan 订阅制：端点 api.z.ai，凭据 coding_plan_key_*。
	channelCodingPlan channel = "coding-plan"
)

// channelModelWhitelist 两条通道各自承载的模型（**小写** model id；协议 §1.6 第 3 层）。
//
// ⚠ 这是**官方声明的白名单**，不是「上游目录里有哪些模型」：远端
// `GET api.z.ai/api/anthropic/v1/models` 实测有 11 个模型，但官方对订阅腿只认白名单里的
// 两个；其余 9 个订阅是否覆盖**未验证**，故不暴露（协议 §1.6 第 3 层的原话）。
var channelModelWhitelist = map[channel][]string{
	channelStartPlan:  {"glm-5.3-flash", "glm-5.2", "glm-5-turbo"},
	channelCodingPlan: {"glm-5.3", "glm-5.3-flash"},
}

// resolveChannel 选腿：**模型归属 + 凭据可用性**（协议 §1.1 的 `resolveChannelFor` 逐条复刻）。
//
// 顺序（照抄参考实现，改动前先读协议 §1.1/§1.6）：
//  1. 模型属于 start-plan 且凭据有 `zcode_jwt` → start-plan；
//  2. 模型属于 coding-plan 且凭据有 `coding_plan_key_*` → coding-plan；
//  3. 其余（模型不在任何白名单、或对应凭据缺失）→ **start-plan**（保守回退：
//     那是唯一已验证可用的一条腿，参考实现明确「凭据缺失时保守回退而不抛错」）。
//
// ⚠ **start-plan 优先**：积分是白给的，不该被订阅额度先消耗掉；两条腿都承载同一模型时
// （`glm-5.3-flash`），规则 1 先命中。
// ⚠ **积分制无论账号是 bigmodel 还是 zai 都发 `zcode.z.ai`**，不要因为账号是国际版就改域名。
func resolveChannel(a *auth.Auth, model string) channel {
	key := strings.ToLower(strings.TrimSpace(model))
	jwtOK := a != nil && strings.TrimSpace(a.AccessTokenValue()) != ""
	codingOK := codingPlanKey(a) != ""
	if jwtOK && contains(channelModelWhitelist[channelStartPlan], key) {
		return channelStartPlan
	}
	if codingOK && contains(channelModelWhitelist[channelCodingPlan], key) {
		return channelCodingPlan
	}
	return channelStartPlan
}

// codingPlanKey 取订阅腿的 api-key（zai 优先，其次 bigmodel；协议 §1.2.3）。
func codingPlanKey(a *auth.Auth) string {
	if a == nil || a.Extra == nil {
		return ""
	}
	if v := strings.TrimSpace(a.Extra["coding_plan_key_zai"]); v != "" {
		return v
	}
	return strings.TrimSpace(a.Extra["coding_plan_key_bigmodel"])
}

// messagesURL 返回该腿的对话端点。
func (c *Client) messagesURL(ch channel) string {
	if ch == channelCodingPlan {
		return c.codingOrigin() + "/api/anthropic/v1/messages"
	}
	return c.origin() + EpZCodePlanMessages
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
// 两类**本地**失败（都不打上游）：
//   - 出站体不可构造（请求 JSON 非法 / 消息为空 / 图片读不到 / 模型不支持图片）
//     → 本地 400 + code `bad_params`（请求级：不罚号、不轮换、原文透传）；
//   - **身份块前置校验失败** → 本地 405 + code 3012 语义的错误体（见身份块注释：
//     上游同条件会回 405 + 3012 并给账号记风控冷却，故宁可本地拦下）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	model := upstreamModelName(modelOf(body))
	entry, known := c.catalogEntry(model)
	out, berr := buildMessagesBody(body, entry, known)
	if berr != nil {
		return nil, http.StatusBadRequest, badParamsBody(berr), nil
	}
	// ★ 本地准入门槛：确认即将发出的请求体真的内嵌了官方身份块。
	if err := verifyOutboundIdentity(out); err != nil {
		return nil, http.StatusMethodNotAllowed, identityGateBody(err), nil
	}

	ch := resolveChannel(a, model)
	authorization := "Bearer " + tokenOf(a)
	if ch == channelCodingPlan {
		authorization = "Bearer " + codingPlanKey(a)
	}
	req, err := http.NewRequest(http.MethodPost, c.messagesURL(ch), bytes.NewReader(out))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header = zcodeHeaders(a, authorization, true)
	if ch == channelCodingPlan {
		// 订阅腿不带 HTTP-Referer（协议 §1.2.3 实测：api.z.ai 只认
		// Authorization + anthropic-version）。
		req.Header.Del(HdrReferer)
	}
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
	return provider.NewIdleReader(resp.Body, c.effectiveIdleTimeout()), resp.StatusCode, nil, nil
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

// RefreshToken 实现 provider.Upstream：本渠道**不可续期**，故是**空实现**。
//
// 依据（协议 §1.8）：`zcode_jwt` 的 JWT payload **没有 `exp`**，参考实现里
// `refresh_token` 恒为 undefined、`ZCODE_REFRESHABLE = false`，上游也没有可用的
// 令牌续期端点（`POST /api/v1/oauth/token` 自 2026-09-28 起稳定 500 / code 2007，
// 登录流程刻意绕开它）。
//
// 为什么返回 nil 而不是报错：
//   - 调用方（app.refreshIfSessionDead / 定时续期）把非 nil 当「续期失败」，会对凭据
//     反复重试并可能记冷却；而「真失效」的唯一可信判据是上游的 401 / code 1002
//     （见 Classify），不该由本地凭空断言。
//
// ⚠ 配套硬约束：`ExpiresAt` 必须是**远期值**（登录包写入），否则 `NeedsRefresh` 恒真
// → 每一轮调度都会调到这里空转，账号还会被反复标记刷新。
func (c *Client) RefreshToken(_ *auth.Auth) error { return nil }

// ---------------------------------------------------------------------------
// provider.Upstream：额度
// ---------------------------------------------------------------------------

// balanceBucket 一条额度桶（`data.balances[]`）。
type balanceBucket struct {
	PlanID   string
	ShowName string
	// UnitType 计量单位：实测为 `"token"`（**不是积分**）。
	// 面板若按「积分」展示会误导用户（参考实现记过一次真实缺陷：1 亿 tokens 被显示成
	// `94539275` 这种看起来像积分的数字）。
	UnitType string
	Meter    string
	Total    int64
	Used     int64
	Remain   int64
	// ExpireAt Unix 秒（0 = 上游未下发）。
	ExpireAt int64
}

// balanceResult 一次余额查询的结果。
type balanceResult struct {
	Buckets []balanceBucket
	// Enterprise 企业版：上游**不下发额度数字**（协议 §1.7.1），此时不得显示成 0
	// （0 是「已用光」的语义）。
	Enterprise bool
}

// UserResource 查询「可消耗积分」。
//
// ⚠️ **本渠道恒返回 0**（2026-10-17 修正，用户实测报告）：ZCode 的 `billing/balance` 下发的是
// **token 桶**（`unit_type="token"`），**不是积分**。早期实现把 Σ token remaining 当可消耗余额返回，
// 后果是 token 数（动辄上亿）被 pool 当作账号余额排序、被面板当积分展示，并进入 ledger/stats 的
// 积分口径 —— 严重污染统计。
//
// token 桶仍然要展示，但只走 `UserResourceDetail` 的 **InfoOnly** 条目：
// 那条路径明确不进 Summarize 小计、不进临期统计、不参与 ledger 差分（与 gemini 的配额窗口同一口径）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	res, err := c.fetchBalance(a)
	if err != nil {
		return 0, err
	}
	// 返回 Σ token remaining —— ⚠️ **这是 token 口径的额度余额，不是积分**。
	// 由渠道级 credits_unit="token" 标注，前端据此用「Token 计数」面板展示；
	// 它不进积分小计（桶条目标了 InfoOnly，Summarize 不统计）、不进积分流水
	// （app.creditSnapshot 按 unit 分流，跳过 ledger 的积分差分）。
	// 同渠道内 pool 按它排序是有意义的（token 余额多的先用）；跨渠道不比较（pool 是 per-channel）。
	var sum int64
	for _, b := range res.Buckets {
		sum += b.Remain
	}
	return sum, nil
}

// UserResourceDetail 返回余额与明细条目。
//
// 条目来源两处（口径同源，避免两处各算一遍）：
//   - `billing/balance` 的每个 token 桶 → 一条 **InfoOnly** 明细（token 不是积分，只作展示）；
//   - `billing/preview` 的每个可领 plan → 一条 **InfoOnly** 条目（只作展示，见下）。
//
// 为什么可领 plan 标 InfoOnly：本渠道**不能自动领取**（见 ErrClaimUnsupported），
// 这些额度对「本工具的额度算术」没有意义——它们既不是余额，也不该被 Summarize 小计、
// 不该参与临期判定与 ledger 差分。展示它们的唯一目的，是让用户看到「官方客户端里
// 今日其实有可领的 1 亿 token」，而不是以为账号真的没额度了（协议 §1.7.1 的实测：
// 同账号 `balances=[]` 而 preview 给出 `…start-plan-trust-1003`）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	res, err := c.fetchBalance(a)
	if err != nil {
		return 0, nil, err
	}
	// 标量余额 = Σ token remaining（**token 口径，不是积分**）；单位由渠道级
	// credits_unit="token" 标注，前端用「Token 计数」展示，且不进积分小计/流水。
	var remain int64
	for _, b := range res.Buckets {
		remain += b.Remain
	}

	if res.Enterprise {
		// 企业版不下发额度数字：用一条**只作展示**的条目承载这个事实，
		// 而不是让面板显示「0」（0 = 已用光，与实际语义相反）。
		return remain, []provider.ResourceItem{{
			Name:   "企业版：上游不下发额度数字（请在企业控制台查看）",
			Usable: true, InfoOnly: true, Key: "enterprise",
		}}, nil
	}

	items := make([]provider.ResourceItem, 0, len(res.Buckets)+2)
	for _, b := range res.Buckets {
		name := b.ShowName
		if name == "" {
			name = b.PlanID
		}
		if name == "" {
			name = "ZCode 额度"
		}
		if strings.EqualFold(b.UnitType, "token") {
			name += "（tokens）"
		}
		items = append(items, provider.ResourceItem{
			Name:     name,
			Total:    b.Total,
			Used:     b.Used,
			Remain:   b.Remain,
			ExpireAt: balanceExpireAt(b.ExpireAt),
			Key:      b.PlanID,
			// Usable=true：它是账号真实可用的额度；
			// ⚠️ InfoOnly=true：它**不是积分**（计量单位是 token），不得进 Summarize 小计、
			// 不得参与临期判定与 ledger 差分 —— 否则上亿 token 会污染积分口径
			// （2026-10-17 修正，与 gemini 的配额窗口同一口径）。
			// Unit="token"：本条目走「Token 计数」展示（前端按 credits_unit 渲染）。
			Unit:     "token",
			Usable:   true,
			InfoOnly: true,
		})
	}
	// preview：失败一律降级为空（余额是主体，preview 是补充）。
	for _, p := range c.previewPlans(a) {
		name := "可领额度（需官方客户端领取）"
		if p.Name != "" {
			name += "：" + p.Name
		}
		items = append(items, provider.ResourceItem{
			Name:   name,
			Total:  p.GrantUnits,
			Remain: p.GrantUnits,
			Key:    p.PlanID,
			// InfoOnly：只作展示，不进任何积分算术（见方法注释）。
			// Unit="token"：可领额度同样是 token 计量。
			Unit:     "token",
			Usable:   true,
			InfoOnly: true,
		})
	}
	return remain, items, nil
}

// balanceExpireAt 把 Unix 秒转成 provider.ResourceItem.ExpireAt 的 **RFC3339 精确时刻**
// （UTC+8 墙钟）；上游未下发时返回空串——前端据此隐藏「有效期」列，
// 不得用零值时间冒充「永不过期」（AGENTS.md 不变量 18）。
//
// 历史：旧实现返回 "YYYY-MM-DD"，丢了时分秒。面板按「N 小时内到期」分桶需要精确时刻
// （2026-10-19 实测）。注：zcode 的 ledger 伪键用 PlanID（不含日期），故零抖动。
func balanceExpireAt(sec int64) string {
	if sec <= 0 {
		return ""
	}
	return time.Unix(sec, 0).In(softRateResetLoc).Format(time.RFC3339)
}

// softRateResetLoc 本渠道的墙钟口径（UTC+8），与全仓其它渠道一致
// （用 time.Local 会在非 UTC+8 机器上算错一天）。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// fetchBalance 查余额（**需要 Authorization + X-Device-Mid**；协议 §1.7.1）。
func (c *Client) fetchBalance(a *auth.Auth) (balanceResult, error) {
	tok := tokenOf(a)
	if tok == "" {
		return balanceResult{}, &provider.Error{Kind: provider.ErrSessionDead, Msg: "zcode：账号没有 zcode_jwt"}
	}
	status, raw, err := c.getJSON(c.origin()+EpBalance, zcodeHeaders(a, "Bearer "+tok, false))
	if err != nil {
		return balanceResult{}, err
	}
	if status >= 400 {
		return balanceResult{}, classifyErr(status, raw)
	}
	top, perr := decodeObject(raw)
	if perr != nil {
		return balanceResult{}, fmt.Errorf("zcode：余额响应解析失败：%w", perr)
	}
	if code, msg, ok := businessCode(top); ok && code != 0 {
		return balanceResult{}, &provider.Error{Kind: Classify(status, string(raw)), Status: status,
			Msg: fmt.Sprintf("zcode：余额接口业务码 %d %s", code, msg)}
	}
	data, derr := decodeObject(top["data"])
	if derr != nil {
		return balanceResult{}, fmt.Errorf("zcode：余额响应缺 data 对象：%w", derr)
	}
	res := balanceResult{}
	if strings.EqualFold(strings.TrimSpace(trimJSONString(data["displayMode"])), "enterprise") {
		res.Enterprise = true
		return res, nil
	}
	for _, item := range objectArray(data["balances"]) {
		b := balanceBucket{
			PlanID:   trimJSONString(item["plan_id"]),
			ShowName: trimJSONString(item["show_name"]),
			UnitType: trimJSONString(item["unit_type"]),
			Meter:    trimJSONString(item["meter"]),
			Total:    amountOf(item["total_units"]),
			Used:     amountOf(item["used_units"]),
			ExpireAt: amountOf(item["expires_at"]),
		}
		// 汇总口径：优先 available_units（若给了），否则 remaining_units（协议 §1.7.1）。
		if raw, has := item["available_units"]; has {
			b.Remain = amountOf(raw)
		} else {
			b.Remain = amountOf(item["remaining_units"])
		}
		if b.PlanID == "" && b.ShowName == "" && b.Total == 0 && b.Remain == 0 {
			continue // 全空的桶：丢掉，不产出无意义的明细行
		}
		res.Buckets = append(res.Buckets, b)
	}
	return res, nil
}

// claimablePlan 一条可领活动（`data.plans[]`）。
type claimablePlan struct {
	PlanID     string
	Name       string
	Priority   int64
	GrantUnits int64
	UnitType   string
}

// previewPlans 查「可领活动」摘要（**只读**，不消耗额度、不领取）。
//
// ⚠ 已知限制（**如实标注，不掩盖**）：协议 §1.7.2 明确「必须补两条活跃事件
// （`app_launch` / `app_daily_active`，走 `POST /api/v1/event/report`），否则 preview
// 恒为空 `plans:[]`」。本包**刻意不发**那两条上报，因为：
//   - 它们只服务于领取路径（补激活 → 计划出现 → claim），而领取在本渠道不可实现
//     （见 ErrClaimUnsupported）；
//   - 领取既然不可用，「为让 preview 非空而上报活跃」就是**零收益的上游写入**；
//   - 本仓纪律：不做薅羊毛式自动化（AGENTS.md §0.2 / R30/R32）。
//
// ⇒ 因此本函数的返回**通常为空**；只有当服务端已经因官方客户端/别处的活动填充了
// plans 时才有内容。查询失败一律降级为空数组，绝不影响余额本身。
//
// 端点鉴权：`preview` **不需要 Authorization**，但**需要 X-Device-Mid**
// （协议 §1.2.2 的逐端点鉴权表）。
func (c *Client) previewPlans(a *auth.Auth) []claimablePlan {
	url := c.origin() + EpPreview + "?app_version=" + appVersionOf(a) + "&platform=" + Platform
	status, raw, err := c.getJSON(url, zcodeHeaders(a, "", false))
	if err != nil || status >= 400 {
		return nil
	}
	top, perr := decodeObject(raw)
	if perr != nil {
		return nil
	}
	data, derr := decodeObject(top["data"])
	if derr != nil {
		return nil
	}
	var out []claimablePlan
	for _, item := range objectArray(data["plans"]) {
		p := claimablePlan{
			PlanID:   trimJSONString(item["plan_id"]),
			Name:     trimJSONString(item["name"]),
			Priority: amountOf(item["priority"]),
		}
		if p.PlanID == "" {
			continue
		}
		// ⚠ 额度只在 `entitlements[0]` 里，plan 顶层**没有**额度字段（协议 §1.7.2 实测）。
		if ents := objectArray(item["entitlements"]); len(ents) > 0 {
			p.GrantUnits = amountOf(ents[0]["grant_units"])
			p.UnitType = trimJSONString(ents[0]["unit_type"])
		}
		out = append(out, p)
	}
	// priority 降序（先领高优先级；本包不领取，但保持与官方一致的顺序便于展示）。
	sortByPriorityDesc(out)
	return out
}

// sortByPriorityDesc 按 priority 降序稳定排序（插入排序：条目数极少，且要稳定）。
func sortByPriorityDesc(xs []claimablePlan) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j].Priority > xs[j-1].Priority; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}

// objectArray 把 JSON 原始值解成「对象数组」（非数组/坏元素直接跳过）。
func objectArray(raw json.RawMessage) []map[string]json.RawMessage {
	if len(raw) == 0 {
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

// ---------------------------------------------------------------------------
// provider.Upstream：签到（**本渠道不支持**）
// ---------------------------------------------------------------------------

// DailyCheckin 实现 provider.Upstream：本渠道**不支持自动领取额度**。
//
// 它**不是**「今日已领取」的幂等成功，也**不是**「暂时失败可重试」——而是一条明确的
// 「本渠道无此能力」结论，故返回 ErrClaimUnsupported 供上层（与日志）如实透出。
//
// ⚠ 为什么不返回 nil：返回 nil 会被调度器记成「签到成功」，面板会显示一个**假成功**
// （用户以为额度拿到了，实际一分没领）。本仓纪律明确禁止伪造成功。
// ⚠ 为什么不实现 provider.CheckinReporter：调度器一旦发现渠道支持结构化上报，就会在
// 当日窗口内反复重试；本渠道无论重试多少次都不可能成功，重试只会制造无谓的上游请求。
func (c *Client) DailyCheckin(_ *auth.Auth) error { return ErrClaimUnsupported }

// ---------------------------------------------------------------------------
// provider.Upstream：错误分类
// ---------------------------------------------------------------------------

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Classify 按状态码 + **业务码** 分类（ErrKind 驱动 pool 的冷却状态机）。
//
// 本渠道的失败形态是两层的：同一个 HTTP 状态里可能夹着完全不同的业务码，
// 所以**必须先读业务码**（协议 §1.7.4 的错误码→处置表）：
//
//	| 码 / 状态        | 语义                       | 归类                                   |
//	|------------------|----------------------------|----------------------------------------|
//	| 3009（429）      | 并发限流（并发配额）       | ErrSoftRate（换号/退避有价值）          |
//	| 1005（429）      | 额度用尽                   | ErrHardCredit（不重试，换号）           |
//	| 1113（429）/余额不足 | 订阅侧余额不足          | ErrHardCredit                          |
//	| 3007             | captcha 校验失败           | ErrSoftRate（本仓无法 mint，见下）      |
//	| 3012             | 风控「unusual activity」   | ErrClient（账号级、换号有效）           |
//	| 1002 / 401       | 凭据失效                   | ErrSessionDead（提示重新登录）          |
//
// 三个必须守住的顺序：
//   - **本地 `bad_params` 标记最优先**：本包自己产的出站体错误（含身份块前置校验的
//     405 体）必须以「请求级、不罚号、不轮转」结束，否则一个本地 bug 会去冷却健康账号；
//   - **3009 先于「余额不足」文本**：限流与欠费是两种完全不同的处置，而限流 body 里
//     可能出现 quota 字样（AGENTS.md 不变量 15 的同款陷阱）；
//   - **显式业务码先于状态码**：否则 429+1005（欠费）会被当成普通限流。
//
// ⚠ 3007（captcha 索要）：参考实现的做法是「换新 param 重发」，而本仓**无法 mint
// captcha**（见 ErrClaimUnsupported 的原因）。故这里归 ErrSoftRate 让上层换号——
// 依据是参考实现把「需要 captcha」按**账号 × 模型**记忆（说明索要是账号维度的），
// 换号可能绕开；同时错误文案里如实写明「本仓无法产出验证头」。
// ⚠ 3012：参考实现把它标成 PERMISSION + **不可重试**（且有账号冷却惩罚：30 分钟起步，
// 24h 内第 3 次起 24h，5 次停用）。本仓把它归 ErrClient（可轮换 + 计入 errCount）：
// 「不重试」指的是**不重试同一个账号**，而换号正是本仓表达这一点的机制——否则同一个被
// 风控标记的账号会被反复使用，最终走向停用。
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)
	switch {
	case provider.CodeMarker(lower, "bad_params"):
		return provider.ErrBadParams
	case provider.CodeMarker(lower, "3012") || strings.Contains(lower, "unusual activity"):
		return provider.ErrClient
	case provider.CodeMarker(lower, "3009"):
		return provider.ErrSoftRate
	case provider.CodeMarker(lower, "1005") || provider.CodeMarker(lower, "1113"),
		strings.Contains(lower, "余额不足") || strings.Contains(lower, "insufficient balance"):
		return provider.ErrHardCredit
	case provider.CodeMarker(lower, "3007"):
		return provider.ErrSoftRate
	case provider.CodeMarker(lower, "1002"):
		return provider.ErrSessionDead
	}
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
		// 其余 4xx：按语义归 ErrClient（可能挂在账号权益上，允许换号一次）。
		return provider.ErrClient
	default:
		return provider.ErrNone
	}
}

// classifyErr 把 HTTP 层失败转成类型化的 *provider.Error。
//
// 类型化很关键：401/403 必须是 *provider.Error{Kind: ErrSessionDead}，否则调度器的
// `errors.As` 判定失效、自愈路径（refresh + 落盘 + 重试一次）不会触发（不变式 19）。
func classifyErr(status int, raw []byte) error {
	return &provider.Error{Kind: Classify(status, string(raw)), Status: status, Msg: snippet(raw)}
}

// ---------------------------------------------------------------------------
// HTTP 小工具
// ---------------------------------------------------------------------------

// getJSON 发一个 GET（请求头由调用方按端点要求装配——本渠道各端点的鉴权要求不同，
// 不能用一个固定的头构造函数一刀切）。
func (c *Client) getJSON(url string, h http.Header) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header = h
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.businessClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw, nil
}

// tokenOf 取账号的 zcode_jwt 快照（锁内读，防与写回竞争）。
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
		"message": "zcode：" + err.Error(),
		"type":    "invalid_request_error",
		"code":    "bad_params",
	}})
	return b
}

// identityGateBody 身份块前置校验失败时的错误体。
//
// 为什么用 **405**：与上游同条件的形态对齐（上游回 HTTP 405 + `{"code":3012}`），
// 便于排障时一眼认出「这是身份块问题」。
//
// 为什么 code 用 `bad_params` 而不是 `3012`：这次失败**发生在本机**（请求根本没发出去），
// 它是请求构造层的缺陷，不是账号被风控。若写成 3012，上层会把它当账号级风控去换号/冷却，
// 等于用本地 bug 惩罚健康账号。故如实标注：本地归类 bad_params（请求级），
// 并在文案里说明上游同条件会回 405 + 3012。
func identityGateBody(err error) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": "zcode：出站请求体未通过身份块前置校验，已本地拦截（上游同条件会回 HTTP 405 + " +
			"code 3012，且有账号冷却惩罚）：" + err.Error(),
		"type": "invalid_request_error",
		"code": "bad_params",
	}})
	return b
}
