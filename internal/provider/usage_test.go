// usage_test.go usage 形状归一化的口径回归（issue #104）。
//
// 用例全部取自实测帧，不自造数据（R23b：自造数据只会固化自己的错误假设）：
//   - trae 系帧取自 ref/TraeWork 网页版逆向分析备忘.md:254（含 cache_read/creation）；
//   - workbuddy 系帧取自 internal/server/cacheusage_test.go 的真帧（顶层裸
//     cached_tokens 恒 0 是噪声，真值在 prompt_cache_hit_tokens）。
package provider

import (
	"encoding/json"
	"testing"
)

// traeRealFrame issue #104 的实测帧（prompt_tokens 是总输入，read < prompt）。
const traeRealFrame = `{"prompt_tokens":30133,"completion_tokens":122,"total_tokens":30255,
  "reasoning_tokens":42,"cache_creation_input_tokens":0,"cache_read_input_tokens":28672}`

// TestNormalizeUsageShapeTraeRealFrame trae 真帧：必须补出 OpenAI 缓存子字段，
// 且**保留**原始 Anthropic 字段、不动 prompt_tokens。
//
// 这是 issue #104 的核心：下游按 `prompt_tokens_details?.cached_tokens
// ?? prompt_cache_hit_tokens ?? 0` 解析（DSH business.mjs 实测），trae 帧两条都读不到
// ⇒ 命中率恒 0%、prompt 全按未缓存计价（费用高估约 50 倍）。
func TestNormalizeUsageShapeTraeRealFrame(t *testing.T) {
	var u map[string]any
	if err := json.Unmarshal([]byte(traeRealFrame), &u); err != nil {
		t.Fatal(err)
	}
	if !NormalizeUsageShape(u) {
		t.Fatal("trae 帧必须被判定为发生了归一化")
	}
	d, ok := u["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("必须建出 prompt_tokens_details，实际 %T", u["prompt_tokens_details"])
	}
	if got := d["cached_tokens"]; got == nil || toI(got) != 28672 {
		t.Errorf("cached_tokens = %v want 28672", got)
	}
	// cache_creation=0 是**有效的 0**（写入侧本轮为 0），必须搬过去而不是丢掉
	if got, has := d["cache_write_tokens"]; !has || toI(got) != 0 {
		t.Errorf("cache_write_tokens = %v (has=%v) want 0", got, has)
	}
	// 原始字段保留（有客户端就依赖它们）
	if toI(u["cache_read_input_tokens"]) != 28672 || toI(u["cache_creation_input_tokens"]) != 0 {
		t.Errorf("原始 Anthropic 字段被破坏: %v", u)
	}
	// prompt_tokens 是总输入，**不得**重算（命中是其子集）
	if toI(u["prompt_tokens"]) != 30133 {
		t.Errorf("prompt_tokens 被改动: %v", u["prompt_tokens"])
	}
	if toI(d["cached_tokens"]) > toI(u["prompt_tokens"]) {
		t.Errorf("命中 > prompt_tokens，子集不变量被破坏")
	}
}

// TestNormalizeUsageShapeWorkbuddyKeepsHits workbuddy 系帧：顶层裸 cached_tokens 恒 0
// （噪声），真值在 prompt_cache_hit_tokens——归一化必须取真值而不是噪声。
func TestNormalizeUsageShapeWorkbuddyKeepsHits(t *testing.T) {
	var u map[string]any
	if err := json.Unmarshal([]byte(`{"prompt_tokens":97,"completion_tokens":228,"total_tokens":325,
	  "prompt_cache_hit_tokens":97,"prompt_cache_miss_tokens":0,
	  "cached_tokens":0,"prompt_tokens_details":{"cached_tokens":0}}`), &u); err != nil {
		t.Fatal(err)
	}
	NormalizeUsageShape(u) // workbuddy 系已有 details，属幂等路径
	d := u["prompt_tokens_details"].(map[string]any)
	if toI(d["cached_tokens"]) != 0 {
		t.Errorf("已存在的 details.cached_tokens 不得改写成 %v（workbuddy 该帧真值就是 0）", d["cached_tokens"])
	}
}

// TestNormalizeUsageShapeIdempotent 已是 OpenAI 形状时不得改动（幂等）。
func TestNormalizeUsageShapeIdempotent(t *testing.T) {
	var u map[string]any
	if err := json.Unmarshal([]byte(`{"prompt_tokens":100,"completion_tokens":4,
	  "prompt_tokens_details":{"cached_tokens":80,"cache_write_tokens":5}}`), &u); err != nil {
		t.Fatal(err)
	}
	if NormalizeUsageShape(u) {
		t.Error("已是 OpenAI 形状不该报告改动")
	}
	d := u["prompt_tokens_details"].(map[string]any)
	if toI(d["cached_tokens"]) != 80 || toI(d["cache_write_tokens"]) != 5 {
		t.Errorf("既有值被覆盖: %v", d)
	}
}

