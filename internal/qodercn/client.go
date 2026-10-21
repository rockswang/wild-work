// client.go QoderCN 上游客户端：业务 API（dt- Bearer）+
// COSY 签名对话转发 + 错误分类，实现 provider.Upstream 接口。
// 代码级复制自 internal/qoder/client.go 后按 qoder2api 参数改造：
//   - 登录/刷新后从 /api/v1/userinfo 实测 userType（identity 签名用实测值）
//   - 模型路由仅用「上次成功拉取的动态表」缓存（无静态兜底）
//   - 签到：双路径（daily-check-in + campaigns，实测主路径是 campaigns）
package qodercn

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/reasoning"
)

// Client QoderCN 上游客户端。
type Client struct {
	HTTP *http.Client
	// StreamHTTP 专供对话流式请求：**无总超时**。
	//
	// 为什么必须分开：http.Client.Timeout 是整请求上限（计时器在 Do() 返回后继续跑，
	// 直到 body 读完），而 SSE 整个生成期都在读 body —— 长思考请求会被从流中间掐断。
	// 2026-09-25 实测同源渠道一次中断恰好 120.00s（= config.upstream.timeout_seconds），
	// 客户端表现为「突然无响应」。空闲兜底改由 IdleReader 承担。
	StreamHTTP *http.Client
	// IdleTimeout 流式空闲超时：连续该时长读不到任何字节即判上游卡死。
	// 0 = 用 DefaultIdleTimeout。由 main 装配时注入 config.upstream.stream_idle_seconds。
	IdleTimeout time.Duration
	Base        string // 业务 API，默认 https://openapi.qoder.com.cn
	Gateway     string // 推理网关，默认 https://gateway.qoder.com.cn

	// modelMap 客户端名（display_name 规范化）→ 上游 model key；
	// cache 为最近一次成功拉取的完整模型表（无静态兜底，仅此缓存）。
	modelMu  sync.RWMutex
	modelMap map[string]string
	cache    []ModelEntry

	// utMu 保护 uid→userType 实测缓存（登录后/首次请求时填充）。
	utMu      sync.RWMutex
	userTypes map[string]string
}

// New 生产默认。Qoder gateway 对 HTTP/2 不友好（stream INTERNAL_ERROR），强制 HTTP/1.1。
func New() *Client {
	return NewWithTimeout(180 * time.Second)
}

// DefaultIdleTimeout 流式空闲超时默认值。
//
// 实测正常请求 8~40s 完成（同一模型），90s 足够宽松——只拦「真的一个字节都不出」。
const DefaultIdleTimeout = 90 * time.Second

// NewWithTimeout 指定非流式超时；配置连接池。
func NewWithTimeout(timeout time.Duration) *Client {
	return &Client{
		HTTP:       &http.Client{Timeout: timeout, Transport: newTransport()},
		StreamHTTP: &http.Client{Transport: newTransport()},
		Base:       OpenAPIBase,
		Gateway:    GatewayBase,
		modelMap:   map[string]string{},
		userTypes:  map[string]string{},
	}
}

// newTransport 出厂 Transport：禁 h2 + 连接池 + ResponseHeaderTimeout 兜底。
//
// StreamHTTP 无总超时后**必须**有 ResponseHeaderTimeout，否则连响应头都等不到就会无限挂住。
func newTransport() *http.Transport {
	return &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{}, // 强制 HTTP/1.1
		ResponseHeaderTimeout: 60 * time.Second,
	}
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

// NewWithBase 测试用：覆盖 base/gateway。
func NewWithBase(base, gateway string) *Client {
	c := New()
	c.Base = base
	c.Gateway = gateway
	return c
}

// setModelMap 记录动态模型映射（客户端名 → key）。
func (c *Client) setModelMap(m map[string]string) {
	c.modelMu.Lock()
	c.modelMap = m
	c.modelMu.Unlock()
}

// modelKey 客户端模型名 → 上游 model key：动态映射优先；
// 无静态兜底表，未命中时原样返回（上游对未知 key 走兜底，不计 quota —— qoder2api 注释）。
func (c *Client) modelKey(clientName string) string {
	c.modelMu.RLock()
	defer c.modelMu.RUnlock()
	if k := c.modelMap[clientName]; k != "" {
		return k
	}
	return clientName
}

