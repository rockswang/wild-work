package workbuddyai

import (
	"net/http"
	"testing"

	"wild-work/internal/auth"
)

// TestChatHeadersCarryClientFingerprint 出站对话请求必须带官方客户端的标记头与机器指纹。
//
// 这是与国内版 internal/upstream/headers.go 的同构要求：上游用 X-Machine-ID /
// X-Session-ID 做客户端归属与限流计数，缺失时本渠道请求在网关侧与官方客户端不是
// 同一口径。2026-10-12 实测：缺失时国际版对话常在 2~3 个请求后即被上游
// 429 code=14003 限流，而同账号官方客户端正常。
func TestChatHeadersCarryClientFingerprint(t *testing.T) {
	var got http.Header
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer srv.Close()

	c := NewWithBase(srv.URL)
	if _, _, _, err := c.ChatStream(testAuth(), []byte(`{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`)); err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	for _, h := range []string{
		"X-CodeBuddy-Request", "X-Machine-ID", "X-Session-ID",
		"X-B3-TraceId", "X-B3-SpanId", "X-B3-Sampled", "Accept-Language",
	} {
		if got.Get(h) == "" {
			t.Errorf("缺失请求头 %s", h)
		}
	}
	if v := got.Get("X-CodeBuddy-Request"); v != "1" {
		t.Errorf("X-CodeBuddy-Request = %q, want 1", v)
	}
	if n := len(got.Get("X-Machine-ID")); n != 36 {
		t.Errorf("X-Machine-ID 长度 = %d, want 36", n)
	}
	// 会话 ID 与消息 ID 是两个独立值：早期实现把四者填成同一个值。
	if cid, mid := got.Get("X-Conversation-Request-ID"), got.Get("X-Conversation-Message-ID"); cid == mid {
		t.Errorf("会话 ID 与消息 ID 不应相同: %q", cid)
	}
	tid, sid := got.Get("X-B3-TraceId"), got.Get("X-B3-SpanId")
	if len(sid) != 16 || len(tid) != 32 || tid[:16] != sid {
		t.Errorf("B3 链路头异常: trace=%q span=%q", tid, sid)
	}
}

// TestMachineIDStablePerUID 同一 uid 必须派生同一个机器号。
// 若每次随机，上游会把每个请求都当成一台新设备，机器维度的计数永远从零开始——
// 限流阈值形同虚设，且更易被判异常。
func TestMachineIDStablePerUID(t *testing.T) {
	var headers []http.Header
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		headers = append(headers, r.Header.Clone())
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer srv.Close()

	c := NewWithBase(srv.URL)
	body := []byte(`{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`)
	a := testAuth()
	for i := 0; i < 2; i++ {
		if _, _, _, err := c.ChatStream(a, body); err != nil {
			t.Fatalf("ChatStream #%d: %v", i, err)
		}
	}
	other := &auth.Auth{Kind: "workbuddyai", AccessToken: "t", UID: "other-uid", Domain: "www.workbuddy.ai"}
	if _, _, _, err := c.ChatStream(other, body); err != nil {
		t.Fatalf("ChatStream(other): %v", err)
	}
	if len(headers) != 3 {
		t.Fatalf("want 3 requests, got %d", len(headers))
	}
	if headers[0].Get("X-Machine-ID") != headers[1].Get("X-Machine-ID") {
		t.Errorf("同一 uid 的机器号不稳定: %q vs %q",
			headers[0].Get("X-Machine-ID"), headers[1].Get("X-Machine-ID"))
	}
	if headers[2].Get("X-Machine-ID") == headers[0].Get("X-Machine-ID") {
		t.Errorf("不同 uid 派生出同一机器号: %q", headers[0].Get("X-Machine-ID"))
	}
}
