// sse.go 入站转换：上游 **v1internal SSE** → 本仓内层统一使用的 **OpenAI Chat SSE**，
// 随后交给 internal/upstream 的 Stream/Aggregate 做规范化（那里已有 tool_calls 分片合并、
// 错误帧识别、截断自证等一整套逻辑，转换后零改动复用）。
//
// 为什么不把 Gemini 形状直接喂给上层：本仓内层协议恒为 OpenAI Chat（外层三接口在
// internal/gateway 收敛），渠道层负责方言投影。
//
// ## 上游 SSE 形态（协议 §2.5，逐条对应本文件）
//
//   - 每帧是**单行** `data:`（**不是** Anthropic 那种多行拼接），且**无 event 名**；
//   - `:` 开头的**注释行是心跳**，必须跳过（当作数据会 JSON 解析失败，误杀正常流）；
//   - `event:` 行直接忽略（上游不发，出现也与我们无关）；
//   - 结束标记是 **`data: [DONE]`**（不是 message_stop）；
//   - 载荷两种形态都认：先试信封 `{"response":{…}}`，再试**裸 Response**；
//   - **只有 `candidates` 非空才算内容帧**：纯 `usageMetadata` 的收尾帧要继续读；
//   - 用量取「见过的**最大** `totalTokenCount`」的那一份（上游多帧重复播报，早期帧偏小）；
//   - 流结束时必须把 buffer 余量**按整行再走一遍**，否则携带 finishReason / usageMetadata
//     的末帧会被静默丢弃（参考实现的真实缺陷，2026-09-29 由单测抓到）；
//   - 一个内容块都没出现 ⇒ **抛错**（静默结束会让上层认为「模型正常回答但没内容」而不重试）。
//
// ## ⚠️ 未确认项（如实标注，不要当成事实）
//
//   - **流内 error 帧的确切形态未确认**：参考实现的解析层对非 JSON / `[DONE]` 帧直接忽略，
//     未见对 in-stream error 对象的处理；错误实际靠 HTTP 状态码 + 响应体文本传达。
//     本包照同一口径：**非 JSON 帧忽略**（不猜形态），错误由 ChatStream 的 status≥400 分支
//     原文透出（Classify 归类）。
//   - `daily` 与 `sandbox` 的**功能差异是否存在**未确认（源码称「未获实验支持」）。
//     本包按轮换顺序发两端点，但不声称换端点能解决 quota。
package gemini

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// usageSnapshot 一次流里被采纳的那一帧 usage（取 totalTokenCount 最大者）。
type usageSnapshot struct {
	seen bool

	total    int64
	totalSet bool

	prompt        int64
	completion    int64
	cached        int64
	thoughts      int64
	promptSet     bool
	completionSet bool
	cachedSet     bool
	thoughtsSet   bool
}

// geminiStreamReader 逐行读上游 SSE，吐出 OpenAI Chat SSE。
type geminiStreamReader struct {
	src   *bufio.Reader
	model string

	pending []byte

	id        string
	created   int64
	sentFirst bool

	// sawContent 是否出现过任何内容（文本 / 思考 / 工具调用）——空响应判据。
	sawContent bool
	// finishReason 上游 candidates[0].finishReason 原值。
	finishReason string
	// toolCount 本次流里出现过的工具调用数（finish_reason 归 tool_calls 的判据）。
	toolCount int

	usage usageSnapshot

	// finished 已吐出收尾帧（含 `data: [DONE]`）；后续 Read 直接 EOF。
	finished bool
	// abnormal 传输层中断（连接被掐 / 空闲看门狗判死）。
	abnormal error
	// err 终止性错误：后续 Read 一律返回它（**不补 [DONE]**，R36）。
	err error
}

// newGeminiStream 包装上游响应体；model 为客户端请求的原始模型名（含渠道前缀），
// 用于回填响应里的 model 字段（R14：上游返回的是裸名/不返回）。
func newGeminiStream(r io.Reader, model string) io.Reader {
	return &geminiStreamReader{
		src:     bufio.NewReaderSize(r, 64*1024),
		model:   model,
		created: time.Now().Unix(),
		id:      "chatcmpl-gemini-" + randomHexFn(8),
	}
}

// Read 以行为单位吐出转换结果。
func (s *geminiStreamReader) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		if s.err != nil {
			// 已判定失败：把错误上抛（**不补 [DONE]**，R36）。
			return 0, s.err
		}
		if s.finished {
			return 0, io.EOF
		}
		line, err := s.src.ReadBytes('\n')
		if len(line) > 0 {
			// ⚠️ **先处理这一行，再判 err**：被截断的流里最后一行（往往正是承载
			// finishReason / usageMetadata 的那一帧）**没有换行符**，bufio.ReadBytes
			// 会把它连同 io.EOF 一起交出来；先判 err 就 return 会整条丢掉它。
			s.pending = s.convertLine(line)
		}
		if err != nil {
			if err != io.EOF && s.abnormal == nil {
				s.abnormal = err
			}
			break // 上游已结束（正常或异常）：不再向它读
		}
	}
	if len(s.pending) > 0 {
		return s.drain(p)
	}
	if s.err == nil && !s.finished {
		s.err = s.finalize() // 可能写入收尾帧，或给出必须上抛的错误
	}
	if len(s.pending) > 0 {
		return s.drain(p)
	}
	if s.err != nil {
		return 0, s.err
	}
	return 0, io.EOF
}

