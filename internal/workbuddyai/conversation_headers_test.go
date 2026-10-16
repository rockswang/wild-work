// conversation_headers_test.go 锁死对话出站的会话头族契约。
// 背景：2026-10-16 起国际版上游新增渠道校验（抽样→按账号灰度必拦），缺
// X-Conversation-ID 的请求被判定「非官方客户端」，返回 400 code=11128
// "conversation id missing"。定位与 A/B 实证见 ardeyouxipianyi/
// workbuddy2api-hub#380（修复 PR #381）、CangShui/workbuddy-gateway#20。
package workbuddyai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// convIDPattern 官方客户端抓包形态：带连字符 UUID（v4）。抓包里 v4/v7 均
// 被上游接受（#381 实测版本号不参与校验），本仓统一 v4。
var convIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// TestConversationIDWBAIShape 形态 + 唯一性：必须是合法 UUIDv4，且随机性成立。
func TestConversationIDWBAIShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := conversationIDWBAI()
		if len(id) != 36 {
			t.Fatalf("conversation id length = %d, want 36: %q", len(id), id)
		}
		if !convIDPattern.MatchString(id) {
			t.Fatalf("conversation id not UUIDv4 shape: %q", id)
		}
		seen[id] = true
	}
	if len(seen) != 64 {
		t.Fatalf("conversation ids not unique: %d unique of 64", len(seen))
	}
}

// TestChatStreamSendsConversationID 端到端（真实 httptest 链路）：对话出站
// 必须带 X-Conversation-ID，且既有会话头族与「绝不带 X-Refresh-Token」红线
// 不回归。缺 X-Conversation-ID 即上游 11128 conversation id missing。
func TestChatStreamSendsConversationID(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c","model":"hy3","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	c.IdleTimeout = time.Minute
	a := &auth.Auth{Kind: "workbuddyai", AccessToken: "test-token", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"hy3","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("ChatStream transport error: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	_, _ = io.Copy(io.Discard, rc)
	rc.Close()

	if cid := got.Get("X-Conversation-ID"); !convIDPattern.MatchString(cid) {
		t.Fatalf("X-Conversation-ID missing or wrong shape: %q", cid)
	}
	// 风控前已带的头族，回归保护。
	for _, h := range []string{"X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Request-ID", "X-Root-Request-ID"} {
		if got.Get(h) == "" {
			t.Fatalf("missing session-family header %s", h)
		}
	}
	// 安全红线（chatHeaders 注释约定）：对话请求绝不携带 refresh token。
	if got.Get("X-Refresh-Token") != "" {
		t.Fatalf("X-Refresh-Token must never be sent on chat requests")
	}
}
