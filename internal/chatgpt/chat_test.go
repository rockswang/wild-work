package chatgpt

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"wild-work/internal/provider"
)

// chatMock 在既有 sentinel 假上游之上补两步对话端点（conduit + conversation）。
//
// 复用 mockSentinel（首页/prepare/finalize）而不是另写一份：五步链路的前三步
// 已经在那里被锁死，这里只需观测第 4、5 步。
type chatMock struct {
	*mockSentinel

	mu         sync.Mutex
	seq        []string
	conduitHit int
	convHit    int
	prepBodies []string
	convBodies []string
	convHdrs   []http.Header
	convStatus int
	convResp   string
	noDone     bool
	conduits   []string
}

func newChatMock() *chatMock { return &chatMock{mockSentinel: newMockSentinel()} }

func (m *chatMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case PathFConversationPrepare:
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.seq = append(m.seq, r.URL.Path)
		m.conduitHit++
		m.conduits = append(m.conduits, "cd-"+itoa(m.conduitHit))
		m.prepBodies = append(m.prepBodies, string(body))
		tok := "cd-" + itoa(m.conduitHit)
		m.mu.Unlock()
		writeJSONTest(w, map[string]any{"conduit_token": tok})
	case PathFConversation:
		body, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.seq = append(m.seq, r.URL.Path)
		m.convHit++
		m.convBodies = append(m.convBodies, string(body))
		m.convHdrs = append(m.convHdrs, r.Header.Clone())
		status := m.convStatus
		resp := m.convResp
		noDone := m.noDone
		m.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(resp))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, resp)
		if !noDone {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}
	default:
		m.mu.Lock()
		m.seq = append(m.seq, r.URL.Path)
		m.mu.Unlock()
		m.mockSentinel.ServeHTTP(w, r)
	}
}

func (m *chatMock) counts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.conduitHit, m.convHit
}

func (m *chatMock) lastConv() (map[string]any, http.Header) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var body map[string]any
	if n := len(m.convBodies); n > 0 {
		_ = json.Unmarshal([]byte(m.convBodies[n-1]), &body)
	}
	var h http.Header
	if n := len(m.convHdrs); n > 0 {
		h = m.convHdrs[n-1]
	}
	return body, h
}

// chatMockClient 建客户端并把对话端点的假上游装好。
func chatMockClient(t *testing.T) (*Client, *chatMock) {
	t.Helper()
	m := newChatMock()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	c := NewWithConfig(Config{Base: srv.URL, DataDir: t.TempDir()})
	t.Cleanup(c.Close)
	return c, m
}

// 一段真实形状的上游流：首帧完整消息 + 包裹 patch + **裸** patch（含引用标记）。
func upstreamFrames() string {
	return "data: " + `{"conversation_id":"c-1","message":{"id":"m1","author":{"role":"assistant"},"content":{"content_type":"text","parts":["你"]}}}` + "\n\n" +
		"data: " + `{"o":"patch","v":[{"p":"/message/content/parts/0","o":"append","v":"好"}]}` + "\n\n" +
		"data: " + `{"v":[{"p":"/message/content/parts/0","o":"append","v":"<Cite refs={[\"a\"]}/>世界"}]}` + "\n\n"
}

func inboundBody(extra string) []byte {
	return []byte(`{"model":"auto","stream":true,` + extra +
		`"messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"第一问"},{"role":"user","content":[{"type":"text","text":"第二问"}]}]}`)
}

