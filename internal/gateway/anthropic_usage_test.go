// anthropic_usage_test.go issue #104（/v1/messages 侧）：缓存计量与 Anthropic 语义换算。
//
// Claude Code 走这条路由。它读 Anthropic 命名（cache_read_input_tokens /
// cache_creation_input_tokens）并假定 `input_tokens` 是**未命中、未写入**的那部分
// 输入（客户端自行相加还原总输入）。内层给的 `prompt_tokens` 却**已是总输入**
// （命中是其子集）——两者语义相反，故必须做减法。
//
// 两个方向的错误都要防：
//   - 不下发 cache_* 字段 → Claude Code 把全部输入按未缓存计价（issue #104 的费用高估）；
//   - 下发了却不从 input_tokens 减 → 客户端求和得到近两倍总输入（同样错，方向相反）。
package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// anthropicUsageOf 提取 /v1/messages 非流式响应里的 usage。
func anthropicUsageOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	u, _ := resp["usage"].(map[string]any)
	return u
}

// TestAnthropicUsageSplitsCacheFromInput issue #104 核心：trae 真帧形状下，
// input_tokens 必须扣掉命中与写入部分，且三者相加 == 原始 prompt_tokens。
func TestAnthropicUsageSplitsCacheFromInput(t *testing.T) {
	// trae 真帧：prompt_tokens 是总输入（30133），cache_read=28672、creation=0
	inner := chatFixtureHandler(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":30133,"completion_tokens":122,"total_tokens":30255,` +
		`"prompt_tokens_details":{"cached_tokens":28672,"cache_write_tokens":0}}}`)
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	u := anthropicUsageOf(t, rec)
	in := u["input_tokens"].(float64)
	read := u["cache_read_input_tokens"].(float64)
	write := u["cache_creation_input_tokens"].(float64)
	if read != 28672 {
		t.Errorf("cache_read_input_tokens = %v want 28672", read)
	}
	if write != 0 {
		t.Errorf("cache_creation_input_tokens = %v want 0", write)
	}
	// ⚠️ 关键断言：input_tokens 是**未命中**部分，不是总输入
	if in != 30133-28672 {
		t.Errorf("input_tokens = %v want %d（必须扣掉命中，否则 Claude Code 求和会双计）",
			in, 30133-28672)
	}
	// 三段相加 == 原始总输入（口径守恒）
	if got := in + read + write; got != 30133 {
		t.Errorf("input+read+write = %v want 30133（总量必须守恒）", got)
	}
}

// TestAnthropicUsageWriteSideAlsoSubtracted 写入侧同样不计入 input_tokens
// （Anthropic 口径里它是独立的一段，客户端会自行相加）。
func TestAnthropicUsageWriteSideAlsoSubtracted(t *testing.T) {
	inner := chatFixtureHandler(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1000,"completion_tokens":10,"total_tokens":1010,` +
		`"prompt_tokens_details":{"cached_tokens":300,"cache_write_tokens":200}}}`)
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	u := anthropicUsageOf(t, rec)
	in := u["input_tokens"].(float64)
	read := u["cache_read_input_tokens"].(float64)
	write := u["cache_creation_input_tokens"].(float64)
	if in != 500 || read != 300 || write != 200 {
		t.Errorf("拆分错误: in=%v read=%v write=%v，want 500/300/200", in, read, write)
	}
	if in+read+write != 1000 {
		t.Errorf("总量不守恒: %v", in+read+write)
	}
}

// TestAnthropicUsageExplicitZeroKept 「上游明确报告 0 命中」必须下发 0，
// 而不是省略该字段（Claude Code 据此区分「未命中」与「该维度不可用」）。
func TestAnthropicUsageExplicitZeroKept(t *testing.T) {
	inner := chatFixtureHandler(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":549,"completion_tokens":5,"total_tokens":554,` +
		`"prompt_tokens_details":{"cached_tokens":0}}}`)
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	u := anthropicUsageOf(t, rec)
	if v, has := u["cache_read_input_tokens"]; !has || v.(float64) != 0 {
		t.Errorf("cache_read_input_tokens = %v (has=%v) want 显式 0", v, has)
	}
	if u["input_tokens"].(float64) != 549 {
		t.Errorf("命中为 0 时 input_tokens 不应变动: %v", u["input_tokens"])
	}
}

