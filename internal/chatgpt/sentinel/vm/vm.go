package vm

// 纯 Go 重实现 chatgpt.com sentinel 挑战程序的 VM（与 vm.js 同构）。
//
// 结构上与 vm.js 逐条对齐：opcode 不是 switch 分支，而是 Q 里的函数值。
// vm.js 的 run() 是 `const [op,...args] = Q.get(9).shift(); Q.get(op)(...args)`，
// 即 **动态查表**——挑战程序可以先用 op8 把某个函数复制到浮点寄存器（如 Q[3.65]），
// 再拿别名当 opcode 调用，以此干扰静态分析。用 `switch int(op)` 实现会把它截断成 3，
// 撞上真正的成功出口寄存器，静默算出错误 token。这是实测真实 dx 才发现的。
//
// JS 语义对齐要点（差分模糊测试抓出来的，不是猜的）：
//   - undefined 与 null 必须区分：`""+undefined`="undefined"、`""+null`="null"、Number(undefined)=NaN、Number(null)=0
//   - Number("")=0、Number("  ")=0
//   - `+` 任一侧 ToPrimitive 是字符串才拼接，否则算术加（undefined+22 === NaN）
//   - `===` 严格相等（7 !== "7"），`<` 两侧都是字符串时按字典序
//   - 数组参与字符串拼接按 "," join，[null].toString() === ""
//   - btoa/atob 只认 Latin-1
//   - 未赋值的寄存器是 undefined 而不是 null

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// undefT 代表 JS 的 undefined（与 JSON null 的 nil 区分开）
type undefT struct{}

var undef = undefT{}

func isUndef(v any) bool { _, ok := v.(undefT); return ok }

type VM struct {
	Q        map[float64]any // 寄存器堆，键是 JS Number（3 与 3.65 是两个不同寄存器）
	steps    int
	done     bool // 对应 vm.js 的 done：已产出结果或已超时
	timedOut bool
	res      any
	err      any
	deadline time.Time
	scripts  []string           // document.scripts 的 src 列表（真实页面里由浏览器提供）
	Window   any                // 寄存器 10 的值：真实页面是 window，纯 Go 模式可传 map 或 nil
	trail    [][]any            // 最近 40 条指令（op + 参数），出错时打印定位
	names    map[float64]string // 寄存器 -> 真实 opcode 名（别名溯源，仅 trace 用）
}

// ---------------------------------------------------------------- JS 语义基础

func isNum(v any) bool { _, ok := v.(float64); return ok }

// jsStr 复刻 `"" + v`
func jsStr(v any) string {
	switch x := v.(type) {
	case undefT:
		return "undefined"
	case nil:
		return "null"
	case string:
		return x
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		if math.IsNaN(x) {
			return "NaN"
		}
		if math.IsInf(x, 1) {
			return "Infinity"
		}
		if math.IsInf(x, -1) {
			return "-Infinity"
		}
		if x == math.Trunc(x) && math.Abs(x) < 1e21 {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e == nil {
				parts[i] = "" // JS: [null].toString() === ""
				continue
			}
			parts[i] = jsStr(e)
		}
		return strings.Join(parts, ",")
	case map[string]any, map[float64]any:
		return "[object Object]"
	}
	return fmt.Sprint(v)
}

// jsNum 复刻 `Number(v)`
func jsNum(v any) float64 {
	switch x := v.(type) {
	case undefT:
		return math.NaN()
	case nil:
		return 0
	case float64:
		return x
	case bool:
		if x {
			return 1
		}
		return 0
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0 // Number("") === 0
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
		return math.NaN()
	case []any:
		return jsNum(jsStr(x))
	}
	return math.NaN()
}

// isStrish 判断 ToPrimitive 后是否为字符串（string/数组/对象/函数都是）
func isStrish(v any) bool {
	switch v.(type) {
	case string, []any, map[string]any, map[float64]any, func(...any) any:
		return true
	}
	return false
}

// jsAdd 复刻 `a + b`
func jsAdd(a, b any) any {
	if isStrish(a) || isStrish(b) {
		return jsStr(a) + jsStr(b)
	}
	return jsNum(a) + jsNum(b)
}

// jsStrictEq 复刻 `a === b`
func jsStrictEq(a, b any) bool {
	switch x := a.(type) {
	case undefT:
		return isUndef(b)
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case bool:
		y, ok := b.(bool)
		return ok && x == y
	case float64:
		y, ok := b.(float64)
		return ok && (x == y || (math.IsNaN(x) && math.IsNaN(y)))
	}
	// 函数/Map 等按指针比较：Go 的函数值不可比较，只认同一个 map 实例
	switch a.(type) {
	case func(...any) any:
		return false
	}
	return a == b
}

