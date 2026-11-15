package doubao

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// ---------- 注入 ----------

func TestToolsPromptShape(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string","description":"城市名"},"unit":{"type":"string","enum":["c","f"]}},"required":["city"]}`)
	tools := []toolDefinition{{Name: "get_weather", Description: "查询天气\n第二行忽略", Parameters: schema}}
	p := toolsPrompt(tools)
	for _, want := range []string{
		"禁止使用内置联网搜索",
		"- get_weather{city!: string, unit?: \"c\"|\"f\"}: 查询天气",
		"<tool_call>",
		`{"name": "工具名", "arguments": {"参数名": "参数值"}}`,
	} {
		if !strings.Contains(p, want) {
			t.Fatalf("prompt 缺少 %q：\n%s", want, p)
		}
	}
	// 确定性：同一定义两次生成同一签名（map 遍历序稳定化）
	if p2 := toolsPrompt(tools); p != p2 {
		t.Fatal("toolsPrompt 输出不确定（map 序未稳定化）")
	}
}

func TestParseToolsOpenAIShape(t *testing.T) {
	body := []byte(`{"model":"doubao/x","tools":[
		{"type":"function","function":{"name":"get_weather","description":"w","parameters":{"type":"object"}}},
		{"type":"custom","custom":{"name":"skip_me"}}
	],"messages":[{"role":"user","content":"hi"}]}`)
	tools := parseTools(body)
	if len(tools) != 1 || tools[0].Name != "get_weather" {
		t.Fatalf("应只接 function 类型：%+v", tools)
	}
	// 无 tools
	if tools := parseTools([]byte(`{"messages":[]}`)); tools != nil {
		t.Fatalf("无 tools 应返回 nil：%+v", tools)
	}
}

// ---------- 解析：四级容错 ----------

func TestParseToolCallsFullTag(t *testing.T) {
	calls, rest := parseToolCalls("好的，我来查。\n<tool_call>\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"北京\"}}\n</tool_call>")
	if len(calls) != 1 || calls[0].Name != "get_weather" || calls[0].Arguments != `{"city": "北京"}` {
		t.Fatalf("解析错误：%+v", calls)
	}
	if rest != "好的，我来查。" {
		t.Fatalf("剩余正文错误：%q", rest)
	}
}

func TestParseToolCallsHeadlessTag(t *testing.T) {
	// 缺开标签（思考模式下常见）
	calls, _ := parseToolCalls(`{"name": "get_weather", "arguments": {"city": "上海"}}</tool_call>`)
	if len(calls) != 1 || calls[0].Name != "get_weather" {
		t.Fatalf("缺头标签解析失败：%+v", calls)
	}
}

func TestParseToolCallsBareJSON(t *testing.T) {
	calls, rest := parseToolCalls(`{"name": "get_weather", "arguments": {"city": "广州"}}`)
	if len(calls) != 1 || rest != "" {
		t.Fatalf("裸 JSON 解析失败：%+v rest=%q", calls, rest)
	}
}

func TestParseToolCallsLegacy(t *testing.T) {
	text := `<tool_calls><invoke name="get_weather"><parameter name="city">深圳</parameter></invoke></tool_calls>`
	calls, _ := parseToolCalls(text)
	if len(calls) != 1 || calls[0].Name != "get_weather" {
		t.Fatalf("旧格式解析失败：%+v", calls)
	}
	var args map[string]string
	if json.Unmarshal([]byte(calls[0].Arguments), &args) != nil || args["city"] != "深圳" {
		t.Fatalf("旧格式参数错误：%s", calls[0].Arguments)
	}
}

func TestParseToolCallsNoFalsePositive(t *testing.T) {
	// 普通回答含 JSON 代码块不得误判
	text := `这是示例代码：` + "\n```json\n{\"key\": \"value\"}\n```"
	calls, rest := parseToolCalls(text)
	if len(calls) != 0 || rest != text {
		t.Fatalf("普通 JSON 不应被判成工具调用：calls=%v", calls)
	}
}

func TestParseToolCallsUnquotedKeys(t *testing.T) {
	// 无引号键修复
	calls, _ := parseToolCalls(`<tool_call>
{ name: "get_weather", arguments: {city: "杭州"}}
</tool_call>`)
	if len(calls) != 1 || calls[0].Name != "get_weather" {
		t.Fatalf("无引号键修复失败：%+v", calls)
	}
}

// ---------- 多轮回传格式化 ----------

func TestFormatToolResult(t *testing.T) {
	s := formatToolResult("get_weather", `{"temp": 25}`)
	if !strings.Contains(s, "[工具调用结果]") || !strings.Contains(s, "get_weather 返回：") {
		t.Fatalf("格式错误：%s", s)
	}
	// 超长截断保头尾
	long := strings.Repeat("x", 30000)
	tr := formatToolResult("t", long)
	if len(tr) > 13000 || !strings.Contains(tr, "中间内容已截断") {
		t.Fatalf("截断异常：%d", len(tr))
	}
}

