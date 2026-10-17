// sse.go 响应流处理：SSE 结束标记、内嵌 error_code 嗅探、model 回填与两段空闲看门狗。
//
// 协议事实（§2.4）：
//   - 只按 `data:` 分帧，**不读 `event:` 名**；
//   - 结束标记是 `data:[DONE]`（参考实现 `line.slice(5).trim()` ⇒ 前缀后有没有空格都认）；
//   - **错误常以 HTTP 200 + 流内 `error_code` 下发**：报文形态是
//     `{"text":"[DONE]","error_code":"InferHub.4291.200","error_msg":"…"}`；
//   - 首 token 超时 300 s、chunk 间超时 600 s（可用环境变量覆盖）。
//
// 本文件做三件事（都在同一趟逐行读取里完成，不缓存整条流）：
//
//  1. `data:[DONE]` → `data: [DONE]`（通用解析器只认带空格的形态；不归一的话上游 DONE
//     会掉进「注释/其它行」分支被原样透传，客户端可能多看到一个不规范的 DONE 行）；
//  2. 内嵌错误帧 → **标准 OpenAI error 帧**（`{"error":{…}}`）：internal/upstream 的
//     error-passthrough 分支会把带 error 键的帧原样透传（白名单不会剥掉），
//     客户端因此能看到真实的错误码，而不是「一个空帧 + 正常 [DONE]」这种伪成功；
//     同一帧被记下来供 StreamError 分类（4291 → ErrHardCredit 等）。
//  3. 回填 model 为客户端请求名（上游回的是裸模型名，客户端按 `<渠道>/<模型>` 记账）。
package codearts

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// benefitLoc benefit 免费额度的墙钟口径：**UTC+8 自然日**（协议 §2.6(a)）。
var benefitLoc = time.FixedZone("UTC+8", 8*60*60)

// BenefitResetAt 返回 now 之后的下一个 UTC+8 自然日零点。
//
// 为什么要本地推算：服务端报文**不给重置时刻**（`InferHub.4291.200` 的 details 只有
// requestId/timestamps/modelId/traceId），参考实现同样按「UTC+8 次日 00:00」推算并
// 明确写「预计」——本函数与 BenefitResetHint 的措辞保持同一口径，不当成上游声明。
func BenefitResetAt(now time.Time) time.Time {
	t := now.In(benefitLoc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, benefitLoc).AddDate(0, 0, 1)
}

// BenefitResetHint 生成人类可读的额度重置说明（含「预计」二字，见上）。
func BenefitResetHint(now time.Time) string {
	return "免费额度按 UTC+8 自然日重置，预计 " + BenefitResetAt(now).Format("2006-01-02 15:04") + "（UTC+8）恢复"
}

// BenefitDay 返回 UTC+8 自然日标识（YYYY-MM-DD）——额度是按日重置，不是滚动窗口。
func BenefitDay(now time.Time) string { return now.In(benefitLoc).Format("2006-01-02") }

// ---------------------------------------------------------------------------
// 流内嗅探 / 归一
// ---------------------------------------------------------------------------

// streamTap 逐行处理上游 SSE：DONE 归一、内嵌错误帧转标准 error 帧、model 回填。
type streamTap struct {
	src     *bufio.Reader
	model   string
	pending []byte
	// errPayload 最后一帧错误（**转化后**的标准 error 帧载荷）；空串 = 无错误。
	errPayload string
}

// newStreamTap 包装读取流。
func newStreamTap(r io.Reader, model string) *streamTap {
	return &streamTap{src: bufio.NewReaderSize(r, 64*1024), model: strings.TrimSpace(model)}
}

