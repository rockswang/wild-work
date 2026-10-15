package upstream

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/loopguard"
)

// reasoningLoopFixture 构造一个「思考链死循环」的 SSE 流：
// 同一长行 reasoning_content 连续重复 8 次（每帧一行），随后继续发正文。
// 守卫应在第 8 行命中并中断，客户端收到 error 帧、无 [DONE]。
func reasoningLoopFixture() string {
	var b strings.Builder
	line := "这里是一个足够长的、会被模型机械重复的思考行内容"
	for i := 0; i < 10; i++ {
		b.WriteString(`data: {"id":"x","model":"m","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"` + line + `\n"}}]}` + "\n\n")
	}
	// 死循环后本应到达的正文（守卫命中后不应再透传）
	b.WriteString(`data: {"id":"x","model":"m","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"正文"}}]}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// TestStreamReasoningLoopIntercepted 守卫命中：发 error 帧（code=upstream_reasoning_loop），
// 不补 [DONE]，返回 *loopguard.Error，且死循环之后的正文帧不再透传。
func TestStreamReasoningLoopIntercepted(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(reasoningLoopFixture()))
	if err == nil {
		t.Fatal("期望守卫命中返回错误，实际 nil")
	}
	var lge *loopguard.Error
	if !errors.As(err, &lge) {
		t.Fatalf("期望 *loopguard.Error，得 %T: %v", err, err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"upstream_reasoning_loop"`) {
		t.Errorf("缺少 upstream_reasoning_loop 错误帧: %q", body)
	}
	if strings.Contains(body, "data: [DONE]") {
		t.Errorf("守卫命中不得补 [DONE]: %q", body)
	}
	if strings.Contains(body, "正文") {
		t.Errorf("死循环之后的正文不应透传: %q", body)
	}
}

// TestStreamReasoningLoopDisabled 关闭守卫时，同一输入应正常走完（透传正文 + [DONE]）。
func TestStreamReasoningLoopDisabled(t *testing.T) {
	loopguard.SetEnabled(false)
	defer loopguard.SetEnabled(true)

	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(reasoningLoopFixture()))
	if err != nil {
		t.Fatalf("禁用守卫后不应报错：%v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("禁用守卫后应正常收尾: %q", body)
	}
	if strings.Contains(body, "upstream_reasoning_loop") {
		t.Errorf("禁用守卫后不应出现错误帧: %q", body)
	}
}

// TestStreamNormalReasoningPasses 正常各不相同的推理行不应触发（零误伤），
// 全程透传并正常 [DONE] 收尾。
func TestStreamNormalReasoningPasses(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString(`data: {"id":"x","model":"m","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"第 ` + itoa(i) + ` 步思考内容各不相同\n"}}]}` + "\n\n")
	}
	b.WriteString("data: [DONE]\n\n")

	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(b.String()))
	if err != nil {
		t.Fatalf("正常推理不应报错：%v", err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("正常推理应正常收尾: %q", rec.Body.String())
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for i > 0 {
		d = append([]byte{byte('0' + i%10)}, d...)
		i /= 10
	}
	return string(d)
}