func TestReconstructToolCall(t *testing.T) {
	s := reconstructToolCall("get_weather", `{"city":"北京"}`)
	if !strings.HasPrefix(s, "<tool_call>") || !strings.Contains(s, `"name": "get_weather"`) {
		t.Fatalf("还原错误：%s", s)
	}
}

// ---------- 压平轮注入 ----------

func TestBuildChatBodyInjectsToolsOnFirstTurn(t *testing.T) {
	tools := []toolDefinition{{Name: "get_weather", Description: "查天气"}}
	req := &chatRequest{Model: "doubao/deepseek-v4-flash", hasTools: true, Messages: []chatMessage{
		{Role: "user", Content: rawStr("北京天气怎么样")},
	}}
	out, err := buildChatBody(req, staticModels["deepseek-v4-flash"].ItemKey, "", diagDev, "", "", sessionState{}, tools)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "禁止使用内置联网搜索") || !strings.Contains(got, "get_weather") {
		t.Fatalf("首轮未注入工具清单：\n%s", got)
	}
	// 复用轮：不重复注入（服务端会话已记住）
	out2, _ := buildChatBody(req, staticModels["deepseek-v4-flash"].ItemKey, "", diagDev, "", "", sessionState{ConversationID: "c1"}, tools)
	if strings.Contains(string(out2), "禁止使用内置联网搜索") {
		t.Fatalf("复用轮不应重复注入：\n%s", out2)
	}
}

func TestBuildChatBodyFormatsToolHistory(t *testing.T) {
	req := &chatRequest{Model: "doubao/deepseek-v4-flash", Messages: []chatMessage{
		{Role: "user", Content: rawStr("北京天气")},
		{Role: "assistant", Content: json.RawMessage("null"), ToolCalls: func() []chatToolCall {
			var tc chatToolCall
			tc.ID, tc.Type = "call_1", "function"
			tc.Function.Name, tc.Function.Arguments = "get_weather", `{"city":"北京"}`
			return []chatToolCall{tc}
		}()},
		{Role: "tool", ToolCallID: "call_1", Content: rawStr(`{"temp":25}`)},
		{Role: "user", Content: rawStr("谢谢，明天呢")},
	}}
	out, err := buildChatBody(req, staticModels["deepseek-v4-flash"].ItemKey, "", diagDev, "", "", sessionState{}, nil)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "get_weather") || !strings.Contains(got, "[工具调用结果]") {
		t.Fatalf("tool 历史未格式化保留：\n%s", got)
	}
	if !strings.Contains(got, "get_weather 返回：") {
		t.Fatalf("tool_call_id 未关联出工具名：\n%s", got)
	}
}

// ---------- 流式门控 ----------

func toolGateSrc(text string) string {
	var b strings.Builder
	// 把文本切成多个 CHUNK_DELTA（每 5 字符一帧，模拟分片）。
	// 转义顺序：反斜杠 → 控制字符（\n/\t/\r，JSON 字符串里必须转义，否则整帧
	// 被 json 解析丢弃——上一版漏了这条，含换行的帧恰好覆盖两个闭合标签）→ 引号。
	runes := []rune(text)
	for i := 0; i < len(runes); i += 5 {
		end := i + 5
		if end > len(runes) {
			end = len(runes)
		}
		chunk := string(runes[i:end])
		chunk = strings.ReplaceAll(chunk, "\\", "\\\\")
		chunk = strings.ReplaceAll(chunk, "\n", "\\n")
		chunk = strings.ReplaceAll(chunk, "\t", "\\t")
		chunk = strings.ReplaceAll(chunk, "\r", "\\r")
		chunk = strings.ReplaceAll(chunk, "\"", "\\\"")
		b.WriteString("event: CHUNK_DELTA\ndata: {\"text\":\"" + chunk + "\"}\n\n")
	}
	b.WriteString("event: SSE_REPLY_END\ndata: {\"end_type\":1}\n\n")
	return b.String()
}

func collectStream(t *testing.T, src string, hasTools bool) string {
	t.Helper()
	r := newDoubaoStreamWithSessions(strings.NewReader(src), "doubao/x", "", nil, hasTools)
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读流: %v", err)
	}
	return string(out)
}

func TestStreamGateNormalAnswerPassthrough(t *testing.T) {
	// 普通回答：不被门控吞掉。护栏会切帧，拼接全部 content delta 后校验完整性
	//（把输出里的 content 字段值抽出来拼回原文）。
	src := toolGateSrc("你好，今天天气不错，适合出门。")
	got := collectStream(t, src, true)
	var full strings.Builder
	for _, line := range strings.Split(got, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, "chat.completion.chunk") {
			continue
		}
		var f struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &f) == nil && len(f.Choices) > 0 {
			full.WriteString(f.Choices[0].Delta.Content)
		}
	}
	if full.String() != "你好，今天天气不错，适合出门。" {
		t.Fatalf("普通回答不完整：got=%q", full.String())
	}
	if strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Fatal("普通回答不应产出 tool_calls 收尾")
	}
}

