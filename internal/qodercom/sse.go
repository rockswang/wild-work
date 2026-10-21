// sse.go 处理 QoderCOM 嵌套 SSE：每行 data:{...,"body":"<json-string>"}，
// body 字段需二次解析得到标准 OpenAI chunk。
// 移植自 qoderwork2api internal/upstream/sse.go，含聚合与流式转写。
package qodercom

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/loopguard"
	"wild-work/internal/provider"
)

// parseNestedSSE 逐行解析嵌套 SSE，每个有效 chunk 调 onChunk。
//   - 信封 statusCodeValue >= 400 → 返回 *sseError（上游以 HTTP 200 包裹业务失败，R108）
//   - body == "[DONE]" → 正常结束
//   - "event:finish" 行 → 忽略
//   - body 二次 unmarshal 失败 → 跳过该行（不致命）
//
// truncated 为出参（调用方给非 nil 指针才写）：报告「可证实的截断」——传输层错误
// （连接中断 / 读超时）或 EOF 处停在一个不完整的帧上。调用方据此决定是向客户端补
// [DONE] 还是改发 error 帧：把半截流伪装成正常结束，会让客户端拿着残缺的 tool_call
// arguments 去解析并报 "tool input was not fully received"（issue #42）。
//
// 注意：本渠道的 [DONE] 在 envelope 内部（body=="[DONE]"），**不是**客户端可见的
// data: [DONE] 帧，故它只用于停止解析，不能拿来抑制调用方的收尾帧。
// sseError 是「HTTP 200 + 信封内嵌 statusCodeValue>=400」形态的流内业务错误。
//
// Qoder 上游把排队/限流拒绝塞在 SSE 信封里下发（2026-10-21 抓包实证）：
//
//	data:{"headers":{...},"body":"{\"code\":\"403\",\"message\":\"{\\\"code\\\":\\\"10605\\\","
//	"...\\\"subCode\\\":\\\"RESOURCE_LIMIT\\\"}\"}","statusCodeValue":403,"statusCode":"FORBIDDEN"}
//
// 修复前信封只解 body：该帧的 body 是合法 JSON（但没有 choices），于是被当普通 chunk
// 原样透传，客户端既看不懂也收不到失败信号。两种表现都坏——
//   - 上游保持连接：连续无字节，90s 后被空闲看门狗掐成「upstream stream truncated」，
//     错误原因（限流/排队）在日志与客户端两侧都看不见；
//   - 上游发完即断连：parseNestedSSE 正常返回 nil，收尾补一个 data:[DONE]，
//     **失败被包装成「空内容的成功流」**。
//
// 现在：判出 statusCodeValue>=400 立刻返回本错误，调用方补一帧带原因的 SSE error
// 并**不补 [DONE]**（R36，同 loomy/officeace 口径），handler 据 Kind() 决定惩罚。
type sseError struct {
	status     int
	msg        string
	retryAfter int // 上游 retryAfterSeconds / waitTime；0 = 上游未声明
}

func (e *sseError) Error() string { return fmt.Sprintf("upstream %d: %s", e.status, e.msg) }

// Kind 复用与 HTTP 路径同一套 Classify，使 server 层不必 import 本渠道包。
func (e *sseError) Kind() provider.ErrKind { return Classify(e.status, e.msg) }

var _ provider.StreamErrorClassifier = (*sseError)(nil)

// envelope Qoder 系 SSE 的外层信封。body 是**字符串**（内层 JSON 文本），
// 与 internal/qwenwork 的 json.RawMessage 形状不同，不能照抄那边的写法。
type envelope struct {
	Body            string              `json:"body"`
	Headers         map[string][]string `json:"headers"`
	StatusCodeValue int                 `json:"statusCodeValue"`
	StatusCode      string              `json:"statusCode"`
}