// jsLess 复刻 `a < b`（两侧都是字符串时按字典序）
func jsLess(a, b any) bool {
	as, aok := a.(string)
	bs, bok := b.(string)
	if aok && bok {
		return as < bs
	}
	return jsNum(a) < jsNum(b)
}

func xorStr(t, k string) string {
	if k == "" {
		return t
	}
	b := make([]byte, len(t))
	for i := 0; i < len(t); i++ {
		b[i] = t[i] ^ k[i%len(k)]
	}
	return string(b)
}

// b64 复刻 btoa：只接受 Latin-1，否则抛 InvalidCharacterError
func b64(s string) string {
	for _, r := range s {
		if r > 0xFF {
			panic("InvalidCharacterError: The string to be encoded contains characters outside of the Latin1 range.")
		}
	}
	b := make([]byte, 0, len(s))
	for _, r := range s {
		b = append(b, byte(r))
	}
	return base64.StdEncoding.EncodeToString(b)
}

// atob 复刻 atob：解出的字节按 Latin-1 逐字节映射回码点
func atob(s string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		panic("InvalidCharacterError: The string to be decoded is not correctly encoded.")
	}
	var sb strings.Builder
	for _, c := range raw {
		sb.WriteRune(rune(c))
	}
	return sb.String()
}

// qkey 把指令首个元素转成 Map 键（JS Map 的键是 Number，浮点别名必须保留）
func qkey(v any) (float64, bool) {
	if f, ok := v.(float64); ok {
		return f, true
	}
	return 0, false
}

// reg 取寄存器值；未赋值的寄存器在 JS 里是 undefined
func (v *VM) reg(i float64) any {
	if val, ok := v.Q[i]; ok {
		return val
	}
	return undef
}

// regArg 取第 i 个参数指向的寄存器值（参数是寄存器号）
func (v *VM) regArg(a []any, i int) any {
	if i >= len(a) {
		return undef
	}
	k, ok := qkey(a[i])
	if !ok {
		// 非数字当键：JS 里 Q.get("3") 取不到 Q[3]
		return undef
	}
	return v.reg(k)
}

func (v *VM) setArg(a []any, i int, val any) {
	if i >= len(a) {
		return
	}
	if k, ok := qkey(a[i]); ok {
		v.Q[k] = val
	}
}

// propGet 复刻 `obj[key]`（用于 op6/op24 访问 window/document 等宿主对象）
// traceOn 由环境变量 CHATGPT_VM_TRACE 打开，用于诊断挑战程序访问了哪些宿主属性。
// 重命名（原 WW3_VM_TRACE）是为了与 v2 的渠道前缀一致，行为不变。
var traceOn = os.Getenv("CHATGPT_VM_TRACE") != ""

func propGet(obj, key any) any {
	if traceOn {
		fmt.Printf("[get] %s . %s\n", typeName(obj), jsStr(key))
	}
	switch o := obj.(type) {
	case undefT, nil:
		panic("TypeError: Cannot read properties of " + jsStr(obj) + " (reading '" + jsStr(key) + "')")
	case map[string]any:
		if val, ok := o[jsStr(key)]; ok {
			return val
		}
		return undef
	case map[float64]any:
		if k, ok := qkey(key); ok {
			if val, ok := o[k]; ok {
				return val
			}
		}
		return undef
	case []any:
		if k, ok := qkey(key); ok {
			i := int(k)
			if i >= 0 && i < len(o) {
				return o[i]
			}
		}
		return undef
	case string:
		if k, ok := qkey(key); ok {
			i := int(k)
			if i >= 0 && i < len(o) {
				return string(o[i])
			}
			return undef
		}
		if name := jsStr(key); name == "length" {
			return float64(len([]rune(o)))
		}
		return strMethod(o, jsStr(key))
	}
	if fn := arrMethod(obj, jsStr(key)); fn != nil {
		return fn
	}
	return undef
}

// propSet 复刻 `obj[key] = val`（挑战程序会通过 Reflect.set 之类写宿主对象）
func typeName(v any) string {
	switch v.(type) {
	case undefT:
		return "undefined"
	case nil:
		return "null"
	case map[string]any:
		return "object"
	case map[float64]any:
		return "Qmap"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case func(...any) any:
		return "function"
	}
	return fmt.Sprintf("%T", v)
}

