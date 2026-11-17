package chatgpt

import (
	"strings"
	"testing"
)

// 引用标记必须**增量**剥离：跨帧到达时不能漏半截，也不能把正文扣到流末。

// TestCitationStrippedAcrossFrames 标记被切成两帧时，正文照常放出、标记不出现。
func TestCitationStrippedAcrossFrames(t *testing.T) {
	var f citationFilter
	var out strings.Builder
	out.WriteString(f.Write("答案 <Ci"))
	out.WriteString(f.Write("te refs={[\"turn422074search0\"]}/> 尾巴"))
	out.WriteString(f.Flush())
	if got := out.String(); got != "答案  尾巴" {
		t.Fatalf("输出 = %q（标记未剥干净或正文被吞）", got)
	}
}

// TestCitationPartialTailHeld 可能是标记开头的尾部被扣住，不提前漏给调用方。
func TestCitationPartialTailHeld(t *testing.T) {
	var f citationFilter
	if got := f.Write("abc <Cite"); got != "abc " {
		t.Fatalf("首帧输出 = %q，期望扣住 \"<Cite\"", got)
	}
	if got := f.Write(" refs={[\"x\"]}/>"); got != "" {
		t.Fatalf("次帧输出 = %q，期望空（标记整体剥掉）", got)
	}
}

// TestCitationFlushDropsResidue 流结束时残余若是残标记则丢弃，不得污染正文。
func TestCitationFlushDropsResidue(t *testing.T) {
	var f citationFilter
	if got := f.Write("abc <Cite ref"); got != "abc " {
		t.Fatalf("输出 = %q", got)
	}
	if got := f.Flush(); got != "" {
		t.Fatalf("Flush = %q，期望丢弃残标记", got)
	}
	var g citationFilter
	if got := g.Write("abc <"); got != "abc " {
		t.Fatalf("输出 = %q", got)
	}
	if got := g.Flush(); got != "" {
		t.Fatalf("Flush = %q，期望丢弃孤立 \"<\"", got)
	}
}

// TestCitationMultipleMarkers 多个标记与夹在中间的正文都正确处理。
func TestCitationMultipleMarkers(t *testing.T) {
	var f citationFilter
	got := f.Write(`一<Cite refs={["a"]}/>二<Cite refs={["b"]}/>三`)
	if got != "一二三" {
		t.Fatalf("输出 = %q", got)
	}
	if got := f.Flush(); got != "" {
		t.Fatalf("Flush = %q", got)
	}
}

// TestCitationUnclosedNotStuckForever 超长未闭合标记不得把正文永久扣死。
func TestCitationUnclosedNotStuckForever(t *testing.T) {
	var f citationFilter
	body := strings.Repeat("x", citeMaxLen+10)
	got := f.Write(citeMarker + body)
	if len(got) < len(body) {
		t.Fatalf("超长未闭合标记把正文扣住了：放出 %d 字节，期望 ≥ %d", len(got), len(body))
	}
}

// TestCitationPlainTextPassthrough 无标记时零改动（不 TrimSpace、不改空白）。
func TestCitationPlainTextPassthrough(t *testing.T) {
	var f citationFilter
	if got := f.Write("  keep  spaces  "); got != "  keep  spaces  " {
		t.Fatalf("输出 = %q", got)
	}
	if got := f.Flush(); got != "" {
		t.Fatalf("Flush = %q", got)
	}
}