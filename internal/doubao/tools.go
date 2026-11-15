// tools.go 工具调用（文本协议路线）。
//
// 上游事实（temp/doubao-tools-research.md 真机取证）：/chat/completion 工作模式
// 不支持 OpenAI tools——顶层注入被静默忽略。但有三个开源项目用「提示词注入 +
// 约定标签 + 网关解析」跑通了 agent 工具循环（temp/doubao-tools-delete-research2.md
// Part A），本文件照搬其中工程化最完整的 SeiShonagon520/doubao2api 语义：
//
//   1. 请求面：OpenAI tools → TS 压缩签名文本 + 调用格式指令，注入 system prompt
//      （复用首轮压平通道；复用轮由豆包服务端会话记忆承载，不重复注入）；
//   2. 响应面：四级容错解析 <tool_call>{json}</tool_call> → 标准 OpenAI tool_calls；
//   3. 多轮回传：role:"tool" → [工具调用结果] 文本段、assistant.tool_calls → 还原标签。
//
// 纪律：解析失败一律降级为普通正文输出（绝不丢正文）；上游未声明该能力，
// /v1/models 不得宣告 supports_tools（R49）——文本协议是网关侧的兼容层，
// 不是上游能力，宣告会让无法处理该协议的客户端误判。
package doubao

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// toolDefinition OpenAI 风格工具定义（请求面入参的最小切片）。
type toolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema
}

