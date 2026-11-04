// shape.go 把客户端请求体整形成「官方 OpenCode 2.2.5 CLI 会话」。
//
// 免费档的判定按 2026-11-04 官方 2.2.5 抓包的形态族对齐（R154）：
//   - system 正文必须是官方 2.2.5 那一段（prompt.go，内嵌原文 + 8 个真值占位符），
//     含其中真实位置的 `<env>` 块与块后的 worktree 句；
//   - 工具集必须是官方 **12** 个工具（edit/glob/grep/question/read/shell/skill/
//     subagent/webfetch/websearch/write/execute，各 strict:false，`execute` 收尾），
//     且**不得残留其他客户端的工具词汇** ⇒ 客户端工具一律改名成 `clint_<摘要>`
//     （tools.go），再追加官方全集；
//   - 聊天体必须带 `stream_options.include_usage` 与 `max_completion_tokens`。
//
// 整形顺序严格照搬参考实现（opencode2api v1.4.6 / dsh `applyFingerprint`）：
// 注入 prompt → 收集「已逐字官方定义」的工具（keep，不改名不重复）→ 改名 +
// 追加官方集 → 追加占位说明 → 删除 `tool_choice:none` → 补流式用量与长度上限。
package oczen

import (
	"encoding/json"
	"strings"
)

// v2CompletionLimit 官方 2.x 客户端在聊天体上带的 max_completion_tokens。
const v2CompletionLimit = 32000

// shapeChatRequest 就地把请求体整形成官方 CLI 形态，返回本次改名集合
// （响应侧据此把 `clint_*` 还原成客户端声明的名字）。
func shapeChatRequest(obj map[string]any) *toolRenameMap {
	if obj == nil {
		return newToolRenameMap()
	}
	// 免费档只接受流式；非流式客户端由 server 侧 Aggregate 本地聚合。
	obj["stream"] = true

	// 1. 官方 system prompt。
	injectOfficialPrompt(obj)

	// 2. 收集已逐字带官方定义的工具：它们是「官方声明」，不改名也不再追加，
	//    从而让重复整形保持幂等。
	keep := map[string]bool{}
	if tools, ok := obj["tools"].([]any); ok {
		for _, raw := range tools {
			name := renamableChatToolName(raw)
			if name == "" {
				continue
			}
			if carriesOfficialDefinition(raw, name) {
				keep[name] = true
			}
		}
	}

	// 3. 改名 + 追加官方集。
	renames, injected := renameAndCompleteTools(obj, keep)

	// 4. 注入的官方工具是「真工具」，模型会真的去调；用提示词压住它们，
	//    但不得误伤调用方自己的同名工具（措辞见 placeholderNote）。
	if len(injected) > 0 {
		appendSystemContext(obj, placeholderNote(injected))
	}

	// 5. 绝不发 tool_choice:none（上游把「禁用工具」读成非 CLI 形态）。
	if isToolChoiceNone(obj["tool_choice"]) {
		delete(obj, "tool_choice")
	}

	// 6. 聊天体自带的常量。
	ensureChatUsage(obj)
	ensureV2CompletionLimit(obj)

	// 7. 至此不再单独挪动 `<env>` 块：官方 2.2.5 的正文（含 env 块与其后的 worktree 句）
	//    由 injectOfficialPrompt 一次性整体前置，块的位置就是官方位置（R154 推翻 R153）。
	return renames
}

// renamableChatToolName 返回一个 Chat 线工具条目里的客户端工具名；
// 非 function 形状（含 Responses/Anthropic 内建工具）返回空串，表示不改名。
func renamableChatToolName(entry any) string {
	tool, ok := entry.(map[string]any)
	if !ok {
		return ""
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := fn["name"].(string)
	return name
}

// carriesOfficialDefinition 报告某工具条目是否逐字携带 `name` 的官方定义
// （描述逐字相同 + 参数 canonical JSON 相同）。
func carriesOfficialDefinition(entry any, name string) bool {
	tool, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		return false
	}
	description, _ := fn["description"].(string)
	rendered := officialTool(name)
	officialFn, _ := rendered["function"].(map[string]any)
	if officialFn == nil {
		return false
	}
	wantDescription, _ := officialFn["description"].(string)
	if description != wantDescription {
		return false
	}
	return canonicalJSON(fn["parameters"]) == canonicalJSON(officialFn["parameters"])
}

// canonicalJSON 把任意 JSON 值序列化成稳定形态（object 键序由 encoding/json 排序）。
func canonicalJSON(value any) string {
	buf, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(buf)
}

