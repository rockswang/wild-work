// 流内业务错误（HTTP 200 + 信封内嵌 statusCodeValue>=400）的守门测试（R108）。
// 帧原文取自 2026-10-21 本机抓包 wildwork-dump/qodercn/…-0021-post-….http（不含凭据）。
package qodercom

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/provider"
)

// nestedRateLimitFrame 上游真实的排队/限流拒绝帧：HTTP 200，失败信息在信封里。
const nestedRateLimitFrame = `data:{"headers":{"Content-Type":["application/json"]},"body":"{\"code\":\"403\",\"message\":\"{\\\"code\\\":\\\"10605\\\",\\\"message\\\":\\\"{\\\\\\\"isQueued\\\\\\\":true,\\\\\\\"modelKey\\\\\\\":\\\\\\\"dfmodel\\\\\\\",\\\\\\\"queueCount\\\\\\\":0,\\\\\\\"queueType\\\\\\\":\\\\\\\"p3\\\\\\\",\\\\\\\"retryAfterSeconds\\\\\\\":30,\\\\\\\"serviceAvailable\\\\\\\":false,\\\\\\\"subCode\\\\\\\":\\\\\\\"RESOURCE_LIMIT\\\\\\\",\\\\\\\"waitTime\\\\\\\":30}\\\",\\\"subCode\\\":\\\"RESOURCE_LIMIT\\\"}\"}","statusCodeValue":403,"statusCode":"FORBIDDEN"}` + "\n\n"

func TestNestedStreamBusinessErrorFailsFast(t *testing.T) {
	rec := httptest.NewRecorder()
	_, err := Stream(rec, strings.NewReader(nestedRateLimitFrame), "qodercom/deepseek-flash")
	if err == nil {
		t.Fatalf("信封内嵌 403 必须报错，不得被当普通 chunk 吞掉；输出=%s", rec.Body.String())
	}
	// 本仓口径（R109/R111）：10605/isQueued 是「模型排队」，比上游 R108 的 ErrSoftRate
	// 更具体——返回 *provider.Queued，handler 只冷「号×模型」，并在零字节阶段换号，
	// 客户端不会收到伪成功流。上游的通用信封判据仍保留（见 TestNestedStreamGenericEnvelopeError）。
	var q *provider.Queued
	if !errors.As(err, &q) {
		t.Fatalf("期望 *provider.Queued，实际 %T: %v", err, err)
	}
	if q.RetryAfter != 30*time.Second {
		t.Errorf("RetryAfter=%s 期望 30s（上游 retryAfterSeconds）", q.RetryAfter)
	}
	if q.WaitSeconds != 30 {
		t.Errorf("WaitSeconds=%d 期望 30（上游 waitTime）", q.WaitSeconds)
	}
	out := rec.Body.String()
	if strings.TrimSpace(out) != "" {
		t.Errorf("首帧即排队必须零字节返回（handler 才能换号）: %s", out)
	}
	// 核心回归：修复前这里会补一个 [DONE]，把失败包装成「空内容的成功流」。
	if strings.Contains(out, "[DONE]") {
		t.Fatalf("业务错误流不得补 [DONE]（R36）——补了就是伪造成功: %s", out)
	}
}

// TestNestedStreamErrorRetryAfterAbsent 上游未声明退避时长时不得编造该字段。
func TestNestedStreamErrorRetryAfterAbsent(t *testing.T) {
	in := `data:{"body":"{\"code\":\"400\",\"message\":\"invalid model\"}","statusCodeValue":400,"statusCode":"BAD_REQUEST"}` + "\n\n"
	rec := httptest.NewRecorder()
	_, err := Stream(rec, strings.NewReader(in), "qodercom/glm-5.3")
	var sErr *sseError
	if !errors.As(err, &sErr) {
		t.Fatalf("期望 *sseError，实际 err=%v", err)
	}
	if sErr.retryAfter != 0 {
		t.Errorf("上游未给退避时长，retryAfter 必须为 0，实际 %d", sErr.retryAfter)
	}
	if out := rec.Body.String(); strings.Contains(out, "retry_after_seconds") {
		t.Errorf("无依据不得输出 retry_after_seconds: %s", out)
	}
}

