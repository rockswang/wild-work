// handler_upstream_error_test.go 回归：上游错误分类 → 账号策略的端到端契约。
//
// 核心断言是「请求级错误不罚号」：内容拦截 / 上下文超限 / 图片格式无效 / 出站 body
// 畸形这几类，是请求内容的问题，与账号健康无关——同一 body 换任何账号结果都一样。
// 一旦分类退化（例如业务码因 JSON 空白漏判、或图片错误落到 ErrClient），
// handler 就会走 default 分支 NoteError，把健康账号喂到冷却。
//
// 假上游的 Classify 直接委托真实实现（internal/upstream.Classify），
// 所以这里同时覆盖「分类」与「策略」两段。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// errUpstream 固定返回一组 (status, body)，Classify 走真实实现。
type errUpstream struct {
	status int
	body   string
}

func (errUpstream) RefreshToken(*auth.Auth) error { return nil }

func (e errUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return io.NopCloser(strings.NewReader("")), e.status, []byte(e.body), nil
}

func (errUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (errUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (errUpstream) UserResource(*auth.Auth) (int64, error) { return 1, nil }
func (errUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (errUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (errUpstream) Classify(status int, body string) provider.ErrKind {
	return upstream.Classify(status, body)
}
func (errUpstream) Stream(w http.ResponseWriter, _ io.Reader, _ string) (map[string]any, error) {
	return nil, nil
}
func (errUpstream) Aggregate(io.Reader, string) (map[string]any, error) {
	return map[string]any{}, nil
}

// newErrHandler 单渠道 + 单账号；ErrThreshold=1 让「罚号」立刻表现为 Cooling，
// 从而把「是否调了 NoteError」变成一个可断言的可见状态。
func newErrHandler(status int, body string) (*Handler, *pool.Pool) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "t", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	rt := &Runtime{Kind: provider.Qoder, Pool: p, Upstream: errUpstream{status: status, body: body}}
	h := NewHandler(Config{
		Runtimes:     map[provider.Kind]*Runtime{provider.Qoder: rt},
		ErrThreshold: 1,
		ErrCooldown:  time.Hour,
		HardCooldown: time.Hour,
		SoftCooldown: time.Minute,
	})
	return h, p
}

func postChat(h *Handler) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"qoder/glm-5.2","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	h.ServeHTTP(rec, req)
	return rec
}

// TestUpstreamRequestLevelErrorsDoNotPunishAccount 请求级错误：原文透传 + 账号零动作。
func TestUpstreamRequestLevelErrorsDoNotPunishAccount(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"图片格式无效（文案）", 400, `{"code":11135,"msg":"invalid_image_data"}`},
		{"图片格式无效（JSON 空白 + 引号码）", 400, `{"code": "11135", "msg": "image rejected"}`},
		{"图片解析失败（信封 code 是 11101）", 400, `{"code":11101,"msg":"Parse message failed: invalid image_url content at index 2"}`},
		{"上下文超限（JSON 空白）", 400, `{"code": 11115, "msg": "prompt is too long"}`},
		{"出站 body 畸形", 400, `{"code":11101,"msg":"Unmarshal chat params failed"}`},
		{"内容策略拦截", 400, `{"code":1,"msg":"blocked by security policy"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, p := newErrHandler(tc.status, tc.body)
			rec := postChat(h)

			if rec.Code != tc.status {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.status, rec.Body.String())
			}
			// 上游原文透传，不包装
			if got := rec.Body.String(); got != tc.body {
				t.Fatalf("body=%q want 原文 %q", got, tc.body)
			}
			st, ok := p.Status("u1")
			if !ok {
				t.Fatal("账号消失")
			}
			if st.Cooling {
				t.Fatalf("请求级错误不得冷却账号：reason=%q until=%v", st.Reason, st.Until)
			}
			if st.ErrCount != 0 {
				t.Fatalf("请求级错误不得累计 errCount：%d", st.ErrCount)
			}
			if st.Disabled {
				t.Fatalf("请求级错误不得禁用账号")
			}
		})
	}
}

// TestUpstreamUnknown4xxStillRotatesWithoutPunishing 对照：未知业务错误归 ErrClient ——
// 只轮转、绝不累计错误计数（PR #145：错误跟着请求体走，换号必然同样拒收，罚号等于
// 拿客户端的问题惩罚健康账号）。对照另一侧换成 5xx（ErrServer 仍 NoteError）才能证明
// 「分类真的被区分」，而不是「handler 根本不罚号」。
func TestUpstreamUnknown4xxStillRotatesWithoutPunishing(t *testing.T) {
	h, p := newErrHandler(400, `{"code":9999,"msg":"unknown business error"}`)
	rec := postChat(h)
	if rec.Code != 400 {
		t.Fatalf("status=%d want 400", rec.Code)
	}
	st, _ := p.Status("u1")
	if st.ErrCount != 0 || st.Cooling {
		t.Fatalf("未知 4xx（ErrClient）不得累计 errCount：errCount=%d cooling=%v", st.ErrCount, st.Cooling)
	}
}

// TestUpstream429With14018UsesHardCreditCooldown 429 + 14018（积分耗尽）按硬冷却弃号。
func TestUpstream429With14018UsesHardCreditCooldown(t *testing.T) {
	h, p := newErrHandler(429, `{"code":14018,"msg":"credits exhausted"}`)
	rec := postChat(h)
	if rec.Code != 429 {
		t.Fatalf("status=%d want 429", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("14018 应冷却账号")
	}
	if st.Reason != "余额/权益不足" {
		t.Fatalf("reason=%q want 余额/权益不足（硬冷却）", st.Reason)
	}
}

// TestUpstream429Without14018UsesSoftCooldown 429 无 14018 仍是软限流
// （限流 body 高频带 quota 措辞，不能被文案带去硬冷却）。
func TestUpstream429Without14018UsesSoftCooldown(t *testing.T) {
	h, p := newErrHandler(429, `{"code":9999,"msg":"quota exceeded"}`)
	rec := postChat(h)
	if rec.Code != 429 {
		t.Fatalf("status=%d want 429", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Cooling || st.Reason != "429 rate limit" {
		t.Fatalf("应为软限流，实际 cooling=%v reason=%q", st.Cooling, st.Reason)
	}
}