// TestAnthropicUsageNoCacheFieldsUnchanged 上游不提供缓存维度时不得凭空造字段
// （不变量 24：把「不知道」当「0%」是错的）。
func TestAnthropicUsageNoCacheFieldsUnchanged(t *testing.T) {
	inner := chatFixtureHandler(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	u := anthropicUsageOf(t, rec)
	if _, has := u["cache_read_input_tokens"]; has {
		t.Errorf("不得凭空造出 cache_read_input_tokens: %v", u)
	}
	if _, has := u["cache_creation_input_tokens"]; has {
		t.Errorf("不得凭空造出 cache_creation_input_tokens: %v", u)
	}
	if u["input_tokens"].(float64) != 10 {
		t.Errorf("input_tokens = %v want 10（无缓存字段时不得变动）", u["input_tokens"])
	}
}

// TestAnthropicUsageNeverNegative 坏数据（cached > prompt）不得产生负 input_tokens
// （负值会让客户端算出负数金额，比不报更糟）。
func TestAnthropicUsageNeverNegative(t *testing.T) {
	inner := chatFixtureHandler(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":40,"completion_tokens":5,"total_tokens":45,` +
		`"prompt_tokens_details":{"cached_tokens":100,"cache_write_tokens":50}}}`)
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)

	u := anthropicUsageOf(t, rec)
	if in := u["input_tokens"].(float64); in < 0 {
		t.Errorf("input_tokens = %v 不得为负（坏数据须钳 0）", in)
	}
}

// TestAnthropicStreamUsageSplitsCache 流式 message_delta 的 usage 走同一映射，
// 必须同样完成拆分（Claude Code 流式会话主要看这里）。
func TestAnthropicStreamUsageSplitsCache(t *testing.T) {
	inner := chatFixtureHandler(`data: {"id":"c1","object":"chat.completion.chunk","model":"up","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
		`data: {"id":"c1","object":"chat.completion.chunk","model":"up","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":1000,"completion_tokens":10,"total_tokens":1010,` +
		`"prompt_tokens_details":{"cached_tokens":400,"cache_write_tokens":100}}}` + "\n\n" +
		"data: [DONE]\n\n")
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/messages", `{"model":"claude-sonnet-4-5","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	var found map[string]any
	for _, line := range splitSSEDataLines(rec.Body.String()) {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev["type"] == "message_delta" {
			found, _ = ev["usage"].(map[string]any)
		}
	}
	if found == nil {
		t.Fatalf("未找到 message_delta 的 usage:\n%s", rec.Body.String())
	}
	in := found["input_tokens"].(float64)
	if in != 500 {
		t.Errorf("流式 input_tokens = %v want 500（1000−400−100）", in)
	}
	if found["cache_read_input_tokens"].(float64) != 400 {
		t.Errorf("流式 cache_read_input_tokens = %v want 400", found["cache_read_input_tokens"])
	}
}

// splitSSEDataLines 提取 SSE 里所有 data: 行的载荷（跳过 [DONE]）。
func splitSSEDataLines(body string) []string {
	var out []string
	for _, line := range splitLines(body) {
		if len(line) > 6 && line[:6] == "data: " {
			p := line[6:]
			if p != "[DONE]" {
				out = append(out, p)
			}
		}
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, trimCR(s[start:i]))
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, trimCR(s[start:]))
	}
	return out
}

func trimCR(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\r' {
		return s[:len(s)-1]
	}
	return s
}
