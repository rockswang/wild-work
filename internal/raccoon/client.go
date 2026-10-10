package raccoon

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
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
)

// Client 小浣熊（官方托管网关 xiaohuanxiong.com）上游客户端。
// 登录编排不在本包：既有「从本机客户端导入」（internal/app/import_local.go），
// 也有「浏览器授权登录」（internal/login_raccoon，复用本包 protocol*.go 的协议工具）。
// 本包只负责上游调用与桌面协议底层能力。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 专供对话流式请求：**无总超时**。
	//
	// 为什么必须分开：http.Client.Timeout 是整请求上限（计时器在 Do() 返回后继续跑，
	// 直到 body 读完），而 SSE 整个生成期都在读 body —— 长思考请求会被从流中间掐断。
	// 本渠道是「剥离档位、走上游默认最深思考」，生成期天然更长，风险同 loomy。
	// 空闲兜底改由 IdleReader 承担。
	StreamHTTP *http.Client
	// IdleTimeout 流式空闲超时：连续该时长读不到任何字节即判上游卡死。
	// 0 = 用 DefaultIdleTimeout。由 main 装配时注入 config.upstream.stream_idle_seconds。
	IdleTimeout time.Duration

	// liveMu 保护 liveIDs（动态目录缓存）。
	//
	// 为什么需要它：ChatStream 会本地校验模型名（上游对未知模型静默回落，见 KnownModel），
	// 但**只能用静态表** ⇒ 上游目录新增的模型（如 sn-sensenova-6-8-flash）会被本地误拒，
	// 而同一个模型正被 /v1/models 正常透出（它读的是动态目录）——"面板列出却调不动"。
	// 故每次 fetchCatalog 成功后把目录记下来，供校验时与静态表取并集。
	// 单调扩大：已知集合只增不减，避免目录瞬时抖动把可用模型判成未知。
	liveMu  sync.RWMutex
	liveIDs map[string]bool

	// Base 网关基址（默认 LLMBase）；测试注入 httptest 假上游用，生产不设。
	Base string
	// AuthBase 认证/业务基址（默认 BaseURL）；仅测试注入用，生产不设。
	// 与 Base 分开：刷新走站点根前缀（/api/web/auth/v1/refresh），而推理走 /api/web/llm/v2。
	AuthBase string
}

// baseURL 生效的网关基址。
func (c *Client) baseURL() string {
	if b := strings.TrimRight(strings.TrimSpace(c.Base), "/"); b != "" {
		return b
	}
	return LLMBase
}

// authBaseURL 生效的认证/业务基址（refresh / 积分等站点根前缀接口）。
func (c *Client) authBaseURL() string {
	if b := strings.TrimRight(strings.TrimSpace(c.AuthBase), "/"); b != "" {
		return b
	}
	return BaseURL
}

// DefaultIdleTimeout 流式空闲超时默认值（与 loomy 对齐，见其注释）。
const DefaultIdleTimeout = 90 * time.Second

// New 默认 180s 超时的客户端（仅作用于非流式调用；流式见 StreamHTTP）。
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

// httpClient 返回非流式调用用的客户端（目录/积分/refresh 的 do()）。
// 仍有总超时：这些是短请求，总超时是恰当的兜底。
func (c *Client) httpClient() *http.Client {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 180 * time.Second, Transport: newTransport()}
	}
	return c.HTTP
}

// streamClient 返回流式调用用的客户端（无总超时）。
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

func accessToken(a *auth.Auth) string { return strings.TrimSpace(a.AccessTokenValue()) }

// do 发一次带 Bearer 的请求（body 为 nil 时视为无请求体）。
// 只用于非流式调用；流式（ChatStream）需要保留 resp.Body，另行实现。
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
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken(a))
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp, raw, rerr
}

// ---------------------------------------------------------------------------
// 凭据刷新
// ---------------------------------------------------------------------------

