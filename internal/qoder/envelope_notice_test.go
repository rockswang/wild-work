// envelope_notice_test.go 锁定 qoder（QoderWork）的「HTTP 200 里包着业务错误」帧识别。
//
// 三渠道（qoder / qodercn / qodercom）的 sse.go 是同源代码复制，本渠道此前也没有
// 帧识别 ⇒ 上游拒绝服务时会照常 200 开流、把错误塞进一帧没有 choices 的 envelope，
// 然后挂住不关连接：客户端只收到一个空 delta，界面停在「正在等待模型…」直到 180s
// 后网关掐断（qodercom 实测 180.2/180.4s，同一套外壳）。
//
// 本渠道**各写一份实现、不共享 qodercom 的代码**（R58：本仓渠道包历来不共享 SSE）。
package qoder

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

// TestQuotaExhaustedFrameIsHardCredit 额度耗尽帧（110/112/pricingUrl/isQuotaExceeded）
// 必须归 ErrHardCredit 且零字节写出。
func TestQuotaExhaustedFrameIsHardCredit(t *testing.T) {
	cases := map[string]string{
		"112 付费墙": mustJSON(t, map[string]any{
			"code":    "112",
			"message": mustJSON(t, map[string]any{"pricingUrl": "https://qoder.com/pricing?client=qoder"}),
		}),
		"110 当日用量超限": `{"code":"110","message":"Billing daily count exceeded"}`,
	}
	for name, inner := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			_, err := StreamCapture(rec, strings.NewReader(envelopeFrame(t, inner, 403, nil)), "deepseek-v4-pro", nil)

			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("额度耗尽帧应上抛 *provider.Error，实际 err=%v", err)
			}
			if pe.Kind != provider.ErrHardCredit {
				t.Fatalf("Kind = %v, want ErrHardCredit", pe.Kind)
			}
			if rec.Body.Len() != 0 {
				t.Fatalf("错误帧不得写出任何字节，实际 %d 字节: %s", rec.Body.Len(), rec.Body.String())
			}
		})
	}
}

// TestQueuedFrameIsQueued 排队帧走模型级冷却语义。
func TestQueuedFrameIsQueued(t *testing.T) {
	inner := mustJSON(t, map[string]any{
		"code": "10605",
		"message": mustJSON(t, map[string]any{
			"isQueued": true, "modelKey": "dfmodel", "queueCount": 512,
			"retryAfterSeconds": 20, "waitTime": 90,
		}),
	})
	rec := httptest.NewRecorder()
	_, err := StreamCapture(rec, strings.NewReader(envelopeFrame(t, inner, 403, nil)), "deepseek-v4-pro", nil)

	var q *provider.Queued
	if !errors.As(err, &q) {
		t.Fatalf("排队帧应上抛 *provider.Queued，实际 err=%v", err)
	}
	if q.QueueCount != 512 || q.RetryAfter != 20*time.Second || q.WaitSeconds != 90 {
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
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "在"}}},
	})
	rec := httptest.NewRecorder()
	_, err := StreamCapture(rec, strings.NewReader(envelopeFrame(t, chunk, 200, nil)), "deepseek-v4-pro", nil)
	if err != nil {
		t.Fatalf("正常帧不应报错: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"content":"在"`) {
		t.Fatalf("正常 delta 应透传，实际: %s", rec.Body.String())
	}
}

// TestClassifyQuotaCodes 4xx 直发的同类 body 也要归 ErrHardCredit。
func TestClassifyQuotaCodes(t *testing.T) {
	if got := Classify(403, `{"code":"112","message":"{}"}`); got != provider.ErrHardCredit {
		t.Fatalf("code 112 应归 ErrHardCredit，实际 %v", got)
	}
	if got := Classify(403, `{"code":"110","message":"Billing daily count exceeded"}`); got != provider.ErrHardCredit {
		t.Fatalf("code 110 应归 ErrHardCredit，实际 %v", got)
	}
}
