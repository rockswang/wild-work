package glm

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// mockChatGLM 模拟 chatglm.cn 的私有接口，用于端到端验证本渠道的完整链路。
// 这是「无真实账号」前提下最有价值的验证：证明请求构造、签名、信封解析、
// SSE 转换、签到流程全部按上游真实形状工作。
type mockChatGLM struct {
	mu sync.Mutex

	// 记录收到的请求，供断言
	refreshCalls  int
	chatCalls     int
	checkinCalls  int
	lastChatBody  map[string]any
	lastCheckin   map[string]any
	seenSign      bool
	seenAuth      string
	conversationN int

	// 行为开关
	checkinAlreadyDone bool
	checkinFails       bool
	// chatPartsSequence 增量帧序列（每帧 part.status=init，text 为增量片段）
	chatPartsSequence [][]part
	// chatFinalText finish 帧给出的完整全文
	chatFinalText string
}

func (m *mockChatGLM) handler() http.Handler {
	mux := http.NewServeMux()

	// 所有私有接口都要求签名头；缺签名返回 400（与真实上游一致）
	guard := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			if r.Header.Get("X-Sign") != "" && r.Header.Get("X-Timestamp") != "" &&
				r.Header.Get("X-Nonce") != "" {
				m.seenSign = true
			} else {
				m.mu.Unlock()
				// 真实上游：缺签名 → 400 bad request(40001)
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"status":40001,"message":"bad request(40001)","result":null}`))
				return
			}
			m.seenAuth = r.Header.Get("Authorization")
			m.mu.Unlock()
			next(w, r)
		}
	}

	// 刷新 token：返回 access_token + 轮换后的 refresh_token
	mux.HandleFunc("/chatglm/user-api/user/refresh", guard(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.refreshCalls++
		m.mu.Unlock()
		if strings.Contains(r.Header.Get("Authorization"), "BAD") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":40102,"message":"unauthorized user(40102)","result":null}`))
			return
		}
		writeEnv(w, map[string]any{
			"access_token":  "AT-new",
			"refresh_token": "RT-rotated",
			"user_id":       "u-123",
			"expires_in":    3600,
		})
	}))

	// 用户信息
	mux.HandleFunc("/chatglm/user-api/user/info", guard(func(w http.ResponseWriter, r *http.Request) {
		writeEnv(w, map[string]any{
			"user_id":  "u-123",
			"nickname": "测试用户",
			"phone":    "138****0000",
		})
	}))

	// 对话：SSE。按**实测语义**下发——init 帧是增量片段，最后补一帧 finish 给全文。
	mux.HandleFunc("/chatglm/backend-api/assistant/stream", guard(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		m.mu.Lock()
		m.chatCalls++
		m.lastChatBody = body
		seq := m.chatPartsSequence
		final := m.chatFinalText
		m.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		write := func(status string, parts []part) {
			frame := map[string]any{
				"conversation_id": "conv-1",
				"status":          status,
				"parts":           parts,
			}
			b, _ := json.Marshal(frame)
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
			if fl != nil {
				fl.Flush()
			}
		}
		// 首帧：空 parts（真实上游如此）
		write("init", []part{})
		// 增量帧
		for _, parts := range seq {
			write("init", parts)
		}
		// finish 帧：给出完整全文
		if final != "" {
			write("init", []part{{
				LogicID: "L1",
				Status:  "finish",
				Content: []partContent{{Type: "text", Text: final}},
			}})
		}
		write("finish", []part{{
			LogicID: "L1",
			Status:  "finish",
			Content: []partContent{{Type: "text", Text: final}},
		}})
	}))

	// 签到状态
	mux.HandleFunc("/chatglm/activity-api/activity/daily/check_in_info", guard(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		already := m.checkinAlreadyDone
		calls := m.checkinCalls
		m.mu.Unlock()
		todayStatus := 0
		if already || calls > 0 {
			todayStatus = 1
		}
		writeEnv(w, map[string]any{
			"check_in_today_status": todayStatus,
			"check_in_total_days":   7,
			"need_days":             7,
			"prize_days":            7,
			"prize_chance":          0,
			"check_in_list":         []any{},
		})
	}))

	// 执行签到
	mux.HandleFunc("/chatglm/activity-api/activity/daily/check_in", guard(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		m.mu.Lock()
		m.checkinCalls++
		m.lastCheckin = body
		fails := m.checkinFails
		m.mu.Unlock()
		if fails {
			writeEnvErr(w, 50001, "打卡失败")
			return
		}
		writeEnv(w, map[string]any{"ok": true})
	}))

	// 删会话
	mux.HandleFunc("/chatglm/backend-api/assistant/conversation/delete", guard(func(w http.ResponseWriter, r *http.Request) {
		writeEnv(w, map[string]any{"ok": true})
	}))

	return mux
}

