package loomy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// snapBody 默认快照的 data 段（活动开启、业务日 2026-10-17、三档、今日未领）。
const snapBody = `{"enabled":true,"businessDate":"2026-10-17","config":{` +
	`"enabled":true,"businessDate":"2026-10-17","configVersion":"pet-work-v1","dailyMaxPoints":1000,` +
	`"tiers":[{"tierId":"tier_1","score":60},{"tierId":"tier_2","score":100},{"tierId":"tier_3","score":150}]},` +
	`"status":{"earnedPointsToday":0,"rewardedTierIds":[],"dailyRewardCapped":false}}`

// snapJSON 默认快照（带信封）。上游统一信封是 {code,desc,data}，data 才是快照本体
// （证据：integration test 的 sendJson({code:'000000',data:{...}}) 与 points-service.js:208 的 code 判定）。
var snapJSON = okEnvelope(snapBody)

// newDailyServer 造一个应答 pet-work 两个端点的假上游。
//
// reward 只处理 POST /pet-work/rewards；快照统一回 snapJSON。
// hits 只统计**领奖**请求次数 —— 快照是每次领奖前的必要读，不算额外探测（AGENTS §0.1）。
func newDailyServer(t *testing.T, reward func(w http.ResponseWriter, r *http.Request)) (*Client, *int32) {
	t.Helper()
	return newDailyServerSnap(t, snapJSON, reward)
}

// newDailyServerSnap 同上，但可指定快照 body（空串表示快照端点回 404）。
func newDailyServerSnap(t *testing.T, snap string, reward func(w http.ResponseWriter, r *http.Request)) (*Client, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpPetWork:
			if snap == "" {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, snap)
		case EpPetWorkRewards:
			atomic.AddInt32(&hits, 1)
			reward(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := New()
	c.Base = srv.URL
	return c, &hits
}

// nopReward 不该被调用的领奖分支（被调用即用 500 暴露）。
func nopReward(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = io.WriteString(w, `unexpected rewards call`)
}

func testAuth() *auth.Auth { return &auth.Auth{Kind: "loomy", AccessToken: "0123456789abcdef0123456789abcdef"} }

// okEnvelope 上游成功信封（code 必须是字符串 "000000"，见 points-service.js:208）。
func okEnvelope(data string) string { return `{"code":"000000","desc":"","data":` + data + `}` }

// TestDailyCheckinRequestShape 守门：路径/方法/鉴权头/请求体都必须与官方客户端一致。
//
// 证据：points-service.js:434-450（grantPetWorkReward 只发三个字段）、
// :416-418（快照路径）、:123-125（**只发 token 头**，没有 Authorization）。
func TestDailyCheckinRequestShape(t *testing.T) {
	var gotPath, gotMethod, gotToken, gotBody, gotVersion string
	c, _ := newDailyServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotToken = r.Header.Get("token")
		gotVersion = r.Header.Get("loomy-version")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = io.WriteString(w, okEnvelope(
			`{"businessDate":"2026-10-17","granted":true,"duplicate":false,"grantedPoints":200,"grantedTierIds":["tier_1"]}`))
	})

	a := testAuth()
	if _, err := c.ClaimPetWorkReward(a, "2026-10-17"); err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}

	if gotPath != EpPetWorkRewards {
		t.Fatalf("path = %q, want %q", gotPath, EpPetWorkRewards)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotToken != a.AccessToken {
		t.Fatalf("token 头 = %q, want session", gotToken)
	}
	if gotVersion != AppVersion {
		t.Fatalf("loomy-version = %q", gotVersion)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(gotBody), &body); err != nil {
		t.Fatalf("请求体不是 JSON: %v (%s)", err, gotBody)
	}
	// 请求体**只能**是服务端定义的三个字段（多传的字段会被服务端忽略/拒绝）。
	want := map[string]string{"businessDate": "2026-10-17", "configVersion": "pet-work-v1", "reachedTierId": "tier_1"}
	if len(body) != len(want) {
		t.Fatalf("请求体字段数 = %d, want %d (%v)", len(body), len(want), body)
	}
	for k, v := range want {
		if body[k] != v {
			t.Fatalf("请求体 %s = %v, want %q", k, body[k], v)
		}
	}
}