// renameAndCompleteTools 把客户端工具改名成不透明线上名，并追加官方全集。
// 返回改名集合与本轮实际追加的官方工具名。
func renameAndCompleteTools(obj map[string]any, keep map[string]bool) (*toolRenameMap, []string) {
	renames := newToolRenameMap()
	used := map[string]bool{}

	items, _ := obj["tools"].([]any)
	// 旧版 OpenAI 的 functions 数组先转成 tools 形状，同样参与改名。
	if legacy, ok := obj["functions"].([]any); ok {
		items = append(items, convertLegacyFunctions(legacy, renames, used)...)
		delete(obj, "functions")
	}

	shaped := make([]any, 0, len(items)+len(officialToolOrder))
	for _, item := range items {
		name := renamableChatToolName(item)
		if name == "" {
			// 无客户端名字的条目（内建/服务端工具）原样保留。
			shaped = append(shaped, item)
			continue
		}
		// 官方声明与上一轮已改名的线上名都不再动。
		if keep[name] || strings.HasPrefix(name, clientToolPrefix) {
			shaped = append(shaped, item)
			continue
		}
		wire, ok := renames.wireOf(name)
		if !ok {
			wire = clientToolWireName(name, used)
			renames.add(name, wire)
		}
		if wire != name {
			shaped = append(shaped, withToolName(item, wire))
		} else {
			shaped = append(shaped, item)
		}
	}

	injected := make([]string, 0, len(officialToolOrder))
	for _, name := range officialToolOrder {
		if keep[name] {
			continue
		}
		shaped = append(shaped, officialTool(name))
		injected = append(injected, name)
	}
	obj["tools"] = shaped

	rewriteToolReferences(obj, renames)
	return renames, injected
}

// withToolName 返回一个换了 function.name 的浅拷贝（不改调用方原来的对象）。
func withToolName(entry any, name string) map[string]any {
	tool, _ := entry.(map[string]any)
	out := make(map[string]any, len(tool)+1)
	for k, v := range tool {
		out[k] = v
	}
	fn, _ := tool["function"].(map[string]any)
	nextFn := make(map[string]any, len(fn)+1)
	for k, v := range fn {
		nextFn[k] = v
	}
	nextFn["name"] = name
	out["function"] = nextFn
	return out
}

// convertLegacyFunctions 把旧版 `functions` 数组转成 tools 形状并改名。
func convertLegacyFunctions(entries []any, renames *toolRenameMap, used map[string]bool) []any {
	converted := make([]any, 0, len(entries))
	for _, raw := range entries {
		fnDef, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := fnDef["name"].(string)
		if name == "" {
			continue
		}
		wire, ok := renames.wireOf(name)
		if !ok {
			wire = clientToolWireName(name, used)
			renames.add(name, wire)
		}
		description, _ := fnDef["description"].(string)
		parameters, ok := fnDef["parameters"]
		if !ok || parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		fn := map[string]any{"name": wire, "description": description, "parameters": parameters}
		if strict, ok := fnDef["strict"]; ok {
			fn["strict"] = strict
		}
		converted = append(converted, map[string]any{"type": "function", "function": fn})
	}
	return converted
}

// rewriteToolReferences 把请求体里所有对客户端工具的引用一并改名，
// 保证 body 自洽：tool_choice（含 function 包装）、旧版 function_call、
// 以及 messages 里 tool_calls[].function.name。
func rewriteToolReferences(obj map[string]any, renames *toolRenameMap) {
	if renames.len() == 0 {
		return
	}
	wireOf := func(container map[string]any) (string, bool) {
		name, ok := container["name"].(string)
		if !ok {
			return "", false
		}
		wire, ok := renames.wireOf(name)
		if !ok || wire == name {
			return "", false
		}
		return wire, true
	}

	if choice, ok := obj["tool_choice"].(map[string]any); ok {
		if fn, ok := choice["function"].(map[string]any); ok {
			if wire, changed := wireOf(fn); changed {
				next := cloneMap(choice)
				nextFn := cloneMap(fn)
				nextFn["name"] = wire
				next["function"] = nextFn
				obj["tool_choice"] = next
			}
		} else if wire, changed := wireOf(choice); changed {
			next := cloneMap(choice)
			next["name"] = wire
			obj["tool_choice"] = next
		}
	}

	if call, ok := obj["function_call"].(map[string]any); ok {
		if wire, changed := wireOf(call); changed {
			next := cloneMap(call)
			next["name"] = wire
			obj["function_call"] = next
		}
	}

	messages, _ := obj["messages"].([]any)
	for i, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		var next map[string]any
		if call, ok := msg["function_call"].(map[string]any); ok {
			if wire, changed := wireOf(call); changed {
				next = cloneMap(msg)
				nextCall := cloneMap(call)
				nextCall["name"] = wire
				next["function_call"] = nextCall
			}
		}
		if calls, ok := msg["tool_calls"].([]any); ok {
			rewritten := make([]any, len(calls))
			changed := false
			for j, rawCall := range calls {
				call, ok := rawCall.(map[string]any)
				if !ok {
					rewritten[j] = rawCall
					continue
				}
				fn, ok := call["function"].(map[string]any)
				if !ok {
					rewritten[j] = rawCall
					continue
				}
				wire, hit := wireOf(fn)
				if !hit {
					rewritten[j] = rawCall
					continue
				}
				nextCall := cloneMap(call)
				nextFn := cloneMap(fn)
				nextFn["name"] = wire
				nextCall["function"] = nextFn
				rewritten[j] = nextCall
				changed = true
			}
			if changed {
				if next == nil {
					next = cloneMap(msg)
				}
				next["tool_calls"] = rewritten
			}
		}
		if next != nil {
			messages[i] = next
		}
	}
}

