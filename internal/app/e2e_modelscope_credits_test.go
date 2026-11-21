// e2e_modelscope_credits_test.go ModelScope 魔粒额度面 e2e（票 02/03/04 的整机级闭环）。
//
// 与 modelscope_credits_test.go 的分工：
//   - 后者（包内单测）锁「单个函数按契约算对」；
//   - 本文件锁「整条链路在真实进程内装配下真的连起来了」：
//     面板粘贴 token → 真 TCP 端口 → App.RefreshCredits → 主站假上游（**另一个 host**）
//     → pool 快照 → /api/state 面板字段 → /api/account/resource_detail 明细 → 每日领取三态。
//
// 为什么额度面必须单独一台假上游（msSite）：真实上游的额度挂在**主站**
// （modelscope.cn/.ai），对话挂在 api-inference.modelscope.*。合成一台服务器时，
// 「把余额请求错发到对话基址」「漏配 SiteBase 打真上游」这类装配缺陷照样能通过——
// 那正是本层要拦的失败面（credits.go D7）。
package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"wild-work/internal/modelscope"
	"wild-work/internal/provider"
)

// msEnv 起一个「对话假上游 + 主站假上游」的整机环境，并粘一个 CN token 进去。
// 返回的 uid 是面板账号主键（mst- 前缀）。
func msEnv(t *testing.T, token string, siteSetup func(*msSite)) (*e2eEnv, *msSite, string) {
	t.Helper()
	cn := newMSFake(t, tinyPNG(t))
	cn.acceptOnly(token)
	site := newMSSite(t)
	site.acceptOnly(token)
	if siteSetup != nil {
		siteSetup(site)
	}
	env := newE2EEnv(t, e2eOptions{CN: cn, Site: site, ExpiringDays: 7})
	resp, raw := env.post("/api/login/modelscope_token", map[string]any{"token": token})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("粘贴 token status = %d，want 200：%s", resp.StatusCode, truncateForLog(raw))
	}
	uid := asString(jsonMap(t, raw)["uid"])
	if uid == "" {
		t.Fatalf("粘贴响应缺 uid：%s", truncateForLog(raw))
	}
	return env, site, uid
}

// msRefresh 走面板「刷新积分」入口（真 HTTP → App.RefreshCredits → 上游）。
func msRefresh(env *e2eEnv, uid string) (int, []byte) {
	env.t.Helper()
	resp, raw := env.post("/api/account/refresh", map[string]any{"uid": uid})
	return resp.StatusCode, raw
}

// msAccount 取面板上该账号的视图（/api/state 的 accounts[]）。
func msAccount(env *e2eEnv, uid string) map[string]any {
	env.t.Helper()
	for _, acct := range env.accounts(providerGroupModelScope) {
		if asString(acct["uid"]) == uid {
			return acct
		}
	}
	env.t.Fatalf("/api/state 里找不到账号 %s", uid)
	return nil
}

// msDetail 取明细弹层载荷（/api/account/resource_detail）。
func msDetail(env *e2eEnv, uid string) map[string]any {
	env.t.Helper()
	resp, raw := env.post("/api/account/resource_detail", map[string]any{"uid": uid})
	if resp.StatusCode != http.StatusOK {
		env.t.Fatalf("resource_detail = %d：%s", resp.StatusCode, truncateForLog(raw))
	}
	return jsonMap(env.t, raw)
}

// msItems 取明细条目里指定 Name 的那些（按上游返回顺序）。
func msItems(detail map[string]any, name string) []map[string]any {
	list, _ := detail["items"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if ok && asString(m["name"]) == name {
			out = append(out, m)
		}
	}
	return out
}

