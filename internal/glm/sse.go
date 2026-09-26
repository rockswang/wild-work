// sse.go 智谱清言 SSE ↔ OpenAI SSE 双向转换。
//
// 上游格式（chatglm.cn/backend-api/assistant/stream，2026-09-26 逐帧实测）：
//
//	event: message
//	data: {"conversation_id":"...","status":"init"|"finish","parts":[{...}]}
//
// ⚠️ 核心语义（实测结论，勿凭直觉改）：
//
//	part.content[].text / .think 的含义**取决于 part.status**：
//
//	  part.status == "init"   → 该字段是**增量片段**（delta），直接透传即可
//	  part.status == "finish" → 该字段是**该 part 的完整全文**，不可重复发出
//
// 实测证据（问「从1数到5」）：
//
//	帧2  status=init   part.status=init   text="1"
//	帧3  status=init   part.status=init   text=","
//	帧4  status=init   part.status=init   text=" "
//	...                                   （每帧只有几个字符的片段）
//	帧12 status=init   part.status=finish text="1, 2, 3, 4, 5"   ← 完整全文
//	帧13 status=finish part.status=finish text="1, 2, 3, 4, 5"
//
// 即：init 帧片段按序拼接 == finish 帧全文。think 段同规则。
//
// 本实现据此：
//   - init 帧 → 直接作为 OpenAI delta 发出，并累加进「已发出」记录
//   - finish 帧 → 与「已发出」比对，仅在全文更长时**补发差额**（防漏兜底）
//
// 段落类型（part.content[].type）：
//
//	text              正文
//	think             思考过程 → OpenAI 的 reasoning_content
//	code              代码块（part 完成时补 ``` 收尾）
//	execution_output  执行输出（part 完成时才完整）
//	image             图片 → 转 Markdown 图片链接（part 完成时）
//	tool_result       工具/检索结果 → 归入 reasoning_content
//	quote_result      引用检索结果 → 归入 reasoning_content（part 完成时）
package glm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// streamFrame 上游单帧。
type streamFrame struct {
	ID             string     `json:"id"`
	ConversationID string     `json:"conversation_id"`
	Status         string     `json:"status"`
	Parts          []part     `json:"parts"`
	Meta           *frameMeta `json:"meta_data,omitempty"`
}

type frameMeta struct {
	ConversationID string `json:"conversation_id"`
}

// part 一个段落。同一段落在流中由 logic_id 稳定标识。
type part struct {
	ID       string        `json:"id"`
	LogicID  string        `json:"logic_id"`
	Role     string        `json:"role"`
	Content  []partContent `json:"content"`
	MetaData *partMeta     `json:"meta_data,omitempty"`
	// Status "init" = 内容为增量；"finish" = 内容为该段落全文
	Status string `json:"status"`
}

// partContent 段落内的一个内容项。
type partContent struct {
	Type    string      `json:"type"`
	Text    string      `json:"text"`
	Think   string      `json:"think"`
	Code    string      `json:"code"`
	Content string      `json:"content"`
	Image   []imageItem `json:"image"`
}

type imageItem struct {
	ImageURL string `json:"image_url"`
}

// partMeta 段落元数据（检索结果）。
type partMeta struct {
	ToolResultExtra *toolResultExtra `json:"tool_result_extra,omitempty"`
	MetadataList    []metadataItem   `json:"metadata_list,omitempty"`
}

type toolResultExtra struct {
	SearchResults []searchResult `json:"search_results"`
}

type searchResult struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type metadataItem struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// partKey 返回段落稳定标识，用于跨帧累加/比对。
// 优先 logic_id（上游对同一逻辑段落恒定），退回 id，再退回序号。
func partKey(p part, index int) string {
	if p.LogicID != "" {
		return p.LogicID
	}
	if p.ID != "" {
		return p.ID
	}
	return fmt.Sprintf("#%d", index)
}

// renderPart 按顺序渲染段落的可见正文与思考文本。
//
// 对 init 帧，返回值即本帧增量；对 finish 帧，返回值即该段落全文。
// 两种情况下渲染规则必须一致，否则「补差额」会算错。
func renderPart(p part) (text, think string) {
	var tb, rb strings.Builder
	finished := p.Status == "finish"
	for _, c := range p.Content {
		switch c.Type {
		case "text":
			tb.WriteString(c.Text)
		case "think":
			rb.WriteString(c.Think)
		case "code":
			tb.WriteString("```python\n" + c.Code)
			if finished {
				tb.WriteString("\n```\n")
			}
		case "execution_output":
			// 执行输出在段落完成时才完整
			if finished && c.Content != "" {
				tb.WriteString(c.Content + "\n")
			}
		case "image":
			if finished {
				for _, img := range c.Image {
					if strings.HasPrefix(img.ImageURL, "http://") || strings.HasPrefix(img.ImageURL, "https://") {
						tb.WriteString(fmt.Sprintf("![图像](%s)", img.ImageURL))
					}
				}
				tb.WriteString("\n")
			}
		case "tool_result":
			if p.MetaData != nil && p.MetaData.ToolResultExtra != nil {
				for _, sr := range p.MetaData.ToolResultExtra.SearchResults {
					rb.WriteString(fmt.Sprintf("> 检索 %s(%s) ...\n", sr.Title, sr.URL))
				}
			}
		case "quote_result":
			// 引用检索结果在段落完成时汇总，避免流式过程中重复列出
			if finished && p.MetaData != nil {
				for _, m := range p.MetaData.MetadataList {
					rb.WriteString(fmt.Sprintf("> 检索 %s(%s) ...\n", m.Title, m.URL))
				}
			}
		}
	}
	return tb.String(), rb.String()
}

