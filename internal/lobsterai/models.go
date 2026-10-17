// models.go LobsterAI 的模型目录：动态解析（协议 §6）+ 静态兜底表。
//
// 两条路径**只共用输出形状，不共用归一规则**（协议 §6 首行）：
//   - 动态：GET {APIBase}/api/models/available + keyfrom 身份 query，
//     响应兼容单层 `{code:0,data:[...]}` 与双层 `{code:0,data:{data:[...]}}`；
//   - 静态：19 条兜底表，contextWindow 统一 131072（**本地估计值**，非远端权威值）。
//
// 三条纪律（都有明确协议出处）：
//  1. **目录门控**：无已登录账号时返回 `[]` 且**不抛错**（协议 §6 末段）——
//     面板据此隐藏整个渠道分组，而不是把「没登录」显示成「渠道故障」；
//  2. **可选字段缺失一律留未知，绝不填 0/false 假值**（协议 §6「取值纪律」）；
//  3. **兜底只在远端整体不可用时用**：远端可用时完全采信远端，不做裁剪合并。
package lobsterai

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// errNoAccount 本地哨兵：没有可用凭据（目录门控），不是上游故障。
var errNoAccount = errors.New("lobsterai：账号没有 access_token")

// entry 归一化后的模型条目；动态目录与静态兜底共用同一输出形状。
type entry struct {
	// ID 上游模型 id，直接作为发给上游的 model 值。
	ID string
	// Name 展示名。协议 §6：展示名是 `名称 · x{倍率}`（倍率只在 > 0 时拼），
	// 写进 name 而不是 description。
	Name string
	// ContextWindow 上下文窗口；**0 = 未知**（不是合法窗口，消费方必须判 > 0）。
	ContextWindow int64
	// MaxTokens 输出上限；**0 = 上游未声明**（不得伪造成 0 值字段）。
	MaxTokens int64
	// SupportsImage 上游显式 true 才为真（缺失 = 未知，不填 false 假值）。
	SupportsImage bool
	// SupportsThinking 上游显式 true 才为真。
	SupportsThinking bool
	// Multiplier costMultiplier（**裸数字**，实测 0.05 / 1.08 / 20）。
	// HasMultiplier=false 表示上游没给该字段（≠ 倍率 1、≠ 免费）。
	Multiplier    float64
	HasMultiplier bool
	// Efforts 上游声明的思考档位表（**wire 值**，去重保序）。
	// nil = 上游未声明档位（消费方不得自行编造，R49）。
	Efforts []string
	// DefaultEffort 上游声明的默认档位（wire 值），且必须落在 options 内。
	DefaultEffort string
	// CanDisableThinking 上游是否声明可关闭思考；nil = 未声明（保留三态）。
	CanDisableThinking *bool
	// Estimated 标记该条目的容量是**本地估计值**（静态兜底表）：
	// 必须让 ModelInfo.ContextFromAPI=false，免得把估值冒充成上游接口返回值。
	Estimated bool
}

// ---------------------------------------------------------------------------
// 静态兜底表（协议 §6）
// ---------------------------------------------------------------------------

// staticCatalog 静态兜底 19 条（id 清单见 constants.go 的 staticModelIDs，
// 逐字照抄协议 §6）。
//
// 为什么 contextWindow 是本地估计值而不是「上游声明」：协议 §6 明确写了
// 「静态兜底表 19 条，contextWindow 统一 131072（本地估计值，**非**远端权威值）」。
// 故这些条目的 ModelInfo.ContextFromAPI=false。
//
// 展示名取 id 本身：协议只给出了 id 清单，没有给出配套展示名（`lobsterai-product.ts:40-52`
// 未收录进文档）——**不编造**展示名，用 id 兜住面板可用性。
//
// ⚠️ 兜底表不含任何能力/档位声明：远端不可用时我们**不知道**这些模型的
// supportsImage / supportsThinking / 档位，按 R49「只透传上游声明」留空。
func staticCatalog() []entry {
	out := make([]entry, 0, len(staticModelIDs))
	for _, id := range staticModelIDs {
		out = append(out, entry{
			ID:            id,
			Name:          id,
			ContextWindow: staticContextWindow,
			Estimated:     true,
		})
	}
	return out
}

// StaticModels 静态兜底模型表（装配时注入 Runtime.StaticModels，面板无凭据时也有列表）。
func StaticModels() []provider.ModelInfo { return modelInfosOf(staticCatalog()) }

// ---------------------------------------------------------------------------
// 远端目录请求
// ---------------------------------------------------------------------------