// errText 提取上游错误文案；非错误信封返回空串。
// body 形如 {"code":"403","message":"<再嵌一层的 JSON 文本>"}，文案取 message 原文
// （内层 10605/RESOURCE_LIMIT 等结构化字段随之带出，不做二次解析）。
func (e *envelope) errText() string {
	if e.StatusCodeValue < 400 {
		return ""
	}
	var withMsg struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(e.Body), &withMsg) == nil && withMsg.Message != "" {
		return withMsg.Message
	}
	return truncate(e.Body, 300)
}

// retryAfterRe 从错误文案里取建议退避秒数。上游在同一处既给 retryAfterSeconds 也给
// waitTime，取前者、回落后者；只匹配十进制整数。
//
// 必须容忍键名两侧的反斜杠：该字段在原始信封 body 里被逐层转义，解出一层后仍带反斜杠。
// 只匹配裸 "retryAfterSeconds" 的正则会静默失配 → retry_after_seconds 不下发，
// 客户端只能盲重试。
var (
	retryAfterRe = regexp.MustCompile(`\\*"retryAfterSeconds\\*"\s*:\s*(\d+)`)
	waitTimeRe   = regexp.MustCompile(`\\*"waitTime\\*"\s*:\s*(\d+)`)
)

// maxRetryAfter 退避时长的合理上限：超了按未声明处理——宁可少给一个提示，
// 不可给一个错的等待时长。
const maxRetryAfter = 3600

func parseRetryAfter(s string) int {
	for _, re := range []*regexp.Regexp{retryAfterRe, waitTimeRe} {
		if m := re.FindStringSubmatch(s); m != nil {
			var n int
			if _, err := fmt.Sscanf(m[1], "%d", &n); err == nil && n > 0 && n <= maxRetryAfter {
				return n
			}
		}
	}
	return 0
}

// streamErrorCode 给出出口 error 帧的 code，便于客户端/日志分原因。
func streamErrorCode(k provider.ErrKind) string {
	switch k {
	case provider.ErrSoftRate:
		return "upstream_rate_limited"
	case provider.ErrSessionDead:
		return "upstream_session_dead"
	default:
		return "upstream_business_error"
	}
}

// writeStreamErrorFrame 把流内业务错误写成一帧 SSE error（调用方**不得**再补 [DONE]）。
// retry_after_seconds 只在上游声明了退避时长时输出——上游没给就不编造。
func writeStreamErrorFrame(w io.Writer, e *sseError) {
	errObj := map[string]any{
		"message": truncate(e.msg, 300),
		"type":    "upstream_error",
		"code":    streamErrorCode(e.Kind()),
	}
	if e.retryAfter > 0 {
		errObj["retry_after_seconds"] = e.retryAfter
	}
	frame, merr := json.Marshal(map[string]any{"error": errObj})
	if merr != nil {
		// 纯字符串/整数 map，序列化不会失败；真失败也要保证客户端拿到一帧原因。
		frame = []byte(`{"error":{"message":"upstream business error","type":"upstream_error","code":"upstream_business_error"}}`)
	}
	if _, werr := w.Write(append(append([]byte("data: "), frame...), []byte("\n\n")...)); werr != nil {
		log.Printf("qodercom stream error frame write failed: %v", werr)
		return
	}
	if fl, ok := w.(interface{ Flush() }); ok {
		fl.Flush()
	}
}