// RefreshToken 用 refresh_token 换新 access_token。
//
// 上游会**轮换 refresh_token**（阶段 C 实测 + 官方 scheduleAuth.js），故响应里带了新值时必须一并更新；
// 落盘由调用方完成（AGENTS §6.20：凡调 RefreshToken 必紧跟 SaveAtomic）。
func (c *Client) RefreshToken(a *auth.Auth) error {
	rt := strings.TrimSpace(a.RefreshTokenValue())
	if rt == "" {
		// 本地无 refresh 可用：客户端文件里可能有活凭证（本渠道与客户端共用会话）。
		if rerr := RescueFromClientSession(a); rerr == nil {
			log.Printf("raccoon: 本地 refresh 为空，已采纳本机客户端最新令牌 uid=%s", a.UID)
			return nil
		}
		return fmt.Errorf("raccoon: 无 refresh token，需重新从客户端导入凭据")
	}
	payload, err := json.Marshal(map[string]string{"refresh_token": rt})
	if err != nil {
		return err
	}
	// 先试 web 前缀：2026-10-10 实测它是唯一可用端点（electron 旧前缀已 404）。
	// 404 时回落 electron 前缀，兼容更早的上游版本。
	base := c.authBaseURL()
	resp, raw, err := c.do(http.MethodPost, base+EpAuthRefreshWeb, a, payload)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp, raw, err = c.do(http.MethodPost, base+EpAuthRefresh, a, payload)
		if err != nil {
			return err
		}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// 401/403 的最常见成因：与客户端共用的会话被客户端续期轮换作废（客户端文件里
		// 始终持有唯一活凭证）。先尝试本机自救，成功即视为刷新成功（调用方会 SaveAtomic）。
		if rerr := RescueFromClientSession(a); rerr == nil {
			log.Printf("raccoon: refresh 被拒（疑客户端轮换），已采纳本机客户端最新令牌 uid=%s", a.UID)
			return nil
		}
		return &provider.Error{Kind: provider.ErrSessionDead, Status: resp.StatusCode, Msg: truncate(string(raw), 300)}
	}
	if resp.StatusCode != http.StatusOK {
		return &provider.Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 300)}
	}
	var out struct {
		Data struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("raccoon refresh 响应解析失败: %w (%s)", err, truncate(string(raw), 200))
	}
	if out.Data.AccessToken == "" {
		return fmt.Errorf("raccoon refresh 未返回 access_token: %s", truncate(string(raw), 200))
	}
	a.Lock()
	a.AccessToken = out.Data.AccessToken
	if out.Data.RefreshToken != "" {
		a.RefreshToken = out.Data.RefreshToken // 上游轮换
	}
	a.ExpiresAt = jwtExp(out.Data.AccessToken)
	a.Unlock()
	return nil
}

// RefreshWithToken 用给定的 refresh_token 换新令牌（不依赖已有 Auth 实例）。
//
// 供「网页版登录」在 CDP 捕获 Cookie 后完成**有效性校验 + 令牌获取**两步：
// 上游 refresh 会轮换 refresh_token，故返回的 refresh 通常与传入值不同，调用方必须落盘
// （AGENTS §6.20：凡调 RefreshToken 必紧跟 SaveAtomic）。
func (c *Client) RefreshWithToken(refreshToken string) (access, refresh string, err error) {
	a := &auth.Auth{RefreshToken: strings.TrimSpace(refreshToken)}
	if err := c.RefreshToken(a); err != nil {
		return "", "", err
	}
	return a.AccessTokenValue(), a.RefreshTokenValue(), nil
}

// jwtExp 从 JWT 取 exp（Unix 秒）；解析失败返回 0（NeedsRefresh 会视作需刷新）。
func jwtExp(tok string) int64 {
	parts := strings.Split(strings.TrimSpace(tok), ".")
	if len(parts) < 2 {
		return 0
	}
	seg := strings.NewReplacer("-", "+", "_", "/").Replace(parts[1])
	if m := len(seg) % 4; m != 0 {
		seg += strings.Repeat("=", 4-m)
	}
	raw, err := base64.StdEncoding.DecodeString(seg)
	if err != nil {
		return 0
	}
	var p struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0
	}
	return p.Exp
}

// ---------------------------------------------------------------------------
// 推理
// ---------------------------------------------------------------------------

// ChatStream 发流式（或非流式）对话请求。
// 返回 (resp.Body, status, nil, nil) 表示成功；上游 4xx/5xx 时返回 (nil, status, body, nil)。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	// 模型名本地校验：阶段 C 实测确认上游对未知模型**静默回落到默认模型**并返回 200，
	// 不校验会让用户以为在用 A 模型、实际消耗 B 模型的额度。
	// 校验集合 = 静态表 ∪ 已拉取到的动态目录（后者由 FetchModels/FetchModelPricing 填充）。
	if m := modelOf(body); m != "" && !c.knownModel(m) {
		return nil, http.StatusBadRequest, []byte(unknownModelBody(m)), nil
	}
	orig := body
	body = forceUpstreamDeepThinking(body)
	logReasoningStrip(orig, body)

	req, err := http.NewRequest(http.MethodPost, c.baseURL()+EpChat, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken(a))
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
	return provider.NewIdleReader(resp.Body, c.idleTimeout()), resp.StatusCode, nil, nil
}

