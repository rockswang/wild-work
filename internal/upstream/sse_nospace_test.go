package upstream

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSSENoSpaceAfterColon 锁定：上游 SSE 帧可能是 `data:{...}`（冒号后**无空格**），
// 解析器必须同样认。这是 SSE 规范的合法形态（规范只要求 `data:` 后跟可选空格）。
//
// 背景（2026-10-18 用户实测报告）：LobsterAI 6 个账号 6/6 全部不可用。
// 上游 SSE 帧形态是 `data:{...}`（无空格），而 internal/upstream 的
// StreamCapture/Aggregate 只认 `data: `（**带空格**）⇒
//   - StreamCapture：帧掉进「其他行」分支原样透传，但 validFrames 恒 0
//     ⇒ 触发「空流」兜底，给**每个正常回答额外追加一帧**
//        `{"error":{"message":"empty upstream stream",...}}`，并返回 errEmptyStream；
//   - Aggregate（非流式）：帧完全不被识别 ⇒ validEvents=0 ⇒ 直接 502 upstream_parse。
// 而 errEmptyStream 会被上层当作「账号连续错误」处理（见 client.go:223 的注释），
// 于是健康账号被冷却 —— 这才是「6/6 全坏」的完整链条。
func TestSSENoSpaceAfterColon(t *testing.T) {
	// ① Aggregate 路径：无空格的帧必须被识别成正文，而不是空流。
	body := strings.Join([]string{
		`data:{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"你好"}}]}`,
		``,
		`data:{"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		``,
		`data:[DONE]`,
		``,
	}, "\n")

	resp, err := Aggregate(io.NopCloser(strings.NewReader(body)))
	if err != nil {
		t.Fatalf("❌ 无空格 data: 帧被当成空流（err=%v）—— 这正是 LobsterAI 全坏的机理", err)
	}
	if resp == nil {
		t.Fatal("❌ Aggregate 返回 nil 响应")
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("❌ 无空格 data: 帧未被解析出任何 choices，resp=%v", resp)
	}
	first, _ := choices[0].(map[string]any)
	msg, _ := first["message"].(map[string]any)
	got, _ := msg["content"].(string)
	if got != "你好" {
		t.Errorf("❌ 正文解析错误：期望 %q，实得 %q", "你好", got)
	}

	// ② StreamCapture 路径：不得把无空格帧判成空流、也不得追加 empty upstream stream。
	rec := httptest.NewRecorder()
	if err := StreamCapture(rec, strings.NewReader(body), nil); err != nil {
		t.Errorf("❌ StreamCapture 返回错误（%v）—— 无空格帧不应被判为空流", err)
	}
	out := rec.Body.String()
	if strings.Contains(out, "empty upstream stream") {
		t.Errorf("❌ 输出里出现了伪造的 empty upstream stream 错误帧：\n%s", out)
	}
	if !strings.Contains(out, "你好") {
		t.Errorf("❌ 输出里没有正文，帧被吞了：\n%s", out)
	}
}

// TestSSEWithSpaceStillWorks 反向保护：带空格的形态（绝大多数上游）不得被这次修复弄坏。
func TestSSEWithSpaceStillWorks(t *testing.T) {
	body := strings.Join([]string{
		`data: {"id":"1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	resp, err := Aggregate(io.NopCloser(strings.NewReader(body)))
	if err != nil {
		t.Fatalf("带空格形态回归：%v", err)
	}
	if ch, _ := resp["choices"].([]any); len(ch) == 0 {
		t.Fatal("带空格形态未解析出 choices")
	}

	rec := httptest.NewRecorder()
	if err := StreamCapture(rec, strings.NewReader(body), nil); err != nil {
		t.Errorf("带空格形态回归：%v", err)
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Errorf("带空格形态正文丢失：%s", rec.Body.String())
	}
}
