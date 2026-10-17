// models.go MiniMax Code 的模型目录：动态解析（协议 §6）+ 静态兜底表。
//
// 两条路径**只共用输出形状，不共用归一规则**（协议 §6 首行）：
//   - 动态：GET {APIHost}/mavis/api/v1/models?region=cn&buildEnv=prod，
//     取 providers[] 里 providerId=="minimax" 的那项，其 config.models 是**对象**，
//     **key 是长名**（注入为 id），条目自带的 name 是**短名**；
//   - 静态：4 条兜底表，逐字段照抄协议 §6 的表格（顺序照抄远端 model_order）。
//
// 兜底触发条件（协议 §6）：远端失败、抛错、或**解析出 0 条**都回退兜底表，且不报错
// ——目录拿不到不该让整条渠道在面板里消失（与 R42「静态表 ∪ 动态目录」同向）。
package minimax

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// catalogEntry 是归一化后的模型条目；动态目录与静态兜底共用同一形状
// （这也是「两条路径只共用输出形状」的落地处）。
type catalogEntry struct {
	// ID 长名（如 MiniMax-M3.1-Flash-Preview）——直接发给上游的 model 值。
	ID string
	// Name 短名（如 M3.1-Flash-Preview）——面板展示用；两条都要保留（协议 §6）。
	Name string
	// ContextWindow 上下文窗口；**0 = 未知**（不是合法窗口，消费方必须判 > 0）。
	ContextWindow int64
	// MaxTokens 输出上限；**<= 0 表示上游未声明/非法**，此时不得对外产出该字段
	// （协议 §6：声明成必填会让类型撒谎）。
	MaxTokens int64
	// SupportsImage modalities.input 含 image。
	SupportsImage bool
	// Efforts 上游声明的思考档位表（去重保序）；nil = 上游未声明档位。
	Efforts []string
	// DefaultEffort 上游声明的默认档位，且必须落在 Efforts 内（否则丢弃）。
	DefaultEffort string
	// ThinkingMode thinking_config.mode：forced_on / switchable / 空（未声明）。
	ThinkingMode string
}

// requiresAdaptive 报告该模型是否**必须**以 adaptive 开启思考。
//
// 判据是 id **前缀** MiniMax-M3.1（协议 §4.1）：不能写成 MiniMax-M3 —— M3 是
// switchable，传 disabled 合法；而 M3.1 传 disabled 会**硬 400**（业务码 2013
// "requires adaptive thinking"），是本渠道唯一已知的「必然失败」组合。
func (e catalogEntry) requiresAdaptive() bool {
	return strings.HasPrefix(e.ID, "MiniMax-M3.1")
}

// ---------------------------------------------------------------------------
// 静态兜底表（协议 §6 的表格，逐字段照抄）
// ---------------------------------------------------------------------------

// staticCatalog 静态兜底 4 条，顺序照抄远端 model_order。
//
// 为什么要含 MiniMax-M3.1-Flash-Preview：它**不在**官方客户端内置的静态表里
// （asar config.js 的 MINIMAX_MODELS 只有 3 条），只出现在远端目录中，
// 故兜底表必须自带它（协议 §6）。
//
// effort_options 只有 M3.1 有；给其余三个编档位就是凭空猜测（协议 §6 末条），
// 故其余三条 Efforts 为 nil、DefaultEffort 为空。
var staticCatalog = []catalogEntry{
	{
		ID: "MiniMax-M3.1-Flash-Preview", Name: "M3.1-Flash-Preview",
		ContextWindow: 1_000_000, MaxTokens: 128_000, SupportsImage: true,
		Efforts:       []string{"default", "low", "medium", "high", "xhigh", "max"},
		DefaultEffort: "default", ThinkingMode: "forced_on",
	},
	{
		ID: "MiniMax-M3", Name: "M3",
		ContextWindow: 1_000_000, MaxTokens: 128_000, SupportsImage: true,
		ThinkingMode: "switchable",
	},
	{
		ID: "MiniMax-M2.7-highspeed", Name: "M2.7-highspeed",
		ContextWindow: 200_000, MaxTokens: 128_000, SupportsImage: false,
		ThinkingMode: "forced_on",
	},
	{
		ID: "MiniMax-M2.7", Name: "M2.7",
		ContextWindow: 200_000, MaxTokens: 128_000, SupportsImage: false,
		ThinkingMode: "forced_on",
	},
}

// staticIndex 静态表的长名索引（本地能力查询用）。
var staticIndex = func() map[string]catalogEntry {
	m := make(map[string]catalogEntry, len(staticCatalog))
	for _, e := range staticCatalog {
		m[e.ID] = e
	}
	return m
}()

