// Package upstream 封装对 CodeBuddy 上游（chat / billing / auth）的全部 HTTP 调用，
// 以及错误分类（驱动 pool 冷却状态机）。
package upstream

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ErrKind 错误分类，pool 据此决定冷却时长。
type ErrKind = provider.ErrKind

const (
	ErrNone           = provider.ErrNone           // 成功
	ErrHardCredit     = provider.ErrHardCredit     // 余额/权益不足 → 长冷却
	ErrSoftRate       = provider.ErrSoftRate       // 429 软限流 → 短冷却
	ErrSessionDead    = provider.ErrSessionDead    // 登录态失效 → 禁用
	ErrNotFound       = provider.ErrNotFound       // 404 上游偶发 → 短冷却不累计 errCount
	ErrServer         = provider.ErrServer         // 5xx 上游故障
	ErrClient         = provider.ErrClient         // 其他 4xx / 业务错误
	ErrContentBlocked = provider.ErrContentBlocked // 内容拦截
	ErrPromptTooLong  = provider.ErrPromptTooLong  // 上下文超限
	ErrImageInvalid   = provider.ErrImageInvalid   // 图片格式/数据无效
	ErrBadParams      = provider.ErrBadParams      // 出站 body 无法解析
	ErrWafBlock       = provider.ErrWafBlock       // WAF 拦截
	ErrAccountFault   = provider.ErrAccountFault   // 账号级故障
	ErrModelBlocked   = provider.ErrModelBlocked   // 模型不存在
)

// Error 带分类的上游错误。
type Error = provider.Error

// hardMarkers 余额不足关键词（小写比较 + 中文原文比较双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// Classify 按 HTTP 状态码 + body 判定错误类别。
// 内容拦截 marker（小写子串匹配）。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// invalidImageMarkers 图片请求格式/数据无效（HTTP 400）的**文案**形态。
//
// 这类错误由请求内容决定，不是账号问题：同一 body 换任何账号都会得到相同的解析
// 错误，轮转白费时间、NoteError 还会把健康账号喂到冷却。故归 ErrImageInvalid
// （请求级错误：不冷却、不计数、原样透传）。
//
// 业务码 11135 不列在这里：字面量 marker 只能覆盖紧凑 JSON（`"code":11135`），
// 上游返回带空白的合法形态（`{"code": 11135, ...}`）会漏判 → 退化成 ErrClient
// 并被罚号。业务码统一走 codeMarker（见 Classify 的 400 分支），与 11115/11101 同口径。
var invalidImageMarkers = []string{
	"invalid image_url content",
	"invalid_image_data",
	"replace the image",
}

// codeMarker 是本包对 provider.CodeMarker 的别名（判定规则与坑的说明见那边）。
var codeMarker = provider.CodeMarker

