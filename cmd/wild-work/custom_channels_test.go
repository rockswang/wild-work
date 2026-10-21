package main

import (
	"time"

	"testing"

	"wild-work/internal/custom"
	"wild-work/internal/provider"
	"wild-work/internal/stats"
)

func TestBuildCustomRuntimes(t *testing.T) {
	stateDir := t.TempDir()

	// R83 起 ChannelConfig 没有 enabled 字段：列表里的渠道一律装配。
	channels := []custom.ChannelConfig{
		{
			Name:    "stepfun",
			BaseURL: "https://api.stepfun.com/step_plan/v1",
			APIKey:  "key-step",
			Models:  []custom.ModelConfig{{ID: "step-5-preview"}},
		},
	}

	sRts, aRts, names := buildCustomRuntimes(channels, stateDir, 120*time.Second)
	if len(sRts) != 1 || len(aRts) != 1 || len(names) != 1 {
		t.Fatalf("expected 1 custom runtime, got s=%d, a=%d, names=%v", len(sRts), len(aRts), names)
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

// TestBuildCustomRuntimesWiresUpstreamTimeout 锁定 R110 的接线：
//
// `upstream.timeout_seconds` 必须真的传到自定义渠道的 client 上。R66 引入自定义渠道时
// 漏了这条接线（内建渠道在 main 里逐个 `xxxUp.HTTP.Timeout = ...`，自定义渠道直接
// `custom.New(cc)` 用硬编码），导致配置项对它们一直是摆设——这也是 2026-10-20
// sgproxy 压缩上下文必然超时的根因之一。
func TestBuildCustomRuntimesWiresUpstreamTimeout(t *testing.T) {
	channels := []custom.ChannelConfig{{Name: "sgproxy", BaseURL: "http://127.0.0.1:8317/v1"}}

	sRts, _, _ := buildCustomRuntimes(channels, t.TempDir(), 300*time.Second)
	rt := sRts[provider.Kind("sgproxy")]
	if rt == nil {
		t.Fatal("runtime 未装配")
	}
	cl, ok := rt.Upstream.(*custom.Client)
	if !ok {
		t.Fatalf("Upstream 类型不符: %T", rt.Upstream)
	}
	if got := cl.HTTP.Timeout; got != 300*time.Second {
		t.Errorf("HTTP.Timeout = %v, want 300s（必须来自 upstream.timeout_seconds）", got)
	}
	// 响应头上限跟随放宽：大 prompt 首字节慢，不能用 90s 紧值。
	if got := cl.HeaderTimeout(); got != 300*time.Second {
		t.Errorf("HeaderTimeout = %v, want 300s", got)
	}
	// 流式 client 必须无总超时（R35）。
	if got := cl.StreamHTTP.Timeout; got != 0 {
		t.Errorf("StreamHTTP.Timeout 必须为 0（R35），got %v", got)
	}

	// 传 0 时回落到渠道默认值（而不是 0 秒的无限超时）。
	sRts, _, _ = buildCustomRuntimes(channels, t.TempDir(), 0)
	cl = sRts[provider.Kind("sgproxy")].Upstream.(*custom.Client)
	if cl.HTTP.Timeout <= 0 {
		t.Fatalf("timeout 传 0 时必须回落默认值，got %v", cl.HTTP.Timeout)
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
