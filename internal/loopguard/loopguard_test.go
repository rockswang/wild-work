package loopguard

import (
	"fmt"
	"strings"
	"testing"
)

// feed 把多行文本逐小片喂入，模拟流式分片（分片边界故意切在字符中间）。
func feed(t *testing.T, g *Guard, text string, chunk int) *Hit {
	t.Helper()
	rs := []rune(text)
	var last *Hit
	for i := 0; i < len(rs); i += chunk {
		end := i + chunk
		if end > len(rs) {
			end = len(rs)
		}
		if hit := g.Push(string(rs[i:end])); hit != nil {
			last = hit
		}
	}
	return last
}

// 正常思考样例：各不相同的行，分片喂入，任何判据都不得触发。
func TestNormalReasoningDoesNotTrip(t *testing.T) {
	g := NewDefault()
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString(fmt.Sprintf("我在思考第 %d 步，内容各不相同。\n", i))
	}
	if hit := feed(t, g, b.String(), 3); hit != nil {
		t.Fatalf("正常推理误触发 %s: %s", hit.Kind, hit.Reason)
	}
}

// 同一长行连续 8 次必触发 repeat-line。
func TestRepeatLineTrips(t *testing.T) {
	g := NewDefault()
	line := "这里是一个足够长的、会被模型机械重复的思考行内容"
	text := strings.Repeat(line+"\n", 8)
	hit := feed(t, g, text, 5)
	if hit == nil || hit.Kind != RepeatLine {
		t.Fatalf("期望 repeat-line，得 %+v", hit)
	}
}

// 短行（<12 字符）连续 8 次不应触发 repeat-line（被 minLineChars 挡掉），
// 但同一短行在 60 行窗口内 ≥30 次应触发 short-line-repeat。
// 构造：短行「做。」与互不相同的正常长度行交错，短行窗口只收短行（30 个全是「做。」），
// 而 spinning 的全行窗口被 30 条正常行稀释，唯一率保持高位——不会先触发 spinning。
func TestShortLineRepeatTrips(t *testing.T) {
	g := NewDefault()
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("做。\n")
		b.WriteString(fmt.Sprintf("这是第 %d 条正常长度的思考内容，各不相同。\n", i))
	}
	hit := feed(t, g, b.String(), 2)
	if hit == nil || hit.Kind != ShortLineRepeat {
		t.Fatalf("期望 short-line-repeat，得 %+v", hit)
	}
}

// 窗口空转：40 行里只有 2 种内容 → 唯一率 0.05 ≤0.25 必触发 spinning。
func TestSpinningTrips(t *testing.T) {
	g := NewDefault()
	var b strings.Builder
	for i := 0; i < 40; i++ {
		if i%2 == 0 {
			b.WriteString("好的，我继续执行。\n")
		} else {
			b.WriteString("让我再检查一遍。\n")
		}
	}
	hit := feed(t, g, b.String(), 4)
	if hit == nil || hit.Kind != Spinning {
		t.Fatalf("期望 spinning，得 %+v", hit)
	}
}

// 行内周期复读：同一单元连续 16 次且累计 ≥160 字（单元含汉字）。
func TestInlineRepeatTrips(t *testing.T) {
	g := NewDefault()
	unit := "这是一段会被重复的思考内容"
	text := strings.Repeat(unit, 20) // 20 次 × 12 字 = 240 字，无换行
	hit := feed(t, g, text, 5)
	if hit == nil || hit.Kind != RepeatInline {
		t.Fatalf("期望 repeat-inline，得 %+v", hit)
	}
}

// 纯符号长分隔线（─×80）是 markdown 常态，不得触发行内复读。
func TestSymbolRunDoesNotTrip(t *testing.T) {
	g := NewDefault()
	text := strings.Repeat("─", 400) + "\n" + strings.Repeat("─", 400) + "\n"
	if hit := feed(t, g, text, 10); hit != nil {
		t.Fatalf("纯符号分隔线误触发 %s: %s", hit.Kind, hit.Reason)
	}
}

