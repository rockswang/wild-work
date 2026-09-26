package glm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"wild-work/internal/auth"
)

// 本文件锁定「每日登录积分领取」（`daily_login_score`）的行为。
//
// 背景（2026-09-26 实测）：该接口是**在线、真实**的每日积分来源
// （从 Web 主包挖出，网页版页面加载时自己就会调）。
// 响应语义：
//
//	{"status":0}                    → 领取成功
//	{"status":10001,"今日已领取"}    → 今日已领（幂等，非错误）
//
// 关键要求：**幂等**——重复调用不得报错、不得重复发放。

// dailyScoreUpstream 模拟 daily_login_score 上游。
type dailyScoreUpstream struct {
	mu      sync.Mutex
	calls   int
	respond func(callIndex int) (int, string) // (status, message)
}

func newDailyScoreServer(t *testing.T, respond func(int) (int, string)) (*httptest.Server, *dailyScoreUpstream) {
	up := &dailyScoreUpstream{respond: respond}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(r.URL.Path, "user/refresh"):
			body, _ := json.Marshal(map[string]any{
				"status": 0, "message": "success",
				"result": map[string]any{
					"user_id": "u-1", "access_token": "AT-fresh", "refresh_token": "RT-fresh",
				},
			})
			_, _ = w.Write(body)
			return

		case strings.Contains(r.URL.Path, "daily_login_score"):
			up.mu.Lock()
			up.calls++
			n := up.calls
			up.mu.Unlock()
			st, msg := up.respond(n)
			body, _ := json.Marshal(map[string]any{
				"status": st, "message": msg, "result": nil,
			})
			_, _ = w.Write(body)
			return

		default:
			_, _ = w.Write([]byte(`{"status":0,"message":"success","result":{}}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, up
}

func newTestAuth() *auth.Auth {
	return &auth.Auth{
		Kind: "glm", AccessToken: "AT-old", RefreshToken: "RT-old",
		ExpiresAt: 4102444800, UID: "u-1",
	}
}

// TestDailyLoginScoreFirstClaimSucceeds 首次领取（status=0）应报「已领取」。
func TestDailyLoginScoreFirstClaimSucceeds(t *testing.T) {
	srv, up := newDailyScoreServer(t, func(n int) (int, string) {
		return 0, "success"
	})
	c := NewWithBase(srv.URL)

	note := c.claimDailyLoginScore(newTestAuth())

	if !strings.Contains(note, "已领取") {
		t.Errorf("首次领取的说明 = %q, want 含「已领取」", note)
	}
	if strings.Contains(note, "失败") {
		t.Errorf("首次领取不应报失败: %q", note)
	}
	up.mu.Lock()
	calls := up.calls
	up.mu.Unlock()
	if calls != 1 {
		t.Errorf("应调用 1 次，实际 %d", calls)
	}
	t.Logf("✅ 首次领取: %q", note)
}

// TestDailyLoginScoreAlreadyClaimedIsIdempotent 今日已领取（status=10001）
// 必须视为**正常**（非错误），且文案明确。
//
// 这是实测的真实响应——重复调用时上游就返回它。
func TestDailyLoginScoreAlreadyClaimedIsIdempotent(t *testing.T) {
	srv, _ := newDailyScoreServer(t, func(n int) (int, string) {
		return 10001, "今日已领取"
	})
	c := NewWithBase(srv.URL)

	note := c.claimDailyLoginScore(newTestAuth())

	if strings.Contains(note, "失败") {
		t.Errorf("「今日已领取」不得被当成失败: %q", note)
	}
	if !strings.Contains(note, "已领") {
		t.Errorf("说明 = %q, want 含「已领」", note)
	}
	t.Logf("✅ 今日已领取（幂等）: %q", note)
}

// TestDailyLoginScoreRepeatedCallsStayIdempotent 连续调用多次，行为必须一致。
//
// 对应调度器可能一天多时段触发（09:00 / 21:00）的现实：
// 第二次起上游返回「今日已领取」，**不得**产生错误或异常文案。
func TestDailyLoginScoreRepeatedCallsStayIdempotent(t *testing.T) {
	srv, up := newDailyScoreServer(t, func(n int) (int, string) {
		if n == 1 {
			return 0, "success"
		}
		return 10001, "今日已领取"
	})
	c := NewWithBase(srv.URL)
	a := newTestAuth()

	first := c.claimDailyLoginScore(a)
	for i := 2; i <= 4; i++ {
		note := c.claimDailyLoginScore(a)
		if strings.Contains(note, "失败") {
			t.Errorf("第 %d 次调用不应报失败: %q", i, note)
		}
	}

	up.mu.Lock()
	calls := up.calls
	up.mu.Unlock()
	if calls != 4 {
		t.Errorf("应调用 4 次，实际 %d", calls)
	}
	t.Logf("✅ 4 次调用全部安全；首次=%q", first)
}

// TestDailyLoginScoreNotFoundDegradesSilently 接口 404 时应静默降级。
//
// 理由：该接口可能尚未对所有账号开放。若它不存在，
// 不该让用户看到无意义的错误（积分可能由服务端自动发放）。
func TestDailyLoginScoreNotFoundDegradesSilently(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "user/refresh") {
			body, _ := json.Marshal(map[string]any{
				"status": 0, "message": "success",
				"result": map[string]any{"user_id": "u-1", "access_token": "AT", "refresh_token": "RT"},
			})
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"status":1,"message":"404 Not Found"}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	note := c.claimDailyLoginScore(newTestAuth())

	if note != "" {
		t.Errorf("404 应静默降级（返回空串），实际 %q", note)
	}
	t.Log("✅ 接口不存在时静默降级（不误导用户）")
}

// TestDailyLoginScoreOtherBizCodeReported 其它业务码应如实反映但简短。
func TestDailyLoginScoreOtherBizCodeReported(t *testing.T) {
	srv, _ := newDailyScoreServer(t, func(n int) (int, string) {
		return 40001, "活动未开放"
	})
	c := NewWithBase(srv.URL)

	note := c.claimDailyLoginScore(newTestAuth())

	if note == "" {
		t.Error("非 404 的业务码不应静默（应如实反映）")
	}
	if !strings.Contains(note, "活动未开放") {
		t.Errorf("说明 = %q, want 含上游 message", note)
	}
	t.Logf("✅ 业务码如实反映: %q", note)
}

// TestDailyCheckinReportIncludesDailyScore 集成：DailyCheckinReport 应
// 同时完成保活对话与每日登录积分领取，且两者都体现在结果文案里。
func TestDailyCheckinReportIncludesDailyScore(t *testing.T) {
	var dailyCalled bool
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(r.URL.Path, "user/refresh"):
			body, _ := json.Marshal(map[string]any{
				"status": 0, "message": "success",
				"result": map[string]any{"user_id": "u-1", "access_token": "AT", "refresh_token": "RT"},
			})
			_, _ = w.Write(body)

		case strings.Contains(r.URL.Path, "daily_login_score"):
			mu.Lock()
			dailyCalled = true
			mu.Unlock()
			_, _ = w.Write([]byte(`{"status":0,"message":"success"}`))

		case strings.Contains(r.URL.Path, "member_info"):
			_, _ = w.Write([]byte(`{"status":0,"message":"success","result":{"left_score":447224,"left_token":17667093,"score_rule":"免费用户，登录赠送200积分/天"}}`))

		case strings.Contains(r.URL.Path, "assistant/stream"):
			// 保活对话：最小合法 SSE
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"c1\",\"status\":\"init\",\"parts\":[{\"logic_id\":\"L1\",\"status\":\"init\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"conversation_id\":\"c1\",\"status\":\"finish\",\"parts\":[{\"logic_id\":\"L1\",\"status\":\"finish\",\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}]}\n\n"))

		default:
			// activity-api 旧签到：模拟已下线
			_, _ = w.Write([]byte(`{"status":500,"message":"活动已结束"}`))
		}
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	rep, err := c.DailyCheckinReport(newTestAuth())
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}

	mu.Lock()
	called := dailyCalled
	mu.Unlock()

	if !called {
		t.Error("❌ DailyCheckinReport 未调用 daily_login_score（每日积分会漏领）")
	}
	if rep.Msg == "" {
		t.Error("结果文案不应为空")
	}
	// 积分余额应出现在文案里
	if !strings.Contains(rep.Msg, "4472") {
		t.Errorf("文案应含积分余额 4472: %q", rep.Msg)
	}
	t.Logf("✅ 集成通过: %q", rep.Msg)
}