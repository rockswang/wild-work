// body.go 出站对话请求体的改写（协议 §2.3）。
//
// 改写的**全部**依据都是协议里那份实测清单，逐条列在这里，方便对照：
//
//	stream           恒 true（上游只按流式服务）
//	prompt_cache_key 必须带，取值 = Session-Id；缺它服务端前缀缓存命中恒为 0
//	include          ['reasoning.encrypted_content']
//	reasoning_summary 'auto'
//	tool_stream      恒 true（让大工具参数分段流式传输，对齐 IDE）
//	max_tokens       缺省 65536；>65536 会被压到 65536（实测 131072 触发空流）
//	thinking         仅当客户端要求「关闭」时下发 {type:'disabled'}（服务端默认开着）；
//	                 本函数不发明该字段，客户端带什么就发什么
//	reasoning_effort / reasoning  实测完全无效 ⇒ 剥离（不下发）
//	tools            仅非 DSML 模式下发
//	messages         assistant 恒带 reasoning_content（缺失 → 400）；孤儿 tool_call/tool_result
//	                 必须整对剔除（否则后端对该会话此后每条消息都回 400）；
//	                 非 text 内容块丢弃
//
// deepseek-v4 的 **DSML 分支**（协议 §2.3）：`^deepseek-v4-(flash|pro)$` 的模型不下发
// `tools`，改把工具 schema 以文本注入 system（位置：system 之后、user 之前），要求模型用
// `<|DSML|...>` 语法输出调用。原因（源码实测记录）：标准 tool_calls 需一次性打包参数，
// SSE 静默 >60s 必被 APIG 网关掐断。注意判据**不覆盖** `deepseek-v4.1-flash`（它是另一个
// 模型，实测走标准 tools）——「别一刀切」指的就是这一点。
package codearts

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// 出站 max_tokens 的缺省与上限（协议 §2.3）。
const (
	maxTokensDefault = 65536
	maxTokensCeiling = 65536
)

// reDSMLModel DSML 分支的模型判据（**精确锚定**，不覆盖 deepseek-v4.1-flash）。
var reDSMLModel = regexp.MustCompile(`^deepseek-v4-(flash|pro)$`)

// isDSMLModel 报告该模型是否走 DSML 无 tools 分支。
func isDSMLModel(model string) bool { return reDSMLModel.MatchString(strings.TrimSpace(model)) }

// sessionIDs 一次对话的会话标识（Chat-Id / Session-Id 各 32 hex）。
// prompt_cache_key 与 Session-Id 同值（协议 §2.3），故两者必须同源生成。
type sessionIDs struct {
	chatID    string
	sessionID string
}

// newSessionIDs 生成一对会话标识。
func newSessionIDs() sessionIDs {
	return sessionIDs{chatID: randomHex(16), sessionID: randomHex(16)}
}

// headers 对话端点的未签名头（协议 §2.2 表）。
func (s sessionIDs) headers() map[string]string {
	return map[string]string{
		HeaderChatID:    s.chatID,
		HeaderSessionID: s.sessionID,
		HeaderLang:      "en",
	}
}

// buildChatBody 改写客户端请求体为上游可接受的形态；sessionID 回填 prompt_cache_key。
func buildChatBody(body []byte, model string, sessionID string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("请求体不是 JSON 对象: %w", err)
	}
	if obj == nil {
		return nil, errors.New("请求体为空 JSON")
	}

	obj["stream"] = true
	if _, has := obj["prompt_cache_key"]; !has && strings.TrimSpace(sessionID) != "" {
		obj["prompt_cache_key"] = sessionID
	}
	if _, has := obj["include"]; !has {
		obj["include"] = []any{"reasoning.encrypted_content"}
	}
	if _, has := obj["reasoning_summary"]; !has {
		obj["reasoning_summary"] = "auto"
	}
	if _, has := obj["tool_stream"]; !has {
		obj["tool_stream"] = true
	}
	clampMaxTokens(obj)
	// 实测完全无效的字段一律不下发（协议 §2.3「思考（推理）控制」）。
	delete(obj, "reasoning_effort")
	delete(obj, "reasoning")

	if msgs, ok := obj["messages"].([]any); ok {
		msgs = normalizeMessages(msgs)
		if isDSMLModel(model) {
			msgs = applyDSML(msgs, obj)
		}
		obj["messages"] = msgs
	} else if isDSMLModel(model) {
		// 没有 messages 数组（异常请求）：仅剥离 tools，交给上游报参数错。
		delete(obj, "tools")
		delete(obj, "tool_choice")
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("序列化请求体失败: %w", err)
	}
	return out, nil
}

// clampMaxTokens 归一 max_tokens：缺失/非正 → 缺省值；超过上限 → 压到上限。
func clampMaxTokens(obj map[string]any) {
	v := numberValue(obj["max_tokens"])
	switch {
	case v <= 0:
		obj["max_tokens"] = maxTokensDefault
	case v > maxTokensCeiling:
		obj["max_tokens"] = maxTokensCeiling
	}
}

// numberValue 宽松取数值（JSON 数字 → float64；数字字符串也认）。
func numberValue(raw any) float64 {
	switch v := raw.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, _ := v.Float64()
		return f
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(v), "%g", &f); err == nil {
			return f
		}
	}
	return 0
}

