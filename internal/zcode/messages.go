// messages.go 出站转换：wild-work 内部的 OpenAI Chat 请求体 → ZCode 的
// **Anthropic Messages** 请求体（协议 §1.4）。
//
// 本仓架构是「外层三接口统一收敛成 OpenAI Chat 形状，再交给渠道」，而 ZCode 的推理端点
// 是 Anthropic Messages 形状（且免费通道实测只认这一种：`openai` / 裸 `v1/chat/completions`
// 一律 404 page not found）。故渠道层需要一层请求方言投影；入站方向的对偶在 sse.go。
//
// 顶层字段（协议 §1.4 的字段表，逐条落实）：
//
//	{model, max_tokens, system:[…], messages:[…], stream:true,
//	 temperature?, stop_sequences?, tools?, output_config?}
//
// 三处**渠道专属**、与同族 Anthropic 渠道（minimax / monkeycode）不同的地方：
//
//  1. `system` **必须**以官方身份块开头（identity.go）——这是准入，不是提示词；
//  2. 首轮 user 消息 content 数组最前面要插 `<system-reminder>` 日期块（官方形态）；
//  3. 思考档位的下发字段是 `output_config.effort`（**不是** `reasoning_effort`，
//     也不是 `thinking`）：官方把「怎么表达档位」在 `client/configs` 里一并下发了
//     （`reasoning.levels[level].anthropic.set = {path:["output_config","effort"], …}`）。
package zcode

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// defaultMaxTokens 请求体未给 max_tokens 时的兜底输出上限。
//
// 依据：参考实现 `body.max_tokens = options.maxTokens ?? 8192`（协议 §1.4 的字段表）。
// ⚠ 刻意**不**用模型声明的 maxCompletionTokens（128000）当兜底：那是模型能力上限，
// 不是「客户端没表态时的合理默认」；照抄参考实现的 8192 更保守。
const defaultMaxTokens = 8192

// buildMessagesBody 构造出站请求体。
//
// entry/known 是该模型在「动态目录 ∪ 静态兜底」里的能力声明（不认识时 known=false）：
// 思考档位是否合法必须以模型声明为输入（R49：只透传上游声明的档位，绝不编造）。
func buildMessagesBody(raw []byte, entry catalogEntry, known bool) ([]byte, error) {
	var in map[string]any
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON：%w", err)
	}
	model := upstreamModelName(modelOfMap(in))
	if model == "" {
		return nil, fmt.Errorf("请求体缺 model 字段")
	}
	msgs, sysTexts, err := serializeMessages(in["messages"], known && entry.SupportsImage)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("请求里没有可用消息")
	}
	// 官方形态：首轮 user 消息 content 数组最前面插 `<system-reminder>` 日期块。
	msgs = withContextPrefix(msgs, time.Now())

	system := identitySystemFn(strings.Join(sysTexts, "\n\n"), environment(model))

	out := map[string]any{
		"model":    model,
		"messages": msgs,
		"system":   system,
		// stream **恒为 true**（协议 §1.4：官方硬编码）。
		// 与 minimax/monkeycode 同款理由：本渠道层的契约是「ChatStream 返回的一定是 SSE 流」，
		// 非流式客户端由网关用 Aggregate 收敛；一旦透传 false，上游会回**单个 JSON 对象**，
		// 被 SSE 转换器整包丢弃 → HTTP 200 + 空 content + 0 usage 的静默失败。
		"stream": true,
	}
	// max_tokens：**恒下发**（官方如此）。有值用值，无值用兜底。
	if n, ok := posNum(in, "max_tokens"); ok {
		out["max_tokens"] = n
	} else if n, ok := posNum(in, "max_completion_tokens"); ok {
		out["max_tokens"] = n
	} else {
		out["max_tokens"] = defaultMaxTokens
	}
	if v, ok := in["temperature"]; ok && v != nil {
		out["temperature"] = v
	}
	// stop_sequences：非空数组才带（字符串单值需包成数组）。
	if seq := stopSequences(in["stop"]); len(seq) > 0 {
		out["stop_sequences"] = seq
	}
	// tools：扁平 `[{name, description, input_schema}]`（协议 §1.4）。
	// ⚠ tools 必须真的下发（参考实现把它列为「三个必须真的做到的点」之一）：
	// 漏发会让模型在正文里臆造 XML/文本形态的工具调用，harness 认不出 → 任务终止。
	// 给**最后一个** tool 打缓存断点（前缀式缓存覆盖「system + 全部 tools」整段）。
	if tools := convertTools(in["tools"]); len(tools) > 0 {
		out["tools"] = withToolCacheBreakpoint(tools)
	}
	// 思考档位：`output_config.effort`，且**只在该模型声明确有该档位时**下发（R49）。
	if effort := declaredEffort(entry, known, effortOf(in)); effort != "" {
		out["output_config"] = map[string]any{"effort": effort}
	}
	return json.Marshal(out)
}

