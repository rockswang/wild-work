package raccoon

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// TestCheckinClaimed 首次调用（popups 非空）→ claimed + 金额正确。
//
// 这是签到接入的**核心判据**：上游没有独立领取端点，发放靠 setting_info 触发，
// 而「本次是否真的领到」只能从 point_grant_popups 读出（余额会被同期对话消耗干扰）。
func TestCheckinClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpOfficeSettingInfo {
			t.Errorf("路径不符：%s（应走 %s）", r.URL.Path, EpOfficeSettingInfo)
		}
		if r.Method != http.MethodGet {
			t.Errorf("方法不符：%s（上游是 GET）", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer at-1" {
			t.Errorf("鉴权头不符：%q", got)
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success","data":{
			"available_points":3896,
			"point_grant_popups":[
				{"source":"daily","display_name":"每日积分发放","points":300,"expire_at":"2026-10-21T23:59:59+08:00"}
			]}}`))
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	rep, err := c.DailyCheckinReport(&auth.Auth{AccessToken: "at-1", UID: "u1"})
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinClaimed {
		t.Fatalf("Status = %v, want claimed", rep.Status)
	}
	if rep.Amount != 300 {
		t.Fatalf("Amount = %d, want 300", rep.Amount)
	}
	if !strings.Contains(rep.Msg, "300") {
		t.Fatalf("Msg 应含金额，实际 %q", rep.Msg)
	}
	if rep.Status.Retryable() {
		t.Fatal("claimed 不得可重试（否则窗口内会反复触发）")
	}
}

// TestCheckinAlready 当天已领过（popups 为空数组）→ already_claimed。
//
// 幂等语义：上游按天去重，重复调用不再发放，此时必须判成「当日完成」，
// **不得**判成 no_campaign（那会让调度器整天重试）。
func TestCheckinAlready(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"available_points":3896,"point_grant_popups":[]}}`))
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	rep, err := c.DailyCheckinReport(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("Status = %v, want already_claimed", rep.Status)
	}
	if rep.Status.Retryable() {
		t.Fatal("already_claimed 不得可重试")
	}
}

// TestCheckinMissingPopupsField 上游未下发 popups 字段（形状突变）时按「已领」处理，
// 不得误判成「领到了 0 分」或报错——本渠道字段缺失与空数组同义。
func TestCheckinMissingPopupsField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"available_points":100}}`))
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	rep, err := c.DailyCheckinReport(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("Status = %v, want already_claimed", rep.Status)
	}
}

// TestCheckinSessionDeadIsTyped 401 必须返回**类型化**的 ErrSessionDead：
// 调度器走 errors.As 判定自愈（refresh + 重试），裸 fmt.Errorf 会让它恒为 false
// （AGENTS 不变量 19 / 23）。
func TestCheckinSessionDeadIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":200003,"message":"authorization_verify_error"}`))
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	rep, err := c.DailyCheckinReport(&auth.Auth{AccessToken: "bad", UID: "u1"})
	if err == nil {
		t.Fatal("401 应返回错误")
	}
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("错误必须是 *provider.Error（调度器 errors.As 依赖），实际 %T", err)
	}
	if pe.Kind != provider.ErrSessionDead {
		t.Fatalf("Kind = %v, want session_dead", pe.Kind)
	}
	if rep.Status != provider.CheckinNoToken {
		t.Fatalf("Status = %v, want no_token（可重试：refresh 后可能恢复）", rep.Status)
	}
	if !rep.Status.Retryable() {
		t.Fatal("no_token 必须可重试")
	}
}

// TestCheckinServerErrorRetryable 上游 5xx / 网络抖动 → error 且可重试
// （窗口内再试一次往往就好了，不得直接判当日完成）。
func TestCheckinServerErrorRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`internal error`))
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	rep, err := c.DailyCheckinReport(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err == nil {
		t.Fatal("500 应返回错误")
	}
	if rep.Status != provider.CheckinError {
		t.Fatalf("Status = %v, want error", rep.Status)
	}
	if !rep.Status.Retryable() {
		t.Fatal("error 必须可重试")
	}
}

// TestCheckinMultiplePopups popups 含多条（如 daily + desktop_login）时金额求和、
// 且仍算 claimed。上游未来新增发放种类不应被漏算。
func TestCheckinMultiplePopups(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"available_points":500,"point_grant_popups":[
			{"source":"daily","display_name":"每日积分发放","points":300},
			{"source":"desktop_login","display_name":"桌面端登录奖励","points":50}
		]}}`))
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	rep, err := c.DailyCheckinReport(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinClaimed || rep.Amount != 350 {
		t.Fatalf("Status=%v Amount=%d, want claimed/350", rep.Status, rep.Amount)
	}
}

// TestDailyCheckinWrapper 旧通道（非结构化）语义：claimed/already → nil，其余 → error。
func TestDailyCheckinWrapper(t *testing.T) {
	claimed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"available_points":1,"point_grant_popups":[{"source":"daily","points":300}]}}`))
	}))
	defer claimed.Close()
	already := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"available_points":1,"point_grant_popups":[]}}`))
	}))
	defer already.Close()

	c := New()
	c.AuthBase = claimed.URL
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
		t.Fatalf("claimed 应返回 nil，实际 %v", err)
	}
	c.AuthBase = already.URL
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
		t.Fatalf("already 应返回 nil，实际 %v", err)
	}
}

// TestCheckinUsesAuthBase 守门：签到必须走 AuthBase（站点根前缀），
// 不得误用 Base（推理网关前缀 /api/web/llm/v2）——两者的路径前缀不同。
func TestCheckinUsesAuthBase(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"code":0,"data":{"available_points":1,"point_grant_popups":[]}}`))
	}))
	defer srv.Close()

	c := New()
	c.AuthBase = srv.URL
	c.Base = srv.URL + "/api/web/llm/v2" // 故意给个不同的 Base，签到不得用它
	if _, err := c.DailyCheckinReport(&auth.Auth{AccessToken: "at", UID: "u1"}); err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if gotPath != EpOfficeSettingInfo {
		t.Fatalf("请求路径 = %q, want %q", gotPath, EpOfficeSettingInfo)
	}
}