func parseNestedSSE(r io.Reader, onChunk func(map[string]any) error, truncated *bool) error {
	set := func() {
		if truncated != nil {
			*truncated = true
		}
	}
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			set() // 连接中断 / 读超时：可证实的截断
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			var env envelope
			outerOK := json.Unmarshal([]byte(payload), &env) == nil
			if outerOK && env.Body != "" {
				// 排队 / 额度耗尽 / 业务错误帧：**必须在 onChunk 之前**识别——这类帧没有
				// choices，透传出去等于给客户端一个空 delta，界面停在「正在等待模型…」，
				// 直到网关掐断（2026-10-20 qodercom 实测同族形态）。
				// 顺序不可换：10605→Queued（模型级，R109）；110/112→HardCredit（账号级，R111）；
				// 其余 >=400→sseError（R108，带 retry_after_seconds）。
				if e := envelopeNotice(env.Body, env.StatusCodeValue, env.Headers); e != nil {
					return e
				}
				if env.StatusCodeValue >= 400 {
					msg := env.errText()
					if msg == "" {
						msg = truncate(env.Body, 300)
					}
					return &sseError{status: env.StatusCodeValue, msg: msg, retryAfter: parseRetryAfter(env.Body + "\n" + msg)}
				}
			}
			innerOK := false
			if outerOK && env.Body != "" {
				if env.Body == "[DONE]" {
					return nil // 上游显式收尾
				}
				var chunk map[string]any
				innerOK = json.Unmarshal([]byte(env.Body), &chunk) == nil
				if innerOK {
					if err := onChunk(chunk); err != nil {
						return err
					}
				}
			}
			if err == io.EOF && !(outerOK && innerOK) {
				set() // EOF 处这一行（内层或外层）不是完整 JSON：半个帧
			}
		}
		if err == io.EOF {
			// 最后一行没有换行符收尾且非空 → 帧不完整（半帧）；正常收尾的最后一行是空行。
			if strings.TrimSpace(line) != "" {
				set()
			}
			return nil
		}
	}
}

// envelopeNotice 识别「HTTP 200 里包着的业务错误帧」：这种帧没有 choices，
// 当普通 chunk 透传只会让客户端拿到空 delta 干等。返回 nil 表示「不是错误帧」。
//
// 两类语义必须分开（2026-10-20 本渠道实测）：
//
//	10605 / isQueued        → *provider.Queued：**模型级**拥塞，换号可能命中不同队列，
//	                          只冷「号×模型」30s（同 qodercn，但两渠道各写一份、不共享代码）
//	110 / 112 / pricingUrl  → ErrHardCredit：**账号级**额度耗尽。A/B 实测同号换模型
//	                          glm-5.3-flash 同样回 112，且 quota 接口同时刻
//	                          isQuotaExceeded=true / remaining=0 ⇒ 整号硬冷却 + 换号。
//	                          码 110 的文案是「Billing daily count exceeded」，
//	                          不含 hardMarkers 里的 "quota exceeded"，必须显式列码。
func envelopeNotice(body string, statusCode int, headers map[string][]string) error {
	if body == "" || strings.Contains(body, `"choices"`) {
		return nil
	}
	vals := qoderUnwrap(body)
	if q := queuedNotice(vals, headers); q != nil {
		return q
	}
	if code := quotaExhaustedCode(vals); code != "" {
		// 包成 Classified：流式路径要靠 Kind() 认出 ErrHardCredit 才能「整号冷却 + 换号」。
		return &provider.Classified{Err: &provider.Error{
			Kind:   provider.ErrHardCredit,
			Status: 402, // 语义是付费墙，透传给客户端用 402 比 403 准确
			Msg:    "upstream quota exhausted (code " + code + "): " + truncate(body, 200),
		}}
	}
	// 通用 statusCode>=400 交给 sseError：它能从文案里取 retryAfterSeconds，
	// 并在出口补带 code/retry_after_seconds 的结构化 error 帧（R108）。
	return nil
}

// quotaExhaustedCode 识别「账号额度/计费限制」帧，返回命中的 code（未命中返回 ""）。
// 判据全部来自本渠道实测原文：内层 code 为 110/112，或带 isQuotaExceeded:true，
// 或带 pricingUrl（112 的 message 体 = 付费墙链接）。
func quotaExhaustedCode(vals map[string]any) string {
	if code, _ := vals["code"].(string); code == "110" || code == "112" {
		return code
	}
	if b, ok := vals["isQuotaExceeded"].(bool); ok && b {
		return "isQuotaExceeded"
	}
	if _, ok := vals["pricingUrl"]; ok {
		return "pricingUrl"
	}
	return ""
}