// Classify 按 HTTP 状态码 + body 判定错误类别。
// 429 必须在 hardMarkers 之前——限流 body 高频带 "quota exceeded"，先判 hardRule 会误归 12h 硬冷却。
// 例外：429 携带业务码 14018 是明确的「账号积分耗尽」（见下），按硬冷却弃号。
func Classify(status int, body string) ErrKind {
	if status == http.StatusPaymentRequired {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return ErrSessionDead
		}
	}
	// 429 + 业务码 14018 = 账号积分耗尽（上游把「余额耗尽」也走 429 返回）：
	// 按硬冷却弃号，等次日签到恢复。必须放在 429 兜底之前。
	// 只认结构化业务码，不靠文案：429 body 高频带 "quota exceeded" 这类跨计费/限流
	// 两界的措辞，靠文案猜会把限流误归硬冷却，白扔号约 12h（这正是 429 前置的原因）。
	if status == http.StatusTooManyRequests && codeMarker(lower, "14018") {
		return ErrHardCredit
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	// 非 429 但 body 含限流文案（200+11140/400）→ 软限流
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "rate-limiting") ||
		strings.Contains(lower, "too many requests") || strings.Contains(lower, "usage limit") ||
		strings.Contains(lower, "请求过于频繁") || strings.Contains(lower, "限流") {
		return ErrSoftRate
	}
	// 请求级错误先于 404/5xx 与通用 4xx 判定：这些业务码出现在 404 上时若落到
	// ErrNotFound（→ 软冷却账号），就把「上下文超限 / 图片格式错 / body 畸形」误判成
	// 账号问题了——它们与账号健康无关，换任何账号结果都一样。
	// 位置必须在 429 / hardMarkers / 限流文案之后：那三条更权威（429 上的
	// "quota exceeded" 措辞会先被接管，见上方注释）。
	if status == http.StatusBadRequest || status == http.StatusNotFound {
		if strings.Contains(lower, "prompt is too long") || codeMarker(lower, "11115") {
			return ErrPromptTooLong
		}
		// 图片格式/数据无效必须排在 11101 之前：上游的图片解析失败信封里 code
		// 就是 11101（`Parse message failed: invalid image_url content ...`），
		// 先判 11101 会把「图片有问题」误归「body 畸形」。
		if status == http.StatusBadRequest {
			for _, m := range invalidImageMarkers {
				if strings.Contains(lower, m) {
					return ErrImageInvalid
				}
			}
			if codeMarker(lower, "11135") {
				return ErrImageInvalid
			}
		}
		if codeMarker(lower, "11101") || strings.Contains(body, "Unmarshal chat params failed") {
			return ErrBadParams
		}
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		for _, m := range contentBlockedMarkers {
			if strings.Contains(lower, m) {
				return ErrContentBlocked
			}
		}
		// WAF 403：无业务信封的拦截形态
		if status == http.StatusForbidden && !strings.Contains(body, `"code":`) && strings.TrimSpace(body) != "" {
			return ErrWafBlock
		}
		// 账号级故障（11140/14017）
		if strings.Contains(lower, "request illegal") || strings.Contains(lower, "trial not activated") {
			return ErrAccountFault
		}
		return ErrClient
	}
	// HTTP 200 但业务 code 非 0 且含余额关键词的情况已被上面 hardMarkers 捕获。
	return ErrNone
}

// apiEnvelope 上游统一信封。
type apiEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// Client 上游 HTTP 客户端。Base 字段可覆盖便于测试。
type Client struct {
	HTTP *http.Client
	// BillingHTTP 供账单/签到接口使用（短超时，慢网络下避免面板操作长时间假死）。
	// 为 nil 时回退到 HTTP。
	BillingHTTP *http.Client
	// StreamHTTP 专供对话流式请求：**无总超时**。
	//
	// 为什么必须分开：http.Client.Timeout 是整请求上限（计时器在 Do() 返回后继续跑，
	// 直到 body 读完），而 SSE 整个生成期都在读 body —— 长思考请求会被从流中间掐断。
	// 客户端表现为「突然无响应」（补帧后改为收到 error 帧 + [DONE]）。空闲兜底改由
	// IdleReader 承担。
	StreamHTTP *http.Client
	// IdleTimeout 流式空闲超时：连续该时长读不到任何字节即判上游卡死。
	// 0 = 用 DefaultIdleTimeout。由 main 装配时注入 config.upstream.stream_idle_seconds。
	IdleTimeout time.Duration

	ChatBaseCN      string
	BillingBaseCN   string
	ChatBaseGlobal  string
	BillingBaseGlob string
}

// DefaultIdleTimeout 流式空闲超时默认值。
//
// 实测正常请求 8~40s 完成（同一模型），90s 足够宽松——只拦「真的一个字节都不出」。
const DefaultIdleTimeout = 90 * time.Second

// New 生产默认值。Transport 加固：禁 h2 + Dial 超时/keepalive + TLS 握手超时 + ResponseHeaderTimeout。
func New() *Client {
	tr := newTransport()
	return &Client{
		HTTP:            &http.Client{Timeout: 120 * time.Second, Transport: tr},
		BillingHTTP:     &http.Client{Timeout: 30 * time.Second, Transport: tr},
		StreamHTTP:      &http.Client{Transport: newTransport()},
		ChatBaseCN:      "https://copilot.tencent.com",
		BillingBaseCN:   "https://www.codebuddy.cn",
		ChatBaseGlobal:  "https://www.workbuddy.ai",
		BillingBaseGlob: "https://www.workbuddy.ai",
	}
}

func newTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 15 * time.Second}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSNextProto:          make(map[string]func(string, *tls.Conn) http.RoundTripper),
		TLSHandshakeTimeout:   10 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
}

// billingClient 返回账单接口用的 HTTP 客户端。
func (c *Client) billingClient() *http.Client {
	if c.BillingHTTP != nil {
		return c.BillingHTTP
	}
	return c.HTTP
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

func (c *Client) chatBase(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return c.ChatBaseGlobal
	}
	return c.ChatBaseCN
}

func (c *Client) billingBase(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return c.BillingBaseGlob
	}
	return c.BillingBaseCN
}

// modelsPath 返回模型/定价接口路径：国际版走 /v2/enterprises/personal/models
// （CN 的 /console/enterprises/personal/models 路径在国际版网关返回 500）。
func (c *Client) modelsPath(a *auth.Auth) string {
	if a != nil && a.Region() == "global" {
		return "/v2/enterprises/personal/models"
	}
	return "/console/enterprises/personal/models"
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 时返回带 body 片段的 *Error。
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	return c.doJSONWith(c.HTTP, req)
}

// doJSONBilling 用短超时账单客户端发请求。
func (c *Client) doJSONBilling(req *http.Request) (json.RawMessage, error) {
	return c.doJSONWith(c.billingClient(), req)
}

func (c *Client) doJSONWith(client *http.Client, req *http.Request) (json.RawMessage, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	if env.Code != 0 {
		kind := Classify(resp.StatusCode, env.Msg)
		if kind == ErrNone {
			kind = ErrClient
		}
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))}
	}
	return env.Data, nil
}

