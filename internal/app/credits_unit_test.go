package app

import (
	"errors"
	"testing"

	"wild-work/internal/config"
	"wild-work/internal/provider"
)

// TestCreditsUnitTokenChannels 守门「Token 计数与积分分开」这套口径（2026-10-17 用户要求）：
//
//   - 以 token 计量的渠道（ZCode）必须由 creditsUnit 标注为 "token" —— 前端据此渲染
//     Token 计数面板，并**不并入**积分合计；
//   - 同一批渠道**不得**同时落进 creditsNA（那会把它显示成「不适用」，用户看不到用量）；
//   - 积分渠道必须保持空串（默认积分口径），防止误标把真实积分也踢出合计。
func TestCreditsUnitTokenChannels(t *testing.T) {
	a := &App{cfg: &config.Config{}}

	if got := a.creditsUnit(provider.ZCode.String()); got != "token" {
		t.Errorf("ZCode creditsUnit = %q，期望 \"token\"（它的 billing/balance 是 token 桶）", got)
	}
	if a.creditsNAKind(provider.ZCode.String()) {
		t.Error("ZCode 应走 Token 计数面板；标成 credits_na 会让用量完全看不见")
	}

	// 积分渠道抽样：都必须是空串（积分口径），且不能是 NA。
	for _, k := range []provider.Kind{provider.Minimax, provider.LobsterAI, provider.CodeArts, provider.Gemini, provider.GLM, provider.Raccoon} {
		if got := a.creditsUnit(k.String()); got != "" {
			t.Errorf("%s creditsUnit = %q，期望空串（积分）", k, got)
		}
		if a.creditsNAKind(k.String()) {
			t.Errorf("%s 不应标 credits_na（它有真实积分/额度）", k)
		}
	}

	// 匿名渠道保持「不适用」语义（token 单位不应该把它们也拉进 Token 面板）。
	for _, k := range []provider.Kind{provider.Oczen, provider.Kilo} {
		if !a.creditsNAKind(k.String()) {
			t.Errorf("%s 应保持 credits_na（无额度概念）", k)
		}
	}
}

// TestCreditsNAComate 守门 comate 的积分口径（R136，推翻 R130 的结论）：
// 额度真实端点是 /llmproxy/v1/user/token-usage（每日 limit/remain/used）+ WPS 账号级
// 每日额度，能查 ⇒ 必须走正常积分口径，**不得**再标 credits_na
// （标了面板就显示「不适用」、也没有「刷新积分」入口，用户看不到真实剩余）。
//
// R130 曾据「/credits /userinfo /me 全 404」判成 NA —— 那是把「端点名猜错」当成了
// 「上游没有额度」。这条测试就是那次反转的护栏。
func TestCreditsNAComate(t *testing.T) {
	a := &App{cfg: &config.Config{}}
	if a.creditsNAKind(provider.Comate.String()) {
		t.Error("comate 有真实额度端点（token-usage 的每日额度），不应标 credits_na")
	}
	if got := a.creditsUnit(provider.Comate.String()); got != "" {
		t.Errorf("comate creditsUnit = %q，期望空串（它不是 token 计量渠道）", got)
	}
}

// TestComateNicknameFor：用户手工改过的昵称不能被「重贴 sid / 启动巡检」冲掉。
// 实测事故：一位用户把企业号改名成「business」，重贴一次 sid 就变回「李柠希（西南大学）」。
func TestComateNicknameFor(t *testing.T) {
	cases := []struct {
		name  string
		uid   string
		prev  string
		fetch func() (string, error)
		want  string
	}{
		{
			name: "已有自定义昵称：原样保留，且不再请求上游",
			uid:  "sid-9d1e0d534e2f4da9",
			prev: "business",
			fetch: func() (string, error) {
				t.Fatal("已有自定义昵称时不该再请求上游")
				return "", nil
			},
			want: "business",
		},
		{
			name:  "占位昵称：换成上游身份",
			uid:   "sid-9d1e0d534e2f4da9",
			prev:  "WPS 账号 2f4da9",
			fetch: func() (string, error) { return "李柠希（西南大学）", nil },
			want:  "李柠希（西南大学）",
		},
		{
			name:  "新账号：用上游身份",
			uid:   "sid-9d1e0d534e2f4da9",
			prev:  "",
			fetch: func() (string, error) { return "李柠希（西南大学）", nil },
			want:  "李柠希（西南大学）",
		},
		{
			name:  "上游取不到：回退 uid 尾号",
			uid:   "sid-9d1e0d534e2f4da9",
			prev:  "",
			fetch: func() (string, error) { return "", errors.New("超时") },
			want:  "WPS 账号 2f4da9",
		},
		{
			name:  "上游回空串：同样回退 uid 尾号",
			uid:   "sid-9d1e0d534e2f4da9",
			prev:  "",
			fetch: func() (string, error) { return "  ", nil },
			want:  "WPS 账号 2f4da9",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := comateNicknameFor(c.uid, c.prev, c.fetch); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestCreditsNAModelscope 守门魔搭两渠道的额度口径（D5）：上游没有余额/积分端点，
// 面板必须显示「不适用」而不是 0（标成 0 会被用户读成「额度已用光」）。
//
// creditsUnit 保持空串：本渠道**不加**「次数」单位（D5 明确否掉了「剩余次数」展示）——
// token 与请求数照常进 stats/ledger 的 tokens/请求数口径，与积分体系无关。
func TestCreditsNAModelscope(t *testing.T) {
	a := &App{cfg: &config.Config{}}
	for _, k := range []provider.Kind{provider.ModelScope, provider.ModelScopeAI} {
		if !a.creditsNAKind(k.String()) {
			t.Errorf("%s 应标 credits_na（上游无余额端点，D5）", k)
		}
		if got := a.creditsUnit(k.String()); got != "" {
			t.Errorf("%s creditsUnit = %q，期望空串（不得引入「次数」单位）", k, got)
		}
	}
}
