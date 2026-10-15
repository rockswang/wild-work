// usage_cache_shape_test.go issue #104 回归：trae 系 usage 必须带 OpenAI 缓存字段。
//
// 背景（实测，2026-10-13）：trae 上游的 token_usage 用 Anthropic 命名
// （cache_read_input_tokens / cache_creation_input_tokens），内层原样透传后，
// 下游按 OpenAI 规范解析（DSH：`prompt_tokens_details?.cached_tokens
// ?? prompt_cache_hit_tokens ?? 0`）两条都读不到 ⇒ 命中率恒 0%、prompt 全按
// 未缓存计价（DSH 价表 input 0.165 vs cacheRead 0.0033 ⇒ 费用高估约 50 倍）。
//
// 本文件锁定两个出口（流式 chunk 与 Aggregate 聚合体）都补出 OpenAI 形状，
// 且**原始字段保留**、**prompt_tokens 不动**（命中是其子集）。
package traework

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// traeUsageFrame 真实帧形状（ref/TraeWork 网页版逆向分析备忘.md:254）。
const traeUsageFrame = `{"prompt_tokens":30133,"completion_tokens":122,"total_tokens":30255,` +
	`"reasoning_tokens":42,"cache_creation_input_tokens":0,"cache_read_input_tokens":28672}`

// usageDetailsOf 从 usage 值里取 prompt_tokens_details.cached_tokens。
func usageDetailsOf(t *testing.T, usage map[string]any) (int64, bool) {
	t.Helper()
	if usage == nil {
		t.Fatal("usage 为 nil")
	}
	d, ok := usage["prompt_tokens_details"].(map[string]any)
	if !ok {
		return 0, false
	}
	v, ok := d["cached_tokens"]
	if !ok {
		return 0, false
	}
	return int64(v.(float64)), true
}

// TestStreamUsageCarriesOpenAICacheFields 流式：末帧 usage 必须同时含
// OpenAI 形状（details.cached_tokens）与原始 Anthropic 字段。
func TestStreamUsageCarriesOpenAICacheFields(t *testing.T) {
	body := soloFrame("output", `{"response":"hi"}`) +
		soloFrame("token_usage", traeUsageFrame) +
		soloFrame("done", `{"finish_reason":"stop"}`)

	w := httptest.NewRecorder()
	var cbUsage map[string]any
	usage, err := streamOpts(w, strings.NewReader(body), nil, "deepseek-v4.1-flash",
		func(u map[string]any) { cbUsage = u })
	if err != nil {
		t.Fatalf("streamOpts: %v", err)
	}
	// ① 返回给调用方的 usage（记账 + 后续转协议都靠它）
	if got, ok := usageDetailsOf(t, usage); !ok || got != 28672 {
		t.Fatalf("返回的 usage 缺 details.cached_tokens（issue #104 的根因）: %v", usage)
	}
	// ② onUsage 回调（记账路径）拿到的也必须是归一化后的形状
	if got, ok := usageDetailsOf(t, cbUsage); !ok || got != 28672 {
		t.Fatalf("onUsage 回调未拿到 OpenAI 形状: %v", cbUsage)
	}
	// ③ 客户端收到的 chunk 里同样要有（这才是下游解析的对象）
	out := w.Body.String()
	if !strings.Contains(out, `"prompt_tokens_details"`) || !strings.Contains(out, `"cached_tokens":28672`) {
		t.Errorf("下发给客户端的 usage 缺 OpenAI 缓存字段:\n%s", out)
	}
	// ④ 原始字段保留（不破坏已有依赖 Anthropic 字段的客户端）
	if !strings.Contains(out, `"cache_read_input_tokens":28672`) {
		t.Errorf("原始 Anthropic 字段被删除:\n%s", out)
	}
	// ⑤ 子集不变量：命中 ≤ prompt_tokens
	if !strings.Contains(out, `"prompt_tokens":30133`) {
		t.Errorf("prompt_tokens 被改动（命中是其子集，不得重算）:\n%s", out)
	}
}

// TestAggregateUsageCarriesOpenAICacheFields 非流式聚合体同样要归一化。
func TestAggregateUsageCarriesOpenAICacheFields(t *testing.T) {
	body := soloFrame("output", `{"response":"hi"}`) +
		soloFrame("token_usage", traeUsageFrame) +
		soloFrame("done", `{"finish_reason":"stop"}`)

	resp, err := AggregateWithModel(strings.NewReader(body), "deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("AggregateWithModel: %v", err)
	}
	usage, _ := resp["usage"].(map[string]any)
	if got, ok := usageDetailsOf(t, usage); !ok || got != 28672 {
		t.Fatalf("非流式 usage 缺 details.cached_tokens: %v", usage)
	}
	// 序列化后确认真的下发给了客户端（map 直读不算，要过一遍 JSON）
	raw, _ := json.Marshal(resp)
	if !strings.Contains(string(raw), `"cached_tokens":28672`) {
		t.Errorf("聚合响应缺 OpenAI 缓存字段:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"cache_read_input_tokens":28672`) {
		t.Errorf("聚合响应丢了原始 Anthropic 字段:\n%s", raw)
	}
}

// TestStreamUsageNoCacheFieldsStaysClean 上游不发缓存字段时**不得**凭空造出
// prompt_tokens_details（否则面板/客户端会把「不知道」当成「命中 0%」，不变量 24）。
func TestStreamUsageNoCacheFieldsStaysClean(t *testing.T) {
	body := soloFrame("output", `{"response":"hi"}`) +
		soloFrame("token_usage", `{"prompt_tokens":21,"completion_tokens":142,"total_tokens":163}`) +
		soloFrame("done", `{"finish_reason":"stop"}`)

	w := httptest.NewRecorder()
	usage, err := streamOpts(w, strings.NewReader(body), nil, "m", nil)
	if err != nil {
		t.Fatalf("streamOpts: %v", err)
	}
	if _, has := usage["prompt_tokens_details"]; has {
		t.Errorf("不得凭空造出 prompt_tokens_details: %v", usage)
	}
	if strings.Contains(w.Body.String(), "prompt_tokens_details") {
		t.Errorf("下发帧里不该出现 prompt_tokens_details:\n%s", w.Body.String())
	}
}
