// client.go QoderCOM 上游客户端：业务 API（dt- Bearer）+
// COSY 签名对话转发 + 错误分类，实现 provider.Upstream 接口。
// 代码级复制自 internal/qodercn/client.go 后改 COM 域名表：
//   - 登录/刷新后从 /api/v1/userinfo 实测 userType（identity 签名用实测值）
//   - 模型路由仅用「上次成功拉取的动态表」缓存（无静态兜底）
//   - 签到：仅 campaigns 活动路径（COM 无 legacy daily-check-in，实测 404）
package qodercom

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
)

// Client QoderCOM 上游客户端。
type Client struct {
	HTTP       *http.Client
	Base       string // 业务 API，默认 https://openapi.qoder.sh
	Gateway    string // 推理网关，默认 https://api1.qoder.sh
	ModelsBase string // 模型列表，默认 https://api2.qoder.sh

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

// NewWithTimeout 指定上游 HTTP 超时；配置连接池。
func NewWithTimeout(timeout time.Duration) *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{}, // 强制 HTTP/1.1
	}
	return &Client{
		HTTP:       &http.Client{Timeout: timeout, Transport: tr},
		Base:       OpenAPIBase,
		Gateway:    GatewayBase,
		ModelsBase: ModelsBase,
		modelMap:   map[string]string{},
		userTypes:  map[string]string{},
	}
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
		name = info.Email // 部分账号 name 为空，email 兑底
	}
	log.Printf("qodercom userinfo uid=%s userType=%s name=%s", a.UID, info.UserType, name)
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

func (c *Client) post(path, dt string, body []byte) (*http.Response, error) {
	if body == nil {
		body = []byte("{}")
	}
	req, err := http.NewRequest(http.MethodPost, c.Base+path, bytes.NewReader(body))
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
	log.Printf("qodercom refresh start uid=%s", a.UID)
	if strings.TrimSpace(a.RefreshToken) == "" {
		err := fmt.Errorf("no drt- available")
		log.Printf("qodercom refresh failed uid=%s err=%v", a.UID, err)
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
		log.Printf("qodercom refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		// drt 失效 → 需重新登录
		err := &provider.Error{Kind: provider.ErrSessionDead, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
		log.Printf("qodercom refresh session-dead uid=%s err=%v", a.UID, err)
		return err
	}
	if resp.StatusCode >= 400 {
		err := fmt.Errorf("deviceToken refresh http %d: %s", resp.StatusCode, truncate(string(raw), 200))
		log.Printf("qodercom refresh failed uid=%s err=%v", a.UID, err)
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
		log.Printf("qodercom refresh parse failed uid=%s err=%v", a.UID, err)
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
	log.Printf("qodercom refresh success uid=%s expires_at=%d", a.UID, a.ExpiresAt)
	return nil
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
		Thinking        *struct {
			Type string `json:"type"`
		} `json:"thinking"`
	}
	if err := json.Unmarshal(body, &reqOpenAI); err != nil {
		return nil, 0, nil, fmt.Errorf("parse chat body: %w", err)
	}
	clientName := reqOpenAI.Model
	modelKey := c.modelKey(clientName)
	if modelKey == "" {
		modelKey = clientName
	}

	// 思考开关：reasoning_effort 或 thinking:{type:"enabled"} → 启用推理
	enableReasoning := false
	if reqOpenAI.ReasoningEffort != "" {
		enableReasoning = true
	} else if reqOpenAI.Thinking != nil && reqOpenAI.Thinking.Type == "enabled" {
		enableReasoning = true
	}

	// 上下文档位（issue #27）：客户端 context_length/context_window 提示 → 模型默认档
	mc := c.modelEntry(modelKey)
	contextWindow := resolveContextWindow(parseContextWindowHint(body), mc)

	rawBody, err := buildAgentBody(reqOpenAI.Messages, mc, reqOpenAI.Tools, enableReasoning, reqOpenAI.MaxTokens, c.userTypeOf(a), contextWindow)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("build qodercom body: %w", err)
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
	resp, err := c.HTTP.Do(req)
	if err != nil {
		log.Printf("qodercom chat_stream uid=%s model=%s: transport error: %v", a.UID, modelKey, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		log.Printf("qodercom chat_stream uid=%s model=%s: upstream %d body=%s",
			a.UID, modelKey, resp.StatusCode, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
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

// DailyCheckin 每日签到（仅 campaigns 活动路径；COM 无 daily-check-in）。
// 详见 checkin.go。已签到/无活动均视为成功（幂等），返回 nil。
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
	// 429 优先于 hardRule：限流 body 高频带 "quota exceeded"。
	if status == http.StatusTooManyRequests {
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
		if (status == 400 || status == 404) &&
			(strings.Contains(lower, "prompt is too long") || strings.Contains(lower, "11115")) {
			return provider.ErrPromptTooLong
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
