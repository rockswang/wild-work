// queue_frame_test.go 锁定「上游排队帧」的识别语义（2026-10-20 生产事故）。
//
// 背景：qodercn deepseek-flash 在网关侧按模型排队，上游用 **HTTP 200 + 一帧业务错误**
// 下发排队提示（三层嵌套 JSON，内层 code 10605 / isQueued）。旧实现把它当普通 chunk
// 透传 ⇒ 客户端只有一个空 delta，界面停在「正在等待模型…」，直到网关 180s 后掐断
// （实测 180.3/180.7/180.8s 三次），零 token、零提示。
//
// 本文件锁定：
//  1. 排队帧必须在写出**任何字节之前**被识别并上抛 *provider.Queued；
//  2. 解析出的 retryAfter / queueCount / waitTime 必须准确（冷却时长据此决定）；
//  3. 非排队的错误帧（statusCode>=400）同样上抛，不许当 chunk 透传。
package qodercn

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/provider"
)

func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// queueFrame 构造与生产同形的排队帧（三层套娃 + 外层 headers 带 Retry-After）。
func queueFrame(t *testing.T) string {
	t.Helper()
	queue := mustMarshal(t, map[string]any{
		"isQueued": true, "modelKey": "dfmodel", "queueCount": 8407, "queueType": "p3",
		"retryAfterSeconds": 30, "serviceAvailable": true, "waitTime": 509,
	})
	lvl2 := mustMarshal(t, map[string]any{"code": "10605", "message": queue})
	lvl1 := mustMarshal(t, map[string]any{"code": "403", "message": lvl2})
	env := mustMarshal(t, map[string]any{
		"headers":         map[string][]string{"Content-Type": {"application/json"}, "Retry-After": {"30"}},
		"body":            lvl1,
		"statusCodeValue": 403,
		"statusCode":      "FORBIDDEN",
	})
	return "data:" + env + "\n\n"
}

func TestQueueFrameAbortsBeforeWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	_, err := StreamCapture(rec, strings.NewReader(queueFrame(t)), "deepseek-flash", nil)

	var q *provider.Queued
	if !errors.As(err, &q) {
		t.Fatalf("排队帧应上抛 *provider.Queued，实际 err=%v", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("排队帧不得写出任何字节（否则客户端会收到空 delta 干等），实际写了 %d 字节: %s",
			rec.Body.Len(), rec.Body.String())
	}
	if q.RetryAfter != 30*time.Second || q.QueueCount != 8407 || q.WaitSeconds != 509 || q.ModelKey != "dfmodel" {
		t.Fatalf("排队元数据解析错误: %+v", q)
	}
	// Kind() 让 handler 的 StreamErrorClassifier 分支能认出它是「排队」而非普通错误。
	if q.Kind() != provider.ErrQueued {
		t.Fatalf("Kind = %v, want ErrQueued", q.Kind())
	}
	if got := q.Cooldown(); got != 30*time.Second {
		t.Fatalf("冷却应取 retryAfter=30s，实际 %v", got)
	}
}

// TestQueueFrameRetryAfterHeaderOnly 上游只在外层 headers 给 Retry-After 时也要认。
func TestQueueFrameRetryAfterHeaderOnly(t *testing.T) {
	queue := mustMarshal(t, map[string]any{"isQueued": true, "modelKey": "dfmodel", "queueCount": 12})
	lvl1 := mustMarshal(t, map[string]any{"code": "10605", "message": queue})
	env := mustMarshal(t, map[string]any{
		"headers":         map[string][]string{"Retry-After": {"45"}},
		"body":            lvl1,
		"statusCodeValue": 403,
	})
	rec := httptest.NewRecorder()
	_, err := StreamCapture(rec, strings.NewReader("data:"+env+"\n\n"), "m", nil)

	var q *provider.Queued
	if !errors.As(err, &q) || q.RetryAfter != 45*time.Second {
		t.Fatalf("应从外层 Retry-After 取 45s，err=%v", err)
	}
}

