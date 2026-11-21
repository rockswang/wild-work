// handler_image_errors.go 生图失败的**账号惩罚**与**对外响应**映射。
//
// 两者刻意挨着：每个错误类别在惩罚表与响应表里必须一一对应（尤其软限流 = 模型级冷却 + 429、
// 任务超时 = 不换号 + 504）。拆出端点编排（handler_images.go）后，改错误语义只需看这一处。
package server

import (
	"errors"
	"log"
	"net/http"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// penalizeImage 按错误类别施加账号惩罚，返回**是否值得换号继续本请求**。
//
// 惩罚表与改造前逐项一致（每个分支的冷却类型/日志字段不交），只是把「写响应」
// 从里面抽出去——因为现在可能不结束请求而是换号。
func (h *Handler) penalizeImage(rt *Runtime, platform, model string, acct *auth.Auth, err error) bool {
	var pe *provider.Error
	if !errors.As(err, &pe) {
		// 传输层错误：与账号健康无关（同一 host），计错但不换号（R109 同口径）
		rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
		log.Printf("images transport error platform=%s uid=%s err=%v", platform, acct.UID, err)
		return false
	}
	switch pe.Kind {
	case provider.ErrSessionDead:
		// 登录态失效：标记（挑号跳过 + 面板徽标），后面的号可继续
		rt.Pool.MarkSessionDead(acct.UID, pe.Msg)
		log.Printf("images session dead platform=%s uid=%s err=%s", platform, acct.UID, pe.Msg)
	case provider.ErrSoftRate:
		// 限流/风控瞬时拒绝：**模型级**冷却（key = 客户端请求的 model），不冻结整个账号
		rt.Pool.CooldownModel(acct.UID, model, h.cfg.SoftCooldown, "生图限流: "+pe.Msg)
		log.Printf("images rate limited platform=%s uid=%s model=%s err=%s", platform, acct.UID, model, pe.Msg)
	case provider.ErrWafBlock, provider.ErrAccountFault:
		rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, pe.Kind.String())
		log.Printf("images waf/fault platform=%s uid=%s kind=%s err=%s", platform, acct.UID, pe.Kind, pe.Msg)
	case provider.ErrContentBlocked, provider.ErrBadParams, provider.ErrPassthrough:
		// 请求内容问题：不罚号，也不换号（换个号必然复现）
		log.Printf("images request rejected platform=%s uid=%s kind=%s err=%s", platform, acct.UID, pe.Kind, pe.Msg)
	default:
		log.Printf("images upstream error platform=%s uid=%s kind=%s err=%s", platform, acct.UID, pe.Kind, pe.Msg)
	}
	return pe.Kind.Rotatable()
}

// writeImageError 把生图错误写成对外响应（状态码/code 映射是外部契约，不得改码）。
// err 为 nil 时不写任何东西（防御：调用方保证非 nil）。
func (h *Handler) writeImageError(w http.ResponseWriter, platform string, err error) {
	if err == nil {
		err = errors.New("unknown image error")
	}
	// 异步任务超时（modelscope 生图）：上游已受理、可能仍在执行 ⇒ 504 + task id
	var tt *provider.TaskTimeout
	if errors.As(err, &tt) {
		log.Printf("images task timeout platform=%s task=%s err=%s", platform, tt.TaskID, tt.Error())
		writeOpenAIError(w, http.StatusGatewayTimeout, "task_timeout", platform+" 生图任务超时："+tt.Error())
		return
	}
	var pe *provider.Error
	if !errors.As(err, &pe) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", platform+" 生图网络错误："+err.Error())
		return
	}
	switch pe.Kind {
	case provider.ErrSessionDead:
		writeOpenAIError(w, http.StatusUnauthorized, "session_dead", platform+" 账号登录态失效，请重新登录："+pe.Msg)
	case provider.ErrSoftRate:
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded", platform+" 生图被限流，请稍后重试："+pe.Msg)
	case provider.ErrWafBlock, provider.ErrAccountFault:
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", platform+" 上游风控拦截："+pe.Msg)
	case provider.ErrContentBlocked, provider.ErrBadParams, provider.ErrPassthrough:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", pe.Msg)
	default:
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", platform+" 生图失败："+pe.Msg)
	}
}

// stickyPin 把粘性路由改钉到指定账号（换号成功后保持后续请求的会话/缓存连续）。
func (h *Handler) stickyPin(rt *Runtime, uid string) {
	h.stickyMu.Lock()
	h.sticky[h.stickyKey(rt.Kind)] = &stickyEntry{uid: uid, maxReqs: h.cfg.StickyMaxRequests}
	h.stickyMu.Unlock()
	log.Printf("new sticky route platform=%s uid=%s maxReqs=%d reason=image_rotate",
		rt.Kind, uid, h.cfg.StickyMaxRequests)
}
