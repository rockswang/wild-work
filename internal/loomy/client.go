package loomy

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/reasoning"
)

// Client 讯飞 Loomy 上游客户端。
// 与既有渠道不同，本渠道不做登录编排（凭据由「从本机客户端导入」得到），
// 且**无 refresh 端点**：session 到期需重新导入。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 专供对话流式请求：**无总超时**。
	//
	// 为什么必须分开：http.Client.Timeout 是整请求上限（计时器在 Do() 返回后继续跑，
	// 直到 body 读完），而 SSE 整个生成期都在读 body —— 长思考请求会被从流中间掐断。
	// 2026-09-24 实测两次中断都恰好 120.00s（= config.upstream.timeout_seconds），
	// 客户端表现为「突然无响应」。空闲兜底改由 IdleReader 承担。
	StreamHTTP *http.Client
	// IdleTimeout 流式空闲超时：连续该时长读不到任何字节即判上游卡死。
	// 0 = 用 DefaultIdleTimeout。⚠️ 目前**没有配置项**（main 不注入），实际恒为默认值。
	IdleTimeout time.Duration

	// liveMu 保护 liveIDs（动态目录缓存）——与 raccoon 同款缺陷的修复：
	// ChatStream 本地校验模型名时若**只查静态表**，上游新增的模型会被误拒，
	// 而它正被 /v1/models 正常透出（后者读动态目录）⇒「面板列出却调不动」。
	// 用户需要它是因为：静态表基于 2026-09 实测，上游随时新增/下线模型。
	liveMu  sync.RWMutex
	liveIDs map[string]bool

	// Base 网关基址（默认 GatewayBase）；测试注入 httptest 假上游用，生产不设。
	Base string
}

// baseURL 生效的网关基址。
func (c *Client) baseURL() string {
	if b := strings.TrimRight(strings.TrimSpace(c.Base), "/"); b != "" {
		return b
	}
	return GatewayBase
}

// DefaultIdleTimeout 流式空闲超时默认值。
//
// 实测正常请求 8~13s 完成（同一模型），故 90s 足够宽松——只拦「真的一个字节都不出」。
const DefaultIdleTimeout = 90 * time.Second

// New 默认 180s 超时（仅作用于非流式调用；流式见 StreamHTTP）。
func New() *Client { return NewWithTimeout(180 * time.Second) }

// NewWithTimeout 指定非流式超时。
func NewWithTimeout(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	return &Client{
		HTTP:       &http.Client{Timeout: timeout, Transport: newTransport()},
		StreamHTTP: &http.Client{Transport: newTransport()},
	}
}

// newTransport 出厂 Transport：禁 h2 + Dial/keepalive + TLS 握手 + ResponseHeaderTimeout。
//
// ⚠️ 此前本包 `&http.Client{Timeout: ...}` 未设 Transport，实际在共用 http.DefaultTransport
// （h2 开启、无 ResponseHeaderTimeout）。StreamHTTP 无总超时后**必须**有
// ResponseHeaderTimeout 兜底，否则连响应头都等不到就会无限挂住。
func newTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper), // 强制 HTTP/1.1
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
}

// httpClient 返回非流式调用用的客户端（models/points/refresh 的 do()）。
// 仍有总超时：这些是短请求，总超时是恰当的兜底。
func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 180 * time.Second, Transport: newTransport()}
	}
	return c.HTTP
}

// streamClient 返回流式调用用的客户端（无总超时，回落 HTTP 保证 nil 安全）。
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

// sessionOf 读取凭据里的 session（nil 安全）。
//
// nil Auth 必须安全：签到/余额这类路径可能在「账号刚被移除」时被调度器调用，
// 旧写法 `a.AccessTokenValue()` 会直接空指针 panic（2026-10-17 由 checkin 测试抓到）。
func sessionOf(a *auth.Auth) string {
	if a == nil {
		return ""
	}
	return strings.TrimSpace(a.AccessTokenValue())
}

func session(a *auth.Auth) string { return sessionOf(a) }

// traceparent 生成 W3C traceparent；官方源码注释实测：**缺这个头 chat/completions 会挂死到超时**。
func traceparent() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00-" + strings.Repeat("0", 32) + "-" + strings.Repeat("0", 16) + "-01"
	}
	return "00-" + hex.EncodeToString(b[:16]) + "-" + hex.EncodeToString(b[16:]) + "-01"
}

