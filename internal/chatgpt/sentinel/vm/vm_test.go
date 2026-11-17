package vm

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"testing"
)

// 本文件锁死纯 Go VM 的三颗牙齿（都是「静默算错」而不是「报错」的形态）：
//  1. 寄存器是 JS Number，浮点别名（3.5）与整数 opcode（3）是**两个不同寄存器**；
//  2. undefined 与 null/"" 必须区分（""+undefined="undefined"、Number(undefined)=NaN、Number("")=0）；
//  3. 未知 opcode 必须被识别出来（Analyze 报得出、执行时不静默跳过）。

// encodeDX 构造一份真实的 dx：base64(xor(JSON 指令流, key))。
// 与生产链路的解码（xorStr(atob(dx), key)）严格互逆。
func encodeDX(t *testing.T, prog any, key string) string {
	t.Helper()
	raw, err := json.Marshal(prog)
	if err != nil {
		t.Fatalf("marshal prog: %v", err)
	}
	return base64.StdEncoding.EncodeToString([]byte(xorStr(string(raw), key)))
}

// TestSolveReturnsExitToken 最简程序：op3（成功出口）带一个字面值。
func TestSolveReturnsExitToken(t *testing.T) {
	dx := encodeDX(t, []any{[]any{float64(3), "hello"}}, "key")
	tok, steps, err := Solve(dx, "key", 500, nil)
	if err != nil {
		t.Fatalf("Solve: %v", err)
	}
	if tok != b64("hello") {
		t.Fatalf("token = %q，期望 %q", tok, b64("hello"))
	}
	if steps != 1 {
		t.Errorf("steps = %d，期望 1", steps)
	}
}

// TestFloatAliasRegisterNotTruncated 是 VM 结构的关键判据。
//
// 程序做两件事：
//
//	op8 把出口 4（失败出口）复制到浮点寄存器 3.5；
//	op7 用 3.5 当 opcode 调用它。
//
// 若实现用 `switch int(op)` 或 `map[int]any`，3.5 会被截断成 3（**成功**出口），
// 于是本程序会「算出一个 token」——这正是参考实现警告的静默错误。
// 正确实现必须调到失败出口，Solve 返回错误。
func TestFloatAliasRegisterNotTruncated(t *testing.T) {
	prog := []any{
		[]any{float64(8), 3.5, float64(4)}, // Q[3.5] = Q[4]（失败出口）
		[]any{float64(7), 3.5},             // Q.get(3.5)()
	}
	dx := encodeDX(t, prog, "k")
	tok, _, err := Solve(dx, "k", 500, nil)
	if err == nil {
		t.Fatalf("浮点别名被截断成整数了：调到了成功出口并返回 token=%q", tok)
	}
}

// TestRegisterLookupByFloatKey 直接验证寄存器表以 JS Number 为键。
func TestRegisterLookupByFloatKey(t *testing.T) {
	v := &VM{Q: map[float64]any{}}
	v.set(3, func(a []any) any { return "three" })
	v.set(3.5, func(a []any) any { return "three-five" })
	if got := v.Q[3].(func(...any) any)(); got != "three" {
		t.Errorf("Q[3] = %v", got)
	}
	if got := v.Q[3.5].(func(...any) any)(); got != "three-five" {
		t.Errorf("Q[3.5] = %v（浮点别名被覆盖了）", got)
	}
}

// TestUndefinedIsNotNilOrEmpty 锁死 JS 语义：undefined / null / "" 三者互不相等，
// 且空串参与算术得 0（而不是 NaN），undefined 参与算术得 NaN。
func TestUndefinedIsNotNilOrEmpty(t *testing.T) {
	if jsStr(undef) != "undefined" {
		t.Errorf(`""+undefined = %q，期望 "undefined"`, jsStr(undef))
	}
	if jsStr(nil) != "null" {
		t.Errorf(`""+null = %q，期望 "null"`, jsStr(nil))
	}
	if jsStr("") != "" {
		t.Errorf(`""+"" = %q，期望空串`, jsStr(""))
	}
	if !math.IsNaN(jsNum(undef)) {
		t.Errorf("Number(undefined) = %v，期望 NaN", jsNum(undef))
	}
	if jsNum(nil) != 0 {
		t.Errorf("Number(null) = %v，期望 0", jsNum(nil))
	}
	if jsNum("") != 0 || jsNum("   ") != 0 {
		t.Errorf(`Number("")/Number("  ") = %v/%v，期望 0`, jsNum(""), jsNum("  "))
	}
	if got := jsAdd("", 22); got != "22" {
		t.Errorf(`"" + 22 = %v，期望 "22"`, got)
	}
	if got := jsAdd(undef, 22); !math.IsNaN(jsNum(got)) {
		t.Errorf("undefined + 22 = %v，期望 NaN", got)
	}
	if jsStrictEq(undef, "") {
		t.Error("undefined === \"\" 不得为真")
	}
	if !jsStrictEq(undef, undef) {
		t.Error("undefined === undefined 必须为真")
	}
	if reg0 := (&VM{Q: map[float64]any{}}).reg(77); !isUndef(reg0) {
		t.Errorf("未赋值寄存器 = %v，期望 undefined", reg0)
	}
}

