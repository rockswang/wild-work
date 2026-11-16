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
	// reasoning 档位声明（issue #96，形状对齐 dsh-workbuddyai-connect 实测类型）。
	Reasoning *catalogReasoning `json:"reasoning"`
}

// catalogReasoning 目录 reasoning 对象（仅取可透传的权威字段；与 upstream 包同形状）。
type catalogReasoning struct {
	SupportedEfforts []string `json:"supportedEfforts"`
	DefaultEffort    string   `json:"defaultEffort"`
	// Effort 老式默认档字段（deepseek 系实测只有它没有列表），DefaultEffort 空时回退。
	Effort             string `json:"effort"`
	CanDisableThinking bool   `json:"canDisableThinking"`
}

// hasReasoningMeta 报告 reasoning 对象是否带任何档位信息（全空 = 视为未声明）。
func (r *catalogReasoning) hasReasoningMeta() bool {
	if r == nil {
		return false
	}
	return len(provider.CleanEfforts(r.SupportedEfforts)) > 0 ||
		strings.TrimSpace(r.DefaultEffort) != "" || strings.TrimSpace(r.Effort) != ""
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

	// v2Down 记录 /v2/enterprises/personal/models 已不可用（2026-11-14 起实测
	// 404 Route Not Found，三个账号一致），此后直接走 v3，避免每轮刷新先撞一次 404。
	// 到期后重探一次：上游若恢复 v2，自动切回（v2 有 v3 缺的能力字段）。
	v2Down    bool
	v2RetryAt time.Time

	v3        []catalogModel // /v3/config 全量模型面（不过滤）
	v3OK      bool           // v3 探测是否成功过（失败不写缓存，避免负缓存住坏结果）
	v3Fetched time.Time
}

const catalogTTL = 10 * time.Minute

// v2RetryInterval 判定 v2 下线后，多久再重探一次。
const v2RetryInterval = 30 * time.Minute

var cache catalogCache