// queuedNotice 识别「模型排队中」帧（qodercn 的 10605 isQueued 同族形态）：
// 外层 headers 带 Retry-After，内层带 queueCount / retryAfterSeconds / waitTime。
func queuedNotice(vals map[string]any, headers map[string][]string) *provider.Queued {
	code, _ := vals["code"].(string)
	queued, _ := vals["isQueued"].(bool)
	if code != "10605" && !queued {
		return nil
	}
	q := &provider.Queued{}
	q.ModelKey, _ = vals["modelKey"].(string)
	if f, ok := vals["queueCount"].(float64); ok {
		q.QueueCount = int(f)
	}
	if f, ok := vals["waitTime"].(float64); ok {
		q.WaitSeconds = int(f)
	}
	if f, ok := vals["retryAfterSeconds"].(float64); ok && f > 0 {
		q.RetryAfter = time.Duration(f) * time.Second
	}
	if q.RetryAfter == 0 {
		for k, vs := range headers {
			if !strings.EqualFold(k, "Retry-After") || len(vs) == 0 {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(vs[0])); err == nil && n > 0 {
				q.RetryAfter = time.Duration(n) * time.Second
			}
		}
	}
	return q
}

// qoderUnwrap 把「message 字段里再套一层 JSON 字符串」逐层解开，收集各层键值
// （深层覆盖浅层）。上游三层套娃：{code:403} → {code:10605} → {...}。
func qoderUnwrap(body string) map[string]any {
	vals := map[string]any{}
	cur := body
	for depth := 0; depth < 6 && cur != ""; depth++ {
		var m map[string]any
		if json.Unmarshal([]byte(cur), &m) != nil {
			break
		}
		for k, v := range m {
			vals[k] = v
		}
		next, _ := m["message"].(string)
		if next == "" || next == cur {
			break
		}
		cur = next
	}
	return vals
}