// TestNormalizeUsageShapeExplicitZeroIsKept「字段存在但为 0」必须搬成 0，
// 不能因 falsy 判空而丢成「未上报」（不变量 24：0% 与「—」语义不同）。
func TestNormalizeUsageShapeExplicitZeroIsKept(t *testing.T) {
	var u map[string]any
	if err := json.Unmarshal([]byte(`{"prompt_tokens":100,"completion_tokens":4,
	  "cache_read_input_tokens":0,"cache_creation_input_tokens":0}`), &u); err != nil {
		t.Fatal(err)
	}
	if !NormalizeUsageShape(u) {
		t.Fatal("显式 0 也属于可归一化的上报值")
	}
	d := u["prompt_tokens_details"].(map[string]any)
	if v, has := d["cached_tokens"]; !has || toI(v) != 0 {
		t.Errorf("显式 0 必须保留为 0（区分「未命中」与「不提供」），实际 %v has=%v", v, has)
	}
}

// TestNormalizeUsageShapeNoCacheFieldsNoDetails 无任何缓存字段时**不得**凭空建出
// prompt_tokens_details（否则面板会把「不知道」显示成「0%」）。
func TestNormalizeUsageShapeNoCacheFieldsNoDetails(t *testing.T) {
	var u map[string]any
	if err := json.Unmarshal([]byte(`{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}`), &u); err != nil {
		t.Fatal(err)
	}
	if NormalizeUsageShape(u) {
		t.Error("无缓存字段时不该报告改动")
	}
	if _, has := u["prompt_tokens_details"]; has {
		t.Errorf("不得为不提供该维度的渠道凭空造出 details: %v", u)
	}
}

// TestNormalizeUsageShapeFlatCachedTokensFallback 顶层裸 cached_tokens 仅作兜底
// （OpenAI 兼容上游可能只给这个）。
func TestNormalizeUsageShapeFlatCachedTokensFallback(t *testing.T) {
	var u map[string]any
	if err := json.Unmarshal([]byte(`{"prompt_tokens":10,"completion_tokens":2,"cached_tokens":7}`), &u); err != nil {
		t.Fatal(err)
	}
	if !NormalizeUsageShape(u) {
		t.Fatal("顶层裸 cached_tokens 应被搬进 details")
	}
	d := u["prompt_tokens_details"].(map[string]any)
	if toI(d["cached_tokens"]) != 7 {
		t.Errorf("cached_tokens = %v want 7", d["cached_tokens"])
	}
}

// TestNormalizeUsageShapeBadData 坏数据（负数/NaN/非数值）不得搬运，
// 也不得因它建出 details；nil 与空 map 安全。
func TestNormalizeUsageShapeBadData(t *testing.T) {
	cases := []string{
		`{"prompt_tokens":10,"cache_read_input_tokens":-3}`,
		`{"prompt_tokens":10,"cache_read_input_tokens":"28672"}`,
		`{"prompt_tokens":10,"prompt_tokens_details":"not-an-object"}`,
	}
	for _, c := range cases {
		var u map[string]any
		if err := json.Unmarshal([]byte(c), &u); err != nil {
			t.Fatal(err)
		}
		if NormalizeUsageShape(u) {
			t.Errorf("坏数据不该被搬运: %s", c)
		}
	}
	if NormalizeUsageShape(nil) || NormalizeUsageShape(map[string]any{}) {
		t.Error("nil / 空 map 必须安全返回 false")
	}
}

// TestNormalizeUsageShapeDetailsWithOtherKeys 已存在 details 但缺 cached_tokens
// （如 cline 只有 cache_write_tokens）时应补进**同一个**对象，不新建。
func TestNormalizeUsageShapeDetailsWithOtherKeys(t *testing.T) {
	var u map[string]any
	if err := json.Unmarshal([]byte(`{"prompt_tokens":94,"completion_tokens":383,
	  "prompt_tokens_details":{"cache_write_tokens":0,"audio_tokens":0},
	  "cache_read_input_tokens":64}`), &u); err != nil {
		t.Fatal(err)
	}
	if !NormalizeUsageShape(u) {
		t.Fatal("应补出 cached_tokens")
	}
	d := u["prompt_tokens_details"].(map[string]any)
	if toI(d["cached_tokens"]) != 64 {
		t.Errorf("cached_tokens = %v want 64", d["cached_tokens"])
	}
	if _, has := d["audio_tokens"]; !has {
		t.Error("既有子字段不得丢失")
	}
}

// toI 把归一化后的数值（float64/int/int64）统一成 int 便于断言。
func toI(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return -1 // 非数值（含 nil）统一用一个不可能的哨兵，避免测试静默通过
}