func propSet(obj, key, val any) bool {
	switch o := obj.(type) {
	case map[string]any:
		o[jsStr(key)] = val
		return true
	case map[float64]any:
		if k, ok := qkey(key); ok {
			o[k] = val
			return true
		}
	case []any:
		if k, ok := qkey(key); ok {
			i := int(k)
			if i >= 0 && i < len(o) {
				o[i] = val
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------- 生命周期

func (v *VM) finish(ok bool, val any) {
	if v.done {
		return
	}
	v.done = true
	if ok {
		v.res = b64(jsStr(val))
	} else {
		v.err = b64(jsStr(val))
	}
}

// reset 对应 vm.js 的 reset()：清空 Q 并注册 0-35 里除 3/4/9/16/30/31/32 之外的全部 opcode
func (v *VM) reset() {
	v.Q = map[float64]any{}
	v.set(0, func(a []any) any { // (dx) => exec(dx, undefined, 60000)：共享 Q 与 steps，不 reset
		savedDone, savedRes, savedErr, savedDL := v.done, v.res, v.err, v.deadline
		v.done, v.res, v.err = false, nil, nil
		v.exec0(jsStr(arg(a, 0)), undef, 60000, false)
		if v.err != nil && savedErr == nil {
			savedErr = v.err
		}
		if v.res != nil && savedRes == nil {
			savedRes = v.res
		}
		v.done, v.res, v.err, v.deadline = savedDone, savedRes, savedErr, savedDL
		return nil
	})
	v.set(1, func(a []any) any { // (e,t) => Q.set(e, xor(""+Q.get(e), ""+Q.get(t)))
		e := v.regArg(a, 0)
		v.setArg(a, 0, xorStr(jsStr(e), jsStr(v.regArg(a, 1))))
		return nil
	})
	v.set(2, func(a []any) any { // (e,t) => Q.set(e, t)   t 是字面值
		v.setArg(a, 0, arg(a, 1))
		return nil
	})
	v.set(5, func(a []any) any { // (e,t) => push 或 n + Q.get(t)
		cur := v.regArg(a, 0)
		if arr, isArr := cur.([]any); isArr {
			v.setArg(a, 0, append(arr, v.regArg(a, 1)))
		} else {
			v.setArg(a, 0, jsAdd(cur, v.regArg(a, 1)))
		}
		return nil
	})
	v.set(6, func(a []any) any { // (e,t,n) => Q.set(e, Q.get(t)[Q.get(n)])
		v.setArg(a, 0, propGet(v.regArg(a, 1), v.regArg(a, 2)))
		return nil
	})
	v.set(7, func(a []any) any { // (e,...t) => Q.get(e)(...t.map(x=>Q.get(x)))  结果不写回
		fn, isFn := v.regArg(a, 0).(func(...any) any)
		if !isFn {
			panic("TypeError: Q.get(" + jsStr(v.regArg(a, 0)) + ") is not a function @op7 reg=" + jsStr(arg(a, 0)))
		}
		args := make([]any, 0, len(a)-1)
		for i := 1; i < len(a); i++ {
			args = append(args, v.regArg(a, i))
		}
		fn(args...)
		return nil
	})
	v.set(8, func(a []any) any { // (e,t) => Q.set(e, Q.get(t))
		v.setArg(a, 0, v.regArg(a, 1))
		if v.names != nil { // 别名溯源：3.65 ← 8 ← 3
			if src, ok := qkey(arg(a, 1)); ok {
				if nm, ok := v.names[src]; ok {
					if dst, ok := qkey(arg(a, 0)); ok {
						v.names[dst] = nm
					}
				}
			}
		}
		return nil
	})
	v.Q[10] = v.Window            // 预置寄存器：window
	v.set(11, func(a []any) any { // document.scripts 里按正则找 src
		pat := jsStr(v.regArg(a, 1))
		var found any = nil
		// Go 的 regexp 不支持 JS 的 lookahead（如 (?=[?#]|$)），编译失败就去掉这些组再试
		re, err := regexp.Compile(pat)
		if err != nil {
			re, err = regexp.Compile(stripLookahead(pat))
		}
		if err == nil {
			for _, s := range v.scripts {
				if m := re.FindString(s); m != "" {
					found = m
					break
				}
			}
		} else if traceOn {
			fmt.Printf("  [op11] 正则无法编译: %v (pattern=%s)\n", err, pat)
		}
		v.setArg(a, 0, found)
		return nil
	})
	v.set(12, func(a []any) any { // (e) => Q.set(e, Q)
		v.setArg(a, 0, v.Q)
		return nil
	})
	v.set(13, func(a []any) any { // (e,t,...n) => try{ Q.get(t)(...n) }catch{ Q.set(e,""+err) }  n 是字面值
		fn, isFn := v.regArg(a, 1).(func(...any) any)
		if !isFn {
			v.setArg(a, 0, "TypeError: Q.get(...) is not a function")
			return nil
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					v.setArg(a, 0, ""+fmt.Sprint(r))
				}
			}()
			fn(a[2:]...)
		}()
		return nil
	})
	v.set(14, func(a []any) any { // JSON.parse
		var out any
		if err := json.Unmarshal([]byte(jsStr(v.regArg(a, 1))), &out); err != nil {
			panic("SyntaxError: " + err.Error())
		}
		v.setArg(a, 0, out)
		return nil
	})
	v.set(15, func(a []any) any { // JSON.stringify
		b, _ := json.Marshal(jsonable(v.regArg(a, 1)))
		v.setArg(a, 0, string(b))
		return nil
	})
	v.set(17, func(a []any) any { // (e,t,...n) => r=Q.get(t)(...n.map(x=>Q.get(x))); Q.set(e,r)
		fn, isFn := v.regArg(a, 1).(func(...any) any)
		if !isFn {
			v.setArg(a, 0, "TypeError: Q.get(...) is not a function")
			return nil
		}
		args := make([]any, 0, len(a)-2)
		for i := 2; i < len(a); i++ {
			args = append(args, v.regArg(a, i))
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					v.setArg(a, 0, ""+fmt.Sprint(r))
				}
			}()
			v.setArg(a, 0, fn(args...))
		}()
		return nil
	})
	v.set(18, func(a []any) any { v.setArg(a, 0, atob(jsStr(v.regArg(a, 0)))); return nil })
	v.set(19, func(a []any) any { v.setArg(a, 0, b64(jsStr(v.regArg(a, 0)))); return nil })
	v.set(20, func(a []any) any { // (e,t,n,...r) => 相等则 Q.get(n)(...r)
		if jsStrictEq(v.regArg(a, 0), v.regArg(a, 1)) {
			if fn, isFn := v.regArg(a, 2).(func(...any) any); isFn {
				fn(a[3:]...)
			}
		}
		return nil
	})
	v.set(21, func(a []any) any { // (e,t,n,r,...i) => |a-b|>c 则 Q.get(r)(...i)
		if math.Abs(jsNum(v.regArg(a, 0))-jsNum(v.regArg(a, 1))) > jsNum(v.regArg(a, 2)) {
			if fn, isFn := v.regArg(a, 3).(func(...any) any); isFn {
				fn(a[4:]...)
			}
		}
		return nil
	})
	v.set(22, func(a []any) any { // (e,t) => saved=[...Q.get(9)]; Q.set(9,[...t]); run().finally(restore)
		body, _ := arg(a, 1).([]any)
		saved, _ := v.Q[9].([]any)
		v.Q[9] = append([]any{}, body...)
		func() {
			defer func() {
				if r := recover(); r != nil {
					v.setArg(a, 0, ""+fmt.Sprint(r))
				}
				v.Q[9] = saved
			}()
			v.run()
		}()
		return nil
	})
	v.set(23, func(a []any) any { // (e,t,...n) => Q.get(e)===undefined ? null : Q.get(t)(...n)  n 是字面值
		if !isUndef(v.regArg(a, 0)) {
			if fn, isFn := v.regArg(a, 1).(func(...any) any); isFn {
				fn(a[2:]...)
			}
		}
		return nil
	})
	v.set(24, func(a []any) any { // (e,t,n) => Q.set(e, Q.get(t)[Q.get(n)].bind(Q.get(t)))
		obj := v.regArg(a, 1)
		name := jsStr(v.regArg(a, 2))
		if isUndef(obj) || obj == nil {
			panic("TypeError: Cannot read properties of " + jsStr(obj) + " (reading '" + name + "')")
		}
		v.setArg(a, 0, func(cargs ...any) any { return callProp(obj, name, cargs) })
		return nil
	})
	v.set(25, func(a []any) any { return nil })
	v.set(26, func(a []any) any { return nil })
	v.set(27, func(a []any) any { // (e,t) => splice 或减法
		cur := v.regArg(a, 0)
		if arr, isArr := cur.([]any); isArr {
			target := v.regArg(a, 1)
			idx := -1
			for i, e := range arr {
				if jsStrictEq(e, target) {
					idx = i
					break
				}
			}
			if idx < 0 {
				idx = len(arr) - 1 // JS: splice(-1,1) 删最后一个
			}
			if idx >= 0 {
				v.setArg(a, 0, append(append([]any{}, arr[:idx]...), arr[idx+1:]...))
			}
		} else {
			v.setArg(a, 0, jsNum(cur)-jsNum(v.regArg(a, 1)))
		}
		return nil
	})
	v.set(28, func(a []any) any { return nil })
	v.set(29, func(a []any) any { v.setArg(a, 0, jsLess(v.regArg(a, 1), v.regArg(a, 2))); return nil })
	v.set(33, func(a []any) any { // Number(t) * Number(n)
		v.setArg(a, 0, jsNum(v.regArg(a, 1))*jsNum(v.regArg(a, 2)))
		return nil
	})
	v.set(34, func(a []any) any { v.setArg(a, 0, v.regArg(a, 1)); return nil }) // Promise.resolve().then
	v.set(35, func(a []any) any {                                               // 除法，除零回 0
		r, d := jsNum(v.regArg(a, 1)), jsNum(v.regArg(a, 2))
		if d == 0 {
			v.setArg(a, 0, float64(0))
		} else {
			v.setArg(a, 0, r/d)
		}
		return nil
	})
}

