// solosse.go SOLO 自定义 SSE 解析 → OpenAI SSE（流式转换 + 非流式聚合）。
//
// SOLO 事件序列（SPEC §4.6，实测）：
//
//	id:1
//	event:metadata
//	data:{"model":"","session_id":"...","prompt_completion_id":0,...}
//
//	id:2
//	event:timing_cost
//	data:{"name":"llm_raw_chat_v2",...}
//
//	event:output                          ← ×N，核心内容
//	data:{"response":"<content 增量>",
//	      "reasoning_content":"<思考链增量>",
//	      "tool_calls":<null 或工具调用>}
//
//	event:extra_info                       ← 含 reasoning_content 完整版
//	event:token_usage
//	data:{"prompt_tokens":21,"completion_tokens":142,"total_tokens":163,"reasoning_tokens":135}
//
//	event:done
//	data:{"finish_reason":"stop"}
package traework

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

// SOLOEvent 单条 SOLO SSE 事件（归一化）。
type SOLOEvent struct {
	Event        string          // metadata | timing_cost | output | extra_info | token_usage | done | error | queue_begin | request_wait_in_queue
	Response     string          // output: content 增量
	Reasoning    string          // output: 思考链增量
	ToolCalls    json.RawMessage // output: 工具调用（null 或对象/数组）
	Usage        map[string]any  // token_usage
	FinishReason string          // done
	ErrorCode    int64           // error
	ErrorMessage string          // error
	QueuePos     int64           // request_wait_in_queue: 上游排队位置（拥塞观测用）
}

// SOLOStreamError 上游 SSE 流内的业务错误（event:error）。非流式聚合时返回，
// 调用方可据此分类冷却账号并轮转。
type SOLOStreamError struct {
	Code int64
	Msg  string
}

func (e *SOLOStreamError) Error() string {
	return fmt.Sprintf("solo error code=%d msg=%s", e.Code, e.Msg)
}

// Kind 将 SSE 流内错误分类。
//
//	1005 → provider.ErrHardCredit（权益/余额不足 → 长冷却）
//	3004 / 9074 → provider.ErrSoftRate（限流 → 短冷却）
//	其余 → provider.ErrClient
//
// 3004 归软限流的依据（**按 R23b：这是待强化的假设，不是既成事实**）：
// 2026-09-29 09:00 checkin 路径的观察——同一秒、同一 IP 下 8 个 traework 账号中
// 仅 1 个（1606848467182378）返回 3004，其余 7 个 ok。该观察**足以排除 IP/全局级**
// 限流，但**不足以证明是账号级**：样本仅 1 例、且是 checkin 路径（非本修复作用的
// 对话流）。要坐实需补「同号连续 N 次稳定 3004 + 换号立即成功」的配对观测。
//
// 之所以仍按「账号级」处置：软冷却只有 60 秒，即使判据不成立，最坏影响也只是
// 某账号闲置一分钟（风险不对称），不会造成危害。详见 AGENTS.md R39。
func (e *SOLOStreamError) Kind() provider.ErrKind {
	switch e.Code {
	case 1005:
		return provider.ErrHardCredit
	case 3004, 9074:
		return provider.ErrSoftRate
	}
	return provider.ErrClient
}

// ParseSOLOLine 解析一条事件（eventName 为 event 行值，dataLine 为 data 行值）。
func ParseSOLOLine(eventName, dataLine string) (*SOLOEvent, error) {
	ev := &SOLOEvent{Event: strings.TrimSpace(eventName)}
	if dataLine == "" {
		return ev, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(dataLine), &raw); err != nil {
		return nil, err
	}
	switch ev.Event {
	case "output":
		if v, ok := raw["response"].(string); ok {
			ev.Response = v
		}
		if v, ok := raw["reasoning_content"].(string); ok {
			ev.Reasoning = v
		}
		if tc, ok := raw["tool_calls"]; ok {
			ev.ToolCalls, _ = json.Marshal(tc)
		}
	case "token_usage":
		ev.Usage = raw
	case "done":
		if v, ok := raw["finish_reason"].(string); ok {
			ev.FinishReason = v
		}
	case "error":
		if v, ok := raw["code"].(float64); ok {
			ev.ErrorCode = int64(v)
		}
		if v, ok := raw["message"].(string); ok {
			ev.ErrorMessage = v
		}
	case "queue_begin", "request_wait_in_queue":
		// 上游拥塞排队心跳：position 递减。透传给日志观测（限频由消费方控制）。
		// queue_begin 帧无 position 字段（QueuePos=0），消费方应跳过或等首条心跳。
		if v, ok := raw["position"].(float64); ok {
			ev.QueuePos = int64(v)
		}
	}
	return ev, nil
}

