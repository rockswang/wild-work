// session.go 会话坑位管理与 idle 心跳（R145）。
//
// 背景（全部为实测，见 constants.go 的 EpChatSessionHeartbeat 注释）：
//
//	上游对单账号有「并发会话数上限 3」的硬限制（`TM.00001041`）。坑位按
//	`user-session-id` 头计数，**对话流结束不会自动释放**；唯一释放手段是官方
//	agentkernelServer 的 ChatSessionHeartbeat：
//	    PUT {Base}/snap-manager/v1/chat-session/heartbeat?status=idle
//	    headers: user-session-id: <会话 id>          （AK/SK 签名，body 为空）
//
// 本仓此前既不盖 `user-session-id`，也从不发心跳 ⇒ 每个请求占一个永不释放的坑，
// 第 3~4 次必撞 400（线上实测：每次重启后前 2~3 次正常，之后连续 400）。
//
// 修复形态：每个请求仍生成独立会话 id（与现状一致，语义上也对——一次请求 = 一次会话），
// 但**在流结束（或客户端提前关闭）时对该 id 发一次 idle 心跳**，把坑位还回去。
// 实测对照：不盖心跳 4 次内必 400；盖心跳后连打 8 次全 200。
package codearts

import (
	"context"
	"io"
	"log"
	"sync"
)

// releaseSession 对该会话发一次 idle 心跳（幂等，失败只记日志）。
//
// 为什么失败不返回错误、不重试：心跳是**善后**动作，发生在响应已完整流给客户端之后；
// 失败最多导致坑位晚释放（下次仍可能撞 400，但那属于既有症状，不该让成功的请求变失败）。
// 也刻意不放在请求关键路径上阻塞——调用点在 Close（流已结束）。
func (c *Client) releaseSession(ac *Account, sid string) {
	if ac == nil || sid == "" {
		return
	}
	url := c.base() + EpChatSessionHeartbeat + "?status=idle"
	// 空 body ⇒ 走 GET 式签名（content-type 不参与），与实测形态一致。
	// user-session-id 必须**签名后追加**（R145 实测：进 canonical 会 401 APIG.0301，
	// 与 chat 的 x-ot-* 头族同纪律），故放在 unsigned 侧。
	// 用独立的短超时 context，避免善后动作拖住进程退出。
	ctx, cancel := context.WithTimeout(context.Background(), heartbeatTimeout)
	defer cancel()
	status, raw, err := c.doSignedCtx(ctx, c.httClient(), ac, "PUT", url, "", nil, nil,
		map[string]string{HeaderUserSessionID: sid})
	if err != nil {
		log.Printf("codearts 会话心跳失败 uid=%s http=0 err=%v（坑位可能延迟释放）", ac.ID, err)
		return
	}
	if status >= 400 {
		log.Printf("codearts 会话心跳被拒 uid=%s http=%d body=%.160s", ac.ID, status, string(raw))
	}
}

// sessionReleaser 包住对话响应体：流读完或提前 Close 时发一次 idle 心跳。
//
// 为什么必须包 Close 而不能只在 EOF 处发：客户端（含本仓 handler 的透传层）经常
// 在收到 [DONE] 后立刻 Close 而不读到 EOF；只挂 EOF 的话大量坑位照样被漏放。
// sync.Once 保证 EOF 与 Close 两条路径合计只发一次（重复心跳虽无害但没必要）。
type sessionReleaser struct {
	io.ReadCloser
	once sync.Once
	fire func()
}

// Read 透传读取；EOF 时触发心跳（不阻断读取，fire 在持有 once 后同步调用）。
func (r *sessionReleaser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err == io.EOF {
		r.once.Do(r.fire)
	}
	return n, err
}

// Close 关闭底层流并触发心跳（幂等）。
func (r *sessionReleaser) Close() error {
	r.once.Do(r.fire)
	return r.ReadCloser.Close()
}
