// usage_cache_test.go issue #104（gateway 侧）：/v1/responses 的缓存命中字段。
//
// 背景：内层把上游 usage 归一化后（trae 系补出 prompt_tokens_details.cached_tokens），
// responsesUsage 还必须把它映射到 Responses 规范自己的字段名
// `input_tokens_details.cached_tokens`，且不能因用 `> 0` 判定而丢掉「显式 0」。
//
// 两个真实缺陷在此锁定：
//  1. 旧实现先读**顶层裸 cached_tokens** —— workbuddy 系该字段实测恒 0 是噪声，
//     真值在 details / prompt_cache_hit_tokens（不变量 24）；
//  2. 旧实现用 `> 0` 判定 —— 把「上游明确报告本轮未命中（0）」丢成「未提供」，
//     下游只能显示「—」而不是 0%。
package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// responsesUsageOf 提取 /v1/responses 响应里的 usage。
func responsesUsageOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	u, _ := resp["usage"].(map[string]any)
	return u
}

// TestResponsesUsageMapsDetailsCachedTokens details 里的 cached_tokens
// 必须映射到 input_tokens_details.cached_tokens。
func TestResponsesUsageMapsDetailsCachedTokens(t *testing.T) {
	inner := chatFixtureHandler(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":30133,"completion_tokens":122,"total_tokens":30255,` +
		`"prompt_tokens_details":{"cached_tokens":28672,"cache_write_tokens":0}}}`)
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

	u := responsesUsageOf(t, rec)
	d, ok := u["input_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("缺 input_tokens_details: %v", u)
	}
	if d["cached_tokens"] != float64(28672) {
		t.Errorf("cached_tokens = %v want 28672", d["cached_tokens"])
	}
	// 未声明的 cache_write 不臆造（responses 里没有对应字段就不输出）
	if _, has := d["cache_write_tokens"]; has {
		t.Errorf("不应输出 cache_write_tokens: %v", d)
	}
}

// TestResponsesUsageKeepsExplicitZero 「上游明确报告 0 命中」必须输出 0，
// 不得因 `> 0` 判定丢成「未提供」（不变量 24：0% ≠ —）。
func TestResponsesUsageKeepsExplicitZero(t *testing.T) {
	inner := chatFixtureHandler(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":549,"completion_tokens":5,"total_tokens":554,` +
		`"prompt_tokens_details":{"cached_tokens":0}}}`)
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

	u := responsesUsageOf(t, rec)
	d, ok := u["input_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("显式 0 命中必须建出 input_tokens_details: %v", u)
	}
	if v, has := d["cached_tokens"]; !has || v != float64(0) {
		t.Errorf("cached_tokens = %v (has=%v) want 显式 0", v, has)
	}
}

// TestResponsesUsagePrefersHitFieldOverBareNoise workbuddy 系帧：顶层裸
// cached_tokens=0 是**恒 0 噪声**，真值在 prompt_cache_hit_tokens ⇒ 必须取真值。
func TestResponsesUsagePrefersHitFieldOverBareNoise(t *testing.T) {
	inner := chatFixtureHandler(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":97,"completion_tokens":228,"total_tokens":325,` +
		`"prompt_cache_hit_tokens":97,"prompt_cache_miss_tokens":0,"cached_tokens":0}}`)
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

	u := responsesUsageOf(t, rec)
	d, _ := u["input_tokens_details"].(map[string]any)
	if d["cached_tokens"] != float64(97) {
		t.Errorf("cached_tokens = %v want 97（顶层裸 cached_tokens=0 是噪声，不得采信）", d["cached_tokens"])
	}
}

// TestResponsesUsageNoCacheFieldsOmitsDetails 上游不提供缓存维度时，
// 不得凭空造出 input_tokens_details（否则下游把「不知道」显示成 0%）。
func TestResponsesUsageNoCacheFieldsOmitsDetails(t *testing.T) {
	inner := chatFixtureHandler(`{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	_, mux := newTestGateway(inner)
	rec := post(mux, "/v1/responses", `{"model":"gpt-5","input":"hi"}`)

	u := responsesUsageOf(t, rec)
	if _, has := u["input_tokens_details"]; has {
		t.Errorf("不得凭空造出 input_tokens_details: %v", u)
	}
	if strings.Contains(rec.Body.String(), "cached_tokens") {
		t.Errorf("响应不该提缓存字段: %s", rec.Body.String())
	}
}
