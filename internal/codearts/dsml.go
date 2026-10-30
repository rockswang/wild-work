// DSML 响应解析（R144）。
//
// 背景：body.go 的 applyDSML 对 ^deepseek-v4-(flash|pro)$ 走"剥离 tools + schema 注入 system"
// 分支，要求模型用文本语法输出工具调用。实测确认（2026-10-24，跳过 applyDSML 只给标准 tools）：
// 该模型**在任何形态下**都输出 DSML 文本，`finish_reason=stop`、无 `tool_calls` 字段。
// 即 DSML 是模型固有行为，不是提示注入造成的。
//
// 此前只注入、不解析，导致客户端永远收不到 tool_calls，工具调用彻底失效。
// 本文件补上缺失的解析一半：把 DSML 文本转回标准 OpenAI tool_calls。
package codearts

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// DSML 标签字面量。与 body.go 的 dsmlInstruction 注入模板**必须逐字一致**，
// 否则解析器认不出自己写的提示。
const (
	dsmlBlockOpen = `<|DSML|tool_calls>`
	// dsmlBlockOpenAlt 真实上游（21 个工具）输出的开标签变体：末尾是 `|` 而非 `>`。
	dsmlBlockOpenAlt = `<|DSML|tool_calls|`
	dsmlBlockClose   = `</|DSML|tool_calls>`
)

var (
	// reDSMLBlockOpen 匹配开标签。真实上游实测（21 个工具时）输出的是
	// `<|DSML|tool_calls|`（末尾 `|` 而非 `>`），故必须容忍两种终止符，
	// 否则整块原样透传给客户端、工具调用失效（R144 实机验证发现）。
	reDSMLBlockOpen = regexp.MustCompile(`<\|DSML\|tool_calls[>|]`)
	// reDSMLInvoke 匹配一次调用：<|DSML|invoke name="X"> 内部 </|DSML|invoke>
	reDSMLInvoke = regexp.MustCompile(`(?s)<\|DSML\|invoke name="([^"]*)">(.*?)</\|DSML\|invoke>`)
	// reDSMLParam 匹配一个参数。属性顺序固定 name 在前、string 在后——与注入模板一致。
	// (?s) 让参数值可含换行。
	reDSMLParam = regexp.MustCompile(`(?s)<\|DSML\|parameter name="([^"]*)" string="(true|false)">(.*?)</\|DSML\|parameter>`)
)

// dsmlBlock 一次提取出的调用块（保存每次 invoke 的 name 与体内文本）。
type dsmlBlock struct {
	names  []string // invoke 的工具名
	bodies []string // 对应 invoke 的体内文本（参数区）
}

// parseDSMLBlocks 从响应正文里提取所有 DSML 调用块，返回块列表与剥离块之后的正文。
//
// 只认**成对**的块（开标签与闭标签都在）：残缺块原样留在 rest 里，不伪造 tool_calls
// （截断场景另有 finish_reason=length 判据，见决议 R144）。
func parseDSMLBlocks(s string) ([]dsmlBlock, string) {
	loc := reDSMLBlockOpen.FindStringIndex(s)
	if loc == nil {
		return nil, s
	}
	var blocks []dsmlBlock
	var rest strings.Builder
	from := 0
	for {
		loc = reDSMLBlockOpen.FindStringIndex(s[from:])
		if loc == nil {
			rest.WriteString(s[from:])
			break
		}
		i := from + loc[0]
		openEnd := from + loc[1] // 开标签实际结束位置（含 > 或 |）
		j := strings.Index(s[openEnd:], dsmlBlockClose)
		if j < 0 {
			// 有开无闭：残缺块，整体留在正文里，不解析。
			rest.WriteString(s[from:])
			break
		}
		closeEnd := openEnd + j + len(dsmlBlockClose)
		rest.WriteString(s[from:i])
		inner := s[openEnd : openEnd+j]
		var blk dsmlBlock
		for _, m := range reDSMLInvoke.FindAllStringSubmatch(inner, -1) {
			blk.names = append(blk.names, m[1])
			blk.bodies = append(blk.bodies, m[2])
		}
		if len(blk.names) > 0 {
			blocks = append(blocks, blk)
		}
		from = closeEnd
	}
	return blocks, strings.TrimSpace(rest.String())
}