// 纯数字命令参数重复（如按键码 0x30,0x30,…）不得触发行内复读。
func TestHexByteCycleDoesNotTrip(t *testing.T) {
	g := NewDefault()
	text := strings.Repeat("0x30,", 60) // 60 次，长度足够，但单元是字节表示
	if hit := feed(t, g, text, 7); hit != nil {
		t.Fatalf("十六进制字节周期误触发 %s: %s", hit.Kind, hit.Reason)
	}
}

// 代码围栏内的行内复读不做中止判定。
func TestFenceExemptsInline(t *testing.T) {
	g := NewDefault()
	var b strings.Builder
	b.WriteString("```\n")
	b.WriteString(strings.Repeat("abcdefghij", 30)) // 围栏内的长周期复读
	b.WriteString("\n```\n")
	if hit := feed(t, g, b.String(), 4); hit != nil {
		t.Fatalf("代码围栏内误触发 %s: %s", hit.Kind, hit.Reason)
	}
}

// 长周期单元在流式分片下不能漏检：整段喂命中、逐小片喂也必须命中。
func TestInlineLongPeriodNotLostByChunking(t *testing.T) {
	unit := "这是一段二十一个字的思考内容啊"        // 21 字单元
	text := strings.Repeat(unit, 18) // 18 次 = 378 字 ≥160
	for _, chunk := range []int{3, 5, 7, 21} {
		g := NewDefault()
		if hit := feed(t, g, text, chunk); hit == nil || hit.Kind != RepeatInline {
			t.Fatalf("chunk=%d 漏检：%+v", chunk, hit)
		}
	}
}

// 全局开关关闭时一律不触发。
func TestDisabledNoTrip(t *testing.T) {
	SetEnabled(false)
	defer SetEnabled(true)
	g := NewDefault()
	text := strings.Repeat("这是一个会被重复的思考行内容\n", 8)
	if hit := feed(t, g, text, 4); hit != nil {
		t.Fatalf("禁用后仍触发 %s", hit.Kind)
	}
}

// 空串 / 纯空白输入安全。
func TestMalformedInputSafe(t *testing.T) {
	g := NewDefault()
	if hit := g.Push(""); hit != nil {
		t.Fatal("空串触发")
	}
	if hit := g.Push("\n\n   \n"); hit != nil {
		t.Fatal("纯空白触发")
	}
}

// 同行重复但行长不足 minLineChars 不应触发 repeat-line（如 "}" ×8）。
func TestShortLineNotCountedAsRepeatLine(t *testing.T) {
	g := NewDefault()
	text := strings.Repeat("}\n", 8)
	hit := feed(t, g, text, 1)
	if hit != nil && hit.Kind == RepeatLine {
		t.Fatalf("短行误触发 repeat-line: %s", hit.Reason)
	}
}

// 回归（2026-10-14 误伤现场）：模型在思维链里正常写 Go 代码，闭合块高频产生
// 「}」短行（60 行里 30+ 行），旧判据 ② 误判 short-line-repeat 把流揧断。
// 结构标点行不得进 ②③ 窗口。
func TestCodePunctShortLinesDoNotTrip(t *testing.T) {
	g := NewDefault()
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString(fmt.Sprintf("第 %d 步：把候选字段按优先级写入 details 归一化输出。\n", i))
		b.WriteString("\t\tif v, ok := usageNum(u, k); ok {\n")
		b.WriteString("\t\t\tputDetail(u, \"cached_tokens\", v)\n")
		b.WriteString("\t\t\tbreak\n")
		b.WriteString("\t\t}\n") // trim 后是 } —— 现场短行
		b.WriteString("\t}\n")   // trim 后是 }
	}
	if hit := feed(t, g, b.String(), 3); hit != nil {
		t.Fatalf("正常代码输出误触发 %s: %s", hit.Kind, hit.Reason)
	}
}