// StaticModels 静态模型表（装配时注入 Runtime.StaticModels；面板无凭据时也有列表）。
func StaticModels() []provider.ModelInfo { return modelInfosOf(staticCatalog) }

// ---------------------------------------------------------------------------
// 远端目录解析
// ---------------------------------------------------------------------------

// modelsURL 目录接口的完整 URL（query 是服务端要求的定位参数，不是可选装饰）。
func (c *Client) modelsURL() string {
	return c.apiHost() + EpModels + "?region=" + Region + "&buildEnv=" + BuildEnv
}

// modelsEnvelope 远端目录响应的最小形状：只取 providers[] 的 providerId 与 config。
// config.models 故意用 json.RawMessage 承接——它的**键序**是排序依据之一，
// 必须有序列化地解析（用 map 会丢序，协议 §6 明确要求「否则保持对象插入序」）。
type modelsEnvelope struct {
	Providers []struct {
		ProviderID string `json:"providerId"`
		Config     struct {
			Models     json.RawMessage `json:"models"`
			ModelOrder []string        `json:"model_order"`
		} `json:"config"`
	} `json:"providers"`
}

// parseCatalog 解析远端目录响应。
//
// 硬约束（协议 §6，逐条对应参考实现 minimax-auth.ts:455-486）：
//  1. providers[] 里必须存在 providerId == "minimax" 的那一项，否则算解析失败；
//  2. 其 config.models 必须是**非空对象**（数组/缺失/空对象一律算失败 → 触发兜底）；
//  3. 对象 key 是长名并注入为 id；条目自带 name 是短名，两者都保留；
//  4. 排序按 config.model_order（若提供），否则保持对象插入序。
func parseCatalog(raw []byte) ([]catalogEntry, error) {
	var env modelsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("目录响应解析失败：%w", err)
	}
	var (
		models json.RawMessage
		order  []string
		found  bool
	)
	for _, p := range env.Providers {
		if p.ProviderID == ProviderID {
			models, order, found = p.Config.Models, p.Config.ModelOrder, true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("providers[] 里没有 providerId=%q 的项", ProviderID)
	}
	keys, vals, err := parseOrderedObject(models)
	if err != nil {
		return nil, fmt.Errorf("config.models 不是对象：%w", err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("config.models 是空对象")
	}
	out := make([]catalogEntry, 0, len(keys))
	for _, k := range orderedKeys(keys, order) {
		out = append(out, normalizeEntry(k, vals[k]))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("config.models 解析出 0 条")
	}
	return out, nil
}

// parseOrderedObject 用 json.Decoder 逐 token 读一个 JSON 对象，**保留键的插入序**，
// 每个键的原始值以 json.RawMessage 交出（形状不符时只损失单条，不拖垮整张目录）。
//
// 为什么不用 map[string]json.RawMessage：Go 的 map 迭代序随机，而协议 §6 要求
// 「无 model_order 时保持对象插入序」，用 map 就是丢序。
func parseOrderedObject(raw []byte) ([]string, map[string]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil, fmt.Errorf("字段缺失")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, nil, fmt.Errorf("不是对象（首 token %v）", tok)
	}
	keys := make([]string, 0, 8)
	vals := make(map[string]json.RawMessage, 8)
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, nil, err
		}
		key, _ := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, nil, err
		}
		if _, dup := vals[key]; !dup {
			keys = append(keys, key) // 重复键：保留首次出现的位置，值以后者为准
		}
		vals[key] = v
	}
	if _, err := dec.Token(); err != nil { // 吃掉收尾的 '}'
		return nil, nil, err
	}
	return keys, vals, nil
}