// applyAuth 写鉴权与追踪头（官方客户端 session 模式是 token 头 + Authorization **双写**）。
func applyAuth(req *http.Request, a *auth.Auth) {
	tok := session(a)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("token", tok)
	req.Header.Set("traceparent", traceparent())
	req.Header.Set("loomy-version", AppVersion)
	req.Header.Set("Accept", "application/json")
}

// do 发一次带鉴权的非流式请求。
func (c *Client) do(method, url string, a *auth.Auth, body []byte) (*http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	applyAuth(req, a)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp, raw, rerr
}

// ---------------------------------------------------------------------------
// 凭据
// ---------------------------------------------------------------------------

// RefreshToken 实现 provider.Upstream。
// 上游**没有 refresh 端点**（客户端登录时请求 14 天有效期的 session，见 docs 备忘）：
// `utils/remote-keepalive.js` 经查是电源保活，与登录态无关。因此这里返回明确错误，
// 让调用方走 session_dead 分支提示用户重新导入，而不是发出一个必然失败的请求。
func (c *Client) RefreshToken(a *auth.Auth) error {
	return &provider.Error{
		Kind: provider.ErrSessionDead, Status: 0,
		Msg: "loomy: 上游无 refresh 端点（session 有效期 14 天），请在面板重新「从本机客户端导入」",
	}
}

// ---------------------------------------------------------------------------
// 推理
// ---------------------------------------------------------------------------

// ChatStream 实现 provider.Upstream。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	// 校验集合 = 静态表 ∪ 已拉取到的动态目录（见 knownModel）。
	if m := modelOf(body); m != "" && !c.knownModel(m) {
		return nil, http.StatusBadRequest, []byte(unknownModelBody(m)), nil
	}
	projected := projectEffort(body)
	logReasoningOnce(body, projected)

	req, err := http.NewRequest(http.MethodPost, c.baseURL()+EpChat, bytes.NewReader(projected))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	applyAuth(req, a)
	// 用无总超时的 client：长思考请求不受整请求上限约束（见 Client.StreamHTTP）。
	resp, err := c.streamClient().Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	// 空闲看门狗兜底：连续 idleTimeout 读不到字节即判上游卡死并关掉 body。
	// 流式与 stream:false 的聚合路径共用这一层（聚合时由「总时长硬上限」
	// 变为「空闲上限」，长而持续输出的响应不再被误杀）。
	return provider.NewIdleReader(resp.Body, c.idleTimeout()), resp.StatusCode, nil, nil
}

// projectEffort 把顶层 `reasoning_effort` 投影成官方客户端的三件套方言，并按模型能力降级档位。
//
// 官方形态（桌面版 `llm-completion.js` 实测）：
//
//	{ reasoning_effort, enable_thinking, chat_template_kwargs: { enable_thinking } }
//
// 档位降级（与 Qoder 三渠道对齐）：上游 `/models` 为每个模型声明 `reasoning_efforts`
// （实测 8 个模型一致，均为 none/low/medium/high/xhigh，default=low）。
// 客户端可能发上游不认的档位（`max` / `ultra` / `minimal`）—— 上游虽然宽容不报错，
// 但会静默按默认档处理，让用户以为档位生效。故先 Clamp 到该模型 ladder 内的档位。
//
// ⚠️ 阶段 C 文档曾记「三件套让思考单调递减」——那是**单次采样**，2026-09-24 用同一模型
// 每档 3 次采样发现值域完全重叠（none 1102/1048/1465、high 1085/1526/1455），
// 该结论不成立。协议层投影正确不代表模型层会精细响应档位，故此处只保证「发出的档位合法」。
//
// 只在客户端确实表达了档位时才补字段（未表达时保持原样，避免替客户端做决定 —— 对齐 R19 的思路）。
func projectEffort(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	raw, ok := obj["reasoning_effort"]
	if !ok {
		return body
	}
	effort, _ := raw.(string)
	effort = strings.TrimSpace(effort)
	if effort == "" {
		return body
	}
	model, _ := obj["model"].(string)
	if clamped := reasoning.Caps.Clamp(reasoning.RealmLoomy, strings.TrimSpace(model), effort); clamped != "" {
		effort = clamped
	}
	on := effort != "none"
	obj["reasoning_effort"] = effort
	obj["enable_thinking"] = on
	if _, exists := obj["chat_template_kwargs"]; !exists {
		obj["chat_template_kwargs"] = map[string]any{"enable_thinking": on}
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// logReasoningOnce 记一行档位日志（对齐 qoder 三渠道的排障约定）。
//
// 动机：Loomy 的档位此前完全静默 —— 实测时无法从 app.log 判断
// 「客户端表达了什么 / Clamp 后下发什么」，只能靠抓包。这里补上最终生效值。
func logReasoningOnce(orig, projected []byte) {
	var before, after struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(orig, &before); err != nil {
		return
	}
	if err := json.Unmarshal(projected, &after); err != nil {
		return
	}
	if before.ReasoningEffort == "" && after.ReasoningEffort == "" {
		return // 客户端未表达且投影不动：无信息量，不打日志
	}
	log.Printf("loomy reasoning: model=%q in=%q out=%q", before.Model, before.ReasoningEffort, after.ReasoningEffort)
}

// modelOf 取请求体里的 model。
func modelOf(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

// unknownModelBody 未知模型的本地 400（上游会静默回落默认模型，故本地拒绝）。
func unknownModelBody(m string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": fmt.Sprintf("loomy: 未知模型 %q。上游对未知模型会静默回落到默认模型，故本地直接拒绝；可用模型见 GET /v1/models", m),
		"type":    "invalid_request_error",
		"code":    "model_not_found",
	}})
	return string(b)
}