// logReasoningStrip 记一行剥离日志（对齐 qoder 三渠道的排障约定）。
//
// 动机：剥离是**静默**的，出问题时无法判断「客户端到底有没有发档位」。
// 特意打 info 级（不是 debug）：该行为反直觉，日志里留痕便于事后核对。
func logReasoningStrip(orig, stripped []byte) {
	// 只有「原本有、剥完没」才算真剥离 —— 逐字节相等说明本就没该字段（或非法 JSON
	// 原样返回），无信息量不打日志。
	if bytes.Equal(orig, stripped) {
		return
	}
	var probe struct {
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(orig, &probe); err != nil {
		return
	}
	log.Printf("raccoon reasoning: model=%q stripped=%q (走上游默认=最深)", probe.Model, probe.ReasoningEffort)
}

// forceUpstreamDeepThinking 移除 `reasoning_effort`，强制走上游默认（= 深度思考）。
//
// 取证结论（2026-09-24 VM 实测，多轮交叉采样）：
//
//  1. 官方客户端的「深度思考 / 快速」按钮产出 `enable_deep_thinking`（布尔，默认 false），
//     但那是 **agent 层**参数（`POST {base}/sessions/{id}/chat-conversations`、
//     `plans/execute-async` 才吃它）。本渠道对接的 LLM 网关对该字段直接 **500**：
//     `AsyncCompletions.create() got an unexpected keyword argument 'enable_deep_thinking'`。
//
//  2. LLM 网关认的是标准 `reasoning_effort`（客户端 LLM 层是 @ai-sdk/openai-compatible，
//     其 settings.reasoningEffort → reasoning_effort；SDK 声明域 low/medium/high）。
//
//  3. **但实测方向与直觉相反**：同 prompt 交叉采样（reasoning_tokens，权威指标）——
//
//     无字段（上游默认）  rtok = 2300 / 2147 / 2133 / 4067 / 1167 / 2241
//     reasoning_effort=high  rtok = 142 / 260 / 331 / 135 / 98 / 145
//     reasoning_effort=low   rtok = 0
//     reasoning_effort=none  rtok = 1803（另有 0 与 7 的样本，见下）
//
//     即：**上游默认档本身就是最深思考，下发任何 `reasoning_effort` 都会削弱它**
//     （litellm 侧把该字段映射成一个偏保守的思考预算）。fast 标签模型上同样成立
//     （sn-glm-5-3-flash：默认 rtok=26 > high=8 > medium=7 > none=0）。
//
// 因此「确保用的是深度思考」的做法是**删除**该字段而非下发 high —— 这正是本渠道
// 不接入档位（`SupportsEffortKind` 不含 raccoon）的原因：客户端若表达了档位
// （WorkBuddy 的档位选择器、面板默认档），一律在此被剥离，保证实际走深度思考。
//
// 代价：客户端**无法**通过档位让该渠道走"快速"（那需要换用 fast 标签的模型）。
// 这是取证的必然结果 —— 下发 low 也未必等于快速（实测 0 与 1803 并存，行为不稳定）。
func forceUpstreamDeepThinking(body []byte) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, ok := obj["reasoning_effort"]; !ok {
		return body
	}
	delete(obj, "reasoning_effort")
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// modelOf 取请求体里的 model（解析失败返回空串）。
func modelOf(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return strings.TrimSpace(probe.Model)
}

// knownModel 模型名是否可接受（ChatStream 的本地校验入口）。
//
// 判据 = KnownModel（静态表 + 默认模型 + 客户端别名） ∪ 已拉取到的动态目录。
// 动态部分**必须**参与：/v1/models 透出的是动态目录（FetchModels 的结果），
// 只认静态表会导致"面板列出却调不动"（2026-10-05 实测 sn-sensenova-6-8-flash：
// 上游 200 正常服务，本地 400 未知模型）。
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
// 目录瞬时拉取失败/上游临时抽掉某模型时，不应把已确认可用的模型判成未知。
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

// unknownModelBody 未知模型时返回的 OpenAI 风格 400（不请求上游）。
func unknownModelBody(m string) string {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": fmt.Sprintf("raccoon: 未知模型 %q。上游对未知模型会静默回落到默认模型，故本地直接拒绝；可用模型见 GET /v1/models", m),
		"type":    "invalid_request_error",
		"code":    "model_not_found",
	}})
	return string(b)
}

// ---------------------------------------------------------------------------
// 模型与费率
// ---------------------------------------------------------------------------

type catalogResp struct {
	Code int `json:"code"`
	Data struct {
		Categories []struct {
			Type         string         `json:"type"`
			DefaultModel string         `json:"default_model"`
			Models       []catalogModel `json:"models"`
		} `json:"categories"`
	} `json:"data"`
}

