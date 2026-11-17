package chatgpt

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// 流式与非流式共用同一份解析（consumeTurn），故两者的断言必须逐字一致：
// 同一个上游流，Stream 拼出的正文 == Aggregate 的 content。
func splitFrames() string {
	return "data: " + `{"conversation_id":"c-1","message":{"id":"m1","author":{"role":"assistant"},"content":{"content_type":"text","parts":["你"]}}}` + "\n\n" +
		"data: " + `{"o":"patch","v":[{"p":"/message/content/thoughts","o":"append","v":"想一想"}]}` + "\n\n" +
		"data: " + `{"o":"patch","v":[{"p":"/message/content/parts/0","o":"append","v":"好<Ci"}]}` + "\n\n" +
		// 引用标记被切在两帧之间：只按帧剥离会漏 ⇒ 必须跨帧缓冲。
		"data: " + `{"v":[{"p":"/message/content/parts/0","o":"append","v":"te refs={[\"a\"]}/>世界"}]}` + "\n\n"
}

// openAISSE 解析出口：返回正文增量拼接、思考增量拼接、收尾原因、是否有 [DONE]、错误帧。
func openAISSE(t *testing.T, body string) (text, reason, finish string, done bool, errFrames []string) {
	t.Helper()
	var texts, reasons strings.Builder
	for _, chunk := range strings.Split(body, "\n\n") {
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}
		if !strings.HasPrefix(chunk, "data: ") {
			t.Fatalf("非 SSE 帧: %q", chunk)
		}
		payload := strings.TrimPrefix(chunk, "data: ")
		if payload == "[DONE]" {
			done = true
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(payload), &ev) != nil {
			t.Fatalf("帧不是 JSON: %q", payload)
		}
		if _, ok := ev["error"]; ok {
			errFrames = append(errFrames, payload)
			continue
		}
		choices, _ := ev["choices"].([]any)
		if len(choices) == 0 {
			t.Fatalf("帧缺 choices: %q", payload)
		}
		ch, _ := choices[0].(map[string]any)
		if fr, _ := ch["finish_reason"].(string); fr != "" {
			finish = fr
		}
		d, _ := ch["delta"].(map[string]any)
		if s, _ := d["content"].(string); s != "" {
			texts.WriteString(s)
		}
		if s, _ := d["reasoning_content"].(string); s != "" {
			reasons.WriteString(s)
		}
	}
	return texts.String(), reasons.String(), finish, done, errFrames
}

// TestStreamEmitsOpenAIFrames 双 patch 形态 + 思考链 + 跨帧引用剥离，收尾 finish + [DONE]。
func TestStreamEmitsOpenAIFrames(t *testing.T) {
	c := New()
	rec := httptest.NewRecorder()
	usage, err := c.Stream(rec, strings.NewReader(splitFrames()+"data: [DONE]\n\n"), "chatgpt/auto")
	if err != nil {
		t.Fatalf("Stream err=%v", err)
	}
	if usage != nil {
		t.Errorf("上游不报 usage，应为 nil，实际 %v", usage)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	text, reason, finish, done, errFrames := openAISSE(t, rec.Body.String())
	if text != "你好世界" {
		t.Errorf("正文 = %q，期望 %q（引用必须剥掉）", text, "你好世界")
	}
	if reason != "想一想" {
		t.Errorf("思考 = %q", reason)
	}
	if finish != "stop" || !done {
		t.Errorf("finish=%q done=%v", finish, done)
	}
	if len(errFrames) != 0 {
		t.Errorf("正常流不得有错误帧: %v", errFrames)
	}
	if strings.Contains(rec.Body.String(), "Cite") || strings.Contains(rec.Body.String(), "tool_calls") {
		t.Error("出口不得残留引用标记，也不得伪造 tool_calls")
	}
}

// TestStreamTruncatedNoDone 截断：报错 + 错误帧，**绝不补 [DONE]**。
func TestStreamTruncatedNoDone(t *testing.T) {
	c := New()
	rec := httptest.NewRecorder()
	_, err := c.Stream(rec, strings.NewReader(splitFrames()), "chatgpt/auto")
	if err == nil {
		t.Fatal("缺 [DONE] 必须报截断")
	}
	if !strings.Contains(err.Error(), "上游流被截断") {
		t.Errorf("错误文案 = %q", err.Error())
	}
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Error("截断流不得补 [DONE]（不得伪装正常收尾）")
	}
	_, _, _, _, errFrames := openAISSE(t, body)
	if len(errFrames) != 1 || !strings.Contains(errFrames[0], "upstream_truncated") {
		t.Errorf("错误帧 = %v", errFrames)
	}
	ce, ok := err.(*provider.Classified)
	if !ok || ce.Kind() != provider.ErrUpstream {
		t.Errorf("截断分类 = %v，期望 ErrUpstream（不换号、不计账号错误）", err)
	}
}

