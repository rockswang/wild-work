// sse.go 处理 QoderCOM 嵌套 SSE：每行 data:{...,"body":"<json-string>"}，
// body 字段需二次解析得到标准 OpenAI chunk。
// 移植自 qoderwork2api internal/upstream/sse.go，含聚合与流式转写。
package qodercom

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/loopguard"
	"wild-work/internal/provider"
)

// parseNestedSSE 逐行解析嵌套 SSE，每个有效 chunk 调 onChunk。
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
			var env struct {
				Body string `json:"body"`
			}
			outerOK := json.Unmarshal([]byte(payload), &env) == nil
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

// Stream 实现 provider.Upstream：嵌套 SSE → 标准 OpenAI SSE 透传。
// 返回值为末帧捕获的 usage（供记账，上游未返回时为 nil）。
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