// declaredEffort 判定档位是否可以下发：必须落在模型声明的档位表里。
//
// 未知模型（known=false）**不下发**：上游「不传就用 defaultLevel」是安全的默认，
// 而按猜测下发一个可能非法的档位会让整个请求失败（档位只是锦上添花）。
func declaredEffort(entry catalogEntry, known bool, effort string) string {
	if effort == "" || !known || len(entry.Efforts) == 0 {
		return ""
	}
	if !contains(entry.Efforts, effort) {
		return ""
	}
	return effort
}

// effortOf 取思考档位（小写归一）。
//
// DSH 侧由 `reasoning_effort` 单字段表达（R63 出站注入）；同时兼容部分客户端直接发
// `thinking:{type:…}` 的形态。
//
// ⚠ 与 minimax 的差异：本渠道**没有**「关闭思考 / 开启思考」这两个伪档位的对应写法
// （协议 §1.4 只有 `effort` 三档 low/high/max），故 "none"/"on" 一律归一成空串
// （= 不下发），而不是编一个不存在的字段。
func effortOf(in map[string]any) string {
	if s := strings.ToLower(strings.TrimSpace(strOf(in["reasoning_effort"]))); s != "" {
		return normalizeEffort(s)
	}
	if th, ok := in["thinking"].(map[string]any); ok {
		switch strings.ToLower(strings.TrimSpace(strOf(th["type"]))) {
		case "disabled":
			return "" // 本渠道无法关闭思考：不发任何档位（交给上游默认）
		case "enabled", "adaptive":
			return "" // 「开启」不是档位，上游默认即开启
		}
	}
	return ""
}

// normalizeEffort 归一档位名：丢弃本渠道不存在的伪档位（none/on/off/default）。
func normalizeEffort(s string) string {
	switch s {
	case "", "none", "on", "off", "default", "auto":
		return ""
	default:
		return s
	}
}