// TestDailyCheckinClaimed 成功领取。
func TestDailyCheckinClaimed(t *testing.T) {
	c, hits := newDailyServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, okEnvelope(
			`{"businessDate":"2026-10-17","grantBatchId":"b1","granted":true,"duplicate":false,`+
				`"grantedPoints":200,"grantedTierIds":["tier_1"],`+
				`"status":{"earnedPointsToday":200,"rewardedTierIds":["tier_1"],"dailyRewardCapped":false}}`))
	})
	rep, err := c.ClaimPetWorkReward(testAuth(), "2026-10-17")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rep.Status != provider.CheckinClaimed {
		t.Fatalf("Status = %v, want claimed", rep.Status)
	}
	if rep.Amount != 200 {
		t.Fatalf("Amount = %d, want 200", rep.Amount)
	}
	if !strings.Contains(rep.Msg, "200") {
		t.Fatalf("Msg 应含金额: %q", rep.Msg)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
}

// TestDailyCheckinAlready 今日已领 —— 上游用 duplicate:true + granted:false + 0 分表达。
//
// 这与「今天还没开工」不同：它是明确的「已处理过」终态，**不得**再重试，
// 也**不得**报成 claimed（否则面板每天都显示"领到 200"）。
func TestDailyCheckinAlready(t *testing.T) {
	c, _ := newDailyServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, okEnvelope(
			`{"businessDate":"2026-10-17","grantBatchId":"","granted":false,"duplicate":true,`+
				`"grantedPoints":0,"grantedTierIds":[],`+
				`"status":{"earnedPointsToday":1000,"rewardedTierIds":["tier_1","tier_2","tier_3"],"dailyRewardCapped":true}}`))
	})
	rep, err := c.ClaimPetWorkReward(testAuth(), "2026-10-17")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("Status = %v, want already_claimed", rep.Status)
	}
	if rep.Status.Retryable() {
		t.Fatal("already 不应可重试")
	}
	if rep.Amount != 0 {
		t.Fatalf("Amount = %d, want 0（已领不得报发放额）", rep.Amount)
	}
}

// TestDailyCheckinCapped 当日已封顶（dailyRewardCapped）也是完成态。
func TestDailyCheckinCapped(t *testing.T) {
	c, _ := newDailyServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, okEnvelope(
			`{"businessDate":"2026-10-17","granted":false,"duplicate":true,"grantedPoints":0,`+
				`"status":{"earnedPointsToday":1000,"rewardedTierIds":["tier_1"],"dailyRewardCapped":true}}`))
	})
	rep, err := c.ClaimPetWorkReward(testAuth(), "2026-10-17")
	if err != nil || rep.Status != provider.CheckinAlready {
		t.Fatalf("Status = %v err = %v, want already", rep.Status, err)
	}
}

// TestDailyCheckinAllTiersDone 快照显示三档全领完：**不发领奖请求**，直接 already。
func TestDailyCheckinAllTiersDone(t *testing.T) {
	snap := strings.Replace(snapJSON,
		`"status":{"earnedPointsToday":0,"rewardedTierIds":[],"dailyRewardCapped":false}`,
		`"status":{"earnedPointsToday":1000,"rewardedTierIds":["tier_1","tier_2","tier_3"],"dailyRewardCapped":true}`, 1)
	c, hits := newDailyServerSnap(t, snap, nopReward)
	rep, err := c.ClaimPetWorkReward(testAuth(), "2026-10-17")
	if err != nil || rep.Status != provider.CheckinAlready {
		t.Fatalf("Status = %v err = %v, want already", rep.Status, err)
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("已领完仍发了 %d 次领奖请求（浪费上游额度和风控）", got)
	}
}

// TestDailyCheckinNoCampaign 活动关闭 → no_campaign（可重试，但**不是**失败）。
func TestDailyCheckinNoCampaign(t *testing.T) {
	snap := `{"enabled":false,"businessDate":"2026-10-17","config":null,"status":null}`
	c, hits := newDailyServerSnap(t, okEnvelope(snap), nopReward)
	rep, err := c.ClaimPetWorkReward(testAuth(), "2026-10-17")
	if err != nil {
		t.Fatalf("err = %v, want nil（无活动不是错误）", err)
	}
	if rep.Status != provider.CheckinNoCampaign {
		t.Fatalf("Status = %v, want no_campaign", rep.Status)
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("活动关闭却发了 %d 次领奖请求", got)
	}
}

