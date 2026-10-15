// cache_test.go 运行统计（stats 口径）的缓存利用率聚合。
//
// 与 ledger 口径的一致性要求：**分子/分母都是「只含上报样本」的加总**。
// 两侧若漂移，面板两个 tab 会对同一模型显示不同命中率。
package stats

import (
	"testing"
	"time"
)

// TestAddUsageCacheUtilization 上报样本进分子分母，未上报样本整条排除。
func TestAddUsageCacheUtilization(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	// workbuddy 两笔上报：命中 80/100，命中 0/300
	s.AddUsage("workbuddy/m1", 100, 10, 0, 0, 0, 80, true, now)
	s.AddUsage("workbuddy/m1", 300, 20, 0, 0, 0, 0, true, now)
	// glm 一笔未上报（不参与缓存口径）
	s.AddUsage("glm/m2", 100, 30, 0, 0, 0, 0, false, now)

	snap := s.Snapshot(now)
	byModel := map[string]ModelStat{}
	for _, m := range snap.Models {
		byModel[m.Model] = m
	}
	if m := byModel["workbuddy/m1"]; m.CR != 80 || m.CRCov != 400 || m.CRReqs != 2 {
		t.Errorf("m1 cr/cr_cov/cr_reqs = %d/%d/%d want 80/400/2", m.CR, m.CRCov, m.CRReqs)
	}
	// 未上报模型不得出现缓存数字（前端据 cr_cov==0 显示「—」）
	if m := byModel["glm/m2"]; m.CRCov != 0 || m.CR != 0 {
		t.Errorf("未上报模型不应有缓存样本，实际 %d/%d", m.CR, m.CRCov)
	}
	// 原有 token 口径不受影响
	if byModel["glm/m2"].Tokens != 130 {
		t.Errorf("未上报模型的 token 统计被影响：%d", byModel["glm/m2"].Tokens)
	}
}

// TestAddUsageCacheClampsNegative 坏数据钳 0，但**仍计入上报**（保持「已上报」语义）。
func TestAddUsageCacheClampsNegative(t *testing.T) {
	s := newTestStats(t)
	now := time.Now()
	s.AddUsage("workbuddy/m1", 100, 10, 0, 0, 0, -5, true, now)
	m := s.Snapshot(now).Models[0]
	if m.CR != 0 {
		t.Errorf("负命中应钳 0，实际 %d", m.CR)
	}
	if m.CRCov != 100 {
		t.Errorf("仍应计入分母（真值是「上报了且为 0」），实际 %d", m.CRCov)
	}
}

// TestCacheSurvivesDayRollover 跨天归档后，缓存的分子分母必须一起走（不能只搬一个）。
func TestCacheSurvivesDayRollover(t *testing.T) {
	s := newTestStats(t)
	base := time.Now()
	s.AddUsage("workbuddy/m1", 200, 10, 0, 0, 0, 50, true, base)
	// 次日再记：应触发今日口径重置
	s.AddUsage("workbuddy/m1", 100, 10, 0, 0, 0, 10, true, base.Add(24*time.Hour))
	m := s.Snapshot(base.Add(24 * time.Hour)).Models[0]
	if m.CR != 10 || m.CRCov != 100 {
		t.Fatalf("跨天后缓存口径应重置为今日值，实际 %d/%d", m.CR, m.CRCov)
	}
}

// TestCacheSurvivesRestart 重启（flush → 新引擎 load）后，今日缓存样本必须恢复。
// 回归：persist.go 的 modelPersist 曾遗漏 cr/cr_cov/cr_reqs 三字段，2026-10-13
// 线上实测每次重启丢失今日缓存（workbuddyai 98%→—），面板「缓存」列大面积回退。
func TestCacheSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Now()
	// workbuddyai：上报 569 次，命中 180355328 / 分母 183851354（线上量级）
	s.AddUsage("workbuddyai/deepseek-v4.1-flash", 183851354, 783024, 0, 4522, 4085, 180355328, true, now)
	// workbuddy：上报但 0 命中（必须恢复成 0% 而非「—」）
	s.AddUsage("workbuddy/step-5-preview", 549, 5465, 0, 10159, 19, 0, true, now)
	s.FlushNow()

	s2, err := New(dir, nil)
	if err != nil {
		t.Fatalf("重开引擎: %v", err)
	}
	snap := s2.Snapshot(now)
	byModel := map[string]ModelStat{}
	for _, m := range snap.Models {
		byModel[m.Model] = m
	}
	m1 := byModel["workbuddyai/deepseek-v4.1-flash"]
	if m1.CR != 180355328 || m1.CRCov != 183851354 || m1.CRReqs != 1 {
		t.Fatalf("重启后缓存样本丢失: cr/cr_cov/cr_reqs = %d/%d/%d", m1.CR, m1.CRCov, m1.CRReqs)
	}
	m2 := byModel["workbuddy/step-5-preview"]
	if m2.CRCov != 549 || m2.CR != 0 || m2.CRReqs != 1 {
		t.Fatalf("0 命中上报重启后应保持 0%%（非—）: cr/cr_cov/cr_reqs = %d/%d/%d", m2.CR, m2.CRCov, m2.CRReqs)
	}
}