// dsmlToToolCalls 把提取的块转成 OpenAI tool_calls 数组。
// 每个块内所有 invoke 摊平成同一个数组；id 与 index 按序生成。
func dsmlToToolCalls(blocks []dsmlBlock) []any {
	var out []any
	seq := 0
	for _, b := range blocks {
		for k, name := range b.names {
			args := parseDSMLParams(b.bodies[k])
			buf, err := json.Marshal(args)
			if err != nil {
				buf = []byte("{}")
			}
			out = append(out, map[string]any{
				"id":    "call_dsml_" + strconv.Itoa(seq),
				"index": seq,
				"type":  "function",
				"function": map[string]any{
					"name":      name,
					"arguments": string(buf),
				},
			})
			seq++
		}
	}
	return out
}

// parseDSMLParams 解析一个 invoke 体内的全部参数。
//
// string="true"  → 值即字符串；
// string="false" → 先尝试 JSON 解析（数字/布尔/对象/数组），失败则原样当字符串。
func parseDSMLParams(body string) map[string]any {
	args := map[string]any{}
	for _, m := range reDSMLParam.FindAllStringSubmatch(body, -1) {
		name, typ, val := m[1], m[2], m[3]
		if typ == "true" {
			args[name] = val
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(val), &v); err == nil {
			args[name] = v
		} else {
			args[name] = val
		}
	}
	return args
}

// parseDSMLResponse 就地改写 OpenAI 兼容聚合响应：
// content 里的 DSML 块 → message.tool_calls，finish_reason → "tool_calls"，
// content 只留块外正文。返回是否发生了改写。
//
// 多 choice 逐个处理（上游实际只给一个，但不假设）。
func parseDSMLResponse(obj map[string]any) bool {
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return false
	}
	changed := false
	for _, c := range choices {
		choice, _ := c.(map[string]any)
		if choice == nil {
			continue
		}
		msg, _ := choice["message"].(map[string]any)
		if msg == nil {
			continue
		}
		// 幂等：streamTap 已转过的帧，Aggregate 这里不能再转（否则 content 被清两次）。
		// Aggregate 内部会把 tap 转好的 tool_calls 帧合并进 message，此时 tool_calls 已在。
		// 注意类型：upstream 内部可能存 []map[string]any 而非 []any，故用长度探测而非断言。
		if hasToolCalls(msg) {
			if choice["finish_reason"] != "tool_calls" {
				choice["finish_reason"] = "tool_calls"
				changed = true
			}
			continue
		}
		content, _ := msg["content"].(string)
		blocks, rest := parseDSMLBlocks(content)
		if len(blocks) == 0 {
			continue
		}
		msg["tool_calls"] = dsmlToToolCalls(blocks)
		msg["content"] = rest
		choice["finish_reason"] = "tool_calls"
		changed = true
	}
	return changed
}

// ---------------------------------------------------------------------------
// 流式 DSML 过滤器（R144 核心）
// ---------------------------------------------------------------------------

// dsmlStreamFilter 流式 DSML 过滤器。
//
// 为什么需要它：Hermes 主对话恒发 stream:true（chat_completion_helpers.py:3117 硬编码），
// 走 Stream 路径；而 upstream.StreamCapture 是边读边转发的透明管道，父层无法在转发前
// 整体改写。DSML 文本在 delta.content 里**跨帧累积**（模型会分多帧吐 "<|DSML|"、
// "tool_calls>"…），所以必须在帧级维护状态：见到开标签后把内容攒起来，
// 闭标签到了才一次性产出 tool_calls 帧。
//
// 状态只有两态：透传（inBlock=false）/ 缓冲（inBlock=true）。
type dsmlStreamFilter struct {
	buf       strings.Builder // 缓冲中的 DSML 原文（自开标签起）
	inBlock   bool            // true = 已见到开标签，正在缓冲
	headProbe string          // 可能是开标签前缀的尾部（跨帧断裂检测）
}