// Read 一次吐出一段（保证 SSE 行边界完整）。
func (t *streamTap) Read(p []byte) (int, error) {
	for len(t.pending) == 0 {
		line, err := t.src.ReadBytes('\n')
		if len(line) > 0 {
			t.pending = append(t.pending, t.transform(line)...)
		}
		if err != nil {
			if len(t.pending) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, t.pending)
	t.pending = t.pending[n:]
	return n, nil
}

// transform 处理一行。
//
// ⚠️ 本函数**统一把 `data:<payload>` 归一成 `data: <payload>`**（补一个空格）：
// 协议给的上游形态是 `data:{...}`（参考实现按 `line.slice(5)` 取值，即冒号后无空格），
// 而 internal/upstream 的通用解析器只认 `data: `（带空格）——不归一的话**所有帧**都会
// 掉进「注释/其它行」分支，表现为「上游空流 upstream_parse」这类假故障（本包回归测试
// TestStreamDoneAndModelRewrite 就是照这个判据写的）。
func (t *streamTap) transform(line []byte) []byte {
	trimmed := strings.TrimRight(string(line), "\r\n")
	if !strings.HasPrefix(trimmed, "data:") {
		return line // 注释行（`: keep-alive`）、event 行、空行一律原样
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	if payload == "" {
		return []byte("data: \n")
	}
	if payload == "[DONE]" {
		return []byte("data: [DONE]\n")
	}
	if !strings.HasPrefix(payload, "{") {
		return []byte("data: " + payload + "\n")
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return []byte("data: " + payload + "\n")
	}
	if frame, ok := errorFrameOf(obj); ok {
		t.errPayload = frame
		return []byte("data: " + frame + "\n")
	}
	if t.model != "" {
		payload = string(rewriteModelLine([]byte(payload), t.model))
	}
	return []byte("data: " + payload + "\n")
}

// errorFrameOf 判定该帧是否是「HTTP 200 内嵌业务错误」，是则返回标准 error 帧载荷。
//
// 判据刻意只认**顶层字段**：`error_code` 非空，或（`error_msg` 非空且该帧**没有
// `choices`**）——后者覆盖「上游只给 error_msg」的形态。
// 绝不对正文做字符串匹配：模型在回答里写「429」「error」是常态，误判会直接把正常
// 回答替换成错误帧（officeace 的 errorSniffer 注释记录过同类误伤）。
func errorFrameOf(obj map[string]any) (string, bool) {
	code := strings.TrimSpace(stringOf(obj["error_code"]))
	msg := strings.TrimSpace(stringOf(obj["error_msg"]))
	if msg == "" {
		msg = strings.TrimSpace(stringOf(obj["message"]))
	}
	_, hasChoices := obj["choices"]
	switch {
	case code != "":
	case msg != "" && !hasChoices:
	default:
		return "", false
	}
	if msg == "" {
		msg = "upstream business error"
	}
	payload, err := json.Marshal(map[string]any{"error": map[string]any{
		"message":      msg,
		"code":         code,
		"type":         "upstream_error",
		"error_code":   code,
		"error_msg":    msg,
		"upstream_raw": rawStringOf(obj),
	}})
	if err != nil {
		return "", false
	}
	return string(payload), true
}

// rawStringOf 保留上游原始帧（最小形态），便于排障时看到 requestId 等字段。
// 只放非内容字段：正文/推理文本不重复携带（体积与隐私）。
func rawStringOf(obj map[string]any) string {
	keep := map[string]any{}
	for _, k := range []string{"error_code", "error_msg", "code", "message", "requestId", "request_id", "traceId", "trace_id", "timestamp"} {
		if v, ok := obj[k]; ok {
			keep[k] = v
		}
	}
	if len(keep) == 0 {
		return ""
	}
	b, err := json.Marshal(keep)
	if err != nil {
		return ""
	}
	return string(b)
}

// stringOf 取字符串（非字符串给空串）。
func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// rewriteModelLine 替换单行里所有 `"model":"…"` 的值（字节级替换，不重新序列化）。
func rewriteModelLine(line []byte, model string) []byte {
	needle := []byte(`"model":"`)
	out := line
	from := 0
	for {
		i := bytes.Index(out[from:], needle)
		if i < 0 {
			return out
		}
		start := from + i + len(needle)
		end := bytes.IndexByte(out[start:], '"')
		if end < 0 {
			return out
		}
		end += start
		repl := make([]byte, 0, len(out)-(end-start)+len(model))
		repl = append(repl, out[:start]...)
		repl = append(repl, model...)
		repl = append(repl, out[end:]...)
		out = repl
		from = start + len(model)
	}
}

// newStreamError 构造流内业务错误（额度类附 UTC+8 重置提示）。
func newStreamError(payload string, now time.Time) *StreamError {
	status := statusOfPayload(payload)
	e := &StreamError{Status: status, Body: payload}
	if Classify(status, payload) == provider.ErrHardCredit {
		e.hint = BenefitResetHint(now)
	}
	return e
}

// ---------------------------------------------------------------------------
// provider.Upstream：Stream / Aggregate
// ---------------------------------------------------------------------------

// Stream 透传上游 SSE（经 streamTap 归一），返回末帧 usage。
//
// 内嵌错误帧优先于传输层错误返回：前者是**上游明确的业务结论**（4291 / 排队），
// 比「流被截断」更具体、更可行动（handler 据此冷却账号或冷却模型）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	tap := newStreamTap(r, model)
	var usage map[string]any
	err := upstream.StreamCapture(w, tap, func(u map[string]any) { usage = u })
	if tap.errPayload != "" {
		return usage, newStreamError(tap.errPayload, time.Now())
	}
	return usage, err
}

// Aggregate 把上游 SSE 收敛成单个 OpenAI 响应（非流式客户端走这里）。
//
// 与 Stream 同款嗅探：上游同样可能在 HTTP 200 + 流内 error 帧下返回业务错误，
// 而通用聚合器会把这种帧当成「没有正文的成功响应」吞掉（客户端只看到空回答，
// 账号也不会被冷却，R40/R60）。命中即返回带分类的错误（优先于聚合结果）。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	tap := newStreamTap(r, model)
	resp, err := upstream.Aggregate(tap)
	if tap.errPayload != "" {
		return nil, newStreamError(tap.errPayload, time.Now())
	}
	if err != nil {
		return nil, err
	}
	if model != "" {
		resp["model"] = model
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// 两段空闲看门狗
// ---------------------------------------------------------------------------

// watch 给流式响应体包上「首 token / chunk 间」两段空闲看门狗。
func (c *Client) watch(rc io.ReadCloser) io.ReadCloser {
	return &phaseReader{rc: rc, first: c.firstTokenTimeout(), chunk: c.chunkTimeout()}
}

// firstTokenTimeout 首 token 超时（默认 300 s，环境变量可覆盖）。
func (c *Client) firstTokenTimeout() time.Duration {
	if c.FirstTokenTimeout > 0 {
		return c.FirstTokenTimeout
	}
	return envDurationMS("DSH_CODEARTS_SSE_FIRST_TOKEN_TIMEOUT_MS", DefaultFirstTokenTimeout)
}

// chunkTimeout chunk 间超时（默认 600 s，环境变量可覆盖）。
func (c *Client) chunkTimeout() time.Duration {
	if c.ChunkTimeout > 0 {
		return c.ChunkTimeout
	}
	return envDurationMS("DSH_CODEARTS_SSE_CHUNK_TIMEOUT_MS", DefaultChunkTimeout)
}

// phaseReader 两段式空闲看门狗。
//
// 为什么不是一个固定阈值：首 token 需要等上游排队 + 预填充（协议给 300 s），
// 而进入流式后每两个 chunk 之间只该有生成间隔（协议给 600 s 这个更宽的值）。
// 用单一阈值时，排队场景要么被误杀（取 chunk 值也救不了：300 s 与 600 s 差一倍），
// 要么把真正卡死的连接拖到 600 s——两段分开是协议给的形态。
//
// 超时返回 provider.ErrIdleTimeout（而不是自造错误）：上层据它区分
// 「我们主动掐的」与「上游自己断的」（R58）。
type phaseReader struct {
	rc      io.ReadCloser
	first   time.Duration
	chunk   time.Duration
	gotData bool
	closed  bool
}

// Read 实现 io.Reader（超时路径会等读取 goroutine 真正退出再返回，避免数据竞争）。
func (r *phaseReader) Read(p []byte) (int, error) {
	if r.closed {
		return 0, provider.ErrIdleTimeout
	}
	d := r.chunk
	if !r.gotData {
		d = r.first
	}
	if d <= 0 {
		n, err := r.rc.Read(p)
		if n > 0 {
			r.gotData = true
		}
		return n, err
	}
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := r.rc.Read(p)
		ch <- result{n, err}
	}()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case res := <-ch:
		if res.n > 0 {
			r.gotData = true
		}
		return res.n, res.err
	case <-t.C:
		r.closed = true
		_ = r.rc.Close()
		<-ch
		return 0, provider.ErrIdleTimeout
	}
}

// Close 关闭底层响应体（幂等）。
func (r *phaseReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.rc.Close()
}