// TestDailyCheckinNotAchieved 未达档位：上游回 granted:false 且没给任何档位。
//
// 这是本项目最常见的真实结果（工具只发推理请求，不走宠物打工）——
// 必须是**明确的可重试状态**，而不是假装领到。
func TestDailyCheckinNotAchieved(t *testing.T) {
	c, _ := newDailyServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, okEnvelope(
			`{"businessDate":"2026-10-17","granted":false,"duplicate":false,"grantedPoints":0,`+
				`"grantedTierIds":[],"status":{"earnedPointsToday":0,"rewardedTierIds":[],"dailyRewardCapped":false}}`))
	})
	rep, err := c.ClaimPetWorkReward(testAuth(), "2026-10-17")
	if rep.Status == provider.CheckinClaimed {
		t.Fatal("未达档位不得报 claimed（假成功）")
	}
	if !rep.Status.Retryable() {
		t.Fatalf("Status = %v 应可重试", rep.Status)
	}
	if err == nil {
		t.Fatal("未领到必须返回 error，不能伪装成功")
	}
	if !strings.Contains(rep.Msg, "轮次") && !strings.Contains(rep.Msg, "档位") {
		t.Fatalf("Msg 应说明未达档位: %q", rep.Msg)
	}
}

// TestDailyCheckinServerError 5xx 可重试，不得报 claimed。
func TestDailyCheckinServerError(t *testing.T) {
	c, _ := newDailyServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `bad gateway`)
	})
	rep, err := c.ClaimPetWorkReward(testAuth(), "2026-10-17")
	if err == nil {
		t.Fatal("5xx 必须返回 error")
	}
	if rep.Status != provider.CheckinError {
		t.Fatalf("Status = %v, want error", rep.Status)
	}
	if !rep.Status.Retryable() {
		t.Fatal("5xx 应可重试")
	}
}

// TestDailyCheckinRateLimited 守门（不变量 15）：429 必须是 ErrSoftRate，
// **不得**被 body 里的 quota 字样归成 ErrHardCredit（会把账号硬冷却 12h）。
func TestDailyCheckinRateLimited(t *testing.T) {
	c, _ := newDailyServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"code":"300005","desc":"quota exceeded, 请求过于频繁"}`)
	})
	_, err := c.ClaimPetWorkReward(testAuth(), "2026-10-17")
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err 类型 = %T, want *provider.Error", err)
	}
	if perr.Kind != provider.ErrSoftRate {
		t.Fatalf("Kind = %v, want ErrSoftRate（429 必须优先于余额类判定）", perr.Kind)
	}
}

// TestDailyCheckinUnauthorized 401 → 类型化 ErrSessionDead（调度器靠 errors.As 自愈）。
func TestDailyCheckinUnauthorized(t *testing.T) {
	// 快照端点也报 401：登录失效在**读快照阶段**就要被抓到，不发第二次请求。
	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"100002","desc":"登录已失效"}`)
	}))
	defer srv401.Close()
	c, _ := newDailyServerSnap(t, "", nopReward)
	c.Base = srv401.URL

	_, err := c.ClaimPetWorkReward(testAuth(), "2026-10-17")
	var perr *provider.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err 类型 = %T, 需要 *provider.Error（裸 error 会让调度器自愈失效）", err)
	}
	if perr.Kind != provider.ErrSessionDead {
		t.Fatalf("Kind = %v, want ErrSessionDead", perr.Kind)
	}
}

// TestDailyCheckinUnauthorizedOnRewards 领奖阶段 401 同样要类型化。
func TestDailyCheckinUnauthorizedOnRewards(t *testing.T) {
	c, _ := newDailyServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":"100002"}`)
	})
	_, err := c.ClaimPetWorkReward(testAuth(), "2026-10-17")
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Kind != provider.ErrSessionDead {
		t.Fatalf("err = %v, want ErrSessionDead", err)
	}
}

// TestDailyCheckinBusinessAuthError 守门：HTTP 200 + code 100002 也是登录失效（本渠道特性）。
func TestDailyCheckinBusinessAuthError(t *testing.T) {
	c, _ := newDailyServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"code":"100002","desc":"未登录或登录已失效"}`)
	})
	rep, err := c.ClaimPetWorkReward(testAuth(), "2026-10-17")
	var perr *provider.Error
	if !errors.As(err, &perr) || perr.Kind != provider.ErrSessionDead {
		t.Fatalf("err = %v, want ErrSessionDead", err)
	}
	if rep.Status == provider.CheckinClaimed {
		t.Fatal("登录失效不得报 claimed")
	}
}

// TestDailyCheckinBadShape 响应形状异常：不猜、不假成功。
func TestDailyCheckinBadShape(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"非 JSON", `<html>oops</html>`},
		{"data 不是对象", `{"code":"000000","data":"nope"}`},
		{"granted 类型错", `{"code":"000000","data":{"granted":"yes","duplicate":false}}`},
		{"data 缺失", `{"code":"000000"}`},
		{"业务失败码", `{"code":"200028","desc":"活动配置已更新"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newDailyServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, tc.body)
			})
			rep, err := c.ClaimPetWorkReward(testAuth(), "2026-10-17")
			if rep.Status == provider.CheckinClaimed {
				t.Fatalf("形状异常不得报 claimed（body=%s）", tc.body)
			}
			if err == nil {
				t.Fatalf("形状异常必须返回 error（body=%s）", tc.body)
			}
		})
	}
}

