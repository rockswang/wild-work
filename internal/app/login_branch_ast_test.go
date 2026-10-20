package app

import (
	"testing"

	"wild-work/internal/provider"
)

// TestStartLoginForPerChannelBranch 断言「每个渠道的登录发起能力恰好一个、且真实可用」。
//
// 背景（2026-10-18 深档排障，H1/H2/H5）：
// 改造前 StartLoginFor 里有一张按 provider.X 分支的 switch，用整文件 strings.Contains
// 断言「文件里出现过 loginraccoon.Start()」，导致
//
//	app.go:737  authURL, err = loginloomy.Start(...)      ← Loomy 的结果
//	app.go:738  authURL, err = loginraccoon.Start()       ← 被下一行覆盖
//
// 这种缺陷**结构上不可能被发现**（那条字面量恰被缺陷本体满足）。
// 当时改成 AST 断言每个 case 分支体对 authURL 的赋值次数。
//
// 2026-10-18 收表后：渠道 → 登录能力收进 loginFlows 单一来源表，「两行挤进同一 case」
// 与「case 只有注释」这两种形态在结构上已不可能出现（一个渠道一条记录，Start 是字段）。
// 故本测试改为**运行时查表**断言，语义更直接：
//   - 每个渠道必须恰好表达一种发起方式（Start / DisplayURL / noFlowReason）；
//   - 有 Start 的，Poll 必须同时存在（否则发起后永远等超时）。
func TestStartLoginForPerChannelBranch(t *testing.T) {
	if len(loginFlows) == 0 {
		t.Fatal("loginFlows 为空 —— 登录能力表未初始化")
	}

	for k, f := range loginFlows {
		// ① 恰好一种发起方式，不允许「两者都有」（含义冲突）或「都没有」（空壳）。
		modes := 0
		if f.Start != nil {
			modes++
		}
		if f.DisplayURL != "" {
			modes++
		}
		if f.noFlowReason != "" {
			modes++
		}
		if modes != 1 {
			t.Errorf("❌ provider.%s 表达了 %d 种发起方式（应为恰好 1 种：Start / DisplayURL / noFlowReason）", k, modes)
		}

		// ② 有 Start 就必须有 Poll：否则发起后拿不到结果，只能等超时。
		//    DisplayURL（只引导不轮询）与 noFlowReason（不发起）不受此约束。
		if f.Start != nil && f.Poll == nil {
			t.Errorf("❌ provider.%s 有 Start 但没有 Poll —— 登录发起后结果永远回不来（只能等超时）", k)
		}

		// ③ 反过来，没有 Start 却登记了 Poll / Shutdown，属冗余配置（多半是复制粘贴残留）。
		if f.Start == nil && f.Poll != nil {
			t.Errorf("❌ provider.%s 没有 Start 却登记了 Poll —— 冗余配置", k)
		}
		if f.Start == nil && f.Shutdown != nil {
			t.Errorf("❌ provider.%s 没有 Start 却登记了 Shutdown —— 冗余配置", k)
		}
	}
}

// TestLoginFlowsCoverKnownChannels 断言表覆盖了所有「需要登录」的渠道，
// 防止将来加渠道时只改了别处、忘了登记（改造前正是这种漏登记造成 7 渠道无 ⟳ 入口）。
func TestLoginFlowsCoverKnownChannels(t *testing.T) {
	// 这些渠道是「用户可添加账号」的，必须有登录发起能力或明确的拒绝说明。
	mustBeKnown := []provider.Kind{
		provider.WorkBuddy, provider.WorkBuddyAI,
		provider.TraeWork, provider.Qoder, provider.QoderCN, provider.QoderCOM,
		provider.QwenWork, provider.Minimax, provider.LobsterAI,
		provider.ZCode, provider.Gemini, provider.CodeArts,
		provider.Raccoon, provider.Loomy, provider.GLM,
		provider.OfficeAce, provider.MonkeyCode,
		provider.Oczen, provider.Kilo,
	}
	for _, k := range mustBeKnown {
		if _, ok := loginFlows[k]; !ok {
			t.Errorf("❌ provider.%s 未在 loginFlows 里登记 —— 前端若有 ⟳ 入口就会变成死按钮", k)
		}
	}
}
