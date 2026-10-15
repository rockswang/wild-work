package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// ---------------------------------------------------------------------------
// 流式收尾：截断时必须补一帧 error，且不得补 [DONE]
// ---------------------------------------------------------------------------
//
// 为什么这是硬要求：SSE 响应头一旦发出（200 + text/event-stream），后续任何故障
// 都无法再用 HTTP 状态码表达，只能用流内帧表达。
//
// 补一帧 error 让客户端拿到明确原因（可据此重试），但**不能**再补 [DONE]（R36 /
// issue #42）：[DONE] 是「正常结束」的标记，与 error 帧同时发出等于把截断伪装成
// 成功收尾——客户端会拿着半截 tool_call arguments 去解析并报
// "tool input was not fully received"，而网关日志里看不到任何错误。

// TruncationErrorCode 把流中断错误归一成客户端可见的错误码。
// 空闲超时单独给码：它是「上游卡死」，与普通读错误（连接被切断等）运维含义不同。
func TruncationErrorCode(err error) string {
	if IsIdleTimeout(err) {
		return "upstream_timeout"
	}
	return "upstream_stream_error"
}

// WriteTruncationFrames 在流中断处补写一帧 error（**不补 [DONE]**），并 flush。
//
// 调用方应在「已开始写流、随后读上游出错」的分支里调用，然后再返回原始错误
// （错误仍需上抛给 handler 记日志；本函数只负责让客户端能收尾）。
// w 不是 Flusher 时静默跳过 flush（httptest.ResponseRecorder 等场景）。
//
// 错误信息做截断：上游/网络错误文本可能很长（含 URL、证书细节），
// 全部塞进 SSE 帧会撑大客户端日志，200 字足够定位。
func WriteTruncationFrames(w io.Writer, err error) {
	msg := "upstream stream interrupted"
	if err != nil {
		msg = truncateRunes(err.Error(), 200)
	}
	frame, merr := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "upstream_error",
		"code":    TruncationErrorCode(err),
	}})
	if merr != nil {
		// 序列化失败理论上不可能（纯字符串 map）；退化成固定帧，仍保证收尾。
		frame = []byte(`{"error":{"message":"upstream stream interrupted","type":"upstream_error","code":"upstream_stream_error"}}`)
	}
	_, _ = w.Write(append(append([]byte("data: "), frame...), []byte("\n\n")...))
	if fl, ok := w.(interface{ Flush() }); ok {
		fl.Flush()
	}
}

// truncateRunes rune 安全截断（不切碎多字节字符）。
func truncateRunes(s string, n int) string {
	rs := []rune(s)
	if len(rs) <= n {
		return s
	}
	return string(rs[:n])
}

// ErrIdleTimeout 上游流长时间无数据（空闲超时）。
//
// 与 http.Client.Timeout 的区别是本模块存在的全部理由：
//
//	http.Client.Timeout  整请求上限 —— 计时器在 Do() 返回后继续跑，直到 body 读完。
//	                     SSE 整个生成期都在读 body，故「慢但一直在出字」的请求
//	                     也会被从流中间掐断（实测 loomy 两次中断都恰好 120.00s）。
//	ErrIdleTimeout      空闲上限 —— 只在「连续 N 秒一个字节都没读到」时才判死。
//	                     慢而持续输出的响应不受影响，真卡住仍能及时发现。
//
// 流式渠道因此必须用无总超时的 client + 本看门狗兜底（见 AGENTS 不变量）。
var ErrIdleTimeout = errors.New("upstream stream idle timeout")

// IsIdleTimeout 报告错误是否为空闲超时（含被 IdleReader 包装后的形态）。
//
// 也识别 internal/idle 看门狗的那一族错误：它没有 ErrIdleTimeout 变量可比较，
// 只能按形态判别——cancel 请求 context 会让在途 Read 返回上下文错误，
// 或（连接被连带关闭时）返回 net.ErrClosed 包装。
//
// ⚠️ 为什么必须认得：idle 看门狗唯一的动作就是 cancel，若这里认不出，
// 「我们主动掐的」与「上游真断的」在日志与出口错误码里就完全同形。
// 实测排查一次 qoder 截断时正是卡在这里（见 R58）。
func IsIdleTimeout(err error) bool {
	if errors.Is(err, ErrIdleTimeout) {
		return true
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, net.ErrClosed)
}