// RefreshToken 刷新 access token；成功时更新 a 的字段（缺省值保留旧值），
// 调用方负责 SaveAtomic。全程持 a 锁，防止并发 SaveAtomic 读半更新 token。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	oldRefresh := a.RefreshToken
	log.Printf("workbuddy refresh start uid=%s", a.UID)
	if strings.TrimSpace(a.RefreshToken) == "" {
		err := fmt.Errorf("no refreshToken")
		log.Printf("workbuddy refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	url := c.chatBase(a) + "/v2/plugin/auth/token/refresh"
	req, err := http.NewRequest(http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	RefreshHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("workbuddy refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		err := fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
		log.Printf("workbuddy refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	// preserveExpiry：响应缺 expiresIn 时保留旧过期时间，避免刷新风暴。
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	log.Printf("workbuddy refresh success uid=%s refresh_rotated=%t expires_at=%d", a.UID, a.RefreshToken != oldRefresh, a.ExpiresAt)
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、body 为上游响应体（供调用方 Classify(status, string(body))）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	url := c.chatBase(a) + "/v2/chat/completions"
	prepared := PrepareBody(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, err
	}
	ChatHeaders(req, a)
	// 用无总超时的 client：长思考请求不受整请求上限约束（见 Client.StreamHTTP）。
	resp, err := c.streamClient().Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_stream uid=%s: upstream %d %s body=%s req=%s",
			a.UID, resp.StatusCode, kind, provider.LogBody(string(raw)), provider.LogParams(prepared))
		return nil, resp.StatusCode, raw, nil
	}
	// 空闲看门狗兜底：连续 idleTimeout 读不到字节即判上游卡死并关掉 body。
	// 流式与 stream:false 的聚合路径共用这一层（聚合时由「总时长硬上限」
	// 变为「空闲上限」，长而持续输出的响应不再被误杀）。
	return provider.NewIdleReader(resp.Body, c.idleTimeout()), resp.StatusCode, nil, nil
}

// ModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type ModelInfo = provider.ModelInfo

// modelReasoningMeta 目录接口返回的思考能力元数据（reasoning 子对象）。
// 远端下发时是档位能力的权威来源（见 internal/reasoning/catalog.go 的三级查找）；
// 未下发（零值）时回落静态兜底表。注意 supportedEfforts 为空 ≠ 不支持思考，
// 而是「该模型没有可选档位」，因此不能据此判定能力。
type modelReasoningMeta struct {
	Effort             string   `json:"effort"`
	Summary            string   `json:"summary"`
	DefaultEffort      string   `json:"defaultEffort"`
	SupportedEfforts   []string `json:"supportedEfforts"`
	CanDisableThinking bool     `json:"canDisableThinking"`
}

// catalogModel 目录接口的原始模型条目（含能力字段）。
type catalogModel struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	MaxInputTokens  int64  `json:"maxInputTokens"`
	MaxOutputTokens int64  `json:"maxOutputTokens"`
	Disabled        bool   `json:"disabled"`
	// 能力字段（上游目录返回）
	SupportsImages     bool               `json:"supportsImages"`
	SupportsReasoning  bool               `json:"supportsReasoning"`
	SupportsToolCall   bool               `json:"supportsToolCall"`
	DisabledMultimodal bool               `json:"disabledMultimodal"`
	Reasoning          modelReasoningMeta `json:"reasoning"`
}

// FetchModels 调上游动态模型接口。
// 字段名与上游实际返回对齐：maxInputTokens（非 contextWindow）、maxOutputTokens（非 maxTokens）。
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	url := c.chatBase(a) + c.modelsPath(a)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				MaxInputTokens  int64  `json:"maxInputTokens"`
				MaxOutputTokens int64  `json:"maxOutputTokens"`
				Disabled        bool   `json:"disabled"`
				// 能力字段（国内版目录同样返回，实测可用）
				SupportsImages     bool               `json:"supportsImages"`
				SupportsReasoning  bool               `json:"supportsReasoning"`
				SupportsToolCall   bool               `json:"supportsToolCall"`
				DisabledMultimodal bool               `json:"disabledMultimodal"`
				Reasoning          modelReasoningMeta `json:"reasoning"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("no cli agent models found")
	}
	dynMap := make(map[string]catalogModel, len(env.Data.Models))
	for _, m := range env.Data.Models {
		dynMap[m.ID] = m
	}
	out := make([]ModelInfo, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		def := m.Reasoning.DefaultEffort
		if def == "" {
			def = m.Reasoning.Effort
		}

		mi := ModelInfo{
			ID:             m.ID,
			Name:           m.Name,
			ContextWindow:  m.MaxInputTokens,
			ContextFromAPI: true, // 目录接口真实返回
			MaxTokens:      m.MaxOutputTokens,
			// 上游显式声明 supportsImages 且未被 disabledMultimodal 关闭
			SupportsImages:    m.SupportsImages && !m.DisabledMultimodal,
			SupportsReasoning: m.SupportsReasoning,
			SupportsTools:     m.SupportsToolCall,
			// 档位能力（远端权威，缺失时由 internal/reasoning 静态表兜底）
			SupportedEfforts:       m.Reasoning.SupportedEfforts,
			ReasoningEfforts:       provider.CleanEfforts(m.Reasoning.SupportedEfforts),
			DefaultEffort:          m.Reasoning.DefaultEffort,
			ReasoningCanDisable:    m.Reasoning.CanDisableThinking,
			DefaultReasoningEffort: def,
		}
		out = append(out, mi)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// applyCatalogReasoning 把目录 reasoning 对象投影进 ModelInfo（供任一渠道复用，
// 字段形状对齐 Buddy2api model_reasoning.py / dsh-connect upstream.ts 实测）。
// 全部字段缺失时不留任何痕迹（nil/空串），消费方不得把「未声明」当「不支持」。
func applyCatalogReasoning(m *catalogModel) (efforts []string, def string, canDisable *bool) {
	if m == nil {
		return nil, "", nil
	}
	r := m.Reasoning
	efforts = provider.CleanEfforts(r.SupportedEfforts)
	def = strings.TrimSpace(r.DefaultEffort)
	if def == "" {
		def = strings.TrimSpace(r.Effort) // 老式默认档字段回退
	}
	if efforts == nil && def == "" {
		return nil, "", nil // reasoning 对象存在但无任何档位信息：整体视为未声明
	}
	return efforts, def, nil
}

// UserResource 查询账号当前可花费积分余额（所有套餐 CycleCapacity 聚合，负值钳 0）。
func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) {
	resp, err := c.getUserResource(a)
	if err != nil {
		return 0, err
	}
	for _, acct := range resp.Response.Data.Accounts {
		remain += acct.remain()
	}
	return remain, nil
}

// UserResourceDetail 查询账号积分明细（所有套餐条目）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	resp, err := c.getUserResource(a)
	if err != nil {
		return 0, nil, err
	}
	var total int64
	items := make([]provider.ResourceItem, 0, len(resp.Response.Data.Accounts))
	for _, acct := range resp.Response.Data.Accounts {
		tot, used, remain := acct.bill()
		total += remain
		items = append(items, provider.ResourceItem{
			Name:     acct.PackageName,
			Total:    tot,
			Used:     used,
			Remain:   remain,
			ExpireAt: acct.expireAt(),
			// 套餐名+到期日组合伪键：上游无显式 ID，同名录包靠周期结束时间区分
			Key:    fmt.Sprintf("%s|%s", acct.PackageName, acct.expireAt()),
			Usable: true, // 国内版无端点分区，所有套餐均可被本工具消耗
		})
	}
	return total, items, nil
}

// softRateResetLoc 上游墙钟时间口径：固定按 UTC+8 解释。
// 上游下发的 CycleEndTime 等时间串均为国内时区墙钟；用 time.Local 解析会在
// 非 UTC+8 机器上把到期日算错一天。
var softRateResetLoc = time.FixedZone("UTC+8", 8*60*60)

// resourceAccount get-user-resource 单套餐条目（UserResource 与 UserResourceDetail 共用）。
type resourceAccount struct {
	PackageName         string `json:"PackageName"`
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
	// CycleEndTime 周期结束时间（"2006-01-02 15:04:05"，UTC+8 墙钟）。
	// 上游不提供 PackageEndTime（R-A/R-B 实测两域 42 字段均无），到期判据即此字段。
	CycleEndTime string `json:"CycleEndTime"`
}

// bill 按周期口径优先返回 (总额, 已用, 剩余)；剩余负值钳 0。
func (r resourceAccount) bill() (tot, used, remain int64) {
	switch {
	case r.CycleCapacitySize > 0:
		tot, used, remain = r.CycleCapacitySize, r.CycleCapacityUsed, r.CycleCapacityRemain
	case r.CycleCapacityRemain > 0 || r.CycleCapacityUsed > 0:
		tot, used, remain = r.CycleCapacityRemain+r.CycleCapacityUsed, r.CycleCapacityUsed, r.CycleCapacityRemain
	default:
		tot, used, remain = r.CapacitySize, r.CapacityUsed, r.CapacityRemain
	}
	if remain < 0 {
		remain = 0
	}
	return tot, used, remain
}

// remain 单套餐剩余额度（bill 的 remain 分量）。
func (r resourceAccount) remain() int64 {
	_, _, remain := r.bill()
	return remain
}

// expireAt 把 CycleEndTime 墙钟串转为 YYYY-MM-DD；缺失/不可解析时返回空串。
func (r resourceAccount) expireAt() string {
	ts := strings.TrimSpace(r.CycleEndTime)
	if ts == "" {
		return ""
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04:05", ts, softRateResetLoc); err == nil {
		return t.Format("2006-01-02")
	}
	return ""
}

// userResourceResp get-user-resource 响应信封。
type userResourceResp struct {
	Response struct {
		Data struct {
			Accounts []resourceAccount `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

// getUserResource 发 get-user-resource 请求并解析响应（两个消费方共享请求体与解析）。
func (c *Client) getUserResource(a *auth.Auth) (*userResourceResp, error) {
	url := c.billingBase(a) + "/v2/billing/meter/get-user-resource"
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	BillingHeaders(req, a)
	data, err := c.doJSONBilling(req)
	if err != nil {
		return nil, err
	}
	var resp userResourceResp
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("resource parse: %w", err)
	}
	return &resp, nil
}