// feed 喂入一段 delta.content：
//
//	text  — 本次可安全下发给客户端的**纯文本**（非空则塞回本帧 delta.content）
//	frame — 本次解析出的**工具调用帧**（nil = 无）。非 nil 时调用方序列化成
//	        额外一行 `data: {...}\n` 追加在本帧之后
func (f *dsmlStreamFilter) feed(content string) (string, map[string]any) {
	if content == "" {
		return "", nil
	}
	if !f.inBlock {
		probe := f.headProbe + content
		loc := reDSMLBlockOpen.FindStringIndex(probe)
		if loc == nil {
			// 未见完整开标签：保留可能是标签前缀的尾巴，其余放行。
			// 前缀集合要覆盖两种终止符（`>` 与 `|`），故取两者中较长的公共前缀长度。
			f.headProbe = tailPrefixAny(probe, []string{dsmlBlockOpen, dsmlBlockOpenAlt})
			return probe[:len(probe)-len(f.headProbe)], nil
		}
		// 见到开标签：标签前的正文先放行，之后进入缓冲。
		before := probe[:loc[0]]
		f.headProbe = ""
		f.inBlock = true
		f.buf.Reset()
		f.buf.WriteString(probe[loc[0]:])
		if strings.Contains(f.buf.String(), dsmlBlockClose) {
			return before, f.takeFrame()
		}
		return before, nil
	}
	f.buf.WriteString(content)
	if strings.Contains(f.buf.String(), dsmlBlockClose) {
		return "", f.takeFrame()
	}
	return "", nil
}

// takeFrame 把缓冲内容转成工具调用帧；解析不出则返回 nil（畸形块由 flush 兜底）。
func (f *dsmlStreamFilter) takeFrame() map[string]any {
	raw := f.buf.String()
	f.buf.Reset()
	f.inBlock = false
	blocks, _ := parseDSMLBlocks(raw)
	if len(blocks) == 0 {
		return nil
	}
	return map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"tool_calls": dsmlToToolCalls(blocks)},
			"finish_reason": "tool_calls",
		}},
		"object": "chat.completion.chunk",
	}
}

// flush 流结束时吐出残余（截断的 DSML 原样保留，绝不伪造 tool_calls）。
func (f *dsmlStreamFilter) flush() string {
	var out strings.Builder
	out.WriteString(f.headProbe)
	f.headProbe = ""
	if f.buf.Len() > 0 {
		out.WriteString(f.buf.String())
		f.buf.Reset()
	}
	f.inBlock = false
	return out.String()
}

// tailPrefix 返回 s 的后缀中，等于 needle 最长真前缀的那一段。
// 用于跨帧断裂检测：可能是标签开头的尾巴要留到下次拼接再判断。
func tailPrefix(s, needle string) string {
	max := len(needle) - 1
	if len(s) < max {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(s, needle[:n]) {
			return s[len(s)-n:]
		}
	}
	return ""
}

// tailPrefixAny 是 tailPrefix 的多候选版：返回最长的、能作为任一 needle 真前缀的后缀。
// 上游开标签有两种终止符（`>` / `|`），缓冲前缀必须按最长候选保留，
// 否则 `...tool_calls|` 里的 `|` 会被提前放行、下一次拼接就认不出了。
func tailPrefixAny(s string, needles []string) string {
	best := ""
	for _, n := range needles {
		if t := tailPrefix(s, n); len(t) > len(best) {
			best = t
		}
	}
	return best
}

// deriveModelName 去掉 "渠道/" 前缀，返回真实模型名（isDSMLModel 只认裸名）。
func deriveModelName(model string) string {
	if i := strings.LastIndex(model, "/"); i >= 0 {
		return model[i+1:]
	}
	return model
}

// hasToolCalls 报告 message 里是否已有非空 tool_calls。
// 用反射式的长度探测而非单一类型断言：upstream.Aggregate 内部可能保存
// []map[string]any，而我们自己写入的是 []any，两种形态都要认。
func hasToolCalls(msg map[string]any) bool {
	raw, ok := msg["tool_calls"]
	if !ok || raw == nil {
		return false
	}
	if tcs, ok := raw.([]any); ok {
		return len(tcs) > 0
	}
	// 其它切片形态（如 []map[string]any）：序列化后看是否非空数组。
	b, err := json.Marshal(raw)
	if err != nil {
		return false
	}
	s := strings.TrimSpace(string(b))
	return s != "" && s != "[]" && s != "null"
}