// aggregate 聚合完整 SSE 为单个 OpenAI chat.completion 响应。
// model 参数覆盖响应中的 model 字段（上游恒为 "auto"）。
// tool_calls 以流式 delta 到达（按 index 合并）。
func aggregate(r io.Reader, model string) (map[string]any, error) {
	var (
		id           string
		created      float64
		content      strings.Builder
		reasoning    strings.Builder
		role         = "assistant"
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
	)
	err := parseNestedSSE(r, func(chunk map[string]any) error {
		if v, ok := chunk["id"].(string); ok && id == "" {
			id = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = v
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		ch, _ := chunk["choices"].([]any)
		for _, ci := range ch {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			if delta, ok := c["delta"].(map[string]any); ok {
				if r2, ok := delta["role"].(string); ok && r2 != "" {
					role = r2
				}
				if txt, ok := delta["content"].(string); ok {
					content.WriteString(txt)
				}
				if rc, ok := delta["reasoning_content"].(string); ok {
					reasoning.WriteString(rc)
				}
				if tcs, ok := delta["tool_calls"].([]any); ok {
					for _, tc := range tcs {
						call, ok := tc.(map[string]any)
						if !ok {
							continue
						}
						idx := 0
						if v, ok := call["index"].(float64); ok {
							idx = int(v)
						}
						merged, seen := toolCalls[idx]
						if !seen {
							merged = map[string]any{"index": idx}
							toolCalls[idx] = merged
							toolOrder = append(toolOrder, idx)
						}
						mergeToolCallDelta(merged, call)
					}
				}
			}
		}
		return nil
	}, nil)
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sortInts(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖，function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// sortInts 升序排序（避免引 sort 包只为三行）。
func sortInts(a []int) {
	for i := 0; i < len(a)-1; i++ {
		for j := i + 1; j < len(a); j++ {
			if a[j] < a[i] {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}

// streamAsOpenAI 把嵌套 SSE 流边读边转写为标准 OpenAI SSE 给客户端。
// 每个 chunk 重写 model 字段为客户端模型名。
//
// 收尾规则（issue #42）：正常收尾（含上游未发 [DONE] 的兜底）补 data: [DONE]；
// 可证实的截断（传输层错误 / 停在半个帧上）改发一帧 OpenAI 规范 error 且**不补**
// [DONE]，让客户端明确感知失败，而不是收到半截 tool_call 参数。
func streamAsOpenAI(w io.Writer, r io.Reader, model string, flush func()) (map[string]any, error) {
	var usage map[string]any
	truncated := false
	// 思考链死循环守卫：仅在全局启用时创建。命中即中断、不罚账号。
	var guard *loopguard.Guard
	if loopguard.Enabled() {
		guard = loopguard.NewDefault()
	}
	err := parseNestedSSE(r, func(chunk map[string]any) error {
		// 思考链死循环守卫：只在 reasoning_content 非空时喂入（无思维链的模型/配置
		// 不经过守卫，零开销零误伤）。命中即中断。
		if guard != nil {
			if rc := loopguard.ReasoningDeltaOf(chunk); rc != "" {
				if hit := guard.Push(rc); hit != nil {
					gerr := &loopguard.Error{Hit: *hit}
					if _, werr := io.WriteString(w, "data: "+loopguard.ErrorFrameJSON(hit)+"\n\n"); werr != nil {
						log.Printf("qodercom reasoning loop error frame write failed: %v", werr)
						return gerr
					}
					if flush != nil {
						flush()
					}
					return gerr
				}
			}
		}
		// usage 捕获：末帧覆盖前面（OpenAI 语义末帧才是全量），并照常透传
		if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
			usage = u
		}
		chunk["model"] = model
		raw, _ := json.Marshal(chunk)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
		return nil
	}, &truncated)
	// 截断优先于 err：读错误（连接中断/超时）本身就体现为 err != nil，必须先把
	// 已透传的半截流的收尾处理掉（发 error 帧、不补 [DONE]），再报错给调用方。
	//
	// 出口帧与错误文本都**保留原始原因**（R58）：此前无论什么原因都只报固定串
	// "upstream stream truncated"，err 被丢弃 ⇒ 生产上分不清「空闲看门狗掐的」
	// （连续无字节，疑似上游排队/卡死）与「上游主动断连 / 读到半个帧」。
	// 空闲超时另给 code=upstream_timeout，其余仍为 upstream_truncated。
	if truncated {
		if werr := provider.WriteTruncationFrame(w, err); werr != nil {
			return usage, werr
		}
		if flush != nil {
			flush()
		}
		return usage, provider.TruncationError(err)
	}
	// 流内业务错误（信封内嵌 statusCodeValue>=400，R108）：补一帧带原因的 SSE error
	// 并**不补 [DONE]**（R36）——补了就把失败伪装成正常结束，客户端会拿着空结果继续走，
	// 网关侧还把它记成一条成功流水。原因与退避时长一并结构化传出，重试交回客户端。
	var sErr *sseError
	if errors.As(err, &sErr) {
		writeStreamErrorFrame(w, sErr)
		if flush != nil {
			flush()
		}
		return usage, sErr
	}
	if err != nil {
		return usage, err
	}
	// 正常收尾（含上游未发 [DONE] 的兜底）：保证恰好一个 data: [DONE]。
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return usage, err
	}
	if flush != nil {
		flush()
	}
	return usage, nil
}

// Stream 实现 provider.Upstream（无 usage 回调的轻量入口，测试与通用路径共用）。
func Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return StreamCapture(w, r, model, nil)
}

// StreamCapture 同 Stream，额外把末帧 usage 回调给 onUsage（非 nil 时）。
func StreamCapture(w http.ResponseWriter, r io.Reader, model string, onUsage func(map[string]any)) (map[string]any, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	usage, err := streamAsOpenAI(w, r, model, flush)
	if err == nil && onUsage != nil && usage != nil {
		onUsage(usage)
	}
	return usage, err
}
