// messages.go 出站转换：wild-work 内部的 OpenAI Chat 请求体 → MiniMax 的
// Anthropic Messages 请求体（协议 §4）。
//
// 本仓架构是「外层三接口统一收敛成 OpenAI Chat 形状，再交给渠道」，而 MiniMax 的
// 推理端点是 **Anthropic Messages 形状**（`{apiHost}/mavis/api/v1/llm/v1/messages`），
// 故渠道层需要一层请求方言投影；入站方向的对偶在 sse.go。
//
// 顶层字段（协议 §4 的字段表，逐条落实）：
//
//	{model, stream:true, messages:[…], system:[…], tools:[…], thinking:{…}, output_config:{…}}
//
// max_tokens / temperature / stop_sequences 一律「有值才带」。
package minimax

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// buildMessagesBody 构造出站请求体。
//
// entry/known 是该模型在「动态目录 ∪ 静态兜底」里的能力声明（不认识时 known=false）：
// 协议 §4.1 的思考决策表**整张**都以模型能力为输入（是否必须 adaptive、有哪些档位），
// 所以 ChatStream 会先做一次目录查询再把结果传进来。
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

	out := map[string]any{
		"model":    model,
		"messages": msgs,
		// stream **恒为 true**（协议 §4：硬编码，不透传客户端的 stream 字段）。
		// 与 monkeycode 同款理由：渠道层契约是「ChatStream 返回的一定是 SSE 流」，
		// 非流式客户端由网关用 Aggregate 收敛；一旦透传 false，上游会回**单个 JSON
		// 对象**，被 SSE 转换器整包丢弃 → HTTP 200 + 空 content + 0 usage 的静默失败。
		"stream": true,
	}
	// system 是**顶层字段**，不是 role:"system" 消息（协议 §4）。
	// 形状说明：文档只钉死「顶层」，没有钉死字符串 / 块数组两种形态（未确认）；
	// 这里取块数组——与同族 Anthropic 形状的 monkeycode 渠道一致，且将来可挂 cache_control。
	if len(sysTexts) > 0 {
		blocks := make([]any, 0, len(sysTexts))
		for _, s := range sysTexts {
			blocks = append(blocks, textBlock(s))
		}
		out["system"] = blocks
	}
	// max_tokens：有值才带（协议 §4）。
	if n, ok := posNum(in, "max_tokens"); ok {
		out["max_tokens"] = n
	} else if n, ok := posNum(in, "max_completion_tokens"); ok {
		out["max_tokens"] = n
	}
	if v, ok := in["temperature"]; ok && v != nil {
		out["temperature"] = v
	}
	// stop_sequences：非空数组才带（字符串单值需包成数组）。
	if seq := stopSequences(in["stop"]); len(seq) > 0 {
		out["stop_sequences"] = seq
	}
	// tools：`[{name, description, input_schema}]`（协议 §4）。
	// ⚠️ 文档的字段表里**没有 tool_choice** ⇒ 本实现不下发它，也不做「tool_choice=none
	// 就裁掉 tools」的改写（monkeycode 有，是因为那份评估文档给了实测依据）。
	if tools := convertTools(in["tools"]); len(tools) > 0 {
		out["tools"] = tools
	}
	// thinking / output_config：协议 §4.1 的决策表。
	if th, oc := thinkingFor(entry, known, effortOf(in)); th != nil {
		out["thinking"] = th
		if oc != nil {
			out["output_config"] = oc
		}
	}
	return json.Marshal(out)
}

// ---------------------------------------------------------------------------
// 思考字段决策（协议 §4.1）
// ---------------------------------------------------------------------------