// userTypeOf 返回 uid 的实测 userType；无缓存返回缺省 personal_standard。
func (c *Client) userTypeOf(a *auth.Auth) string {
	c.utMu.RLock()
	defer c.utMu.RUnlock()
	if ut := c.userTypes[a.UID]; ut != "" {
		return ut
	}
	return "personal_standard"
}

// setUserType 记录实测 userType。
func (c *Client) setUserType(uid, ut string) {
	if uid == "" || ut == "" {
		return
	}
	c.utMu.Lock()
	c.userTypes[uid] = ut
	c.utMu.Unlock()
}

// FetchUserInfo 拉取 /api/v1/userinfo 并缓存 userType。
// 返回 (name, userType, error)；name 为用户显示名（邮箱或昵称），
// 供登录流程回填 auth.Nickname（设备流响应里无 nickname）。
func (c *Client) FetchUserInfo(a *auth.Auth) (string, string, error) {
	dt := a.JWT()
	if dt == "" {
		return "", "", fmt.Errorf("no dt- available")
	}
	req, err := http.NewRequest(http.MethodGet, c.Base+EpUserInfo, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+dt)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("userinfo http %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var info struct {
		UserType string `json:"userType"`
		Name     string `json:"name"`
		Email    string `json:"email"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	c.setUserType(a.UID, info.UserType)
	name := info.Name
	if name == "" {
		name = info.Email // 部分账号 name 为空，email 兜底
	}
	log.Printf("qodercn userinfo uid=%s userType=%s name=%s", a.UID, info.UserType, name)
	return name, info.UserType, nil
}

func billingHeaders(req *http.Request, dt string) {
	req.Header.Set("Authorization", "Bearer "+dt)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
}

func (c *Client) get(path, dt string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	billingHeaders(req, dt)
	return c.HTTP.Do(req)
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// ---------------------------------------------------------------------------
// provider.Upstream 实现
// ---------------------------------------------------------------------------

// RefreshToken 用 drt- 走 /api/v1/deviceToken/refresh 轮换 dt/drt。
// 401/403 → ErrSessionDead（需重新 OAuth 登录）。
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	log.Printf("qodercn refresh start uid=%s", a.UID)
	if strings.TrimSpace(a.RefreshToken) == "" {
		err := fmt.Errorf("no drt- available")
		log.Printf("qodercn refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": a.RefreshToken})
	req, err := http.NewRequest(http.MethodPost, c.Base+EpDTRefresh, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// refresh 用独立短超时客户端
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("qodercn refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// drt 失效 → 需重新登录
		err := &provider.Error{Kind: provider.ErrSessionDead, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
		log.Printf("qodercn refresh session-dead uid=%s err=%v", a.UID, err)
		return err
	}
	if resp.StatusCode >= 400 {
		err := fmt.Errorf("deviceToken refresh http %d: %s", resp.StatusCode, truncate(string(raw), 200))
		log.Printf("qodercn refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var out struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"` // ms
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		log.Printf("qodercn refresh parse failed uid=%s err=%v", a.UID, err)
		return fmt.Errorf("deviceToken refresh parse: %w", err)
	}
	dt := out.Token
	if dt == "" {
		dt = out.DeviceToken
	}
	if dt == "" || out.RefreshToken == "" {
		return fmt.Errorf("deviceToken refresh: incomplete token pair")
	}
	a.AccessToken = dt
	a.RefreshToken = out.RefreshToken
	now := time.Now()
	if out.ExpiresIn > 0 {
		a.ExpiresAt = now.Add(time.Duration(out.ExpiresIn) * time.Millisecond).Unix()
	} else if out.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, out.ExpiresAt); err == nil {
			a.ExpiresAt = t.Unix()
		}
	}
	if a.ExpiresAt == 0 {
		a.ExpiresAt = now.Add(30 * 24 * time.Hour).Unix() // dt 观测寿命 30d
	}
	log.Printf("qodercn refresh success uid=%s expires_at=%d", a.UID, a.ExpiresAt)
	return nil
}