// TestUnknownOpcodeIsIdentified Analyze 必须把不认识的 opcode 原样报出来（供 fail-closed 门禁）。
func TestUnknownOpcodeIsIdentified(t *testing.T) {
	prog := []any{
		[]any{float64(999), float64(0)},
		[]any{3.5, float64(0)}, // 浮点别名（不是 opcode，但 Analyze 应登记为 alias）
		[]any{float64(3), "ok"},
	}
	dx := encodeDX(t, prog, "k")
	n, direct, alias, err := Analyze(dx, "k")
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if n != 3 {
		t.Errorf("指令数 = %d，期望 3", n)
	}
	if direct[999] != 1 {
		t.Errorf("direct[999] = %d，期望 1", direct[999])
	}
	if Supported(999) {
		t.Error("未知 opcode 999 不得被当作受支持")
	}
	if Supported(31) || Supported(32) {
		t.Error("31/32 是参考实现未实现的 opcode，必须报不可支持")
	}
	if !Supported(3) || !Supported(33) || !Supported(35) {
		t.Error("已实现的 opcode 必须报受支持")
	}
	if len(alias) != 1 || alias[0] != 3.5 {
		t.Errorf("alias = %v，期望 [3.5]", alias)
	}
}

var errShapeRe = regexp.MustCompile(`^\d+: `)

// TestUnknownOpcodeNotSilentlySkipped 执行未知 opcode 时不得「跳过继续跑」并给出干净 token。
// 真实 VM 会把它变成一条 base64 的 "N: TypeError…" 错误串（由 validateTurnstileToken 拦下）。
func TestUnknownOpcodeNotSilentlySkipped(t *testing.T) {
	dx := encodeDX(t, []any{[]any{float64(999), float64(0)}}, "k")
	tok, _, err := Solve(dx, "k", 500, nil)
	if err != nil {
		return // 报错也是可识别的形态
	}
	raw, decErr := base64.StdEncoding.DecodeString(tok)
	if decErr != nil || !errShapeRe.Match(raw) {
		t.Fatalf("未知 opcode 被静默跳过：token=%q raw=%q", tok, raw)
	}
}

// TestHostEnvExposesChallengeKeys 浏览器环境桩必须齐备——挑战程序会读 navigator/screen/document。
func TestHostEnvExposesChallengeKeys(t *testing.T) {
	env := DefaultEnv()
	w, ok := NewWindow(env).(map[string]any)
	if !ok {
		t.Fatal("NewWindow 未返回对象")
	}
	nav, ok := w["navigator"].(map[string]any)
	if !ok {
		t.Fatal("缺 window.navigator")
	}
	if nav["userAgent"] != env.UserAgent {
		t.Errorf("navigator.userAgent = %v", nav["userAgent"])
	}
	if !isUndef(nav["webdriver"]) {
		t.Errorf("navigator.webdriver = %v，真页面上是 undefined（自动化痕迹）", nav["webdriver"])
	}
	if !isUndef(nav["doNotTrack"]) {
		t.Errorf("navigator.doNotTrack = %v，期望 undefined", nav["doNotTrack"])
	}
	for _, k := range []string{"appCodeName", "product", "vendor", "language", "languages",
		"hardwareConcurrency", "deviceMemory", "cookieEnabled", "onLine", "pdfViewerEnabled", "storage"} {
		if _, ok := nav[k]; !ok {
			t.Errorf("navigator 缺键 %s", k)
		}
	}
	scr, ok := w["screen"].(map[string]any)
	if !ok {
		t.Fatal("缺 window.screen")
	}
	if scr["width"] != float64(env.ScreenW) || scr["height"] != float64(env.ScreenH) {
		t.Errorf("screen 尺寸 = %vx%v，期望 %dx%d", scr["width"], scr["height"], env.ScreenW, env.ScreenH)
	}
	doc, ok := w["document"].(map[string]any)
	if !ok {
		t.Fatal("缺 window.document")
	}
	if _, ok := doc["scripts"].([]any); !ok {
		t.Error("document.scripts 缺失（挑战程序按正则找 sdk.js 版本号）")
	}
	if _, ok := doc["cookie"]; !ok {
		t.Error("document.cookie 缺失")
	}
	if _, ok := doc["visibilityState"]; !ok {
		t.Error("document.visibilityState 缺失")
	}
	if _, ok := w["performance"].(map[string]any)["memory"]; !ok {
		t.Error("performance.memory 缺失")
	}
	for _, k := range []string{"localStorage", "sessionStorage", "location", "history",
		"crypto", "Date", "Math", "JSON", "innerWidth", "devicePixelRatio", "btoa", "atob"} {
		if _, ok := w[k]; !ok {
			t.Errorf("window 缺键 %s", k)
		}
	}
}

// TestScriptsOfReadsWindow 挑战程序用 op11 正则匹配 document.scripts 里的 sdk 地址。
func TestScriptsOfReadsWindow(t *testing.T) {
	w := NewWindow(DefaultEnv())
	got := scriptsOf(w)
	if len(got) == 0 || !strings.Contains(got[0], "sdk.js") {
		t.Fatalf("scriptsOf = %v，期望含 sdk.js", got)
	}
	if scriptsOf(nil) != nil {
		t.Error("nil window 应返回 nil")
	}
}