// fetchCatalog 拉取目录（带 10 分钟缓存），返回 cli agent 过滤后的模型列表。
//
// 主源是 /v2/enterprises/personal/models；它 404 时（2026-11-14 起上游已下线，
// 见 v2Down 注释）自动改用 /v3/config —— 两者 data 结构同形（models / agents /
// modelPromotions 字段名一致），解析逻辑可完全共用。
func (c *Client) fetchCatalog(a *auth.Auth) ([]catalogModel, error) {
	cache.mu.RLock()
	if len(cache.models) > 0 && time.Since(cache.fetched) < catalogTTL {
		out := cache.models
		cache.mu.RUnlock()
		return out, nil
	}
	cache.mu.RUnlock()

	env, fromV3, err := c.fetchCatalogEnv(a)
	if err != nil {
		return nil, err
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
	// v3 兜底时把同一份响应灌进 v3 缓存，FetchModels/FetchModelPricing 的
	// mergeV3 就不会再打一次同一个端点。
	if fromV3 {
		cache.v3, cache.v3OK, cache.v3Fetched = env.Data.Models, true, time.Now()
	}
	cache.mu.Unlock()
	return out, nil
}

// fetchCatalogEnv 请求目录接口：v2 优先，404 后切 v3（并记住，避免每轮先撞 404）。
// 其它错误（网络、401）原样返回，不做源切换 —— 那类失败换端点救不回来。
func (c *Client) fetchCatalogEnv(a *auth.Auth) (catalogResp, bool, error) {
	cache.mu.RLock()
	down, retryAt := cache.v2Down, cache.v2RetryAt
	cache.mu.RUnlock()

	if !down || time.Now().After(retryAt) {
		env, status, err := c.getCatalog(a, EpCatalog, false)
		switch {
		case err == nil:
			if down {
				cache.mu.Lock()
				cache.v2Down, cache.v2RetryAt = false, time.Time{}
				cache.mu.Unlock()
			}
			return env, false, nil
		case status == http.StatusNotFound:
			cache.mu.Lock()
			cache.v2Down, cache.v2RetryAt = true, time.Now().Add(v2RetryInterval)
			cache.mu.Unlock()
		default:
			return catalogResp{}, false, err
		}
	}

	env, _, err := c.getCatalog(a, EpCatalogV3, true)
	if err != nil {
		return catalogResp{}, false, err
	}
	return env, true, nil
}

// getCatalog 请求一个目录端点并解析外层信封。status 供调用方判 404。
func (c *Client) getCatalog(a *auth.Auth, path string, cliUA bool) (catalogResp, int, error) {
	req, err := http.NewRequest(http.MethodGet, c.base()+path, nil)
	if err != nil {
		return catalogResp{}, 0, err
	}
	commonHeaders(req)
	req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	req.Header.Set("Accept", "application/json")
	if cliUA {
		// /v3/config 有 UA 门禁：web UA → 400 code=12403 "check ua"。
		req.Header.Set("User-Agent", clientUA)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return catalogResp{}, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		// 类型化错误：上层靠 errors.As 取分类才能 refresh + 重试（不变量 19），
		// 裸 fmt.Errorf 会让模型/费率面的 401 自愈永远不生效（R79）。
		label := "models api"
		if path == EpCatalogV3 {
			label = "v3 config"
		}
		return catalogResp{}, resp.StatusCode, &provider.Error{
			Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: fmt.Sprintf("%s: %s", label, truncate(string(raw), 200))}
	}
	var env catalogResp
	if err := json.Unmarshal(raw, &env); err != nil {
		return catalogResp{}, resp.StatusCode, fmt.Errorf("%s parse: %w", path, err)
	}
	if env.Code != 0 {
		return catalogResp{}, resp.StatusCode, fmt.Errorf("%s code=%d", path, env.Code)
	}
	return env, resp.StatusCode, nil
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
	commonHeaders(req)
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
		// 同上：类型化错误（R79）。
		return nil, &provider.Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: fmt.Sprintf("v3 config: %s", truncate(string(raw), 200))}
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
			// 思考档位：v3 带则覆盖，v3 无则保留 v2（同「非空者优先」口径）
			if vm.Reasoning.hasReasoningMeta() {
				m.Reasoning = vm.Reasoning
			}
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
	commonHeaders(req)
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
	commonHeaders(req)
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
		// 429（含按**模型**限额的 6004「usage exceeds frequency limit / switch to the
		// other models」）一律软限流：handler 对 ErrSoftRate 走 CooldownModel，只冷却
		// 「账号×该模型」到响应体重置点，同号其他模型不受牵连（issue #53 的担忧已由
		// 模型级冷却化解），模型全冷时降级链自动顶替。历史上曾把 6004 归为
		// ErrPassthrough 纯透传（当时只有账号级冷却这一种手段），但透传什么都不冷却，
		// 降级链永不触发，客户端只能对着重置时间干等。
		// 例外：积分耗尽（14018，msg 是复数 "Credits exhausted"——与 hardMarkers 的
		// "credit exhausted" 差一个 s，429 分支在 hardMarkers 之前直接 return 时永远
		// 漏匹配）是账号级余额问题而非限流：归软限流只冷却「账号×该模型」60s（body
		// 无重置时间戳，回退 SoftCooldown），到期容量"恢复"，降级链永不触发，客户端
		// 死循环吃 429（2026-09-23 起生产实录）。归 ErrHardCredit：账号级 12h 硬冷却
		// （充值后 ReenableIfCredits 自动解冻），下一请求入口降级链正常顶替。
		// 匹配防前缀误伤："code":140180 含子串 "code":14018——命中后下一字符须非
		// 数字，防未来 14xxxx 新码误归 12h 账号硬冷却。
		if idx := strings.Index(body, `"code":14018`); idx >= 0 {
			if end := idx + len(`"code":14018`); end >= len(body) || body[end] < '0' || body[end] > '9' {
				return provider.ErrHardCredit
			}
		}
		if strings.Contains(lower, "credits exhausted") {
			return provider.ErrHardCredit
		}
		return provider.ErrSoftRate
	}
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return provider.ErrHardCredit
		}
	}
	// 非 429 但 body 含限流文案 → 软限流（含「switch to the other models」的模型级
	// 限额同此：handler 走 CooldownModel 冷却到重置点，不罚账号）。
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
		// 会话头族缺失（11128 "conversation id missing"）是上游按账号灰度的渠道校验，
		// 属账号级问题（补头后仍撞 ⇒ 该号被灰度到别的口径）：软冷却 + 换号。必须排在
		// contentBlocked 之前——同码的另一形态 "Illegal API invocation from an unapproved
		// channel" 才是请求级指纹问题（改提示词就能过，不罚号）。R116。
		if strings.Contains(lower, "conversation id missing") {
			return provider.ErrAccountFault
		}
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