// TestChatStreamWalksFiveSteps 五步链路齐全，第 5 步拿到的 body/头符合上游形状。
func TestChatStreamWalksFiveSteps(t *testing.T) {
	c, m := chatMockClient(t)
	m.convResp = upstreamFrames() + "data: [DONE]\n\n"

	rc, status, _, err := c.ChatStream(sentinelAuth(), inboundBody(`"tools":[{"type":"function"}],`))
	if err != nil || status != http.StatusOK {
		t.Fatalf("ChatStream status=%d err=%v", status, err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	for _, p := range []string{"/", PathSentinelPrepare, PathSentinelFinalize} {
		if m.hits(p) == 0 {
			t.Errorf("前三步缺 %s", p)
		}
	}
	conduitHit, convHit := m.counts()
	if conduitHit != 1 || convHit != 1 {
		t.Fatalf("conduit=%d conv=%d，期望各 1 次", conduitHit, convHit)
	}

	// 顺序锁死：conduit（第 4 步）必须在预检之后、对话（第 5 步）之前——
	// 它只有 60 秒生命，提前取等于白取。
	m.mu.Lock()
	seq := append([]string(nil), m.seq...)
	m.mu.Unlock()
	want := []string{"/", PathSentinelPrepare, PathSentinelFinalize, PathFConversationPrepare, PathFConversation}
	if len(seq) != len(want) {
		t.Fatalf("请求序列 = %v，期望 %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("第 %d 步 = %s，期望 %s（顺序 = %v）", i+1, seq[i], want[i], seq)
		}
	}

	body, h := m.lastConv()
	if h.Get("x-conduit-token") != "cd-1" {
		t.Errorf("conduit 头 = %q", h.Get("x-conduit-token"))
	}
	if h.Get("openai-sentinel-chat-requirements-token") != "req-token-xyz" {
		t.Errorf("requirements 头 = %q", h.Get("openai-sentinel-chat-requirements-token"))
	}
	if body["model"] != "auto" || body["parent_message_id"] != parentMessageRoot {
		t.Errorf("body model=%v parent=%v", body["model"], body["parent_message_id"])
	}
	// tools 必须被丢掉（本渠道无工具能力）。
	if _, ok := body["tools"]; ok {
		t.Error("出站 body 不得带 tools")
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("上游只吃一条 user 消息，实际 %d 条", len(msgs))
	}
	msg, _ := msgs[0].(map[string]any)
	author, _ := msg["author"].(map[string]any)
	if author["role"] != "user" {
		t.Errorf("role = %v", author["role"])
	}
	content, _ := msg["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	text, _ := parts[0].(string)
	if !strings.Contains(text, "你是助手") || !strings.Contains(text, "第二问") {
		t.Errorf("正文 = %q，期望含 system 与最后一条 user", text)
	}
	if strings.Contains(text, "第一问") {
		t.Errorf("正文 = %q，不得包含更早的 user 消息", text)
	}
}

// TestChatStreamRejectsNoUserText 没有 user 文本就在本地拒绝（400），不打上游。
func TestChatStreamRejectsNoUserText(t *testing.T) {
	c, m := chatMockClient(t)
	body := []byte(`{"model":"auto","messages":[{"role":"system","content":"只有系统提示"}]}`)
	rc, status, resp, err := c.ChatStream(sentinelAuth(), body)
	if err != nil || rc != nil {
		t.Fatalf("err=%v rc=%v", err, rc)
	}
	if status != http.StatusBadRequest || !bytes.Contains(resp, []byte("chatgpt_bad_request")) {
		t.Fatalf("status=%d body=%s", status, resp)
	}
	if _, convHit := m.counts(); convHit != 0 || m.hits(PathSentinelPrepare) != 0 {
		t.Error("本地拒绝后不得产生任何上游请求（含预检）")
	}
}

// TestChatStreamConduitIsPerRequest conduit 60 秒 TTL + 每轮不同 ⇒ 每次请求都现取。
func TestChatStreamConduitIsPerRequest(t *testing.T) {
	c, m := chatMockClient(t)
	m.convResp = upstreamFrames() + "data: [DONE]\n\n"
	for i := 0; i < 2; i++ {
		rc, status, _, err := c.ChatStream(sentinelAuth(), inboundBody(""))
		if err != nil || status != http.StatusOK {
			t.Fatalf("第 %d 次 status=%d err=%v", i+1, status, err)
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	conduitHit, convHit := m.counts()
	if conduitHit != 2 || convHit != 2 {
		t.Fatalf("conduit=%d conv=%d，期望 2/2", conduitHit, convHit)
	}
	m.mu.Lock()
	toks := append([]string(nil), m.conduits...)
	m.mu.Unlock()
	if len(toks) != 2 || toks[0] == toks[1] {
		t.Errorf("两次 conduit = %v，期望各不相同", toks)
	}
}

// TestChatStream403UnusualIsAccountLevel 「异常活动」403 是账号级：只打一次、不重试。
func TestChatStream403UnusualIsAccountLevel(t *testing.T) {
	c, m := chatMockClient(t)
	m.convStatus = http.StatusForbidden
	m.convResp = `{"detail":"Unusual activity has been detected from your device. Try again later."}`
	_, status, resp, err := c.ChatStream(sentinelAuth(), inboundBody(""))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("status=%d", status)
	}
	if _, convHit := m.counts(); convHit != 1 {
		t.Errorf("conv 命中 %d 次，风控类 403 不得同号重试", convHit)
	}
	if got := Classify(status, string(resp)); got != provider.ErrAccountFault {
		t.Errorf("Classify = %v，期望 ErrAccountFault", got)
	}
	if bytes.Contains(resp, []byte("代理")) {
		t.Error("风控类 403 不得附「没配代理」提示（会误导）")
	}
}

// TestChatStream403OtherIsPassthroughWithHint 普通 403 原文透传 + 可行动提示。
func TestChatStream403OtherIsPassthroughWithHint(t *testing.T) {
	c, m := chatMockClient(t)
	m.convStatus = http.StatusForbidden
	m.convResp = `{"detail":"Forbidden"}`
	_, status, resp, err := c.ChatStream(sentinelAuth(), inboundBody(""))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("status=%d", status)
	}
	var m2 map[string]any
	if json.Unmarshal(resp, &m2) != nil {
		t.Fatalf("响应体 = %s", resp)
	}
	if m2["detail"] != "Forbidden" {
		t.Errorf("上游原文被改动: %v", m2["detail"])
	}
	if hint, _ := m2["hint"].(string); !strings.Contains(hint, "代理") {
		t.Errorf("hint = %v，期望提示检查代理", m2["hint"])
	}
	if got := Classify(status, string(resp)); got != provider.ErrPassthrough {
		t.Errorf("Classify = %v，期望 ErrPassthrough", got)
	}
}

// TestChatStreamServerErrorVerbatim 5xx 原样返回（handler 侧不换号、不计账号错误）。
func TestChatStreamServerErrorVerbatim(t *testing.T) {
	c, m := chatMockClient(t)
	m.convStatus = http.StatusInternalServerError
	m.convResp = `{"detail":"boom"}`
	_, status, resp, err := c.ChatStream(sentinelAuth(), inboundBody(""))
	if err != nil || status != http.StatusInternalServerError {
		t.Fatalf("status=%d err=%v", status, err)
	}
	if string(resp) != `{"detail":"boom"}` {
		t.Errorf("5xx 原文被改写: %s", resp)
	}
}

// TestChatStreamDriftSendsNoBusinessRequest 预检漂移 ⇒ 拒发，且失败是「上游错误」（不换号）。
func TestChatStreamDriftSendsNoBusinessRequest(t *testing.T) {
	m := newChatMock()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	dir := t.TempDir()

	// 第一轮落基线（新建第二个客户端是为了避开 120 秒的令牌缓存，否则不会重跑预检）。
	prime := NewWithConfig(Config{Base: srv.URL, DataDir: dir})
	if _, err := prime.RunSentinel(sentinelAuth()); err != nil {
		t.Fatalf("基线轮: %v", err)
	}
	prime.Close()

	m.prepExtraKeys = []string{"brand_new_field"}
	c := NewWithConfig(Config{Base: srv.URL, DataDir: dir})
	t.Cleanup(c.Close)

	rc, status, _, err := c.ChatStream(sentinelAuth(), inboundBody(""))
	if err == nil || rc != nil || status != 0 {
		t.Fatalf("漂移必须返回 error（rc=%v status=%d err=%v）", rc, status, err)
	}
	if _, convHit := m.counts(); convHit != 0 {
		t.Errorf("漂移时业务请求数必须为 0，实际 %d", convHit)
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrUpstream {
		t.Errorf("漂移错误分类 = %v，期望 ErrUpstream（不换号、不计账号错误）", err)
	}
}

// TestChatStreamTransportErrorNotClassified 传输层错误保持为「裸 error」：
// 一旦被类型化成 *provider.Error，handler 就会当账号错误去换号/计数（spec §10 禁止）。
func TestChatStreamTransportErrorNotClassified(t *testing.T) {
	m := newChatMock()
	srv := httptest.NewServer(m)
	base := srv.URL
	srv.Close()

	c := NewWithConfig(Config{Base: base, DataDir: t.TempDir()})
	t.Cleanup(c.Close)

	rc, status, _, err := c.ChatStream(sentinelAuth(), inboundBody(""))
	if err == nil || rc != nil || status != 0 {
		t.Fatalf("连不上必须是裸 error（rc=%v status=%d err=%v）", rc, status, err)
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		t.Errorf("传输层错误被类型化了（Kind=%v）⇒ handler 会误判成账号错误", pe.Kind)
	}
}
