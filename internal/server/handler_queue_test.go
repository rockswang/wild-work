// handler_queue_test.go 锁定「排队 → 换下一个账号；全号排队才失败」与
// 「传输层错误不换号」两条语义（2026-10-20 生产事故后的决议）。
//
// 生产形态（qodercn/deepseek-flash）：上游 200 + 首帧 10605 isQueued（queueCount 8407,
// waitTime 509s），旧实现把它当普通 chunk 透传 ⇒ 界面卡在「正在等待模型…」180s。
// 新语义：首帧即识别 → 冷却「号×该模型」（30s，取上游 retryAfter）→ 换下一个号；
// 三个号都排队才 429 model_queued（带 Retry-After），而不是 503。
package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// queueUpstream 假上游：ChatStream 把账号 UID 写进 body 交给 Stream；
// Stream 对 queued 里的账号返回 *provider.Queued（一字节不写），其余正常回一帧 SSE。
type queueUpstream struct {
	mu       sync.Mutex
	queued   map[string]bool
	calls    []string
	netError bool // true 时 ChatStream 直接返回传输层错误
}

func (u *queueUpstream) RefreshToken(*auth.Auth) error { return nil }

func (u *queueUpstream) ChatStream(a *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
	u.mu.Lock()
	u.calls = append(u.calls, a.UID)
	u.mu.Unlock()
	if u.netError {
		// 与生产同形的传输层错误（net/http 的 url.Error 被 fmt 包装后仍含特征文案）。
		return nil, 0, nil, errors.New(`Post "https://gw/v1/chat": net/http: timeout awaiting response headers`)
	}
	return io.NopCloser(strings.NewReader(a.UID)), http.StatusOK, nil, nil
}

