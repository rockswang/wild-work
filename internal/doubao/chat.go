package doubao

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// chatRequest 客户端 OpenAI 风格请求里本渠道用得到的字段。
type chatRequest struct {
	Model           string        `json:"model"`
	Messages        []chatMessage `json:"messages"`
	Stream          bool          `json:"stream"`
	ReasoningEffort string        `json:"reasoning_effort"`
	// Tools 不进请求体（上游无 tools 通道，真机已证），只用于首轮注入 system prompt
	// 与流式门控（tools.go / sse.go）。
	hasTools bool
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
	// 工具循环字段（OpenAI 形状）：assistant 的调用发起 + tool 的结果回传。
	// 上游无 tools 通道，这些字段只用于首轮压平时的格式化还原（tools.go）。
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	// toolName 从 tool_call_id 关联出的工具名（压平时回填，见 buildChatBody 调用方）。
	toolName string
}

// chatToolCall OpenAI assistant.tool_calls 的最小切片。
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Text 提取 content 的纯文本（兼容 string 与分块数组两种形态）。
func (m chatMessage) Text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// buildChatBody 构造 /chat/completion 的请求体（工作模式 agent_mode=1 + model_item_key）。
//
// 逐项照搬 yyydsz/doubao2api（MIT，2026-11-15 本仓实测打通）：这是「桌面端身份免签名」
// 路线唯一被大规模验证过、且能选真模型（deepseek/kimi/glm）的形状。本仓 11-15 之前走的
// /samantha/chat/completion 只有思考档位、选不了模型，且初版在这里用 /chat/completion 却报
// `710020202`——根因是请求体形状不对（缺 content_block / conversation_init_ext /
// model_config / general_task_param 这一整套），不是号的问题（已在合格账号上实测通过）。
//
//   - 多轮处理：会话式复用（2026-11-15 起，session.go）——首轮 need_create_conversation=true
//   - 历史压平；同会话键后续轮 need_create_conversation=false、只发最后一句 user，
//     上下文由豆包服务端会话承载。会话锚点从 SSE_ACK 的 ack_client_meta 捕获。
func buildChatBody(req *chatRequest, itemKey, effort, dev, memProfile, totalWindow string, session sessionState, tools []toolDefinition) ([]byte, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages 不能为空")
	}
	lastUser := ""
	lastUserIdx := -1 // 最后一条 user 的下标：压平时跳过它（末尾统一拼），不是跳过末条消息
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUser = req.Messages[i].Text()
			lastUserIdx = i
			break
		}
	}
	if lastUser == "" {
		return nil, fmt.Errorf("缺少 user 消息")
	}

	// 首轮（无会话）：历史压平成「角色：内容」段落；复用轮（有会话）：
	// 只发最后一句，上下文由豆包服务端会话承载（yyydsz/doubao2api 同款规则）。
	// 工具协议（tools.go）：压平轮额外做两件事——① 工具清单注入段首（含调用格式
	// 指令与禁用内置搜索）；② role:tool / assistant.tool_calls 消息格式化保留
	// （调研报告 A6.4：丢弃会让 agent 循环第二轮就断）。复用轮不重复注入：
	// 豆包服务端会话已记住首轮的工具清单与格式约定。
	text := lastUser
	if session.ConversationID == "" {
		// tool_call_id → 工具名关联：role:tool 的 tool_call_id 对应同会话里 assistant
		// 的某个 tool_calls[].id，压平时回填工具名让回传段可读。
		callNames := map[string]string{}
		for _, m := range req.Messages {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					callNames[tc.ID] = tc.Function.Name
				}
			}
		}
		// 旧实现跳过条件误用 len-1：agent 第二轮的末条是 role:tool，被当「当前消息」
		// 跳过 ⇒ 工具结果整段丢失 + 问题重复（2026-11-15 审查探针实测复现）。
		// 跳过条件应为「最后一条 user」的下标。
		// 末条是 role:tool 的续跑轮（模型该接着工具结果往下写）不回问 lastUser：
		// 压平到工具结果为止收尾，避免「回答完工具结果再重答一遍原问题」。
		tailIsTool := req.Messages[len(req.Messages)-1].Role == "tool"
		var b strings.Builder
		if len(tools) > 0 {
			b.WriteString(toolsPrompt(tools))
			b.WriteString("\n")
		}
		for i, m := range req.Messages {
			t := strings.TrimSpace(m.Text())
			if !tailIsTool && i == lastUserIdx {
				continue
			}
			// 工具历史消息格式化保留（即使 t 为空也处理 tool_calls 还原）
			if m.Role == "tool" {
				b.WriteString(formatToolResult(callNames[m.ToolCallID], m.Text()))
				b.WriteString("\n")
				continue
			}
			if m.Role == "assistant" && len(m.ToolCalls) > 0 {
				b.WriteString("助手：")
				for _, tc := range m.ToolCalls {
					b.WriteString(reconstructToolCall(tc.Function.Name, tc.Function.Arguments))
					b.WriteString("\n")
				}
				if t != "" {
					b.WriteString(t + "\n")
				}
				continue
			}
			if t == "" {
				continue
			}
			switch m.Role {
			case "user":
				b.WriteString("用户：")
			case "assistant":
				b.WriteString("助手：")
			default:
				b.WriteString(m.Role + "：")
			}
			b.WriteString(t)
			b.WriteString("\n")
		}
		if b.Len() > 0 {
			if tailIsTool {
				text = b.String()
			} else {
				text = b.String() + "用户：" + lastUser
			}
		}
	}

	msgID := newUUID()
	localConv := "local_" + randDigits(16, "")
	wo := defaultWorkOptions()
	if effort == "" {
		effort = wo.ReasoningEffort
	}
	if memProfile == "" {
		memProfile = wo.MemoryProfile
	}
	if totalWindow == "" {
		totalWindow = wo.TotalWindow
	}

	block := map[string]any{
		"block_type": 10000,
		"content": map[string]any{
			"text_block":     map[string]any{"text": text, "icon_url": "", "icon_url_dark": "", "summary": ""},
			"pc_event_block": "",
		},
		"block_id": newUUID(), "parent_id": "", "meta_info": []any{}, "append_fields": []any{},
	}
	taskParam := map[string]any{
		"runtime_type": 2, "sandbox_auth_type": 1, "device_name": wo.DeviceName,
		"folder_name": "", "local_app_id": wo.LocalAppID, "local_device_id": dev, "workspace": wo.Workspace,
	}
	taskParamStr := mustJSONString(taskParam)
	initExt := map[string]any{
		"model_item_key": itemKey, "reasoning_effort": effort, "mode_id": WorkModeID,
		"agent_task_param": taskParamStr,
	}
	clientOpt := map[string]any{
		"enable_sandbox": false, "os": "Windows", "shared_folder_path": []string{},
		"agent_workspace": map[string]any{"agent_workspace": wo.Workspace, "local_skill_paths": []string{}},
		"client_env_id":   newUUID(), "sandbox_id": "sandbox-" + newUUID(), "sandbox_auth_type": 1,
	}
	taskInput := map[string]any{
		"agents_md":      map[string]any{"files": []any{}, "state": 2},
		"schema_version": 1, "home_dir": "/home/user", "project_context": map[string]any{}, "localConnectors": []any{},
	}
	gtp := map[string]any{
		"action": 0, "thread_local_message_id": []string{msgID}, "client_option": clientOpt,
		"runtime_type": 2, "agent_task_param": taskParam,
		"agent_task_param_change":  map[string]any{"runtime_changed": false, "device_changed": false, "sandbox_auth_type_changed": false},
		"need_modify_conversation": false, "task_input_json": mustJSONString(taskInput),
	}
	gtpStr := mustJSONString(gtp)

	convID := ""
	if session.ConversationID != "" {
		convID = session.ConversationID
	}
	opt := map[string]any{
		"create_time_ms": time.Now().UnixMilli(), "answer_with_suggest": false, "agent_mode": 1,
		"need_deep_think": 10001, "unique_key": newUUID(), "start_seq": 0,
		"need_create_conversation":  convID == "",
		"conversation_init_option":  map[string]any{"need_ack_conversation": true},
		"sse_recv_event_options":    map[string]any{"support_chunk_delta": true},
		"support_lazy_fetch_stream": true, "is_old_user": true, "message_storage_type": 0,
		"conversation_mode":     2,
		"conversation_init_ext": initExt,
		"model_config": map[string]any{
			"model_item_key":     itemKey,
			"model_extra_params": map[string]any{"memory_profile": memProfile, "total_window_size": totalWindow},
			"reasoning_effort":   4,
		},
		"aggregate_params": map[string]any{
			"mention_skill_list": "[]", "mention_plugin_list": "[]", "mention_ext": "[{}]",
			"conversation_mode": "2", "mode_id": WorkModeID, "model_item_key": itemKey,
			"agent_mode": "1", "reasoning_effort": effort, "provider_id": "",
		},
		"general_task_param": gtp,
		"ext": map[string]any{
			"general_task_param": gtpStr, "use_deep_think": "10001", "agent_mode": "1",
			"is_finish": "1", "conversation_init_option": `{"need_ack_conversation":true}`,
			"commerce_credit_config_enable": "0",
		},
	}
	body := map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": localConv, "conversation_id": convID, "bot_id": WorkBotID,
			"last_section_id": session.SectionID, "last_message_index": nil,
			"local_permissions": []any{
				map[string]any{"permission_name": "ACCESS_COARSE_LOCATION", "status": 1},
				map[string]any{"permission_name": "ACCESS_FINE_LOCATION", "status": 1},
				map[string]any{"permission_name": "ACCESS_BACKGROUND_LOCATION", "status": 1},
			},
		},
		"messages":     []any{map[string]any{"local_message_id": msgID, "content_block": []any{block}, "message_status": 0}},
		"option":       opt,
		"user_context": []any{},
	}
	return json.Marshal(body)
}

// ---------- 小工具 ----------

func mustJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