// sseState 维护一行 SSE 的 event/data 跨行累积。
type sseState struct {
	event string
	data  strings.Builder
}

// reset 清空状态（事件边界触发）。
func (s *sseState) reset() {
	s.event = ""
	s.data.Reset()
}

// scanLine 处理一行；返回该行触发的事件（事件边界时解析并返回）。
func scanLine(st *sseState, line string) *SOLOEvent {
	switch {
	case line == "":
		if st.event == "" {
			st.reset()
			return nil
		}
		ev, err := ParseSOLOLine(st.event, st.data.String())
		st.reset()
		if err != nil {
			return nil
		}
		return ev
	case strings.HasPrefix(line, "event:"):
		st.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		st.data.WriteString(strings.TrimPrefix(line, "data:"))
	case strings.HasPrefix(line, ":"):
		// 注释行忽略
	}
	return nil
}

// AggregateWithModel 同 Aggregate，额外把 model 回填到响应（上游 SOLO 事件不带模型名）。
func AggregateWithModel(r io.Reader, model string) (map[string]any, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var (
		id           string
		content      strings.Builder
		reasoning    strings.Builder
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
		upstreamErr  error
	)
	st := &sseState{}
	// 排队心跳限频：首条 + 每 30s 一条（与 streamOpts 同策略）。
	var lastQueueLog time.Time
	queueFirst := true
	logQueue := func(pos int64) {
		if pos <= 0 {
			return // queue_begin 无 position，跳过，等首条真实心跳
		}
		now := time.Now()
		if queueFirst || now.Sub(lastQueueLog) >= 30*time.Second {
			log.Printf("solo queue congested model=%s position=%d", model, pos)
			queueFirst = false
			lastQueueLog = now
		}
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return nil, err
		}
		if ev := scanLine(st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "metadata":
				if id == "" && ev.Usage != nil {
					// metadata 无 id 可用，保留为空，末尾补 chatcmpl。
				}
			case "output":
				content.WriteString(ev.Response)
				reasoning.WriteString(ev.Reasoning)
				mergeToolCallJSON(toolCalls, &toolOrder, ev.ToolCalls)
			case "token_usage":
				usage = ev.Usage
			case "done":
				if ev.FinishReason != "" {
					finishReason = ev.FinishReason
				}
			case "error":
				upstreamErr = &SOLOStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
			case "queue_begin", "request_wait_in_queue":
				logQueue(ev.QueuePos)
			}
		}
		if err == io.EOF {
			break
		}
	}
	if upstreamErr != nil {
		return nil, upstreamErr
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	message := map[string]any{
		"role":    "assistant",
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
		"created": time.Now().Unix(),
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
		// 缓存计量归一化（issue #104）：trae 上游下发的是 Anthropic 字段名
		// （cache_read_input_tokens / cache_creation_input_tokens），下游按 OpenAI 规范
		// 解析（prompt_tokens_details.cached_tokens ?? prompt_cache_hit_tokens）两条都读不到
		// ⇒ 命中率恒 0%、费用高估。此处补 OpenAI 形状子字段，原始字段保留。
		provider.NormalizeUsageShape(usage)
		resp["usage"] = usage
	}
	return resp, nil
}

// mergeToolCallJSON 把 SOLO output.tool_calls（json.RawMessage，可能 null/对象/数组）
// 合并进 toolCalls（按 index）。
func mergeToolCallJSON(toolCalls map[int]map[string]any, toolOrder *[]int, raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		var one map[string]any
		if json.Unmarshal(raw, &one) != nil {
			return
		}
		arr = []map[string]any{one}
	}
	for _, call := range arr {
		if call == nil {
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
			*toolOrder = append(*toolOrder, idx)
		}
		mergeToolCallDelta(merged, call)
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖，function.arguments 拼接。
// 上游 SOLO 用 `function_call` 字段（实测），OpenAI 标准用 `function`；两者都兼容。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		df, _ = delta["function_call"].(map[string]any) // SOLO 专属字段名
	}
	if df == nil {
		return
	}
	// 清理 SOLO 专属字段,只保留标准 OpenAI function 结构(name/arguments)
	delete(df, "namespace")
	delete(df, "partial_arguments")
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