// withToolCacheBreakpoint 给最后一个 tool 打 `cache_control:{type:'ephemeral'}` 断点。
//
// 只打一个点的理由与 system 侧相同（前缀式缓存 + 断点数量上限），
// 且这个点覆盖「system + 全部 tools」这一整段可缓存前缀。
func withToolCacheBreakpoint(tools []any) []any {
	if len(tools) == 0 {
		return tools
	}
	if m, ok := tools[len(tools)-1].(map[string]any); ok {
		m["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	return tools
}

// ---------------------------------------------------------------------------
// 消息序列化（协议 §1.4 的转换规则表）
// ---------------------------------------------------------------------------

// turn 是中间形态：一个角色 + 三类块。
type turn struct {
	role string
	// texts 文本 / 图片块（顺序保留）。
	texts []any
	// uses tool_use 块（仅 assistant）。
	uses []map[string]any
	// results tool_result 块（仅 user）。
	results []map[string]any
}

// serializeMessages 把 OpenAI Chat 的 messages 翻成 Anthropic messages，
// 并把 system 内容单独交出（调用方交给身份块构造器放顶层 system）。
//
// 规则（协议 §1.4 的转换规则表 + 同族 Anthropic 渠道的既有实现口径）：
//
//  1. 先归一化「一等 tool 消息」形状（role:"tool" + 顶层 tool_call_id / toolCallId，
//     判据是**形状不是版本号**）；
//  2. 剔除无法配对的 tool_use / tool_result（**孤儿两侧都拒**）；
//  3. assistant 的工具调用 → `{type:"tool_use", id, name, input}`，input 解析成**对象**，
//     失败退化为 `{}`；
//  4. tool-result → **user 消息里的** `{type:"tool_result", tool_use_id, content, is_error?}`
//     —— Anthropic 没有 `role:"tool"`；
//  5. 同批 tool_result 必须合并进**紧跟** assistant(tool_use) 的那一条 user 消息，
//     且**不得跨 assistant 边界累积**；
//  6. system / developer 角色消息**提升到顶层 system**（本仓内层协议 system 只出现在
//     messages 里，直接跳过会把系统提示词整段丢掉；出站 messages 里同样没有 system 角色）；
//  7. 历史里的 reasoning / thinking 块**不回传**（Anthropic 要求 thinking 块带签名，
//     而本实现不持久化签名）；
//  8. 图片走 Anthropic `image` 块，`data` 是**裸 base64**（无 `data:` 前缀）；
//  9. 消息体里的图片**读不到 → 抛错**（不静默丢图）；工具结果里的图片读不到 → **跳过**；
//  10. 模型是否支持图片由 supportsImage 判定（allowImage）：不支持却收到图片 → 报错。
func serializeMessages(raw any, allowImage bool) ([]any, []string, error) {
	list, _ := raw.([]any)
	var (
		sysTexts []string
		turns    []turn
	)
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(strOf(m["role"])))
		switch role {
		case "system", "developer":
			sysTexts = append(sysTexts, textOfContent(m["content"])...)
		case "assistant":
			t := turn{role: "assistant"}
			for _, b := range contentList(m["content"]) {
				switch v := b.(type) {
				case string:
					if v != "" {
						t.texts = append(t.texts, textBlock(v))
					}
				case map[string]any:
					if u, ok := toolUseBlock(v); ok {
						t.uses = append(t.uses, u)
						continue
					}
					if isTextBlock(v) && strOf(v["text"]) != "" {
						t.texts = append(t.texts, textBlock(strOf(v["text"])))
					}
					// 其余块（reasoning / thinking / signature 等）一律丢弃：规则 7。
				}
			}
			if calls, ok := m["tool_calls"].([]any); ok {
				for _, c := range calls {
					cm, _ := c.(map[string]any)
					if u, ok := toolUseFromCall(cm); ok {
						t.uses = append(t.uses, u)
					}
				}
			}
			turns = append(turns, t)
		default:
			// user（含未知角色）与 role:"tool"/"function" 都是 user 侧内容：
			// 规则 4 —— Anthropic 里工具结果属于 user 消息。
			t := turn{role: "user"}
			if id := toolResultID(m); id != "" || role == "tool" || role == "function" {
				t.results = append(t.results, toolResultBlock(id, m))
			} else {
				for _, b := range contentList(m["content"]) {
					switch v := b.(type) {
					case string:
						if v != "" {
							t.texts = append(t.texts, textBlock(v))
						}
					case map[string]any:
						switch strOf(v["type"]) {
						case "tool_result":
							// 客户端已按 Anthropic 形态传来的历史（罕见但合法）。
							t.results = append(t.results, sanitizeToolResult(v))
						case "image_url", "image", "input_image":
							if !allowImage {
								return nil, nil, fmt.Errorf("该模型不支持图片输入（supportsImage=false）")
							}
							img, ok := imageBlockOf(v)
							if !ok {
								// 规则 9：消息体里的图片读不到 → 抛错，不静默丢图。
								return nil, nil, fmt.Errorf("消息里的图片读不到（只支持 data: URL 形态的裸 base64）")
							}
							t.texts = append(t.texts, img)
						default:
							if isTextBlock(v) && strOf(v["text"]) != "" {
								t.texts = append(t.texts, textBlock(strOf(v["text"])))
							}
						}
					}
				}
			}
			turns = append(turns, t)
		}
	}

	// 规则 2 / 5：配对 + 合并成「紧跟 assistant 的那一条 user 消息」。
	// 「不跨 assistant 边界」的落地方式：向后扫描到下一个 assistant 为止，
	// 把这段区间内的 tool_result 全部算作本轮结果，再**构造**一条紧跟 assistant 的
	// user 消息承载它们（结果在前、文本在后）。
	//
	// 为什么不要求「紧邻下一条必须是 tool 消息」：本仓的 Anthropic 兼容层会把
	// 「同时含 tool_result 与文本的 user 消息」拆成 `[user(文本), tool(结果A), …]`，
	// 结果前面插了一条 user 文本。用最严格的紧邻判据会把这类工具调用上下文
	// **双向剔除**，Claude Code 的整轮工具上下文就此丢失。区间上界仍是 assistant，
	// 故「不得跨 assistant 边界累积」这条硬约束依旧成立。
	emitted := make([]turn, 0, len(turns))
	for i := 0; i < len(turns); i++ {
		t := turns[i]
		if t.role != "assistant" || len(t.uses) == 0 {
			// 不在任何 assistant(tool_use) 区间内的 tool_result 全是孤儿 → 剔除。
			t.results = nil
			emitted = append(emitted, t)
			continue
		}
		end := i + 1
		for end < len(turns) && turns[end].role != "assistant" {
			end++
		}
		resIDs := make(map[string]bool, 4)
		for k := i + 1; k < end; k++ {
			for _, r := range turns[k].results {
				if id := strOf(r["tool_use_id"]); id != "" {
					resIDs[id] = true
				}
			}
		}
		allowed := make(map[string]bool, len(t.uses))
		kept := make([]map[string]any, 0, len(t.uses))
		for _, u := range t.uses {
			id := strOf(u["id"])
			if id != "" && resIDs[id] {
				kept = append(kept, u)
				allowed[id] = true
			}
		}
		t.uses = kept
		emitted = append(emitted, t)
		if len(kept) == 0 {
			continue
		}
		merged := turn{role: "user"}
		for k := i + 1; k < end; k++ {
			for _, r := range turns[k].results {
				if allowed[strOf(r["tool_use_id"])] {
					merged.results = append(merged.results, r)
				}
			}
			merged.texts = append(merged.texts, turns[k].texts...)
			// 已并入合并消息：清空原 turn，避免同一内容被下发两次。
			turns[k].results = nil
			turns[k].texts = nil
		}
		if len(merged.results) > 0 || len(merged.texts) > 0 {
			emitted = append(emitted, merged)
		}
	}

	// 合并相邻同角色（Anthropic 要求角色交替）、丢掉空 turn。
	out := make([]any, 0, len(emitted))
	for _, t := range emitted {
		blocks := make([]any, 0, len(t.texts)+len(t.uses)+len(t.results))
		if t.role == "user" {
			for _, r := range t.results {
				blocks = append(blocks, r)
			}
			blocks = append(blocks, t.texts...)
		} else {
			blocks = append(blocks, t.texts...)
			for _, u := range t.uses {
				blocks = append(blocks, u)
			}
		}
		if len(blocks) == 0 {
			continue
		}
		if n := len(out); n > 0 {
			if prev, ok := out[n-1].(map[string]any); ok && prev["role"] == t.role {
				if pb, ok := prev["content"].([]any); ok {
					prev["content"] = append(pb, blocks...)
					continue
				}
			}
		}
		out = append(out, map[string]any{"role": t.role, "content": blocks})
	}
	if len(out) == 0 {
		return nil, sysTexts, fmt.Errorf("请求里没有可用消息")
	}
	// Anthropic 硬约束：messages 必须以 user 开头；assistant 开头时补一条占位 user。
	if r, _ := out[0].(map[string]any)["role"].(string); r != "user" {
		out = append([]any{map[string]any{"role": "user", "content": []any{textBlock("(continue)")}}}, out...)
	}
	return out, sysTexts, nil
}

