// Package login_raccoon 实现小浣熊的「网页版自动登录」。
//
// 历史：本包原实现的是**桌面协议回调**登录（把 HKCU 的 office-raccoon 协议临时指向本工具，
// 接住网页授权回传的深链）。该方式仅 Windows 可用、且要求本机装有官方客户端。
// 2026-10-10 改为**网页版流程**（见 web.go）：拉起独立 profile 的浏览器 → 用户登录
// → CDP 捕获 Cookie 里的 raccoon_refresh_token → 换取并落盘凭据。纯 HTTP + 浏览器，跨平台。
//
// 「从本机客户端导入」路径保留（internal/app/import_local.go），作为不想新开浏览器时的替代。
//
// 与调用方（internal/app）的契约：
//
//	Start()      → 拉起浏览器并返回登录页地址，后台开始等待
//	Poll()       → 轮询结果：ErrPending / Result / 终态 error
//	Shutdown()   → 取消并关闭浏览器（幂等）
//
// 会话状态存于包级变量（App 已用 loginBusy 保证同一时刻只有一次登录）。
package login_raccoon

import (
	"errors"
	"log"
	"sync"
)

// ErrPending 表示登录尚未完成（用户在浏览器里还没走完）。
var ErrPending = errors.New("login pending")

// 包级当前会话（App 层 loginBusy 已保证串行）。
var (
	mu      sync.Mutex
	current *WebSession
)

// Start 发起网页版登录：拉起独立 profile 的浏览器并打开登录页。
// 返回值即登录页地址（面板可展示，无需再 window.open——浏览器已由本函数拉起）。
func Start() (string, error) {
	mu.Lock()
	if current != nil {
		if st, _ := current.Status(); st == "pending" {
			mu.Unlock()
			return "", errors.New("已有小浣熊登录流程进行中，请先在浏览器完成登录或取消")
		}
	}
	mu.Unlock()

	sess, err := StartWebLogin(nil)
	if err != nil {
		return "", err
	}
	mu.Lock()
	current = sess
	mu.Unlock()
	return WebLoginURL, nil
}

// Poll 单次检查登录结果：未完成返回 ErrPending；完成返回 Result；失败返回错误。
//
// 无会话（如已被 Shutdown 取消）时返回 ErrPending 而非硬错误 —— 与其它渠道一致：
// 取消后由 pollLogin 的 ctx.Done() 分支给出「已取消」文案，而不是把取消报成登录失败。
func Poll() (Result, error) {
	mu.Lock()
	sess := current
	mu.Unlock()
	if sess == nil {
		return Result{}, ErrPending
	}
	status, serr := sess.Status()
	switch status {
	case "success":
		if r, ok := sess.Result(); ok {
			return r, nil
		}
		return Result{}, errors.New("登录状态异常：成功但无凭据")
	case "failed":
		if serr == nil {
			serr = errors.New("登录失败")
		}
		return Result{}, serr
	case "cancelled":
		return Result{}, errors.New("登录已取消")
	default:
		return Result{}, ErrPending
	}
}

// Shutdown 取消登录并关闭浏览器（幂等：无会话、已结束都不报错）。
func Shutdown() {
	mu.Lock()
	sess := current
	current = nil
	mu.Unlock()
	if sess == nil {
		return
	}
	if st, _ := sess.Status(); st == "pending" {
		sess.Cancel()
		log.Printf("raccoon 网页版登录：已取消并关闭浏览器")
	}
}