// keyfromQuery 构造 keyfrom 身份载荷的 query（协议 §1.3）。
//
// ⚠️ 两个易错点：
//   - 键名是**驼峰**（firstKeyfrom / latestKeyfrom / userId），而凭据里存的是
//     snake_case（first_keyfrom / user_id）——两套命名，别混；
//   - **空值键不发送**：上游按「键存在」判定身份，发空串等于发一个伪身份。
//     故这里只在取值非空时才 Set。
//
// 载荷**不含 refreshToken**（协议 §1.3 加粗强调，`:302-305` 有说明）：
// 模型列表是数据面请求，凭据本体不进 query（query 会进日志/代理缓存）。
func (c *Client) keyfromQuery(a *auth.Auth) url.Values {
	q := url.Values{}
	// version 恒有值（取不到真值时有兜底常量），故总是发送。
	q.Set("version", c.ClientVersion())
	if v := extraOf(a, ExtraFirstKeyfrom); v != "" {
		q.Set("firstKeyfrom", v)
	}
	if v := extraOf(a, ExtraLatestKeyfrom); v != "" {
		q.Set("latestKeyfrom", v)
	}
	if v := extraOf(a, ExtraUUID); v != "" {
		q.Set("uuid", v)
	}
	if v := extraOf(a, ExtraUserID); v != "" {
		q.Set("userId", v)
	}
	return q
}

// modelsURL 目录接口的完整 URL（query 是上游要求的定位参数，不是装饰）。
func (c *Client) modelsURL(a *auth.Auth) string {
	return c.base() + EpModelsAvailable + "?" + c.keyfromQuery(a).Encode()
}

// fetchCatalog 拉取并解析远端目录。返回 errNoAccount 表示「本地无凭据」（目录门控）。
func (c *Client) fetchCatalog(a *auth.Auth) ([]entry, error) {
	tok := tokenOf(a)
	if tok == "" {
		return nil, errNoAccount
	}
	req, err := http.NewRequest(http.MethodGet, c.modelsURL(a), nil)
	if err != nil {
		return nil, err
	}
	// 模型列表的头集合（协议 §2.1）：与对话同源，但 Accept 是 application/json，
	// 且两个 X-LobsterAI-Client-* 头**必需**（缺 kimi-k3-agentic-v1 会少一个模型）。
	c.authHeaders(req, tok, "application/json", true)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("lobsterai：目录接口 HTTP %d：%s", resp.StatusCode, snippet(raw))
	}
	return parseCatalog(raw)
}