// ---------------------------------------------------------------------------
// 块级转换
// ---------------------------------------------------------------------------

// textBlock 构造 Anthropic 文本块。
func textBlock(s string) map[string]any { return map[string]any{"type": "text", "text": s} }

// isTextBlock 判定块是否是文本（type 缺失也按文本处理，兼容 `{text:…}` 简写）。
func isTextBlock(v map[string]any) bool {
	t := strOf(v["type"])
	return t == "" || t == "text"
}

// contentList 把 content 归一成块数组（字符串 → 单元素数组；nil → 空）。
func contentList(v any) []any {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return nil
		}
		return []any{t}
	case []any:
		return t
	default:
		return nil
	}
}

// textOfContent 抽取消息里的纯文本（system 内容提升用）。
func textOfContent(v any) []string {
	var out []string
	for _, b := range contentList(v) {
		switch t := b.(type) {
		case string:
			if strings.TrimSpace(t) != "" {
				out = append(out, t)
			}
		case map[string]any:
			if isTextBlock(t) && strOf(t["text"]) != "" {
				out = append(out, strOf(t["text"]))
			}
		}
	}
	return out
}

// toolResultID 取工具结果消息的配对 id：**判据是形状不是版本号**，故 snake_case 与
// camelCase 两种都认。
func toolResultID(m map[string]any) string {
	for _, k := range []string{"tool_call_id", "toolCallId"} {
		if s := strings.TrimSpace(strOf(m[k])); s != "" {
			return s
		}
	}
	return ""
}

