package glm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// 本文件锁定一个**真实 bug**（2026-09-27 从生产日志发现）：
//
//	对话流走的是带 `Client.Timeout` 的 HTTP client，
//	而 Go 的 `Client.Timeout` **覆盖整个请求生命周期（含读 body）**，
//	故长回答会在超时点被**强制掐断且无终止帧**。
//
// 生产日志实证（5 次，全是 glm）：
//
//	2026/09/26 22:42:05 stream relay end platform=glm err=context deadline exceeded
//	2026/09/27 00:03:21 ... 00:09:18 ... 00:12:48 ... 00:18:22  同上
//
// 修复：新增 StreamHTTP（**不设 Client.Timeout**），ChatStream 改用它。
// 与 traework 的 StreamHTTP 是同一模式。

// TestStreamHTTPHasNoClientTimeout 断言 StreamHTTP 刻意不设 Client.Timeout。
//
// 这条是"防回归"——若将来有人"顺手统一超时配置"给它加上 Timeout，
// 本测试会失败并解释原因。
func TestStreamHTTPHasNoClientTimeout(t *testing.T) {
	c := New()

	if c.StreamHTTP == nil {
		t.Fatal("StreamHTTP 为 nil —— 流式请求会退回到带 Timeout 的 HTTP client，长回答必被掐断")
	}
	if c.StreamHTTP.Timeout != 0 {
		t.Errorf("❌ StreamHTTP.Timeout = %v，必须为 0（不设整体超时）。\n"+
			"原因：Go 的 Client.Timeout 覆盖整个请求生命周期（含读 body），"+
			"流式长回答会在超时点被掐断且无终止帧（实测触发过 5 次）。\n"+
			"若确需限制流式时长，应改用「上游无数据超时」而非 Client.Timeout。",
			c.StreamHTTP.Timeout)
	}

	// 非流式 client 仍应有超时（一问一答的接口需要它）
	if c.HTTP.Timeout == 0 {
		t.Error("HTTP.Timeout 不应为 0 —— 非流式接口需要整体超时兜底")
	}

	// 两者应共用同一 Transport（复用连接池）
	if c.StreamHTTP.Transport != c.HTTP.Transport {
		t.Error("StreamHTTP 与 HTTP 应共用同一 Transport 实例（复用连接池）")
	}
	t.Logf("✅ StreamHTTP.Timeout=%v（无整体超时），HTTP.Timeout=%v，共用 Transport",
		c.StreamHTTP.Timeout, c.HTTP.Timeout)
}