type catalogModel struct {
	Name              string   `json:"name"`
	ModelName         string   `json:"model_name"`
	Description       string   `json:"description"`
	DisplayDesc       string   `json:"display_description"`
	Visible           bool     `json:"visible"`
	BillingCategory   string   `json:"billing_category"`
	BillingMultiplier float64  `json:"billing_multiplier"`
	Tags              []string `json:"tags"`
	AbilityLevel      int      `json:"ability_level"`
	Params            struct {
		ContextWindow int64 `json:"context_window"`
		MaxTokens     int64 `json:"max_tokens"`
	} `json:"params"`
}

// fetchCatalog 拉取模型目录（模型表与费率共用同一响应）。
func (c *Client) fetchCatalog(a *auth.Auth) (*catalogResp, []catalogModel, error) {
	resp, raw, err := c.do(http.MethodGet, c.baseURL()+EpModelCatalog, a, nil)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, &provider.Error{
			Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 300),
		}
	}
	var out catalogResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil, fmt.Errorf("raccoon 模型目录解析失败: %w (%s)", err, truncate(string(raw), 200))
	}
	var models []catalogModel
	for _, cat := range out.Data.Categories {
		models = append(models, cat.Models...)
	}
	// 记下本次目录里的模型名，供 ChatStream 的本地校验取并集
	// （否则上游新增模型会被误拒，而 /v1/models 已把它列出来）。
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if id := strings.TrimSpace(m.ModelName); id != "" {
			ids = append(ids, id)
		} else if id := strings.TrimSpace(m.Name); id != "" {
			ids = append(ids, id)
		}
	}
	c.rememberLiveModels(ids...)
	return &out, models, nil
}

// FetchModels 实现 provider.Upstream。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	_, models, err := c.fetchCatalog(a)
	if err != nil {
		return nil, err
	}
	out := make([]provider.ModelInfo, 0, len(models))
	for _, m := range models {
		id := strings.TrimSpace(m.ModelName)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" {
			continue
		}
		name := strings.TrimSpace(m.Description)
		if name == "" {
			name = id
		}
		info := provider.ModelInfo{
			ID:             id,
			Name:           name,
			ContextWindow:  m.Params.ContextWindow,
			MaxTokens:      m.Params.MaxTokens,
			ContextFromAPI: m.Params.ContextWindow > 0,
			SupportsImages: hasTag(m.Tags, "vision"),
			// 本渠道不投影档位（上游目录未声明 reasoning_efforts，见阶段 C 实测），
			// 仅如实声明模型是否具备推理标签，便于客户端自行选择。
			SupportsReasoning: hasTag(m.Tags, "reasoning"),
			// 上游 catalog 不声明工具能力（无 tools 字段），但 2026-09-23 阶段 E 实测
			// 默认模型 `raccoon-8c4485` 在带 tools 的请求下返回结构化
			// `finish_reason=tool_calls` + `tool_calls`，故按实测声明支持。
			// sn-* 系列因账号冷却未逐一验证（上游无声明，只能实测）。
			SupportsTools: true,
		}
		out = append(out, info)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("raccoon 模型目录为空")
	}
	return out, nil
}

