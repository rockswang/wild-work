// conversation_headers_test.go 锁死**国内版**（copilot.tencent.com）对话出站的会话头族。
//
// 背景（R116）：国内版此前完全不发 X-Conversation-ID —— 国际版 2026-10-16 起已按账号
// 灰度校验会话头族（缺即 400 / 11128 "conversation id missing"），国内版 10-20 夜里跟着
// 上了同一套渠道校验；国内版不能再单边裸奔（本渠道自持实现，见 convheaders.go，
// 不与国际版共用代码——渠道包之间不耦合是 R58/R111 的既定纪律）。
//
// 三层语义：会话级恒定 / 轮级一次 user send（重试复用）/ 调用级每请求新生成。
package upstream

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"wild-work/internal/auth"
)

const cnConvBody = `{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"s"},{"role":"user","content":"第一问"}]}`

// captureCNChat 跑一次真实 HTTP 链路（假上游），回传它收到的头。
func captureCNChat(t *testing.T, uid, body string) http.Header {
	t.Helper()
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"c","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{
		HTTP:        srv.Client(),
		ChatBaseCN:  srv.URL,
		IdleTimeout: time.Minute,
	}
	a := &auth.Auth{Kind: "workbuddy", AccessToken: "test-token", UID: uid}
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

// TestChatSendsConversationFamilyID 国内版对话必须带 X-Conversation-ID（R116 核心）。
func TestChatSendsConversationFamilyID(t *testing.T) {
	got := captureCNChat(t, "u1", cnConvBody)

	if cid := got.Get("X-Conversation-ID"); !uuidV4Re.MatchString(cid) {
		t.Fatalf("X-Conversation-ID 缺失或形态错：%q", cid)
	}
	turnID := got.Get("X-Conversation-Request-ID")
	msgID := got.Get("X-Conversation-Message-ID")
	if !hex32Re.MatchString(turnID) || !hex32Re.MatchString(msgID) {
		t.Fatalf("轮级/调用级 id 形态错：turn=%q msg=%q", turnID, msgID)
	}
	if turnID == msgID {
		t.Fatalf("轮级与调用级 id 绑成同一个值（R116 回归）")
	}
	if got.Get("X-Request-ID") != msgID {
		t.Fatalf("X-Request-ID 应等于调用级 id")
	}
	if got.Get("X-Root-Request-ID") != turnID {
		t.Fatalf("X-Root-Request-ID 应等于本轮 id：%q vs %q", got.Get("X-Root-Request-ID"), turnID)
	}
	// B3 是每请求链路头，跟调用级 id 走（形态不回归）。
	if got.Get("X-B3-TraceId") != msgID {
		t.Fatalf("X-B3-TraceId 应跟随调用级 id")
	}
	if got.Get("X-Refresh-Token") != "" {
		t.Fatalf("X-Refresh-Token 绝不允许出现在 chat 请求里")
	}
}

// TestChatConversationIDStableWithinSession 同会话多轮：会话 id 恒定，轮级/调用级变化。
func TestChatConversationIDStableWithinSession(t *testing.T) {
	first := captureCNChat(t, "u1", cnConvBody)
	secondTurn := `{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"s"},{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},{"role":"user","content":"第二问"}]}`
	second := captureCNChat(t, "u1", secondTurn)

	if first.Get("X-Conversation-ID") != second.Get("X-Conversation-ID") {
		t.Fatalf("换轮后会话 id 漂移：%q → %q", first.Get("X-Conversation-ID"), second.Get("X-Conversation-ID"))
	}
	if first.Get("X-Conversation-Request-ID") == second.Get("X-Conversation-Request-ID") {
		t.Fatalf("新一轮应换轮级 id")
	}
}

// TestChatConversationIDScopedByAccount 不同账号 / 不同会话不得共用会话 id（关联键）。
func TestChatConversationIDScopedByAccount(t *testing.T) {
	base := captureCNChat(t, "u1", cnConvBody)
	otherUser := captureCNChat(t, "u2", cnConvBody)
	otherSession := captureCNChat(t, "u1", `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"另一个问题"}]}`)

	if base.Get("X-Conversation-ID") == otherUser.Get("X-Conversation-ID") {
		t.Fatalf("不同账号不得共用会话 id")
	}
	if base.Get("X-Conversation-ID") == otherSession.Get("X-Conversation-ID") {
		t.Fatalf("不同会话不得共用会话 id")
	}
}

// TestChatConversationFamilyStableAcrossRetries 同 body 重发（网关内换号重试）：
// 会话级/轮级恒定，调用级新生成——上游按 Request-ID 计「一次 user send」。
func TestChatConversationFamilyStableAcrossRetries(t *testing.T) {
	one := captureCNChat(t, "u1", cnConvBody)
	two := captureCNChat(t, "u1", cnConvBody)

	if one.Get("X-Conversation-ID") != two.Get("X-Conversation-ID") {
		t.Fatalf("重试后会话 id 漂移")
	}
	if one.Get("X-Conversation-Request-ID") != two.Get("X-Conversation-Request-ID") {
		t.Fatalf("重试后轮级 id 漂移（上游会记成多次 user send）")
	}
	if one.Get("X-Conversation-Message-ID") == two.Get("X-Conversation-Message-ID") {
		t.Fatalf("重试后调用级 id 应新生成")
	}
}

// TestChatConversationFamilyFallbackRandom 抽不到锚点时退回随机，形态仍须合法。
func TestChatConversationFamilyFallbackRandom(t *testing.T) {
	one := captureCNChat(t, "u1", `{"model":"deepseek-v4.1-flash"}`)
	two := captureCNChat(t, "u1", `{"model":"deepseek-v4.1-flash"}`)
	for _, h := range []http.Header{one, two} {
		if !uuidV4Re.MatchString(h.Get("X-Conversation-ID")) {
			t.Fatalf("无锚点时会话 id 形态错：%q", h.Get("X-Conversation-ID"))
		}
	}
}

// TestConversationFamilyOffOtherEndpoints 爆炸半径：账单/刷新端点绝不带会话头族。
func TestConversationFamilyOffOtherEndpoints(t *testing.T) {
	a := &auth.Auth{Kind: "workbuddy", AccessToken: "at", RefreshToken: "rt", UID: "u1"}

	req, _ := http.NewRequest(http.MethodPost, "https://billing.example/x", nil)
	BillingHeaders(req, a)
	refresh, _ := http.NewRequest(http.MethodPost, "https://chat.example/x", nil)
	RefreshHeaders(refresh, a)

	for name, h := range map[string]http.Header{"billing": req.Header, "refresh": refresh.Header} {
		for _, k := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Root-Request-ID"} {
			if h.Get(k) != "" {
				t.Fatalf("%s 端点不得携带 %s", name, k)
			}
		}
	}
}