// TestE2E_MSCredits_BalanceUsageAndPanel 票 02 主链路：
// 「魔粒余额 + 今日用量能被刷新、并且面板按『魔粒』口径展示」。
//
// 断言面刻意包含「请求落到哪台 host、用哪种鉴权」——这是双 host 双鉴权唯一可观测的证据。
func TestE2E_MSCredits_BalanceUsageAndPanel(t *testing.T) {
	const tok = "ms-e2e-credits-cn-0001"
	env, site, uid := msEnv(t, tok, func(s *msSite) {
		// total=500 / available=480 / frozen=20 ⇒ used = 0（负夹）由渠道侧算，见明细断言。
		s.setBalance(tok, 500, 480, 20)
		s.setUsage(tok, 1234)
		// 两笔在效发放：+2d 与 +20d ⇒ 7 日窗口内只有前者该计入「临期」。
		s.addEarnIn(tok, 100, 48*time.Hour)
		s.addEarnIn(tok, 380, 20*24*time.Hour)
	})

	if status, raw := msRefresh(env, uid); status != http.StatusOK {
		t.Fatalf("刷新积分 status = %d，want 200：%s", status, truncateForLog(raw))
	}

	t.Run("面板口径：魔粒 / 不适用已移出 / 未 stale / 冻结单独计不可用", func(t *testing.T) {
		a := msAccount(env, uid)
		if got := asFloat(a["credits"]); got != 480 {
			t.Errorf("credits = %v，want 480（= available_balance，不是 total_balance）", got)
		}
		if got := asString(a["credits_term"]); got != "魔粒" {
			t.Errorf("credits_term = %q，want 「魔粒」（R161 移出 credits_na 后的渠道级文案）", got)
		}
		if na, _ := a["credits_na"].(bool); na {
			t.Error("credits_na = true，want false（魔粒是真实额度，不是「不适用」）")
		}
		if st, _ := a["credits_stale"].(bool); st {
			t.Error("credits_stale = true，want false（刷新成功不该是「待刷新」）")
		}
		if got := asFloat(a["unusable_credits"]); got != 20 {
			t.Errorf("unusable_credits = %v，want 20（冻结魔粒进「不可用」，不进路由余额）", got)
		}
		if got := asFloat(a["expiring_credits"]); got != 100 {
			t.Errorf("expiring_credits = %v，want 100（7 日窗口内只有 +2d 那笔）", got)
		}
	})

	t.Run("明细：拆分条目 + 冻结单列 + 今日用量只读", func(t *testing.T) {
		d := msDetail(env, uid)
		if got := asFloat(d["usable_remain"]); got != 480 {
			t.Errorf("usable_remain = %v，want 480", got)
		}
		if got := asFloat(d["unusable_remain"]); got != 20 {
			t.Errorf("unusable_remain = %v，want 20", got)
		}
		usable := msItems(d, "可用魔粒")
		if len(usable) != 2 {
			t.Fatalf("可用魔粒条目数 = %d，want 2（逐笔到期拆分）：%s", len(usable), truncateForLog(mustJSON(t, d)))
		}
		var sum float64
		for _, it := range usable {
			sum += asFloat(it["remain"])
			if asString(it["expire_at"]) == "" {
				t.Errorf("拆分条目缺 expire_at：%v（逐笔到期是票 03 的全部意义）", it)
			}
			if asString(it["key"]) != "" {
				t.Errorf("拆分条目带了 key=%q，want 空（ledger 按 Name 聚合，设 key 会凭空多出假流水）", asString(it["key"]))
			}
		}
		if sum != 480 {
			t.Errorf("拆分条目 remain 之和 = %v，want 480（必须与可用余额逐位相等）", sum)
		}
		frozen := msItems(d, "冻结魔粒")
		if len(frozen) != 1 {
			t.Fatalf("冻结魔粒条目数 = %d，want 1", len(frozen))
		}
		if u, _ := frozen[0]["usable"].(bool); u {
			t.Error("冻结条目 usable = true，want false（不可消耗额度不参与选号）")
		}
		usage := msItems(d, "今日用量")
		if len(usage) != 1 {
			t.Fatalf("今日用量条目数 = %d，want 1", len(usage))
		}
		if got := asFloat(usage[0]["used"]); got != 1234 {
			t.Errorf("今日用量 used = %v，want 1234（currentUsagePerDay 落到 Used 列）", got)
		}
		if info, _ := usage[0]["info_only"].(bool); !info {
			t.Error("今日用量 info_only = false，want true（token 计的数字不得混进积分算术）")
		}
		if got := asString(usage[0]["unit"]); got != "token" {
			t.Errorf("今日用量 unit = %q，want token", got)
		}
	})

	t.Run("双 host 双鉴权：余额走 Bearer、用量走 Cookie，且都落在主站", func(t *testing.T) {
		bal := site.reqsByPath("/openapi/v1/magicubes/balance")
		if len(bal) == 0 {
			t.Fatal("主站没收到 balance 请求（额度面没走 SiteBase？）")
		}
		for _, r := range bal {
			if r.Token != tok {
				t.Errorf("balance 凭据 = %q，want Authorization: Bearer <token>", r.Token)
			}
		}
		use := site.reqsByPath("/api/v1/inference/rate-limit")
		if len(use) == 0 {
			t.Fatal("主站没收到 rate-limit 请求（今日用量没读）")
		}
		if got := use[0].Header.Get("Cookie"); !strings.Contains(got, "m_session_id=") {
			t.Errorf("rate-limit Cookie = %q，want m_session_id=<token>（该端点只认 Cookie）", got)
		}
		if use[0].Header.Get("Authorization") != "" {
			t.Error("rate-limit 带了 Authorization，want 只带 Cookie（两种鉴权不可混用）")
		}
		// 对话基址（另一台 host）不得收到任何额度面请求。
		for _, p := range []string{"/openapi/v1/magicubes/balance", "/api/v1/inference/rate-limit"} {
			if got := len(env.cn.reqsByPath(p)); got != 0 {
				t.Errorf("对话基址收到了 %d 次 %s，want 0（额度面在主站，不在 api-inference）", got, p)
			}
		}
	})

	t.Run("两次相同刷新不产生假流水", func(t *testing.T) {
		if status, raw := msRefresh(env, uid); status != http.StatusOK {
			t.Fatalf("二次刷新 status = %d：%s", status, truncateForLog(raw))
		}
		resp, raw := env.get("/api/usage?days=1")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/api/usage = %d：%s", resp.StatusCode, truncateForLog(raw))
		}
		credit, _ := jsonMap(t, raw)["credit"].(map[string]any)
		if got := asFloat(credit["spend"]); got != 0 {
			t.Errorf("spend = %v，want 0（余额没变，拆分/聚合口径不同源会误记消耗）", got)
		}
		if got := asFloat(credit["expire"]); got != 0 {
			t.Errorf("expire = %v，want 0（无条目到期）", got)
		}
	})

	if log := env.appLog(); strings.Contains(log, tok) {
		t.Error("app.log 出现了 ms- token 原文（零 token 落盘纪律）")
	}
}

