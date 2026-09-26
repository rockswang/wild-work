package glm

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ===========================================================================
// 本文件的帧序列全部来自 2026-09-26 对 chatglm.cn 的**真实抓包**，
// 不是臆造的。这是本渠道最容易写错的地方（详见 sse.go 顶部注释）：
//
//	part.status == "init"   → text/think 是**增量片段**
//	part.status == "finish" → text/think 是**该段落完整全文**
// ===========================================================================

// TestRenderPartInitVsFinish 校验同一段落两种 status 的渲染差异。
func TestRenderPartInitVsFinish(t *testing.T) {
	// init：增量片段，原样返回
	initPart := part{
		Status:  "init",
		Content: []partContent{{Type: "text", Text: "你好"}},
	}
	text, _ := renderPart(initPart)
	if text != "你好" {
		t.Errorf("init 段 text = %q, want 你好（增量原样）", text)
	}

	// finish：完整全文，代码块要收尾
	finPart := part{
		Status:  "finish",
		Content: []partContent{{Type: "code", Code: "print(1)"}},
	}
	text, _ = renderPart(finPart)
	if !strings.Contains(text, "```python") || !strings.Contains(text, "print(1)") {
		t.Errorf("finish 段代码块未包裹: %q", text)
	}
	if !strings.HasSuffix(text, "\n```\n") {
		t.Errorf("finish 段代码块未收尾: %q", text)
	}
}

// TestRenderPartImage 校验图片转 Markdown，仅 http(s) 保留，且只在 finish 输出。
func TestRenderPartImage(t *testing.T) {
	img := []imageItem{
		{ImageURL: "https://example.com/a.png"},
		{ImageURL: "data:image/png;base64,xxx"}, // 非 http 应被丢弃
	}
	// init 阶段不应输出图片（内容还不完整）
	text, _ := renderPart(part{Status: "init", Content: []partContent{{Type: "image", Image: img}}})
	if strings.Contains(text, "![图像]") {
		t.Errorf("init 段不应输出图片: %q", text)
	}
	// finish 阶段输出
	text, _ = renderPart(part{Status: "finish", Content: []partContent{{Type: "image", Image: img}}})
	if !strings.Contains(text, "![图像](https://example.com/a.png)") {
		t.Errorf("finish 段 http 图片未转换: %q", text)
	}
	if strings.Contains(text, "data:image") {
		t.Errorf("data: URI 不应输出: %q", text)
	}
}

// realFrames 真实抓包：问「从1数到5，只输出数字，用逗号分隔」的完整帧序列。
// 这些 text 值是从上游原样抄下来的，构成回归基线。
var realFrames = []string{
	`{"id":"a","conversation_id":"c1","status":"init","parts":[]}`,
	`{"id":"a","conversation_id":"c1","status":"init","parts":[{"id":"a","logic_id":"L1","role":"assistant","status":"init","content":[{"type":"text","text":"1"}]}]}`,
	`{"id":"a","conversation_id":"c1","status":"init","parts":[{"id":"a","logic_id":"L1","role":"assistant","status":"init","content":[{"type":"text","text":","}]}]}`,
	`{"id":"a","conversation_id":"c1","status":"init","parts":[{"id":"a","logic_id":"L1","role":"assistant","status":"init","content":[{"type":"text","text":" "}]}]}`,
	`{"id":"a","conversation_id":"c1","status":"init","parts":[{"id":"a","logic_id":"L1","role":"assistant","status":"init","content":[{"type":"text","text":"2"}]}]}`,
	`{"id":"a","conversation_id":"c1","status":"init","parts":[{"id":"a","logic_id":"L1","role":"assistant","status":"init","content":[{"type":"text","text":","}]}]}`,
	`{"id":"a","conversation_id":"c1","status":"init","parts":[{"id":"a","logic_id":"L1","role":"assistant","status":"init","content":[{"type":"text","text":" "}]}]}`,
	`{"id":"a","conversation_id":"c1","status":"init","parts":[{"id":"a","logic_id":"L1","role":"assistant","status":"init","content":[{"type":"text","text":"3"}]}]}`,
	`{"id":"a","conversation_id":"c1","status":"init","parts":[{"id":"a","logic_id":"L1","role":"assistant","status":"init","content":[{"type":"text","text":","}]}]}`,
	`{"id":"a","conversation_id":"c1","status":"init","parts":[{"id":"a","logic_id":"L1","role":"assistant","status":"init","content":[{"type":"text","text":" "}]}]}`,
	`{"id":"a","conversation_id":"c1","status":"init","parts":[{"id":"a","logic_id":"L1","role":"assistant","status":"init","content":[{"type":"text","text":"4, 5"}]}]}`,
	// finish 帧：同一段落给出完整全文
	`{"id":"a","conversation_id":"c1","status":"init","parts":[{"id":"a","logic_id":"L1","role":"assistant","status":"finish","content":[{"type":"text","text":"1, 2, 3, 4, 5"}]}]}`,
	`{"id":"a","conversation_id":"c1","status":"finish","parts":[{"id":"a","logic_id":"L1","role":"assistant","status":"finish","content":[{"type":"text","text":"1, 2, 3, 4, 5"}]}]}`,
}

