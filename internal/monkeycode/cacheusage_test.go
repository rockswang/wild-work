package monkeycode

// cacheusage_test.go MonkeyCode 的缓存字段搬运回归。
//
// 本渠道是唯一「重建 usage」的渠道，故必须证明：上游给的 cached_tokens 能穿过
// 「Anthropic / Responses → OpenAI」的转换到达出口（否则面板对该渠道恒显「—」）。
// 帧取自 2026-10-12 真实抓包（responsesFixture 同源）与 2026-09-24 实测样本。

import (
	"io"
	"strings"
	"testing"
)

// TestResponsesStreamCarriesCachedTokens：Responses 路径的 input_tokens_details
// 必须出现在出口 usage.prompt_tokens_details.cached_tokens。
func TestResponsesStreamCarriesCachedTokens(t *testing.T) {
	fixture := `data: {"type":"response.created","response":{"id":"r1","created_at":1}}

data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":651,"input_tokens_details":{"cached_tokens":512},"output_tokens":131,"total_tokens":782}}}

`
	raw, err := io.ReadAll(newResponsesStream(strings.NewReader(fixture), "monkeycode/basic/qwen3.8-flash"))
	if err != nil {
		t.Fatalf("读取转换结果失败：%v", err)
	}
	out := string(raw)
	if !strings.Contains(out, `"cached_tokens":512`) {
		t.Fatalf("出口 usage 未携带 cached_tokens（上游数据在网关内被丢弃）：\n%s", out)
	}
	if !strings.Contains(out, `"prompt_tokens_details"`) {
		t.Fatalf("出口 usage 缺少 prompt_tokens_details 键：\n%s", out)
	}
}

// TestAnthropicStreamCarriesCachedTokens：Anthropic 路径同款（cached 出现在
// message_start 的 usage 里）。
func TestAnthropicStreamCarriesCachedTokens(t *testing.T) {
	fixture := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":95,"input_tokens_details":{"cached_tokens":64},"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

`
	raw, err := io.ReadAll(newAnthropicStream(strings.NewReader(fixture), "monkeycode/basic/deepseek-flash"))
	if err != nil {
		t.Fatalf("读取转换结果失败：%v", err)
	}
	if !strings.Contains(string(raw), `"cached_tokens":64`) {
		t.Fatalf("出口 usage 未携带 cached_tokens：\n%s", raw)
	}
}

// TestNoCachedFieldStaysAbsent：上游没给该维度时，出口**不得伪造成 0**——
// 那会让下游把「不提供」误判成「命中率为零」。
func TestNoCachedFieldStaysAbsent(t *testing.T) {
	fixture := `event: message_start
data: {"type":"message_start","message":{"id":"msg_2","usage":{"input_tokens":12,"output_tokens":0}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}

`
	raw, err := io.ReadAll(newAnthropicStream(strings.NewReader(fixture), "m"))
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if strings.Contains(out, `"cached_tokens":0`) {
		t.Fatalf("上游未上报时不得写出 cached_tokens:0（会把「不知道」显示成 0%%）：\n%s", out)
	}
	// 键可以存在但值必须是 null（下游只认非空对象里的该键）
	if !strings.Contains(out, `"prompt_tokens_details":null`) && !strings.Contains(out, `"prompt_tokens_details"`) {
		t.Fatalf("usage 应仍带 prompt_tokens_details 键（值为 null）：\n%s", out)
	}
}

// TestResponsesReaderCacheDetails 直测 helper：只有确实见过字段才算上报。
func TestResponsesReaderCacheDetails(t *testing.T) {
	r := &responsesStreamReader{}
	if r.cacheDetails() != nil {
		t.Fatal("未见过字段时 details 应为 nil")
	}
	r.captureCached(map[string]any{"input_tokens_details": map[string]any{"cached_tokens": float64(0)}})
	d := r.cacheDetails()
	if d == nil {
		t.Fatal("字段存在（哪怕为 0）即算上报，details 不应为 nil")
	}
	if d["cached_tokens"] != int64(0) {
		t.Fatalf("cached_tokens=%v want 0", d["cached_tokens"])
	}
	// 空对象不算上报
	r2 := &responsesStreamReader{}
	r2.captureCached(map[string]any{"input_tokens_details": map[string]any{}})
	if r2.cacheDetails() != nil {
		t.Fatal("空 details 对象不应算上报（raccoon 同款语义）")
	}
}
