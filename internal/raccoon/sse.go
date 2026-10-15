package raccoon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"wild-work/internal/loopguard"
	"wild-work/internal/provider"
)

// 上游已是标准 OpenAI SSE（`data: {...}` + 结尾 `data: [DONE]`），
// 因此这里只做「重写 model 字段 + 透传」，不做协议转换。
// 参考：阶段 C 实测的 13 个事件序列与 delta 形状。

// Stream 实现 provider.Upstream。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return Stream(w, r, model)
}

// Aggregate 实现 provider.Upstream。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	return aggregate(r, model)
}

// Stream 边读边把上游 SSE 转写为标准 OpenAI SSE 给客户端（每个 chunk 重写 model）。
// 返回末帧捕获的 usage（OpenAI 形状，供 handler 记 token 流水）；上游未返回时为 nil。
func Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
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
	// 思考链死循环守卫：仅在全局启用时创建。命中即中断、不罚账号。
	var guard *loopguard.Guard
	if loopguard.Enabled() {
		guard = loopguard.NewDefault()
	}

	sc := newSSEScanner(r)
	for sc.Scan() {
		payload, ok := ssePayload(sc.Text())
		if !ok {
			continue
		}
		if payload == "[DONE]" {
			// 上游已收尾，直接把 DONE 转发给客户端并结束。
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flush()
			return usage, nil
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// 解析不了的行原样透传：宁可让客户端看到上游原文，也不要吞掉信息。
			if _, werr := fmt.Fprintf(w, "data: %s\n\n", payload); werr != nil {
				return usage, werr
			}
			flush()
			continue
		}
		// 思考链死循环守卫：只在 reasoning_content 非空时喂入。命中即中断。
		if guard != nil {
			if rc := loopguard.ReasoningDeltaOf(chunk); rc != "" {
				if hit := guard.Push(rc); hit != nil {
					gerr := &loopguard.Error{Hit: *hit}
					if _, werr := io.WriteString(w, "data: "+loopguard.ErrorFrameJSON(hit)+"\n\n"); werr != nil {
						log.Printf("raccoon reasoning loop error frame write failed: %v", werr)
					} else {
						flush()
					}
					return usage, gerr
				}
			}
		}
		// usage 捕获：末帧覆盖前面（OpenAI 语义末帧才是全量），并照常透传
		if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
			usage = u
		}
		chunk["model"] = model
		raw, err := json.Marshal(chunk)
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return usage, err
		}
		flush()
	}
	if err := sc.Err(); err != nil {
		// 流中断（空闲超时 / 连接被切断）：响应头早已按 200 发出，只能用流内帧表达故障。
		// 不补帧 = 客户端收到无收尾的截断流 =「突然无响应」（与 loomy 同源，见其注释）。
		provider.WriteTruncationFrames(w, err)
		return usage, err
	}
	// 上游异常收尾（无 [DONE]）时补一个，避免客户端一直等。
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flush()
	return usage, nil
}

// aggregate 把上游响应转成单个 chat.completion（非流式请求走这里）。
//
// 上游对非流式请求**可能直接返回 JSON 而不是 SSE**（实测 Loomy 的 stream=false 路径就是普通
// chat.completion JSON）。若只按 SSE 解析，会得到「content 空 + created 用 time.Now() 兜底」的
// 假响应 —— 静默丢失全部内容。所以先探测 JSON，探测不到再回落 SSE 聚合。
func aggregate(r io.Reader, model string) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 16<<20))
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		var out map[string]any
		if err := json.Unmarshal(trimmed, &out); err == nil {
			// 回填客户端模型名（上游常返回裸名或别名）。
			out["model"] = model
			return out, nil
		}
	}
	return aggregateSSE(bytes.NewReader(trimmed), model)
}

// aggregateSSE 按 SSE 逐块聚合（上游返回流式时的路径）。
func aggregateSSE(r io.Reader, model string) (map[string]any, error) {
	var (
		id        string
		created   float64
		content   strings.Builder
		reasoning strings.Builder
		finish    string
		usage     map[string]any
		toolIdx   []int
		toolCalls = map[int]map[string]any{}
	)

	sc := newSSEScanner(r)
	for sc.Scan() {
		payload, ok := ssePayload(sc.Text())
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if id == "" {
			if s, ok := chunk["id"].(string); ok {
				id = s
			}
		}
		if c, ok := chunk["created"].(float64); ok && created == 0 {
			created = c
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch, _ := choices[0].(map[string]any)
		if ch == nil {
			continue
		}
		if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
			finish = fr
		}
		delta, _ := ch["delta"].(map[string]any)
		if delta == nil {
			if msg, ok := ch["message"].(map[string]any); ok {
				delta = msg
			}
		}
		if delta == nil {
			continue
		}
		if s, ok := delta["content"].(string); ok {
			content.WriteString(s)
		}
		if s, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(s)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, item := range tcs {
				tc, _ := item.(map[string]any)
				if tc == nil {
					continue
				}
				idx := 0
				if f, ok := tc["index"].(float64); ok {
					idx = int(f)
				}
				merged, seen := toolCalls[idx]
				if !seen {
					merged = map[string]any{"index": idx}
					toolCalls[idx] = merged
					toolIdx = append(toolIdx, idx)
				}
				mergeToolCallDelta(merged, tc)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolIdx) > 0 {
		sort.Ints(toolIdx)
		list := make([]any, 0, len(toolIdx))
		for _, i := range toolIdx {
			list = append(list, toolCalls[i])
		}
		msg["tool_calls"] = list
	}
	if finish == "" {
		finish = "stop"
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out, nil
}

// mergeToolCallDelta 合并流式 tool_calls 分片（function.arguments 需要字符串拼接）。
func mergeToolCallDelta(merged, delta map[string]any) {
	if s, ok := delta["id"].(string); ok && s != "" {
		merged["id"] = s
	}
	if s, ok := delta["type"].(string); ok && s != "" {
		merged["type"] = s
	}
	dfn, _ := delta["function"].(map[string]any)
	if dfn == nil {
		return
	}
	fn, _ := merged["function"].(map[string]any)
	if fn == nil {
		fn = map[string]any{}
		merged["function"] = fn
	}
	if s, ok := dfn["name"].(string); ok && s != "" {
		fn["name"] = s
	}
	if s, ok := dfn["arguments"].(string); ok && s != "" {
		prev, _ := fn["arguments"].(string)
		fn["arguments"] = prev + s
	}
}

// newSSEScanner 构造 SSE 行扫描器（单行上限 4 MiB，足够容纳超大 chunk）。
func newSSEScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	return sc
}

// ssePayload 从一行中取出 `data:` 后的载荷；非 data 行返回 ok=false。
func ssePayload(line string) (string, bool) {
	line = strings.TrimRight(line, "\r")
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" {
		return "", false
	}
	return payload, true
}