// TestE2E_MSCredits_FailureKeepsSnapshot 票 02 的失败口径（D5）：
// 上游挂了不能把余额写成 0、不能惩罚账号；401 也不能触发「自动刷新重试」
// （ms- token 没有 refresh 概念，refreshIfSessionDead 必然返回 false）。
func TestE2E_MSCredits_FailureKeepsSnapshot(t *testing.T) {
	const tok = "ms-e2e-credits-cn-0002"
	env, site, uid := msEnv(t, tok, func(s *msSite) { s.setBalance(tok, 400, 400, 0) })
	if status, raw := msRefresh(env, uid); status != http.StatusOK {
		t.Fatalf("首次刷新 status = %d：%s", status, truncateForLog(raw))
	}

	t.Run("5xx：保旧值 + 标待刷新 + 零账号惩罚", func(t *testing.T) {
		site.setBalanceStatus(tok, http.StatusInternalServerError)
		if status, _ := msRefresh(env, uid); status == http.StatusOK {
			t.Error("上游 500 时刷新仍回 200，want 非 200（失败必须如实上报）")
		}
		a := msAccount(env, uid)
		if got := asFloat(a["credits"]); got != 400 {
			t.Errorf("credits = %v，want 400（保上次快照，绝不写 0——写 0 会被读成「额度用光」）", got)
		}
		if st, _ := a["credits_stale"].(bool); !st {
			t.Error("credits_stale = false，want true（面板要能说出「这个数字待刷新」）")
		}
		if cooling, _ := a["cooling"].(bool); cooling {
			t.Error("冷却了账号，want 不冷却（额度面失败不是账号错误）")
		}
		if disabled, _ := a["disabled"].(bool); disabled {
			t.Error("禁用了账号，want 不禁用")
		}
		if got := asFloat(a["err_count"]); got != 0 {
			t.Errorf("err_count = %v，want 0（额度面不得计入账号错误）", got)
		}
		if na, _ := a["credits_na"].(bool); na {
			t.Error("credits_na = true，want false（失败 ≠ 不适用）")
		}

		site.setBalanceStatus(tok, 0)
		if status, raw := msRefresh(env, uid); status != http.StatusOK {
			t.Fatalf("恢复后刷新 status = %d：%s", status, truncateForLog(raw))
		}
		if st, _ := msAccount(env, uid)["credits_stale"].(bool); st {
			t.Error("恢复成功后 credits_stale 仍为 true，want 清除")
		}
	})

	t.Run("401：只刷一次、不自动重试、不惩罚", func(t *testing.T) {
		site.setBalanceStatus(tok, http.StatusUnauthorized)
		before := len(site.reqsByPath("/openapi/v1/magicubes/balance"))
		if status, _ := msRefresh(env, uid); status == http.StatusOK {
			t.Error("401 时刷新回 200，want 非 200")
		}
		after := len(site.reqsByPath("/openapi/v1/magicubes/balance"))
		if after-before != 1 {
			t.Errorf("401 触发了 %d 次余额请求，want 1（ms- token 无 refresh 端点 ⇒ 不得自动重试）", after-before)
		}
		a := msAccount(env, uid)
		if cooling, _ := a["cooling"].(bool); cooling {
			t.Error("401 让账号进入冷却，want 不动（R76：失效走可自愈标记，不是惩罚）")
		}
		if disabled, _ := a["disabled"].(bool); disabled {
			t.Error("401 禁用了账号，want 不禁用")
		}
	})

	t.Run("流水失败只降级：余额照刷、只是没有逐笔到期", func(t *testing.T) {
		site.setBalanceStatus(tok, 0)
		site.setTxStatus(tok, http.StatusInternalServerError)
		if status, raw := msRefresh(env, uid); status != http.StatusOK {
			t.Fatalf("流水 500 时刷新 status = %d，want 200（锦上添花的维度不得搞挂余额刷新）：%s", status, truncateForLog(raw))
		}
		d := msDetail(env, uid)
		if got := asFloat(d["usable_remain"]); got != 400 {
			t.Errorf("usable_remain = %v，want 400（降级后仍应是可用余额）", got)
		}
		if got := len(msItems(d, "可用魔粒")); got != 1 {
			t.Errorf("可用魔粒条目数 = %d，want 1（读不到流水 ⇒ 退回单条聚合口径）", got)
		}
	})
}