// parseCatalog 解析目录响应（协议 §6）。
//
// 形状不符（含 code!==0、data 既不是数组也不是带 data 数组的对象）→ 返回错误，
// 由 FetchModels 走静态兜底——协议写的是「→ 空数组」，两种说法在调用方**等价**
// （空数组同样触发兜底），这里保留错误以便日志能说清原因。
func parseCatalog(raw []byte) ([]entry, error) {
	var top struct {
		Code int             `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, fmt.Errorf("lobsterai：目录响应不是 JSON 对象：%w", err)
	}
	if top.Code != 0 {
		return nil, fmt.Errorf("lobsterai：目录接口业务码 %d %s", top.Code, strings.TrimSpace(top.Msg))
	}
	items, err := modelItems(top.Data)
	if err != nil {
		return nil, err
	}
	out := make([]entry, 0, len(items))
	for _, it := range items {
		// modelId 是必填：**空则跳过**（协议 §6「modelId（必填，空则跳过）」）。
		// 不跳过就会产出一条 id 为空的模型，路由时直接串味。
		if e, ok := normalizeEntry(it); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// modelItems 兼容两种信封（协议 §6）：`data:[...]`（单层）与 `data:{data:[...]}`（双层）。
func modelItems(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("lobsterai：目录响应没有 data 字段")
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var nested struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &nested); err == nil && nested.Data != nil {
		return nested.Data, nil
	}
	return nil, errors.New("lobsterai：目录响应形状不符（data 既不是数组，也不是带 data 数组的对象）")
}

// modelWire 单个模型条目的原始字段（协议 §6 的字段表）。
//
// 指针用于区分「字段缺失」与「显式 0/false」：协议要求缺失一律留未知，
// **绝不填 0/false 假值**。requestCapabilities / description 也解析出来，
// 但**不参与归一**——两者语义未在协议中确认，宁可留空也不猜（R49 同款纪律）。
type modelWire struct {
	ModelID             string          `json:"modelId"`
	ModelName           string          `json:"modelName"`
	ContextWindow       *int64          `json:"contextWindow"`
	MaxTokens           *int64          `json:"maxTokens"`
	SupportsImage       *bool           `json:"supportsImage"`
	SupportsThinking    *bool           `json:"supportsThinking"`
	ThinkingConfig      json.RawMessage `json:"thinkingConfig"`
	RequestCapabilities []string        `json:"requestCapabilities"`
	Description         string          `json:"description"`
	CostMultiplier      *float64        `json:"costMultiplier"`
}

// normalizeEntry 把单条原始 JSON 归一成 entry。ok=false 表示该条不可用（无 modelId）。
func normalizeEntry(raw json.RawMessage) (entry, bool) {
	var w modelWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return entry{}, false // 坏条目：整条丢掉，不产出半成品模型
	}
	id := strings.TrimSpace(w.ModelID)
	if id == "" {
		return entry{}, false
	}
	e := entry{ID: id}
	// 展示名缺失时回退 id：这是**展示兜底**，不是伪造上游字段值
	// （面板上一条没有名字的模型等于不可用）。
	e.Name = strings.TrimSpace(w.ModelName)
	if e.Name == "" {
		e.Name = id
	}
	// contextWindow / maxTokens：只在**正数**时采纳。0 与负数是「未知/坏数据」，
	// 不是合法容量——写成 0 会让消费方把未知当 0 用。
	if w.ContextWindow != nil && *w.ContextWindow > 0 {
		e.ContextWindow = *w.ContextWindow
	}
	if w.MaxTokens != nil && *w.MaxTokens > 0 {
		e.MaxTokens = *w.MaxTokens
	}
	// 能力位：只有显式 true 才算「已确认支持」；false 与缺失都保持 false（未知）。
	// provider.ModelInfo 的口径就是这样（「未知与明确不支持对消费方处理一致」）。
	e.SupportsImage = w.SupportsImage != nil && *w.SupportsImage
	e.SupportsThinking = w.SupportsThinking != nil && *w.SupportsThinking
	// costMultiplier 是**裸数字**（与 buddy 系的字符串 "x0.05" 不同，协议 §6），
	// 且只在 > 0 时才带上：0/负数不产出（unknown ≠ 免费）。
	if w.CostMultiplier != nil && *w.CostMultiplier > 0 {
		e.Multiplier = *w.CostMultiplier
		e.HasMultiplier = true
		// 展示名 `名称 · x{倍率}`（协议 §6：写进 name，不是 description）。
		e.Name = e.Name + " · x" + trimFloat(e.Multiplier)
	}
	// 思考档位：supportsThinking 显式 false 时忽略 thinkingConfig
	// （「不支持思考」比档位表更权威，且两者同时出现时以能力位为准——**推断**，
	//  协议只说 supportsThinking 是权威能力位，没说冲突时谁赢）。
	if w.SupportsThinking == nil || *w.SupportsThinking {
		e.Efforts, e.DefaultEffort, e.CanDisableThinking = parseThinkingConfig(w.ThinkingConfig)
	}
	return e, true
}

// ---------------------------------------------------------------------------
// 思考档位解析（协议 §4.1：本渠道最容易踩的坑）
// ---------------------------------------------------------------------------

// 两个取值域（协议 §4.1 逐字照抄）：
//
//	产品侧档位名（level）      = off|minimal|low|medium|high|xhigh|max   （7 档，含 max）
//	发给服务端的 wire 值       = off|minimal|low|medium|high|xhigh       （6 档，**没有 max**）
//
// 远端把 level:'max' 映射到 openclawLevel:'xhigh'；实测**直接发 reasoning_effort:'max'
// 与不带参数无差异**，发 'xhigh' 才触发最高档。因此对外声明的档位表必须是 **wire 值**：
// wild-work 的 ReasoningEfforts 会被原样注入 reasoning_effort（R63「出站注入」），
// 声明一个上游不认识的值等于给用户一档静默失效的选项。
var (
	productLevels = map[string]bool{
		"off": true, "minimal": true, "low": true, "medium": true,
		"high": true, "xhigh": true, "max": true,
	}
	wireLevels = map[string]bool{
		"off": true, "minimal": true, "low": true, "medium": true, "high": true, "xhigh": true,
	}
)

// parseThinkingConfig 解析 thinkingConfig（协议 §4.1 的严格规则）。
//
// 参考实现的解析很严格（非空 options、level/openclawLevel 均合法、两者「是否 off」
// 一致、不允许重复、defaultLevel 必须落在 options 内；只有 off 一档视为无档位）。
// 本实现保留**全部判据**，但把「整表判失败」降级为「逐档丢弃」：
// 一个坏档位不该让整张模型目录的思考能力消失（R55 的容错承接精神）。
//
// ⚠️ **未确认**：thinkingConfig 的**字段名**（options / defaultLevel / level /
// openclawLevel）来自协议 §4.1 的转述，文档没有逐字给出 JSON 结构。这里同时容忍
// 「options 里放字符串」的写法（`["low","high"]`），两种形态都认。
func parseThinkingConfig(raw json.RawMessage) (efforts []string, def string, canDisable *bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, "", nil
	}
	var cfg struct {
		Options      []json.RawMessage `json:"options"`
		DefaultLevel string            `json:"defaultLevel"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, "", nil // 形状不符：整块当未声明（不猜）
	}
	if len(cfg.Options) == 0 {
		// 「非空 options」是必要条件：空表不构成档位声明。
		return nil, "", nil
	}

	type opt struct {
		wire  string
		level string // 产品侧名，可能为空（只有 openclawLevel 时）
	}
	parsed := make([]opt, 0, len(cfg.Options))
	seen := make(map[string]bool, len(cfg.Options))
	offSeen := false
	for _, o := range cfg.Options {
		level, wire := "", ""
		// 形态一：对象 `{"level":"high","openclawLevel":"high"}`；形态二：裸字符串。
		var asObj struct {
			Level         string `json:"level"`
			OpenclawLevel string `json:"openclawLevel"`
		}
		if err := json.Unmarshal(o, &asObj); err == nil && (asObj.Level != "" || asObj.OpenclawLevel != "") {
			level, wire = strings.TrimSpace(asObj.Level), strings.TrimSpace(asObj.OpenclawLevel)
		} else {
			var asStr string
			if err := json.Unmarshal(o, &asStr); err != nil {
				continue // 既不是对象也不是字符串：丢这一档
			}
			level = strings.TrimSpace(asStr)
		}
		if level != "" && !productLevels[level] {
			continue // 非法产品侧名：丢这一档
		}
		if wire != "" && !wireLevels[wire] {
			continue // 非法 wire 值：丢这一档
		}
		if level != "" && wire != "" && (level == "off") != (wire == "off") {
			// 两者「是否 off」必须一致（协议 §4.1 的判据）——不一致说明表本身是坏的。
			continue
		}
		if wire == "" {
			// 只有产品侧名：按 §4.1 的映射换 wire（max → xhigh）。
			mapped, ok := wireOfProduct(level)
			if !ok {
				continue
			}
			wire = mapped
		}
		if seen[wire] {
			continue // 不允许重复：保留首次出现
		}
		seen[wire] = true
		if wire == "off" {
			// off 是「关闭思考」开关，不是档位：不进 efforts（与 workbuddyai 的
			// CanDisableThinking + 档位表并列惯例一致）。
			offSeen = true
			continue
		}
		parsed = append(parsed, opt{wire: wire, level: level})
	}

	// 只有 off 一档 → 「视为无档位」（协议 §4.1），但「可关闭思考」这一事实要留住。
	if offSeen && len(parsed) == 0 {
		t := true
		return nil, "", &t
	}
	// 一档合法档位都没有（且没有 off）→ 整块当未声明。
	if len(parsed) == 0 {
		return nil, "", nil
	}
	efforts = make([]string, 0, len(parsed))
	for _, p := range parsed {
		efforts = append(efforts, p.wire)
	}
	if offSeen {
		t := true
		canDisable = &t
	} else {
		// options 非空且没有 off ⇒ 上游声明了档位但**没有**关闭项。
		// 这是「上游显式声明不可关闭」（false），不是「未声明」（nil）。
		f := false
		canDisable = &f
	}
	// defaultLevel 必须落在 options 内（协议 §4.1）；可以按产品侧名或 wire 值命中。
	if d := strings.TrimSpace(cfg.DefaultLevel); d != "" {
		if d == "off" && offSeen {
			def = "off"
		} else {
			for _, p := range parsed {
				if p.wire == d || (p.level != "" && p.level == d) {
					def = p.wire
					break
				}
			}
		}
	}
	return efforts, def, canDisable
}

