package monkeycode

// cacheusage.go MonkeyCode 渠道的缓存命中搬运。
//
// 背景：本渠道是**唯一**会重建 usage 的渠道——上游回的是 Anthropic / Responses 形状
// （`input_tokens` + `input_tokens_details.cached_tokens`），而 finish() 只拼
// OpenAI 三件套（prompt/completion/total）。若不显式搬运，上游给的缓存数据在网关
// 内部就丢了，面板的缓存利用率对该渠道恒为「—」（实测字段见
// monkeycode_test.go 的 responsesFixture：`"input_tokens_details":{"cached_tokens":512}`）。
//
// 两个 reader（Anthropic / Responses 两条路径）共用以下方法，置于各自 struct 的字段之上。

// captureCached 从上游 usage 对象提取缓存命中并累加。
//
// 判定：`input_tokens_details` 是**对象且确有 cached_tokens 键**才算「上报」——
// 与 internal/server/cacheusage.go 的语义 2 一致（区分「命中 0」与「不提供」）。
// 一次请求可能在 message_start 与 message_delta 各带一次 usage，故用累加。
func (s *anthropicStreamReader) captureCached(u map[string]any) {
	if v, ok := cachedOf(u); ok {
		s.cacheReported = true
		s.cachedTokens += v
	}
}

func (s *responsesStreamReader) captureCached(u map[string]any) {
	if v, ok := cachedOf(u); ok {
		s.cacheReported = true
		s.cachedTokens += v
	}
}

// cachedOf 取 input_tokens_details.cached_tokens；键存在才返回 ok=true。
func cachedOf(u map[string]any) (int64, bool) {
	d, ok := u["input_tokens_details"].(map[string]any)
	if !ok {
		return 0, false
	}
	if _, has := d["cached_tokens"]; !has {
		return 0, false
	}
	n := intField(d, "cached_tokens")
	if n < 0 {
		n = 0
	}
	return n, true
}

// cacheDetails 生成回填进 OpenAI usage 的 prompt_tokens_details。
// **上游没报过就返回 nil**——nil 会被 omitempty 语义留给调用方决定；这里返回 nil
// 时 finish() 仍会写入该键（值为 null），而下游 cacheUsageOf 只认「非空对象里
// 确有该键」，故 null 被正确判为「未上报」，不会伪造 0%。
func (s *anthropicStreamReader) cacheDetails() map[string]any {
	if !s.cacheReported {
		return nil
	}
	return map[string]any{"cached_tokens": s.cachedTokens}
}

func (s *responsesStreamReader) cacheDetails() map[string]any {
	if !s.cacheReported {
		return nil
	}
	return map[string]any{"cached_tokens": s.cachedTokens}
}