// TestE2E_MSCredits_PerGrantExpiryFIFO 票 03：逐笔到期必须按「从最新往前分配」落到
// 余额上（老几笔已被烧掉），输出升序，ΣRemain 恒等于可用余额。
func TestE2E_MSCredits_PerGrantExpiryFIFO(t *testing.T) {
	const tok = "ms-e2e-credits-cn-0003"
	// available=320，三笔在效发放：100(+2d) / 50(+3d) / 300(+20d)。
	// 从最新往前分配：300 归 +20d，再从 50 里取 20 归 +3d ⇒ 剩下 30 of 50 已被烧掉。
	env, _, uid := msEnv(t, tok, func(s *msSite) {
		s.setBalance(tok, 450, 320, 0)
		s.addEarnIn(tok, 100, 48*time.Hour)
		s.addEarnIn(tok, 50, 72*time.Hour)
		s.addEarnIn(tok, 300, 20*24*time.Hour)
	})
	if status, raw := msRefresh(env, uid); status != http.StatusOK {
		t.Fatalf("刷新 status = %d：%s", status, truncateForLog(raw))
	}

	items := msItems(msDetail(env, uid), "可用魔粒")
	if len(items) != 2 {
		t.Fatalf("拆分条目数 = %d，want 2：%s", len(items), truncateForLog(mustJSON(t, items)))
	}
	// 升序输出：先 +3d 的 20，再 +20d 的 300（方向反了就会把「今晚到期」虚报成 320）。
	first, second := items[0], items[1]
	if got := asFloat(first["remain"]); got != 20 {
		t.Errorf("条目[0].remain = %v，want 20（= 50 那笔剩余部分）", got)
	}
	if got := asFloat(second["remain"]); got != 300 {
		t.Errorf("条目[1].remain = %v，want 300（= 最新那笔整笔）", got)
	}
	if asString(first["expire_at"]) >= asString(second["expire_at"]) {
		t.Errorf("expire_at 未升序：%q >= %q", asString(first["expire_at"]), asString(second["expire_at"]))
	}
	if got := asFloat(first["remain"]) + asFloat(second["remain"]); got != 320 {
		t.Errorf("ΣRemain = %v，want 320（= available_balance）", got)
	}
	// 临期只算窗口内的那笔：7 日窗口 ⇒ 20（+3d）在窗、300（+20d）不在。
	if got := asFloat(msAccount(env, uid)["expiring_credits"]); got != 20 {
		t.Errorf("expiring_credits = %v，want 20（按逐笔到期算，不是把整池算成临期）", got)
	}
}

