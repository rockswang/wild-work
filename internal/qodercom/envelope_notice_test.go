// envelope_notice_test.go 锁定 qodercom 的「HTTP 200 里包着业务错误」帧识别。
//
// 生产事故（2026-10-20 21:09/21:12）：qodercom/deepseek-flash 请求卡死 180.2/180.4s，
// 日志 err=upstream stream truncated: unexpected EOF，客户端停在「正在等待模型…」。
// 抓上游原始帧（单发实测）：
//
//	data:{"headers":{"Content-Type":["application/json"]},
//	      "body":"{\"code\":\"112\",\"message\":\"{\\\"pricingUrl\\\":\\\"https://qoder.com/pricing?client=qoder\\\"}\"}",
//	      "statusCodeValue":403,"statusCode":"FORBIDDEN"}
//
// 另一形态是 code 110「Billing daily count exceeded」。两者都是**账号级额度耗尽**，
// 证据链：① A/B 单发（同号同 body，只换模型）deepseek-flash 与 glm-5.3-flash **都回 112**
// ⇒ 不是模型级；② 同时刻 /api/v2/quota/usage 五个号全部 isQuotaExceeded=true、remaining=0。
//
// 本文件锁定：
//  1. 110/112/pricingUrl/isQuotaExceeded → ErrHardCredit（整号硬冷却 + 换号），不是排队；
//  2. 10605/isQueued → *provider.Queued（只冷「号×模型」）；
//  3. 这类帧必须在写出任何字节之前上抛（否则客户端只收到空 delta）；
//  4. 正常 delta（带 choices）不受影响。
package qodercom

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/provider"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

// envelopeFrame 构造与生产同形的 envelope 帧：外层 200 外壳 + 内层业务错误字符串。
func envelopeFrame(t *testing.T, inner string, statusCode int, headers map[string][]string) string {
	t.Helper()
	env := mustJSON(t, map[string]any{
		"headers":         headers,
		"body":            inner,
		"statusCodeValue": statusCode,
		"statusCode":      "FORBIDDEN",
	})
	return "data:" + env + "\n\n"
}

// TestQuotaExhaustedFrameIsHardCredit code 112（付费墙）/ 110（当日用量超限）
// 必须归 ErrHardCredit，且零字节写出。
func TestQuotaExhaustedFrameIsHardCredit(t *testing.T) {
	cases := map[string]string{
		"112 付费墙": mustJSON(t, map[string]any{
			"code":    "112",
			"message": mustJSON(t, map[string]any{"pricingUrl": "https://qoder.com/pricing?client=qoder"}),
		}),
		"110 当日用量超限":         `{"code":"110","message":"Billing daily count exceeded"}`,
		"isQuotaExceeded 标记": `{"code":"403","message":"{\"isQuotaExceeded\":true}"}`,
	}
	for name, inner := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			_, err := StreamCapture(rec, strings.NewReader(envelopeFrame(t, inner, 403, nil)), "deepseek-flash", nil)

			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("额度耗尽帧应上抛 *provider.Error，实际 err=%v", err)
			}
			if pe.Kind != provider.ErrHardCredit {
				t.Fatalf("Kind = %v, want ErrHardCredit（账号级额度耗尽，不是排队）", pe.Kind)
			}
			if !pe.Kind.Rotatable() {
				t.Fatal("ErrHardCredit 必须可换号")
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("错误帧不得写出任何字节（否则客户端拿到空 delta 干等 180s），实际写了 %d 字节: %s",
					rec.Body.Len(), rec.Body.String())
			}
		})
	}
}

// TestQueuedFrameIsQueued 10605/isQueued 走排队语义：只冷「号×模型」，不是额度耗尽。
func TestQueuedFrameIsQueued(t *testing.T) {
	inner := mustJSON(t, map[string]any{
		"code": "10605",
		"message": mustJSON(t, map[string]any{
			"isQueued": true, "modelKey": "dfmodel", "queueCount": 8407,
			"retryAfterSeconds": 30, "waitTime": 509,
		}),
	})
	rec := httptest.NewRecorder()
	_, err := StreamCapture(rec, strings.NewReader(envelopeFrame(t, inner, 403, map[string][]string{"Retry-After": {"30"}})),
		"deepseek-flash", nil)

	var q *provider.Queued
	if !errors.As(err, &q) {
		t.Fatalf("排队帧应上抛 *provider.Queued，实际 err=%v", err)
	}
	if q.Kind() != provider.ErrQueued {
		t.Fatalf("Kind = %v, want ErrQueued", q.Kind())
	}
	if q.QueueCount != 8407 || q.RetryAfter != 30*time.Second || q.WaitSeconds != 509 {
		t.Fatalf("排队元数据解析错误: %+v", q)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("排队帧不得写出任何字节，实际 %d 字节", rec.Body.Len())
	}
}

// TestNormalChunkStillForwarded 带 choices 的正常 delta 不受影响。
func TestNormalChunkStillForwarded(t *testing.T) {
	chunk := mustJSON(t, map[string]any{
		"id": "c1", "object": "chat.completion.chunk",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "你好"}}},
	})
	rec := httptest.NewRecorder()
	_, err := StreamCapture(rec, strings.NewReader(envelopeFrame(t, chunk, 200, nil)), "deepseek-flash", nil)
	if err != nil {
		t.Fatalf("正常帧不应报错: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"content":"你好"`) {
		t.Fatalf("正常 delta 应透传，实际: %s", rec.Body.String())
	}
}

// TestClassifyQuotaCodes 4xx 直发的同类 body 也要归 ErrHardCredit（不只是 envelope 形态）。
func TestClassifyQuotaCodes(t *testing.T) {
	if got := Classify(403, `{"code":"112","message":"{}"}`); got != provider.ErrHardCredit {
		t.Fatalf("code 112 应归 ErrHardCredit，实际 %v", got)
	}
	if got := Classify(403, `{"code":"110","message":"Billing daily count exceeded"}`); got != provider.ErrHardCredit {
		t.Fatalf("code 110 应归 ErrHardCredit，实际 %v", got)
	}
	if got := Classify(403, "blocked by security policy"); got != provider.ErrContentBlocked {
		t.Fatalf("普通内容拦截不应被误判，实际 %v", got)
	}
}
