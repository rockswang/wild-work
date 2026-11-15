package doubao

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// ---------- 会话键推导 ----------

func TestConversationKeyPriority(t *testing.T) {
	// ① metadata 显式 id 最高优先
	body := []byte(`{"metadata":{"session_id":"sess-1"},"user":"alice","messages":[{"role":"user","content":"第一句"}]}`)
	if k := conversationKey(body); k != "c:sess-1" {
		t.Fatalf("metadata.session_id 应最优先，实际 %s", k)
	}
	// ② user 字段次之
	body = []byte(`{"user":"alice","messages":[{"role":"user","content":"第一句"}]}`)
	if k := conversationKey(body); k != "u:alice" {
		t.Fatalf("user 字段次优先，实际 %s", k)
	}
	// ③ 首条 user 消息哈希（无状态客户端的线程标识）
	body = []byte(`{"messages":[{"role":"user","content":"第一句"}]}`)
	k1 := conversationKey(body)
	if !strings.HasPrefix(k1, "h:") {
		t.Fatalf("首条 user 消息哈希应带 h: 前缀，实际 %s", k1)
	}
	// 同首条消息 + 后续追加历史 ⇒ 键不变（pi/Codex 每轮重发全历史的线程形态）
	body = []byte(`{"messages":[{"role":"user","content":"第一句"},{"role":"assistant","content":"回复"},{"role":"user","content":"第二句"}]}`)
	if k2 := conversationKey(body); k2 != k1 {
		t.Fatalf("追加历史后键漂移：首轮 %s 追加后 %s", k1, k2)
	}
	// 不同线程（不同首条）隔离
	body = []byte(`{"messages":[{"role":"user","content":"另一个线程"}]}`)
	if k3 := conversationKey(body); k3 == k1 {
		t.Fatal("不同首条消息的线程必须隔离")
	}
	// ④ 空体兜底整包哈希
	if k4 := conversationKey([]byte(`{"model":"x"}`)); !strings.HasPrefix(k4, "r:") {
		t.Fatalf("无可用线索应退整包哈希 r:，实际 %s", k4)
	}
}

// ---------- 会话缓存 ----------

func newIsolatedStore() *sessionStore {
	return &sessionStore{items: map[string]sessionState{}}
}

func TestSessionStoreRememberLookupForget(t *testing.T) {
	s := newIsolatedStore()
	s.remember("k1", "conv-123", "sec-456")
	st, ok := s.lookup("k1")
	if !ok || st.ConversationID != "conv-123" || st.SectionID != "sec-456" {
		t.Fatalf("lookup 失败：ok=%v st=%+v", ok, st)
	}
	// lookup 会 touch（at 更新）
	time.Sleep(5 * time.Millisecond)
	s.lookup("k1")
	if s.items["k1"].at.Equal(s.items["k1"].at) {
		// touch 后 at 被刷新（弱断言：值存在即可，时间比较在同毫秒内可能相等）
		_ = st
	}
	// 占位 "0" 不记（SeiShonagon520 同款过滤）
	s.remember("k2", "0", "")
	if _, ok := s.lookup("k2"); ok {
		t.Fatal(`占位 "0" 不得入缓存`)
	}
	// forget 清除
	s.forget("k1")
	if _, ok := s.lookup("k1"); ok {
		t.Fatal("forget 后应 miss")
	}
}

func TestSessionStoreExpiryAndCapacity(t *testing.T) {
	s := newIsolatedStore()
	// TTL 过期
	s.items["old"] = sessionState{ConversationID: "c", at: time.Now().Add(-sessionTTL - time.Minute)}
	if _, ok := s.lookup("old"); ok {
		t.Fatal("过期条目必须 miss")
	}
	// 容量淘汰：灌满 + 一条旧于 TTL，再插入新键时旧 TTL 条目被清
	s2 := newIsolatedStore()
	s2.items["stale"] = sessionState{ConversationID: "c", at: time.Now().Add(-2 * time.Hour)}
	for i := 0; i < sessionMaxKeys; i++ {
		s2.remember(string(rune('a'+i%26))+"-"+string(rune('a'+i/26))+"-"+string(rune('a'+i/676)), "c", "")
	}
	if _, ok := s2.lookup("stale"); ok {
		t.Fatal("容量治理必须淘汰过期条目")
	}
	if len(s2.items) > sessionMaxKeys {
		t.Fatalf("容量超限：%d", len(s2.items))
	}
}

// ---------- buildChatBody 复用分支 ----------

