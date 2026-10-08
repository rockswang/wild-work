package app

import (
	"testing"
	"time"

	"wild-work/internal/provider"
)

// TestGetStateExposesModelCooling 验证 /api/state 的账号视图暴露模型级冷却：
// 补丁 0006 的 pool.CooldownModel 只冻「账号+模型」，面板需据此把「模型限流」
// 与「整号冷却」区分展示（补丁 0007 的数据来源）。
func TestGetStateExposesModelCooling(t *testing.T) {
	up := &fakeUpstream{remain: 100}
	a, p := newTestApp(t, provider.WorkBuddyAI, up, "uid-mc")

	// 单模型冷却：账号整体仍健康
	if escalated := p.CooldownModel("uid-mc", "glm-5.3", 2*time.Hour, "429 rate limit"); escalated {
		t.Fatal("首次单模型冷却不应升级整号")
	}

	var view *AccountView
	accounts := a.GetState().Accounts
	for i := range accounts {
		if accounts[i].UID == "uid-mc" {
			view = &accounts[i]
			break
		}
	}
	if view == nil {
		t.Fatal("state 中未找到账号 uid-mc")
	}
	if view.Cooling {
		t.Errorf("单模型冷却不应把整号标记为 cooling: %+v", view)
	}
	if len(view.ModelCooling) != 1 || view.ModelCooling["glm-5.3"] == "" {
		t.Fatalf("应暴露 1 条模型级冷却且含 glm-5.3: %+v", view.ModelCooling)
	}
}