// DailyCheckin 执行每日签到。已签到（业务 code 非 0）也返回错误，调用方按 msg 区分。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	log.Printf("workbuddy checkin start uid=%s", a.UID)
	url := c.billingBase(a) + "/v2/billing/meter/daily-checkin"
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	BillingHeaders(req, a)
	_, err = c.doJSONBilling(req)
	if err != nil {
		log.Printf("workbuddy checkin failed uid=%s err=%v", a.UID, err)
		return err
	}
	log.Printf("workbuddy checkin success uid=%s", a.UID)
	return nil
}

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Stream 实现 provider.Upstream（WorkBuddy 上游已是 OpenAI SSE，直接透传）。
// 返回值为末帧捕获的 usage（供记账，上游未返回时为 nil）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	err := StreamCapture(w, r, func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 实现 provider.Upstream（WorkBuddy OpenAI SSE 聚合）。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) { return Aggregate(r) }

// FetchModelPricing 从 /console/enterprises/personal/models 拉取模型积分倍率。
// 返回全量模型定价（含 credits 字段），不受 cli agent 过滤限制。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	url := c.chatBase(a) + c.modelsPath(a)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.AccessTokenValue())
	req.Header.Set("Accept", "application/json")
	origin := originRefererFor(a)
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("X-User-Id", a.UID)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("pricing api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID      string   `json:"id"`
				Name    string   `json:"name"`
				Credits string   `json:"credits"` // "x0.79 credits"
				Tags    []string `json:"tags"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("pricing parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("pricing api code=%d", env.Code)
	}
	out := make([]provider.ModelPricing, 0, len(env.Data.Models))
	for _, m := range env.Data.Models {
		rate := parseCredits(m.Credits)
		if rate <= 0 && m.ID != "auto" {
			continue // hunyuan-chat 等非计费模型
		}
		note, color := parseBadge(m.Tags)
		// auto 调度器无 credits 字段：标记为非显式倍率，
		// 避免费率面板把「缺失→0」误显示为 Free。
		explicit := strings.TrimSpace(m.Credits) != ""
		out = append(out, provider.ModelPricing{
			Model:    m.ID,
			Channel:  "workbuddy",
			Rate:     rate,
			Note:     note,
			Color:    color,
			Explicit: &explicit,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pricing api returned empty models")
	}
	return out, nil
}

// parseCredits 解析 "x0.08 credits" → 0.08。
func parseCredits(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	// 去掉 "x" 前缀和 " credits" 后缀
	s = strings.TrimPrefix(s, "x")
	s = strings.TrimSuffix(s, " credits")
	s = strings.TrimSpace(s)
	var v float64
	fmt.Sscanf(s, "%f", &v)
	return v
}

// parseBadge 从上流 tags 中提取促销标签与颜色。
// 上流格式为 "badge:<文案>:<#颜色>"（如 "badge:独家优惠:#FF0000"），
// 颜色部分必须剥离，否则会原样显示在 UI 上。
// 兼容无颜色后缀（"badge:限时免费"）的情形。
func parseBadge(tags []string) (label, color string) {
	const prefix = "badge:"
	for _, tag := range tags {
		if !strings.HasPrefix(tag, prefix) {
			continue
		}
		body := strings.TrimPrefix(tag, prefix)
		// 颜色取最后一个 ':' 之后的 #RRGGBB/#RGB；无则视为纯文案
		if i := strings.LastIndex(body, ":"); i >= 0 {
			c := strings.TrimSpace(body[i+1:])
			if strings.HasPrefix(c, "#") && len(c) >= 4 && len(c) <= 9 {
				return strings.TrimSpace(body[:i]), c
			}
		}
		return strings.TrimSpace(body), ""
	}
	return "", ""
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