// toolResultBlock 把 role:"tool" 消息转成 tool_result 块。
// 内容为纯文本时用字符串形态，含图片时用块数组（Anthropic 两种都接受）。
func toolResultBlock(id string, m map[string]any) map[string]any {
	block := map[string]any{"type": "tool_result", "tool_use_id": id}
	if b, ok := m["is_error"].(bool); ok && b {
		block["is_error"] = true
	} else if b, ok := m["isError"].(bool); ok && b {
		block["is_error"] = true
	}
	parts := []string{}
	blocks := []any{}
	for _, b := range contentList(m["content"]) {
		switch v := b.(type) {
		case string:
			parts = append(parts, v)
		case map[string]any:
			switch strOf(v["type"]) {
			case "text", "":
				if strOf(v["text"]) != "" {
					parts = append(parts, strOf(v["text"]))
				}
			case "image_url", "image", "input_image":
				// 规则 9：**工具结果里的图片读不到就跳过**（与消息体里抛错不同）。
				if img, ok := imageBlockOf(v); ok {
					blocks = append(blocks, img)
				}
			}
		}
	}
	if len(blocks) == 0 {
		block["content"] = strings.Join(parts, "")
		return block
	}
	arr := make([]any, 0, len(parts)+len(blocks))
	for _, p := range parts {
		arr = append(arr, textBlock(p))
	}
	arr = append(arr, blocks...)
	block["content"] = arr
	return block
}

// sanitizeToolResult 清洗客户端已按 Anthropic 形态传来的 tool_result 块：
// 只保留 tool_use_id / content / is_error 三个字段，避免把客户端私有字段透传给上游。
func sanitizeToolResult(v map[string]any) map[string]any {
	out := map[string]any{"type": "tool_result", "tool_use_id": strOf(v["tool_use_id"])}
	if c, ok := v["content"]; ok {
		out["content"] = c
	}
	if b, ok := v["is_error"].(bool); ok && b {
		out["is_error"] = true
	}
	return out
}