// thinkingParam 客户端 thinking 参数：Qoder 系只关心 type（enabled/adaptive/disabled）。
type thinkingParam struct {
	Type string `json:"type"`
}

// reasoningSpecFor 把归一化后的思考控制投影成 QoderCN 的请求字段。
//
// 官方客户端同时写 model_config.is_reasoning、parameters.reasoning_effort、
// parameters.enable_thinking，因此这里返回一个 reasoningSpec 交给 body 层三处一起写，
// 避免出现矛盾态（is_reasoning=true + enable_thinking=false）。
//
// 语义（clientModel 用客户端名，与模型目录的能力表同键；能力来自 RealmQoderCN 面）：
//   - 未表达 → 关，且不下发档位字段（与旧行为一致，避免打扰上游默认档）；
//   - 显式关闭 → 该模型声明可关闭（thinking_config.disabled）时真关
//     （effort=none + enable_thinking=false + is_reasoning=false）；
//     不可关闭时**降到最低档**而不是发 none——上游对不认识的 none 会静默
//     忽略并按其默认档执行，反而偏离客户端「别想太多」的意图；
//     能力未知（目录未拉到）时只关开关，不猜档位；
//   - 指定档位 → 按该模型 ladder 就近降级（reasoning.Caps.Clamp），
//     避免把 high 发给只认 low/medium/xhigh 的模型；
//   - 只说开思考 → 补上游标了 is_default 的档；没有声明则只翻开关。
//
// thinking.type 作为兜底：未经 server 层归一化的直连调用仍可能只带该字段。
func reasoningSpecFor(clientModel, reasoningEffort string, thinking *thinkingParam) reasoningSpec {
	probe := map[string]any{}
	if reasoningEffort != "" {
		probe["reasoning_effort"] = reasoningEffort
	}
	if thinking != nil && thinking.Type != "" {
		probe["thinking"] = map[string]any{"type": thinking.Type}
	}
	control, err := reasoning.Resolve(probe, false)
	if err != nil {
		return reasoningSpec{} // 非法控制已在 server 层拦下；此处兜底按未表达处理
	}
	const realm = reasoning.RealmQoderCN
	switch control.Mode {
	case reasoning.ModeDisabled:
		cap, ok := reasoning.Caps.Lookup(realm, clientModel)
		if !ok {
			return reasoningSpec{}
		}
		if cap.SupportsDisable {
			return reasoningSpec{Enabled: false, Effort: "none"}
		}
		if lowest := reasoning.LowestEffort(cap.Efforts); lowest != "" {
			return reasoningSpec{Enabled: true, Effort: lowest}
		}
		return reasoningSpec{}
	case reasoning.ModeEnabled, reasoning.ModeEffort:
		// 保守策略（让「对外声明」与「实际下发」严格对齐）：能力未知的模型只翻开关、
		// 不下发档位字段——面板与 /v1/models 对未声明 thinking_config 的模型同样不暴露档位
		// （reasoning.ListingForKind 返回空），两端口径一致。
		// ✅ 档位下发已实测：2026-09-22 探针直连真实账号（见 docs/qoderCN渠道接入备忘.md §8 ——
		// low 2206 字 < medium 2502 字 < xhigh 超时截断，上游确接受 parameters.reasoning_effort）；
		// 2026-09-24 再经网关端到端复核（qwen3.8-max：low → rtok 429/429、xhigh → 1082/285、
		// 未表达 → 0）。故「有 thinking_config 声明」的模型档位确被上游采纳；守卫只留给
		// 「无声明」模型（是否像 internal/qoder 那样对未知模型也原样透传，属风格选择而非缺陷）。
		cap, ok := reasoning.Caps.Lookup(realm, clientModel)
		if !ok {
			return reasoningSpec{Enabled: true}
		}
		effort := reasoning.ChatEffort(control)
		if control.Mode == reasoning.ModeEnabled && control.BudgetTokens == nil {
			// 只说开思考：用该模型声明的默认档；没有声明就不下发档位字段。
			effort = cap.DefaultEffort
			if effort == "" {
				return reasoningSpec{Enabled: true}
			}
		}
		if effort != "" {
			effort = reasoning.Caps.Clamp(realm, clientModel, effort)
		}
		return reasoningSpec{Enabled: true, Effort: effort}
	}
	return reasoningSpec{}
}