// ---------------------------------------------------------------------------
// 模型与费率
// ---------------------------------------------------------------------------

type loomyModel struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	ContextLength          int64    `json:"context_length"`
	MaxOutputTokens        int64    `json:"max_output_tokens"`
	ReasoningEfforts       []string `json:"reasoning_efforts"`
	DefaultReasoningEffort string   `json:"default_reasoning_effort"`
	Capabilities           struct {
		Reasoning       bool     `json:"reasoning"`
		Vision          bool     `json:"vision"`
		FunctionCalling bool     `json:"function_calling"`
		InputModalities []string `json:"input_modalities"`
	} `json:"capabilities"`
}

// modelsResp /models 信封：只保留被消费的字段。
type modelsResp struct {
	Data []loomyModel `json:"data"`
}

// fetchModels 拉取上游模型目录。
func (c *Client) fetchModels(a *auth.Auth) (*modelsResp, error) {
	resp, raw, err := c.do(http.MethodGet, c.baseURL()+EpModels, a, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &provider.Error{
			Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 300),
		}
	}
	var out modelsResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("loomy 模型目录解析失败: %w (%s)", err, truncate(string(raw), 200))
	}
	if len(out.Data) == 0 {
		return nil, fmt.Errorf("loomy 模型目录为空")
	}
	// 记下目录里的模型名，供 ChatStream 的本地校验取并集（同 raccoon）。
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		if id := strings.TrimSpace(m.ID); id != "" {
			ids = append(ids, id)
		}
	}
	c.rememberLiveModels(ids...)
	return &out, nil
}

// knownModel 模型名是否可接受（ChatStream 的本地校验入口）。
//
// 判据 = KnownModel（静态表） ∪ 已拉取到的动态目录。
// 动态部分**必须**参与：/v1/models 透出的是动态目录（FetchModels 的结果），
// 只认静态表会导致「面板列出却调不动」。
func (c *Client) knownModel(id string) bool {
	if KnownModel(id) {
		return true
	}
	c.liveMu.RLock()
	ok := c.liveIDs[id]
	c.liveMu.RUnlock()
	return ok
}

// rememberLiveModels 记下动态目录里的模型名（单调扩大，只增不减）。
func (c *Client) rememberLiveModels(ids ...string) {
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	if c.liveIDs == nil {
		c.liveIDs = make(map[string]bool, len(ids))
	}
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			c.liveIDs[id] = true
		}
	}
}

// FetchModels 实现 provider.Upstream（档位直接取上游 reasoning_efforts —— 权威值）。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	resp, err := c.fetchModels(a)
	if err != nil {
		return nil, err
	}
	out := make([]provider.ModelInfo, 0, len(resp.Data))
	for _, m := range resp.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		name := cleanName(m.Name)
		if name == "" {
			name = id
		}
		info := provider.ModelInfo{
			ID:                     id,
			Name:                   name,
			ContextWindow:          m.ContextLength,
			MaxTokens:              m.MaxOutputTokens,
			ContextFromAPI:         m.ContextLength > 0,
			SupportsImages:         m.Capabilities.Vision,
			SupportsReasoning:      m.Capabilities.Reasoning,
			SupportsTools:          m.Capabilities.FunctionCalling,
			SupportedEfforts:       append([]string{}, m.ReasoningEfforts...),
			ReasoningEfforts:       provider.CleanEfforts(m.ReasoningEfforts),
			DefaultEffort:          strings.TrimSpace(m.DefaultReasoningEffort),
			DefaultReasoningEffort: strings.TrimSpace(m.DefaultReasoningEffort),
			ReasoningCanDisable:    false, // 实测该系列"强制思考"，最低档仍会产生思考
		}
		out = append(out, info)
	}
	return out, nil
}