func (u *queueUpstream) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	raw, _ := io.ReadAll(r)
	uid := string(raw)
	u.mu.Lock()
	isQueued := u.queued[uid]
	u.mu.Unlock()
	if isQueued {
		// 关键：不写任何字节就上抛排队错误（生产里这帧原本会被当 chunk 透传）。
		return nil, &provider.Queued{ModelKey: "dfmodel", QueueCount: 8407,
			RetryAfter: 30 * time.Second, WaitSeconds: 509}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"up",`+
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`+"\n\n"+
		`data: {"id":"c1","object":"chat.completion.chunk","model":"up",`+
		`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
		"data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil, nil
}

func (u *queueUpstream) Aggregate(r io.Reader, model string) (map[string]any, error) {
	return nil, errors.New("not used")
}

func (u *queueUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (u *queueUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *queueUpstream) UserResource(*auth.Auth) (int64, error) { return 1, nil }
func (u *queueUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *queueUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (u *queueUpstream) Classify(int, string) provider.ErrKind {
	return provider.ErrNone
}

// newQueueHandler 三账号 qodercn Handler；queued 里的 UID 会在流首帧返回排队。
func newQueueHandler(t *testing.T, queued ...string) (*Handler, *pool.Pool, *queueUpstream) {
	t.Helper()
	p := pool.New(t.TempDir() + "/state.json")
	for _, uid := range []string{"uA", "uB", "uC"} {
		p.Add(&auth.Auth{Kind: "qodercn", AccessToken: "at", ExpiresAt: 4102444800, UID: uid})
		p.SetCreditDetail(uid, 1000, 0, 0, time.Time{})
	}
	up := &queueUpstream{queued: map[string]bool{}}
	for _, uid := range queued {
		up.queued[uid] = true
	}
	rt := &Runtime{Kind: provider.QoderCN, Pool: p, Upstream: up}
	h := NewHandler(Config{Runtimes: map[provider.Kind]*Runtime{provider.QoderCN: rt}})
	return h, p, up
}

func postStream(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"qodercn/deepseek-flash","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestQueuedRotatesToNextAccount 首个号排队 → 冷却「号×模型」并换下一个号，最终 200。
func TestQueuedRotatesToNextAccount(t *testing.T) {
	h, p, up := newQueueHandler(t, "uA")
	rec := postStream(t, h)

	if rec.Code != http.StatusOK {
		t.Fatalf("换号后应成功 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	up.mu.Lock()
	calls := append([]string(nil), up.calls...)
	up.mu.Unlock()
	if len(calls) != 2 || calls[0] != "uA" {
		t.Fatalf("应先打 uA、排队后换号再打一次，实际调用序列 %v", calls)
	}
	st, _ := p.Status("uA")
	if st.Cooling {
		t.Fatalf("排队只该冷却「号×模型」，不应整号冷却: %+v", st)
	}
	var until time.Time
	for _, mc := range st.ModelCooling {
		if mc.Model == "deepseek-flash" {
			until = mc.Until
		}
	}
	if until.IsZero() {
		t.Fatalf("uA 的 deepseek-flash 应进入模型级冷却: %+v", st.ModelCooling)
	}
	if d := time.Until(until); d > 31*time.Second || d < 25*time.Second {
		t.Fatalf("冷却时长应取上游 retryAfter≈30s，实际 %v", d)
	}
}

// TestAllQueuedReturns429WithRetryAfter 三个号全排队 → 429 model_queued + Retry-After，
// 不是 503，也不再无限重试。
func TestAllQueuedReturns429WithRetryAfter(t *testing.T) {
	h, _, up := newQueueHandler(t, "uA", "uB", "uC")
	rec := postStream(t, h)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("全号排队应 429，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	body := rec.Body.String()
	for _, want := range []string{"model_queued", "queue_count", "wait_seconds", "dfmodel"} {
		if !strings.Contains(body, want) {
			t.Errorf("出口错误体应包含 %q：%s", want, body)
		}
	}
	up.mu.Lock()
	n := len(up.calls)
	up.mu.Unlock()
	if n != 3 {
		t.Fatalf("应把 3 个号各试一次，实际 %d 次", n)
	}
}

// TestTransportErrorDoesNotRotate 传输层错误（同一 host 换号必撞同一面墙）：
// 只打 1 次就 502，不再把 1 次失败放大成 N 次 × 最长 120s。
func TestTransportErrorDoesNotRotate(t *testing.T) {
	h, _, _ := newQueueHandler(t)
	h.cfg.Runtimes[provider.QoderCN].Upstream.(*queueUpstream).netError = true
	rec := postStream(t, h)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("传输层错误应 502，实际 %d：%s", rec.Code, rec.Body.String())
	}
	up := h.cfg.Runtimes[provider.QoderCN].Upstream.(*queueUpstream)
	up.mu.Lock()
	n := len(up.calls)
	up.mu.Unlock()
	if n != 1 {
		t.Fatalf("传输层错误不得换号（应只打 1 个号），实际 %d 次", n)
	}
}

// quotaFrameUpstream 假上游：对 hardUIDs 里的账号在流首帧返回 ErrHardCredit
// （模拟 qodercom 的 envelope 403 里包 code 110/112 额度耗尽帧），其余账号正常出字。
type quotaFrameUpstream struct {
	queueUpstream
	hardUIDs map[string]bool
}

func (u *quotaFrameUpstream) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	raw, _ := io.ReadAll(r)
	uid := string(raw)
	// 用 Classified 包装：流式路径靠 Kind() 认出 ErrHardCredit（provider.Error 的
	// 字段名就叫 Kind，不能再有同名方法，故生产代码也用同一包装）。
	if u.hardUIDs[uid] {
		// 关键：不写任何字节就上抛（真实实现里这帧原本会被当 chunk 透传，客户端干等 180s）。
		return nil, &provider.Classified{Err: &provider.Error{Kind: provider.ErrHardCredit, Status: 402,
			Msg: "upstream quota exhausted (code 112): Billing daily count exceeded"}}
	}
	// 正常账号：自己写一帧，不能转交给 queueUpstream（reader 已被读空）。
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, `data: {"id":"c1","object":"chat.completion.chunk","model":"up",`+
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`+"\n\n"+
		"data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil, nil
}

// TestHardCreditFirstFrameRotates 首帧额度耗尽（qodercom code 110/112）：
// 一字节未写 ⇒ 整号硬冷却 + 换下一个号，最终 200；不是「只罚不换」让客户端干等。
func TestHardCreditFirstFrameRotates(t *testing.T) {
	h, p, _ := newQueueHandler(t)
	up := &quotaFrameUpstream{queueUpstream: queueUpstream{queued: map[string]bool{}},
		hardUIDs: map[string]bool{"uA": true}}
	h.cfg.Runtimes[provider.QoderCN].Upstream = up

	rec := postStream(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("换号后应成功 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	up.mu.Lock()
	calls := append([]string(nil), up.calls...)
	up.mu.Unlock()
	if len(calls) != 2 || calls[0] != "uA" {
		t.Fatalf("应先打 uA、额度耗尽后换号再打一次，实际调用序列 %v", calls)
	}
	st, _ := p.Status("uA")
	if !st.Cooling {
		t.Fatalf("额度耗尽应整号硬冷却（ErrHardCredit），实际 status=%+v", st)
	}
}

// TestHardCreditAllAccountsExhausted 全部账号额度耗尽：逐号试满后透传 402，
// 而不是 503「渠道不可用」（用户需要看到「没钱了/该充值」这个可行动结论）。
func TestHardCreditAllAccountsExhausted(t *testing.T) {
	h, _, _ := newQueueHandler(t)
	up := &quotaFrameUpstream{queueUpstream: queueUpstream{queued: map[string]bool{}},
		hardUIDs: map[string]bool{"uA": true, "uB": true, "uC": true}}
	h.cfg.Runtimes[provider.QoderCN].Upstream = up

	rec := postStream(t, h)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("全号额度耗尽应透传 402，实际 %d：%s", rec.Code, rec.Body.String())
	}
	up.mu.Lock()
	n := len(up.calls)
	up.mu.Unlock()
	if n != 3 {
		t.Fatalf("应把 3 个号各试一次，实际 %d 次", n)
	}
}
