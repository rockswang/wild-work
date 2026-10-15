// usage.go 上游 usage 的 OpenAI 形状归一化（跨渠道共用）。
//
// 为什么需要它：本仓上游的缓存计量字段分两族（实测口径见 internal/server/cacheusage.go）：
//
//	OpenAI 族   prompt_cache_hit_tokens（workbuddy 系）/ prompt_tokens_details.cached_tokens
//	Anthropic 族 cache_read_input_tokens / cache_creation_input_tokens（trae 系）
//
// 内层（internal/traework 等）过去把上游 usage **原样透传**，于是 trae 系下发的是
// Anthropic 字段名。下游按 OpenAI 规范解析（issue #104 的 DSH 实测：
// `prompt_tokens_details?.cached_tokens ?? prompt_cache_hit_tokens ?? 0`），两条都命中不了
// ⇒ 命中率恒 0%、prompt 全按未缓存计价（以 DSH 价表 input 0.165 vs cacheRead 0.0033 计，
// 费用高估约 50 倍）。**面板口径不受影响**（cacheUsageOf 早已两族都读），
// 坏的只有下发给客户端的 usage。
//
// 归一化方向是「补齐 OpenAI 形状」而非「让下游兼容 Anthropic」：后者要改所有客户端，
// 且与 OpenAI SDK / AI SDK / DSH 的实际读法相悖（它们都读 OpenAI 形状）。
//
// 语义约束（与 cacheUsageOf 同源，不可违反）：
//   - **命中数是 prompt_tokens 的子集**（trae 实测 total=prompt+completion 且
//     read(40960) < prompt(41610)）⇒ 子字段只做搬运，**不得**重算 prompt_tokens。
//   - **原始字段一律保留**：已有依赖 cache_read_input_tokens 的客户端不受影响。
//   - **只在缺失时补写**：上游若已给 prompt_tokens_details.cached_tokens，不覆盖
//     （避免把 0 写成有效值或改变既有语义）。
//   - **认「字段存在」而非「值 > 0」**：0 是有效值（本轮未命中），
//     只看 >0 会把「明确的 0」丢成「未上报」。
package provider

import "math"

// NormalizeUsageShape 就地补齐 usage 的 OpenAI 缓存字段，返回是否发生改动。
//
// 映射（仅在目标子字段缺失时写入）：
//
//	prompt_tokens_details.cached_tokens       ← prompt_cache_hit_tokens
//	                                          ← cached_tokens（顶层裸字段，仅当无上者）
//	                                          ← cache_read_input_tokens（Anthropic 族）
//	prompt_tokens_details.cache_write_tokens  ← cache_creation_input_tokens（Anthropic 族）
//
// 顶层 `cached_tokens` 排在 `prompt_cache_hit_tokens` 之后：workbuddy 系同一帧里
// 两者并存且顶层那份**实测恒 0**（见 cacheusage.go 语义 3），优先取前者的真值。
//
// u 为 nil 或空 map 时不动。本函数**不新增** prompt_tokens_details
// （仅当确有可搬运的缓存字段时才建），避免给不提供该维度的渠道凭空造出 0。
func NormalizeUsageShape(u map[string]any) bool {
	if len(u) == 0 {
		return false
	}
	d, _ := u["prompt_tokens_details"].(map[string]any)
	changed := false

	// cached：命中（读）侧。依次尝试三种来源，取第一个「存在且为数值」的。
	if _, has := numAt(d, "cached_tokens"); !has {
		for _, k := range []string{"prompt_cache_hit_tokens", "cached_tokens", "cache_read_input_tokens"} {
			if v, ok := numAt(u, k); ok {
				d = ensureSub(u, "prompt_tokens_details", d)
				d["cached_tokens"] = v
				changed = true
				break
			}
		}
	}
	// cache_write：写入侧（Anthropic 独有；OpenAI 族无对应顶层字段）。
	if _, has := numAt(d, "cache_write_tokens"); !has {
		if v, ok := numAt(u, "cache_creation_input_tokens"); ok {
			d = ensureSub(u, "prompt_tokens_details", d)
			d["cache_write_tokens"] = v
			changed = true
		}
	}
	return changed
}

// numAt 取 m[key] 并归一为数值：**键存在且是有限非负数值**才算命中。
//
// 与 server.presentNum 的区别：那边返回 int64（面板口径），这边返回原值类型
// （float64/int），因为要原样写回下游 usage，不能把 1234.5 截成 1234。
// 负数与 NaN/Inf 视为坏数据，不搬运（仍算「无效」，交由上层保持原样）。
func numAt(m map[string]any, key string) (any, bool) {
	if m == nil {
		return nil, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return nil, false
	}
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
			return nil, false
		}
		return n, true
	case int:
		if n < 0 {
			return nil, false
		}
		return n, true
	case int64:
		if n < 0 {
			return nil, false
		}
		return n, true
	}
	return nil, false
}