// cloneMap 浅拷贝一个 JSON 对象。
func cloneMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src)+1)
	for k, v := range src {
		out[k] = v
	}
	return out
}

// placeholderNote 是注入官方工具后追加到 system 的占位说明。
//
// v2 的定义是**真工具**，模型会真的去调注入的 `read`/`shell`。本仓的下游并不
// 实现官方工具集，必须用提示词压住；写成「不可用」会让模型连调用方自己的
// `Read`/`Bash` 也一起拒绝，「占位、请用别的工具」才既压住注入项又不误伤。
func placeholderNote(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "`" + name + "`"
	}
	return "The tools " + strings.Join(quoted, ", ") +
		" are part of the host CLI's own toolset and are placeholders in this session; they do nothing here. " +
		"Use the other tools you have been given instead."
}

// appendSystemContext 在**不替换**调用方 system 文本/分片的前提下，把一段上下文
// 追加到 system 内容末尾。
//
// 调用点：官方正文（含 `<env>` 块与其后的 worktree 句）由 injectOfficialPrompt
// 整体前置，本函数追加的占位说明必然落在官方正文之后——这与官方 2.2.5 抓包一致
// （官方正文之后也还有 worktree 句，说明「正文之后不能再有文本」并不成立，R154）。
func appendSystemContext(obj map[string]any, note string) {
	if note == "" {
		return
	}
	messages, _ := obj["messages"].([]any)
	if len(messages) > 0 {
		if first, ok := messages[0].(map[string]any); ok {
			role, _ := first["role"].(string)
			if role == "system" || role == "developer" {
				switch content := first["content"].(type) {
				case string:
					next := cloneMap(first)
					if strings.TrimSpace(content) == "" {
						next["content"] = note
					} else {
						next["content"] = strings.TrimRight(content, "\n") + "\n\n" + note
					}
					obj["messages"] = append([]any{next}, messages[1:]...)
					return
				case []any:
					next := cloneMap(first)
					fragments := make([]any, 0, len(content)+1)
					fragments = append(fragments, content...)
					fragments = append(fragments, map[string]any{"type": "text", "text": note})
					next["content"] = fragments
					obj["messages"] = append([]any{next}, messages[1:]...)
					return
				}
			}
		}
	}
	obj["messages"] = append([]any{map[string]any{"role": "system", "content": note}}, messages...)
}

// ensureChatUsage 给流式聊天体补 stream_options.include_usage=true。
func ensureChatUsage(obj map[string]any) bool {
	if stream, _ := obj["stream"].(bool); !stream {
		return false
	}
	if options, ok := obj["stream_options"].(map[string]any); ok {
		if include, _ := options["include_usage"].(bool); include {
			return false
		}
		next := cloneMap(options)
		next["include_usage"] = true
		obj["stream_options"] = next
		return true
	}
	obj["stream_options"] = map[string]any{"include_usage": true}
	return true
}

// ensureV2CompletionLimit 在调用方没指定任何长度上限时补官方默认值。
// max_completion_tokens 与 max_tokens 都缺才补（尊重调用方显式选择）。
func ensureV2CompletionLimit(obj map[string]any) bool {
	if _, ok := obj["max_completion_tokens"]; ok {
		return false
	}
	if _, ok := obj["max_tokens"]; ok {
		return false
	}
	obj["max_completion_tokens"] = v2CompletionLimit
	return true
}

// isToolChoiceNone 判断 tool_choice 是否表示「禁止调用工具」。
func isToolChoiceNone(value any) bool {
	if choice, ok := value.(string); ok {
		return choice == "none"
	}
	if choice, ok := value.(map[string]any); ok {
		if kind, _ := choice["type"].(string); kind == "none" {
			return true
		}
	}
	return false
}