// TestE2E_MSCredits_DailyClaimThreeStates 票 04：每日领取三态 + R150（无活动不是失败）。
func TestE2E_MSCredits_DailyClaimThreeStates(t *testing.T) {
	t.Run("already_claimed：当日已有 EARN ⇒ 一次都不打 login/info", func(t *testing.T) {
		const tok = "ms-e2e-claim-already"
		env, site, uid := msEnv(t, tok, func(s *msSite) {
			s.setBalance(tok, 300, 300, 0)
			s.addEarnIn(tok, 200, 24*time.Hour) // gmt_created = 今天
		})
		r := msCheckin(t, env, uid)
		if ok, _ := r["ok"].(bool); !ok {
			t.Errorf("ok = false，want true（今日已领也是成功）：%v", r)
		}
		if got := asString(r["msg"]); !strings.Contains(got, "今日已领取") || !strings.Contains(got, "200") {
			t.Errorf("msg = %q，want 含「今日已领取 200 魔粒」", got)
		}
		if got := site.loginInfoCount(tok); got != 0 {
			t.Errorf("login/info 被调 %d 次，want 0（已领就不该再领）", got)
		}
	})

	t.Run("claimed：先领再复读流水拿金额，且签到后刷余额", func(t *testing.T) {
		const tok = "ms-e2e-claim-fresh"
		env, site, uid := msEnv(t, tok, func(s *msSite) {
			s.setBalance(tok, 700, 680, 0) // 领取后的余额（上游在 login/info 时发奖）
			s.setGrant(tok, 200)
		})
		r := msCheckin(t, env, uid)
		if ok, _ := r["ok"].(bool); !ok {
			t.Errorf("ok = false，want true：%v", r)
		}
		if got := asString(r["msg"]); !strings.Contains(got, "200") {
			t.Errorf("msg = %q，want 含领到的金额 200", got)
		}
		if got := site.loginInfoCount(tok); got != 1 {
			t.Errorf("login/info 调用次数 = %d，want 1", got)
		}
		// 金额只进文案：Amount 恒 0（否则会与 ledger 差分重复记 earn）。
		if got := asFloat(r["remain"]); got != 680 {
			t.Errorf("remain = %v，want 680（签到后必须再查一次余额）", got)
		}
		if got := asFloat(msAccount(env, uid)["credits"]); got != 680 {
			t.Errorf("面板 credits = %v，want 680（签到路径写回同一份 pool 快照）", got)
		}
	})

	t.Run("no_campaign：不报红、可重试、不写假金额", func(t *testing.T) {
		const tok = "ms-e2e-claim-none"
		env, site, uid := msEnv(t, tok, func(s *msSite) { s.setBalance(tok, 120, 120, 0) })
		r := msCheckin(t, env, uid)
		if ok, _ := r["ok"].(bool); ok {
			t.Errorf("ok = true，want false（今天没有可领的活动）：%v", r)
		}
		if retry, _ := r["retryable"].(bool); !retry {
			t.Error("retryable = false，want true（活动可能整点后才创建，窗口内要继续重试）")
		}
		msg := asString(r["msg"])
		if !strings.Contains(msg, "上游还没有发放") {
			t.Errorf("msg = %q，want 「上游还没有发放（今日暂无魔粒活动）」（R150：不得说成签到失败）", msg)
		}
		if strings.Contains(msg, "领到") {
			t.Errorf("msg = %q，want 不含「领到」（没领到就不许编造金额）", msg)
		}
		if got := site.loginInfoCount(tok); got != 1 {
			t.Errorf("login/info 调用次数 = %d，want 1（领取动作本身要打）", got)
		}
		a := msAccount(env, uid)
		if got := asFloat(a["credits"]); got != 120 {
			t.Errorf("credits = %v，want 120（无活动不改余额）", got)
		}
	})
}