func TestStreamGateToolCallConverted(t *testing.T) {
	src := toolGateSrc("<tool_call>\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"北京\"}}\n</tool_call>")
	got := collectStream(t, src, true)
	for _, want := range []string{
		`"tool_calls"`,
		`"name":"get_weather"`,
		`"finish_reason":"tool_calls"`,
		`"id":"call_`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("流缺少 %q：\n%s", want, got)
		}
	}
	// 标签原文不得泄漏进正文
	if strings.Contains(got, "<tool_call>") {
		t.Fatalf("标签原文泄漏：\n%s", got)
	}
	// stop 收尾不得与 tool_calls 收尾并存
	if strings.Contains(got, `"finish_reason":"stop"`) {
		t.Fatalf("stop 与 tool_calls 收尾并存：\n%s", got)
	}
}

func TestStreamGateParseFailureDegrades(t *testing.T) {
	// 标签齐全但 JSON 坏 → 降级为正文
	src := toolGateSrc("<tool_call>\n{not valid json}\n</tool_call>")
	got := collectStream(t, src, true)
	if !strings.Contains(got, "not valid json") {
		t.Fatalf("解析失败应降级为正文输出：\n%s", got)
	}
	if strings.Contains(got, `"finish_reason":"tool_calls"`) {
		t.Fatal("降级路径不应产出 tool_calls 收尾")
	}
}

func TestStreamNoToolsNoGate(t *testing.T) {
	// hasTools=false：含标签的正文原样透传（不做门控）。正文分片发送，
	// 断言按分片片段检查而不是整串（标签会被 5 字符分片切断）。
	src := toolGateSrc("回答 <tool_call> 原文保持")
	got := collectStream(t, src, false)
	for _, part := range []string{"回答", `\u003ct`, `ool_c`, "原文保持"} {
		if !strings.Contains(got, part) {
			t.Fatalf("无 tools 时应原样透传，缺 %q：\n%s", part, got)
		}
	}
}

func TestStreamGateArgumentsChunked(t *testing.T) {
	// 多帧分片拼接后 arguments 完整
	src := toolGateSrc(`<tool_call>{"name":"search","arguments":{"q":"deepseek news","mode":"full"}}</tool_call>`)
	got := collectStream(t, src, true)
	if !strings.Contains(got, `"q":"deepseek news"`) && !strings.Contains(got, `\"q\":\"deepseek news\"`) {
		t.Fatalf("arguments 未完整下发：\n%s", got)
	}
}

// TestStreamGateTagAfterProse 真机形态（2026-11-15 实证）：模型先说一句话再输出标签，
// 门控必须在普通放行模式下拦住中途出现的标签（修复「I'll look up... <tool_call>」泄漏）。
func TestStreamGateTagAfterProse(t *testing.T) {
	text := "我来查一下天气。\n<tool_call>\n{\"name\": \"get_weather\", \"arguments\": {\"city\": \"东莞\"}}\n</tool_call>\n再看看别的。"
	g := newToolGate()
	var emitted strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); i += 5 {
		end := i + 5
		if end > len(runes) {
			end = len(runes)
		}
		emitted.WriteString(g.feed(string(runes[i:end])))
	}
	calls, rest := g.flush()
	out := emitted.String() + rest
	// 标签绝不能出现在下发正文里
	if strings.Contains(out, "<tool_call>") || strings.Contains(out, "<tool") {
		t.Fatalf("标签泄漏进正文：\n%s", out)
	}
	if len(calls) != 1 || calls[0].Name != "get_weather" {
		t.Fatalf("调用未解析出：calls=%v out=%s", calls, out)
	}
	// 前后普通文本不丢
	if !strings.Contains(out, "我来查一下天气。") || !strings.Contains(out, "再看看别的。") {
		t.Fatalf("普通正文被误吞：\n%s", out)
	}
}

// TestGateGuardNoFalsePositive 护栏不误吞普通长回答的尾部（flush 把护栏里的尾巴放出来）。
func TestGateGuardNoFalsePositive(t *testing.T) {
	text := "普通回答结尾，不含任何标签，最后这几个字也要完整送达。"
	g := newToolGate()
	var emitted strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); i += 7 {
		end := i + 7
		if end > len(runes) {
			end = len(runes)
		}
		emitted.WriteString(g.feed(string(runes[i:end])))
	}
	_, rest := g.flush()
	out := emitted.String() + rest
	if out != text {
		t.Fatalf("普通回答尾部不完整：\nwant %q\ngot  %q", text, out)
	}
}

// TestIndexTagPrefixCrossFrame 标签跨帧断裂也必须能拦住。
func TestIndexTagPrefixCrossFrame(t *testing.T) {
	// "回答 <tool_ca"："回答"=6B + 空格1B → "<tool_ca" 从字节 7 起
	if idx := indexTagPrefix("回答 <tool_ca", 0); idx != 7 {
		t.Fatalf("断裂前缀应命中 7，实际 %d", idx)
	}
	if idx := indexTagPrefix("纯文本没有标签", 0); idx != -1 {
		t.Fatalf("无标签应返回 -1，实际 %d", idx)
	}
	// 已放行区域之后的新出现
	if idx := indexTagPrefix("abcdef<tool_call>x", 4); idx != 6 {
		t.Fatalf("应从 emitted 之后找，实际 %d", idx)
	}
}
