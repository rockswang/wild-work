// handler_model_filter_test.go 回归：渠道出口过滤必须穿透 server 的 1h 目录缓存。
//
// 线上缺陷（2026-10-26，codearts）：渠道层把「本账号用不了」的模型标记并落盘后，
// /v1/models 仍然列出它们。根因是**双层缓存**：
//   - 渠道层 catalogCache（codearts catalogTTL）
//   - server 层 rt.models（dynamicModelsTTL = 1h）
//
// 渠道只能清自己那层，碰不到 rt.models ⇒ 标记最长 1 小时后才生效。
// 修法：把过滤挂到 Runtime.ModelFilter，在**所有出口**（缓存命中 / 新拉取 / 静态兜底）
// 都过一遍，令任何一层缓存都绕不过它。
package server

import (
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// filterOutRemoves 模拟渠道注入的出口过滤（去掉指定 ID）。
func filterOut(ids ...string) func([]provider.ModelInfo) []provider.ModelInfo {
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	return func(in []provider.ModelInfo) []provider.ModelInfo {
		out := in[:0]
		for _, m := range in {
			if !drop[m.ID] {
				out = append(out, m)
			}
		}
		return out
	}
}

// TestModelFilterAppliesToCachedModels 缓存命中路径也必须过滤。
//
// 修复前：rt.models 命中即原样返回 ⇒ 刚被标记的模型继续列出（最长 1h）。
func TestModelFilterAppliesToCachedModels(t *testing.T) {
	up := &cachedModelsUpstream{infos: []provider.ModelInfo{
		{ID: "deepseek-v4.1-flash"},
		{ID: "deepseek-v4-pro"},
		{ID: "glm-5.3-flash"},
	}}
	h, _ := newCacheTestHandler(t, provider.CodeArts, up)
	h.cfg.Runtimes[provider.CodeArts].ModelFilter = filterOut("deepseek-v4-pro", "glm-5.3-flash")

	// 首次拉取即写入 rt.models（缓存随后命中）。
	got := h.ChannelModels()[provider.CodeArts]
	if len(got) != 1 || got[0].ID != "deepseek-v4.1-flash" {
		t.Fatalf("新拉取路径必须过滤，得到 %+v", got)
	}

	// 缓存仍在 TTL 内（rt.models 已填充）⇒ 再调一次 ChannelModels 就走
	// fetchRuntimeModels 的「rt.models 命中」分支（**不是** CachedChannelModels，
	// 那条路自己也会过滤，用它测会掩盖这里的缺陷）。
	if len(h.cfg.Runtimes[provider.CodeArts].models) != 3 {
		t.Fatalf("前置条件：rt.models 应缓存了 3 个模型")
	}
	again := h.ChannelModels()[provider.CodeArts]
	if len(again) != 1 || again[0].ID != "deepseek-v4.1-flash" {
		t.Fatalf("rt.models 缓存命中路径必须过滤，得到 %+v", again)
	}

	// CachedChannelModels（面板路径）同样不得泄漏。
	cached := h.CachedChannelModels()[provider.CodeArts]
	if len(cached) != 1 || cached[0].ID != "deepseek-v4.1-flash" {
		t.Fatalf("CachedChannelModels 必须过滤，得到 %+v", cached)
	}
}

// TestModelFilterAppliesToStaticFallback 静态兜底路径也必须过滤。
//
// 修复前：modelList 在动态目录为空时直接 infos = rt.StaticModels，
// 而 staticCatalog 里就含被标记的模型 ⇒ 标记等于被撤销。
func TestModelFilterAppliesToStaticFallback(t *testing.T) {
	// 上游返回空 ⇒ fetchRuntimeModels 返回 nil ⇒ 触发静态兜底。
	up := &cachedModelsUpstream{}
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at"})
	h := NewHandler(Config{Runtimes: map[provider.Kind]*Runtime{
		provider.CodeArts: {
			Kind:         provider.CodeArts,
			Pool:         p,
			Upstream:     up,
			StaticModels: []provider.ModelInfo{{ID: "ok-model"}, {ID: "bad-model"}},
			ModelFilter:  filterOut("bad-model"),
		},
	}})

	for _, e := range h.modelList() {
		if e["id"] == "codearts/bad-model" {
			t.Fatal("静态兜底路径必须过滤被标记的模型，但它出现在 /v1/models")
		}
	}
	found := false
	for _, e := range h.modelList() {
		if e["id"] == "codearts/ok-model" {
			found = true
		}
	}
	if !found {
		t.Fatal("过滤不得误伤正常模型")
	}
}

// TestModelFilterNilKeepsBehavior 未注入过滤的渠道行为不变（零回归）。
func TestModelFilterNilKeepsBehavior(t *testing.T) {
	up := &cachedModelsUpstream{infos: []provider.ModelInfo{{ID: "a"}, {ID: "b"}}}
	h, _ := newCacheTestHandler(t, provider.QoderCN, up)
	rt := h.cfg.Runtimes[provider.QoderCN]
	if len(h.ChannelModels()[provider.QoderCN]) != 2 { // 先填充 rt.models
		t.Fatal("前置条件：首次拉取应得 2 个")
	}
	rt.fetched = time.Now()
	if got := h.CachedChannelModels()[provider.QoderCN]; len(got) != 2 {
		t.Fatalf("nil 过滤时必须原样返回 2 个，得到 %d", len(got))
	}
}