// wireOfProduct 把产品侧档位名映射成 wire 值（协议 §4.1：max → xhigh）。
func wireOfProduct(level string) (string, bool) {
	switch level {
	case "max":
		return "xhigh", true
	case "":
		return "", false
	}
	if wireLevels[level] {
		return level, true
	}
	return "", false
}

// ---------------------------------------------------------------------------
// provider.Upstream：目录与定价
// ---------------------------------------------------------------------------

// FetchModels 实现 provider.Upstream。
//
// 兜底纪律（协议 §6）：远端失败 / 解析出 0 条 → 回退静态兜底表且**不返回错误**
// （目录是展示与能力查询的数据来源，让整条渠道因目录失败而消失没有收益）。
// 唯一例外是**目录门控**：本地压根没有凭据时返回空表且不报错（协议 §6 末段）。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	entries, err := c.fetchCatalog(a)
	switch {
	case errors.Is(err, errNoAccount):
		return []provider.ModelInfo{}, nil
	case err != nil || len(entries) == 0:
		if err == nil {
			err = errors.New("目录解析出 0 条")
		}
		log.Printf("lobsterai：模型目录不可用（%v），回退静态兜底表 %d 条", err, len(staticModelIDs))
		return StaticModels(), nil
	}
	return modelInfosOf(entries), nil
}

