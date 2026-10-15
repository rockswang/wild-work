package server

import (
	"net/http"
	"testing"
)

// body14003Cooling 上游 14003 瞬时拥塞限流响应体（2026-10-12 实测）。
const body14003Cooling = `{"code":14003,"msg":"too many requests","requestId":"r1",` +
	`"retry_policy":{"retry":true},` +
	`"displayTips":{"en":"Wait about a minute before retry, or switch to a less busy model."}}`

// Test14003SoftRateIsModelScoped 14003（模型瞬时拥塞）必须走模型级冷却。
//
// 历史缺陷（2026-10-12 实测）：`IsModelScopedSoftRate` 只匹配 6004 的措辞
// "switch to the other models"，而 14003 的措辞是 "switch to a less busy model"——
// 同一语义两种说法，于是 14003 落到账号级冷却。生产后果（state v4）：
// 两个账号 18~48s 内相继 14003 → 双双账号级冷却 60s → 无健康账号 → 503。
// 账号级既救不了当前模型，还把该账号其它可用模型一起拖死。
func Test14003SoftRateIsModelScoped(t *testing.T) {
	h, p := newModelCoolingHandler(body14003Cooling)

	if rec := postModel(h, "qoder/flash-1"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("flash status=%d want 429, body=%s", rec.Code, rec.Body.String())
	}

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 应存在")
	}
	if st.Cooling {
		t.Errorf("14003 是模型瞬时拥塞，不应把账号标记为整体冷却：%+v", st)
	}
	if st.Reason != "" {
		t.Errorf("不应写账号级 reason（面板会误报整号被限流）：%q", st.Reason)
	}
	if !modelCooling(st, "flash-1") {
		t.Errorf("u1 的 flash-1 应处于模型级冷却：%+v", st.ModelCooling)
	}
	// 同账号的其他模型必须仍然可用
	if rec := postModel(h, "qoder/luna-1"); rec.Code != http.StatusOK {
		t.Fatalf("同账号其他模型应仍可路由：luna status=%d body=%s", rec.Code, rec.Body.String())
	}
}
