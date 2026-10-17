// cacheusage.go ZCode 渠道的 usage / 缓存计量搬运。
//
// 本渠道与 monkeycode/minimax 属同一形态：**上游是 Anthropic 形状**，而 sse.go 的
// 转换器会**重建** OpenAI 三件套 usage（finish() 只拼 prompt/completion/total）。
// 若不显式搬运缓存字段，上游给的缓存数据在网关内部就被丢掉，面板的缓存利用率
// 对该渠道恒为「—」（AGENTS.md 不变量 24 的落地场景）。
//
// 字段来源（协议 §1.5 的事件表）：
//
//	message_start.usage.input_tokens                         → prompt 里的「未缓存输入」
//	message_start.usage.cache_read_input_tokens              → 命中（读）
//	message_start.usage.cache_creation_input_tokens          → 写入
//	message_delta.usage.output_tokens                        → 输出总量（**累积值，不是增量**）
//	message_delta.usage.output_tokens_details.thinking_tokens → 思考 token（output 的子集）
//
// 两条必须守住的语义：
//
//  1. **只在裸字段存在时上报**：字段缺失 = 上游不提供该维度 ⇒ `prompt_tokens_details`
//     整体不出现（面板显示「—」）；字段存在且为 0 = 上游明确的「本轮未命中」⇒ 报 0。
//     绝不把「不提供」显示成 0%（AGENTS.md 不变量 24）。
//
//  2. **prompt_tokens 必须是「总输入」**：Anthropic 的 `input_tokens` 是**未命中、也未
//     写入缓存**的那部分，命中数是它与 cache_* 之和的**子集**关系。
//     所以这里让 prompt_tokens = input + cache_read + cache_creation —— 于是
//     「命中数是 prompt_tokens 的子集」这条跨渠道不变量同样成立（利用率恒 ∈ [0,1]），
//     且 internal/gateway 的 Anthropic 回映射会用
//     `input_tokens = prompt_tokens − cached − write` 做减法，正好还原上游原值。
package zcode

// captureUsage 吸收一帧 usage（message_start 与 message_delta 都会带）。
//
// ⚠ 用 **max 语义**而不是累加：Anthropic 的 usage 是**累积值**（message_delta 给的是
// 最终总量，不是本帧增量），累加会把 output 翻倍。
func (s *anthropicStreamReader) captureUsage(u map[string]any) {
	if v, ok := numOf(u, "input_tokens"); ok {
		s.inTokens = max64(s.inTokens, v)
	}
	if v, ok := numOf(u, "output_tokens"); ok {
		s.outTokens = max64(s.outTokens, v)
	}
	// 缓存命中（读）：**键存在**才算上报（值 0 也是有效上报）。
	if v, ok := numOf(u, "cache_read_input_tokens"); ok {
		s.cachedTokens = max64(s.cachedTokens, v)
		s.cacheReported = true
	}
	// 缓存写入：Anthropic 独有维度，独立标注上报状态。
	if v, ok := numOf(u, "cache_creation_input_tokens"); ok {
		s.writeTokens = max64(s.writeTokens, v)
		s.writeReported = true
	}
	if d, ok := u["output_tokens_details"].(map[string]any); ok {
		if v, ok := numOf(d, "thinking_tokens"); ok {
			s.thinkingTokens = max64(s.thinkingTokens, v)
			s.thinkingReported = true
		}
	}
}

// promptTokens 返回**总输入**（口径见文件头第 2 条）：
// input_tokens + cache_read_input_tokens + cache_creation_input_tokens。
func (s *anthropicStreamReader) promptTokens() int64 {
	return s.inTokens + s.cachedTokens + s.writeTokens
}

// cacheDetails 生成回填进 OpenAI usage 的 prompt_tokens_details。
//
// **上游没报过就返回 nil**（该键整条不出现）——「字段不存在」与「命中 0」必须区分：
// 前者是上游不提供该维度（面板显示「—」），后者是明确的「本轮未命中」（显示 0%）。
func (s *anthropicStreamReader) cacheDetails() map[string]any {
	if !s.cacheReported && !s.writeReported {
		return nil
	}
	d := map[string]any{}
	if s.cacheReported {
		d["cached_tokens"] = s.cachedTokens
	}
	if s.writeReported {
		d["cache_write_tokens"] = s.writeTokens
	}
	return d
}
