package chatgpt

import "strings"

// 引用标记的增量剥离。
//
// 上游在正文里内联 `<Cite refs={["turn422074search0"]}/>` 这类标记（搜索结果引用）。
// 这些标记是**流式到达**的：一次 chunk 可能只给到 `<Cite ref`，下一帧才补齐
// `s={…}/>`。直接转发会把半截标记漏给调用方；等整段剥干净再发又会让正文延迟到流末。
//
// 做法：写多少放多少，但把「可能是标记开头」的尾部**扣住**（pending），
// 等下一帧拼上再判；流结束（或截断）时 Flush 放出残余——残余若是残标记则丢弃，
// 绝不污染正文。
const (
	citeMarker = "<Cite refs="
	// citeMaxLen 是标记长度上限：超过就认定不是标记（真意见到的只有几十字节），
	// 避免一个「像标记开头」的正文把后面的内容永久扣住。
	citeMaxLen = 2048
)

// citationFilter 是内容流上的引用剥离器（每路内容一份）。
type citationFilter struct {
	pending string // 被扣住的尾部（可能是标记的开头或未闭合的标记）
}

// Write 追加一段增量，返回可以立即下发的文本（已剥离完整标记）。
func (f *citationFilter) Write(delta string) string {
	if delta == "" {
		return ""
	}
	buf := f.pending + delta
	f.pending = ""

	var out strings.Builder
	i := 0
	for i < len(buf) {
		if strings.HasPrefix(buf[i:], citeMarker) {
			if end := strings.Index(buf[i:], "/>"); end >= 0 {
				i += end + len("/>")
				continue
			}
			// 未闭合：超出上限就当普通文本处理，否则扣住等后续帧。
			if len(buf)-i > citeMaxLen {
				out.WriteString(buf[i:])
				i = len(buf)
				continue
			}
			f.pending = buf[i:]
			return out.String()
		}
		// 命中一个「可能是标记开头」的尾部：扣住。
		if n := partialMarkerLen(buf[i:]); n > 0 {
			cut := len(buf) - n
			out.WriteString(buf[i:cut])
			f.pending = buf[cut:]
			return out.String()
		}
		out.WriteByte(buf[i])
		i++
	}
	return out.String()
}

// Flush 放出残余：标记之前的正文照发，标记本身（含残缺）一律丢弃——
// 截断时最忌讳的就是把半截 `<Cite ref` 漏给调用方。
func (f *citationFilter) Flush() string {
	buf := f.pending
	f.pending = ""
	if buf == "" {
		return ""
	}
	if i := strings.Index(buf, citeMarker); i >= 0 {
		return buf[:i]
	}
	if n := partialMarkerLen(buf); n > 0 {
		return buf[:len(buf)-n]
	}
	return buf
}

// partialMarkerLen 返回 buf 开头与 citeMarker 相同前缀的长度（0 = 不是标记开头）。
//
// 尾部每一层都要扣：`<`、`<C`、`<Ci`… 整串 marker 由 Write 的完整分支处理，
// 故上限只到 len(citeMarker)-1。
func partialMarkerLen(buf string) int {
	n := len(buf)
	if n >= len(citeMarker) {
		n = len(citeMarker) - 1
	}
	for i := n; i > 0; i-- {
		if strings.HasPrefix(citeMarker, buf[:i]) {
			return i
		}
	}
	return 0
}