func sseFrom(frames []string) string {
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString("event: message\ndata: " + f + "\n\n")
	}
	return sb.String()
}

// TestStreamRealFramesNoDuplication 是本渠道最关键的回归测试。
//
// 用真实抓包帧序列，断言流式输出恰好等于 finish 帧的全文。
// 这能同时抓住两类错误：
//   - 把 init 增量当快照 → 丢字（曾实测拼出 "1, 3, 4, 5, 5"）
//   - 把 finish 全文当增量重复发 → 重复
func TestStreamRealFramesNoDuplication(t *testing.T) {
	rec := &flushRecorder{}
	c := NewWithBase("http://unused")
	if _, err := c.Stream(rec, strings.NewReader(sseFrom(realFrames)), "glm/chatglm"); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	content, reasoning := collectDeltas(t, rec)
	const want = "1, 2, 3, 4, 5"
	if content != want {
		t.Errorf("正文 = %q, want %q\n（真实抓包基线：init 帧是增量，finish 帧是全文）", content, want)
	}
	if reasoning != "" {
		t.Errorf("不该有思考内容: %q", reasoning)
	}
	if !strings.Contains(rec.String(), "data: [DONE]") {
		t.Error("缺少 [DONE]")
	}
	if !strings.Contains(rec.String(), `"finish_reason":"stop"`) {
		t.Error("缺少 finish_reason=stop")
	}
	if !strings.Contains(rec.String(), `"role":"assistant"`) {
		t.Error("缺少 role 帧")
	}
	if !strings.Contains(rec.String(), `"id":"c1"`) {
		t.Error("未使用上游 conversation_id 作为响应 id")
	}
}

