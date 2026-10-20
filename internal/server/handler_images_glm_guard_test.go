// handler_images_glm_guard_test.go glm 对话语义简化（2026-10-20 决议）在 server 层的端到端回归：
// 多轮/assistant 请求必须 400 透传、不罚账号、不触发上游请求。
package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/glm"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// TestGLMChatMultiTurnRejected400 多轮请求 → 400 glm_unsupported_conversation，
// 上游零请求、账号零惩罚（ErrBadParams 透传语义）。
func TestGLMChatMultiTurnRejected400(t *testing.T) {
	m := &glmE2EUpstream{} // 见下方：httptest 形态的 glm mock（复用 glm 包的 e2e 模式太重，这里只数调用次数）
	srv := httptest.NewServer(m)
	defer srv.Close()

	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "AT", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.GLM: {Kind: provider.GLM, Pool: p, Upstream: glm.NewWithBase(srv.URL)},
		},
		APIKey: "", ErrThreshold: 3,
	})

	// 两个 user 段落 → 拒绝
	body := `{"model":"glm/chatglm","messages":[{"role":"user","content":"一问"},{"role":"user","content":"二问"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("多 user 请求 status = %d, want 400；body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "glm_unsupported_conversation") {
		t.Errorf("错误体缺 glm_unsupported_conversation: %s", rec.Body.String())
	}
	// assistant 段落 → 拒绝
	body2 := `{"model":"glm/chatglm","messages":[{"role":"user","content":"一问"},{"role":"assistant","content":"一答"},{"role":"user","content":"二问"}]}`
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body2)))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("assistant 段落 status = %d, want 400", rec2.Code)
	}
	// 上游零请求、账号零惩罚
	if m.calls > 0 {
		t.Errorf("上游被请求 %d 次，want 0（本地拒绝不应打上游）", m.calls)
	}
	if st, ok := p.Status("u1"); ok && (st.Cooling || st.SessionDead) {
		t.Errorf("账号不应被惩罚: %+v", st)
	}
}

// glmE2EUpstream 是一台数请求的假 chatglm.cn（路径全部 200 空响应即可）。
type glmE2EUpstream struct{ calls int }

func (m *glmE2EUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.calls++
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":0,"message":"success","result":{}}`))
}