// FetchModelPricing 实现 provider.Upstream：倍率写在模型显示名里（`（x3.0）`），需解析。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	resp, err := c.fetchModels(a)
	if err != nil {
		return nil, err
	}
	out := make([]provider.ModelPricing, 0, len(resp.Data))
	for _, m := range resp.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		rate := priceFromName(m.Name)
		// ⚠️ 每轮必须取**新的**变量地址（2026-10-17 修复）：旧实现把 `explicit` 声明在循环外、
		// 每轮 `&explicit` 后又在末尾复位为 true ⇒ 所有条目共享同一个 bool 且末值恒 true，
		// 于是「解析不到倍率 → 面板显示未知而不是免费」（R5 约定）**静默失效**。
		// 现在按本条自己的解析结果取值：解析不到倍率即为 false。
		explicit := rate > 0
		out = append(out, provider.ModelPricing{
			Model:    id,
			Channel:  ChannelName,
			Rate:     rate,
			Explicit: &explicit,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 额度 / 签到
// ---------------------------------------------------------------------------

type pointsRecord struct {
	Balance *int64 `json:"balance"`
	Points  *int64 `json:"points"`
	Data    struct {
		Balance *int64 `json:"balance"`
		// DailyBalance 每日积分池余量（按 dailyCycleDate 每日循环，扣分优先消耗该池）。
		DailyBalance *int64 `json:"dailyBalance"`
		// AvailableBalance 上游给出的可用总额 = Balance + DailyBalance（**仅 v1 面下发**）。
		AvailableBalance *int64 `json:"availableBalance"`

		CurrentBalance *int64 `json:"currentBalance"`
		RemainPoints   *int64 `json:"remainPoints"`
		TotalPoints    *int64 `json:"totalPoints"`
	} `json:"data"`
}

// UserResource 实现 provider.Upstream：读积分记录接口的余额字段。
//
// 上游积分以「积分」为单位（推理响应里的 `usage.points_consumed` 即扣减量）。
// 端点在阶段 A 静态取证确认、字段名按宽松多候选解析；解析不到一律返回 0（宁缺勿错）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	remain, _, err := c.UserResourceDetail(a)
	return remain, err
}

// UserResourceDetail 实现 provider.Upstream。
//
// 余额口径 = 上游 `availableBalance`（常规池 + 每日池），**不能只取 `balance`** ——
// 2026-09-23 实测该账号 `balance=15000 / dailyBalance=4800 / availableBalance=19800`，
// 只读 balance 会把每日积分整块漏掉（面板少显示、pool 路由口径偏低）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	resp, raw, err := c.do(http.MethodGet, c.baseURL()+EpPointsV1, a, nil)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, nil, &provider.Error{
			Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 300),
		}
	}
	// 上游可能返回 HTTP 200 + 业务错误码（同 100002 的形态）。
	if k := businessErrKind(string(raw)); k != provider.ErrNone {
		return 0, nil, &provider.Error{Kind: k, Status: resp.StatusCode, Msg: truncate(string(raw), 300)}
	}
	var rec pointsRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return 0, nil, nil // 结构未知：不报错、不留余额（避免把账号误判为异常）
	}
	remain, items := parsePoints(rec)
	return remain, items, nil
}