// callProp 复刻 `obj[name](...args)`（bind 后的调用）
func callProp(obj any, name string, args []any) any {
	if traceOn {
		fmt.Printf("[call] %s . %s (%d args)\n", typeName(obj), name, len(args))
	}
	switch o := obj.(type) {
	case map[string]any:
		if fn, ok := o[name].(func(...any) any); ok {
			return fn(args...)
		}
		return undef
	case map[float64]any:
		if k, ok := qkey(jsonNum(name)); ok {
			if fn, ok2 := o[k].(func(...any) any); ok2 {
				return fn(args...)
			}
		}
	case string:
		// op24 会 bind 原型方法：`Q.get(t)[Q.get(n)].bind(Q.get(t))`，
		// 之后调用时走的就是这里（"a/b".split.bind("a/b")("/")）
		if fn, ok := strMethod(o, name).(func(...any) any); ok {
			return fn(args...)
		}
	}
	// 数组原型方法同理
	if fn, ok := arrMethod(obj, name).(func(...any) any); ok {
		return fn(args...)
	}
	return undef
}

func jsonNum(s string) any {
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

// jsonable 把内部值转成可 JSON 序列化的形状（undefined → null）
func jsonable(v any) any {
	switch x := v.(type) {
	case undefT:
		return nil
	case func(...any) any:
		return nil
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = jsonable(e)
		}
		return out
	case map[float64]any:
		out := map[string]any{}
		for k, e := range x {
			out[jsStr(k)] = jsonable(e)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = jsonable(e)
		}
		return out
	}
	return v
}

