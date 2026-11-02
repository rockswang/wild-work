package server

import (
	"net/http/httptest"
	"testing"
	"time"
)

// TestStatWriterRecordsTTFB 首字节计一次即定（WriteHeader/Write/Flush 先到先记，不重记）。
func TestStatWriterRecordsTTFB(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statWriter{ResponseWriter: rec, start: time.Now()}
	time.Sleep(5 * time.Millisecond)
	sw.WriteHeader(200)
	if !sw.written {
		t.Fatal("WriteHeader 后应标记首字节")
	}
	if sw.ttfbMs < 4 {
		t.Fatalf("TTFB 应 ≥4ms（sleep 5ms），实际 %d", sw.ttfbMs)
	}
	first := sw.ttfbMs
	_, _ = sw.Write([]byte("x"))
	if sw.ttfbMs != first {
		t.Fatalf("首字节后不应重记：%d → %d", first, sw.ttfbMs)
	}
}

// TestStatWriterFlushCounts 流式场景：首个 Flush 即首字节时机（SSE 首帧先 flush 后有 body）。
func TestStatWriterFlushCounts(t *testing.T) {
	rec := httptest.NewRecorder() // ResponseRecorder 实现 http.Flusher
	sw := &statWriter{ResponseWriter: rec, start: time.Now()}
	time.Sleep(5 * time.Millisecond)
	sw.Flush()
	if !sw.written || sw.ttfbMs < 4 {
		t.Fatalf("Flush 应记首字节，实际 written=%v ttfbMs=%d", sw.written, sw.ttfbMs)
	}
}

// statWriter 按秒记录写帧数：诚实吞吐的原始数据源。
// 一次 SSE 帧 = 一次 Write（见 internal/upstream/sse.go 的 streamCore），
// 故「写帧数按秒分布」即「token 投递按秒分布」的代理量。
func TestStatWriterCountsChunksPerSecond(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := &statWriter{ResponseWriter: rec, start: time.Now()}
	sw.WriteHeader(200)
	for i := 0; i < 3; i++ {
		_, _ = sw.Write([]byte("data: {}\n\n"))
	}
	if len(sw.chunks) != 1 {
		t.Fatalf("同一秒内应有 1 个桶，实际 %d：%+v", len(sw.chunks), sw.chunks)
	}
	for _, c := range sw.chunks {
		if c != 3 {
			t.Fatalf("该秒应记 3 帧，实际 %d", c)
		}
	}
	// 空写不计数（心跳/空帧不是 token 投递）
	before := totalChunks(sw.chunks)
	_, _ = sw.Write(nil)
	if after := totalChunks(sw.chunks); after != before {
		t.Fatalf("空写不应计数：%d → %d", before, after)
	}
}

func totalChunks(m map[int64]int) int {
	n := 0
	for _, c := range m {
		n += c
	}
	return n
}

// TestUsageCredit 单次积分提取：WorkBuddy 系下发 float；comate 下发复数 `credits`；
// 缺失/非数值 → 0。
func TestUsageCredit(t *testing.T) {
	if got := usageCredit(map[string]any{"credit": 12.5}); got != 12.5 {
		t.Fatalf("应提取 12.5，实际 %v", got)
	}
	// comate 的真实形态：usage.credits（复数）—— 不接它则该渠道积分永远不入账（R130）。
	if got := usageCredit(map[string]any{"credits": 0.01}); got != 0.01 {
		t.Fatalf("应提取复数 credits 0.01，实际 %v", got)
	}
	if got := usageCredit(map[string]any{"prompt_tokens": 10.0}); got != 0 {
		t.Fatalf("无 credit 字段应 0，实际 %v", got)
	}
	if got := usageCredit(map[string]any{"credit": "12.5"}); got != 0 {
		t.Fatalf("非数值 credit 应 0，实际 %v", got)
	}
	if got := usageCredit(nil); got != 0 {
		t.Fatalf("nil usage 应 0，实际 %v", got)
	}
}
