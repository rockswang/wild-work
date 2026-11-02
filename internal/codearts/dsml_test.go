package codearts

// DSML 响应解析测试（R144）。
//
// 背景：body.go 的 applyDSML 对 ^deepseek-v4-(flash|pro)$ 走"剥离 tools + schema 注入 system"
// 分支，模型输出 <|DSML|tool_calls> 文本语法（实测确认是模型固有行为，与提示无关）。
// 此前只注入不解析，工具调用完全失效。本文件锁住解析行为。

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseDSMLSingleInvoke 最小形态：一个 invoke、一个字符串参数。
func TestParseDSMLSingleInvoke(t *testing.T) {
	s := `<|DSML|tool_calls><|DSML|invoke name="get_weather"><|DSML|parameter name="city" string="true">北京</|DSML|parameter></|DSML|invoke></|DSML|tool_calls>`

	blocks, rest := parseDSMLBlocks(s)
	if len(blocks) != 1 {
		t.Fatalf("应提取 1 个块，得 %d", len(blocks))
	}
	if rest != "" {
		t.Errorf("块外正文应为空，得 %q", rest)
	}

	calls := dsmlToToolCalls(blocks)
	if len(calls) != 1 {
		t.Fatalf("应得 1 个 tool_call，得 %d", len(calls))
	}
	call := calls[0].(map[string]any)
	fn := call["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("name = %v", fn["name"])
	}
	if call["type"] != "function" {
		t.Errorf("type = %v", call["type"])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments 不是合法 JSON: %v (%v)", err, fn["arguments"])
	}
	if args["city"] != "北京" {
		t.Errorf("city = %v", args["city"])
	}
}

// TestParseDSMLMultiInvokeAndTypes 一个块内多 invoke + 每 invoke 多参数 + 字符串/数字/布尔混用。
func TestParseDSMLMultiInvokeAndTypes(t *testing.T) {
	s := `<|DSML|tool_calls>` +
		`<|DSML|invoke name="read_file">` +
		`<|DSML|parameter name="path" string="true">/tmp/a.txt</|DSML|parameter>` +
		`<|DSML|parameter name="limit" string="false">100</|DSML|parameter>` +
		`</|DSML|invoke>` +
		`<|DSML|invoke name="set_flag">` +
		`<|DSML|parameter name="enabled" string="false">true</|DSML|parameter>` +
		`</|DSML|invoke>` +
		`</|DSML|tool_calls>`

	blocks, rest := parseDSMLBlocks(s)
	if len(blocks) != 1 || rest != "" {
		t.Fatalf("blocks=%d rest=%q", len(blocks), rest)
	}
	calls := dsmlToToolCalls(blocks)
	if len(calls) != 2 {
		t.Fatalf("应得 2 个 tool_call，得 %d", len(calls))
	}

	first := calls[0].(map[string]any)["function"].(map[string]any)
	if first["name"] != "read_file" {
		t.Errorf("第 1 个 name = %v", first["name"])
	}
	var a1 map[string]any
	if err := json.Unmarshal([]byte(first["arguments"].(string)), &a1); err != nil {
		t.Fatalf("第 1 个 arguments 非法: %v", err)
	}
	if a1["path"] != "/tmp/a.txt" {
		t.Errorf("path = %v", a1["path"])
	}
	if a1["limit"] != float64(100) {
		t.Errorf("limit 应是数字 100，得 %T(%v)", a1["limit"], a1["limit"])
	}

	second := calls[1].(map[string]any)["function"].(map[string]any)
	var a2 map[string]any
	if err := json.Unmarshal([]byte(second["arguments"].(string)), &a2); err != nil {
		t.Fatalf("第 2 个 arguments 非法: %v", err)
	}
	if a2["enabled"] != true {
		t.Errorf("enabled 应是布尔 true，得 %T(%v)", a2["enabled"], a2["enabled"])
	}
}

// TestParseDSMLKeepsSurroundingText 块外正文必须保留（模型常先说话再调工具）。
func TestParseDSMLKeepsSurroundingText(t *testing.T) {
	s := `我先查一下天气。` +
		`<|DSML|tool_calls><|DSML|invoke name="get_weather"><|DSML|parameter name="city" string="true">北京</|DSML|parameter></|DSML|invoke></|DSML|tool_calls>` +
		` 稍等。`
	blocks, rest := parseDSMLBlocks(s)
	if len(blocks) != 1 {
		t.Fatalf("blocks=%d", len(blocks))
	}
	if !strings.Contains(rest, "我先查一下天气。") || !strings.Contains(rest, "稍等。") {
		t.Errorf("块外正文丢失: %q", rest)
	}
	if strings.Contains(rest, "DSML") {
		t.Errorf("DSML 残留在正文里: %q", rest)
	}
}