// parseTools 从客户端请求体提取 tools 数组（OpenAI 形状）。
func parseTools(rawBody []byte) []toolDefinition {
	var req struct {
		Tools []struct {
			Type     string `json:"type"`
			Function *struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(rawBody, &req) != nil {
		return nil
	}
	var out []toolDefinition
	for _, t := range req.Tools {
		// 只接 function 类型；custom/mcp 等其它类型静默跳过（不编造兼容）。
		if t.Type == "" || t.Type == "function" {
			if t.Function != nil && t.Function.Name != "" {
				out = append(out, toolDefinition{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					Parameters:  t.Function.Parameters,
				})
			}
		}
	}
	return out
}

// ---------- 注入：TS 压缩签名（SeiShonagon520 同款，省 ~90% token） ----------

// toolsPrompt 把工具定义压成 TS 风格签名 + 调用格式指令。
//
// 形如 `- get_weather{city!: string}: 查询天气`（!=required、?=optional、
// enum 内联、短 description 内联）。JSON Schema 的 properties/required 逐项展开。
func toolsPrompt(tools []toolDefinition) string {
	var b strings.Builder
	b.WriteString("你是一个工具调用助手。禁止使用内置联网搜索。\n\n## 可用工具\n")
	for _, t := range tools {
		b.WriteString("- " + t.Name)
		b.WriteString(sigOf(t))
		if d := strings.TrimSpace(t.Description); d != "" {
			b.WriteString(": " + firstLine(d))
		}
		b.WriteString("\n")
	}
	b.WriteString(`
## 调用格式
<tool_call>
{"name": "工具名", "arguments": {"参数名": "参数值"}}
</tool_call>

多个工具可并行调用（输出多个<tool_call>块）。

## 规则
1. 必须使用上面列出的**精确工具名**，不要用别名或猜测的名字
2. 参数必须严格匹配工具签名中的字段名（!为必填，?为可选）
3. 需要调用工具时只输出<tool_call>块，不加解释文字
4. 不需要工具时直接用自然语言回答
`)
	return b.String()
}

// sigOf 从 JSON Schema 生成 TS 风格参数签名。
func sigOf(t toolDefinition) string {
	if len(t.Parameters) == 0 {
		return "()"
	}
	var schema struct {
		Properties map[string]struct {
			Type        string   `json:"type"`
			Description string   `json:"description"`
			Enum        []string `json:"enum"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if json.Unmarshal(t.Parameters, &schema) != nil || len(schema.Properties) == 0 {
		return "()"
	}
	reqSet := map[string]bool{}
	for _, r := range schema.Required {
		reqSet[r] = true
	}
	var parts []string
	for name, p := range schema.Properties {
		mark := "?"
		if reqSet[name] {
			mark = "!"
		}
		typ := p.Type
		if typ == "" {
			typ = "any"
		}
		if len(p.Enum) > 0 {
			// enum 内联成联合类型（覆盖原类型声明）
			var es []string
			for _, e := range p.Enum {
				es = append(es, fmt.Sprintf("%q", e))
			}
			typ = strings.Join(es, "|")
		}
		parts = append(parts, name+mark+": "+typ)
	}
	// map 遍历序不稳定：排序保证同一定义每次生成同一签名（对上游缓存亲和）
	for i := 1; i < len(parts); i++ {
		for j := i; j > 0 && parts[j] < parts[j-1]; j-- {
			parts[j], parts[j-1] = parts[j-1], parts[j]
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:117] + "…"
	}
	return s
}

// ---------- 响应面：四级容错解析（SeiShonagon520 tool_calling.py 同款语义） ----------

// parsedToolCall 一个解析出的工具调用。
type parsedToolCall struct {
	Name      string
	Arguments string // JSON 字符串（与 OpenAI arguments 字段同形）
}

// 正则按优先级排列，命中即返回。
var (
	// ① 完整标签：<tool_call>{json}</tool_call>
	reToolCallFull = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)
	// ② 缺头标签（思考模式下常见）：{json}</tool_call>
	reToolCallHeadless = regexp.MustCompile(`(?s)\{[^<]*?"name"\s*:\s*"[^"]+".*?\}\s*</tool_call>`)
	// ④ 旧格式：<tool_calls><invoke name="x"><parameter name="p">v</parameter>…</tool_calls>
	reToolCallsLegacy = regexp.MustCompile(`(?s)<tool_calls>\s*<invoke\s+name="([^"]+)">(.*?)</invoke>\s*</tool_calls>`)
	reLegacyParam     = regexp.MustCompile(`<parameter\s+name="([^"]+)">([\s\S]*?)</parameter>`)
)

// parseToolCalls 从模型全文解析工具调用（四级容错 + 无引号键修复）。
// 返回 (调用列表, 剩余正文)。剩余正文 = 剥掉调用块后的文本（trim 后）。
func parseToolCalls(text string) ([]parsedToolCall, string) {
	var calls []parsedToolCall

	// ① 完整标签（可多个）
	for _, m := range reToolCallFull.FindAllStringSubmatch(text, -1) {
		if c, ok := parseCallJSON(m[1]); ok {
			calls = append(calls, c)
		}
	}
	if len(calls) > 0 {
		rest := reToolCallFull.ReplaceAllString(text, "")
		return calls, strings.TrimSpace(rest)
	}

	// ② 缺头标签
	for _, m := range reToolCallHeadless.FindAllStringSubmatch(text, -1) {
		if c, ok := parseCallJSON(m[0][:len(m[0])-len("</tool_call>")]); ok {
			calls = append(calls, c)
		}
	}
	if len(calls) > 0 {
		rest := reToolCallHeadless.ReplaceAllString(text, "")
		return calls, strings.TrimSpace(rest)
	}

	// ③ 完全裸 JSON：全文（trim 后）必须整体是 {"name":...,"arguments":...}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") && strings.Contains(trimmed[:minStr(100, len(trimmed))], `"name"`) {
		if c, ok := parseCallJSON(trimmed); ok {
			return []parsedToolCall{c}, ""
		}
	}

	// ④ 旧格式
	for _, m := range reToolCallsLegacy.FindAllStringSubmatch(text, -1) {
		args := map[string]string{}
		for _, p := range reLegacyParam.FindAllStringSubmatch(m[2], -1) {
			args[p[1]] = p[2]
		}
		if len(args) > 0 {
			ab, _ := json.Marshal(args)
			calls = append(calls, parsedToolCall{Name: m[1], Arguments: string(ab)})
		}
	}
	if len(calls) > 0 {
		rest := reToolCallsLegacy.ReplaceAllString(text, "")
		return calls, strings.TrimSpace(rest)
	}
	return nil, text
}

// parseCallJSON 解析调用 JSON，含无引号键修复（两轮 re.sub 语义）。
func parseCallJSON(s string) (parsedToolCall, bool) {
	var c parsedToolCall
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		// 无引号键修复：{name: "x", arguments: {...}} → 补引号后重试
		fixed := fixUnquotedJSON(s)
		if fixed == s {
			return c, false
		}
		if err := json.Unmarshal([]byte(fixed), &raw); err != nil {
			return c, false
		}
	}
	if raw.Name == "" {
		return c, false
	}
	args := normalizeArguments(raw.Arguments)
	c = parsedToolCall{Name: raw.Name, Arguments: args}
	return c, true
}

// normalizeArguments arguments 兼容对象/字符串/缺失三种形态，统一成 JSON 字符串。
func normalizeArguments(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "{}"
	}
	if s[0] == '{' {
		return s
	}
	// 已是字符串：可能是双重编码的 JSON
	var inner string
	if json.Unmarshal(raw, &inner) == nil {
		inner = strings.TrimSpace(inner)
		if strings.HasPrefix(inner, "{") {
			return inner
		}
	}
	return "{}"
}

// fixUnquotedJSON 补无引号的键（简化版修复：^{\s*key: → {"key":）。
func fixUnquotedJSON(s string) string {
	re := regexp.MustCompile(`([{,]\s*)([A-Za-z_][A-Za-z0-9_]*)(\s*):`)
	return re.ReplaceAllString(s, `$1"$2"$3:`)
}

func minStr(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- 多轮回传格式化（SeiShonagon520 convert_messages_with_tools 同款） ----------

// formatToolResult role:"tool" 消息 → 回传文本段。
func formatToolResult(name, content string) string {
	if name == "" {
		name = "tool"
	}
	content = truncateToolResult(content, 12000)
	return "[工具调用结果]\n" + name + " 返回：" + content
}

// truncateToolResult 单结果截断（保头尾各半，SeiShonagon520 同款）。
func truncateToolResult(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	half := limit / 2
	return s[:half] + "\n[... 中间内容已截断 ...]\n" + s[len(s)-half:]
}

// reconstructToolCall assistant.tool_calls（OpenAI 形状）→ 还原 <tool_call> 标签文本。
func reconstructToolCall(name, arguments string) string {
	// arguments 是 JSON 字符串；还原成对象形态让模型看得懂
	inner := strings.TrimSpace(arguments)
	if inner == "" {
		inner = "{}"
	}
	return "<tool_call>\n{\"name\": \"" + name + "\", \"arguments\": " + inner + "}\n</tool_call>"
}

// newCallID 生成 OpenAI 形状的 tool_call id。
func newCallID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "call_" + hex.EncodeToString(b)
}

// ---------- 流式门控（SeiShonagon520 StreamingGuard 同款语义） ----------
//
// 状态机：普通放行 / 缓冲观察 / 工具收集三态。
//   - warmup 阶段（前 warmupChars 字符）缓冲，判断是不是标签开头——普通回答
//     以「我来/好的/今天」等自然语言开头，放行；以「<tool_call>」或裸 JSON 开头则进工具收集；
//   - 工具收集中遇到 </tool_call> 即解析产出；解析失败降级为普通正文（绝不丢正文）。

const (
	warmupChars  = 20
	gateMaxChars = 64 << 10 // 门控缓冲上限，超过强制放行（防失控）
	// guardWindow 普通放行模式的尾部护栏宽度（≥最长标签长度）。
	guardWindow = 12
	tagOpen  = "<tool_call>"
	tagClose = "</tool_call>"
)

type toolGate struct {
	buf      strings.Builder
	toolMode bool
	done     bool // 普通放行模式：仍留护栏（标签可能中途出现）
	emitted  int  // 普通模式下已放行的字符数（护栏基准）
}

func newToolGate() *toolGate { return &toolGate{} }

// feed 喂一段正文，返回应下发的 content 文本（可能为空）。
// 结尾是否出现了完整调用由 flush 判定。
//
// 普通放行模式必须留尾部护栏：真实模型会先说一句话再输出标签（真机实证
// 2026-11-15："I'll look up...\n<tool_call> …"），一次性放完会让中途出现的
// 标签漏成正文。放行时末尾留一截不吐，等下一帧确认是普通文本才放行；
// 拼出标签（或其前缀，防跨帧断裂）立即切回收集模式。
func (g *toolGate) feed(text string) string {
	if text == "" {
		return ""
	}
	if g.done {
		g.buf.WriteString(text)
		all := g.buf.String()
		// 半路杀出标签（含不完整前缀）：切收集模式，标签起的前文放行
		if idx := indexTagPrefix(all, g.emitted); idx >= 0 {
			g.buf.Reset()
			g.buf.WriteString(all[idx:])
			g.toolMode = true
			g.done = false
			return all[:idx]
		}
		safe := len(all) - guardWindow
		if safe <= g.emitted {
			return "" // 全在护栏内
		}
		out := all[g.emitted:safe]
		g.emitted = safe
		return out
	}
	g.buf.WriteString(text)
	all := g.buf.String()

	// warmup：还不够判断
	if !g.toolMode && len(all) < warmupChars {
		return ""
	}
	if !g.toolMode {
		// 判断开头：<tool_call> 或裸 JSON {"name"...
		t := strings.TrimSpace(all)
		if strings.HasPrefix(t, tagOpen) || (strings.HasPrefix(t, "{") && strings.Contains(t[:minStr(warmupChars, len(t))], `"name"`)) {
			g.toolMode = true
			return "" // 继续缓冲，等闭合
		}
		// 普通回答：转普通放行模式（留护栏，标签可能在中途出现）
		g.done = true
		g.emitted = 0
		safe := len(all) - guardWindow
		if safe <= 0 {
			return ""
		}
		g.emitted = safe
		return all[:safe]
	}
	// 工具收集中：等 </tool_call>（flush 解析）
	if strings.Contains(all, tagClose) {
		return ""
	}
	if len(all) > gateMaxChars {
		// 失控保护：当普通正文放行
		g.done = true
		g.emitted = 0
		return all
	}
	return ""
}

// indexTagPrefix 在 all[searchFrom:] 找标签或其**尾部不完整前缀**的最早出现位置
//（标签跨帧断裂时也要拦，宁枉勿纵）。没有返回 -1。
func indexTagPrefix(all string, searchFrom int) int {
	patterns := []string{"<tool_call>", "</tool_call>", "<tool_call", "</tool_call", "<tool", "</tool"}
	best := -1
	for _, p := range patterns {
		if i := strings.Index(all[searchFrom:], p); i >= 0 {
			idx := searchFrom + i
			if best < 0 || idx < best {
				best = idx
			}
		}
		// 尾部不完整前缀（如缓冲以 "<tool_ca" 结尾）
		tailStart := len(all) - len(p) + 1
		if tailStart < searchFrom {
			tailStart = searchFrom
		}
		for i := tailStart; i < len(all); i++ {
			if strings.HasPrefix(p, all[i:]) {
				if best < 0 || i < best {
					best = i
				}
				break
			}
		}
	}
	return best
}

// flush 流结束时调用：解析缓冲，产出 (toolCalls, 剩余正文)。
// 解析失败 → 全部当普通正文（降级，绝不丢）。普通模式已 emit 的部分不重复返回。
func (g *toolGate) flush() ([]parsedToolCall, string) {
	all := g.buf.String()
	g.buf.Reset()
	if g.done {
		// 普通模式：放行护栏里的尾巴（已 emit 的不再重复）
		tail := ""
		if len(all) > g.emitted {
			tail = all[g.emitted:]
		}
		return nil, tail
	}
	all = strings.TrimSpace(all)
	if all == "" {
		return nil, ""
	}
	calls, rest := parseToolCalls(all)
	if len(calls) == 0 {
		return nil, all // 降级为普通正文
	}
	return calls, rest
}