// futureUnix 返回一个远期 Unix 秒（让 NeedsRefresh 恒为假，避免测试触发刷新）。
func futureUnix() int64 { return time.Now().Add(24 * time.Hour).Unix() }

// nowTime 当前时刻（测试内取 event_date 用）。
func nowTime() time.Time { return time.Now() }

// writeEnv 按清言信封写成功响应。
func writeEnv(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": 0, "message": "success", "result": result, "rid": "test-rid",
	})
}

// writeEnvErr 按清言信封写业务错误。
func writeEnvErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": status, "message": msg, "result": nil, "rid": "test-rid",
	})
}

// newTestClient 起一个 mock 上游并返回指向它的客户端。
func newTestClient(t *testing.T, m *mockChatGLM) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(m.handler())
	c := NewWithBase(srv.URL)
	return c, srv
}

// TestE2E_RefreshAndUserInfo 端到端：refresh → user/info，并验证签名头被带上。
func TestE2E_RefreshAndUserInfo(t *testing.T) {
	m := &mockChatGLM{}
	c, srv := newTestClient(t, m)
	defer srv.Close()

	acct, err := c.Validate(t.Context(), "RT-initial")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if acct.AccessToken != "AT-new" {
		t.Errorf("access_token = %q, want AT-new", acct.AccessToken)
	}
	if acct.RefreshToken != "RT-rotated" {
		t.Errorf("refresh_token = %q, want RT-rotated（必须用轮换后的值）", acct.RefreshToken)
	}
	if acct.UID != "u-123" {
		t.Errorf("uid = %q, want u-123", acct.UID)
	}
	if acct.Nickname != "测试用户" {
		t.Errorf("nickname = %q, want 测试用户", acct.Nickname)
	}
	if !m.seenSign {
		t.Error("请求未携带完整签名头（X-Sign/X-Timestamp/X-Nonce）")
	}
	if m.refreshCalls != 1 {
		t.Errorf("refresh 调用次数 = %d, want 1", m.refreshCalls)
	}
	_ = c
}

// TestE2E_BadTokenRejected 无效 token 必须报错，且分类为 session_dead。
func TestE2E_BadTokenRejected(t *testing.T) {
	m := &mockChatGLM{}
	c, srv := newTestClient(t, m)
	defer srv.Close()

	_, err := c.refresh(t.Context(), "BAD-token")
	if err == nil {
		t.Fatal("无效 token 应当报错")
	}
	pe, ok := err.(*provider.Error)
	if !ok {
		t.Fatalf("错误类型 = %T, want *provider.Error", err)
	}
	if pe.Kind != provider.ErrSessionDead {
		t.Errorf("错误分类 = %s, want session_dead", pe.Kind)
	}
}