// ChatStream 发 chat 请求并返回原始嵌套 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、respBody 为上游响应体、err 为 nil；只有传输层失败才返回 err。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	// body 是 server 侧改写后的 OpenAI 请求；qoder 需要 messages/model/tools/max_tokens。
	var reqOpenAI struct {
		Model           string           `json:"model"`
		Messages        []map[string]any `json:"messages"`
		Tools           []any            `json:"tools"`
		ReasoningEffort string           `json:"reasoning_effort"`
		MaxTokens       int              `json:"max_tokens"`
		Thinking        *thinkingParam   `json:"thinking"`
	}
	if err := json.Unmarshal(body, &reqOpenAI); err != nil {
		return nil, 0, nil, fmt.Errorf("parse chat body: %w", err)
	}
	clientName := reqOpenAI.Model
	modelKey := c.modelKey(clientName)
	if modelKey == "" {
		modelKey = clientName
	}

	// 思考控制：server 层（internal/server.prepareChatBody）已把 reasoning_effort /
	// reasoning.effort / thinking.* / enable_thinking 等写法归一化为顶层 reasoning_effort，
	// 这里按官方投影成 is_reasoning + parameters{reasoning_effort, enable_thinking}。
	spec := reasoningSpecFor(clientName, reqOpenAI.ReasoningEffort, reqOpenAI.Thinking)
	log.Printf("qodercn reasoning: client=%q key=%q is_reasoning=%v effort=%q",
		clientName, modelKey, spec.Enabled, spec.Effort)

	// 上下文档位（上游 issue #27）：客户端 context_length/context_window 提示 → 模型档位表
	mc := c.modelEntry(modelKey)
	contextWindow := resolveContextWindow(parseContextWindowHint(body), mc)

	rawBody, err := buildAgentBody(reqOpenAI.Messages, mc, reqOpenAI.Tools, spec, reqOpenAI.MaxTokens, c.userTypeOf(a), contextWindow)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("build qodercn body: %w", err)
	}
	encoded := qoderEncode(rawBody)
	url := c.Gateway + EpChat
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(encoded))
	if err != nil {
		return nil, 0, nil, err
	}
	dt := a.JWT()
	sess, err := NewCosySession(a.MachineID, a.MachineToken, a.MachineType, a.Nickname, a.UID, dt, a.RefreshToken, c.userTypeOf(a))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, encoded, url, a.UID, "text/event-stream", true, modelKey); err != nil {
		return nil, 0, nil, fmt.Errorf("cosy headers: %w", err)
	}
	// 用无总超时的 client：长思考请求不受整请求上限约束（见 Client.StreamHTTP）。
	resp, err := c.streamClient().Do(req)
	if err != nil {
		log.Printf("qodercn chat_stream uid=%s model=%s: transport error: %v", a.UID, modelKey, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		log.Printf("qodercn chat_stream uid=%s model=%s: upstream %d body=%s",
			a.UID, modelKey, resp.StatusCode, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// 空闲看门狗兜底：连续 idleTimeout 读不到字节即判上游卡死并关掉 body。
	// 流式与 stream:false 的聚合路径共用这一层（聚合时由「总时长硬上限」
	// 变为「空闲上限」，长而持续输出的响应不再被误杀）。
	return provider.NewIdleReader(resp.Body, c.idleTimeout()), resp.StatusCode, nil, nil
}

// modelEntry 按 key 找缓存中的模型条目（构造 model_config 全字段用）；找不到返回 nil（auto 兜底）。
func (c *Client) modelEntry(key string) *ModelEntry {
	for i := range c.cache {
		if c.cache[i].Key == key {
			return &c.cache[i]
		}
	}
	return nil
}

// UserResource 查询账号当前可花费积分（基础 + 赠送聚合）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	dt := a.JWT()
	if dt == "" {
		return 0, fmt.Errorf("no dt- available")
	}
	resp, err := c.get(EpQuotaUsage, dt)
	if err != nil {
		return 0, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode >= 400 {
		return 0, c.classifyError(resp.StatusCode, string(raw))
	}
	var q struct {
		UserQuota struct {
			Remaining float64 `json:"remaining"`
		} `json:"userQuota"`
		AddOnQuota struct {
			Remaining float64 `json:"remaining"`
		} `json:"addOnQuota"`
		IsQuotaExceeded bool `json:"isQuotaExceeded"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return 0, fmt.Errorf("quota parse: %w", err)
	}
	return int64(q.UserQuota.Remaining + q.AddOnQuota.Remaining), nil
}

// UserResourceDetail 查询积分明细：userQuota + addOnQuota 两个条目。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	dt := a.JWT()
	if dt == "" {
		return 0, nil, fmt.Errorf("no dt- available")
	}
	resp, err := c.get(EpQuotaUsage, dt)
	if err != nil {
		return 0, nil, err
	}
	raw, err := readBody(resp)
	if err != nil {
		return 0, nil, err
	}
	if resp.StatusCode >= 400 {
		return 0, nil, c.classifyError(resp.StatusCode, string(raw))
	}
	var q struct {
		UserQuota struct {
			Total     float64 `json:"total"`
			Used      float64 `json:"used"`
			Remaining float64 `json:"remaining"`
		} `json:"userQuota"`
		AddOnQuota struct {
			Total     float64 `json:"total"`
			Used      float64 `json:"used"`
			Remaining float64 `json:"remaining"`
		} `json:"addOnQuota"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return 0, nil, fmt.Errorf("quota parse: %w", err)
	}
	total := int64(q.UserQuota.Remaining + q.AddOnQuota.Remaining)
	items := []provider.ResourceItem{
		{Name: "用户套餐", Total: int64(q.UserQuota.Total), Used: int64(q.UserQuota.Used), Remain: int64(q.UserQuota.Remaining), Key: "userQuota", Usable: true},
	}
	if q.AddOnQuota.Total > 0 || q.AddOnQuota.Remaining > 0 {
		items = append(items, provider.ResourceItem{Name: "赠送额度", Total: int64(q.AddOnQuota.Total), Used: int64(q.AddOnQuota.Used), Remain: int64(q.AddOnQuota.Remaining), Key: "addOnQuota", Usable: true})
	}
	return total, items, nil
}

// DailyCheckin 每日签到（campaigns 活动路径）。详见 checkin.go。
// 已签到/无活动均视为成功（幂等），返回 nil；仅网络/401 等才返回 error。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := checkin(a)
	return err
}

// DailyCheckinReport 实现 provider.CheckinReporter：额外返回结构化状态，
// 供调度器区分 no_campaign（需在窗口内重试）与已领取（当日完成）。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	return checkin(a)
}