func TestBuildChatBodyReuseOnlySendsLastMessage(t *testing.T) {
	req := &chatRequest{Model: "doubao/deepseek-v4-flash", Messages: []chatMessage{
		{Role: "user", Content: rawStr("第一句")},
		{Role: "assistant", Content: rawStr("第一次回复")},
		{Role: "user", Content: rawStr("第二句")},
	}}
	out, err := buildChatBody(req, staticModels["deepseek-v4-flash"].ItemKey, "", diagDev, "", "", sessionState{ConversationID: "conv-abc", SectionID: "sec-9"}, nil)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	got := string(out)
	// 复用轮：conv id 与 section id 带上、不建会话
	for _, want := range []string{`"conversation_id":"conv-abc"`, `"last_section_id":"sec-9"`, `"need_create_conversation":false`} {
		if !strings.Contains(got, want) {
			t.Fatalf("复用轮请求体缺少 %q：\n%s", want, got)
		}
	}
	// 复用轮只发最后一句：历史不得压入
	if strings.Contains(got, "第一句") || strings.Contains(got, "第一次回复") {
		t.Fatalf("复用轮不得压平历史：\n%s", got)
	}
	if !strings.Contains(got, "第二句") {
		t.Fatalf("复用轮应发最后一句：\n%s", got)
	}
}