// TestDailyCheckinNoToken 零 token 不得发请求（零 token 落盘 + 不浪费上游配额）。
func TestDailyCheckinNoToken(t *testing.T) {
	c, hits := newDailyServer(t, nopReward)
	for _, a := range []*auth.Auth{nil, {Kind: "loomy"}, {Kind: "loomy", AccessToken: "   "}} {
		rep, err := c.ClaimPetWorkReward(a, "2026-10-17")
		if rep.Status != provider.CheckinNoToken {
			t.Fatalf("Status = %v, want no_token", rep.Status)
		}
		if err != nil {
			t.Fatalf("no_token 不应同时返回 error: %v", err)
		}
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("零 token 却发了 %d 次请求", got)
	}
}

// TestDailyCheckinMissingBusinessDate 没有业务日就不发请求
// （businessDate 是服务端必填且会校验，白发一次只增风控成本）。
func TestDailyCheckinMissingBusinessDate(t *testing.T) {
	c, hits := newDailyServer(t, nopReward)
	rep, err := c.ClaimPetWorkReward(testAuth(), "")
	if rep.Status != provider.CheckinNoCampaign {
		t.Fatalf("Status = %v, want no_campaign（本机还不知道业务日）", rep.Status)
	}
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("缺业务日却发了 %d 次请求", got)
	}
}

// TestDailyCheckinBusinessDateRolled 业务日跨天：不发领奖请求，报可重试。
func TestDailyCheckinBusinessDateRolled(t *testing.T) {
	c, hits := newDailyServer(t, nopReward)
	rep, err := c.ClaimPetWorkReward(testAuth(), "2026-10-16")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if rep.Status != provider.CheckinNoCampaign {
		t.Fatalf("Status = %v, want no_campaign", rep.Status)
	}
	if !strings.Contains(rep.Msg, "业务日") {
		t.Fatalf("Msg 应说明业务日切换: %q", rep.Msg)
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("业务日不符却发了 %d 次领奖请求", got)
	}
}

// TestPetWorkSnapshot 快照解析：业务日/配置版本/档位推进都取上游权威值。
func TestPetWorkSnapshot(t *testing.T) {
	c, _ := newDailyServerSnap(t, snapJSON, nopReward)
	d, ok, err := c.PetWorkSnapshot(testAuth())
	if err != nil || !ok {
		t.Fatalf("ok = %v err = %v", ok, err)
	}
	if d.BusinessDate != "2026-10-17" || d.ConfigVersion != "pet-work-v1" {
		t.Fatalf("快照解析错误: %+v", d)
	}
	if d.NextTierID != "tier_1" {
		t.Fatalf("NextTierID = %q, want tier_1", d.NextTierID)
	}

	// 已发 tier_1 → 下一档是 tier_2。
	snap2 := strings.Replace(snapJSON,
		`"status":{"earnedPointsToday":0,"rewardedTierIds":[],"dailyRewardCapped":false}`,
		`"status":{"earnedPointsToday":200,"rewardedTierIds":["tier_1"],"dailyRewardCapped":false}`, 1)
	c2, _ := newDailyServerSnap(t, snap2, nopReward)
	d2, _, err := c2.PetWorkSnapshot(testAuth())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if d2.NextTierID != "tier_2" {
		t.Fatalf("NextTierID = %q, want tier_2", d2.NextTierID)
	}
}

// TestPetWorkSnapshotNoToken 零 token 的快照不得发请求。
func TestPetWorkSnapshotNoToken(t *testing.T) {
	c, _ := newDailyServerSnap(t, snapJSON, nopReward)
	d, ok, err := c.PetWorkSnapshot(nil)
	if ok || err != nil || d.BusinessDate != "" {
		t.Fatalf("nil auth: ok=%v err=%v d=%+v", ok, err, d)
	}
}

