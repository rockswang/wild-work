// models.go 模型目录与倍率：解析 /v2/enterprises/personal/models，
// 抽出 cli agent 的模型列表（该接口同时返回 models[] 与 agents[]，
// cli agent 的 models 字段才是「可用于 CLI 的模型」权威顺序）。
package workbuddyai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// catalogModel 目录原始模型条目。
// 能力字段来自上游实测返回：supportsImages/supportsReasoning/supportsToolCall
// 与 disabledMultimodal（true 表示上游显式关闭多模态）。
type catalogModel struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Credits         string   `json:"credits"`
	MaxInputTokens  int64    `json:"maxInputTokens"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	Disabled        bool     `json:"disabled"`
	Tags            []string `json:"tags"`

	SupportsImages     bool `json:"supportsImages"`
	SupportsReasoning  bool `json:"supportsReasoning"`
	SupportsToolCall   bool `json:"supportsToolCall"`
	DisabledMultimodal bool `json:"disabledMultimodal"`
	// Reasoning 档位能力元数据（远端权威；缺失时由 internal/reasoning 静态表兜底）。
	Reasoning modelReasoningMeta `json:"reasoning"`
}

// modelReasoningMeta 与国内版同形（字段名以国际版目录实测返回为准）。
type modelReasoningMeta struct {
	Effort           string   `json:"effort"`
	Summary          string   `json:"summary"`
	DefaultEffort    string   `json:"defaultEffort"`
	SupportedEfforts []string `json:"supportedEfforts"`
}

// imageOK 判定该模型可否接收图像输入：
// 上游显式声明 supportsImages 且未被 disabledMultimodal 关闭。
func (m catalogModel) imageOK() bool {
	return m.SupportsImages && !m.DisabledMultimodal
}

// catalogResp 目录响应（只取所需字段）。
type catalogResp struct {
	Code int `json:"code"`
	Data struct {
		Models []catalogModel `json:"models"`
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
		ModelPromotions []struct {
			Enabled  bool     `json:"enabled"`
			Model    []string `json:"modelIds"`
			Discount struct {
				Factor float64 `json:"factor"`
			} `json:"discount"`
			Badge struct {
				Label string `json:"label"`
				Color string `json:"color"`
			} `json:"badge"`
		} `json:"modelPromotions"`
	} `json:"data"`
}

// catalogCache 目录缓存：FetchModels 与 FetchModelPricing 共用，避免重复请求。
// v3 与 v2 分别缓存（两路独立探测、独立容错）。
type catalogCache struct {
	mu      sync.RWMutex
	models  []catalogModel
	promo   map[string]float64
	badges  map[string]string
	colors  map[string]string
	fetched time.Time

	v3      []catalogModel // /v3/config 全量模型面（不过滤）
	v3OK    bool           // v3 探测是否成功过（失败不写缓存，避免负缓存住坏结果）
	v3Fetched time.Time
}

const catalogTTL = 10 * time.Minute

var cache catalogCache

// fetchCatalog 拉取目录（带 10 分钟缓存），返回 cli agent 过滤后的模型列表。
func (c *Client) fetchCatalog(a *auth.Auth) ([]catalogModel, error) {
	cache.mu.RLock()
	if len(cache.models) > 0 && time.Since(cache.fetched) < catalogTTL {
		out := cache.models
		cache.mu.RUnlock()
		return out, nil
	}
	cache.mu.RUnlock()

	req, err := http.NewRequest(http.MethodGet, c.base()+EpCatalog, nil)
	if err != nil {
		return nil, err
	}
	commonHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env catalogResp
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}

	// cli agent 的模型 ID 顺序为准；models[] 提供元数据。
	var cliIDs []string
	for _, ag := range env.Data.Agents {
		if ag.Name == "cli" {
			cliIDs = ag.Models
			break
		}
	}
	if len(cliIDs) == 0 {
		return nil, fmt.Errorf("models api: no cli agent models")
	}
	byID := make(map[string]catalogModel, len(env.Data.Models))
	for _, m := range env.Data.Models {
		byID[m.ID] = m
	}
	out := make([]catalogModel, 0, len(cliIDs))
	for _, id := range cliIDs {
		m, ok := byID[id]
		if !ok {
			m = catalogModel{ID: id, Name: id}
		}
		if m.Name == "" {
			m.Name = id
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api: empty cli list")
	}

	// 促销折扣：factor==0 表示当前免费；badge 供费率面板备注。
	promo := make(map[string]float64)
	badges := make(map[string]string)
	colors := make(map[string]string)
	for _, p := range env.Data.ModelPromotions {
		if !p.Enabled {
			continue
		}
		for _, id := range p.Model {
			promo[id] = p.Discount.Factor
			if p.Badge.Label != "" {
				badges[id] = p.Badge.Label
			}
			if p.Badge.Color != "" {
				colors[id] = p.Badge.Color
			}
		}
	}
	_ = badges

	cache.mu.Lock()
	cache.models, cache.promo, cache.badges, cache.colors, cache.fetched = out, promo, badges, colors, time.Now()
	cache.mu.Unlock()
	return out, nil
}

// fetchV3Catalog 抓取 /v3/config 全量模型面（2026-09-28 实测，issue #39）。
//
// 与 v2 的三点差异：
//   - 模型为**全量面**（22 个 vs cli 面 18 个），不经 agents[cli] 过滤——
//     extraModels 里的 deepseek-v4.1-flash/gpt-6-astra/kimi-k2.8-preview 只在这里有；
//   - **UA 门禁**：web UA → 400 code=12403 "check ua"，必须带 CLI 形 UA（clientUA 即可）；
//   - credits 可能为空串（如 default-model），合并时以「非空者优先」。
//
// 失败不影响 v2：两路独立容错，v3 挂了退化为原有行为（issue #39 的 unknown 依旧）。
func (c *Client) fetchV3Catalog(a *auth.Auth) ([]catalogModel, error) {
	cache.mu.RLock()
	if cache.v3OK && len(cache.v3) > 0 && time.Since(cache.v3Fetched) < catalogTTL {
		out := cache.v3
		cache.mu.RUnlock()
		return out, nil
	}
	cache.mu.RUnlock()

	req, err := http.NewRequest(http.MethodGet, c.base()+EpCatalogV3, nil)
	if err != nil {
		return nil, err
	}
	commonHeaders(req, a)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	// UA 门禁：commonHeaders 已设 clientUA（CLI 形），实测可通过；
	// 此处显式再设一次以防调用方改写（12403 的教训）。
	req.Header.Set("User-Agent", clientUA)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("v3 config status %d: %s", resp.StatusCode, truncate(string(raw), 120))
	}
	var env catalogResp
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("v3 config parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("v3 config code=%d", env.Code)
	}
	if len(env.Data.Models) == 0 {
		return nil, fmt.Errorf("v3 config empty models")
	}

	cache.mu.Lock()
	cache.v3, cache.v3OK, cache.v3Fetched = env.Data.Models, true, time.Now()
	cache.mu.Unlock()
	return env.Data.Models, nil
}

// mergeV3 合并 v3 目录：v2 为主序与主数据源，v3 只补 v2 缺失的 id；
// 对两边都有的模型，v3 的 credits 非空时**覆盖** v2（v3 面更新，如 hy4-preview）。
//
// 返回 (合并后的模型表, v3 倍率表)。失败（v3 不可用）时返回 (v2 原样, nil)，
// 调用方无需感知 v3 是否参与——这正是「尽力增强，不拖累主路」的容错语义。
func mergeV3(v2 []catalogModel, v3 []catalogModel) ([]catalogModel, map[string]string) {
	if len(v3) == 0 {
		return v2, nil
	}
	byID := make(map[string]catalogModel, len(v3))
	for _, m := range v3 {
		byID[m.ID] = m
	}
	out := make([]catalogModel, 0, len(v2)+len(v3))
	seen := make(map[string]bool, len(v2)+len(v3))
	for _, m := range v2 {
		if vm, ok := byID[m.ID]; ok {
			if vm.Credits != "" {
				m.Credits = vm.Credits // v3 的倍率面更新
			}
			// 能力/上下文字段以 v3 为准（字段更全：maxAllowedSize/vendor/描述）
			if vm.MaxInputTokens > 0 {
				m.MaxInputTokens = vm.MaxInputTokens
			}
			if vm.MaxOutputTokens > 0 {
				m.MaxOutputTokens = vm.MaxOutputTokens
			}
			m.SupportsImages = vm.SupportsImages || m.SupportsImages
			m.SupportsToolCall = vm.SupportsToolCall || m.SupportsToolCall
		}
		if m.ID == "" || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	// v3 独有：追加在尾部（issue #39 的主角正是这批）
	for _, m := range v3 {
		if seen[m.ID] || m.ID == "" {
			continue
		}
		seen[m.ID] = true
		out = append(out, m)
	}
	rates := make(map[string]string, len(v3))
	for _, m := range v3 {
		if m.Credits != "" {
			rates[m.ID] = m.Credits
		}
	}
	return out, rates
}

// fetchPromotions 返回促销折扣表（factor==0 → 免费）。
func (c *Client) fetchPromotions(a *auth.Auth) map[string]float64 {
	if _, err := c.fetchCatalog(a); err != nil {
		return nil
	}
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	out := make(map[string]float64, len(cache.promo))
	for k, v := range cache.promo {
		out[k] = v
	}
	return out
}

// badgeOf 取模型促销标签（如 "Free now"）。
func badgeOf(id string) string {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.badges[id]
}

// colorOf 取模型促销标签的颜色（如 "#FF0000"），无则空。
func colorOf(id string) string {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.colors[id]
}

// ---------------------------------------------------------------------------
// 登录（与国内版路径相同，仅 host 不同）
// ---------------------------------------------------------------------------

// StartLogin 发起登录：POST auth/state 拿 state+授权 URL。
func (c *Client) StartLogin() (state, authURL string, err error) {
	req, err := http.NewRequest(http.MethodPost, c.base()+EpAuthState, strings.NewReader("{}"))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", originRef)
	req.Header.Set("Referer", originRef+"/")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("X-No-Authorization", "true")
	req.Header.Set("X-No-User-Id", "true")
	req.Header.Set("X-No-Enterprise-Id", "true")
	req.Header.Set("X-No-Department-Info", "true")
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", fmt.Errorf("auth state failed: %w", err)
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", "", fmt.Errorf("auth state: missing state or authUrl")
	}
	return st.State, st.AuthURL, nil
}

// PollToken 轮询登录 token。未完成时返回 nil, nil（对应上游 code=11217）。
// 成功时返回凭据字段。
type LoginResult struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Domain       string
	UID          string
	EnterpriseID string
	Nickname     string
}

// PollLogin 单次轮询；pending 为 true 表示浏览器尚未完成。
func (c *Client) PollLogin(state string) (r LoginResult, pending bool, err error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+EpAuthToken+state, nil)
	if err != nil {
		return r, false, err
	}
	commonHeaders(req, nil)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-No-Authorization", "true")
	req.Header.Set("X-No-User-Id", "true")
	req.Header.Set("X-No-Enterprise-Id", "true")
	req.Header.Set("X-No-Department-Info", "true")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return r, false, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env apiEnvelope
	if json.Unmarshal(raw, &env) != nil {
		return r, false, fmt.Errorf("token parse failed: %s", truncate(string(raw), 120))
	}
	if env.Code == 11217 {
		return r, true, nil // login ing...
	}
	if env.Code != 0 {
		return r, false, fmt.Errorf("code=%d msg=%s", env.Code, truncate(env.Msg, 160))
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
	}
	if err := json.Unmarshal(env.Data, &tok); err != nil || tok.AccessToken == "" {
		return r, true, nil // 尚未返回可用 token，视为 pending
	}
	r.AccessToken, r.RefreshToken, r.ExpiresIn = tok.AccessToken, tok.RefreshToken, tok.ExpiresIn
	r.Domain = tok.Domain
	// 取账号信息（带 Bearer）；失败不影响登录结果。
	acct := c.fetchAccount(state, tok.AccessToken)
	r.UID, r.EnterpriseID, r.Nickname = acct[0], acct[1], acct[2]
	return r, false, nil
}

// fetchAccount GET login/account 取 uid/enterpriseId/nickname。
func (c *Client) fetchAccount(state, accessToken string) [3]string {
	req, err := http.NewRequest(http.MethodGet, c.base()+EpLoginAcct+state, nil)
	if err != nil {
		return [3]string{}
	}
	commonHeaders(req, nil)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	data, err := c.doJSON(req)
	if err != nil {
		return [3]string{}
	}
	var acct struct {
		UID          string `json:"uid"`
		EnterpriseID string `json:"enterpriseId"`
		Nickname     string `json:"nickname"`
	}
	_ = json.Unmarshal(data, &acct)
	return [3]string{acct.UID, acct.EnterpriseID, acct.Nickname}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

// hardMarkers 余额/权益不足关键词（小写比较 + 中文原文双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// sessionDeadMarkers 会话失效标记（需重新登录）。
var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// Classify 按 HTTP 状态码 + body 判定错误类别（与国内版分类语义一致）。
func Classify(status int, body string) provider.ErrKind {
	if status == http.StatusPaymentRequired {
		return provider.ErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range sessionDeadMarkers {
		if strings.Contains(body, m) {
			return provider.ErrSessionDead
		}
	}
	// 429 优先于 hardRule：限流 body 高频带 "quota exceeded"。
	if status == http.StatusTooManyRequests {
		// 上游按**模型**限流（6004 / "switch to the other models"）时仍返回 ErrSoftRate，
		// 由 handler 按 provider.IsModelScopedSoftRate(body) 分流成 (账号, 模型) 粒度冷却：
		// 只让该账号在这个模型上不可选，账号上其它模型照常路由，且下一次请求能自动换号
		// （见 R47）。若改判 ErrPassthrough 则完全不冷却，同一账号会被反复选中、反复撞 429。
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
