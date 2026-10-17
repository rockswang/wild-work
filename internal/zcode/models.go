// models.go ZCode 的模型目录：动态目录（协议 §1.6 第 1 层）+ 静态兜底表（第 2 层）。
//
// 动态目录来源：`GET {Origin}/api/v1/client/configs?app_version=<v>&platform=unknown`
// 的 `data.builtinModels`。
//
// ⚠ 两个必须照抄的实现细节（协议 §1.6）：
//
//  1. `builtinModels` 是**对象**（键为序号字串，如 `{"0":{…},"1":{…}}`），必须按值遍历；
//     用「数组」假设会得到「0 个模型」的**假阴性**；
//  2. query 的 `platform` **必须是 `unknown`**：实测 `win32`/`win64`/`windows`/`electron`/
//     `desktop`/`linux` 一律 `400 {"code":3001}`（协议 §1.2.2）。
//
// 静态兜底表**只放实测可用的两个**（协议 §1.6 第 2 层）：`GLM-5.3-Flash`（有 vision）
// 与 `GLM-5.3`（上游 capabilities 为空对象 ⇒ **无** vision）。
// `GLM-5-Turbo` / `GLM-5.2` 在上游清单里存在，但 2026-09-28 实测在 Start Plan 下返回空响应
// （0/3 正确率），故**不暴露**——这也是「能力字段必须抄上游、不能按同族推断」的同一条纪律。
//
// 兜底触发条件：远端失败、抛错、**或解析出 0 条**都回退静态表，且**不返回错误**
// ——目录拿不到不该让整条渠道在面板里消失（与 R42「静态表 ∪ 动态目录」同向）。
package zcode

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

// catalogEntry 归一化后的模型条目；动态目录与静态兜底共用同一形状。
type catalogEntry struct {
	// ID 直接发给上游的 model 值（如 GLM-5.3-Flash）。
	ID string
	// Name 展示名（上游未给时回落 ID）。
	Name string
	// ContextWindow 上下文窗口；**0 = 未知**（不是合法窗口，消费方必须判 > 0）。
	ContextWindow int64
	// MaxTokens 输出上限；**<= 0 表示上游未声明/非法**，此时不得对外产出该字段。
	MaxTokens int64
	// SupportsImage 上游声明的视觉能力（capabilities.vision 或 modalities.input 含 image）。
	SupportsImage bool
	// Efforts 上游声明的思考档位表（去重保序）；nil = 上游未声明档位。
	Efforts []string
	// DefaultEffort 上游声明的默认档位，且必须落在 Efforts 内（否则丢弃）。
	DefaultEffort string
}

// effortOrder 档位展示序（协议 §1.6：显式排序为 low → high → max）。
//
// 为什么需要显式排序：`reasoning.levels` 是**对象**，其键序由上游序列化顺序决定；
// 协议明确记录了官方展示序，故按它排，未列的档位按插入序追加在后。
var effortOrder = []string{"low", "high", "max"}

// ---------------------------------------------------------------------------
// 静态兜底表（协议 §1.6 第 2 层，逐字段照抄）
// ---------------------------------------------------------------------------