func (v *VM) set(i float64, fn func(a []any) any) {
	if v.names == nil {
		v.names = map[float64]string{}
	}
	if _, ok := v.names[i]; !ok {
		v.names[i] = "op" + strconv.FormatFloat(i, 'g', -1, 64)
	}
	v.Q[i] = func(a ...any) any { return fn(a) }
}

func arg(a []any, i int) any {
	if i >= len(a) {
		return undef
	}
	return a[i]
}

// run 对应 vm.js 的 async run()：顺序执行 Q[9] 里的指令流
func (v *VM) run() {
	for {
		if v.done {
			return
		}
		if !v.deadline.IsZero() && time.Now().After(v.deadline) {
			v.done, v.timedOut = true, true // 对应 setTimeout 分支
			v.res = strconv.Itoa(v.steps)
			return
		}
		prog, ok := v.Q[9].([]any)
		if !ok || len(prog) == 0 {
			return
		}
		ins, ok := prog[0].([]any)
		if !ok || len(ins) == 0 {
			v.Q[9] = prog[1:]
			continue
		}
		v.Q[9] = prog[1:]
		op, ok := qkey(ins[0])
		if !ok {
			panic("TypeError: Q.get(...) is not a function")
		}
		fn, isFn := v.Q[op].(func(...any) any) // ← 动态查表，浮点别名在这里生效
		if !isFn {
			panic("TypeError: Q.get(" + jsStr(ins[0]) + ") is not a function")
		}
		if traceOn {
			v.trail = append(v.trail, append([]any{}, ins...))
			if len(v.trail) > 40 {
				v.trail = v.trail[1:]
			}
		}
		fn(ins[1:]...)
		v.steps++
	}
}

// exec0 对应 vm.js 的 exec(dx, key, timeoutMs, softTimeout)
func (v *VM) exec0(dx string, key any, timeoutMs int, soft bool) {
	if !isUndef(key) {
		v.reset()
		v.steps = 0
		v.Q[16] = key
	}
	v.done, v.timedOut = false, false
	v.res, v.err = nil, nil
	v.deadline = time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	// Q.set(3/4) 是出口，Q.set(30) 是定义函数——注意它们不在 reset() 里
	v.set(3, func(a []any) any { v.finish(true, arg(a, 0)); return nil })
	v.set(4, func(a []any) any { v.finish(false, arg(a, 0)); return nil })
	v.set(30, func(a []any) any { // (e,t,n,r) => Q.set(e, (...args)=>{...run()...return Q.get(t)})
		hasParams := false
		var params, body []any
		if arr, isArr := arg(a, 3).([]any); isArr {
			hasParams = true
			params, _ = arg(a, 2).([]any)
			body = arr
		} else {
			body, _ = arg(a, 2).([]any)
		}
		name, _ := qkey(arg(a, 1))
		v.setArg(a, 0, func(cargs ...any) any {
			if v.done {
				return undef
			}
			saved, _ := v.Q[9].([]any)
			if hasParams {
				for i := range params {
					if k, ok := qkey(params[i]); ok {
						v.Q[k] = arg(cargs, i)
					}
				}
			}
			v.Q[9] = append([]any{}, body...)
			v.run()
			out := v.reg(name)
			v.Q[9] = saved
			return out
		})
		return nil
	})
	// 对应 vm.js：try { Q.set(9, JSON.parse(xor(atob(dx), ""+Q.get(16)))); run().catch(...) } catch(err) { ok(...) }
	defer func() {
		if r := recover(); r != nil {
			if traceOn {
				fmt.Printf("  [trail] 最近 %d 条指令（出错前）:\n", len(v.trail))
				for i, ins := range v.trail {
					nm := ""
					if k, ok := qkey(ins[0]); ok {
						if n2, ok2 := v.names[k]; ok2 {
							nm = "  (" + n2 + ")"
						}
					}
					fmt.Printf("    #%d %v%s\n", i, ins, nm)
				}
			}
			v.done = false
			v.finish(true, fmt.Sprintf("%d: %v", v.steps, r))
		}
	}()
	plain := xorStr(atob(dx), jsStr(v.Q[16]))
	var prog any
	if err := json.Unmarshal([]byte(plain), &prog); err != nil {
		panic("SyntaxError: " + err.Error())
	}
	arr, ok := prog.([]any)
	if !ok {
		panic("TypeError: 挑战程序不是数组")
	}
	v.Q[9] = arr
	v.run()
}