// TestStreamBusinessErrorFrameNoDone 信封业务错误：同样是错误帧，不补收尾。
func TestStreamBusinessErrorFrameNoDone(t *testing.T) {
	c := New()
	payload := `{"type":"error","error":{"message":"boom","code":"bad_thing"}}`
	rec := httptest.NewRecorder()
	_, err := c.Stream(rec, strings.NewReader("data: "+payload+"\n\ndata: [DONE]\n\n"), "chatgpt/auto")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("业务错误帧必须报错，err=%v", err)
	}
	body := rec.Body.String()
	if strings.Contains(body, "[DONE]") {
		t.Error("业务错误不得补 [DONE]")
	}
	_, _, _, _, errFrames := openAISSE(t, body)
	if len(errFrames) != 1 || !strings.Contains(errFrames[0], "upstream_error") {
		t.Errorf("错误帧 = %v（code 必须是 upstream_error，不能混成截断）", errFrames)
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrUpstream {
		t.Errorf("分类 = %v，期望 ErrUpstream", err)
	}
}

// TestAggregateSameContentAsStream 非流式与流式的正文/思考/收尾完全一致，且不给 usage。
func TestAggregateSameContentAsStream(t *testing.T) {
	c := New()
	rec := httptest.NewRecorder()
	if _, err := c.Stream(rec, strings.NewReader(splitFrames()+"data: [DONE]\n\n"), "chatgpt/auto"); err != nil {
		t.Fatalf("Stream err=%v", err)
	}
	streamText, streamReason, _, _, _ := openAISSE(t, rec.Body.String())

	body, err := c.Aggregate(strings.NewReader(splitFrames()+"data: [DONE]\n\n"), "chatgpt/auto")
	if err != nil {
		t.Fatalf("Aggregate err=%v", err)
	}
	choices, _ := body["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != streamText {
		t.Errorf("非流式 content=%v，流式=%q，必须一致", msg["content"], streamText)
	}
	if msg["reasoning_content"] != streamReason {
		t.Errorf("非流式思考=%v，流式=%q", msg["reasoning_content"], streamReason)
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("finish_reason 缺失: %v", choices[0])
	}
	if _, ok := body["usage"]; ok {
		t.Error("上游不报 usage 时不得编造 usage 字段")
	}
	if body["model"] != "chatgpt/auto" {
		t.Errorf("model = %v，必须回显客户端请求的模型名", body["model"])
	}
	if strings.Contains(msg["content"].(string), "Cite") {
		t.Error("非流式正文也必须是剥净引用的")
	}
}

// TestAggregateTruncatedFails 非流式同样不得把截断伪装成正常完成。
func TestAggregateTruncatedFails(t *testing.T) {
	c := New()
	body, err := c.Aggregate(strings.NewReader(splitFrames()), "chatgpt/auto")
	if err == nil || body != nil {
		t.Fatalf("截断必须报错，body=%v err=%v", body, err)
	}
	if !strings.Contains(err.Error(), "上游流被截断") {
		t.Errorf("错误文案 = %q", err.Error())
	}
}

// TestStreamEmptyUpstreamIsError 上游零语义帧 ⇒ 报错（不得回一条空成功）。
func TestStreamEmptyUpstreamIsError(t *testing.T) {
	c := New()
	rec := httptest.NewRecorder()
	_, err := c.Stream(rec, strings.NewReader("data: [DONE]\n\n"), "chatgpt/auto")
	if err == nil || !strings.Contains(err.Error(), "未产出任何内容") {
		t.Fatalf("空流必须报错，err=%v", err)
	}
	if strings.Contains(rec.Body.String(), "[DONE]") {
		t.Error("空流不得补 [DONE]")
	}
}

// TestAggregateKeepsPlainTextVerbatim 纯文本不经脱敏/裁剪（引用剥离只碰标记本身）。
func TestAggregateKeepsPlainTextVerbatim(t *testing.T) {
	c := New()
	payload := `{"message":{"id":"m","author":{"role":"assistant"},"content":{"parts":["  keep  spaces  "]}}}`
	body, err := c.Aggregate(strings.NewReader("data: "+payload+"\n\ndata: [DONE]\n\n"), "chatgpt/auto")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	msg := body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "  keep  spaces  " {
		t.Errorf("正文被改动: %q", msg["content"])
	}
}