// FetchModelPricing 实现 provider.Upstream：倍率直接取上游 billing_multiplier。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	_, models, err := c.fetchCatalog(a)
	if err != nil {
		return nil, err
	}
	explicit := true
	out := make([]provider.ModelPricing, 0, len(models))
	for _, m := range models {
		id := strings.TrimSpace(m.ModelName)
		if id == "" {
			id = strings.TrimSpace(m.Name)
		}
		if id == "" {
			continue
		}
		out = append(out, provider.ModelPricing{
			Model:    id,
			Channel:  ChannelName,
			Rate:     m.BillingMultiplier,
			Explicit: &explicit,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 额度 / 签到
// ---------------------------------------------------------------------------

// UserResource 实现 provider.Upstream：可用积分（`available_points`）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	remain, _, err := c.UserResourceDetail(a)
	return remain, err
}

// pointsBalanceData /api/web/points/v1/balance 的 data 段。
type pointsBalanceData struct {
	AvailablePoints int64 `json:"available_points"`
	DailyPoints     int64 `json:"daily_points"`
	MonthlyPoints   int64 `json:"monthly_points"`
	RewardPoints    int64 `json:"reward_points"`
	TopupPoints     int64 `json:"topup_points"`
	TopupFrozen     bool  `json:"topup_frozen"`
}

// pointsBalanceResp /api/web/points/v1/balance 的响应（2026-09-22 实测）。
type pointsBalanceResp struct {
	Code int               `json:"code"`
	Data pointsBalanceData `json:"data"`
}

// UserResourceDetail 实现 provider.Upstream：拉取积分余额并按上游的池子拆分条目。
//
// 端点 `GET {站点根}/api/web/points/v1/balance`（**与推理网关不同前缀**：推理在
// `/api/web/llm/v2`，积分在 `/api/web/points/v1`）。上游把额度分成四个池：
// 每日 / 月度 / 奖励 / 充值，另有 `topup_frozen` 标记充值池是否被冻结 —— 冻结时该池
// 对用户「看得见用不了」，正好对应 `provider.ResourceItem.Usable` 的语义。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	resp, raw, err := c.do(http.MethodGet, BaseURL+EpPointsBalance, a, nil)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, nil, &provider.Error{
			Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 300),
		}
	}
	var out pointsBalanceResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, nil, fmt.Errorf("raccoon 积分响应解析失败: %w (%s)", err, truncate(string(raw), 200))
	}
	// 业务码非 0 视为失败：points 服务用 `100002` 表示**参数错误**（与鉴权无关 ——
	// 这也是本渠道不按业务码判会话失效、只看 HTTP 状态的原因）。
	if out.Code != 0 {
		kind := provider.ErrClient
		if resp.StatusCode == http.StatusUnauthorized {
			kind = provider.ErrSessionDead
		}
		return 0, nil, &provider.Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 300)}
	}
	remain, items := parsePointsBalance(out.Data)
	return remain, items, nil
}

// parsePointsBalance 把上游余额结构转成 (可用余额, 明细条目)。
// 抽成纯函数便于单测（Client 的 baseURL 是常量，不便在测试里换 host）。
func parsePointsBalance(d pointsBalanceData) (int64, []provider.ResourceItem) {
	items := []provider.ResourceItem{
		{Name: "每日额度", Total: d.DailyPoints, Remain: d.DailyPoints, Usable: true},
		{Name: "奖励积分", Total: d.RewardPoints, Remain: d.RewardPoints, Usable: true},
		{Name: "充值积分", Total: d.TopupPoints, Remain: d.TopupPoints, Usable: !d.TopupFrozen},
		{Name: "月度积分", Total: d.MonthlyPoints, Remain: d.MonthlyPoints, Usable: true},
	}
	return d.AvailablePoints, items
}

// DailyCheckin 实现 provider.Upstream：小浣熊无签到活动。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return fmt.Errorf("raccoon 无签到活动")
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// Classify 按 HTTP 状态码 + body 判定错误类别（实现 provider.Upstream）。
//
// 上游错误信封为 LiteLLM 风格（阶段 C 实测）：
//
//	401 {"code":200001,"message":"authorization_empty_error"}
//	401 {"code":200003,"message":"authorization_verify_error"}
//	400 {"error":{"code":"400","message":"litellm.BadRequestError: Custom_raccoonException - ..."}}
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Classify 包级分类（便于单测与复用）。
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)

	// 本地拒绝的未知模型（见 ChatStream）：请求级错误，不罚号。
	if strings.Contains(lower, "model_not_found") {
		return provider.ErrBadParams
	}
	// 鉴权：上游用 401 + 200001/200003（缺 token / 校验失败）。
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return provider.ErrSessionDead
	}
	if status == http.StatusPaymentRequired {
		return provider.ErrHardCredit
	}
	// 429 优先于 hardMarkers（限流 body 常带 quota 字样，见 AGENTS §6.15）。
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
		// 上下文超限 / 内容拦截 / 参数错误都是请求级：不罚号、原文透传（AGENTS §6.25）。
		switch {
		case strings.Contains(lower, "message is bigger than"), strings.Contains(lower, "context length"),
			strings.Contains(lower, "context_length_exceeded"), strings.Contains(lower, "too long"):
			return provider.ErrPromptTooLong
		case strings.Contains(lower, "content policy"), strings.Contains(lower, "content filter"),
			strings.Contains(lower, "sensitive"):
			return provider.ErrContentBlocked
		case strings.Contains(lower, "invalid image"), strings.Contains(lower, "image_url"):
			return provider.ErrImageInvalid
		}
		return provider.ErrBadParams
	}
	return provider.ErrNone
}

// hardMarkers 余额/权益不足关键词。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "积分不足", "额度不足", "余额不足", "积分用完", "额度用尽",
}

// hasTag 判断 tags 是否含指定标签（大小写不敏感）。
func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

// truncate 字符串截断（rune 安全）。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}
