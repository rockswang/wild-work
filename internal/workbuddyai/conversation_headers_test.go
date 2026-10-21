// conversation_headers_test.go 锁死对话出站的会话头族契约。
//
// 背景：2026-10-16 起国际版上游新增渠道校验（抽样→按账号灰度必拦），缺
// X-Conversation-ID 的请求被判定「非官方客户端」，返回 400 code=11128
// "conversation id missing"。定位与 A/B 实证见 ardeyouxipianyi/
// workbuddy2api-hub#380（修复 PR #381）、CangShui/workbuddy-gateway#20。
//
// 2026-10-21（R116）起四个头按官方三层语义分层，不再绑成同一个值：
//
//	会话级 X-Conversation-ID          同会话恒定
//	轮级   X-Conversation-Request-ID  一次 user send 一个（重试/换号复用）
//	调用级 X-Conversation-Message-ID  每次上游请求新生成（= X-Request-ID）
//	       X-Root-Request-ID          = 本轮 Request-ID
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

// hex32Pattern 调用级/轮级 id 形态：32 位无连字符 hex。
var hex32Pattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

const convBody = `{"model":"hy3","messages":[{"role":"system","content":"s"},{"role":"user","content":"第一问"}]}`

// captureChat 起一个假上游，跑一次 ChatStream 并回传它收到的头。
func captureChat(t *testing.T, uid string, body string) http.Header {
	t.Helper()
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
	a := &auth.Auth{Kind: "workbuddyai", AccessToken: "test-token", UID: uid}
	rc, status, _, err := c.ChatStream(a, []byte(body))
	if err != nil {
		t.Fatalf("ChatStream transport error: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	_, _ = io.Copy(io.Discard, rc)
	rc.Close()
	return got
}

// TestConversationFamilyShape 形态与三层绑定关系。
func TestConversationFamilyShape(t *testing.T) {
	got := captureChat(t, "u1", convBody)

	convID := got.Get("X-Conversation-ID")
	if !convIDPattern.MatchString(convID) {
		t.Fatalf("X-Conversation-ID 缺失或形态错：%q", convID)
	}
	turnID := got.Get("X-Conversation-Request-ID")
	msgID := got.Get("X-Conversation-Message-ID")
	if !hex32Pattern.MatchString(turnID) {
		t.Fatalf("X-Conversation-Request-ID 形态错：%q", turnID)
	}
	if !hex32Pattern.MatchString(msgID) {
		t.Fatalf("X-Conversation-Message-ID 形态错：%q", msgID)
	}
	if turnID == msgID {
		t.Fatalf("轮级与调用级 id 绑成了同一个值（R116 回归）：%q", turnID)
	}
	// 官方语义：X-Request-ID = Message-ID；X-Root-Request-ID = 本轮 Request-ID。
	if got.Get("X-Request-ID") != msgID {
		t.Fatalf("X-Request-ID 应等于 X-Conversation-Message-ID：%q vs %q", got.Get("X-Request-ID"), msgID)
	}
	if got.Get("X-Root-Request-ID") != turnID {
		t.Fatalf("X-Root-Request-ID 应等于本轮 X-Conversation-Request-ID：%q vs %q", got.Get("X-Root-Request-ID"), turnID)
	}
	// 安全红线（chatHeaders 注释约定）：对话请求绝不携带 refresh token。
	if got.Get("X-Refresh-Token") != "" {
		t.Fatalf("X-Refresh-Token must never be sent on chat requests")
	}
}

// TestConversationIDStableWithinSession 同一会话多轮：会话 id 恒定、轮 id 变化。
func TestConversationIDStableWithinSession(t *testing.T) {
	first := captureChat(t, "u1", convBody)
	secondTurn := `{"model":"hy3","messages":[{"role":"system","content":"s"},{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},{"role":"user","content":"第二问"}]}`
	second := captureChat(t, "u1", secondTurn)

	if first.Get("X-Conversation-ID") != second.Get("X-Conversation-ID") {
		t.Fatalf("同一会话换了轮次后会话 id 漂移：%q → %q",
			first.Get("X-Conversation-ID"), second.Get("X-Conversation-ID"))
	}
	if first.Get("X-Conversation-Request-ID") == second.Get("X-Conversation-Request-ID") {
		t.Fatalf("新一轮应有新的轮级 id")
	}
	if first.Get("X-Conversation-Message-ID") == second.Get("X-Conversation-Message-ID") {
		t.Fatalf("每次上游请求应有新的调用级 id")
	}
}

// TestConversationIDSessionScoped 不同会话 / 不同账号不得共用会话 id（那是关联键）。
func TestConversationIDSessionScoped(t *testing.T) {
	otherSession := `{"model":"hy3","messages":[{"role":"user","content":"完全不同的问题"}]}`
	base := captureChat(t, "u1", convBody)
	diff := captureChat(t, "u1", otherSession)
	otherUser := captureChat(t, "u2", convBody)

	if base.Get("X-Conversation-ID") == diff.Get("X-Conversation-ID") {
		t.Fatalf("不同会话不得共用会话 id")
	}
	if base.Get("X-Conversation-ID") == otherUser.Get("X-Conversation-ID") {
		t.Fatalf("不同账号不得共用会话 id")
	}
}

// TestConversationFamilyStableAcrossRetries 同一 body 重发（网关内重试/换号）：
// 会话级与轮级恒定，调用级必须新生成——上游按 Request-ID 计「一次 user send」。
func TestConversationFamilyStableAcrossRetries(t *testing.T) {
	one := captureChat(t, "u1", convBody)
	two := captureChat(t, "u1", convBody)

	if one.Get("X-Conversation-ID") != two.Get("X-Conversation-ID") {
		t.Fatalf("重试后会话 id 漂移")
	}
	if one.Get("X-Conversation-Request-ID") != two.Get("X-Conversation-Request-ID") {
		t.Fatalf("重试后轮级 id 漂移（上游会记成多次 user send）：%q → %q",
			one.Get("X-Conversation-Request-ID"), two.Get("X-Conversation-Request-ID"))
	}
	if one.Get("X-Conversation-Message-ID") == two.Get("X-Conversation-Message-ID") {
		t.Fatalf("重试后调用级 id 应新生成")
	}
}

// TestConversationIDFallbackRandom 抽不到锚点（无 messages）时退回随机 v4，
// 形态仍须合法——旧行为是每请求随机，退路不能变成非法头。
func TestConversationIDFallbackRandom(t *testing.T) {
	noAnchor := `{"model":"hy3"}`
	one := captureChat(t, "u1", noAnchor)
	two := captureChat(t, "u1", noAnchor)
	for _, h := range []http.Header{one, two} {
		if !convIDPattern.MatchString(h.Get("X-Conversation-ID")) {
			t.Fatalf("无锚点时会话 id 形态错：%q", h.Get("X-Conversation-ID"))
		}
		if !hex32Pattern.MatchString(h.Get("X-Conversation-Request-ID")) {
			t.Fatalf("无锚点时轮级 id 形态错：%q", h.Get("X-Conversation-Request-ID"))
		}
	}
}