// normalizeMessages 出站消息归一（协议 §2.3「messages 序列化的硬约束」）：
//  1. 非 text 内容块丢弃（图片等一律不发）；
//  2. assistant 恒带 reasoning_content（缺失即补空串，否则上游 400）；
//  3. 孤儿 tool_call / tool_result 整对剔除。
func normalizeMessages(msgs []any) []any {
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if blocks, ok := msg["content"].([]any); ok {
			msg["content"] = textBlocksOnly(blocks)
		}
		if role, _ := msg["role"].(string); role == "assistant" {
			if _, has := msg["reasoning_content"]; !has {
				msg["reasoning_content"] = ""
			}
		}
	}
	return cleanupOrphanToolCalls(msgs)
}

// textBlocksOnly 只保留 type=="text" 的内容块。
func textBlocksOnly(blocks []any) []any {
	out := make([]any, 0, len(blocks))
	for _, b := range blocks {
		blk, ok := b.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := blk["type"].(string); t == "text" {
			out = append(out, blk)
		}
	}
	return out
}

// cleanupOrphanToolCalls 剔除无法配对的 tool_call 与 tool 结果（两侧按同一份
// keepCalls 对称裁剪，任何输入都不会留下半截配对）。
//
// 语义与 internal/upstream/tool_pairing.go 的 cleanupOrphanToolCalls 一致，
// 但**本包不复用** upstream.PrepareBody：那套管线含 WorkBuddy 专用的 tool_choice
// 改写（对象 → 裸字符串），而 CodeArts 是标准 OpenAI 形态，改写反而会被上游拒。
func cleanupOrphanToolCalls(msgs []any) []any {
	callIDs := map[string]bool{}
	resultIDs := map[string]bool{}
	traffic := false
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		switch msg["role"] {
		case "tool":
			if id, ok := msg["tool_call_id"].(string); ok && id != "" {
				resultIDs[id] = true
				traffic = true
			}
		case "assistant":
			for _, tci := range toolCallsOf(msg) {
				if id, ok := tci["id"].(string); ok && id != "" {
					callIDs[id] = true
					traffic = true
				}
			}
		}
	}
	if !traffic {
		return msgs
	}
	keep := map[string]bool{}
	for id := range callIDs {
		if resultIDs[id] {
			keep[id] = true
		}
	}
	changed := false
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := msg["role"].(string); role != "assistant" {
			continue
		}
		tcs := toolCallsOf(msg)
		if len(tcs) == 0 {
			continue
		}
		kept := make([]any, 0, len(tcs))
		for _, tc := range tcs {
			if id, _ := tc["id"].(string); keep[id] {
				kept = append(kept, tc)
			}
		}
		if len(kept) == len(tcs) {
			continue
		}
		changed = true
		if len(kept) == 0 {
			delete(msg, "tool_calls")
			continue
		}
		msg["tool_calls"] = kept
	}
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		if role, _ := msg["role"].(string); role == "tool" {
			id, _ := msg["tool_call_id"].(string)
			if !keep[id] {
				changed = true
				continue
			}
		}
		out = append(out, m)
	}
	if !changed {
		return msgs
	}
	return out
}

// toolCallsOf 取消息的 tool_calls（过滤掉非对象元素）。
func toolCallsOf(msg map[string]any) []map[string]any {
	raw, ok := msg["tool_calls"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		if tc, ok := it.(map[string]any); ok {
			out = append(out, tc)
		}
	}
	return out
}

// applyDSML 切到 DSML 无 tools 分支：剥离 tools/tool_choice，把工具 schema 以文本
// 注入 system（位置：第一条 system 之后、user 之前）。
func applyDSML(msgs []any, obj map[string]any) []any {
	tools, _ := obj["tools"].([]any)
	delete(obj, "tools")
	delete(obj, "tool_choice")
	if len(tools) == 0 {
		// 本来就没有工具：DSML 分支无需注入（避免给普通对话塞一段无意义提示）。
		return msgs
	}
	return injectSystem(msgs, dsmlInstruction(tools))
}

// dsmlInstruction 组装 DSML 语法说明 + 工具 JSON Schema。
func dsmlInstruction(tools []any) string {
	raw, err := json.Marshal(tools)
	if err != nil {
		raw = []byte("[]")
	}
	return strings.Join([]string{
		"你可以调用工具。需要调用工具时，必须**只**用下面的 DSML 语法输出，不要使用任何其它工具调用格式：",
		`<|DSML|tool_calls><|DSML|invoke name="工具名"><|DSML|parameter name="参数名" string="true">参数值</|DSML|parameter></|DSML|invoke></|DSML|tool_calls>`,
		"规则：一次可以写多个 <|DSML|invoke>；每个参数一个 <|DSML|parameter>；字符串参数写 string=\"true\"（参数值是 JSON 字符串字面量），数字/布尔参数写 string=\"false\"；工具调用块之外不要输出任何工具调用语法。",
		"可用工具的 JSON Schema：" + string(raw),
	}, "\n")
}

// injectSystem 把文本作为一条 system 消息插到第一条 system 之后；没有 system 时插到最前。
func injectSystem(msgs []any, text string) []any {
	wrapper := map[string]any{"role": "system", "content": text}
	out := make([]any, 0, len(msgs)+1)
	inserted := false
	for _, m := range msgs {
		out = append(out, m)
		if inserted {
			continue
		}
		if msg, ok := m.(map[string]any); ok {
			if role, _ := msg["role"].(string); role == "system" {
				out = append(out, wrapper)
				inserted = true
			}
		}
	}
	if !inserted {
		out = append([]any{wrapper}, out...)
	}
	return out
}