// msCheckin 走面板「立即签到」入口，返回 CheckinResult JSON。
func msCheckin(t *testing.T, env *e2eEnv, uid string) map[string]any {
	t.Helper()
	resp, raw := env.post("/api/account/checkin", map[string]any{"uid": uid})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("签到 status = %d，want 200：%s", resp.StatusCode, truncateForLog(raw))
	}
	return jsonMap(t, raw)
}

// TestE2E_MSCredits_TwoZonesIndependent 两区独立：token 不通用（粘贴校验按区走）、
// 池互不兜底（AI 号不会被 CN 的余额救活）、主站请求按 kind 各带各的 token。
func TestE2E_MSCredits_TwoZonesIndependent(t *testing.T) {
	const cnTok, aiTok = "ms-e2e-zone-cn-1", "ms-e2e-zone-ai-1"
	cn := newMSFake(t, tinyPNG(t))
	ai := newMSFake(t, tinyPNG(t))
	cn.acceptOnly(cnTok)
	ai.acceptOnly(aiTok)
	site := newMSSite(t)
	site.acceptOnly(cnTok) // AI 站的 token 在主站被拒（模拟「区域不通用」）
	site.setBalance(cnTok, 500, 480, 20)
	env := newE2EEnv(t, e2eOptions{
		CN: cn, AI: ai, Site: site, ExpiringDays: 7,
		CNTokens: []string{cnTok}, AITokens: []string{aiTok},
	})

	// ① 两区各自一枚号：装配后都在池里（kind 不同、uid 前缀不同）。
	if got := len(env.accounts(providerGroupModelScope)); got != 1 {
		t.Fatalf("CN 渠道账号数 = %d，want 1", got)
	}
	if got := len(env.accounts(providerGroupModelScopeAI)); got != 1 {
		t.Fatalf("AI 渠道账号数 = %d，want 1", got)
	}

	// ② CN token 粘到 AI 渠道：上游（AI 区）401 ⇒ 面板入口如实拒收，且不得动既有账号。
	resp, raw := env.post("/api/login/modelscopeai_token", map[string]any{"token": cnTok})
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("用 CN token 粘 AI 渠道成功了，want 拒收：%s", truncateForLog(raw))
	}
	if got := len(env.accounts(providerGroupModelScopeAI)); got != 1 {
		t.Errorf("拒收后 AI 渠道账号数 = %d，want 1（不得新增/覆盖）", got)
	}

	// ③ CN 号刷新照常拿到自己的魔粒；AI 号因主站拒绝而失败，且**不得**借用 CN 的余额。
	cnUID := modelscope.UIDFor(provider.ModelScope, cnTok)
	aiUID := modelscope.UIDFor(provider.ModelScopeAI, aiTok)
	if status, raw := msRefresh(env, cnUID); status != http.StatusOK {
		t.Fatalf("CN 刷新 status = %d，want 200：%s", status, truncateForLog(raw))
	}
	if got := asFloat(msAccount(env, cnUID)["credits"]); got != 480 {
		t.Errorf("CN credits = %v，want 480", got)
	}
	if status, _ := msRefresh(env, aiUID); status == http.StatusOK {
		t.Error("AI 号（主站不认它的 token）刷新回 200，want 非 200（失败必须如实上报）")
	}
	for _, acct := range env.accounts(providerGroupModelScopeAI) {
		if got := asFloat(acct["credits"]); got != 0 {
			t.Errorf("AI credits = %v，want 0（两区互不兜底，不得回落到 CN 的数字）", got)
		}
	}

	// ④ 主站侧取证：余额请求里出现了**两枚不同的 token**，各自来自自己的区
	//    （借用 CN token 也能让 AI 面板出数字，那才是最难发现的串区）。
	seen := map[string]bool{}
	for _, r := range site.reqsByPath("/openapi/v1/magicubes/balance") {
		seen[r.Token] = true
	}
	if !seen[cnTok] {
		t.Errorf("主站没收到 CN token 的余额请求（seen=%v）", seen)
	}
	if !seen[aiTok] {
		t.Errorf("主站没收到 AI token 的余额请求（seen=%v）⇒ 两区可能共用了同一枚 token", seen)
	}
	if seen[aiTok] && len(seen) > 2 {
		t.Errorf("主站收到了额外的 token：%v", seen)
	}
}

// mustJSON 序列化任意值（断言失败时打印可读证据）。
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return b
}