// FetchModelPricing 实现 provider.Upstream：用目录里的 costMultiplier 生成费率。
//
// 为什么复用目录接口而不是另找定价端点：协议 §6 明确 costMultiplier 就在
// /api/models/available 的条目里（裸数字），没有独立费率接口。
//
// 只有**上游显式给出倍率（>0）**的模型才产出条目：缺失时不给条目，
// 面板显示「未知」——把缺失当成「免费」是明确禁止的口径（Explicit 字段哲学）。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) {
	entries, err := c.fetchCatalog(a)
	if err != nil || len(entries) == 0 {
		// 目录不可用时**不套静态兜底**：兜底表没有任何倍率信息，
		// 用它产出的空/零值定价会把「不知道」显示成「免费」。
		return []provider.ModelPricing{}, nil
	}
	yes := true
	out := make([]provider.ModelPricing, 0, len(entries))
	for _, e := range entries {
		if !e.HasMultiplier || e.Multiplier <= 0 {
			continue
		}
		out = append(out, provider.ModelPricing{
			Model:    e.ID,
			Channel:  string(Kind),
			Rate:     e.Multiplier,
			Explicit: &yes,
		})
	}
	return out, nil
}

// modelInfosOf 把归一化条目转成 provider.ModelInfo。
//
// 字段口径（对齐 provider.ModelInfo 的「未知不声明」哲学）：
//   - MaxTokens 只在 > 0 时写入（0 = 未声明）；
//   - ContextFromAPI：只有条目自带远端容量时才置 true。**静态兜底表恒 false**
//     ——它的 131072 是本地估计值（协议 §6），冒充「上游接口返回」是造假；
//   - SupportsTools 是**渠道级**声明（见下）。
func modelInfosOf(entries []entry) []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(entries))
	for _, e := range entries {
		mi := provider.ModelInfo{
			ID:   e.ID,
			Name: e.Name,
			// 0 = 未知哨兵：消费方必须判 > 0。
			ContextWindow: e.ContextWindow,
			// 远端条目给了正数容量才算「来自上游接口」；估计值路径（Estimated）
			// 一律 false。
			ContextFromAPI: !e.Estimated && (e.ContextWindow > 0 || e.MaxTokens > 0),
			SupportsImages: e.SupportsImage,
			// 工具调用是**渠道级**能力声明：本渠道的对话体有完整的 tools 字段
			// （协议 §4），且 kimi-k3-agentic-v1 能力头就是 agentic 面。
			// ⚠️ 未确认：远端条目里没有 per-model 工具能力字段（requestCapabilities
			// 语义未在协议中确认，故不参与归一）⇒ 这是渠道级声明，不是逐模型实测。
			SupportsTools: true,
		}
		if e.MaxTokens > 0 {
			mi.MaxTokens = e.MaxTokens
		}
		// 思考能力：能力位为真、或上游声明了档位表、或声明了「可关闭思考」。
		if e.SupportsThinking || len(e.Efforts) > 0 ||
			(e.CanDisableThinking != nil && *e.CanDisableThinking) {
			mi.SupportsReasoning = true
		}
		mi.ReasoningEfforts = provider.CleanEfforts(e.Efforts)
		mi.DefaultReasoningEffort = strings.TrimSpace(e.DefaultEffort)
		mi.CanDisableThinking = e.CanDisableThinking
		out = append(out, mi)
	}
	return out
}

// trimFloat 把倍率格式化成最短十进制表示（0.05 → "0.05"、20 → "20"），
// 用于展示名 `· x{倍率}`。不用 %g：它会把 20 打成 "20"、0.05 打成 "0.05" 没错，
// 但 1e+06 会变科学计数法，与上游面板的写法不一致。
func trimFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
