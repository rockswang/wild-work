// handler_models_test.go 校验 /v1/models 的输出契约：
// 满足 OpenAI 官方字段，并按 OpenRouter / llama.cpp 惯例透传输入模态。
package server

import (
	"encoding/json"
	"testing"

	"wild-work/internal/provider"
)

// TestModelEntryContract 单条模型条目应含 OpenAI 官方字段 + architecture 模态声明。
func TestModelEntryContract(t *testing.T) {
	mi := provider.ModelInfo{
		ID: "vision-model", Name: "Vision", ContextWindow: 128000, MaxTokens: 8192,
		ContextFromAPI: true, SupportsImages: true,
	}
	entry := buildModelEntry(provider.WorkBuddyAI, mi)

	// OpenAI 官方规定字段
	for _, k := range []string{"id", "object", "created", "owned_by"} {
		if _, ok := entry[k]; !ok {
			t.Errorf("缺少 OpenAI 官方字段 %q", k)
		}
	}
	if entry["object"] != "model" {
		t.Errorf("object 应为 model，得到 %v", entry["object"])
	}
	if entry["id"] != "workbuddyai/vision-model" {
		t.Errorf("id 应带渠道前缀，得到 %v", entry["id"])
	}

	// 模态声明：OpenRouter / llama.cpp 通行形状
	arch, ok := entry["architecture"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 architecture 对象，得到 %T", entry["architecture"])
	}
	mods, ok := arch["input_modalities"].([]string)
	if !ok || len(mods) != 2 || mods[0] != "text" || mods[1] != "image" {
		t.Errorf("input_modalities 应为 [text image]，得到 %v", arch["input_modalities"])
	}
	if arch["modality"] != "text+image->text" {
		t.Errorf("modality 错误：%v", arch["modality"])
	}

	// 不应存在自造的并行编码（收敛为单一惯例）
	for _, k := range []string{"input_modalities", "capabilities"} {
		if _, has := entry[k]; has {
			t.Errorf("不应输出冗余字段 %q（统一用 architecture）", k)
		}
	}

	// 可 JSON 序列化（下游按 JSON 解析）
	if _, err := json.Marshal(entry); err != nil {
		t.Fatalf("条目无法序列化为 JSON: %v", err)
	}
}

// TestModelEntryTextOnly 不支持图像的模型只声明 text 模态。
func TestModelEntryTextOnly(t *testing.T) {
	mi := provider.ModelInfo{ID: "text-model", ContextWindow: 1000, ContextFromAPI: true}
	entry := buildModelEntry(provider.WorkBuddy, mi)

	arch, _ := entry["architecture"].(map[string]any)
	mods, ok := arch["input_modalities"].([]string)
	if !ok || len(mods) != 1 || mods[0] != "text" {
		t.Errorf("不支持图像时应为 [text]，得到 %v", arch["input_modalities"])
	}
	if arch["modality"] != "text->text" {
		t.Errorf("modality 应为 text->text，得到 %v", arch["modality"])
	}
	// 容量字段
	if entry["context_length"] != int64(1000) {
		t.Errorf("context_length 应透传，得到 %v", entry["context_length"])
	}
	if _, has := entry["max_output_tokens"]; has {
		t.Error("MaxTokens 为 0 时不应输出 max_output_tokens")
	}
}

// TestModelEntryOmitsEstimatedCapacity 硬编码估算的容量不外溢到 /v1/models，
// 与面板「未知」口径一致；上游声明的可选档位照常透出。
func TestModelEntryOmitsEstimatedCapacity(t *testing.T) {
	mi := provider.ModelInfo{ID: "est-model", ContextWindow: 180000, MaxTokens: 32000}
	entry := buildModelEntry(provider.Qoder, mi)
	if _, has := entry["context_length"]; has {
		t.Errorf("估算值不应输出 context_length，得到 %v", entry["context_length"])
	}
	if _, has := entry["max_output_tokens"]; has {
		t.Errorf("估算值不应输出 max_output_tokens，得到 %v", entry["max_output_tokens"])
	}

	mi = provider.ModelInfo{ID: "opt-model", ContextOptions: []int64{200000, 400000, 1000000}}
	entry = buildModelEntry(provider.Qoder, mi)
	opts, ok := entry["context_options"].([]int64)
	if !ok || len(opts) != 3 || opts[2] != 1000000 {
		t.Errorf("context_options 应透传，得到 %v", entry["context_options"])
	}
}

