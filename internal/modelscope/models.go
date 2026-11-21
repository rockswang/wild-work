package modelscope

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 目录端点与预算。
//
// 参考 R122/R133 的两条教训：① 上游目录可以很大（数 MiB，Go 侧 1MiB 上限会把
// 合法 JSON 拦腰截断成「不是合法 JSON」），故上限给到 8MiB、超限**明确报错**；
// ② 目录是渠道维度的，单次失败不得让整条渠道模型面归零。
const (
	modelsPath = "/v1/models"
	// modelsBodyCap 目录响应体上限。
	modelsBodyCap = 8 << 20
	// modelsTimeout 目录拉取的独立超时（走代理/国际站常破 15s）。
	modelsTimeout = 45 * time.Second
)

// staticEntry 静态目录条目（静态表与上游 /v1/models 取并集，R42）。
type staticEntry struct {
	// ID 上游模型名，**保留上游斜杠**（如 Qwen/Qwen-Image）。对外 ID 由
	// provider.PublicModelID 拼成 `modelscope/Qwen/Qwen-Image`，渠道内不得自己拼前缀。
	ID string
	// Image 标记「生图 / 改图」模型（D12）：进 /v1/models 目录（客户端要能看到可画图模型），
	// 但**不参与对话路由**。
	//
	// ⚠️ 与 provider.ModelInfo.SupportsImages 是**两个维度**：后者是「能看图」（视觉输入），
	// 本字段是「能出图」。`Qwen2.5-VL` 这类视觉理解模型不得标 Image。
	Image bool
}

// staticCatalog 最小静态目录：至少一款对话模型 + 一款生图模型。
//
// 条目按 2026-11-14 的**实测**目录（`GET https://api-inference.modelscope.cn/v1/models`，
// 无需鉴权即 200，返回 38 条 OpenAI 形状 `{id,object,owned_by,created}`）挑取 ——
// 静态表只在「远程目录拉不到」时兜底，绝不能塞上游当前不提供的模型：那会让客户端
// 选中一个必然 404 的模型，而 404 在本渠道归 ErrNotFound（会把**账号**软冷却）。
// 待观察（未实证）：带 token 的目录是否比未鉴权目录更宽。
//
// 能力字段（tools / 思考档位 / 上下文）**一律不声明**：上游没说过的话不得替它说
// （R49/R119）。真实能力由 /v1/models 的上游声明透传（见 modelInfoFromRemote）。
var staticCatalog = []staticEntry{
	{ID: "Qwen/Qwen3.5-122B-A10B"},
	{ID: "Qwen/Qwen-Image-Edit", Image: true},
}

// StaticModels 静态目录（对外 ID 仍是上游名，前缀由 server 层用 PublicModelID 拼）。
//
// 上游 /v1/models 拉取失败时的兜底，也是「启动即有目录」的来源（R42 并集口径）。
func StaticModels() []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(staticCatalog))
	for _, e := range staticCatalog {
		out = append(out, provider.ModelInfo{ID: e.ID, Name: e.ID, SupportsImageGeneration: e.Image})
	}
	return out
}

// 生图判定词表（D12 的命名启发式）。
//
// 两条硬边界：① **视觉理解优先排除**——`Qwen2.5-VL` 的 `vl` 命中 vision 词表即判「不是生图」，
// 否则「能看图」会被误标成「能出图」；② 只认**整 token** 匹配，不做子串偶合，
// 避免 `imagegen-...`、`imaging` 这类拼写误伤。
var (
	imageOutputTokens = []string{"image", "images", "flux", "diffusion", "kolors", "t2i", "sdxl", "sd3"}
	visionInputTokens = []string{"vl", "vlm", "vision", "visual"}
)