// TestAggregateRealFrames 非流式聚合也必须等于 finish 帧全文。
func TestAggregateRealFrames(t *testing.T) {
	c := NewWithBase("http://unused")
	resp, err := c.Aggregate(strings.NewReader(sseFrom(realFrames)), "glm/chatglm")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if resp["id"] != "c1" {
		t.Errorf("id = %v, want c1", resp["id"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "1, 2, 3, 4, 5" {
		t.Errorf("content = %q, want %q", msg["content"], "1, 2, 3, 4, 5")
	}
}

// TestStreamThinkingIncremental 推理模型的 think 段同样按增量处理。
// 帧数据取自真实抓包（9.11 vs 9.9 实验的 think 片段）。
func TestStreamThinkingIncremental(t *testing.T) {
	frames := []string{
		`{"conversation_id":"c2","status":"init","parts":[{"logic_id":"T1","status":"init","content":[{"type":"think","think":"Simple"}]}]}`,
		`{"conversation_id":"c2","status":"init","parts":[{"logic_id":"T1","status":"init","content":[{"type":"think","think":" question"}]}]}`,
		`{"conversation_id":"c2","status":"init","parts":[{"logic_id":"T1","status":"init","content":[{"type":"think","think":":"}]}]}`,
		`{"conversation_id":"c2","status":"init","parts":[{"logic_id":"T1","status":"init","content":[{"type":"text","text":"9.9"}]}]}`,
		`{"conversation_id":"c2","status":"init","parts":[{"logic_id":"T1","status":"finish","content":[{"type":"think","think":"Simple question:"},{"type":"text","text":"9.9 更大"}]}]}`,
		`{"conversation_id":"c2","status":"finish","parts":[{"logic_id":"T1","status":"finish","content":[{"type":"think","think":"Simple question:"},{"type":"text","text":"9.9 更大"}]}]}`,
	}
	rec := &flushRecorder{}
	c := NewWithBase("http://unused")
	if _, err := c.Stream(rec, strings.NewReader(sseFrom(frames)), "glm/chatglm-think"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	content, reasoning := collectDeltas(t, rec)
	if content != "9.9 更大" {
		t.Errorf("正文 = %q, want %q", content, "9.9 更大")
	}
	if reasoning != "Simple question:" {
		t.Errorf("思考 = %q, want %q", reasoning, "Simple question:")
	}
}

// TestStreamFinishRepairsMissingDelta 验证 finish 帧的「差额补发」兜底：
// 若上游漏发了某些 init 增量，finish 帧必须把缺口补上。
func TestStreamFinishRepairsMissingDelta(t *testing.T) {
	frames := []string{
		// 只发了前两个片段（模拟丢帧）
		`{"conversation_id":"c3","status":"init","parts":[{"logic_id":"L1","status":"init","content":[{"type":"text","text":"你好"}]}]}`,
		`{"conversation_id":"c3","status":"init","parts":[{"logic_id":"L1","status":"init","content":[{"type":"text","text":"，"}]}]}`,
		// finish 给出全文，应补发 "世界"
		`{"conversation_id":"c3","status":"finish","parts":[{"logic_id":"L1","status":"finish","content":[{"type":"text","text":"你好，世界"}]}]}`,
	}
	rec := &flushRecorder{}
	c := NewWithBase("http://unused")
	if _, err := c.Stream(rec, strings.NewReader(sseFrom(frames)), "glm/chatglm"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	content, _ := collectDeltas(t, rec)
	if content != "你好，世界" {
		t.Errorf("正文 = %q, want 你好，世界（finish 帧应补上丢失的片段）", content)
	}
}

// TestStreamNoDuplicateOnRepeatedFinish 重复 finish 帧不得重复发出内容。
func TestStreamNoDuplicateOnRepeatedFinish(t *testing.T) {
	finish := `{"conversation_id":"c4","status":"finish","parts":[{"logic_id":"L1","status":"finish","content":[{"type":"text","text":"完整内容"}]}]}`
	frames := []string{
		`{"conversation_id":"c4","status":"init","parts":[{"logic_id":"L1","status":"init","content":[{"type":"text","text":"完整"}]}]}`,
		`{"conversation_id":"c4","status":"init","parts":[{"logic_id":"L1","status":"init","content":[{"type":"text","text":"内容"}]}]}`,
		finish,
		finish, // 上游重复发 finish（真实发生过：帧12、13 内容相同）
	}
	rec := &flushRecorder{}
	c := NewWithBase("http://unused")
	if _, err := c.Stream(rec, strings.NewReader(sseFrom(frames)), "glm/chatglm"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	content, _ := collectDeltas(t, rec)
	if content != "完整内容" {
		t.Errorf("正文 = %q, want 完整内容（重复 finish 不得重复输出）", content)
	}
	// finish_reason=stop 只应出现一次
	if n := strings.Count(rec.String(), `"finish_reason":"stop"`); n != 1 {
		t.Errorf("finish_reason=stop 出现 %d 次, want 1", n)
	}
}

// TestStreamMultiPart 多段落（正文 + 代码）按 logic_id 分别累加。
//
// 注意 init 帧只带**增量**（真实上游行为）：段 A 的 "说明：" 只在第一帧出现，
// 第二帧只带段 B 的代码增量。若某帧重复带全量文本，那才是异常。
func TestStreamMultiPart(t *testing.T) {
	frames := []string{
		`{"conversation_id":"c5","status":"init","parts":[{"logic_id":"A","status":"init","content":[{"type":"text","text":"说明："}]}]}`,
		// 第二帧：段 A 不再出现（已发完），只带段 B 的增量
		`{"conversation_id":"c5","status":"init","parts":[{"logic_id":"B","status":"init","content":[{"type":"code","code":"print(1)"}]}]}`,
		// finish：A 与 B 都给全文
		`{"conversation_id":"c5","status":"finish","parts":[{"logic_id":"A","status":"finish","content":[{"type":"text","text":"说明："}]},{"logic_id":"B","status":"finish","content":[{"type":"code","code":"print(1)"}]}]}`,
	}
	rec := &flushRecorder{}
	c := NewWithBase("http://unused")
	if _, err := c.Stream(rec, strings.NewReader(sseFrom(frames)), "glm/chatglm"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	content, _ := collectDeltas(t, rec)
	if strings.Count(content, "说明：") != 1 {
		t.Errorf("段 A 内容重复: %q", content)
	}
	if strings.Count(content, "```python") != 1 {
		t.Errorf("代码块起始标记重复: %q", content)
	}
	if !strings.Contains(content, "print(1)") {
		t.Errorf("代码段缺失: %q", content)
	}
}

// TestStreamTruncatedStillCloses 上游异常中断（无 finish 帧）也必须收尾，
// 否则客户端会一直挂住等 [DONE]。
func TestStreamTruncatedStillCloses(t *testing.T) {
	frames := []string{
		`{"conversation_id":"c6","status":"init","parts":[{"logic_id":"L1","status":"init","content":[{"type":"text","text":"半截"}]}]}`,
	}
	rec := &flushRecorder{}
	c := NewWithBase("http://unused")
	if _, err := c.Stream(rec, strings.NewReader(sseFrom(frames)), "glm/chatglm"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	out := rec.String()
	if !strings.Contains(out, "data: [DONE]") {
		t.Error("异常中断未补 [DONE]")
	}
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Error("异常中断未补 finish_reason")
	}
	if content, _ := collectDeltas(t, rec); content != "半截" {
		t.Errorf("正文 = %q, want 半截", content)
	}
}

// TestParseSSEMultiLineData 校验多行 data 与 event 行处理。
func TestParseSSEMultiLineData(t *testing.T) {
	input := "event: message\n" +
		"data: {\"a\":1,\n" +
		"data: \"b\":2}\n" +
		"\n"
	var got []string
	err := parseSSE(strings.NewReader(input), func(payload []byte) error {
		got = append(got, string(payload))
		return nil
	})
	if err != nil {
		t.Fatalf("parseSSE: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("帧数 = %d, want 1", len(got))
	}
	if got[0] != "{\"a\":1,\n\"b\":2}" {
		t.Errorf("多行 data 拼接错误: %q", got[0])
	}
}

// TestPartKeyFallback 段落标识回退顺序：logic_id > id > 序号。
func TestPartKeyFallback(t *testing.T) {
	if k := partKey(part{LogicID: "L", ID: "I"}, 0); k != "L" {
		t.Errorf("应优先 logic_id, got %q", k)
	}
	if k := partKey(part{ID: "I"}, 0); k != "I" {
		t.Errorf("无 logic_id 应用 id, got %q", k)
	}
	if k := partKey(part{}, 7); k != "#7" {
		t.Errorf("都无应退回序号, got %q", k)
	}
}

// TestResolveAssistant 校验模型名 → (assistant_id, chat_mode) 映射。
func TestResolveAssistant(t *testing.T) {
	cases := []struct {
		model    string
		wantID   string
		wantMode string
	}{
		{"chatglm", DefaultAssistantID, ""},
		{"glm/chatglm", DefaultAssistantID, ""},
		{"", DefaultAssistantID, ""},
		{"unknown-model", DefaultAssistantID, ""},
		{"chatglm-think", DefaultAssistantID, "zero"},
		{"chatglm-zero", DefaultAssistantID, "zero"},
		{"chatglm-deepresearch", DefaultAssistantID, "deep_research"},
		{"ppt", "670e3c3e119b48fe5a851149", "ppt"},
		{"video", "668d03b2e99d661ed3c32516", "video"},
		{"67c66a1b3ea6af772310b112", "67c66a1b3ea6af772310b112", ""},
		{"glm/67c66a1b3ea6af772310b112", "67c66a1b3ea6af772310b112", ""},
	}
	for _, c := range cases {
		id, mode := resolveAssistant(c.model)
		if id != c.wantID || mode != c.wantMode {
			t.Errorf("resolveAssistant(%q) = (%s, %s), want (%s, %s)",
				c.model, id, mode, c.wantID, c.wantMode)
		}
	}
}

// TestBuildGLMMessages 校验 OpenAI → 清言消息合并。
func TestBuildGLMMessages(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "你是助手"},
		map[string]any{"role": "user", "content": "你好"},
		map[string]any{"role": "assistant", "content": "你好呀"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "继续"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "http://x"}},
		}},
	}
	out, err := buildGLMMessages(msgs)
	if err != nil {
		t.Fatalf("buildGLMMessages: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("消息数 = %d, want 1（全部合并为一条）", len(out))
	}
	text := out[0].Content[0].Text
	for _, want := range []string{"<|sytstem|>", "<|user|>", "<|assistant|>", "你是助手", "你好", "继续"} {
		if !strings.Contains(text, want) {
			t.Errorf("合并文本缺少 %q: %q", want, text)
		}
	}
	if strings.Contains(text, "http://x") {
		t.Error("图像段不应进入文本")
	}
}

// TestBuildGLMMessagesEmpty 空消息必须报错而非发出空请求。
func TestBuildGLMMessagesEmpty(t *testing.T) {
	if _, err := buildGLMMessages(nil); err == nil {
		t.Error("空消息应报错")
	}
}

// TestClassify 校验错误分类顺序（40102 优先、429 不被误判）。
func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{401, `{"status":40102,"message":"unauthorized user"}`, "session_dead"},
		{200, `{"status":40102,"message":"unauthorized user"}`, "session_dead"},
		{429, `{"message":"quota exceeded"}`, "soft_rate"},
		{403, `<html>waf</html>`, "waf_block"},
		{500, `oops`, "server"},
		{400, `{"message":"bad"}`, "client"},
		{200, `ok`, "none"},
	}
	for _, c := range cases {
		got := Classify(c.status, c.body).String()
		if got != c.want {
			t.Errorf("Classify(%d, %q) = %s, want %s", c.status, c.body, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 测试辅助
// ---------------------------------------------------------------------------

// flushRecorder 捕获写入的字节并实现 http.Flusher。
type flushRecorder struct {
	buf strings.Builder
}

func (r *flushRecorder) Header() http.Header         { return http.Header{} }
func (r *flushRecorder) Write(p []byte) (int, error) { return r.buf.Write(p) }
func (r *flushRecorder) WriteHeader(int)             {}
func (r *flushRecorder) Flush()                      {}
func (r *flushRecorder) String() string              { return r.buf.String() }

// chunks 解析出所有 data: 帧的 JSON（跳过 [DONE]）。
func (r *flushRecorder) chunks(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(r.buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" || payload == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(payload), &m); err != nil {
			t.Fatalf("帧不是合法 JSON: %q", payload)
		}
		out = append(out, m)
	}
	return out
}

// collectDeltas 把所有帧的 content / reasoning_content 增量拼起来。
func collectDeltas(t *testing.T, r *flushRecorder) (content, reasoning string) {
	t.Helper()
	var cb, rb strings.Builder
	for _, chunk := range r.chunks(t) {
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		c, _ := choices[0].(map[string]any)
		delta, _ := c["delta"].(map[string]any)
		if s, ok := delta["content"].(string); ok {
			cb.WriteString(s)
		}
		if s, ok := delta["reasoning_content"].(string); ok {
			rb.WriteString(s)
		}
	}
	return cb.String(), rb.String()
}