// StreamWithModel 同 Stream，额外把 model 回填到每个 chunk（上游 SOLO 事件不带模型名）。
// Stream 实现 provider.Upstream（无 model/usage 的轻量入口，测试与通用路径共用）。
func Stream(w http.ResponseWriter, r io.Reader) error {
	_, err := StreamWithModel(w, r, "")
	return err
}

func StreamWithModel(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return streamOpts(w, r, nil, model, nil)
}

// streamOpts Stream 的可选参数版本；onUsage 非 nil 时回调 token_usage 事件的 usage。
func streamOpts(w http.ResponseWriter, r io.Reader, onErr func(*SOLOStreamError), model string, onUsage func(map[string]any)) (map[string]any, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	br := bufio.NewReaderSize(r, 64*1024)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	var pendingUsage map[string]any
	sawDone := false
	st := &sseState{}
	// 思考链死循环守卫（发癫检测）：仅在全局启用时创建。命中即中断、不罚账号。
	var guard *loopguard.Guard
	if loopguard.Enabled() {
		guard = loopguard.NewDefault()
	}
	// 排队心跳限频：首条 + 每 30s 一条，避免长时间拥塞时刷屏。
	var lastQueueLog time.Time
	queueFirst := true
	logQueue := func(pos int64) {
		if pos <= 0 {
			return // queue_begin 无 position，跳过，等首条真实心跳
		}
		now := time.Now()
		if queueFirst || now.Sub(lastQueueLog) >= 30*time.Second {
			log.Printf("solo queue congested model=%s position=%d", model, pos)
			queueFirst = false
			lastQueueLog = now
		}
	}
	writeChunk := func(delta map[string]any, finish string) error {
		chunk := map[string]any{
			"id":      id,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []any{
				map[string]any{
					"index": 0,
					"delta": delta,
				},
			},
		}
		choice := chunk["choices"].([]any)[0].(map[string]any)
		if finish != "" {
			choice["finish_reason"] = finish
		}
		if pendingUsage != nil {
			chunk["usage"] = pendingUsage
			pendingUsage = nil
		}
		raw, _ := json.Marshal(chunk)
		if _, err := io.WriteString(w, "data: "+string(raw)+"\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}
	writeDONE := func() error {
		if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
			return err
		}
		if fl != nil {
			fl.Flush()
		}
		return nil
	}

	var usage map[string]any
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			// 流中断（空闲超时 / 连接被切断）：响应头早已按 200 发出，只能用流内帧表达故障。
			// 不补帧 = 客户端收到无收尾的截断流 =「突然无响应」。
			provider.WriteTruncationFrames(w, err)
			return usage, err
		}
		if ev := scanLine(st, strings.TrimRight(line, "\r\n")); ev != nil {
			switch ev.Event {
			case "output":
				// 思考链死循环守卫：只在 reasoning_content 非空时喂入（无思维链的
				// 模型/配置不经过守卫，零开销零误伤）。命中即中断、不罚账号。
				if guard != nil && ev.Reasoning != "" {
					if hit := guard.Push(ev.Reasoning); hit != nil {
						gerr := &loopguard.Error{Hit: *hit}
						frame := "data: " + loopguard.ErrorFrameJSON(hit) + "\n\n"
						if _, werr := io.WriteString(w, frame); werr != nil {
							log.Printf("traework reasoning loop error frame write failed: %v", werr)
						} else if fl != nil {
							fl.Flush()
						}
						return usage, gerr
					}
				}
				delta := map[string]any{}
				if ev.Response != "" {
					delta["content"] = ev.Response
				}
				if ev.Reasoning != "" {
					delta["reasoning_content"] = ev.Reasoning
				}
				if len(ev.ToolCalls) > 0 && string(ev.ToolCalls) != "null" {
					var tc []map[string]any
					if err := json.Unmarshal(ev.ToolCalls, &tc); err == nil {
						// SOLO 上游 tool_call 条目用 `function_call` 字段 → 转成 OpenAI 的 `function`
						for _, call := range tc {
							if fc, ok := call["function_call"].(map[string]any); ok {
								call["function"] = fc
								delete(call, "function_call")
							}
							// 清理 SOLO 专属字段,只保留标准 OpenAI function 结构(name/arguments)
							if fn, ok := call["function"].(map[string]any); ok {
								delete(fn, "namespace")
								delete(fn, "partial_arguments")
							}
						}
						delta["tool_calls"] = tc
					}
				}
				if len(delta) > 0 {
					if err := writeChunk(delta, ""); err != nil {
						return usage, err
					}
				}
			case "token_usage":
				// 缓存计量归一化（issue #104）放在**捕获点**而非写出点：这样
				// onUsage 回调、返回给调用方的 usage、以及「不含 done 的异常收尾」
				// 三条路径都拿到 OpenAI 形状（trae 上游给的是 Anthropic 字段名）。
				// （本地旧 mirrorCacheReadToOpenAI 是其子集，合并时摘除。）
				if ev.Usage != nil {
					provider.NormalizeUsageShape(ev.Usage)
				}
				pendingUsage = ev.Usage
				if onUsage != nil && ev.Usage != nil {
					onUsage(ev.Usage)
				}
			case "queue_begin", "request_wait_in_queue":
				logQueue(ev.QueuePos)
			case "done":
				// 必须先取再写：writeChunk 会消费 pendingUsage（附加进 chunk 后置
				// nil），SOLO 协议 token_usage 先于 done 到达，先 writeChunk 再
				// 取值会永远拿到 nil → 记账流水丢失 token（客户端不受影响，
				// usage 已透传；仅网关自身统计为空）。
				usage = pendingUsage
				if err := writeChunk(map[string]any{}, ev.FinishReason); err != nil {
					return usage, err
				}
				if err := writeDONE(); err != nil {
					return usage, err
				}
				sawDone = true
			case "error":
				// 上游 SOLO 业务错误（1005 权益 / 1001 模型不可用 / 3004 限流 …）。
				//
				// 旧实现把它写成 delta.content 文本 + finish_reason:"stop" + 补 [DONE]，
				// 客户端看到的是「正常结束、内容莫名其妙」，agent 会拿着半截回答继续跑；
				// 且函数返回 nil，调用方无从得知失败。这与上游 issue #42 修的是同一类
				// 「把失败伪装成成功收尾」问题（当时只覆盖了 qwenwork/qoder 系，漏了 traework）。
				//
				// 现改为（对齐 #42）：发一帧 OpenAI 规范 error，**不补 [DONE]**，
				// 并把错误返回给调用方 —— 由 handler 据此冷却账号、打破粘性路由，
				// 使客户端重试时能挑到别的账号（3004 是账号级限流，换号有效）。
				se := &SOLOStreamError{Code: ev.ErrorCode, Msg: ev.ErrorMessage}
				if onErr != nil {
					onErr(se)
				}
				// 防御分支：若 done 已发过 [DONE]，流在本客户端看来已正常终止，
				// 此时再写 error 帧会造成「两个终止信号」（严格客户端行为未定义）。
				// 故只记日志并把错误返回给调用方（账号照常冷却），不再写第二帧。
				// 上游理论上不会 done→error，此处纯防御。
				if sawDone {
					log.Printf("traework stream error after done code=%d msg=%s（已终止，不再发 error 帧）",
						se.Code, se.Msg)
					return usage, se
				}
				code := "upstream_error"
				if se.Kind() == provider.ErrSoftRate {
					code = "upstream_rate_limited"
				}
				frame := fmt.Sprintf(`{"error":{"message":%s,"type":"upstream_error","code":%s}}`,
					jsonEscape(se.Error()), jsonEscape(code))
				// 写入失败（客户端已断开）时**仍返回业务错误**：调用方要据此冷却账号，
				// 若改成返回 werr 会丢掉 *SOLOStreamError，errors.As 取不到分类，
				// 账号就不会被冷却——本次修复的目标链路恰好断在这里。
				if _, werr := io.WriteString(w, "data: "+frame+"\n\n"); werr != nil {
					log.Printf("traework stream error frame write failed uid-less code=%d: %v", se.Code, werr)
				} else if fl != nil {
					fl.Flush()
				}
				// 刻意不写 [DONE]：终止信号交给 error 帧，避免被当成正常收尾。
				// （若上游在本帧之前已发过 done，则 [DONE] 已写出、此处不再补第二个——
				// 两个终止信号会让严格客户端行为未定义。此顺序上游理论上不会发，仅防御。）
				return usage, se
			}
		}
		if err == io.EOF {
			break
		}
	}
	if !sawDone {
		// 幂等兜底：上游中断（无 done）仍写 [DONE]。
		werr := writeDONE()
		return usage, werr
	}
	return usage, nil
}

func jsonEscape(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}