// ---- 思考档位透传（issue #96）----
// 原则：上游目录声明了档位才输出 reasoning 对象与 supports_reasoning；
// 未声明时两者都不出现（不编造），保持与 /api/fees 面板同源。

// TestModelEntryReasoning 带档位的模型应输出 reasoning 对象（Buddy2api v2.1.11 形状）。
func TestModelEntryReasoning(t *testing.T) {
	cd := true
	mi := provider.ModelInfo{
		ID: "glm-5.3", Name: "GLM-5.3", SupportsReasoning: true,
		ReasoningEfforts: []string{"low", "high", "max"}, DefaultReasoningEffort: "high",
		CanDisableThinking: &cd,
	}
	entry := buildModelEntry(provider.WorkBuddy, mi)
	// JSON 序列化验证（客户端按 JSON 解析）
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back["supports_reasoning"] != true {
		t.Errorf("supports_reasoning 应为 true，得到 %v", back["supports_reasoning"])
	}
	r, ok := back["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 reasoning 对象: %s", raw)
	}
	efforts, ok := r["supported_efforts"].([]any)
	if !ok || len(efforts) != 3 || efforts[0] != "low" || efforts[2] != "max" {
		t.Errorf("supported_efforts 应为 [low high max]，得到 %v", r["supported_efforts"])
	}
	if r["default_effort"] != "high" {
		t.Errorf("default_effort 应为 high，得到 %v", r["default_effort"])
	}
	if r["can_disable_thinking"] != true {
		t.Errorf("can_disable_thinking 应为 true，得到 %v", r["can_disable_thinking"])
	}
	// id 带渠道前缀（其余字段不受影响）
	if back["id"] != "workbuddy/glm-5.3" {
		t.Errorf("id 应带渠道前缀，得到 %v", back["id"])
	}
}

// TestModelEntryReasoningAbsent 上游未声明档位时不得输出 reasoning/supports_reasoning。
func TestModelEntryReasoningAbsent(t *testing.T) {
	cases := []provider.ModelInfo{
		{ID: "plain"}, // 全未声明
		{ID: "bool-only", SupportsReasoning: true}, // 只有 bool（旧数据形状），无档位列表
	}
	for _, mi := range cases {
		entry := buildModelEntry(provider.WorkBuddy, mi)
		if _, has := entry["reasoning"]; has {
			t.Errorf("%s 未声明档位时不应输出 reasoning 对象", mi.ID)
		}
		// supports_reasoning 由能力位决定（本仓形状）：bool-only 仍应透出。
		_, has := entry["supports_reasoning"]
		if want := mi.SupportsReasoning; has != want {
			t.Errorf("%s supports_reasoning present=%v, want %v", mi.ID, has, want)
		}
	}
}

// TestModelEntryReasoningMinimal 只有默认档没有列表（deepseek 系形状）也应输出。
func TestModelEntryReasoningMinimal(t *testing.T) {
	cd := false
	mi := provider.ModelInfo{ID: "ds", SupportsReasoning: true, DefaultReasoningEffort: "high", CanDisableThinking: &cd}
	entry := buildModelEntry(provider.WorkBuddy, mi)
	if entry["supports_reasoning"] != true {
		t.Fatalf("supports_reasoning 应为 true")
	}
	r, ok := entry["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 reasoning 对象")
	}
	if _, has := r["supported_efforts"]; has {
		t.Errorf("无列表时不应输出 supported_efforts")
	}
	if r["default_effort"] != "high" {
		t.Errorf("default_effort 应为 high，得到 %v", r["default_effort"])
	}
	if r["can_disable_thinking"] != false {
		t.Errorf("can_disable_thinking 应为 false，得到 %v", r["can_disable_thinking"])
	}
}