// orderedKeys 按 model_order 重排键：**列在 order 里的按 order 排在前，未列的按插入序
// 追加在后**。
//
// 为什么不把未列出的直接丢掉：model_order 是排序声明，不是白名单；丢模型会让新上线的
// 模型在面板里静默消失（协议 §6 只说了「按 model_order 排」，没说它会列举全集）。
func orderedKeys(keys, order []string) []string {
	if len(order) == 0 {
		return keys
	}
	present := make(map[string]bool, len(keys))
	for _, k := range keys {
		present[k] = true
	}
	out := make([]string, 0, len(keys))
	seen := make(map[string]bool, len(keys))
	for _, k := range order {
		if present[k] && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// modelWire 单个模型条目的原始字段。
//
// ⚠️ 已知覆盖缺口（协议 §6 已记录，本实现**同样没做**）：只实现了 snake_case 字段名；
// 参考实现的 asar 版 parseModel 还认 4 组 camelCase 别名并做 ≤ 2^31-1 的上限校验。
// 远端若改下发 camelCase，这里会静默取不到值（表现为窗口/档位丢失，而不是报错）。
type modelWire struct {
	Name string `json:"name"`
	// Limit.Context / Limit.Output 用指针以区分「字段缺失」与「显式 0」
	// （0 在协议里是「未知」哨兵，不是合法容量）。
	Limit struct {
		Context *int64 `json:"context"`
		Output  *int64 `json:"output"`
	} `json:"limit"`
	ContextWindowOptions []int64  `json:"context_window_options"`
	Modalities           struct { // 输入模态
		Input []string `json:"input"`
	} `json:"modalities"`
	EffortOptions  []string `json:"effort_options"`
	DefaultEffort  string   `json:"default_effort"`
	ThinkingConfig struct {
		Mode string `json:"mode"`
	} `json:"thinking_config"`
}

// normalizeEntry 把（长名 key + 原始 JSON 值）归一成 catalogEntry。
// 形状不符时只损失该条的可选字段，不返回错误（协议 §6 的「容错承接」）。
func normalizeEntry(id string, raw json.RawMessage) catalogEntry {
	e := catalogEntry{ID: strings.TrimSpace(id)}
	var w modelWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return e // 坏条目：只有 id 可用（至少不丢模型）
	}
	e.Name = strings.TrimSpace(w.Name)
	if e.Name == "" {
		e.Name = e.ID
	}
	// contextWindow = context_window_options 的**最大档**；无档位表回退 limit.context。
	//
	// 为什么取最大档而不是 limit.context：协议 §6 的实测注释——M3.1-Flash-Preview 的
	// 远端 limit.context 是 512000，而 context_window_options 是 [512000, 1000000]；
	// 取小了会让 DSH 远早于官方能力触发上下文压缩。
	e.ContextWindow = maxOf(w.ContextWindowOptions)
	if e.ContextWindow <= 0 && w.Limit.Context != nil {
		e.ContextWindow = *w.Limit.Context
	}
	if e.ContextWindow < 0 {
		e.ContextWindow = 0 // 0 = 未知哨兵；负数是坏数据，一并归 0
	}
	// maxTokens = limit.output，**仅安全正整数**（协议 §6）。
	if w.Limit.Output != nil && *w.Limit.Output > 0 {
		e.MaxTokens = *w.Limit.Output
	}
	for _, m := range w.Modalities.Input {
		if strings.EqualFold(strings.TrimSpace(m), "image") {
			e.SupportsImage = true
		}
	}
	// 档位：去重保序（provider.CleanEfforts 是全仓统一的清洗口径，空表 → nil）。
	e.Efforts = provider.CleanEfforts(w.EffortOptions)
	e.DefaultEffort = strings.TrimSpace(w.DefaultEffort)
	// defaultEffort 必须落在 effortOptions 内，否则丢弃（协议 §6）。
	if e.DefaultEffort != "" && !contains(e.Efforts, e.DefaultEffort) {
		e.DefaultEffort = ""
	}
	e.ThinkingMode = strings.TrimSpace(w.ThinkingConfig.Mode)
	return e
}

// maxOf 取切片最大值（≤ 0 的值一律忽略：0 是「未知」哨兵，不是合法窗口）。
func maxOf(xs []int64) int64 {
	var best int64
	for _, x := range xs {
		if x > best {
			best = x
		}
	}
	return best
}

// ---------------------------------------------------------------------------
// 客户端侧：目录拉取 + 能力查询
// ---------------------------------------------------------------------------

// fetchCatalog 拉远端目录（失败即返回错误，由调用方决定兜底）。
func (c *Client) fetchCatalog(a *auth.Auth) ([]catalogEntry, error) {
	tok := tokenOf(a)
	if tok == "" {
		return nil, fmt.Errorf("账号无 access_token")
	}
	req, err := http.NewRequest(http.MethodGet, c.modelsURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("目录接口 HTTP %d：%s", resp.StatusCode, snippet(raw))
	}
	return parseCatalog(raw)
}

// FetchModels 实现 provider.Upstream。
//
// 兜底纪律（协议 §6）：远端失败 / 抛错 / **解析出 0 条** 都回退静态兜底表，
// **不返回错误**——目录是展示与能力查询的数据来源，让它把整条渠道打成错误没有收益。
// 同时**只缓存真实的远端目录**：兜底表每次现算，一次瞬时失败不会污染整个进程生命周期
// （失败时也**不清空**上一次成功的动态目录：能力查询宁可用旧的真实数据，也不用兜底表冒充）。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	entries, err := c.fetchCatalog(a)
	if err != nil || len(entries) == 0 {
		if err == nil {
			err = fmt.Errorf("目录解析出 0 条")
		}
		log.Printf("minimax：模型目录不可用（%v），回退静态兜底表 %d 条", err, len(staticCatalog))
		return modelInfosOf(staticCatalog), nil
	}
	c.setDynamic(entries)
	return modelInfosOf(entries), nil
}

