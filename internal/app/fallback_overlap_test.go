// 备胎重叠合法（工单 01）：同一模型可以既是某规则的主力、又作为**别的**规则
// 的备胎。后端本来只在「备胎 == 本规则主力」（自环）处拦，重叠从未被禁；
// 本文件把这条语义锁成契约，避免将来有人把面板侧那套「全主力集合排除」搬回后端。
package app

import (
	"testing"

	"wild-work/internal/config"
)

// TestFallbackOverlapAccepted 重叠配置必须**保存成功且热生效**（syncer 收到同一份规则）。
// 场景：A 的主力量降级到 B，而 B 同时是另一条规则的主力。
func TestFallbackOverlapAccepted(t *testing.T) {
	a, cfgPath := newAliasFallbackApp(t, nil)
	var pushed []config.ModelFallbackRule
	a.SetModelFallbackSyncer(func(rules []config.ModelFallbackRule) {
		pushed = append([]config.ModelFallbackRule(nil), rules...)
	})
	rules := []config.ModelFallbackRule{
		{From: "workbuddyai/deepseek-v4.1-flash", To: []string{"workbuddy/glm-5.2"}},
		{From: "workbuddy/glm-5.2", To: []string{"oczen/big-pickle"}},
	}
	warns, err := a.SetModelFallback(rules)
	if err != nil {
		t.Fatalf("重叠配置应被接受，实际被拒: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("重叠不该产生软警告，实际 %v", warns)
	}

	saved, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("重新读取配置: %v", err)
	}
	if len(saved.ModelFallback) != 2 {
		t.Fatalf("重叠条目被落盘时剔除，实际 %+v", saved.ModelFallback)
	}
	if got := saved.ModelFallback[0].To; len(got) != 1 || got[0] != "workbuddy/glm-5.2" {
		t.Fatalf("规则 1 的备胎应是另一条规则的主力 workbuddy/glm-5.2，实际 %v", got)
	}

	// 热生效：无需重启，syncer 立即收到同一份（含重叠）规则。
	if len(pushed) != 2 {
		t.Fatalf("保存后必须立即推送热更新，实际收到 %d 条", len(pushed))
	}
	if got := pushed[0].To; len(got) != 1 || got[0] != "workbuddy/glm-5.2" {
		t.Fatalf("热更新内容应保留重叠备胎，实际 %v", got)
	}
}

// TestFallbackSelfLoopRejected 自环（备胎 == 本规则主力）必须仍被硬拒，且不落盘。
func TestFallbackSelfLoopRejected(t *testing.T) {
	a, cfgPath := newAliasFallbackApp(t, nil)
	a.SetModelFallbackSyncer(func([]config.ModelFallbackRule) {
		t.Fatal("被拒的配置不得热更新")
	})
	_, err := a.SetModelFallback([]config.ModelFallbackRule{
		{From: "workbuddyai/deepseek-v4.1-flash", To: []string{"workbuddyai/deepseek-v4.1-flash", "workbuddy/glm-5.2"}},
	})
	if err == nil {
		t.Fatal("自环应被硬拒，实际保存成功")
	}
	saved, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("重新读取配置: %v", err)
	}
	if len(saved.ModelFallback) != 0 {
		t.Fatalf("被拒的配置不得落盘，实际 %+v", saved.ModelFallback)
	}
}
