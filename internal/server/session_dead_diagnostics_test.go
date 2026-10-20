package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// 本文件锁定 MonkeyCode 的 4 处「放大缺陷」中**属于 server 层**的两处：
//
//	③ 粘性路由判据漏查 SessionDead ⇒ 登录态已失效的账号被粘住，每个请求
//	   白打一次必 401 的上游探测（pool.healthy() 早已把它算作不健康，粘性路由
//	   直接调 AuthByUID 绕过了那一层）。
//	④ 503 文案只统计「禁用/冷却」，登录态失效的账号被算进「可用账号」⇒
//	   文案落到「其余余额耗尽或出错」，把唯一可行动的结论（需重新登录）盖掉。

// TestStickyRouteSkipsSessionDead 粘性路由不得选中登录态失效的账号。
func TestStickyRouteSkipsSessionDead(t *testing.T) {
	up := &kindedUpstream{status: http.StatusOK}
	h := newRotateTestHandler(t, up, "sd-")
	rt := h.cfg.Runtimes[provider.GLM]
	p := rt.Pool

	// 先把 B 标记为登录态失效，并让它成为粘性首选（A 无粘性记录时按积分挑 A，
	// 所以这里手动建立指向 B 的粘性记录）。
	p.MarkSessionDead("sd-B", "refresh token is invalid")
	acct := h.pickWithSticky(rt, "m")
	if acct == nil {
		t.Fatal("pickWithSticky 返回 nil（应回落到健康的 A）")
	}
	if acct.UID == "sd-B" {
		t.Errorf("❌ 粘性路由选中了登录态失效的账号 %s —— "+
			"每个请求都会白打一次必 401 的上游探测", acct.UID)
	}

	// 再验证「粘性记录指向 B」时也必须跳过：模拟上一次成功路由到 B、之后 B 失效。
	h.stickyMu.Lock()
	h.sticky[h.stickyKey(provider.GLM, "m")] = &stickyEntry{uid: "sd-B", maxReqs: 50}
	h.stickyMu.Unlock()
	acct = h.pickWithSticky(rt, "m")
	if acct != nil && acct.UID == "sd-B" {
		t.Error("❌ 粘性记录指向已失效账号时仍被复用 —— 应跳过并回落到健康账号")
	}
}

// Test503MessageMentionsSessionDead 全池不可用时，文案必须报出「登录态失效」计数，
// 而不是把它混进「余额耗尽或出错」。
func Test503MessageMentionsSessionDead(t *testing.T) {
	up := &kindedUpstream{status: http.StatusOK}
	h := newRotateTestHandler(t, up, "m2-")
	rt := h.cfg.Runtimes[provider.GLM]
	p := rt.Pool

	// 两个账号全部标记登录态失效 ⇒ 无可用账号。
	p.MarkSessionDead("m2-A", "refresh token is invalid")
	p.MarkSessionDead("m2-B", "refresh token is invalid")

	rec := postModel(h, "glm/m")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("期望 503，实得 %d（body=%s）", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "登录态失效") {
		t.Errorf("❌ 503 文案未提及「登录态失效」（body=%s）—— "+
			"用户会被引向「余额耗尽」，而真正要做的只是重新登录", body)
	}
	if !strings.Contains(body, "需重新登录") {
		t.Errorf("❌ 503 文案未给出可行动提示「需重新登录」（body=%s）", body)
	}
}

// TestSessionDeadAccountNotCountedAsBound 登录态失效账号不得被算进「可用账号」。
func TestSessionDeadAccountNotCountedAsBound(t *testing.T) {
	up := &kindedUpstream{status: http.StatusOK}
	h := newRotateTestHandler(t, up, "bd-")
	rt := h.cfg.Runtimes[provider.GLM]

	rt.Pool.MarkSessionDead("bd-A", "refresh token is invalid")
	rt.Pool.MarkSessionDead("bd-B", "refresh token is invalid")

	// 全池失效 ⇒ 走 bound==0 分支（而不是「其余余额耗尽」）。
	rec := postModel(h, "glm/m")
	body := rec.Body.String()
	// 关键判据：文案里「登录态失效 2」而不是「登录态失效 0」。
	if !strings.Contains(body, "登录态失效 2") {
		t.Errorf("❌ 登录态失效账号未被单独计数（body=%s）—— "+
			"它们会被算成可用账号，从而掩盖真实原因", body)
	}
}

// 确保测试用的 stickyEntry 类型与实现一致（编译期约束）。
var _ = stickyEntry{uid: "", maxReqs: 0}

// 确保 pool 与 auth 在必要时可被引用（避免将来删测试时留下空 import）。
var (
	_ = pool.CoolSoft
	_ = auth.Auth{}
	_ = time.Now
)