// 40 行窗口里 30+ 个 } 会把唯一率稀释到 ≤0.25，旧判据 ③ 会接力误伤同一段代码。
// 标点行排除后窗口不充盈，不判定。
func TestSpinningIgnoresPunctLines(t *testing.T) {
	g := NewDefault()
	var b strings.Builder
	for i := 0; i < 35; i++ {
		b.WriteString("}\n")
	}
	for i := 0; i < 5; i++ {
		b.WriteString(fmt.Sprintf("独一的内容行 %d。\n", i))
	}
	if hit := feed(t, g, b.String(), 3); hit != nil {
		t.Fatalf("标点行稀释窗口误触发 spinning: %s", hit.Reason)
	}
}

// 豁免只针对标点行：含内容短行（「做。」）夹在大量 } 中间仍必须触发——
// 防止把标点行计入窗口分母挤掉真发癫（修过头的反向回归）。
func TestPunctDoesNotDiluteShortLineRepeat(t *testing.T) {
	g := NewDefault()
	var b strings.Builder
	for i := 0; i < 30; i++ {
		b.WriteString("做。\n")
		b.WriteString(fmt.Sprintf("这是第 %d 条正常长度的思考内容，各不相同。\n", i))
		b.WriteString("}\n")
	}
	hit := feed(t, g, b.String(), 2)
	if hit == nil || hit.Kind != ShortLineRepeat {
		t.Fatalf("期望 short-line-repeat，得 %+v", hit)
	}
}

// 含字母的短行高频仍触发（只有纯标点/纯数字豁免）。
func TestWordShortLinesStillTrip(t *testing.T) {
	g := NewDefault()
	var b strings.Builder
	for i := 0; i < 35; i++ {
		b.WriteString("do.\n") // 含字母 → 非标点行
	}
	hit := feed(t, g, b.String(), 2)
	if hit == nil || hit.Kind != ShortLineRepeat {
		t.Fatalf("期望 short-line-repeat，得 %+v", hit)
	}
}

// 回归（2026-10-14）：围栏豁免必须覆盖窗口判据 ②③，不能只豁免 ④。
// 同一段重复样板代码（20 连发 if err != nil { return nil, err }），在围栏内不触发、
// 围栏外触发 spinning——对照锁死豁免是围栏驱动的，不是把判据整个废掉。
func TestFenceExemptsWindowCriteria(t *testing.T) {
	var b strings.Builder
	b.WriteString("```go\n")
	for i := 0; i < 20; i++ {
		b.WriteString("if err != nil {\n\treturn nil, err\n}\n")
	}
	b.WriteString("```\n")
	if hit := feed(t, NewDefault(), b.String(), 3); hit != nil {
		t.Fatalf("围栏内重复样板误触发 %s: %s", hit.Kind, hit.Reason)
	}

	// 围栏外同文本：两个内容行反复交替 → spinning 必触发（`}` 已被标点豁免）
	var c strings.Builder
	for i := 0; i < 20; i++ {
		c.WriteString("if err != nil {\n\treturn nil, err\n}\n")
	}
	hit := feed(t, NewDefault(), c.String(), 3)
	if hit == nil || hit.Kind != Spinning {
		t.Fatalf("围栏外对照应触发 spinning，得 %+v", hit)
	}
}

// 无围栏裸代码：`}` 短行高频靠标点豁免兜底（fence 状态机管不到）。
func TestBareCodePunctLinesDoNotTrip(t *testing.T) {
	g := NewDefault()
	var b strings.Builder
	for i := 0; i < 35; i++ {
		b.WriteString("}\n")
	}
	if hit := feed(t, g, b.String(), 2); hit != nil {
		t.Fatalf("无围栏标点行误触发 %s: %s", hit.Kind, hit.Reason)
	}
}