// drain 把待吐内容拷进 p。
func (s *geminiStreamReader) drain(p []byte) (int, error) {
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// finalize 在「上游流已结束」时决定下一步：补收尾帧，或给出必须上抛的错误。
//
// 两种必须报错的形态（都**不补 [DONE]**，R36：截断不得伪装正常收尾）：
//   - 传输层中断（连接被掐 / 空闲看门狗判死）→ 流被从中间砍断；
//   - 一个内容块都没出现过 → 空响应（协议 §2.5 末条）。
//
// 正常 EOF（帧完整、只是上游没发 `[DONE]`）不算截断：兜底补收尾帧即可。
func (s *geminiStreamReader) finalize() error {
	if s.abnormal != nil {
		return fmt.Errorf("gemini：上游流读取中断（%v）", s.abnormal)
	}
	if !s.sawContent {
		return fmt.Errorf("gemini：模型未返回任何内容块")
	}
	s.pending = s.finish()
	return nil
}

// convertLine 处理单行上游 SSE，返回要下发的 OpenAI SSE 文本（可能为空）。
func (s *geminiStreamReader) convertLine(line []byte) []byte {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	// `:` 开头是注释/心跳；`event:` 无意义（上游无 event 名）。两者都必须跳过。
	if trimmed[0] == ':' || bytes.HasPrefix(trimmed, []byte("event:")) {
		return nil
	}
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil
	}
	payload := bytes.TrimSpace(trimmed[len("data:"):])
	if len(payload) == 0 {
		return nil
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		if !s.sawContent {
			// 空响应：连 [DONE] 也不能掩盖（协议 §2.5）。
			s.err = fmt.Errorf("gemini：模型未返回任何内容块")
			return nil
		}
		return s.finish()
	}
	var frame map[string]any
	if err := json.Unmarshal(payload, &frame); err != nil {
		// 非 JSON 帧忽略（流内 error 帧形态未确认，不猜；见文件头）。
		return nil
	}
	// ⚠️ 先试信封形态 `{"response":{…}}`，再试裸 Response（原版同序）。
	inner := frame
	if resp, ok := frame["response"].(map[string]any); ok {
		inner = resp
	}
	s.noteUsage(inner["usageMetadata"])

	candidates, _ := inner["candidates"].([]any)
	if len(candidates) == 0 {
		// 纯 usageMetadata 的收尾帧：只记用量，**不算内容**，继续读。
		return nil
	}
	cand, _ := candidates[0].(map[string]any)
	if cand == nil {
		return nil
	}
	if fr := strOf(cand["finishReason"]); fr != "" {
		s.finishReason = fr
	}
	content, _ := cand["content"].(map[string]any)
	parts, _ := content["parts"].([]any)

	var out []byte
	for _, raw := range parts {
		pm, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fc, ok := pm["functionCall"].(map[string]any); ok {
			name := strOf(fc["name"])
			if name == "" {
				continue
			}
			// 上游的 functionCall 是**一次给全**的（不是增量），故一次性下发完整 arguments。
			args := alphaJSON("{}")
			if fc["args"] != nil {
				if canon, err := canonicalRaw(fc["args"]); err == nil {
					args = canon
				}
			}
			s.sawContent = true
			out = append(out, s.toolCallChunks(name, string(args))...)
			continue
		}
		text := strOf(pm["text"])
		if text == "" {
			continue
		}
		s.sawContent = true
		if thought, ok := pm["thought"].(bool); ok && thought {
			// ⚠️ 思考分片必须走 reasoning_content，**不得混进正文**（协议 §2.5）。
			out = append(out, s.chunks(map[string]any{"reasoning_content": text})...)
			continue
		}
		out = append(out, s.chunks(map[string]any{"content": text})...)
	}
	return out
}

// noteUsage 吸收一帧 usageMetadata（取 totalTokenCount 最大的一份）。
func (s *geminiStreamReader) noteUsage(raw any) {
	m, ok := raw.(map[string]any)
	if !ok {
		return
	}
	total := int64(-1)
	if v, has := numOf(m, "totalTokenCount"); has {
		total = v
	}
	if s.usage.seen && total < s.usage.total {
		return
	}
	s.usage = usageSnapshot{seen: true, total: total}
	if v, has := numOf(m, "promptTokenCount"); has {
		s.usage.prompt, s.usage.promptSet = v, true
	}
	if v, has := numOf(m, "candidatesTokenCount"); has {
		s.usage.completion, s.usage.completionSet = v, true
	}
	if v, has := numOf(m, "cachedContentTokenCount"); has {
		s.usage.cached, s.usage.cachedSet = v, true
	}
	if v, has := numOf(m, "thoughtsTokenCount"); has {
		s.usage.thoughts, s.usage.thoughtsSet = v, true
	}
	if total >= 0 {
		s.usage.totalSet = true
	}
}

