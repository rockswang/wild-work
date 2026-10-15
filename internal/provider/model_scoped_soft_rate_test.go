package provider

import "testing"

// modelScopedBody14003 WorkBuddyAI 14003 瞬时拥塞限流的真实响应体（2026-10-12 实测）。
// 与 6004 同语义（= 这个模型正忙、换一个试试），只是措辞不同。
const modelScopedBody14003 = `{"code":14003,"msg":"too many requests","requestId":"r1",` +
	`"retry_policy":{"retry":true},` +
	`"displayTips":{"en":"Wait about a minute before retry, or switch to a less busy model."}}`

// TestIsModelScopedSoftRate14003 14003 的 displayTips 明写 "less busy model"，
// 必须判为模型级软限流。
//
// 背景（2026-10-12 state v4 实测）：14003 此前落到**账号级**冷却，而它会在两个账号
// 18~48s 内相继触发 → 双双账号级冷却 → 无健康账号 → 客户端拿到 503。
// 账号级既救不了当前模型，还把该账号其它可用模型一起拖死。
func TestIsModelScopedSoftRate14003(t *testing.T) {
	if !IsModelScopedSoftRate(modelScopedBody14003) {
		t.Error("14003 应按模型级软限流处理")
	}
	// 大小写不敏感（判据走 lower 比较）
	if !IsModelScopedSoftRate(`{"displayTips":{"en":"... switch to a LESS BUSY MODEL."}}`) {
		t.Error("特征串应大小写不敏感")
	}
	// 6004 原文必须仍判模型级（不能被本次新增破坏）
	const body6004 = `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your ` +
		`usage will reset at 2026-10-12 09:00:02 UTC+8, alternatively, ` +
		`you can switch to the other models to continue using it."}`
	if !IsModelScopedSoftRate(body6004) {
		t.Error("6004 仍应判模型级软限流")
	}
}

// TestIsModelScopedSoftRateNegative 普通 429（无任何模型级声明）仍走账号级：
// R47 的保守默认不得被这次补充破坏。
func TestIsModelScopedSoftRateNegative(t *testing.T) {
	for _, b := range []string{
		`{"code":429,"msg":"too many requests"}`,
		`{"code":6004,"msg":"usage exceeds frequency limit"}`, // 6004 但缺声明句
		`<html>429 Too Many Requests</html>`,
		"",
	} {
		if IsModelScopedSoftRate(b) {
			t.Errorf("无从判断维度的 429 应保守走账号级，但被判成模型级：%q", b)
		}
	}
}