// IsImageModel 报告上游模型名是否是生图/改图模型：
// 静态表标记（image:true）∪ 命名启发式（D12）。
//
// 只用于两件事：① 目录标注「可画图」；② 对话路径本地拒绝生图模型（ChatStream）。
// **不**参与 pool 选号（路由是渠道维度的）。
func IsImageModel(upstreamID string) bool {
	id := strings.TrimSpace(upstreamID)
	if id == "" {
		return false
	}
	for _, e := range staticCatalog {
		if e.Image && e.ID == id {
			return true
		}
	}
	tokens := nameTokens(id)
	for _, t := range tokens {
		for _, v := range visionInputTokens {
			if t == v {
				return false
			}
		}
	}
	for _, t := range tokens {
		for _, im := range imageOutputTokens {
			if t == im {
				return true
			}
		}
	}
	return false
}

// nameTokens 把模型名切成小写 token（按 `/ - _ . 空白` 分隔，丢空段）。
func nameTokens(id string) []string {
	fields := strings.FieldsFunc(strings.ToLower(id), func(r rune) bool {
		switch r {
		case '/', '-', '_', '.', ' ':
			return true
		}
		return false
	})
	return fields
}

// ---------------------------------------------------------------------------
// 目录：静态表 ∪ 上游 /v1/models（R42）
// ---------------------------------------------------------------------------

// FetchModels 目录来源 = 静态表 ∪ `GET {base}/v1/models`。
//
// **失败即降级，且恒返回非空**：
//   - 远程失败（404/5xx/非 JSON/超限）→ 打日志 + 只回静态表，error 恒 nil；
//   - 恒回静态表 ⇒ server 层的「目录为空 = 渠道没有模型」+ 5min 负缓存永远不会被
//     一次上游抖动触发（R133：一个坏 token 不该让整渠道目录归零）。
//
// 为什么不把远程错误往上抛：本渠道是多 token 池，目录本身**渠道维度**可用（静态表
// 就够路由），把错误抛上去只会让 models 面进入负缓存并把渠道从面板上抹掉。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	remote, err := c.fetchRemoteModels(a)
	if err != nil {
		log.Printf("modelscope: /v1/models 拉取失败，降级为静态目录 kind=%s err=%v", c.Kind, err)
	}
	return mergeModels(StaticModels(), remote), nil
}

// fetchRemoteModels 拉取并宽容解析上游目录（只声明上游真的写了的字段）。
func (c *Client) fetchRemoteModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), modelsTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, modelsPath, a, nil)
	if err != nil {
		return nil, err
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, modelsBodyCap+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > modelsBodyCap {
		return nil, &provider.Error{
			Kind:   provider.ErrServer,
			Status: resp.StatusCode,
			Msg:    "目录响应体超过 8MiB 上限（明确失败，不返回半截清单）",
		}
	}
	if resp.StatusCode >= 400 {
		// R79：≥400 一律类型化，否则任何 `errors.As` 自愈链都拿不到分类。
		return nil, &provider.Error{
			Kind:   c.Classify(resp.StatusCode, string(raw)),
			Status: resp.StatusCode,
			Msg:    strings.TrimSpace(string(raw)),
		}
	}
	return parseModelList(raw)
}

// parseModelList 宽容解析目录：接受 `{"data":[…]}`（OpenAI 形状）与顶层数组两种形态。
func parseModelList(raw []byte) ([]provider.ModelInfo, error) {
	var top any
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, &provider.Error{Kind: provider.ErrServer, Msg: "目录响应不是合法 JSON: " + err.Error()}
	}
	entries := listEntries(top)
	out := make([]provider.ModelInfo, 0, len(entries))
	for _, e := range entries {
		if mi, ok := modelInfoFromRemote(e); ok {
			out = append(out, mi)
		}
	}
	return out, nil
}

// listEntries 从顶层结构里取出条目列表（`data` / `models` / 顶层数组）。
func listEntries(top any) []map[string]any {
	switch v := top.(type) {
	case []any:
		return mapsOf(v)
	case map[string]any:
		// 只认 OpenAI 列表形状的 `data`：本渠道实测（2026-11-14）就是它，
		// 不猜 `models`/`result` 这类别名（猜不到的别名等于多一条静默失效的路径）。
		if arr, ok := v["data"].([]any); ok {
			return mapsOf(arr)
		}
	}
	return nil
}