var staticCatalog = []catalogEntry{
	{
		ID: "GLM-5.3-Flash", Name: "GLM-5.3-Flash",
		ContextWindow: 1_000_000, MaxTokens: 128_000,
		SupportsImage: true,
		Efforts:       []string{"low", "high", "max"}, DefaultEffort: "max",
	},
	{
		ID: "GLM-5.3", Name: "GLM-5.3",
		ContextWindow: 1_000_000, MaxTokens: 128_000,
		// ⚠ 上游 `capabilities` 是空对象 ⇒ 这个模型**没有 vision**
		// （曾按「同族应该一样」推断成 true，被实测推翻）。
		SupportsImage: false,
		Efforts:       []string{"low", "high", "max"}, DefaultEffort: "max",
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
//
// ⚠ 已知取舍（如实标注）：静态表是**渠道级**的，不带通道（start-plan / coding-plan）维度。
// 通道承载白名单（协议 §1.6 第 3 层：start-plan = glm-5.3-flash/glm-5.2/glm-5-turbo，
// coding-plan = glm-5.3/glm-5.3-flash）需要凭据才能判定，而本函数没有凭据入参，
// 故不在这里过滤——越界模型由上游按凭据权益拒（本仓的 ErrHardCredit/ErrClient 路径表达）。
func StaticModels() []provider.ModelInfo { return modelInfosOf(staticCatalog) }

// ---------------------------------------------------------------------------
// 远端目录解析
// ---------------------------------------------------------------------------

// clientConfigsURL 目录接口的完整 URL。`platform=unknown` 不是可选装饰（见文件头）。
func (c *Client) clientConfigsURL(a *auth.Auth) string {
	return c.origin() + EpClientConfigs +
		"?app_version=" + appVersionOf(a) + "&platform=" + configsPlatform
}

// configsPlatform 目录接口要求的 platform 取值（**必须 unknown**，协议 §1.2.2）。
const configsPlatform = "unknown"

// parseBuiltinModels 从客户端配置响应里解析 `data.builtinModels`。
//
// 用有序解析（parseOrderedObject）而不是 map：`builtinModels` 的对象键序就是上游的
// 下发顺序，保持它可以让面板顺序与官方一致（无需 model_order 之类的排序声明）。
func parseBuiltinModels(raw []byte) ([]catalogEntry, error) {
	top, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("目录响应解析失败：%w", err)
	}
	if code, msg, ok := businessCode(top); ok && code != 0 {
		return nil, fmt.Errorf("目录接口业务码 %d %s", code, msg)
	}
	data, err := decodeObject(top["data"])
	if err != nil {
		return nil, fmt.Errorf("目录响应缺 data 对象：%w", err)
	}
	// businessObject 的口径：字段可能平铺在 data 下（正常）或直接被包一层。
	modelsRaw := data["builtinModels"]
	if len(modelsRaw) == 0 {
		return nil, fmt.Errorf("data.builtinModels 缺失")
	}
	keys, vals, err := parseOrderedObject(modelsRaw)
	if err != nil {
		return nil, fmt.Errorf("builtinModels 不是对象：%w", err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("builtinModels 是空对象")
	}
	out := make([]catalogEntry, 0, len(keys))
	for _, k := range keys {
		if e, ok := normalizeModelEntry(vals[k]); ok {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("builtinModels 解析出 0 条")
	}
	return out, nil
}

// parseOrderedObject 用 json.Decoder 逐 token 读一个 JSON 对象，**保留键的插入序**，
// 每个键的原始值以 json.RawMessage 交出（形状不符时只损失单条，不拖垮整张目录）。
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

// modelWire 单个 builtinModels 条目的原始字段（协议 §1.6 第 1 层）。
type modelWire struct {
	ModelID string `json:"modelId"`
	// ContextWindow / MaxCompletionTokens 用指针以区分「字段缺失」与「显式 0」
	// （0 在协议里是「未知」哨兵，不是合法容量）。
	ContextWindow       *int64 `json:"contextWindow"`
	MaxCompletionTokens *int64 `json:"maxCompletionTokens"`
	Capabilities        struct {
		Vision bool `json:"vision"`
	} `json:"capabilities"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Reasoning struct {
		// Levels 是**对象**：键为档位名，值是 `{anthropic:{set:{path:[…],value:…}}}`。
		// 档位真值取 `levels[k].anthropic.set.value`（上游连"怎么表达档位"都下发了）；
		// 取不到时回落键名本身。
		Levels       json.RawMessage `json:"levels"`
		DefaultLevel string          `json:"defaultLevel"`
	} `json:"reasoning"`
}

// normalizeModelEntry 把一条 builtinModels 原始值归一成 catalogEntry。
// 形状不符时返回 ok=false（丢掉该条，不拖垮整张目录）。
func normalizeModelEntry(raw json.RawMessage) (catalogEntry, bool) {
	var w modelWire
	if err := json.Unmarshal(raw, &w); err != nil {
		return catalogEntry{}, false
	}
	id := strings.TrimSpace(w.ModelID)
	if id == "" {
		// 少数上游形态把模型名放在键上；此时无法确定 modelId，宁可丢一条也不猜
		// （猜错会让整个请求打到不存在的模型上）。
		return catalogEntry{}, false
	}
	e := catalogEntry{ID: id, Name: id}
	if w.ContextWindow != nil && *w.ContextWindow > 0 {
		e.ContextWindow = *w.ContextWindow
	}
	if w.MaxCompletionTokens != nil && *w.MaxCompletionTokens > 0 {
		e.MaxTokens = *w.MaxCompletionTokens
	}
	e.SupportsImage = w.Capabilities.Vision
	for _, m := range w.Modalities.Input {
		if strings.EqualFold(strings.TrimSpace(m), "image") {
			e.SupportsImage = true
		}
	}
	e.Efforts = parseEffortLevels(w.Reasoning.Levels)
	e.DefaultEffort = strings.TrimSpace(w.Reasoning.DefaultLevel)
	if e.DefaultEffort != "" && !contains(e.Efforts, e.DefaultEffort) {
		e.DefaultEffort = ""
	}
	return e, true
}

// parseEffortLevels 解析 `reasoning.levels` 对象 → 档位表（按 effortOrder 排序）。
func parseEffortLevels(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	keys, vals, err := parseOrderedObject(raw)
	if err != nil {
		// 少数形态可能直接给字符串数组；容错承接（不因此丢掉整条模型）。
		var list []string
		if json.Unmarshal(raw, &list) == nil {
			return provider.CleanEfforts(list)
		}
		return nil
	}
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		name := strings.TrimSpace(k)
		var lv struct {
			Anthropic struct {
				Set struct {
					Value string `json:"value"`
				} `json:"set"`
			} `json:"anthropic"`
		}
		if json.Unmarshal(vals[k], &lv) == nil {
			if v := strings.TrimSpace(lv.Anthropic.Set.Value); v != "" {
				// ⚠ 取上游下发的 value（不是键名）：它是「这个档位该怎么写进请求体」的
				// 权威表述，与键名不一致时以上游为准。
				name = v
			}
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return orderEfforts(provider.CleanEfforts(names))
}

// orderEfforts 按 effortOrder 重排档位：列在 order 里的按 order 排在前，未列的按原序追加。
func orderEfforts(xs []string) []string {
	if len(xs) == 0 {
		return nil
	}
	out := make([]string, 0, len(xs))
	seen := make(map[string]bool, len(xs))
	present := make(map[string]bool, len(xs))
	for _, x := range xs {
		present[x] = true
	}
	for _, want := range effortOrder {
		if present[want] && !seen[want] {
			seen[want] = true
			out = append(out, want)
		}
	}
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 客户端侧：目录拉取 + 能力查询
// ---------------------------------------------------------------------------

// fetchCatalog 拉远端目录（失败即返回错误，由调用方决定兜底）。
func (c *Client) fetchCatalog(a *auth.Auth) ([]catalogEntry, error) {
	req, err := http.NewRequest(http.MethodGet, c.clientConfigsURL(a), nil)
	if err != nil {
		return nil, err
	}
	// 目录端点**需要 Authorization 且需要 X-Device-Mid**（协议 §1.2.2）。
	req.Header = zcodeHeaders(a, "Bearer "+tokenOf(a), false)
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
	return parseBuiltinModels(raw)
}

// FetchModels 实现 provider.Upstream。
//
// 兜底纪律：远端失败 / 抛错 / **解析出 0 条** 都回退静态兜底表，**不返回错误**
// ——目录是展示与能力查询的数据来源，让它把整条渠道打成错误没有收益。
// 同时**只缓存真实的远端目录**：失败时也**不清空**上一次成功的动态目录
// （能力查询宁可用旧的真实数据，也不用兜底表冒充）。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	entries, err := c.fetchCatalog(a)
	if err != nil || len(entries) == 0 {
		if err == nil {
			err = fmt.Errorf("目录解析出 0 条")
		}
		log.Printf("zcode：模型目录不可用（%v），回退静态兜底表 %d 条", err, len(staticCatalog))
		return modelInfosOf(staticCatalog), nil
	}
	c.setDynamic(entries)
	return modelInfosOf(entries), nil
}

// FetchModelPricing 实现 provider.Upstream：本渠道**没有倍率/定价端点**
// （协议 §1.1 的端点表里没有任何 pricing 条目），故返回空切片——面板显示「未知」，
// 而不是把缺失当成「免费」。
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
// ok=false 表示本工具完全不认识该模型（此时思考档位不下发，见 declaredEffort）。
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
//   - MaxTokens 仅在 > 0 时写入（0 = 未声明）；
//   - SupportsImages 只认上游声明的 capabilities.vision / modalities.input；
//   - SupportsTools 是**渠道级**声明：本渠道协议通篇以 tool_use / tool_result 为核心，
//     且参考实现把「tools 必须真的下发」列为三个必须做到的点之一；
//   - SupportsReasoning / ReasoningEfforts / DefaultReasoningEffort 只透传上游声明
//     （R49：绝不编造档位）；
//   - ContextFromAPI：值取自上游目录或上游声明的兜底表，不是本工具实测
//     （HandMeasured 会给从未实测的值冒充「实测」标注，更不实）。
func modelInfosOf(entries []catalogEntry) []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(entries))
	for _, e := range entries {
		mi := provider.ModelInfo{
			ID:             e.ID,
			Name:           e.Name,
			ContextWindow:  e.ContextWindow,
			ContextFromAPI: e.ContextWindow > 0 || e.MaxTokens > 0,
			SupportsImages: e.SupportsImage,
			SupportsTools:  true,
		}
		if e.MaxTokens > 0 {
			mi.MaxTokens = e.MaxTokens
		}
		if len(e.Efforts) > 0 {
			mi.SupportsReasoning = true
			mi.ReasoningEfforts = provider.CleanEfforts(e.Efforts)
			mi.DefaultReasoningEffort = e.DefaultEffort
		}
		// can_disable_thinking：本渠道协议里**没有**关闭思考的写法
		// （只有 `output_config.effort` 三档），故显式声明为 false（不可关闭）。
		// 用指针表达三态：false 是「上游/协议明确的不可关闭」，不是「未声明」。
		f := false
		mi.CanDisableThinking = &f
		out = append(out, mi)
	}
	return out
}
