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
}

type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
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
// 多轮处理：每次请求 need_create_conversation=true + 本地 uuid，**无状态**；首轮把历史压成
// 一段文本（与旧 samantha 同款取舍），适合 Codex/Claude Code 这种每轮发全历史的客户端。
func buildChatBody(req *chatRequest, itemKey, effort, dev, memProfile, totalWindow string) ([]byte, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages 不能为空")
	}
	lastUser := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUser = req.Messages[i].Text()
			break
		}
	}
	if lastUser == "" {
		return nil, fmt.Errorf("缺少 user 消息")
	}

	// 历史压平：除最后一条 user 外，全部拼成「角色：内容」段落。
	text := lastUser
	{
		lastIdx := len(req.Messages) - 1
		var b strings.Builder
		for i, m := range req.Messages {
			t := strings.TrimSpace(m.Text())
			if t == "" || i == lastIdx {
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
			text = b.String() + "用户：" + lastUser
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
			"text_block":    map[string]any{"text": text, "icon_url": "", "icon_url_dark": "", "summary": ""},
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
		"client_env_id":  newUUID(), "sandbox_id": "sandbox-" + newUUID(), "sandbox_auth_type": 1,
	}
	taskInput := map[string]any{
		"agents_md": map[string]any{"files": []any{}, "state": 2},
		"schema_version": 1, "home_dir": "/home/user", "project_context": map[string]any{}, "localConnectors": []any{},
	}
	gtp := map[string]any{
		"action": 0, "thread_local_message_id": []string{msgID}, "client_option": clientOpt,
		"runtime_type": 2, "agent_task_param": taskParam,
		"agent_task_param_change": map[string]any{"runtime_changed": false, "device_changed": false, "sandbox_auth_type_changed": false},
		"need_modify_conversation":  false, "task_input_json": mustJSONString(taskInput),
	}
	gtpStr := mustJSONString(gtp)

	opt := map[string]any{
		"create_time_ms": time.Now().UnixMilli(), "answer_with_suggest": false, "agent_mode": 1,
		"need_deep_think": 10001, "unique_key": newUUID(), "start_seq": 0,
		"need_create_conversation": true,
		"conversation_init_option":  map[string]any{"need_ack_conversation": true},
		"sse_recv_event_options":    map[string]any{"support_chunk_delta": true},
		"support_lazy_fetch_stream": true, "is_old_user": true, "message_storage_type": 0,
		"conversation_mode":    2,
		"conversation_init_ext": initExt,
		"model_config": map[string]any{
			"model_item_key": itemKey,
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
			"local_conversation_id": localConv, "conversation_id": "", "bot_id": WorkBotID,
			"last_section_id": "", "last_message_index": nil,
			"local_permissions": []any{
				map[string]any{"permission_name": "ACCESS_COARSE_LOCATION", "status": 1},
				map[string]any{"permission_name": "ACCESS_FINE_LOCATION", "status": 1},
				map[string]any{"permission_name": "ACCESS_BACKGROUND_LOCATION", "status": 1},
			},
		},
		"messages":    []any{map[string]any{"local_message_id": msgID, "content_block": []any{block}, "message_status": 0}},
		"option":      opt,
		"user_context": []any{},
	}
	return json.Marshal(body)
}

// ---------- 小工具 ----------

func mustJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