// usageMap 生成 OpenAI 形状的 usage。
//
// 口径（与 AGENTS.md 不变量 24 对齐）：
//   - `promptTokenCount` 是**含缓存**的总输入，故直接作为 `prompt_tokens`：
//     于是「命中数是 prompt_tokens 的子集」这条跨渠道不变量成立（利用率恒 ∈ [0,1]），
//     gateway 回映射成 Anthropic 形状时会做减法还原（internal/gateway/anthropicUsage）；
//   - 缓存字段**只在裸字段存在时**上报：字段缺失 = 上游不提供该维度（面板显示「—」）；
//     字段存在且为 0 = 上游明确的「本轮未命中」（显示 0%）。绝不把「不提供」显示成 0%；
//   - `thoughtsTokenCount` 是 output 的**子集**，映射成 reasoning_tokens，**不**累加；
//   - 上游一帧 usage 都没有 ⇒ 返回 ok=false（不伪造 0）。
func (s *geminiStreamReader) usageMap() (map[string]any, bool) {
	if !s.usage.seen {
		return nil, false
	}
	pt := s.usage.prompt
	ct := s.usage.completion
	total := pt + ct
	if s.usage.totalSet {
		total = s.usage.total
	}
	u := map[string]any{
		"prompt_tokens":     pt,
		"completion_tokens": ct,
		"total_tokens":      total,
	}
	if s.usage.cachedSet {
		u["prompt_tokens_details"] = map[string]any{"cached_tokens": s.usage.cached}
	}
	if s.usage.thoughtsSet {
		u["completion_tokens_details"] = map[string]any{"reasoning_tokens": s.usage.thoughts}
	}
	return u, true
}

// chunks 下发一个 delta 帧；首个内容帧前先补一个 role=assistant 的空帧
// （OpenAI 客户端依赖它建立消息）。
func (s *geminiStreamReader) chunks(delta map[string]any) []byte {
	var out []byte
	if !s.sentFirst {
		s.sentFirst = true
		out = append(out, s.frame(map[string]any{
			"id":      s.id,
			"object":  "chat.completion.chunk",
			"created": s.created,
			"model":   s.model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil,
			}},
		})...)
	}
	out = append(out, s.frame(map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index": 0, "delta": delta, "finish_reason": nil,
		}},
	})...)
	return out
}

// toolCallChunks 下发一个工具调用（独占一个块，参数一次性给全）。
func (s *geminiStreamReader) toolCallChunks(name, args string) []byte {
	idx := s.toolCount
	s.toolCount++
	// 上游不提供工具调用 id，本地生成稳定可读的形式（与参考实现同构）。
	id := "gemini_tool_" + strconv.FormatInt(nowMillisFn(), 36) + "_" + strconv.Itoa(idx)
	return s.chunks(map[string]any{"tool_calls": []any{map[string]any{
		"index": idx,
		"id":    id,
		"type":  "function",
		"function": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}}})
}

// finish 生成收尾帧（finish_reason + usage）并附上 `data: [DONE]`。幂等：重复调用返回空。
func (s *geminiStreamReader) finish() []byte {
	if s.finished {
		return nil
	}
	s.finished = true
	frame := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": s.created,
		"model":   s.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": s.openAIFinish(),
		}},
	}
	if u, ok := s.usageMap(); ok {
		frame["usage"] = u
	}
	return append(s.frame(frame), []byte("data: [DONE]\n\n")...)
}

// frame 把对象编成 `data: <json>\n\n` 帧。
func (s *geminiStreamReader) frame(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return append(append([]byte("data: "), b...), '\n', '\n')
}

// openAIFinish 把上游 finishReason 映射成 OpenAI 的 finish_reason（协议 §2.5）。
//
//	有工具调用 → tool_calls；MAX_TOKENS → length；其余（STOP / STOP_SEQUENCE /
//	FINISH_REASON_UNSPECIFIED / 未知 / 缺失）→ stop。
//
// ⚠️ 未知一律 stop，**不**编造成 error：那会让 harness 重试一个其实成功的响应。
func (s *geminiStreamReader) openAIFinish() string {
	if s.toolCount > 0 {
		return "tool_calls"
	}
	if strings.EqualFold(strings.TrimSpace(s.finishReason), "MAX_TOKENS") {
		return "length"
	}
	return "stop"
}

// ---------------------------------------------------------------------------
// 取值小工具
// ---------------------------------------------------------------------------

// strOf 取字符串（非字符串/缺失 → 空串）。
func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// numOf 取数值字段；**键存在且是有限数值**才算命中（负数钳 0 但仍算「已上报」，
// 与 internal/provider.numInt 同款语义：坏数据不该让整行退化成「—」）。
func numOf(m map[string]any, key string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0, true
		}
		return int64(n), true
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		if f < 0 {
			return 0, true
		}
		return int64(f), true
	case int:
		if n < 0 {
			return 0, true
		}
		return int64(n), true
	case int64:
		if n < 0 {
			return 0, true
		}
		return n, true
	}
	return 0, false
}