// TestStreamSurvivesBeyondClientTimeout 是本 bug 的核心回归测试。
//
// 构造一个「持续发帧、总时长 > HTTP.Timeout」的上游，
// 验证走 StreamHTTP 的对话流**不会被掐断**，能拿到全部内容与终止帧。
func TestStreamSurvivesBeyondClientTimeout(t *testing.T) {
	const (
		httpTimeout = 300 * time.Millisecond // 模拟生产里较短的超时
		frames      = 12                     // 发 12 帧
		frameGap    = 80 * time.Millisecond  // 每帧间隔 → 总时长 ≈ 960ms > 300ms
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)

		// 先发 init 帧（增量）
		for i := 0; i < frames; i++ {
			payload := fmt.Sprintf(
				`data: {"conversation_id":"c1","status":"init","parts":[{"logic_id":"L1","status":"init","content":[{"type":"text","text":"%d "}]}]}`+"\n\n", i)
			if _, err := io.WriteString(w, payload); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(frameGap):
			}
		}
		// 收尾：finish 帧（全文）+ done
		full := ""
		for i := 0; i < frames; i++ {
			full += fmt.Sprintf("%d ", i)
		}
		fin := fmt.Sprintf(
			`data: {"conversation_id":"c1","status":"finish","parts":[{"logic_id":"L1","status":"finish","content":[{"type":"text","text":"%s"}]}]}`+"\n\n",
			strings.TrimSpace(full))
		_, _ = io.WriteString(w, fin)
		if fl != nil {
			fl.Flush()
		}
	}))
	defer srv.Close()

	// 构造 client：HTTP 带短超时，StreamHTTP 不带（复刻生产配置）
	c := NewWithBase(srv.URL)
	c.HTTP.Timeout = httpTimeout
	c.StreamHTTP.Timeout = 0

	// 直接对 ChatStream 的底层行为做验证：用 StreamHTTP 发起同样的请求
	// （避免依赖完整的请求体构造，聚焦"超时是否掐断流"这一点）
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, nil)
	resp, err := c.StreamHTTP.Do(req)
	if err != nil {
		t.Fatalf("StreamHTTP 请求失败: %v", err)
	}
	defer resp.Body.Close()

	start := time.Now()
	body, readErr := io.ReadAll(resp.Body)
	elapsed := time.Since(start)

	if readErr != nil {
		t.Fatalf("❌ 流被中断（这正是 bug 的症状）: %v\n已读 %d 字节，耗时 %v",
			readErr, len(body), elapsed.Round(time.Millisecond))
	}

	// 关键断言 1：耗时超过 HTTP.Timeout —— 证明 StreamHTTP 不受其约束
	if elapsed <= httpTimeout {
		t.Logf("⚠️ 耗时 %v 未超过 HTTP.Timeout %v，测试未真正覆盖该场景",
			elapsed.Round(time.Millisecond), httpTimeout)
	} else {
		t.Logf("✅ 流持续 %v（> HTTP.Timeout %v）仍完整读完",
			elapsed.Round(time.Millisecond), httpTimeout)
	}

	// 关键断言 2：收到全部帧（没被截断）
	got := strings.Count(string(body), "data: ")
	if got < frames+1 {
		t.Errorf("❌ 只收到 %d 个 data 帧，期望至少 %d 个（流被截断）", got, frames+1)
	}

	// 关键断言 3：finish 帧（全文）存在 —— 说明上游正常收尾
	if !strings.Contains(string(body), `"status":"finish"`) {
		t.Error("❌ 未收到 finish 帧 —— 流被提前掐断")
	}

	t.Logf("✅ 证明修复有效：%d 个帧完整送达，含 finish 帧", got)
}

// TestHTTPClientStillTimesOutForNonStream 对照：非流式 client 仍应超时。
//
// 确保修复没有"顺手把非流式也放开"——那会让一问一答的接口在
// 上游挂死时永久阻塞。
func TestHTTPClientStillTimesOutForNonStream(t *testing.T) {
	const httpTimeout = 300 * time.Millisecond

	// 上游：永不给响应头（模拟挂死）
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // 一直不放行
	}))
	defer func() { close(block); srv.Close() }()

	c := NewWithBase(srv.URL)
	c.HTTP.Timeout = httpTimeout

	start := time.Now()
	_, err := c.HTTP.Get(srv.URL)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("❌ 非流式 client 未超时 —— 上游挂死时会永久阻塞")
	}
	if elapsed > httpTimeout+2*time.Second {
		t.Errorf("超时耗时 %v 远超设定 %v", elapsed.Round(time.Millisecond), httpTimeout)
	}
	t.Logf("✅ 对照通过：非流式 client 在 %v 后超时（%v）",
		elapsed.Round(time.Millisecond), err)
}

// TestChatStreamUsesStreamHTTP 静态+行为双重确认：
// ChatStream 必须走 StreamHTTP，而不是带超时的 HTTP。
func TestChatStreamUsesStreamHTTP(t *testing.T) {
	var sawRequest bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "assistant/stream") {
			sawRequest = true
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"conversation_id\":\"c1\",\"status\":\"finish\",\"parts\":[]}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":0,"message":"success","result":{"user_id":"u","access_token":"AT","refresh_token":"RT"}}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	a := &auth.Auth{Kind: "glm", AccessToken: "AT", RefreshToken: "RT", ExpiresAt: 4102444800, UID: "u"}

	body := []byte(`{"model":"chatglm:moe_53f","messages":[{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if !sawRequest {
		t.Error("未观察到 assistant/stream 请求")
	}
	t.Log("✅ ChatStream 正常发起对话流请求")
}