// Solve 对应 window.__sentinelVM.turnstile(dx, key)：timeoutMs=500、softTimeout=true
func Solve(dx, key string, timeoutMs int, win any) (token string, steps int, err error) {
	v := &VM{Q: map[float64]any{}, Window: win, scripts: scriptsOf(win)}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	v.exec0(dx, key, timeoutMs, true)
	if v.done {
		if v.err != nil {
			return "", v.steps, fmt.Errorf("%s", jsStr(v.err))
		}
		return jsStr(v.res), v.steps, nil
	}
	return strconv.Itoa(v.steps), v.steps, nil
}

// SolveSO 对应 window.__sentinelVM.so(collectorDx, snapshotDx, key)：
// 同一份 Q，collector 先跑，snapshot 沿用状态（key=undefined 不 reset）
func SolveSO(collectorDx, snapshotDx, key string, win any) (token string, steps int, err error) {
	v := &VM{Q: map[float64]any{}, Window: win, scripts: scriptsOf(win)}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	v.exec0(collectorDx, key, 60000, false)
	if v.err != nil {
		return "", v.steps, fmt.Errorf("%s", jsStr(v.err))
	}
	v.exec0(snapshotDx, undef, 60000, false)
	if v.err != nil {
		return "", v.steps, fmt.Errorf("%s", jsStr(v.err))
	}
	if !v.done {
		return "", v.steps, fmt.Errorf("vm 未产出结果")
	}
	return jsStr(v.res), v.steps, nil
}

