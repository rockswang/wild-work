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
	if r["mandatory"] != false {
		t.Errorf("mandatory 应为 false (can_disable_thinking=true 时 mandatory=false)，得到 %v", r["mandatory"])
	}
	// id 带渠道前缀（其余字段不受影响）
	if back["id"] != "workbuddy/glm-5.3" {
		t.Errorf("id 应带渠道前缀，得到 %v", back["id"])
	}
}

// TestModelEntryReasoningAbsent 上游未声明档位时不得输出 reasoning/supports_reasoning。
func TestModelEntryReasoningAbsent(t *testing.T) {
	cases := []provider.ModelInfo{
		{ID: "plain"},                              // 全未声明
		{ID: "bool-only", SupportsReasoning: true}, // 只有 bool（旧数据形状），无档位列表
	}
	for _, mi := range cases {
		entry := buildModelEntry(provider.WorkBuddy, mi)
		if _, has := entry["reasoning"]; has {
			t.Errorf("%s 未声明档位时不应输出 reasoning 对象", mi.ID)
		}
		if _, has := entry["supports_reasoning"]; has {
			t.Errorf("%s 未声明档位时不应输出 supports_reasoning", mi.ID)
		}
		if _, has := entry["supported_parameters"]; has {
			t.Errorf("%s 未声明工具或思考时不应输出 supported_parameters", mi.ID)
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
	if r["mandatory"] != true {
		t.Errorf("mandatory 应为 true (can_disable_thinking=false 时 mandatory=true)，得到 %v", r["mandatory"])
	}
}

// TestModelEntryOpenRouterParameters 校验 OpenRouter 规范与 R49 护栏：
// 1. supported_parameters 严格由声明能力动态导出，绝不编造未确认的采样参数 (temperature, top_p, max_tokens 等)；
// 2. CanDisableThinking 为 nil 时，mandatory 与 can_disable_thinking 必须缺席 (严禁编造 mandatory: true)；
// 3. 既有 issue #96 字段 (supported_efforts, default_effort, can_disable_thinking) 完整保留共存。
func TestModelEntryOpenRouterParameters(t *testing.T) {
	t.Run("tools_and_reasoning", func(t *testing.T) {
		cd := true
		mi := provider.ModelInfo{
			ID:               "deepseek-r1",
			SupportsTools:    true,
			ReasoningEfforts: []string{"low", "medium", "high"},
			CanDisableThinking: &cd,
		}
		entry := buildModelEntry(provider.WorkBuddy, mi)

		params, ok := entry["supported_parameters"].([]string)
		if !ok {
			t.Fatalf("缺少 supported_parameters，得到 %T", entry["supported_parameters"])
		}
		expected := []string{"tools", "tool_choice", "reasoning", "include_reasoning", "reasoning_effort"}
		if len(params) != len(expected) {
			t.Fatalf("supported_parameters 长度期望 %d，实际 %d (%v)", len(expected), len(params), params)
		}
		for i, v := range expected {
			if params[i] != v {
				t.Errorf("supported_parameters[%d] 期望 %q，实际 %q", i, v, params[i])
			}
		}

		r, ok := entry["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("缺少 reasoning 对象")
		}
		// issue #96 字段保持
		if efforts, ok := r["supported_efforts"].([]string); !ok || len(efforts) != 3 {
			t.Errorf("supported_efforts 未保留: %v", r["supported_efforts"])
		}
		if r["can_disable_thinking"] != true {
			t.Errorf("can_disable_thinking 期望 true，实际 %v", r["can_disable_thinking"])
		}
		// OpenRouter 字段
		if r["mandatory"] != false {
			t.Errorf("mandatory 期望 false，实际 %v", r["mandatory"])
		}
	})

	t.Run("tools_only", func(t *testing.T) {
		mi := provider.ModelInfo{
			ID:            "gpt-4o",
			SupportsTools: true,
		}
		entry := buildModelEntry(provider.WorkBuddy, mi)

		params, ok := entry["supported_parameters"].([]string)
		if !ok {
			t.Fatalf("缺少 supported_parameters")
		}
		expected := []string{"tools", "tool_choice"}
		if len(params) != len(expected) || params[0] != "tools" || params[1] != "tool_choice" {
			t.Errorf("supported_parameters 期望 %v，实际 %v", expected, params)
		}
		if _, has := entry["reasoning"]; has {
			t.Errorf("无思考时不应输出 reasoning")
		}
	})

	t.Run("r49_undeclared_guard", func(t *testing.T) {
		// CanDisableThinking 为 nil 时，绝不能编造 mandatory 或 can_disable_thinking
		mi := provider.ModelInfo{
			ID:               "reasoning-undeclared-disable",
			ReasoningEfforts: []string{"medium"},
		}
		entry := buildModelEntry(provider.WorkBuddy, mi)

		r, ok := entry["reasoning"].(map[string]any)
		if !ok {
			t.Fatalf("缺少 reasoning 对象")
		}
		if _, has := r["mandatory"]; has {
			t.Errorf("CanDisableThinking 为 nil 时严禁输出 mandatory (违背 R49 原则)")
		}
		if _, has := r["can_disable_thinking"]; has {
			t.Errorf("CanDisableThinking 为 nil 时严禁输出 can_disable_thinking")
		}
		if efforts, ok := r["supported_efforts"].([]string); !ok || len(efforts) != 1 || efforts[0] != "medium" {
			t.Errorf("supported_efforts 异常: %v", r["supported_efforts"])
		}

		// 检查 supported_parameters 绝不包含未确认的采样参数
		params, ok := entry["supported_parameters"].([]string)
		if !ok {
			t.Fatalf("缺少 supported_parameters")
		}
		forbiddenParams := []string{"temperature", "top_p", "top_k", "max_tokens", "frequency_penalty", "presence_penalty"}
		for _, fp := range forbiddenParams {
			for _, p := range params {
				if p == fp {
					t.Errorf("supported_parameters 严禁编造未在上游声明的采样参数 %q", fp)
				}
			}
		}
	})

	t.Run("none_declared", func(t *testing.T) {
		mi := provider.ModelInfo{
			ID: "simple-text",
		}
		entry := buildModelEntry(provider.WorkBuddy, mi)
		if _, has := entry["supported_parameters"]; has {
			t.Errorf("无工具与思考时 supported_parameters 必须缺席")
		}
		if _, has := entry["reasoning"]; has {
			t.Errorf("无思考时 reasoning 必须缺席")
		}
	})
}