// thinkingFor 按协议 §4.1 的决策表产出 (thinking, output_config)；thinking 为 nil
// 表示**整个 thinking 字段不下发**。
//
// 决策表（逐条来自参考实现与其实测注释）：
//  1. 必须 adaptive 的模型（id 前缀 MiniMax-M3.1）→ `{type:"adaptive"}`，有 effort 时
//     再加 `output_config.effort`。实测该前缀传 disabled 会**硬 400**（业务码 2013
//     "requires adaptive thinking"）；
//  2. `effort == "none"`（用户选「关闭思考」）→ `{type:"disabled"}`；
//  3. `effort == "on"`（用户选「开启思考」）→ `{type:"adaptive"}`；
//  4. 其它有 effort 的模型 → `{type:"adaptive"}` + `output_config.effort`；
//  5. M2.7 系（无档位表）→ 整个 thinking 不发（服务端默认思考）。
//
// ⚠️ 与文档表的一处**刻意差异**：文档把 "none" 列在第 1 条，本实现把
// 「必须 adaptive」提到最前。理由是那条规则是**硬 400 防护**（发送必然失败的请求
// 比忽略一个「关闭」意图更糟）；而按 §4.1 的档位构造规则，forced_on 模型根本不会
// 被追加 on/none 两档，所以真实流量里 M3.1 拿不到 "none" 这个值——两者不会冲突。
func thinkingFor(entry catalogEntry, known bool, effort string) (any, any) {
	// 档位合法性（协议 §4.1 末条）：选中的 effort 必须在该模型声明的档位表里，否则丢弃。
	// on/none 是本工具对 switchable 模型追加的两档**伪档位**，不参与上游 effort 校验。
	if effort != "" && effort != "on" && effort != "none" &&
		known && len(entry.Efforts) > 0 && !contains(entry.Efforts, effort) {
		effort = ""
	}
	if known && entry.requiresAdaptive() {
		if effort != "" && effort != "on" && effort != "none" {
			return map[string]any{"type": "adaptive"}, map[string]any{"effort": effort}
		}
		return map[string]any{"type": "adaptive"}, nil
	}
	switch effort {
	case "none":
		return map[string]any{"type": "disabled"}, nil
	case "on":
		return map[string]any{"type": "adaptive"}, nil
	case "":
		// 未表达档位：M2.7 系（无档位表）不发 thinking（协议 §4.1 第 5 条）；
		// M3（switchable）「不传 → 0 思考块」，同样不发（协议 §4.1 注释）。
		// 未知模型（known=false）也走这里：不发 = 交给服务端默认，是最保守的一档。
		return nil, nil
	default:
		return map[string]any{"type": "adaptive"}, map[string]any{"effort": effort}
	}
}