// ---------------------------------------------------------------------------
// 帧解析
// ---------------------------------------------------------------------------

// parseSSE 逐帧解析上游 SSE，把每帧 data 交给 onFrame。
// 兼容 `event:` 行与多行 data（以空行分帧）。
func parseSSE(r io.Reader, onFrame func([]byte) error) error {
	br := bufio.NewReaderSize(r, 256*1024)
	var dataBuf strings.Builder

	flush := func() error {
		if dataBuf.Len() == 0 {
			return nil
		}
		payload := dataBuf.String()
		dataBuf.Reset()
		return onFrame([]byte(payload))
	}

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				// 空行 = 一帧结束
				if ferr := flush(); ferr != nil {
					return ferr
				}
			case strings.HasPrefix(trimmed, "data:"):
				v := strings.TrimPrefix(trimmed, "data:")
				v = strings.TrimPrefix(v, " ")
				if dataBuf.Len() > 0 {
					dataBuf.WriteString("\n")
				}
				dataBuf.WriteString(v)
			default:
				// event:/id:/retry: 等忽略
			}
		}
		if err != nil {
			if err == io.EOF {
				return flush()
			}
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// 流式转换：上游增量 SSE → OpenAI SSE
// ---------------------------------------------------------------------------

// streamConverter 维护「已发出的文本」，用于 finish 帧的差额补发兜底。
type streamConverter struct {
	model   string
	id      string
	created int64

	// sentText / sentThink 按段落记录已发出的文本（logic_id → 已发出内容）。
	sentText  map[string]string
	sentThink map[string]string

	firstChunk bool
	finishSent bool
	usage      map[string]any
}

func newStreamConverter(model string) *streamConverter {
	return &streamConverter{
		model:      model,
		created:    time.Now().Unix(),
		sentText:   map[string]string{},
		sentThink:  map[string]string{},
		firstChunk: true,
	}
}

// writeChunk 写一帧 OpenAI SSE。
func (sc *streamConverter) writeChunk(w io.Writer, delta map[string]any, finishReason any) error {
	chunk := map[string]any{
		"id":      sc.id,
		"object":  "chat.completion.chunk",
		"created": sc.created,
		"model":   sc.model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
	return err
}

// ensureRole 首次发内容前补一帧 role=assistant（OpenAI 客户端要求）。
func (sc *streamConverter) ensureRole(w io.Writer, flush func()) error {
	if !sc.firstChunk {
		return nil
	}
	sc.firstChunk = false
	if err := sc.writeChunk(w, map[string]any{"role": "assistant"}, nil); err != nil {
		return err
	}
	if flush != nil {
		flush()
	}
	return nil
}

// emitDelta 发送增量内容（init 帧路径）：直接透传，并累加已发出记录。
func (sc *streamConverter) emitDelta(w io.Writer, flush func(), key, text, think string) error {
	if text == "" && think == "" {
		return nil
	}
	if err := sc.ensureRole(w, flush); err != nil {
		return err
	}
	if think != "" {
		if err := sc.writeChunk(w, map[string]any{"reasoning_content": think}, nil); err != nil {
			return err
		}
		sc.sentThink[key] += think
		if flush != nil {
			flush()
		}
	}
	if text != "" {
		if err := sc.writeChunk(w, map[string]any{"content": text}, nil); err != nil {
			return err
		}
		sc.sentText[key] += text
		if flush != nil {
			flush()
		}
	}
	return nil
}

// emitDiff 发送全文与已发出内容的差额（finish 帧路径，防漏兜底）。
// 仅当全文确实以已发出内容为前缀且更长时才补发，避免结构变化导致错乱。
func (sc *streamConverter) emitDiff(w io.Writer, flush func(), key, text, think string) error {
	prevText := sc.sentText[key]
	prevThink := sc.sentThink[key]

	var dText, dThink string
	if len(text) > len(prevText) && strings.HasPrefix(text, prevText) {
		dText = text[len(prevText):]
	}
	if len(think) > len(prevThink) && strings.HasPrefix(think, prevThink) {
		dThink = think[len(prevThink):]
	}
	if dText == "" && dThink == "" {
		return nil
	}
	if err := sc.ensureRole(w, flush); err != nil {
		return err
	}
	if dThink != "" {
		if err := sc.writeChunk(w, map[string]any{"reasoning_content": dThink}, nil); err != nil {
			return err
		}
		sc.sentThink[key] = think
		if flush != nil {
			flush()
		}
	}
	if dText != "" {
		if err := sc.writeChunk(w, map[string]any{"content": dText}, nil); err != nil {
			return err
		}
		sc.sentText[key] = text
		if flush != nil {
			flush()
		}
	}
	return nil
}

// handleFrame 处理一帧。
func (sc *streamConverter) handleFrame(w io.Writer, flush func(), payload []byte) error {
	var f streamFrame
	if err := json.Unmarshal(payload, &f); err != nil {
		// 无法解析的帧跳过（不致命，上游偶发心跳/空帧）
		return nil
	}
	if sc.id == "" {
		switch {
		case f.ConversationID != "":
			sc.id = f.ConversationID
		case f.Meta != nil && f.Meta.ConversationID != "":
			sc.id = f.Meta.ConversationID
		default:
			sc.id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
		}
	}

	// 逐段落处理：init 帧发增量，finish 帧补差额
	for i, p := range f.Parts {
		key := partKey(p, i)
		text, think := renderPart(p)
		var err error
		if p.Status == "finish" {
			err = sc.emitDiff(w, flush, key, text, think)
		} else {
			err = sc.emitDelta(w, flush, key, text, think)
		}
		if err != nil {
			return err
		}
	}

	// 顶层 status == "finish" 表示整轮结束
	if f.Status == "finish" && !sc.finishSent {
		sc.finishSent = true
		if err := sc.ensureRole(w, flush); err != nil {
			return err
		}
		if err := sc.writeChunk(w, map[string]any{}, "stop"); err != nil {
			return err
		}
		if flush != nil {
			flush()
		}
	}
	return nil
}

// Stream 实现 provider.Upstream：上游增量 SSE → OpenAI SSE。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
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

	sc := newStreamConverter(model)
	parseErr := parseSSE(r, func(payload []byte) error {
		return sc.handleFrame(w, flush, payload)
	})

	// ⚠️ 收尾必须**无条件执行**，即使上游中断（parseErr != nil）。
	// 早期实现在此处直接 return，导致上游一中断就漏发 [DONE]，
	// 客户端会永远等待终止帧而挂住（见 hang_test.go
	// TestStreamHangsUpstreamDoesNotBlockClient）。
	if !sc.finishSent {
		sc.finishSent = true
		if werr := sc.ensureRole(w, flush); werr != nil && parseErr == nil {
			parseErr = werr
		}
		if werr := sc.writeChunk(w, map[string]any{}, "stop"); werr != nil && parseErr == nil {
			parseErr = werr
		}
	}
	if _, werr := io.WriteString(w, "data: [DONE]\n\n"); werr != nil && parseErr == nil {
		parseErr = werr
	}
	flush()
	return sc.usage, parseErr
}

// Aggregate 把上游 SSE 收敛成单个 OpenAI 响应（非流式客户端用）。
//
// 规则同 Stream：init 帧累加，finish 帧以全文为准覆盖。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	var (
		id     string
		order  []string
		texts  = map[string]string{}
		thinks = map[string]string{}
	)
	err := parseSSE(r, func(payload []byte) error {
		var f streamFrame
		if err := json.Unmarshal(payload, &f); err != nil {
			return nil
		}
		if id == "" {
			switch {
			case f.ConversationID != "":
				id = f.ConversationID
			case f.Meta != nil && f.Meta.ConversationID != "":
				id = f.Meta.ConversationID
			}
		}
		for i, p := range f.Parts {
			key := partKey(p, i)
			if _, seen := texts[key]; !seen {
				order = append(order, key)
			}
			text, think := renderPart(p)
			if p.Status == "finish" {
				// finish 帧是该段落权威全文，直接覆盖
				texts[key] = text
				thinks[key] = think
			} else {
				// init 帧是增量，累加
				texts[key] += text
				thinks[key] += think
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}

	var fullText, fullThink strings.Builder
	for _, key := range order {
		if t := texts[key]; t != "" {
			if fullText.Len() > 0 {
				fullText.WriteString("\n")
			}
			fullText.WriteString(t)
		}
		if t := thinks[key]; t != "" {
			if fullThink.Len() > 0 {
				fullThink.WriteString("\n")
			}
			fullThink.WriteString(t)
		}
	}

	message := map[string]any{"role": "assistant", "content": fullText.String()}
	if fullThink.Len() > 0 {
		message["reasoning_content"] = fullThink.String()
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": "stop",
		}},
	}, nil
}
