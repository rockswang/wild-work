// session_test.go 会话坑位修复（R145）的回归测试：**零真实网络**（httptest 假上游）。
//
// 修复要点（全部对应用实测结论，见 constants.go 的 EpChatSessionHeartbeat 注释）：
//  1. 对话请求必须带 `user-session-id`，且与 `Session-Id` 同值；
//  2. 响应体读完（EOF）或提前 Close 时，必须对该会话发一次
//     `PUT {Base}/snap-manager/v1/chat-session/heartbeat?status=idle`，头带 user-session-id，
//     且该头**参与签名**（否则上游 401 APIG.0301，坑位照样不释放）；
//  3. 心跳幂等：EOF 与 Close 两条路径合计只发一次；
//  4. 非 2xx 路径同样归还坑位。
package codearts

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// heartbeatRecorder 记录假上游收到的对话与心跳请求。
type heartbeatRecorder struct {
	mu      sync.Mutex
	chatSID string   // 对话请求的 user-session-id
	chatSes string   // 对话请求的 Session-Id
	beats   []string // 心跳请求的 user-session-id
	beatQ   []string // 心跳请求的 query
	beatHM  []string // 心跳请求的 SignedHeaders
}

func (h *heartbeatRecorder) snapshot() (chatSID, chatSes string, beats, beatQ, beatHM []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.chatSID, h.chatSes, append([]string(nil), h.beats...),
		append([]string(nil), h.beatQ...), append([]string(nil), h.beatHM...)
}

// newSessionServer 起一个假上游：对话返回一段正常 SSE，心跳回 {"status":"ok"}。
func newSessionServer(t *testing.T, rec *heartbeatRecorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, EpChatSessionHeartbeat):
			rec.mu.Lock()
			rec.beats = append(rec.beats, r.Header.Get(HeaderUserSessionID))
			rec.beatQ = append(rec.beatQ, r.URL.RawQuery)
			rec.beatHM = append(rec.beatHM, SignedHeaderNames(r.Header.Get("Authorization")))
			rec.mu.Unlock()
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		case strings.HasSuffix(r.URL.Path, EpChatCompletions):
			rec.mu.Lock()
			rec.chatSID = r.Header.Get(HeaderUserSessionID)
			rec.chatSes = r.Header.Get(HeaderSessionID)
			rec.mu.Unlock()
			_, _ = io.WriteString(w, `data:{"choices":[{"delta":{"content":"hi"},"index":0}]}`+"\n")
			_, _ = io.WriteString(w, "data:[DONE]\n")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

const chatBodyFixture = `{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"x"}]}`

// TestChatStreamSendsUserSessionIDWithSameValue 对话必须同时带两个会话头且同值。
func TestChatStreamSendsUserSessionIDWithSameValue(t *testing.T) {
	var rec heartbeatRecorder
	srv := newSessionServer(t, &rec)
	c := NewWithBase(srv.URL)

	rc, status, _, err := c.ChatStream(makeAuth("u1"), []byte(chatBodyFixture))
	if err != nil || status != http.StatusOK {
		t.Fatalf("ChatStream status=%d err=%v", status, err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()

	chatSID, chatSes, _, _, _ := rec.snapshot()
	if chatSID == "" {
		t.Fatal("对话请求必须带 user-session-id（缺它上游按每个新 session 占坑，3 个即 TM.00001041）")
	}
	if chatSID != chatSes {
		t.Fatalf("user-session-id 必须与 Session-Id 同值（官方形态），实际 %q vs %q", chatSID, chatSes)
	}
}

// TestChatStreamReleasesSlotOnEOF 读完流必须发一次 idle 心跳，且针对本会话。
func TestChatStreamReleasesSlotOnEOF(t *testing.T) {
	var rec heartbeatRecorder
	srv := newSessionServer(t, &rec)
	c := NewWithBase(srv.URL)

	rc, _, _, err := c.ChatStream(makeAuth("u1"), []byte(chatBodyFixture))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(rc) // 读到 EOF 触发心跳
	_ = rc.Close()        // 再 Close 不得重复发

	chatSID, _, beats, beatQ, beatHM := rec.snapshot()
	if len(beats) != 1 {
		t.Fatalf("EOF+Close 合计应恰好 1 次心跳（幂等），实际 %d 次: %v", len(beats), beats)
	}
	if beats[0] != chatSID {
		t.Fatalf("心跳必须释放本次对话的会话，心跳 sid=%q 对话 sid=%q", beats[0], chatSID)
	}
	if !strings.Contains(beatQ[0], "status=idle") {
		t.Fatalf("心跳必须带 status=idle（官方 status 走 query 参数），实际 %q", beatQ[0])
	}
	if strings.Contains(beatHM[0], HeaderUserSessionID) {
		t.Fatalf("心跳的 user-session-id 必须在签名**之后**追加（进 canonical 会 401 APIG.0301），SignedHeaders=%q", beatHM[0])
	}
}

// TestChatStreamReleasesSlotOnEarlyClose 客户端不读到 EOF 就 Close 也必须发心跳。
//
// 线上正是这个形态才让坑位持续累积：handler 收到 [DONE] 就关流，永远走不到 EOF。
func TestChatStreamReleasesSlotOnEarlyClose(t *testing.T) {
	var rec heartbeatRecorder
	srv := newSessionServer(t, &rec)
	c := NewWithBase(srv.URL)

	rc, _, _, err := c.ChatStream(makeAuth("u1"), []byte(chatBodyFixture))
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close() // 一个字节都不读

	chatSID, _, beats, _, _ := rec.snapshot()
	if len(beats) != 1 {
		t.Fatalf("提前 Close 必须发 1 次心跳，实际 %d 次", len(beats))
	}
	if beats[0] != chatSID {
		t.Fatalf("心跳 sid 与对话 sid 不一致: %q vs %q", beats[0], chatSID)
	}
}

// TestChatStreamReleasesSlotOnUpstreamError 非 2xx 也要归还坑位。
func TestChatStreamReleasesSlotOnUpstreamError(t *testing.T) {
	var rec heartbeatRecorder
	var chatSID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, EpChatSessionHeartbeat):
			rec.mu.Lock()
			rec.beats = append(rec.beats, r.Header.Get(HeaderUserSessionID))
			rec.mu.Unlock()
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		default:
			rec.mu.Lock()
			chatSID = r.Header.Get(HeaderUserSessionID)
			rec.mu.Unlock()
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error_code":"InferHub.ModelArts.81114.429","error_msg":"rate limit"}`)
		}
	}))
	defer srv.Close()
	c := NewWithBase(srv.URL)

	_, status, _, err := c.ChatStream(makeAuth("u1"), []byte(chatBodyFixture))
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusTooManyRequests {
		t.Fatalf("上游 429 必须原文透传，实际 %d", status)
	}
	_, _, beats, _, _ := rec.snapshot()
	if len(beats) != 1 || beats[0] != chatSID {
		t.Fatalf("非 2xx 也必须归还坑位，实际 %v（对话 sid=%q）", beats, chatSID)
	}
}

// TestSessionReleaserIsIdempotent 直接对包装器做幂等断言（Read/Close 交错）。
func TestSessionReleaserIsIdempotent(t *testing.T) {
	var n int
	r := &sessionReleaser{
		ReadCloser: io.NopCloser(strings.NewReader("data:[DONE]\n")),
		fire:       func() { n++ },
	}
	_, _ = io.ReadAll(r)
	_, _ = io.ReadAll(r) // 第二次读仍是 EOF
	_ = r.Close()
	if n != 1 {
		t.Fatalf("心跳必须幂等，实际触发 %d 次", n)
	}
}