// FetchModelPricing 实现 provider.Upstream：本渠道**没有费率端点**（协议 §6/§9 全篇
// 没有倍率/定价字段），故返回空切片——面板显示「未知」，而不是把缺失当成「免费」。
func (c *Client) FetchModelPricing(_ *auth.Auth) ([]provider.ModelPricing, error) {
	return []provider.ModelPricing{}, nil
}

// setDynamic 记录最近一次成功拉取的真实远端目录。
func (c *Client) setDynamic(entries []catalogEntry) {
	c.catMu.Lock()
	c.dynamic = entries
	c.catMu.Unlock()
}

// catalogEntry 查模型能力：动态目录优先，其次静态兜底表。
// ok=false 表示本工具完全不认识该模型（此时思考决策走保守分支，见 thinkingFor）。
func (c *Client) catalogEntry(id string) (catalogEntry, bool) {
	id = upstreamModelName(id)
	c.catMu.RLock()
	for _, e := range c.dynamic {
		if e.ID == id {
			c.catMu.RUnlock()
			return e, true
		}
	}
	c.catMu.RUnlock()
	e, ok := staticIndex[id]
	return e, ok
}

// ---------------------------------------------------------------------------
// 输出形状
// ---------------------------------------------------------------------------

// modelInfosOf 把归一化条目转成 provider.ModelInfo。
//
// 字段口径（对齐 provider.ModelInfo 的「未知不声明」哲学）：
//   - MaxTokens 仅在 > 0 时写入；0 = 未声明（Go 侧零值即「不产出该字段」。
//     协议 §6 明确：远端非正整数时**整个键不产出**，写成 0 会让消费方把「未知」当 0 用）。
//   - SupportsImages 只认上游声明的 modalities.input。
//   - SupportsTools 是**渠道级**声明（见下）。
//   - ContextFromAPI：值取自上游目录 / 上游声明的兜底表，不是本工具实测（HandMeasured
//     会给从未实测的值冒充「实测」标注，更不实）——与 monkeycode 的同款取舍。
func modelInfosOf(entries []catalogEntry) []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(entries))
	for _, e := range entries {
		mi := provider.ModelInfo{
			ID:   e.ID,
			Name: e.Name,
			// 0 = 未知哨兵：调用方必须判 > 0（协议 §6）。
			ContextWindow: e.ContextWindow,
			// 只有拿到真实容量时才敢标「来自上游接口」。
			ContextFromAPI: e.ContextWindow > 0 || e.MaxTokens > 0,
			SupportsImages: e.SupportsImage,
			// 本渠道协议通篇以 tool_use / tool_result 为核心（协议 §4.2 十条规则都在讲
			// 工具调用配对），故按**渠道级**能力声明为 true。
			// ⚠️ 未确认：远端条目里没有 per-model 的工具能力字段，因此「是否存在模型级差异」
			// 无从判断——这是渠道级声明，不是逐模型实测。
			SupportsTools: true,
		}
		if e.MaxTokens > 0 {
			mi.MaxTokens = e.MaxTokens
		}
		// 思考能力：只有上游声明了 thinking_config.mode 或档位表才置位。
		if e.ThinkingMode != "" || len(e.Efforts) > 0 {
			mi.SupportsReasoning = true
		}
		mi.ReasoningEfforts = effortsFor(e)
		mi.DefaultReasoningEffort = e.DefaultEffort
		switch e.ThinkingMode {
		case "switchable":
			// M3 是 switchable：不传 / disabled → 0 思考块，adaptive → 有思考块
			// （协议 §4.1）。两态都要能被用户选到，故追加 on / none 两档（on 在前）。
			t := true
			mi.CanDisableThinking = &t
		case "forced_on":
			// forced_on：**绝不**追加 on / none（协议 §4.1）——它的思考关不掉，
			// 给面板一档「关闭」只会诱导用户选出一个必然 400 或静默无效的档位。
			f := false
			mi.CanDisableThinking = &f
		}
		out = append(out, mi)
	}
	return out
}

// effortsFor 产出对外可选的思考档位表：上游 effort_options 去重保序，
// switchable 模型再追加 on / none（顺序 on 在 none 前，协议 §4.1）。
// 上游未声明档位且不可切换时返回 nil（不编档，R49）。
func effortsFor(e catalogEntry) []string {
	out := provider.CleanEfforts(e.Efforts)
	if e.ThinkingMode != "switchable" {
		return out
	}
	for _, pseudo := range []string{"on", "none"} {
		if !contains(out, pseudo) {
			out = append(out, pseudo)
		}
	}
	return out
}

// contains 判定字符串切片是否含某值（大小写敏感：档位名是协议里的字面量）。
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// snippet 截一段响应体用于错误信息（排障线索，不含凭据）。
func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return "空响应"
	}
	return s
}