// effortOf 取思考档位（小写）：DSH 侧由 reasoning_effort 单字段表达（R63 出站注入）。
func effortOf(in map[string]any) string {
	s, _ := in["reasoning_effort"].(string)
	if s == "" {
		// 兼容 thinking:{type:"enabled"|"disabled"} 形态（部分客户端直接发这个）。
		if th, ok := in["thinking"].(map[string]any); ok {
			switch strings.ToLower(strings.TrimSpace(strOf(th["type"]))) {
			case "disabled":
				return "none"
			case "enabled", "adaptive":
				return "on"
			}
		}
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// ---------------------------------------------------------------------------
// 消息序列化（协议 §4.2 十条规则）
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
// 并把 system 内容单独交出（调用方放到顶层 system）。
//
// 规则（协议 §4.2，编号与文档一致）：
//
//  1. 先归一化「一等 tool 消息」形状（role:"tool" + 顶层 tool_call_id / toolCallId，
//     判据是**形状不是版本号**）；漏掉会 400 "tool call result does not follow tool call"
//     （业务码 2013 — 实测）；
//  2. 剔除无法配对的 tool_use / tool_result（**孤儿两侧都拒**）；
//  3. assistant 的工具调用 → `{type:"tool_use", id, name, input}`，input 解析成**对象**，
//     失败退化为 `{}`；
//  4. tool-result → **user 消息里的** `{type:"tool_result", tool_use_id, content, is_error?}`
//     —— Anthropic 没有 `role:"tool"`；
//  5. 同批 tool_result 必须合并进**紧跟** assistant(tool_use) 的那一条 user 消息，
//     且**不得跨 assistant 边界累积**（用「assistant 与结果成对提交」实现）；
//  6. system 角色消息被显式跳过（见下方偏差说明）；
//  7. 历史里的 reasoning 块**不回传**（Anthropic 要求 thinking 块带签名，而本实现
//     不持久化签名）；
//  8. 图片走 Anthropic `image` 块，`data` 是**裸 base64**（无 `data:` 前缀）——
//     OpenAI 的 `image_url` 形状会被服务端明确拒绝（400 unsupported content type
//     'image_url'，实测）；
//  9. 消息体里的图片**读不到 → 抛错**（不静默丢图）；工具结果里的图片读不到 → **跳过**
//     （区别对待是有意的）；
//  10. 模型是否支持图片由 supportsImage 判定（allowImage）：不支持却收到图片 → 报错。
//
// ⚠️ 规则 6 的偏差说明：参考实现「跳过 role:"system" 消息」的前提是 DSH harness 把
// system 放在顶层 options.system 里，messages 里的 system 只是残留。本仓内层协议是
// OpenAI Chat，system 恰恰**只在** messages 里，直接跳过就会把系统提示词整段丢掉。
// 故这里做的是「提升到顶层 system」：出站 messages 里同样**没有** system 角色消息
// （规则 6 的字面要求成立），但内容不丢。
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
			// OpenAI 形态的工具调用。
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
							// 规则 10：模型不支持图片却收到图片 → 报错（UNSUPPORTED_CONTENT 等价物）。
							if !allowImage {
								return nil, nil, fmt.Errorf("该模型不支持图片输入（supportsImage=false）")
							}
							img, ok := imageBlockOf(v)
							if !ok {
								// 规则 9：消息体里的图片读不到 → 抛错，不静默丢图。
								return nil, nil, fmt.Errorf("消息里的图片读不到（只支持 data: URL 形态的裸 base64；image_url 形状会被上游拒绝）")
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
	//
	// 「不跨 assistant 边界」的落地方式：向后扫描**直到下一个 assistant 消息**为止，
	// 把这段区间内的 tool_result 全部算作本轮的结果，然后**构造**一条紧跟 assistant 的
	// user 消息承载它们（结果在前、文本在后）。
	//
	// 为什么不要求「紧邻下一条必须是 tool 消息」：本仓的 Anthropic 兼容层
	// （internal/gateway/anthropic.go:236-253）会把同时含 tool_result 与文本的 user 消息
	// 拆成 `[user(文本), tool(结果A), tool(结果B)]` —— 结果前面插了一条 user 文本。
	// 若按最严格的紧邻判据，这条 tool_use 与两个 tool_result 会被**双向剔除**，
	// Claude Code 的整轮工具调用上下文就此丢失。区间上界仍是 assistant，
	// 所以「不得跨 assistant 边界累积」这条硬约束依旧成立。
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
			// 全是不成对的 tool_use：本区间没有任何结果可合并，后续 turn 原样保留
			//（它们的 results 会在各自的分支里被当孤儿剔除）。
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
			// 工具结果先给出，再补用户文本（Anthropic 惯例）。
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
	// 占位文本只是把角色序列拉合法，内容不参与语义（与 monkeycode 同款处理）。
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

// toolResultID 取工具结果消息的配对 id：**判据是形状不是版本号**（协议 §4.2 第 1 条），
// 故 snake_case 与 camelCase 两种都认。
func toolResultID(m map[string]any) string {
	for _, k := range []string{"tool_call_id", "toolCallId"} {
		if s := strings.TrimSpace(strOf(m[k])); s != "" {
			return s
		}
	}
	return ""
}

// toolResultBlock 把 role:"tool" 消息转成 tool_result 块（规则 4）。
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
// 只保留 tool_use_id / content / is_error 三个字段，避免把客户端的私有字段透传给上游。
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

// toolUseFromCall 把 OpenAI 形态的 tool_call 转成 tool_use 块（规则 3）。
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
// 失败退化为 `{}`（协议 §4.2 第 3 条）。
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

// convertTools 把 OpenAI 的 tools 定义转成 Anthropic 形态（规则见协议 §4 字段表）：
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
// 为什么 http(s) URL 也算读不到：协议 §4.2 第 8 条**只确认了裸 base64 可用**，
// 且 OpenAI 的 image_url 形状会被服务端明确拒绝；把网络 URL 变成 base64 需要本工具
// 自己去抓那张图（额外的出站请求 + 额外失败面），参考实现也没有这一步。
// 「source.type=url 是否被 agent.minimax.cn 接受」文档未确认 ⇒ 宁可报错，
// 也不发一个已知会被拒的形态。
func imageBlockOf(v map[string]any) (map[string]any, bool) {
	// 已是 Anthropic 形态（部分客户端直接透传）。
	if strOf(v["type"]) == "image" {
		src, _ := v["source"].(map[string]any)
		if src == nil || strOf(src["type"]) != "base64" {
			return nil, false // source.type=url：未确认，视为读不到
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

// splitDataURL 拆 `data:<media_type>;base64,<data>`；其它形态（http(s) URL、本地路径、
// 无 media_type 的裸 base64）一律算读不到。
//
// media_type 缺失时不算「可用」：Anthropic 的 base64 source 必填 media_type，
// 猜一个类型（如 image/png）就是在编协议事实。
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
	// data 必须是**裸 base64**（无 data: 前缀）——协议 §4.2 第 8 条。
	return map[string]any{"type": "image", "source": map[string]any{
		"type": "base64", "media_type": media, "data": clean,
	}}, true
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// modelOfMap 取请求体里的 model。
func modelOfMap(in map[string]any) string { return strOf(in["model"]) }

// upstreamModelName 去掉客户端模型 ID 上的渠道前缀（`minimax/MiniMax-M3` → `MiniMax-M3`）。
// handler 已把上游名写回 body.model，这里再兜一次：手填/直连场景可能仍带前缀。
func upstreamModelName(id string) string {
	s := strings.TrimSpace(id)
	s = strings.TrimPrefix(s, Kind.String()+"/")
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

// strOf 宽松取字符串（JSON 里偶发数值/布尔时也给一个稳定文本，避免 nil 拼接）。
func strOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}
