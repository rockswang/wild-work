// handler_lastok_fallback_test.go 校验 lastOK 兜底：无静态表的渠道
// （qodercn/qodercom）在动态模型缓存 TTL 过期时，费率表应回退到
// 最近一次成功拉取的模型列表，而不是整渠道消失。
package server

import (
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// TestCachedChannelModels_lastOKFallback 模拟真实修复场景：
// 1) 动态缓存 TTL 有效 → 返回 rt.models；
// 2) 缓存过期、无静态表、有 lastOK → 回退 lastOK（渠道不消失）；
// 3) 缓存过期、无静态表、无 lastOK → 才落空（原始行为）。
func TestCachedChannelModels_lastOKFallback(t *testing.T) {
	cfg := Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.QoderCN: newTestRuntime(t, provider.QoderCN),
		},
	}
	h := &Handler{cfg: cfg}
	rt := cfg.Runtimes[provider.QoderCN]
	if rt.StaticModels != nil {
		rt.StaticModels = nil // 模拟 qodercn：无静态兜底表
	}

	ok := []provider.ModelInfo{{ID: "qwen3.8-flash"}, {ID: "glm-5.3-flash"}}
	now := time.Now()

	// 1) 缓存有效 → 返回动态缓存
	rt.mu.Lock()
	rt.models = ok
	rt.fetched = now
	rt.lastOK = ok
	rt.mu.Unlock()
	got := h.CachedChannelModels()[provider.QoderCN]
	if len(got) != 2 {
		t.Fatalf("缓存有效时应返回 2 个模型，得到 %d", len(got))
	}

	// 2) 缓存过期（fetched 拨回 2h 前）→ 回退 lastOK，渠道仍在
	rt.mu.Lock()
	rt.fetched = now.Add(-2 * time.Hour)
	rt.mu.Unlock()
	got = h.CachedChannelModels()[provider.QoderCN]
	if len(got) != 2 {
		t.Fatalf("缓存过期但有 lastOK 时应回退显示 2 个模型，得到 %d", len(got))
	}

	// 3) 缓存为空且无 lastOK、无静态表 → 落空（保持原语义）。
	//    注意：合并官方 issue #40 后，只要 rt.models 非空（哪怕已过 TTL）就会展示
	//    （过期数据比空数据好）。故此处必须把 models 一并清空，才能模拟
	//    「invalidated + 上游重拉前、又无 lastOK」的真实空窗。
	rt.mu.Lock()
	rt.lastOK = nil
	rt.models = nil
	rt.mu.Unlock()
	if got := h.CachedChannelModels()[provider.QoderCN]; len(got) != 0 {
		t.Fatalf("无 lastOK 无静态表时应为空，得到 %d", len(got))
	}
}

func newTestRuntime(t *testing.T, k provider.Kind) *Runtime {
	t.Helper()
	p := pool.New("") // 内存池，无落盘
	p.Add(&auth.Auth{UID: "test-uid", Nickname: "t"})
	return &Runtime{Kind: k, Pool: p}
}