// parsePoints 把上游积分响应拆成 (可消耗余额, 明细条目)。
//
// 上游把积分分成两个池：
//
//	balance       常规池（注册奖励 / 新手任务 / 充值，长期有效）
//	dailyBalance  每日池（每日循环发放，扣分优先消耗；ledger 里的 consumeSource=daily）
//
// 可用总额优先取上游的 `availableBalance`；该字段缺失（v2 面）时按两池相加兜底。
// 抽成纯函数便于单测（Client 的 baseURL 是常量，不便在测试里换 host）。
func parsePoints(rec pointsRecord) (int64, []provider.ResourceItem) {
	base := firstInt(rec.Data.Balance, rec.Balance, rec.Points,
		rec.Data.CurrentBalance, rec.Data.RemainPoints, rec.Data.TotalPoints)
	daily := rec.Data.DailyBalance
	if base == nil && daily == nil {
		return 0, nil
	}
	items := make([]provider.ResourceItem, 0, 2)
	if base != nil {
		items = append(items, provider.ResourceItem{Name: "积分", Total: *base, Remain: *base, Usable: true})
	}
	if daily != nil {
		items = append(items, provider.ResourceItem{Name: "每日积分", Total: *daily, Remain: *daily, Usable: true})
	}
	if v := rec.Data.AvailableBalance; v != nil {
		return *v, items
	}
	var sum int64
	if base != nil {
		sum += *base
	}
	if daily != nil {
		sum += *daily
	}
	return sum, items
}

func firstInt(vals ...*int64) *int64 {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// DailyCheckin 实现 provider.Upstream（**无业务日**的窄接口）。
//
// 本渠道的真实领取是 PET-WORK 领奖，请求体**必须**带服务端业务日 + 配置版本 + 档位
// （见 checkin.go）。本方法拿不到业务日，故**永远返回错误**，绝不假成功：
// 返回 nil 会让调度器记一次「已签到」，而实际一分钱没领到。
//
// 正确的入口是 DailyCheckinReport（带业务日），调度器在拿到快照后用业务日调用它。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return &provider.Error{
		Kind: provider.ErrClient,
		Msg: "loomy: 宠物打工领奖需要服务端业务日（/api/v1/pet-work 快照里的 businessDate），" +
			"不能走无参签到入口；请改用 DailyCheckinReport 或面板的「领取每日积分」",
	}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// businessErrKind 识别"HTTP 200 + 业务码"形态的失败（Loomy 的鉴权失效就是这样返回的）。
// 返回 ErrNone 表示不是已知业务错误。
func businessErrKind(body string) provider.ErrKind {
	lower := strings.ToLower(body)
	// 100002 登录态失效；020002 会话校验失败（源码 AUTH_ERROR_CODES）。
	// CodeMarker 的第一个参数要求是**已小写化**的 body。
	if provider.CodeMarker(lower, "100002") || provider.CodeMarker(lower, "020002") {
		return provider.ErrSessionDead
	}
	if strings.Contains(lower, "model_not_found") {
		return provider.ErrBadParams
	}
	return provider.ErrNone
}

// Classify 包级分类。
//
// ⚠️ 顺序关键：上游把**鉴权失效塞在 HTTP 200** 里（{"code":"100002"}），
// 所以业务码判定必须排在状态码判定之前（对齐 AGENTS §6.25）。
func Classify(status int, body string) provider.ErrKind {
	if k := businessErrKind(body); k != provider.ErrNone {
		return k
	}
	lower := strings.ToLower(body)

	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return provider.ErrSessionDead
	}
	if status == http.StatusPaymentRequired {
		return provider.ErrHardCredit
	}
	// 429 优先于 hardMarkers（§6.15）。
	if status == http.StatusTooManyRequests {
		return provider.ErrSoftRate
	}
	for _, m := range hardMarkers {
		if strings.Contains(lower, m) {
			return provider.ErrHardCredit
		}
	}
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests") || strings.Contains(lower, "请求过于频繁") {
		return provider.ErrSoftRate
	}
	if status == http.StatusNotFound {
		return provider.ErrNotFound
	}
	if status >= 500 {
		return provider.ErrServer
	}
	if status >= 400 {
		switch {
		case strings.Contains(body, "messages 不能为空"):
			return provider.ErrBadParams
		case strings.Contains(lower, "context length"), strings.Contains(lower, "too long"), strings.Contains(lower, "tokens exceed"):
			return provider.ErrPromptTooLong
		case strings.Contains(lower, "invalid image"), strings.Contains(lower, "image_url"):
			return provider.ErrImageInvalid
		case strings.Contains(lower, "content policy"), strings.Contains(lower, "sensitive"):
			return provider.ErrContentBlocked
		}
		return provider.ErrBadParams
	}
	return provider.ErrNone
}

// hardMarkers 余额/积分不足关键词。
var hardMarkers = []string{
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽",
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required",
}

// truncate rune 安全截断。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