// toolUseFromCall 把 OpenAI 形态的 tool_call 转成 tool_use 块。
func toolUseFromCall(c map[string]any) (map[string]any, bool) {
	if c == nil {
		return nil, false
	}
	fn, _ := c["function"].(map[string]any)
	name := strings.TrimSpace(strOf(fn["name"]))
	if name == "" {
		name = strings.TrimSpace(strOf(c["name"]))
	}
	if name == "" {
		return nil, false
	}
	id := strings.TrimSpace(strOf(c["id"]))
	if id == "" {
		id = strings.TrimSpace(strOf(c["tool_call_id"]))
	}
	args := fn["arguments"]
	if args == nil {
		args = c["arguments"]
	}
	if id == "" {
		// Anthropic 要求 tool_use 带 id；没有 id 就无法配对，本块随即会被当孤儿剔除。
		id = "call_" + name
	}
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": parseArgs(args)}, true
}

// toolUseBlock 识别已是工具调用形态的**内容块**（判据是形状不是版本号）：
//
//	Anthropic 原生  {type:"tool_use",  id, name, input}
//	harness 一等形状 {type:"tool-call", toolCallId|id, toolName|name, args|input|arguments}
func toolUseBlock(v map[string]any) (map[string]any, bool) {
	switch strOf(v["type"]) {
	case "tool_use", "tool-call", "tool_call":
	default:
		return nil, false
	}
	name := strings.TrimSpace(strOf(v["name"]))
	if name == "" {
		name = strings.TrimSpace(strOf(v["toolName"]))
	}
	if name == "" {
		name = strings.TrimSpace(strOf(v["tool_name"]))
	}
	if name == "" {
		return nil, false
	}
	id := strings.TrimSpace(strOf(v["id"]))
	if id == "" {
		id = strings.TrimSpace(strOf(v["toolCallId"]))
	}
	if id == "" {
		id = strings.TrimSpace(strOf(v["tool_call_id"]))
	}
	if id == "" {
		id = "call_" + name
	}
	in := v["input"]
	if in == nil {
		in = v["args"]
	}
	if in == nil {
		in = v["arguments"]
	}
	return map[string]any{"type": "tool_use", "id": id, "name": name, "input": parseArgs(in)}, true
}

// parseArgs 把工具入参解析成**对象**（Anthropic 要求 input 是对象，字符串会被拒）；
// 失败退化为 `{}`。
func parseArgs(v any) map[string]any {
	switch t := v.(type) {
	case map[string]any:
		if t == nil {
			return map[string]any{}
		}
		return t
	case string:
		if strings.TrimSpace(t) == "" {
			return map[string]any{}
		}
		var out map[string]any
		if err := json.Unmarshal([]byte(t), &out); err == nil && out != nil {
			return out
		}
	}
	return map[string]any{}
}

