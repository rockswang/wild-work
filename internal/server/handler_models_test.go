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
	mi := provider.ModelInfo{ID: "text-model", ContextWindow: 1000}
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

// ---- 思考档位透传（issue #96）----
// 原则：上游目录声明了档位才输出 reasoning 对象与 supports_reasoning；
// 未声明时两者都不出现（不编造），保持与 /api/fees 面板同源。

// TestModelEntryReasoning 对齐 OpenRouter 规范：具备推理能力时输出 reasoning 对象与 supported_parameters，
// 且不再向客户端下发一长串思考强度档位列表（supported_efforts）。
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
	// 用户决议：获取模型不要发一长串思考强度档位列表（supported_efforts）
	if _, has := r["supported_efforts"]; has {
		t.Errorf("获取模型不应再输出一长串 supported_efforts 列表，实际得到了: %v", r["supported_efforts"])
	}
	if r["mandatory"] != false {
		t.Errorf("can_disable_thinking 为 true 时 mandatory 应为 false，得到 %v", r["mandatory"])
	}
	if r["default_effort"] != "high" {
		t.Errorf("default_effort 应为 high，得到 %v", r["default_effort"])
	}
	if r["can_disable_thinking"] != true {
		t.Errorf("can_disable_thinking 应为 true，得到 %v", r["can_disable_thinking"])
	}

	// OpenRouter 规范 supported_parameters 校验
	params, ok := back["supported_parameters"].([]any)
	if !ok {
		t.Fatalf("缺少 supported_parameters 数组")
	}
	paramSet := map[string]bool{}
	for _, p := range params {
		if s, ok := p.(string); ok {
			paramSet[s] = true
		}
	}
	for _, expected := range []string{"reasoning", "include_reasoning", "reasoning_effort"} {
		if !paramSet[expected] {
			t.Errorf("supported_parameters 应包含 %q，实际为 %v", expected, params)
		}
	}

	// id 带渠道前缀（其余字段不受影响）
	if back["id"] != "workbuddy/glm-5.3" {
		t.Errorf("id 应带渠道前缀，得到 %v", back["id"])
	}
}

// TestModelEntryReasoningAbsent 完全未具备推理能力时不输出 reasoning/supports_reasoning。
func TestModelEntryReasoningAbsent(t *testing.T) {
	mi := provider.ModelInfo{ID: "plain"}
	entry := buildModelEntry(provider.WorkBuddy, mi)
	if _, has := entry["reasoning"]; has {
		t.Errorf("%s 未具备推理能力时不应输出 reasoning 对象", mi.ID)
	}
	if _, has := entry["supports_reasoning"]; has {
		t.Errorf("%s 未具备推理能力时不应输出 supports_reasoning", mi.ID)
	}
	if params, ok := entry["supported_parameters"].([]string); ok {
		for _, p := range params {
			if p == "reasoning" || p == "reasoning_effort" {
				t.Errorf("%s 未具备推理能力时 supported_parameters 不应包含 %s", mi.ID, p)
			}
		}
	}
}

// TestModelEntryReasoningWithoutEfforts 原生推理模型（如 DeepSeek-R1，只有推理能力但无档位列表）也应标准输出 reasoning 标记。
func TestModelEntryReasoningWithoutEfforts(t *testing.T) {
	mi := provider.ModelInfo{ID: "deepseek-r1", SupportsReasoning: true}
	entry := buildModelEntry(provider.WorkBuddy, mi)
	if entry["supports_reasoning"] != true {
		t.Fatalf("supports_reasoning 应为 true")
	}
	r, ok := entry["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 reasoning 对象")
	}
	if _, has := r["supported_efforts"]; has {
		t.Errorf("不应输出 supported_efforts")
	}
	if r["mandatory"] != true {
		t.Errorf("未声明可关闭时 mandatory 应默认为 true，得到 %v", r["mandatory"])
	}
	params, ok := entry["supported_parameters"].([]string)
	if !ok {
		t.Fatalf("缺少 supported_parameters")
	}
	hasReasoning := false
	for _, p := range params {
		if p == "reasoning" {
			hasReasoning = true
		}
	}
	if !hasReasoning {
		t.Errorf("supported_parameters 应包含 reasoning")
	}
}

// TestModelEntryReasoningMinimal 只有默认档没有列表也应输出。
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
	if r["mandatory"] != true {
		t.Errorf("mandatory 应为 true，得到 %v", r["mandatory"])
	}
}