// TestParseDSMLIncompleteBlockNotFaked 残缺块（有开无闭）不得伪造 tool_calls。
func TestParseDSMLIncompleteBlockNotFaked(t *testing.T) {
	s := `<|DSML|tool_calls><|DSML|invoke name="get_weather"><|DSML|parameter name="city"`
	blocks, rest := parseDSMLBlocks(s)
	if len(blocks) != 0 {
		t.Fatalf("残缺块不该被解析，得 %d 块", len(blocks))
	}
	if rest == "" {
		t.Error("残缺块应原样留在正文里")
	}
}

// TestParseDSMLResponseRewritesFinishReason 聚合响应里的 DSML 必须转成标准 tool_calls，
// 且 finish_reason 从 stop 改成 tool_calls（否则客户端仍不执行工具）。
func TestParseDSMLResponseRewritesFinishReason(t *testing.T) {
	obj := map[string]any{
		"id":    "x",
		"model": "deepseek-v4-flash",
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": `<|DSML|tool_calls><|DSML|invoke name="get_weather"><|DSML|parameter name="city" string="true">北京</|DSML|parameter></|DSML|invoke></|DSML|tool_calls>`,
			},
			"finish_reason": "stop",
		}},
	}

	if !parseDSMLResponse(obj) {
		t.Fatal("应报告已改写")
	}

	choice := obj["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason 应为 tool_calls，得 %v", choice["finish_reason"])
	}
	tcs, ok := msg["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls 缺失或数量不对: %v", msg["tool_calls"])
	}
	if c, _ := msg["content"].(string); c != "" {
		t.Errorf("DSML 块应被剥离，content 得 %q", c)
	}
}

// TestParseDSMLResponseNoDSMLLeavesObjectAlone 无 DSML 的响应必须原样返回 false。
func TestParseDSMLResponseNoDSMLLeavesObjectAlone(t *testing.T) {
	obj := map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": "普通回答"},
			"finish_reason": "stop",
		}},
	}
	if parseDSMLResponse(obj) {
		t.Error("无 DSML 时不该报告改写")
	}
	choice := obj["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Error("finish_reason 不该被改")
	}
	if _, has := choice["message"].(map[string]any)["tool_calls"]; has {
		t.Error("不该凭空生成 tool_calls")
	}
}