// convertTools 把 OpenAI 的 tools 定义转成 Anthropic 形态：
// `[{name, description, input_schema}]`，input_schema 来自 tool.parameters。
func convertTools(raw any) []any {
	list, _ := raw.([]any)
	out := make([]any, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			// 已是对端形态（{name, input_schema}）时直接透传 name/description/schema。
			fn = t
		}
		name := strings.TrimSpace(strOf(fn["name"]))
		if name == "" {
			continue
		}
		tool := map[string]any{"name": name}
		if d := strOf(fn["description"]); d != "" {
			tool["description"] = d
		}
		if p, ok := fn["parameters"].(map[string]any); ok && len(p) > 0 {
			tool["input_schema"] = p
		} else if p, ok := fn["input_schema"].(map[string]any); ok && len(p) > 0 {
			tool["input_schema"] = p
		} else {
			// 无 schema 时给最小合法对象：Anthropic 的 input_schema 必填。
			tool["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, tool)
	}
	return out
}

// ---------------------------------------------------------------------------
// 图片（规则 8 / 9）
// ---------------------------------------------------------------------------

// imageBlockOf 把 OpenAI 形态的图片块转成 Anthropic `image` 块，`data` 是**裸 base64**。
//
// ok=false 表示「读不到」，调用方据此决定抛错（消息体）还是跳过（工具结果）。
//
// 只认 data: URL：协议只确认了裸 base64 可用，http(s) URL 需要本工具自己去抓图
// （额外的出站请求 + 额外失败面），而 `source.type=url` 是否被上游接受**未确认**
// —— 宁可报错，也不发一个已知会被拒的形态。
func imageBlockOf(v map[string]any) (map[string]any, bool) {
	if strOf(v["type"]) == "image" {
		src, _ := v["source"].(map[string]any)
		if src == nil || strOf(src["type"]) != "base64" {
			return nil, false
		}
		return base64Image(strOf(src["media_type"]), strOf(src["data"]))
	}
	var url string
	switch t := v["image_url"].(type) {
	case string:
		url = t
	case map[string]any:
		url = strOf(t["url"])
	}
	if url == "" {
		return nil, false
	}
	media, data, ok := splitDataURL(url)
	if !ok {
		return nil, false
	}
	return base64Image(media, data)
}

// splitDataURL 拆 `data:<media_type>;base64,<data>`；其它形态一律算读不到。
func splitDataURL(url string) (media, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return "", "", false
	}
	rest := url[len(prefix):]
	m, d, found := strings.Cut(rest, ";base64,")
	if !found {
		return "", "", false
	}
	m = strings.TrimSpace(m)
	d = strings.TrimSpace(d)
	if m == "" || d == "" {
		return "", "", false
	}
	return m, d, true
}

// base64Image 构造 image 块并顺手做一次 base64 合法性校验（坏数据宁可判「读不到」，
// 也不要让上游回一个语焉不详的 400）；顺带剥掉 base64 内部的换行/空白。
func base64Image(media, data string) (map[string]any, bool) {
	media = strings.TrimSpace(media)
	clean := strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', ' ', '\t':
			return -1
		}
		return r
	}, data)
	if media == "" || clean == "" {
		return nil, false
	}
	if _, err := base64.StdEncoding.DecodeString(clean); err != nil {
		return nil, false
	}
	return map[string]any{"type": "image", "source": map[string]any{
		"type": "base64", "media_type": media, "data": clean,
	}}, true
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// modelOfMap 取请求体里的 model。
func modelOfMap(in map[string]any) string { return strOf(in["model"]) }

// upstreamModelName 去掉客户端模型 ID 上的渠道前缀（`zcode/GLM-5.3-Flash` → `GLM-5.3-Flash`）。
// handler 已把上游名写回 body.model，这里再兜一次：手填/直连场景可能仍带前缀。
func upstreamModelName(id string) string {
	s := strings.TrimSpace(id)
	s = strings.TrimPrefix(s, kindPrefix)
	return strings.TrimSpace(s)
}

// posNum 取正数数值字段（缺失/非正/非数值 → false）。
func posNum(in map[string]any, key string) (int64, bool) {
	switch v := in[key].(type) {
	case float64:
		if v > 0 {
			return int64(v), true
		}
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return n, true
		}
	case int:
		if v > 0 {
			return int64(v), true
		}
	case int64:
		if v > 0 {
			return v, true
		}
	}
	return 0, false
}

// stopSequences 归一 stop：字符串单值包成数组；非空数组原样；其余为空。
func stopSequences(v any) []any {
	switch t := v.(type) {
	case string:
		if strings.TrimSpace(t) != "" {
			return []any{t}
		}
	case []any:
		out := make([]any, 0, len(t))
		for _, s := range t {
			if str, ok := s.(string); ok && str != "" {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}