// ensureSub 取（或按需创建）u 下的子对象，并复用已解析的引用避免重复查找。
func ensureSub(u map[string]any, key string, cur map[string]any) map[string]any {
	if cur != nil {
		return cur
	}
	if d, ok := u[key].(map[string]any); ok {
		return d
	}
	d := map[string]any{}
	u[key] = d
	return d
}

// ---------------------------------------------------------------------------
// 缓存计量提取（**唯一真源**）
// ---------------------------------------------------------------------------
//
// 本函数取代过去散在三处的取数逻辑（面板 internal/server/cacheusage.go、
// /v1/responses 与 /v1/messages 两个转换器），它们各自实现了一套优先级且**已经开始
// 漂移**（转换器先读顶层裸 cached_tokens，而该字段在 workbuddy 系实测恒 0 是噪声，
// 会让真值被噪声掩盖；面板口径则正确地先读 prompt_cache_hit_tokens）。
// 收敛到一处后，面板与三接口对外声明的缓存数字必然同源。

// UsageCache 缓存计量提取结果。
//
//	CachedReported=false → 上游不提供命中维度（面板显示「—」、转换器不下发该字段）
//	CachedReported=true  → Cached 可信（可能为 0，即「本轮未命中」）
//
// Write（缓存写入侧）独立标注上报状态：OpenAI 族没有这个概念，只在 Anthropic 族出现。
type UsageCache struct {
	Cached         int64
	CachedReported bool
	Write          int64
	WriteReported  bool
}

// ExtractUsageCache 从上游 usage 提取缓存计量（命中/写入）。
//
// 取数优先级（与 AGENTS.md 不变量 24 一致，不可随意调整）：
//
//	命中 cached_tokens：
//	  ① prompt_cache_hit_tokens（workbuddy 系主字段；与 miss 互补且和 == prompt_tokens）
//	  ② cache_read_input_tokens（traework/traecode 的 Anthropic 命名）
//	  ③ prompt_tokens_details.cached_tokens / input_tokens_details.cached_tokens
//	     （必须**非空对象且确有该键**——raccoon 下发空 {} 属「不提供」）
//	写入 cache_write_tokens：
//	  ④ cache_creation_input_tokens（Anthropic 族）
//	  ⑤ prompt_tokens_details.cache_write_tokens
//
// **刻意不认顶层裸 `cached_tokens`**：workbuddy 系同一帧里 `prompt_cache_hit_tokens:97`
// 与 `cached_tokens:0` 并存，后者实测恒 0（噪声）。只认各渠道族主字段才能保证取值
// 唯一确定。
func ExtractUsageCache(u map[string]any) UsageCache {
	var c UsageCache
	if len(u) == 0 {
		return c
	}
	// ① workbuddy / workbuddyai
	if v, ok := numInt(u, "prompt_cache_hit_tokens"); ok {
		c.Cached, c.CachedReported = v, true
	}
	// ② traework / traecode
	if !c.CachedReported {
		if v, ok := numInt(u, "cache_read_input_tokens"); ok {
			c.Cached, c.CachedReported = v, true
		}
	}
	// ③ details 子对象（qodercn/qodercom/qwenwork/cline/oczen/monkeycode）
	if !c.CachedReported {
		for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
			d, ok := u[key].(map[string]any)
			if !ok || len(d) == 0 {
				continue
			}
			if v, ok := numInt(d, "cached_tokens"); ok {
				c.Cached, c.CachedReported = v, true
				break
			}
		}
	}
	// ④ Anthropic 写入侧
	if v, ok := numInt(u, "cache_creation_input_tokens"); ok {
		c.Write, c.WriteReported = v, true
	}
	// ⑤ OpenAI 形态的写入侧（极少见，兜底）
	if !c.WriteReported {
		for _, key := range []string{"prompt_tokens_details", "input_tokens_details"} {
			d, ok := u[key].(map[string]any)
			if !ok || len(d) == 0 {
				continue
			}
			if v, ok := numInt(d, "cache_write_tokens"); ok {
				c.Write, c.WriteReported = v, true
				break
			}
		}
	}
	return c
}

// numInt 取 m[key] 并归一为 int64；**键存在且可转非负数值**才算命中（负数钳 0 但算已上报）。
func numInt(m map[string]any, key string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		if n < 0 {
			return 0, true // 坏数据钳 0，但仍算「已上报」（不让整行退化成「—」）
		}
		return int64(n), true
	case int:
		if n < 0 {
			return 0, true
		}
		return int64(n), true
	case int64:
		if n < 0 {
			return 0, true
		}
		return n, true
	}
	return 0, false
}