// TestAggregateEndToEndDSML 端到端：上游返回 DSML 文本流，Aggregate 后得到标准 tool_calls。
func TestAggregateEndToEndDSML(t *testing.T) {
	dsml := `<|DSML|tool_calls><|DSML|invoke name="get_weather"><|DSML|parameter name="city" string="true">北京</|DSML|parameter></|DSML|invoke></|DSML|tool_calls>`
	frame, _ := json.Marshal(map[string]any{
		"id":    "1",
		"model": "deepseek-v4-flash",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{"content": dsml},
			"finish_reason": "stop",
		}},
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data:"+string(frame)+"\n")
		_, _ = io.WriteString(w, "data:[DONE]\n")
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	rc, _, _, err := c.ChatStream(makeAuth("u1"),
		[]byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"北京天气"}]}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()

	resp, err := c.Aggregate(rc, "codearts/deepseek-v4-flash")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}

	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v，应为 tool_calls", choice["finish_reason"])
	}
	// 注意：upstream.Aggregate 存的是 []map[string]any（非 []any），全渠道共用该形态，
	// 序列化成 JSON 后客户端无差异。故经 JSON 往返再断言，不直接做 []any 类型断言。
	var probe struct {
		Choices []struct {
			Message struct {
				ToolCalls []map[string]any `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	raw, _ := json.Marshal(resp)
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("回读响应失败: %v", err)
	}
	tcs := probe.Choices[0].Message.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("tool_calls 数量=%d，应为 1（raw=%s）", len(tcs), string(raw))
	}
	fn := tcs[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("name = %v", fn["name"])
	}
}

// ---------------------------------------------------------------------------
// 流式 DSML 过滤器（R144 核心）
//
// Hermes 主对话恒发 stream:true（chat_completion_helpers.py:3117 hardcode），
// 走 Stream 路径；StreamCapture 边读边转发，父层无法整体改写。
// DSML 文本在 delta.content 里跨帧累积，故须帧级状态机。
// ---------------------------------------------------------------------------

// TestDSMLStreamFilterBuffersUntilBlockCloses 开标签后进入缓冲，期间不吐；闭标签后给 tool_calls 帧。
func TestDSMLStreamFilterBuffersUntilBlockCloses(t *testing.T) {
	var f dsmlStreamFilter

	text, frame := f.feed("我先查天气。")
	if frame != nil {
		t.Fatal("普通文本不该产生 frame")
	}
	if text != "我先查天气。" {
		t.Errorf("普通文本应原样吐出，得 %q", text)
	}

	// 开标签跨段断裂
	text, frame = f.feed(`<|DSML|too`)
	if text != "" || frame != nil {
		t.Errorf("开标签片段不该吐：text=%q frame=%v", text, frame)
	}
	text, frame = f.feed(`l_calls><|DSML|invoke name="get_weather">`)
	if text != "" || frame != nil {
		t.Errorf("缓冲期不该吐：text=%q frame=%v", text, frame)
	}
	text, frame = f.feed(`<|DSML|parameter name="city" string="true">北京</|DSML|parameter>`)
	if text != "" || frame != nil {
		t.Errorf("缓冲期不该吐：text=%q frame=%v", text, frame)
	}

	// 闭标签
	text, frame = f.feed(`</|DSML|invoke></|DSML|tool_calls>`)
	if text != "" {
		t.Errorf("闭合帧不该带文本，得 %q", text)
	}
	if frame == nil {
		t.Fatal("闭合应产生 frame")
	}
	choices, _ := frame["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("frame 结构不对: %v", frame)
	}
	ch := choices[0].(map[string]any)
	if ch["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason = %v", ch["finish_reason"])
	}
	delta := ch["delta"].(map[string]any)
	tcs, ok := delta["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("delta.tool_calls = %v", delta["tool_calls"])
	}
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("name = %v", fn["name"])
	}
}

// TestDSMLStreamFilterPassthroughWhenNoDSML 无 DSML 必须逐字节原样放行。
func TestDSMLStreamFilterPassthroughWhenNoDSML(t *testing.T) {
	var f dsmlStreamFilter
	for _, s := range []string{"你好", "，这是", "普通回答。"} {
		text, frame := f.feed(s)
		if frame != nil {
			t.Fatal("无 DSML 不该产生 frame")
		}
		if text != s {
			t.Errorf("应原样放行 %q，得 %q", s, text)
		}
	}
}

// TestDSMLStreamFilterKeepsTextBeforeBlock 块前正文必须先放行。
func TestDSMLStreamFilterKeepsTextBeforeBlock(t *testing.T) {
	var f dsmlStreamFilter
	text, _ := f.feed(`我先查。` + `<|DSML|tool_calls><|DSML|invoke name="get_weather">`)
	if text != "我先查。" {
		t.Errorf("块前正文应放行，得 %q", text)
	}
	_, frame := f.feed(`<|DSML|parameter name="city" string="true">北京</|DSML|parameter></|DSML|invoke></|DSML|tool_calls>`)
	if frame == nil {
		t.Fatal("闭合应产生 frame")
	}
}

// TestDSMLStreamFilterTruncatedFlushesRaw 截断（无闭标签）→ 原样吐出，不伪造 tool_calls。
func TestDSMLStreamFilterTruncatedFlushesRaw(t *testing.T) {
	var f dsmlStreamFilter
	f.feed(`<|DSML|tool_calls><|DSML|invoke name="x">`)
	tail := f.flush()
	if !strings.Contains(tail, "<|DSML|") {
		t.Errorf("截断时应原样吐出缓冲，得 %q", tail)
	}
	if strings.Contains(tail, `"tool_calls"`) {
		t.Error("截断时不得伪造 tool_calls 结构")
	}
}

// TestStreamEndToEndDSML 端到端流式：上游分多帧吐 DSML，Stream 输出必须是 tool_calls 帧
// 而不是原始 DSML 文本。这条对应 Hermes 的真实调用路径（stream:true 硬编码）。
func TestStreamEndToEndDSML(t *testing.T) {
	frames := []string{
		`{"choices":[{"index":0,"delta":{"content":"我先查。"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"<|DSML|tool_calls><|DSML|invoke name=\"get_weather\">"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"<|DSML|parameter name=\"city\" string=\"true\">北京</|DSML|parameter>"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"</|DSML|invoke></|DSML|tool_calls>"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, f := range frames {
			_, _ = io.WriteString(w, "data:"+f+"\n")
		}
		_, _ = io.WriteString(w, "data:[DONE]\n")
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	rc, _, _, err := c.ChatStream(makeAuth("u1"),
		[]byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"北京天气"}],"stream":true}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()

	rec := httptest.NewRecorder()
	if _, err := c.Stream(rec, rc, "codearts/deepseek-v4-flash"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	out := rec.Body.String()

	if strings.Contains(out, `<|DSML|tool_calls>`) {
		t.Errorf("DSML 原文透给了客户端:\n%s", out)
	}
	if !strings.Contains(out, `"tool_calls"`) {
		t.Errorf("缺 tool_calls 帧:\n%s", out)
	}
	if !strings.Contains(out, `"finish_reason":"tool_calls"`) {
		t.Errorf("finish_reason 未改成 tool_calls:\n%s", out)
	}
	if !strings.Contains(out, "我先查。") {
		t.Errorf("普通文本丢失:\n%s", out)
	}
}

// TestStreamNonDSMLModelUnaffected 非 DSML 模型（deepseek-v4.1-flash）必须完全不受影响：
// 内联的 "<|DSML|" 字样应逐字透传（证明过滤器没误伤）。
func TestStreamNonDSMLModelUnaffected(t *testing.T) {
	text := `模型说：这是 <|DSML|tool_calls> 字样，不该被吃`
	frame, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil,
		}},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "data:"+string(frame)+"\n")
		_, _ = io.WriteString(w, "data:[DONE]\n")
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	rc, _, _, err := c.ChatStream(makeAuth("u1"),
		[]byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"x"}],"stream":true}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()

	rec := httptest.NewRecorder()
	if _, err := c.Stream(rec, rc, "codearts/deepseek-v4.1-flash"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "不该被吃") {
		t.Errorf("非 DSML 模型的内容被误伤:\n%s", rec.Body.String())
	}
}

