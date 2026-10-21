package workbuddyai

import (
	"net/http"
	"testing"
)

// TestChatHeadersCarryClientFingerprint 出站对话请求必须带官方桌面客户端的身份链。
//
// 判据随上游 R117（2026-10-21）整体换过一轮：旧实现自造 X-Machine-ID / X-Session-ID
// 并沿用 CodeBuddy CLI 时代的 UA 与 X-IDE-Version 5.5.4，而官方桌面客户端根本不发
// 前者、后两者也分属不同代次——「混合身份」本身就是反作弊视角下的特征。
// 现行口径（对本机 WorkBuddy.app 5.8.3 静态解包取证）：UA / X-IDE-Version / X-Product /
// X-Private-Data / X-Agent-Intent 必须**整串同代**，且**不得**再出现自造头。
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
	// 必须存在：基础标记 + 归属头 + 会话头族
	for _, h := range []string{
		"X-CodeBuddy-Request", "Accept-Language",
		"X-Agent-Intent", "X-Agent-Purpose", "X-IDE-Name", "X-IDE-Type",
		"X-IDE-Version", "X-Product", "X-Private-Data",
		"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID",
		"X-Request-ID", "X-Root-Request-ID",
	} {
		if got.Get(h) == "" {
			t.Errorf("缺失请求头 %s", h)
		}
	}
	// 官方桌面客户端的取值
	if v := got.Get("X-IDE-Version"); v != clientVersion {
		t.Errorf("X-IDE-Version = %q, want %q（桌面版本，不是 CLI 版本）", v, clientVersion)
	}
	if v := got.Get("X-Product"); v != "SaaS" {
		t.Errorf("X-Product = %q, want SaaS（product.json 的 deploymentType）", v)
	}
	if v := got.Get("X-Private-Data"); v != "true" {
		t.Errorf("X-Private-Data = %q, want true", v)
	}
	// 自造头必须绝迹（R117）：官方客户端不发，混进去反而是特征。
	for _, h := range []string{"X-Machine-ID", "X-Session-ID"} {
		if v := got.Get(h); v != "" {
			t.Errorf("不应再发送自造指纹头 %s = %q（R117 已删除）", h, v)
		}
	}
	// 三层语义：会话 ID 带连字符 UUID，轮/调用 ID 是 32 hex，且互不相同。
	cid := got.Get("X-Conversation-ID")
	if len(cid) != 36 {
		t.Errorf("X-Conversation-ID 应为带连字符 UUID（36 字符），实际 %q", cid)
	}
	turnID := got.Get("X-Conversation-Request-ID")
	msgID := got.Get("X-Conversation-Message-ID")
	if len(turnID) != 32 || len(msgID) != 32 {
		t.Errorf("轮/调用 ID 应为 32 hex: turn=%q msg=%q", turnID, msgID)
	}
	if turnID == msgID {
		t.Errorf("轮 ID 与调用 ID 不应相同: %q", turnID)
	}
	if got.Get("X-Root-Request-ID") != turnID {
		t.Errorf("X-Root-Request-ID 应等于本轮 Request-ID")
	}
	if got.Get("X-Request-ID") != msgID {
		t.Errorf("X-Request-ID 应等于调用 ID")
	}
}

// TestConversationIDStablePerSession 会话级 ID 必须按（账号 + 会话锚点）稳定：
// 同一段多轮对话在官方客户端眼里是一个会话，每请求换一个等于把一次对话拆成上千个
// 一次性会话（hub#381 指出的最明显非人类形态）。轮/调用 ID 则各自刷新。
func TestConversationIDStablePerSession(t *testing.T) {
	body := []byte(`{"model":"hy3","messages":[{"role":"user","content":"first"},{"role":"assistant","content":"ok"},{"role":"user","content":"second"}]}`)
	uid := testAuth().UID

	if a, b := conversationID(uid, "anchor-x"), conversationID(uid, "anchor-x"); a != b {
		t.Errorf("同一会话锚点的会话 ID 应稳定: %q vs %q", a, b)
	}
	if a, b := conversationID(uid, "anchor-x"), conversationID(uid, "anchor-y"); a == b {
		t.Errorf("不同会话锚点不应得到同一会话 ID: %q", a)
	}
	if a, b := conversationID("uid-1", "anchor-x"), conversationID("uid-2", "anchor-x"); a == b {
		t.Errorf("不同账号服务同一段对话时不应共用会话 ID（关联系数会串号）: %q", a)
	}
	if v := conversationID(uid, ""); len(v) != 36 {
		t.Errorf("空锚点应回退随机 UUIDv4，实际 %q", v)
	}

	// 端到端：同一 body 连发两次，会话 ID 不变、调用 ID 各自刷新。
	var convs, msgs []string
	srv := testServer(func(w http.ResponseWriter, r *http.Request, n int) {
		convs = append(convs, r.Header.Get("X-Conversation-ID"))
		msgs = append(msgs, r.Header.Get("X-Conversation-Message-ID"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	defer srv.Close()
	c := NewWithBase(srv.URL)
	for i := 0; i < 2; i++ {
		if _, _, _, err := c.ChatStream(testAuth(), body); err != nil {
			t.Fatalf("ChatStream #%d: %v", i, err)
		}
	}
	if len(convs) != 2 || convs[0] != convs[1] {
		t.Errorf("同一会话两次请求的会话 ID 应一致: %v", convs)
	}
	if len(msgs) != 2 || msgs[0] == msgs[1] {
		t.Errorf("调用级 ID 每次请求都应新生成: %v", msgs)
	}
}

// TestAttributionHeadersMatchDesktop 归属头组合必须是官方那一串（R117）：
// X-Agent-Intent 缺省 craft、X-Product 取 deploymentType、X-Private-Data 默认 true。
func TestAttributionHeadersMatchDesktop(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	injectAttributionWBAI(req)
	want := map[string]string{
		"X-Agent-Intent":  "craft",
		"X-Agent-Purpose": "conversation",
		"X-IDE-Name":      "WorkBuddy",
		"X-IDE-Type":      "WorkBuddy",
		"X-IDE-Version":   clientVersion,
		"X-Product":       "SaaS",
		"X-Private-Data":  "true",
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// TestMachineIDGone 回归锁：走完整 chatHeaders 路径，确认自造指纹头不会以任何形式回归
// （R117 删除后若有人再补回来，这里立刻红）。
func TestMachineIDGone(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.invalid/", nil)
	chatHeaders(req, testAuth(), "conv-anchor", "turn-anchor")
	if v := req.Header.Get("X-Machine-ID"); v != "" {
		t.Errorf("X-Machine-ID 不应再被注入: %q", v)
	}
	if v := req.Header.Get("X-Session-ID"); v != "" {
		t.Errorf("X-Session-ID 不应再被注入: %q", v)
	}
}