// TestE2E_ChatStreamDelta 端到端：对话 SSE 增量帧 → OpenAI 增量，内容完整无重复。
//
// 帧数据按**实测语义**构造：init 帧是增量片段，finish 帧给全文。
func TestE2E_ChatStreamDelta(t *testing.T) {
	m := &mockChatGLM{
		chatPartsSequence: [][]part{
			{{LogicID: "L1", Status: "init", Content: []partContent{{Type: "think", Think: "想想"}}}},
			{{LogicID: "L1", Status: "init", Content: []partContent{{Type: "text", Text: "你好"}}}},
			{{LogicID: "L1", Status: "init", Content: []partContent{{Type: "text", Text: "，世界"}}}},
		},
		chatFinalText: "你好，世界",
	}
	c, srv := newTestClient(t, m)
	defer srv.Close()

	// 构造一个已就绪的 Auth（token 未过期，避免触发刷新）
	a := &auth.Auth{
		Kind:        string(Kind),
		AccessToken: "AT-ready",
		ExpiresAt:   futureUnix(),
		UID:         "u-123",
	}

	reqBody, _ := json.Marshal(map[string]any{
		"model": "glm/chatglm",
		"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
		},
		"stream": true,
	})
	rc, status, respBody, err := c.ChatStream(a, reqBody)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, body = %s", status, respBody)
	}
	defer rc.Close()

	// 转换并断言内容完整且无重复
	rec := &flushRecorder{}
	if _, err := c.Stream(rec, rc, "glm/chatglm"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	content, reasoning := collectDeltas(t, rec)
	if content != "你好，世界" {
		t.Errorf("正文 = %q, want %q（增量拼接必须完整且不重复）", content, "你好，世界")
	}
	if reasoning != "想想" {
		t.Errorf("思考 = %q, want 想想", reasoning)
	}

	// 断言请求体形状正确
	if m.lastChatBody == nil {
		t.Fatal("未记录到对话请求体")
	}
	if m.lastChatBody["assistant_id"] != DefaultAssistantID {
		t.Errorf("assistant_id = %v, want %s", m.lastChatBody["assistant_id"], DefaultAssistantID)
	}
	if m.lastChatBody["chat_type"] != "user_chat" {
		t.Errorf("chat_type = %v, want user_chat", m.lastChatBody["chat_type"])
	}
	meta, _ := m.lastChatBody["meta_data"].(map[string]any)
	if meta == nil {
		t.Fatal("缺少 meta_data")
	}
	if meta["platform"] != "pc" {
		t.Errorf("meta_data.platform = %v, want pc", meta["platform"])
	}
	if meta["if_plus_model"] != true {
		t.Errorf("meta_data.if_plus_model = %v, want true", meta["if_plus_model"])
	}
	// messages 必须是单条合并消息
	msgs, _ := m.lastChatBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Errorf("messages 数 = %d, want 1（清言要求合并为单条）", len(msgs))
	}
}

// TestE2E_CheckinSuccess 端到端：签到成功 → claimed。
func TestE2E_CheckinSuccess(t *testing.T) {
	m := &mockChatGLM{
		// 保活对话也要能跑通
		chatPartsSequence: [][]part{
			{{LogicID: "L1", Status: "init", Content: []partContent{{Type: "text", Text: "好"}}}},
		},
		chatFinalText: "好",
	}
	c, srv := newTestClient(t, m)
	defer srv.Close()

	a := &auth.Auth{
		Kind:        string(Kind),
		AccessToken: "AT-ready",
		ExpiresAt:   futureUnix(),
		UID:         "u-123",
	}

	report, err := c.DailyCheckinReport(a)
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if report.Status != provider.CheckinClaimed {
		t.Errorf("status = %s, want claimed", report.Status)
	}
	if m.checkinCalls != 1 {
		t.Errorf("签到调用次数 = %d, want 1", m.checkinCalls)
	}
	// event_date 必须是 UTC+8 当天
	if ed, _ := m.lastCheckin["event_date"].(string); ed != eventDate(nowTime()) {
		t.Errorf("event_date = %q, want %q", ed, eventDate(nowTime()))
	}
	// 保活对话应当已发生
	if m.chatCalls == 0 {
		t.Error("签到前未发送保活对话")
	}
}