func mapsOf(arr []any) []map[string]any {
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// modelInfoFromRemote 把一条上游目录条目转成 ModelInfo。
//
// R49/R119：**未探测到就一律不声明**——这里每个能力字段都是「上游显式写了才置位」，
// 不做任何按模型名猜能力的动作（唯一的命名判定是 IsImageModel 的「能出图」，
// 那是路由分类，不是能力声明）。
func modelInfoFromRemote(e map[string]any) (provider.ModelInfo, bool) {
	id, ok := objString(e, "id", "model", "model_id", "name")
	if !ok || strings.TrimSpace(id) == "" {
		return provider.ModelInfo{}, false
	}
	id = strings.TrimSpace(id)
	mi := provider.ModelInfo{ID: id, Name: id}

	if v, ok := objInt(e, "context_length", "context_window", "max_context_length", "max_model_len", "max_context_tokens"); ok && v > 0 {
		mi.ContextWindow = v
		mi.ContextFromAPI = true
	}
	if v, ok := objInt(e, "max_output_tokens", "max_tokens", "max_new_tokens", "max_completion_tokens"); ok && v > 0 {
		mi.MaxTokens = v
		mi.ContextFromAPI = true
	}
	if v, ok := objBool(e, "supports_tools", "tools", "function_calling", "tool_calls"); ok && v {
		mi.SupportsTools = true
	}
	if v, ok := objBool(e, "supports_reasoning", "reasoning", "thinking"); ok && v {
		mi.SupportsReasoning = true
	}
	// 思考档位：扁平 `reasoning_efforts` 与嵌套 `reasoning.efforts` 两种形态都接。
	efforts := []string(nil)
	if got, ok := objStrings(e, "reasoning_efforts", "reasoning_effort"); ok {
		efforts = got
	} else if sub, ok := e["reasoning"].(map[string]any); ok {
		if got, ok := objStrings(sub, "efforts", "supported_efforts"); ok {
			efforts = got
		}
	}
	if clean := provider.CleanEfforts(efforts); len(clean) > 0 {
		mi.SupportsReasoning = true
		mi.ReasoningEfforts = clean
		if def, ok := objString(e, "default_reasoning_effort", "default_effort"); ok {
			mi.DefaultReasoningEffort = def
		}
	}
	// SupportsImages 只表示「能看图」（视觉输入），与「能出图」是两个维度。
	if v, ok := objBool(e, "supports_images", "supports_vision", "vision", "multimodal"); ok && v {
		mi.SupportsImages = true
	}
	if mods, ok := objStrings(e, "input_modalities", "modalities"); ok && hasAnyFold(mods, "image") {
		mi.SupportsImages = true
	}
	if tags, ok := objStrings(e, "tags", "capabilities"); ok && hasAnyFold(tags, "vision", "image-input") {
		mi.SupportsImages = true
	}
	if caps, ok := objStrings(e, "capabilities"); ok && hasAnyFold(caps, "tools", "function_calling", "tool-calls") {
		mi.SupportsTools = true
	}
	// 「能出图」= 静态表 image:true ∪ 命名启发式（D12）：上游目录**不带**该字段，
	// 所以这条是路由分类结果而不是上游声明（不得写成「上游说有」）。
	mi.SupportsImageGeneration = IsImageModel(id)
	return mi, true
}

// mergeModels 静态表 ∪ 远程目录：静态表条目保序在前（含无上游目录时的可用性），
// 远程独有的条目按 ID 排序追加在后；同 ID 时以远程的上游声明**补全**静态条目
// （静态表刻意不带能力字段，远程说了就采纳）。
func mergeModels(static, remote []provider.ModelInfo) []provider.ModelInfo {
	out := make([]provider.ModelInfo, 0, len(static)+len(remote))
	// at 只记「已入 out 的静态条目位置」；远程独有的重复 ID 由 seen 拦掉
	// ——用同一个 map 记两者会让「远程独有条目」取到 -1 这样的假位置（越界 panic）。
	at := make(map[string]int, len(static))
	for _, mi := range static {
		if mi.ID == "" {
			continue
		}
		if _, dup := at[mi.ID]; dup {
			continue
		}
		at[mi.ID] = len(out)
		out = append(out, mi)
	}
	var extras []provider.ModelInfo
	extraAt := make(map[string]int, len(remote))
	for _, mi := range remote {
		if mi.ID == "" {
			continue
		}
		// 重复 ID 一律**合并**（不是丢弃）：静态条目补进 out 原位，远程独有条目补进
		// extras 原位；两张表分开记下标，任何一路都不会拿到 -1 这种假位置。
		if i, ok := at[mi.ID]; ok {
			out[i] = fillFrom(out[i], mi)
			continue
		}
		if j, ok := extraAt[mi.ID]; ok {
			extras[j] = fillFrom(extras[j], mi)
			continue
		}
		extraAt[mi.ID] = len(extras)
		extras = append(extras, mi)
	}
	sort.Slice(extras, func(i, j int) bool { return extras[i].ID < extras[j].ID })
	return append(out, extras...)
}

// fillFrom 用远程条目补全静态条目：静态已有的字段不被覆盖（零值才补），
// 能力标志只做「或」（静态表从不声明能力，故等价于采纳远程声明）。
func fillFrom(dst, src provider.ModelInfo) provider.ModelInfo {
	if dst.Name == "" {
		dst.Name = src.Name
	}
	adopted := false
	if dst.ContextWindow == 0 && src.ContextWindow > 0 {
		dst.ContextWindow = src.ContextWindow
		adopted = true
	}
	if dst.MaxTokens == 0 && src.MaxTokens > 0 {
		dst.MaxTokens = src.MaxTokens
		adopted = true
	}
	// ContextFromAPI 的口径是「容量字段来自上游接口」（两个字段共用一词），
	// 因此只要有一个容量字段被采纳就跟着搬，避免出现「MaxTokens 来自上游却标成估算」。
	if adopted && src.ContextFromAPI {
		dst.ContextFromAPI = true
	}
	dst.SupportsTools = dst.SupportsTools || src.SupportsTools
	dst.SupportsImages = dst.SupportsImages || src.SupportsImages
	dst.SupportsImageGeneration = dst.SupportsImageGeneration || src.SupportsImageGeneration
	dst.SupportsReasoning = dst.SupportsReasoning || src.SupportsReasoning
	if len(dst.ReasoningEfforts) == 0 {
		dst.ReasoningEfforts = src.ReasoningEfforts
		dst.DefaultReasoningEffort = src.DefaultReasoningEffort
	}
	return dst
}

// ---------------------------------------------------------------------------
// 宽容取值小工具（上游字段名各家不一，只取已见过的保守别名）
// ---------------------------------------------------------------------------

func objString(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

func objBool(m map[string]any, keys ...string) (bool, bool) {
	for _, k := range keys {
		if v, ok := m[k].(bool); ok {
			return v, true
		}
	}
	return false, false
}

func objInt(m map[string]any, keys ...string) (int64, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case float64:
			return int64(v), true
		case int64:
			return v, true
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return n, true
			}
		case string:
			if n, err := parseInt64(v); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

func objStrings(m map[string]any, keys ...string) ([]string, bool) {
	for _, k := range keys {
		switch v := m[k].(type) {
		case []any:
			out := make([]string, 0, len(v))
			for _, item := range v {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
				if sub, ok := item.(map[string]any); ok { // [{type:"image"}] 形态
					if s, ok := objString(sub, "type", "name", "id"); ok {
						out = append(out, s)
					}
				}
			}
			if len(out) > 0 {
				return out, true
			}
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return strings.Split(s, ","), true
			}
		}
	}
	return nil, false
}

func hasAnyFold(list []string, wants ...string) bool {
	for _, got := range list {
		for _, want := range wants {
			if strings.EqualFold(strings.TrimSpace(got), want) {
				return true
			}
		}
	}
	return false
}

func parseInt64(s string) (int64, error) { return strconv.ParseInt(strings.TrimSpace(s), 10, 64) }