// TestParseDSMLBlockOpenVariant 真实上游（21 个工具时）输出的开标签是
// `<|DSML|tool_calls|`（末尾 `|` 而非 `>`），实测于 Hermes 真实请求（92KB 体、21 tools）。
// 解析器必须容忍该变体，否则整块原样透传给客户端（工具调用失效）。
func TestParseDSMLBlockOpenVariant(t *testing.T) {
	// 上游真实形态：开标签末尾是 '|'
	s := `<|DSML|tool_calls|<|DSML|invoke name="terminal"><|DSML|parameter name="command" string="true">date</|DSML|parameter></|DSML|invoke></|DSML|tool_calls>`

	blocks, rest := parseDSMLBlocks(s)
	if len(blocks) != 1 {
		t.Fatalf("变体开标签应被识别，得 %d 块（rest=%q）", len(blocks), rest)
	}
	if rest != "" {
		t.Errorf("块外正文应为空，得 %q", rest)
	}
	calls := dsmlToToolCalls(blocks)
	if len(calls) != 1 {
		t.Fatalf("应得 1 个 tool_call，得 %d", len(calls))
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "terminal" {
		t.Errorf("name = %v", fn["name"])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("arguments 非法: %v", err)
	}
	if args["command"] != "date" {
		t.Errorf("command = %v", args["command"])
	}
}

// TestDSMLStreamFilterOpenVariant 流式下上游开标签变体（末尾 `|`）也必须被缓冲识别。
// 对应 Hermes 真实请求（21 tools）的输出形态。
func TestDSMLStreamFilterOpenVariant(t *testing.T) {
	var f dsmlStreamFilter
	// 第 1 段：变体开标签 + invoke 头
	text, frame := f.feed(`<|DSML|tool_calls|<|DSML|invoke name="terminal">`)
	if text != "" || frame != nil {
		t.Fatalf("变体开标签应进入缓冲：text=%q frame=%v", text, frame)
	}
	// 第 2 段：参数 + 闭标签
	text, frame = f.feed(`<|DSML|parameter name="command" string="true">date</|DSML|parameter></|DSML|invoke></|DSML|tool_calls>`)
	if text != "" {
		t.Errorf("不该带文本，得 %q", text)
	}
	if frame == nil {
		t.Fatal("闭合应产生 frame（变体开标签）")
	}
	choices := frame["choices"].([]any)
	delta := choices[0].(map[string]any)["delta"].(map[string]any)
	tcs := delta["tool_calls"].([]any)
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "terminal" {
		t.Errorf("name = %v", fn["name"])
	}
}