// TestE2E_CheckinLegacyActivityOffline 旧签到活动已下线时，
// 「签到」仍应成功——因为本渠道的实质动作是**保活对话**，积分由服务端按天被动发放
// （score_rule = "免费用户，登录赠送200积分/天"），与旧活动无关。
//
// 这条替换了早期「签到失败必须报失败」的断言：那个断言建立在
// 「积分靠 activity-api 签到领取」的错误假设上（2026-09-26 以真实账号证伪）。
func TestE2E_CheckinLegacyActivityOffline(t *testing.T) {
	m := &mockChatGLM{
		checkinFails: true, // 旧活动接口报错（模拟「活动已结束」）
		chatPartsSequence: [][]part{
			{{LogicID: "L1", Status: "init", Content: []partContent{{Type: "text", Text: "好"}}}},
		},
		chatFinalText: "好",
	}
	c, srv := newTestClient(t, m)
	defer srv.Close()

	a := &auth.Auth{Kind: string(Kind), AccessToken: "AT-ready", ExpiresAt: futureUnix(), UID: "u-123"}
	report, err := c.DailyCheckinReport(a)
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	// 保活对话成功 = 签到成功（积分发放与旧活动无关）
	if report.Status != provider.CheckinClaimed {
		t.Errorf("status = %s, want claimed（保活对话成功即算签到成功）", report.Status)
	}
	if report.Status.Retryable() {
		t.Error("claimed 不应可重试")
	}
	if m.chatCalls == 0 {
		t.Error("未发送保活对话（这是本渠道签到的实质动作）")
	}
	// 旧活动失败不得污染结果文案
	if strings.Contains(report.Msg, "活动") || strings.Contains(report.Msg, "失败") {
		t.Errorf("旧活动失败不应出现在结果文案里（会误导用户）: %q", report.Msg)
	}
}

// TestE2E_CheckinChatFailureIsRealFailure 保活对话失败才是真失败。
//
// 与上一条对照：区分「旧活动下线」（无害）与「对话本身失败」（账号可能失效）。
func TestE2E_CheckinChatFailureIsRealFailure(t *testing.T) {
	// 对话接口返回 401，模拟会话失效
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "assistant/stream") {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"status":40102,"message":"unauthorized user(40102)"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":0,"message":"success","result":{}}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	a := &auth.Auth{Kind: string(Kind), AccessToken: "AT", ExpiresAt: futureUnix(), UID: "u-1"}

	report, err := c.DailyCheckinReport(a)
	if report.Status == provider.CheckinClaimed || report.Status == provider.CheckinAlready {
		t.Errorf("对话失败却报成功状态 %s（假成功）", report.Status)
	}
	if !report.Status.Retryable() {
		t.Errorf("对话失败状态 %s 应可重试", report.Status)
	}
	_ = err
}

// TestE2E_ScoreFromMemberInfo 积分必须来自 member_info 且单位换算正确。
//
// 实测依据：上游 left_score=300000（单位「分」），App 显示 3000 积分 → 除以 100。
func TestE2E_ScoreFromMemberInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "member_info") {
			_, _ = w.Write([]byte(`{"status":0,"message":"success","result":{
				"left_score":300000,"true_left_score":0,"left_token":12000000,
				"is_member":false,"score_rule":"免费用户，登录赠送200积分/天"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":0,"message":"success","result":{}}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	a := &auth.Auth{Kind: string(Kind), AccessToken: "AT", ExpiresAt: futureUnix(), UID: "u-1"}

	// UserResource：应返回换算后的积分（300000/100 = 3000）
	score, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("UserResource: %v", err)
	}
	if score != 3000 {
		t.Errorf("积分 = %d, want 3000（上游 300000 分 ÷ 100）", score)
	}

	// 明细：一条汇总条目，ExpireAt 必须为空（上游未下发到期时间）
	remain, items, err := c.UserResourceDetail(a)
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if remain != 3000 {
		t.Errorf("明细 remain = %d, want 3000", remain)
	}
	if len(items) != 1 {
		t.Fatalf("明细条目数 = %d, want 1", len(items))
	}
	if items[0].ExpireAt != "" {
		t.Errorf("ExpireAt 应为空串（上游未下发），got %q", items[0].ExpireAt)
	}
	if !items[0].Usable {
		t.Error("积分条目应标记为可用（Usable=true）")
	}
	if !strings.Contains(items[0].Name, "积分") {
		t.Errorf("条目名应含「积分」: %q", items[0].Name)
	}
}

// TestE2E_DeleteConversation 删除会话请求形状正确。
func TestE2E_DeleteConversation(t *testing.T) {
	m := &mockChatGLM{}
	c, srv := newTestClient(t, m)
	defer srv.Close()

	a := &auth.Auth{Kind: string(Kind), AccessToken: "AT-ready", ExpiresAt: futureUnix(), UID: "u-123"}
	if err := c.DeleteConversation(a, "conv-1", DefaultAssistantID); err != nil {
		t.Fatalf("DeleteConversation: %v", err)
	}
}
