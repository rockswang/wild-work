package server

import "wild-work/internal/provider"

// cacheusage.go 从上游 usage 帧提取「缓存命中 token」——面板「缓存利用率」的取数点。
//
// 取数规则已收敛到 internal/provider.ExtractUsageCache（唯一真源，因为
// /v1/responses 与 /v1/messages 两个转换器也要用同一套优先级）；本文件只保留
// 面板专用类型与适配。实测依据与三条语义仍见下方。
//
// 实测依据（2026-10-12，24 份真实上游快照，见 AGENTS.md §8.5 devdump）：
//
//	渠道族                     字段                                          命中示例
//	workbuddy / workbuddyai    prompt_cache_hit_tokens（也回 cached_tokens）  97/103
//	traework / traecode        cache_read_input_tokens                        40960/41610
//	qodercn / qodercom /       prompt_tokens_details.cached_tokens            本轮 0
//	  qwenwork / cline / oczen
//	monkeycode                 input_tokens_details.cached_tokens             本轮 0
//	raccoon                    prompt_tokens_details 为**空对象** {}          → 未上报
//	glm / loomy                整条流无 usage 字段                            → 未上报
//
// 三条必须遵守的语义（否则面板会显示错数）：
//
//  1. **命中数是 prompt_tokens 的子集**，不是并列项。workbuddy 系实测
//     `hit + miss == prompt_tokens`；traework 实测 `total = prompt + completion`
//     且 cache_read(40960) < prompt(41610) ⇒ prompt_tokens 已是**总输入**。
//     故分母直接用 PT，**不可**做 `PT + cacheRead`（那是 Anthropic **官方**口径，
//     与本仓实测的 trae 口径相反）。副产品：利用率恒 ∈ [0,1]，坏数据天然被钳住。
//
//  2. **「字段存在但为 0」与「字段不存在」必须区分**。前者是有意上报的「未命中」
//     （应当显示 0%），后者是上游不提供该维度（应当显示「—」）。raccoon 下发空对象
//     `{}`，即属后者——若只判「有 details 对象就算上报」，面板会给它恒显 0%，
//     把「不知道」伪装成「命中率为零」。
//
//  3. **绝不用 usage 里那些跨渠道不同的重复字段**（workbuddy 系同一帧里同时出现
//     `cached_tokens:0` 与 `prompt_cache_hit_tokens:97`，二者矛盾且前者恒为 0）。
//     只认各自渠道族的主字段，保证取值唯一确定。

// cacheUsage 提取结果。
//
//	reported=false → 上游不提供该维度，面板显示「—」（优雅降级；不得显示 0%）
//	reported=true  → cached 可信（可能为 0，即本轮无命中），面板显示百分比
type cacheUsage struct {
	cached   int64
	reported bool
}

// cacheUsageOf 从 usage 帧提取缓存命中数。
//
// 传入的 u 是**上游原始 usage 对象**（OpenAI 形状；traework 的 SOLO token_usage 亦兼容）。
// u 为 nil 或非 map 时返回「未上报」。
//
// **实现在 internal/provider.ExtractUsageCache**（唯一真源）：本函数只做类型适配。
// 抽取原因：/v1/responses 与 /v1/messages 两个转换器各写了一套取数优先级且已开始
// 漂移（转换器先读顶层裸 cached_tokens，而它在 workbuddy 系实测恒 0 是噪声，
// 会把真值掩盖）。收敛到一处后，面板与三接口对外声明的缓存数字必然同源。
func cacheUsageOf(u map[string]any) cacheUsage {
	c := provider.ExtractUsageCache(u)
	return cacheUsage{cached: c.Cached, reported: c.CachedReported}
}
