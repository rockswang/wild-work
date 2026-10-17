package app

import (
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
