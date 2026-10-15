package main

import (
	"testing"

	"wild-work/internal/custom"
	"wild-work/internal/provider"
	"wild-work/internal/stats"
)

func TestBuildCustomRuntimes(t *testing.T) {
	stateDir := t.TempDir()
	enabled := true
	disabled := false

	channels := []custom.ChannelConfig{
		{
			Name:    "stepfun",
			BaseURL: "https://api.stepfun.com/step_plan/v1",
			APIKey:  "key-step",
			Models:  []custom.ModelConfig{{ID: "step-5-preview"}},
			Enabled: &enabled,
		},
		{
			Name:    "offchan",
			BaseURL: "http://127.0.0.1:8000/v1",
			Enabled: &disabled,
		},
	}

	sRts, aRts, names := buildCustomRuntimes(channels, stateDir)
	if len(sRts) != 1 || len(aRts) != 1 || len(names) != 1 {
		t.Fatalf("expected 1 enabled custom runtime, got s=%d, a=%d, names=%v", len(sRts), len(aRts), names)
	}

	k := provider.Kind("stepfun")
	sRt := sRts[k]
	if sRt == nil || sRt.Kind != k || !sRt.SingleAccount || !sRt.NoCooldownOnServerError {
		t.Fatalf("server runtime attributes mismatch: %+v", sRt)
	}
	if len(sRt.Pool.List()) != 1 || sRt.Pool.List()[0].UID != "custom-stepfun" {
		t.Fatalf("expected pool with custom-stepfun, got: %+v", sRt.Pool.List())
	}

	aRt := aRts[k]
	if aRt == nil || aRt.Kind != k || !aRt.Custom {
		t.Fatalf("app runtime attributes mismatch: %+v", aRt)
	}

	if !stats.IsKnownChannel("stepfun") {
		t.Errorf("stepfun should be registered in stats channelKind")
	}
}

// TestChannelListSharedAcrossSyncers 锁定 R66 的兼容层渠道清单共享态。
// 原始实现的缺陷：compat syncer 闭包捕获**启动快照**（initialChannels），
// 于是「先加自定义渠道、之后再单改模型映射并保存」会把清单重置回启动态，
// 新渠道从 /v1/responses 与 /v1/messages 的路由白名单里消失（报 unknown channel）。
// 修法：两条 syncer 共读一个可变 channelList，这里的断言即锁死该语义。
func TestChannelListSharedAcrossSyncers(t *testing.T) {
	cl := &channelList{builtin: []string{"workbuddy", "cline"}}
	if got := cl.snapshot(); len(got) != 2 {
		t.Fatalf("初始快照应为 2 项，got %v", got)
	}
	// 面板保存自定义渠道 → 清单含新渠道
	cl.setCustom([]string{"stepfun"})
	got := cl.snapshot()
	want := map[string]bool{"workbuddy": true, "cline": true, "stepfun": true}
	if len(got) != 3 || !want[got[0]] || !want[got[1]] || !want[got[2]] {
		t.Fatalf("setCustom 后快照应含 3 项（内建+自定义），got %v", got)
	}
	// 模拟 compat syncer 的第二次调用（用户只改了模型映射）：
	// 由于读取的是同一份共享态，自定义渠道不得丢失。
	again := cl.snapshot()
	if len(again) != 3 {
		t.Fatalf("再次 snapshot 不应丢失自定义渠道，got %v", again)
	}
	// 变更自定义渠道后，两次 syncer 都看到新值
	cl.setCustom([]string{"stepfun", "acustom"})
	if got := cl.snapshot(); len(got) != 4 {
		t.Fatalf("替换自定义清单后应为 4 项，got %v", got)
	}
	// 快照是拷贝：调用方改动不影响内部状态
	s := cl.snapshot()
	s[0] = "mutated"
	if cl.snapshot()[0] == "mutated" {
		t.Error("snapshot 必须返回拷贝，否则调用方会污染共享态")
	}
}