// Analyze 离线解码 dx 并统计 opcode（零账号、零网络）：返回指令数、整数 opcode 直方图、浮点别名列表
func Analyze(dx, key string) (n int, direct map[float64]int, alias []float64, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	plain := xorStr(atob(dx), key)
	var prog []any
	if e := json.Unmarshal([]byte(plain), &prog); e != nil {
		return 0, nil, nil, e
	}
	direct = map[float64]int{}
	seen := map[float64]bool{}
	for _, ins := range prog {
		arr, ok := ins.([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		k, ok := qkey(arr[0])
		if !ok {
			continue
		}
		direct[k]++
		if k != math.Trunc(k) && !seen[k] {
			seen[k] = true
			alias = append(alias, k)
		}
	}
	return len(prog), direct, alias, nil
}

// Supported 判断 opcode 是否被本 VM 实现（0-30 除 31/32，加 33/34/35）
func Supported(op float64) bool {
	if op != math.Trunc(op) {
		return true // 浮点别名指向的是复制来的函数值，本身不是 opcode
	}
	i := int(op)
	if i < 0 || i > 35 {
		return false
	}
	return i != 31 && i != 32
}

// strMethod 返回字符串原型方法（JS 里原始值会被装箱，所以 `"a,b".split(",")` 合法）
func strMethod(s, name string) any {
	switch name {
	case "split":
		return func(a ...any) any {
			sep := jsStr(arg(a, 0))
			if sep == "" {
				out := []any{}
				for _, r := range s {
					out = append(out, string(r))
				}
				return out
			}
			parts := strings.Split(s, sep)
			out := make([]any, len(parts))
			for i, p := range parts {
				out[i] = p
			}
			return out
		}
	case "slice", "substring", "substr":
		return func(a ...any) any { return strSlice(s, a) }
	case "charAt":
		return func(a ...any) any {
			i, rs := int(jsNum(arg(a, 0))), []rune(s)
			if i < 0 || i >= len(rs) {
				return ""
			}
			return string(rs[i])
		}
	case "charCodeAt":
		return func(a ...any) any {
			i, rs := int(jsNum(arg(a, 0))), []rune(s)
			if i < 0 || i >= len(rs) {
				return math.NaN()
			}
			return float64(rs[i])
		}
	case "indexOf":
		return func(a ...any) any { return float64(strings.Index(s, jsStr(arg(a, 0)))) }
	case "lastIndexOf":
		return func(a ...any) any { return float64(strings.LastIndex(s, jsStr(arg(a, 0)))) }
	case "includes":
		return func(a ...any) any { return strings.Contains(s, jsStr(arg(a, 0))) }
	case "startsWith":
		return func(a ...any) any { return strings.HasPrefix(s, jsStr(arg(a, 0))) }
	case "endsWith":
		return func(a ...any) any { return strings.HasSuffix(s, jsStr(arg(a, 0))) }
	case "toLowerCase":
		return func(a ...any) any { return strings.ToLower(s) }
	case "toUpperCase":
		return func(a ...any) any { return strings.ToUpper(s) }
	case "trim":
		return func(a ...any) any { return strings.TrimSpace(s) }
	case "trimStart", "trimLeft":
		return func(a ...any) any { return strings.TrimLeft(s, " \t\n\r") }
	case "trimEnd", "trimRight":
		return func(a ...any) any { return strings.TrimRight(s, " \t\n\r") }
	case "replace":
		return func(a ...any) any { return strings.Replace(s, jsStr(arg(a, 0)), jsStr(arg(a, 1)), 1) }
	case "replaceAll":
		return func(a ...any) any { return strings.ReplaceAll(s, jsStr(arg(a, 0)), jsStr(arg(a, 1))) }
	case "repeat":
		return func(a ...any) any { return strings.Repeat(s, int(jsNum(arg(a, 0)))) }
	case "padStart":
		return func(a ...any) any { return padStr(s, a, true) }
	case "padEnd":
		return func(a ...any) any { return padStr(s, a, false) }
	case "concat":
		return func(a ...any) any {
			out := s
			for i := range a {
				out += jsStr(a[i])
			}
			return out
		}
	case "toString", "valueOf":
		return func(a ...any) any { return s }
	}
	return undef
}

func strSlice(s string, a []any) string {
	rs, n := []rune(s), len([]rune(s))
	from := int(jsNum(arg(a, 0)))
	if from < 0 {
		from = n + from
	}
	to := n
	if len(a) > 1 && !isUndef(a[1]) {
		if to = int(jsNum(arg(a, 1))); to < 0 {
			to = n + to
		}
	}
	if from < 0 {
		from = 0
	}
	if to > n {
		to = n
	}
	if from >= to {
		return ""
	}
	return string(rs[from:to])
}

func padStr(s string, a []any, start bool) string {
	want, pad := int(jsNum(arg(a, 0))), " "
	if len(a) > 1 {
		pad = jsStr(arg(a, 1))
	}
	if pad == "" || len(s) >= want {
		return s
	}
	fill := strings.Repeat(pad, (want-len(s))/len(pad)+1)[:want-len(s)]
	if start {
		return fill + s
	}
	return s + fill
}

// arrMethod 返回数组原型方法（挑战程序大量使用 push/pop/splice/map/join）
func arrMethod(obj any, name string) any {
	arr, ok := obj.([]any)
	if !ok {
		return nil
	}
	switch name {
	case "length":
		return float64(len(arr))
	case "push":
		return func(a ...any) any { return float64(len(arr) + len(a)) }
	case "pop":
		return func(a ...any) any {
			if len(arr) == 0 {
				return undef
			}
			return arr[len(arr)-1]
		}
	case "shift":
		return func(a ...any) any {
			if len(arr) == 0 {
				return undef
			}
			return arr[0]
		}
	case "indexOf":
		return func(a ...any) any {
			for i, e := range arr {
				if jsStrictEq(e, arg(a, 0)) {
					return float64(i)
				}
			}
			return float64(-1)
		}
	case "includes":
		return func(a ...any) any {
			for _, e := range arr {
				if jsStrictEq(e, arg(a, 0)) {
					return true
				}
			}
			return false
		}
	case "join":
		return func(a ...any) any {
			sep := ","
			if len(a) > 0 && !isUndef(a[0]) {
				sep = jsStr(arg(a, 0))
			}
			parts := make([]string, len(arr))
			for i, e := range arr {
				parts[i] = jsStr(e)
			}
			return strings.Join(parts, sep)
		}
	case "slice":
		return func(a ...any) any { return arrSlice(arr, a) }
	case "splice":
		return func(a ...any) any {
			start := int(jsNum(arg(a, 0)))
			if start < 0 {
				start = len(arr) + start
			}
			if start < 0 {
				start = 0
			}
			if start > len(arr) {
				start = len(arr)
			}
			del := len(arr) - start
			if len(a) > 1 {
				del = int(jsNum(arg(a, 1)))
			}
			if start+del > len(arr) {
				del = len(arr) - start
			}
			return append([]any{}, arr[start:start+del]...)
		}
	case "concat":
		return func(a ...any) any {
			out := append([]any{}, arr...)
			for i := range a {
				if sub, isArr := a[i].([]any); isArr {
					out = append(out, sub...)
				} else {
					out = append(out, a[i])
				}
			}
			return out
		}
	case "reverse":
		return func(a ...any) any {
			out := append([]any{}, arr...)
			for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
				out[i], out[j] = out[j], out[i]
			}
			return out
		}
	case "map":
		return func(a ...any) any {
			fn, _ := arg(a, 0).(func(...any) any)
			out := make([]any, 0, len(arr))
			for i, e := range arr {
				if fn != nil {
					out = append(out, fn(e, float64(i), arr))
				} else {
					out = append(out, e)
				}
			}
			return out
		}
	case "filter":
		return func(a ...any) any {
			fn, _ := arg(a, 0).(func(...any) any)
			out := []any{}
			for i, e := range arr {
				if fn != nil && jsTruthy(fn(e, float64(i), arr)) {
					out = append(out, e)
				}
			}
			return out
		}
	case "forEach":
		return func(a ...any) any {
			if fn, ok := arg(a, 0).(func(...any) any); ok {
				for i, e := range arr {
					fn(e, float64(i), arr)
				}
			}
			return undef
		}
	case "some":
		return func(a ...any) any {
			fn, _ := arg(a, 0).(func(...any) any)
			for i, e := range arr {
				if fn != nil && jsTruthy(fn(e, float64(i), arr)) {
					return true
				}
			}
			return false
		}
	case "every":
		return func(a ...any) any {
			fn, _ := arg(a, 0).(func(...any) any)
			for i, e := range arr {
				if fn != nil && !jsTruthy(fn(e, float64(i), arr)) {
					return false
				}
			}
			return true
		}
	case "find":
		return func(a ...any) any {
			fn, _ := arg(a, 0).(func(...any) any)
			for i, e := range arr {
				if fn != nil && jsTruthy(fn(e, float64(i), arr)) {
					return e
				}
			}
			return undef
		}
	case "reduce":
		return func(a ...any) any {
			fn, _ := arg(a, 0).(func(...any) any)
			var acc any
			i := 0
			if len(a) > 1 {
				acc = a[1]
			} else if len(arr) > 0 {
				acc, i = arr[0], 1
			}
			for ; i < len(arr); i++ {
				if fn != nil {
					acc = fn(acc, arr[i], float64(i), arr)
				}
			}
			return acc
		}
	case "toString":
		return func(a ...any) any {
			parts := make([]string, len(arr))
			for i, e := range arr {
				parts[i] = jsStr(e)
			}
			return strings.Join(parts, ",")
		}
	}
	return nil
}

func arrSlice(arr []any, a []any) []any {
	n := len(arr)
	from := int(jsNum(arg(a, 0)))
	if from < 0 {
		from = n + from
	}
	to := n
	if len(a) > 1 && !isUndef(a[1]) {
		if to = int(jsNum(arg(a, 1))); to < 0 {
			to = n + to
		}
	}
	if from < 0 {
		from = 0
	}
	if to > n {
		to = n
	}
	if from >= to {
		return []any{}
	}
	return append([]any{}, arr[from:to]...)
}

// jsTruthy 复刻 JS 的 truthiness
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case undefT, nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	case []any:
		return true
	}
	return true
}