// TestDailyCheckinNilAuthNoPanic 守门：nil Auth 不得 panic
// （账号刚被移除时调度器仍可能调用；旧 session() 在这里空指针）。
func TestDailyCheckinNilAuthNoPanic(t *testing.T) {
	c, _ := newDailyServer(t, nopReward)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil Auth 触发 panic: %v", r)
		}
	}()
	rep, _ := c.ClaimPetWorkReward(nil, "2026-10-17")
	if rep.Status != provider.CheckinNoToken {
		t.Fatalf("Status = %v, want no_token", rep.Status)
	}
}

// TestDailyCheckinViaDailyCheckinReportsClosure 守门：无参 DailyCheckin 不得假成功。
//
// 它没有业务日，故**永远**返回错误 —— 否则调度器会记一次"已签到"却一分没领到。
func TestDailyCheckinViaDailyCheckinReportsClosure(t *testing.T) {
	c, hits := newDailyServer(t, nopReward)
	if err := c.DailyCheckin(testAuth()); err == nil {
		t.Fatal("DailyCheckin 无业务日时必须报错，不得假成功")
	}
	if got := atomic.LoadInt32(hits); got != 0 {
		t.Fatalf("DailyCheckin 不应发领奖请求，实际 %d 次", got)
	}
}

// TestImplementsProviderCheckinReporter 守门：Client **必须**实现 provider.CheckinReporter。
//
// # 为什么这条断言反过来了（2026-10-17 Lead 决策）
//
// 原设计是用独立的 DatedCheckinReporter 表达「领奖需要业务日」这个维度，并要求调度器
// 先取快照再调用。但那段调度器接线从未落地 ⇒ Loomy 的签到**没有任何入口**：面板按钮
// 被 noExplicitCheckin 挡着、定时任务全关，功能等于没做（"写了却不生效"）。
//
// 现在改为让 DailyCheckinReport(a) 自己在内部取业务日，从而复用既有的三条签到链路
//（手动 / 全部签到 / 定时）。因此本断言必须**正向**锁定它实现了标准接口 ——
// 否则一旦有人"清理"掉这个方法，签到会再次静默失效。
func TestImplementsProviderCheckinReporter(t *testing.T) {
	var up provider.Upstream = New()
	if _, ok := up.(provider.CheckinReporter); !ok {
		t.Fatal("loomy.Client 必须实现 provider.CheckinReporter —— 否则调度器/面板无法驱动签到")
	}
}

// TestDailyCheckinReportFetchesBusinessDate 守门：标准接口的 1 参入口必须**自己**取业务日。
//
// 它先用快照拿 businessDate，再据此打领奖接口；快照拿不到业务日时不得打领奖接口。
func TestDailyCheckinReportFetchesBusinessDate(t *testing.T) {
	// 快照可用 + 领奖成功：应发出 1 次领奖请求，且状态是 claimed。
	c, hits := newDailyServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"000000","data":{"granted":true,"grantedPoints":300}}`))
	})
	rep, err := c.DailyCheckinReport(testAuth())
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinClaimed {
		t.Fatalf("Status = %v, want claimed（说明它没能自己走完「取业务日→领奖」）", rep.Status)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("领奖请求次数 = %d, want 1", got)
	}

	// 快照不可用（活动关闭）：不得打领奖接口，返回可重试状态。
	c2, hits2 := newDailyServerSnap(t, `{"code":"000000","data":{"enabled":false}}`, nopReward)
	rep2, _ := c2.DailyCheckinReport(testAuth())
	if rep2.Status != provider.CheckinNoCampaign {
		t.Fatalf("Status = %v, want no_campaign", rep2.Status)
	}
	if got := atomic.LoadInt32(hits2); got != 0 {
		t.Fatalf("活动关闭时不应打领奖接口，实际 %d 次", got)
	}
}

// TestBusinessDateOf 业务日只从快照读；活动关闭时为空串（调用方据此跳过）。
func TestBusinessDateOf(t *testing.T) {
	c, _ := newDailyServerSnap(t, snapJSON, nopReward)
	if bd, err := c.BusinessDateOf(testAuth()); err != nil || bd != "2026-10-17" {
		t.Fatalf("BusinessDateOf = %q err=%v, want 2026-10-17", bd, err)
	}

	off := okEnvelope(`{"enabled":false,"businessDate":"2026-10-17","config":null,"status":null}`)
	c2, _ := newDailyServerSnap(t, off, nopReward)
	if bd, err := c2.BusinessDateOf(testAuth()); err != nil || bd != "" {
		t.Fatalf("活动关闭时 BusinessDateOf = %q err=%v, want 空串", bd, err)
	}
}