// TruncationCause 把流被砍断的**原始原因**压成一行短文本，供日志与出口 error 帧使用。
//
// 背景：qoder/qodercn/qodercom/qwenwork 四个渠道原先无论什么原因都只报固定串
// "upstream stream truncated"，原始 err 被**丢弃** ⇒ 生产上无法区分三种完全不同的故障：
//   - 空闲看门狗掐的（客户端长时间无字节，可能是上游排队/卡死）
//   - 上游主动断连（connection reset / unexpected EOF）
//   - 读到半个帧就 EOF
//
// 与 internal/upstream/sse.go 保留 err.Error() 的做法对齐（R36 家族）。
func TruncationCause(err error) string {
	switch {
	case err == nil:
		return "unknown cause"
	case IsIdleTimeout(err):
		// 归一成单一措辞：调用方看到的 err 可能是 context.Canceled 或 net.ErrClosed，
		// 直写原文只会让人以为是客户端断开。
		return ErrIdleTimeout.Error()
	default:
		return err.Error()
	}
}

// TruncationError 构造「流被截断」错误：保留原始原因，并让 IsIdleTimeout 能穿透。
//
// 不直接把原 err 返回，是因为调用方还需附加「截断」这一事实；但原因必须留在文本里，
// 且空闲超时不能只留文本——上游 handler 用 errors.Is 判形态，故用 %w 保留链。
//
// 空闲超时统一换成 ErrIdleTimeout 再包装：在途 Read 返回的是 context.Canceled，
// 直写原文会让人误以为「客户端断开」，而实际是看门狗主动掐的（同一事实两种措辞
// 会让日志检索漏掉一半，见 R58）。
func TruncationError(err error) error {
	switch {
	case err == nil:
		return errors.New("upstream stream truncated")
	case IsIdleTimeout(err):
		return fmt.Errorf("upstream stream truncated: %w", ErrIdleTimeout)
	default:
		return fmt.Errorf("upstream stream truncated: %w", err)
	}
}

// TruncationCode 给出出口 error 帧应使用的 code：空闲超时单独给码。
func TruncationCode(err error) string {
	if IsIdleTimeout(err) {
		return "upstream_timeout"
	}
	return "upstream_truncated"
}

// TruncationFrame 构造出口 error 帧的 JSON 载荷（不含 "data: " 前缀与结尾空行）。
// msg 里可能含双引号/反斜杠（原始 err 文本），故必须走 JSON 编码而非字符串拼接。
func TruncationFrame(err error) string {
	payload, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": TruncationCause(err),
		"type":    "upstream_error",
		"code":    TruncationCode(err),
	}})
	return string(payload)
}

// WriteTruncationFrame 写一帧截断 error（**不补 [DONE]**，R36）并 flush。
// 调用方随后应把 TruncationError(err) 上抛给 handler 记日志。
func WriteTruncationFrame(w io.Writer, err error) error {
	frame := "data: " + TruncationFrame(err) + "\n\n"
	if _, werr := io.WriteString(w, frame); werr != nil {
		return werr
	}
	if fl, ok := w.(interface{ Flush() }); ok {
		fl.Flush()
	}
	return nil
}

// IdleReader 给流式响应体加「空闲超时」看门狗。
//
// 每次 Read 起一个 goroutine 去读底层，与定时器 select 竞争：
//   - 数据先到 → 正常返回；
//   - 定时器先到 → Close 底层（解除 net/http body 在途的 Read）→ 返回 ErrIdleTimeout。
//
// 超时路径会**等读取 goroutine 真正退出**再返回，否则调用方拿到的 p 会被那个
// goroutine 并发写（数据竞争）。Close 能中断挂起的 Read，所以这个等待不会永久阻塞。
//
// 非流式（aggregate）路径同样适用：由「总时长硬上限」变为「空闲上限」，
// 长而持续输出的响应不再被误杀，真卡住仍会报错。
type IdleReader struct {
	rc io.ReadCloser
	d  time.Duration

	closed bool // 已判定空闲超时并关掉底层（后续 Read 直接报错，不再重复关）
}

// NewIdleReader 包装 rc；d <= 0 时不加看门狗（直接透传，便于测试与「显式关闭」场景）。
func NewIdleReader(rc io.ReadCloser, d time.Duration) *IdleReader {
	return &IdleReader{rc: rc, d: d}
}

// Read 实现 io.Reader。
func (r *IdleReader) Read(p []byte) (int, error) {
	if r.d <= 0 {
		return r.rc.Read(p)
	}
	if r.closed {
		return 0, ErrIdleTimeout
	}

	type result struct {
		n   int
		err error
	}
	// 缓冲 1：超时分支返回后，读取 goroutine 仍可能写入，不能被阻塞住。
	ch := make(chan result, 1)
	go func() {
		n, err := r.rc.Read(p)
		ch <- result{n, err}
	}()

	t := time.NewTimer(r.d)
	defer t.Stop()
	select {
	case res := <-ch:
		return res.n, res.err
	case <-t.C:
		// 关底层解除挂起的 Read，然后等它退出——p 的所有权必须干净交还。
		r.closed = true
		_ = r.rc.Close()
		<-ch
		return 0, ErrIdleTimeout
	}
}

// Close 关闭底层响应体。幂等（超时路径已关过时只返回 nil）。
func (r *IdleReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.rc.Close()
}