// scriptsOf 从模拟 window.document.scripts 里取 src 列表。
// 挑战程序用 op11 按正则 /sentinel/[^/]+/sdk\.js/ 找 sdk 地址，再 split("/").pop() 取版本号；
// 真实页面里这个 script 是首页 bundle 动态插入的，所以纯 Go 模式必须手工填进去。
func scriptsOf(win any) []string {
	w, ok := win.(map[string]any)
	if !ok {
		return nil
	}
	doc, ok := w["document"].(map[string]any)
	if !ok {
		return nil
	}
	arr, ok := doc["scripts"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			if src, ok := m["src"].(string); ok {
				out = append(out, src)
			}
		}
	}
	return out
}

// stripLookahead 去掉 JS 正则里的 (?=...) / (?!...) 断言组（Go regexp 不支持）
func stripLookahead(pat string) string {
	var sb strings.Builder
	for i := 0; i < len(pat); i++ {
		if i+2 < len(pat) && pat[i] == '(' && pat[i+1] == '?' && (pat[i+2] == '=' || pat[i+2] == '!') {
			depth, j := 1, i+3
			for ; j < len(pat) && depth > 0; j++ {
				if pat[j] == '(' {
					depth++
				} else if pat[j] == ')' {
					depth--
				}
			}
			i = j - 1
			continue
		}
		sb.WriteByte(pat[i])
	}
	return sb.String()
}