// TestStreamNormalStillExactlyOneDone 正向回归：正常多帧流仍恰好一个 [DONE]，
// 且正文里出现 RESOURCE_LIMIT / 10605 字样（模型在解释报错码）不得被误判成业务错误。
func TestStreamNormalStillExactlyOneDone(t *testing.T) {
	in := "data:" + `{"headers":{"Content-Type":["application/json"]},"body":"{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"RESOURCE_LIMIT 10605 只是正文文本\"}}]}","statusCodeValue":200,"statusCode":"OK"}` + "\n\n" +
		"data:" + `{"headers":{"Content-Type":["application/json"]},"body":"[DONE]","statusCodeValue":200,"statusCode":"OK"}` + "\n\n"
	rec := httptest.NewRecorder()
	if _, err := Stream(rec, strings.NewReader(in), "qodercom/deepseek-flash"); err != nil {
		t.Fatalf("正常流不应报错: %v", err)
	}
	out := rec.Body.String()
	if n := strings.Count(out, "[DONE]"); n != 1 {
		t.Fatalf("正常流应恰好一个 [DONE]，实际 %d 个: %s", n, out)
	}
	if strings.Contains(out, `"error"`) {
		t.Fatalf("正文含限流字样被误判成业务错误: %s", out)
	}
}

func TestClassifyResourceLimitIsSoftRate(t *testing.T) {
	// 带队列明细（10605 + isQueued）→ Queued：比 ErrSoftRate 更具体，冷却粒度是
	// 「号×模型」而不是整号（本仓 R109/R111 口径）。
	msg := `{"code":"10605","message":"{\"isQueued\":true,\"modelKey\":\"dfmodel\",\"retryAfterSeconds\":30,\"serviceAvailable\":false,\"subCode\":\"RESOURCE_LIMIT\"}","subCode":"RESOURCE_LIMIT"}`
	if got := Classify(403, msg); got != provider.ErrQueued {
		t.Errorf("Classify(403, 10605/isQueued)=%v 期望 ErrQueued", got)
	}
	// 只有 subCode.RESOURCE_LIMIT、没有队列明细 → 软限流（吸收上游 R108 判据）。
	if got := Classify(403, `{"subCode":"RESOURCE_LIMIT"}`); got != provider.ErrSoftRate {
		t.Errorf("Classify(403, 仅 RESOURCE_LIMIT)=%v 期望 ErrSoftRate", got)
	}
	if got := Classify(403, `{"code":"10605","serviceAvailable":false}`); got != provider.ErrQueued {
		t.Errorf("仅 10605 + serviceAvailable 期望 ErrQueued，实际 %v", got)
	}
}

// TestNestedStreamGenericEnvelopeError 吸收上游 R108：非队列/非额度的信封 4xx
// （纯文本 rate limit）仍要返回 *sseError，并在出口补 code + retry_after_seconds。
func TestNestedStreamGenericEnvelopeError(t *testing.T) {
	in := `data:{"body":"{\"code\":\"403\",\"message\":\"rate limit exceeded; {\\\"retryAfterSeconds\\\":30}\"}","statusCodeValue":403,"statusCode":"FORBIDDEN"}` + "\n\n"
	rec := httptest.NewRecorder()
	_, err := StreamCapture(rec, strings.NewReader(in), "qodercom/deepseek-flash", nil)
	var sErr *sseError
	if !errors.As(err, &sErr) {
		t.Fatalf("期望 *sseError，实际 %T: %v", err, err)
	}
	if got := sErr.Kind(); got != provider.ErrSoftRate {
		t.Errorf("Kind()=%v 期望 ErrSoftRate", got)
	}
	if sErr.retryAfter != 30 {
		t.Errorf("retryAfter=%d 期望 30（从文案里取的 retryAfterSeconds）", sErr.retryAfter)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"retry_after_seconds":30`) {
		t.Errorf("error 帧缺 retry_after_seconds，客户端只能盲重试: %s", out)
	}
	if strings.Contains(out, "[DONE]") {
		t.Errorf("业务错误流不得补 [DONE]: %s", out)
	}
}

// TestClassifyNotFooedByDigitsInIDs 负例：request_id/trace_id 里含 "10605" 五个数字
// 不得被判成限流（裸 Contains 的历史教训，与 11115 同源）。
func TestClassifyNotFooedByDigitsInIDs(t *testing.T) {
	body := `{"error":"boom","request_id":"a10605b","trace_id":"e75b73939da3c10605123"}`
	if got := Classify(403, body); got == provider.ErrSoftRate {
		t.Fatal("无关文本里的 10605 数字串被误判成限流")
	}
	if got := Classify(400, body); got == provider.ErrSoftRate {
		t.Fatal("无关文本里的 10605 数字串被误判成限流(400)")
	}
}