// Classify 实现 provider.Upstream。
func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }

// Stream 实现 provider.Upstream（嵌套 SSE → 标准 OpenAI SSE 透传）。
// model 为客户端请求的模型名，直接注入每个 chunk（上游恒为 "auto"）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return StreamCapture(w, r, model, nil)
}

// Aggregate 实现 provider.Upstream（嵌套 SSE 聚合）。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	return aggregate(r, model)
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "isquotaexceeded\":true",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) provider.ErrKind {
	if status == http.StatusPaymentRequired {
		return provider.ErrHardCredit
	}
	lower := strings.ToLower(body)
	// TOKEN_EXPIRE 优先于通用 401
	if status == http.StatusUnauthorized && strings.Contains(body, "TOKEN_EXPIRE") {
		return provider.ErrSessionDead
	}
	if status == http.StatusUnauthorized {
		return provider.ErrSessionDead
	}
	// 排队提示（10605 isQueued）：就算外层是 4xx（实测 403），语义也是「等一会儿再来」，
	// 换号可能命中不同队列 ⇒ 独立类别，冷却「号×模型」而不是罚整号（见 provider.Queued）。
	if strings.Contains(body, "10605") || strings.Contains(body, "isQueued") {
		return provider.ErrQueued
	}
	// 账号额度耗尽（qodercom 同族实测 110「Billing daily count exceeded」/ 112 付费墙）：
	// 文案不含 hardMarkers 的 "quota exceeded"，必须显式列码。
	if strings.Contains(body, `"code":"110"`) || strings.Contains(body, `"code":"112"`) ||
		strings.Contains(body, `\"code\":\"110\"`) || strings.Contains(body, `\"code\":\"112\"`) ||
		strings.Contains(body, "pricingUrl") {
		return provider.ErrHardCredit
	}
	// 429 优先于 hardRule：限流 body 高频带 "quota exceeded"。
	if status == http.StatusTooManyRequests {
		return provider.ErrSoftRate
	}
	// 上游以 HTTP 200 包裹的排队/限流拒绝：内嵌 403 + subCode:RESOURCE_LIMIT
	// （业务码 10605，带 retryAfterSeconds/waitTime/serviceAvailable:false）。
	// 必须排在「403 且 body 为空 → ErrWafBlock」与 4xx 兜底之前，否则落 ErrClient
	// ——既不罚账号也不进流错误分类，客户端只看到一条被空闲看门狗掐成的截断流（R108）。
	// 10605 走 provider.CodeMarker 而非裸 Contains：错误文本里的 trace_id/requestId
	// 常含五位数字，裸匹配会把无关失败误判成限流（与 11115 同一教训）。
	if strings.Contains(body, "RESOURCE_LIMIT") ||
		(strings.Contains(body, "serviceAvailable") && provider.CodeMarker(lower, "10605")) {
		return provider.ErrSoftRate
	}
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return provider.ErrHardCredit
		}
	}
	// 非 429 但 body 含限流文案 → 软限流
	if strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "usage limit") || strings.Contains(lower, "请求过于频繁") {
		return provider.ErrSoftRate
	}
	// 上下文超限（11115）是请求级错误：必须排在 404 兜底（→软冷却账号）之前——
	// 换任何账号结果都一样，与账号健康无关（AGENTS.md §6.25，与 internal/upstream 同口径）。
	// 业务码走 provider.CodeMarker：裸 Contains(lower,"11115") 会被 request_id 等任意含这五个
	// 数字的文本误命中，把该罚号的错误原因透传出去。
	if status == http.StatusBadRequest || status == http.StatusNotFound {
		if strings.Contains(lower, "prompt is too long") || provider.CodeMarker(lower, "11115") {
			return provider.ErrPromptTooLong
		}
	}
	if status == http.StatusNotFound {
		return provider.ErrNotFound
	}
	if status >= 500 {
		return provider.ErrServer
	}
	if status >= 400 {
		if strings.Contains(lower, "blocked by security policy") ||
			strings.Contains(lower, "unapproved channel") ||
			strings.Contains(lower, "illegal api invocation") {
			return provider.ErrContentBlocked
		}
		if status == 403 && strings.TrimSpace(body) == "" {
			return provider.ErrWafBlock
		}
		if strings.Contains(lower, "request illegal") || strings.Contains(lower, "trial not activated") {
			return provider.ErrAccountFault
		}
		return provider.ErrClient
	}
	return provider.ErrNone
}

// classifyError 转成 provider.Error。
func (c *Client) classifyError(status int, body string) error {
	return &provider.Error{Kind: Classify(status, body), Status: status, Msg: truncate(body, 200)}
}

// EnsureFingerprint 为账号生成/保持 COSY 机器指纹（MachineID/Token/Type）。
// 幂等：已有则保留。与 qoder2api 的稳定派生不同，沿用随机生成 + 持久化
// （凭据文件即存储，跨重启稳定）。
func EnsureFingerprint(a *auth.Auth) {
	if a.MachineID == "" {
		a.MachineID = uuid4()
	}
	if a.MachineToken == "" {
		seed := []byte(uuid4() + uuid4())
		if len(seed) > 50 {
			seed = seed[:50]
		}
		a.MachineToken = base64.RawURLEncoding.EncodeToString(seed)
	}
	if a.MachineType == "" {
		a.MachineType = strings.ReplaceAll(uuid4(), "-", "")[:18]
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