// TestEnvelopeErrorFrameNotForwarded HTTP 200 里包着的普通业务错误帧（非排队）
// 同样没有 choices，当 chunk 透传只会让客户端干等 ⇒ 必须上抛按状态码分类的错误。
func TestEnvelopeErrorFrameNotForwarded(t *testing.T) {
	lvl1 := mustMarshal(t, map[string]any{"code": "500", "message": "model provider is temporarily unavailable"})
	env := mustMarshal(t, map[string]any{"body": lvl1, "statusCodeValue": 500})

	rec := httptest.NewRecorder()
	_, err := StreamCapture(rec, strings.NewReader("data:"+env+"\n\n"), "m", nil)

	// 合并后口径：通用信封错误由 sseError 承接（带 Kind/退避秒数），出口补一帧
	// 结构化 error；不得把原始信封当 chunk 透传，也不得补 [DONE]（R36/R108）。
	var sErr *sseError
	if !errors.As(err, &sErr) {
		t.Fatalf("业务错误帧应上抛 *sseError，实际 %T: %v", err, err)
	}
	if sErr.status != 500 {
		t.Errorf("status=%d 期望 500", sErr.status)
	}
	out := rec.Body.String()
	if strings.Contains(out, `"delta"`) || strings.Contains(out, "[DONE]") {
		t.Fatalf("错误帧不得被当 chunk 透传、也不得补 [DONE]: %s", out)
	}
	if !strings.Contains(out, `"error"`) {
		t.Fatalf("应补一帧结构化 error 让客户端看到原因: %s", out)
	}
}

// TestNormalChunkStillForwarded 正常 delta（带 choices）不受影响。
func TestNormalChunkStillForwarded(t *testing.T) {
	chunk := mustMarshal(t, map[string]any{
		"id": "c1", "object": "chat.completion.chunk", "model": "auto",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "二"}}},
	})
	env := mustMarshal(t, map[string]any{"body": chunk})
	rec := httptest.NewRecorder()
	usage, err := StreamCapture(rec, strings.NewReader("data:"+env+"\n\ndata:{\"body\":\"[DONE]\"}\n\n"), "deepseek-flash", nil)
	if err != nil {
		t.Fatalf("正常帧不应报错: %v", err)
	}
	if usage != nil {
		t.Logf("usage=%v", usage)
	}
	if !strings.Contains(rec.Body.String(), `"content":"二"`) || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("正常 delta 应透传并以 [DONE] 收尾，实际: %s", rec.Body.String())
	}
}

// TestClassifyQueueBody 上游若改用 4xx + 排队 body（另一种下发形态），
// Classify 也必须归到 ErrQueued，否则会被当普通 403/429 罚整号。
func TestClassifyQueueBody(t *testing.T) {
	body := `{"code":"403","message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":true}\"}"}`
	if got := Classify(403, body); got != provider.ErrQueued {
		t.Fatalf("10605 排队体应归 ErrQueued，实际 %v", got)
	}
	if got := Classify(403, "blocked by security policy"); got == provider.ErrQueued {
		t.Fatalf("普通 403 不应误判为排队，实际 %v", got)
	}
}

// TestQuotaExhaustedFrameIsHardCredit 额度耗尽帧（110/112/pricingUrl/isQuotaExceeded）
// 必须归 ErrHardCredit 且零字节写出（2026-10-20 qodercom 同族实测，本渠道代码同源一并补）。
func TestQuotaExhaustedFrameIsHardCredit(t *testing.T) {
	cases := map[string]string{
		"112 付费墙": mustMarshal(t, map[string]any{
			"code":    "112",
			"message": mustMarshal(t, map[string]any{"pricingUrl": "https://qoder.com/pricing?client=qoder"}),
		}),
		"110 当日用量超限": `{"code":"110","message":"Billing daily count exceeded"}`,
	}
	for name, inner := range cases {
		t.Run(name, func(t *testing.T) {
			env := mustMarshal(t, map[string]any{"body": inner, "statusCodeValue": 403})
			rec := httptest.NewRecorder()
			_, err := StreamCapture(rec, strings.NewReader("data:"+env+"\n\n"), "deepseek-flash", nil)

			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("额度耗尽帧应上抛 *provider.Error，实际 err=%v", err)
			}
			if pe.Kind != provider.ErrHardCredit {
				t.Fatalf("Kind = %v, want ErrHardCredit（账号级额度耗尽）", pe.Kind)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("错误帧不得写出任何字节，实际 %d 字节: %s", rec.Body.Len(), rec.Body.String())
			}
		})
	}
}