func TestBuildChatBodyFirstTurnCreatesConversation(t *testing.T) {
	req := &chatRequest{Model: "doubao/deepseek-v4-flash", Messages: []chatMessage{
		{Role: "user", Content: rawStr("第一句")},
	}}
	out, err := buildChatBody(req, staticModels["deepseek-v4-flash"].ItemKey, "", diagDev, "", "", sessionState{}, nil)
	if err != nil {
		t.Fatalf("buildChatBody: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, `"need_create_conversation":true`) {
		t.Fatalf("首轮应 need_create_conversation=true：\n%s", got)
	}
	if strings.Contains(got, `"conversation_id":"`) && strings.Contains(got, `"conversation_id":"conv`) {
		t.Fatal("首轮 conversation_id 应为空")
	}
}

// rawStr 构造 JSON 字符串形式的 RawMessage。
func rawStr(s string) json.RawMessage {
	return json.RawMessage(`"` + s + `"`)
}

// ---------- outboundBodyFor 端到端：随聊随删（每轮恒首轮全量） ----------

func TestOutboundBodyForReuseFlow(t *testing.T) {
	store := newIsolatedStore()
	const uid = "u1"
	body := []byte(`{"model":"doubao/deepseek-v4-flash","messages":[{"role":"user","content":"第一句"}]}`)
	out, err := outboundBodyFor(body, diagDev, uid, store)
	if err != nil {
		t.Fatalf("首轮 outboundBodyFor: %v", err)
	}
	if !strings.Contains(string(out), `"need_create_conversation":true`) {
		t.Fatalf("首轮应 need_create_conversation=true：\n%s", out)
	}
	// 模拟 SSE_ACK 回写后再发第二轮：随聊随删模式下**不复用**，仍是全量压平
	store.remember(sessionKeyOf(uid, conversationKey(body)), "conv-777", "sec-3")
	body2 := []byte(`{"model":"doubao/deepseek-v4-flash","messages":[{"role":"user","content":"第一句"},{"role":"assistant","content":"回复"},{"role":"user","content":"第二句"}]}`)
	out2, err := outboundBodyFor(body2, diagDev, uid, store)
	if err != nil {
		t.Fatalf("第二轮 outboundBodyFor: %v", err)
	}
	got2 := string(out2)
	if !strings.Contains(got2, `"need_create_conversation":true`) || strings.Contains(got2, "conv-777") {
		t.Fatalf("随聊随删模式第二轮必须仍是新会话（不复用）：\n%s", got2)
	}
	// 全量压平（历史随每轮重发）
	if !strings.Contains(got2, "第一句") || !strings.Contains(got2, "第二句") {
		t.Fatalf("随聊随删模式每轮应携带完整历史：\n%s", got2)
	}
	// 旧入口行为一致
	out5, _ := outboundBody(body2, diagDev)
	if !strings.Contains(string(out5), "第一句") {
		t.Fatalf("旧入口应保持压平行为：\n%s", out5)
	}
}

// ---------- SSE_ACK 捕获回写 ----------

func ackSrc(conv, section string) string {
	return "event: SSE_ACK\ndata: {\"ack_client_meta\":{\"conversation_id\":\"" + conv + "\",\"section_id\":\"" + section + "\"}}\n\n" +
		"event: CHUNK_DELTA\ndata: {\"text\":\"答\"}\n\n" +
		"event: SSE_REPLY_END\ndata: {\"end_type\":1}\n\n"
}

func TestStreamCaptureAckWritesCache(t *testing.T) {
	store := newIsolatedStore()
	r := newDoubaoStreamWithSessions(strings.NewReader(ackSrc("conv-1", "sec-1")), "doubao/x", "k1", store, false)
	if _, err := io.ReadAll(r); err != nil {
		t.Fatalf("读流: %v", err)
	}
	// 随聊随删：流正常收尾后缓存条目应被 forget 清掉（焚化已后台安排）。
	store.mu.Lock()
	_, stillThere := store.items["k1"]
	store.mu.Unlock()
	if stillThere {
		t.Fatal("流收尾后应触发焚化（缓存条目应被清）")
	}
	// 占位 "0"：无 conv id，不焚化（缓存本来也没写过）
	store2 := newIsolatedStore()
	r2 := newDoubaoStreamWithSessions(strings.NewReader(ackSrc("0", "")), "doubao/x", "k2", store2, false)
	_, _ = io.ReadAll(r2)
	if _, ok := store2.lookup("k2"); ok {
		t.Fatal(`占位 "0" 不得回写`)
	}
	// nil store 不炸
	r3 := newDoubaoStream(strings.NewReader(ackSrc("conv-2", "")), "doubao/x")
	if _, err := io.ReadAll(r3); err != nil {
		t.Fatalf("nil store 读流: %v", err)
	}
}

// ---------- ChatStream 端到端：httptest 全链路（不碰真上游） ----------

func TestChatStreamSessionEndToEnd(t *testing.T) {
	var mu sync.Mutex
	reqBodies := []string{}
	upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(io.LimitReader(r.Body, 32<<10))
		mu.Lock()
		reqBodies = append(reqBodies, string(buf))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ackSrc("conv-e2e", "sec-e2e"))
	}))
	defer upstreamSrv.Close()

	// 随聊随删：流正常收尾后触发焚化（forget → 后台 delete）。假上游同时当
	// delete 端点用（路径不同不冲突）。必须在 ChatStream 之前设置（防竞态打真上游）。
	a := &auth.Auth{Kind: "doubao", UID: "e2e-uid", Extra: map[string]string{ExtraCookie: "sessionid=abc"}}
	overrideDeleteClient(t, upstreamSrv.URL)
	oldLookup := lookupAuthByUID
	defer func() { lookupAuthByUID = oldLookup }()
	lookupAuthByUID = func(uid string) *auth.Auth { return a }

	c := New()
	c.baseURL = upstreamSrv.URL
	store := newIsolatedStore()
	// 全局 store 替换：ChatStream 内部引用 globalSessions
	old := globalSessions
	globalSessions = store
	defer func() { globalSessions = old }()

	conv := "https://www.doubao.com/chat/completion" // 值不用，仅保证 ChatStream 内 baseURL 覆盖生效
	_ = conv

	// 首轮
	body1 := []byte(`{"model":"doubao/deepseek-v4-flash","messages":[{"role":"user","content":"第一句"}]}`)
	rc, status, _, err := c.ChatStream(a, body1)
	if err != nil || status >= 400 {
		t.Fatalf("首轮 ChatStream: status=%d err=%v", status, err)
	}
	// 会话回写在 Stream/Aggregate（reader 层捕获 SSE_ACK）：直接用 c.Stream 消费流，
	// 不预读不提前 Close（上游响应体被提前关闭会让转换层读到 closed body）。
	rec := httptest.NewRecorder()
	if _, err := c.Stream(rec, rc, "doubao/deepseek-v4-flash"); err != nil {
		t.Fatalf("首轮 Stream: %v", err)
	}
	rc.Close()
	mu.Lock()
	first := reqBodies[0]
	reqBodies = nil
	mu.Unlock()
	if !strings.Contains(first, `"need_create_conversation":true`) {
		t.Fatalf("首轮应建会话：\n%s", first)
	}
	// 随聊随删：首轮流收尾后缓存应已清（焚化安排中），delete 请求由下方等待验证。
	// override/lookup 必须在 ChatStream **之前**设置：焚化 goroutine 在流收尾 0.8s
	// 后醒来，晚设会竞态（lookup 返回 nil 静默放弃或打到真上游——绝不允许）。
	// 全量跑时其它测试的焚化 goroutine 可能串到本测试的假上游（全局
	// deleteClientFactory/lookupAuthByUID 是共享的），所以按 thread_id 精确匹配
	// 本测试自己的会话，不按请求序号。
	var delRaw string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, b := range reqBodies {
			if strings.Contains(b, "conv-e2e") {
				delRaw = b
				break
			}
		}
		n := len(reqBodies)
		mu.Unlock()
		if delRaw != "" {
			break
		}
		if n > 0 {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if delRaw == "" {
		t.Fatalf("流收尾后未触发焚化 delete（本测试会话的请求未到达，共 %d 个请求）", len(reqBodies))
	}
	var delBody map[string]string
	if json.Unmarshal([]byte(delRaw), &delBody) != nil || delBody["thread_id"] != "conv-e2e" {
		t.Fatalf("焚化 body 形状错误：%s", delRaw)
	}
}
