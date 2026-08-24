// body.go 构造 agent_chat_generation 请求体（纯透传模式）。
// 移植自 qoderwork2api internal/upstream/body.go：客户端消息全量转发，
// tools 仅在客户端显式传入时注入。
package qoder

import (
	"encoding/json"
	"time"
)

// buildAgentBody 构造请求体。
//   - messages：客户端原始消息列表（可含 system/assistant/tool 多轮）
//   - modelKey：上游模型 key（如 dmodel）
//   - tools：客户端传来的 OpenAI tools 数组；为空则不注入 tools 字段
//   - enableReasoning：是否启用思考模式
//   - reasoningEffort：思考强度 low/medium/high，空串表示不传
//
// 注意：developer 角色必须改写为 system。
func buildAgentBody(messages []map[string]any, modelKey string, tools []any, enableReasoning bool, reasoningEffort string) ([]byte, error) {
	// developer → system（浅拷贝消息避免污染调用方数据）
	msgs := make([]map[string]any, len(messages))
	for i, m := range messages {
		cp := make(map[string]any, len(m)+1)
		for k, v := range m {
			cp[k] = v
		}
		if r, _ := cp["role"].(string); r == "developer" {
			cp["role"] = "system"
		}
		msgs[i] = cp
	}

	// 最后一条 user 消息文本（chat_context.text 上游协议要求必填）
	prompt := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i]["role"] == "user" {
			if c, ok := msgs[i]["content"].(string); ok && c != "" {
				prompt = c
				break
			}
		}
	}

	now := time.Now()
	newUUID := uuid4()

	base := map[string]any{
		"request_id":       newUUID,
		"chat_record_id":   newUUID,
		"request_set_id":   uuid4(),
		"session_id":       uuid4(),
		"stream":           true,
		"aliyun_user_type": "personal_professional_trial",
		"agent_id":         "agent_common",
		"chat_task":        "FREE_INPUT",
		"is_reply":         true,
		"image_urls":       nil,
		"session_type":     "qodercli",
		"model_config":     map[string]any{"key": modelKey, "is_reasoning": enableReasoning},
		"chat_context": map[string]any{
			"chatPrompt": "",
			"text":       map[string]any{"type": "text", "text": prompt},
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"key": modelKey, "is_reasoning": enableReasoning},
				"originalContent": map[string]any{"type": "text", "text": prompt},
			},
			"features":  []any{},
			"imageUrls": nil,
		},
		"messages": msgs,
		"business": map[string]any{
			"id":       uuid4(),
			"begin_at": now.UnixMilli(),
			"name":     truncateRunes(prompt, 30),
		},
	}

	if len(tools) > 0 {
		base["tools"] = tools
	}

	return json.Marshal(base)
}

// truncateRunes 截断到 n 个 rune。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
