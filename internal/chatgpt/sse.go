package chatgpt

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// 上游对话流的解析（登录链路 /backend-api/f/conversation）。
//
// 上游形状（2026-10 抓包，同源 ww3 internal/cgpt/sse.go）：
//   - 首帧是**完整消息**，之后是 JSON Patch 增量；
//   - 增量帧可能包成 {"o":"patch","v":[…]}，也可能是**裸的** {"v":[…]}
//     —— 只认前者会静默丢帧（ww3 第一版只拼出半句「Hello. What can」）；
//   - 正文只认对用户可见的 assistant 消息：隐藏消息（is_visually_hidden_from_conversation）、
//     非 final 通道（channel）、tool 通道（recipient）都不进正文；
//   - 思考内容走 content_type="thoughts"，与正文分开累积。
const (
	partsPtr    = "/message/content/parts/0"
	thoughtsPtr = "/message/content/thoughts"
)

// turnState 是一轮对话的解析状态（正文 / 思考 / 会话锚点）。
//
// 正文与思考都给**全量文本**（上游 patch 是「改写该字段」语义），由输出层做增量差分。
type turnState struct {
	ConvID string // 首帧非空者胜（后续帧重复携带同一值）
	MsgID  string // 最后一个可见 assistant 消息 id = 下一轮的 parent_message_id
	Text   string
	Reason string
}

// apply 应用一帧事件（完整消息 / 单条 patch / patch 数组）。
func (t *turnState) apply(ev map[string]any) {
	for _, cand := range []any{ev, ev["v"]} {
		cm, ok := cand.(map[string]any)
		if !ok {
			continue
		}
		if t.ConvID == "" {
			t.ConvID = asString(cm["conversation_id"])
		}
	}
	if t.ConvID == "" {
		t.ConvID = asString(ev["conversation_id"])
	}
	for _, cand := range []any{ev, ev["v"]} {
		cm, ok := cand.(map[string]any)
		if !ok {
			continue
		}
		if m, ok := cm["message"].(map[string]any); ok && visibleAssistant(m) {
			if id := asString(m["id"]); id != "" {
				t.MsgID = id
			}
		}
	}
	t.Text = applyPatchEvent(ev, t.Text)
	t.Reason = applyThoughts(ev, t.Reason)
}

// visibleAssistant 判定消息是否属于「用户能看到的 assistant 正文」。
//
// 四道过滤缺一不可：role、隐藏标记、recipient（tool/search 通道）、channel（非 final 通道）。
func visibleAssistant(m map[string]any) bool {
	author, _ := m["author"].(map[string]any)
	if asString(author["role"]) != "assistant" {
		return false
	}
	if b, ok := m["is_visually_hidden_from_conversation"].(bool); ok && b {
		return false
	}
	switch asString(m["recipient"]) {
	case "", "all":
	default:
		return false // web.run / python 等工具通道
	}
	switch asString(m["channel"]) {
	case "", "final":
	default:
		return false
	}
	return true
}

// partsText 取出可见正文消息的文本（只拼接 text 分片；图片等非文本分片不进正文）。
func partsText(m map[string]any) string {
	content, _ := m["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	var out strings.Builder
	for _, p := range parts {
		if s, ok := p.(string); ok {
			out.WriteString(s)
		}
	}
	return out.String()
}

// applyPatchEvent 把一帧事件作用到正文上，返回作用后的全量正文。
//
// 三形态都要认：完整消息（替换）、单条 patch（p/o/v）、patch 数组（带 o:"patch" 包裹的与裸的）。
func applyPatchEvent(ev map[string]any, text string) string {
	for _, cand := range []any{ev, ev["v"]} {
		cm, ok := cand.(map[string]any)
		if !ok {
			continue
		}
		m, ok := cm["message"].(map[string]any)
		if !ok || !visibleAssistant(m) {
			continue
		}
		return partsText(m) // 完整消息 = 替换（不只是追加）
	}
	switch p := asString(ev["p"]); {
	case p == partsPtr:
		switch asString(ev["o"]) {
		case "append":
			return text + asString(ev["v"])
		case "replace":
			return asString(ev["v"])
		}
		return text
	case p != "":
		return text // 其它字段的 patch 与我们无关
	}
	if arr, ok := ev["v"].([]any); ok {
		for _, it := range arr {
			if im, ok := it.(map[string]any); ok {
				text = applyPatchEvent(im, text)
			}
		}
	}
	return text
}

// applyThoughts 把一帧事件作用到思考链上，返回作用后的全量思考文本。
func applyThoughts(ev map[string]any, cur string) string {
	if s, ok := fullThoughts(ev); ok {
		return s
	}
	if p := asString(ev["p"]); p == thoughtsPtr {
		if asString(ev["o"]) == "append" {
			return joinLine(cur, asString(ev["v"]))
		}
		return cur
	}
	if arr, ok := ev["v"].([]any); ok {
		for _, it := range arr {
			if im, ok := it.(map[string]any); ok {
				cur = applyThoughts(im, cur)
			}
		}
	}
	return cur
}

// fullThoughts 从「完整消息」里取思考正文（content_type="thoughts"）。
func fullThoughts(ev map[string]any) (string, bool) {
	for _, cand := range []any{ev, ev["v"]} {
		cm, ok := cand.(map[string]any)
		if !ok {
			continue
		}
		m, ok := cm["message"].(map[string]any)
		if !ok {
			continue
		}
		content, _ := m["content"].(map[string]any)
		if asString(content["content_type"]) != "thoughts" {
			continue
		}
		items, _ := content["thoughts"].([]any)
		var lines []string
		for _, it := range items {
			im, _ := it.(map[string]any)
			if s := asString(im["content"]); s != "" {
				lines = append(lines, s)
			}
		}
		return strings.Join(lines, "\n"), true
	}
	return "", false
}

// applyDelta 计算增量输出：返回本次可以下发的片段。
//
// 上游 patch 语义是「改写全量」，替换（非前缀扩展）时只能补发公共前缀之后的部分
// （已下发的字节无法收回）；文本变小则不补发。这是**推断**级取舍，见票 03 记录。
func applyDelta(prev, next string) (string, bool) {
	if next == prev {
		return "", false
	}
	if strings.HasPrefix(next, prev) {
		return next[len(prev):], true
	}
	common := commonPrefixLen(prev, next)
	if len(next) > common {
		return next[common:], true
	}
	return "", false
}

func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// scanSSE 逐帧读 SSE，把 `data:` 后的载荷交给 fn（裸串帧与 `[DONE]` 都照收）。
//
// 单一入站帧解析入口：`data: ` 与 `data:` 两种写法都必须认（LobsterAI 事故：
// 只认带空格的写法会把健康账号判成「空流」并整池冷却）。
func scanSSE(rd io.Reader, fn func(payload string) error) error {
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20) // 单帧上限 8MB（生图帧可能很大）
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		if payload == "" {
			continue
		}
		if err := fn(payload); err != nil {
			return err
		}
	}
	return sc.Err()
}

// asString 宽容取串（上游字段可能是 string / 数字 / nil）。
func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return jsonNumber(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return ""
}

// jsonNumber 把 JSON 数字转串（避免引入 strconv 的格式化差异）。
func jsonNumber(f float64) string {
	b, err := json.Marshal(f)
	if err != nil {
		return ""
	}
	return string(b)
}

// joinLine 用换行连接非空片段（思考链的分段追加）。
func joinLine(cur, add string) string {
	if add == "" {
		return cur
	}
	if cur == "" {
		return add
	}
	return cur + "\n" + add
}
