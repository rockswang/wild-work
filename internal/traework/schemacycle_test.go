package traework

import (
	"encoding/json"
	"testing"
)

// sanitize1 处理单 tool 并返回消毒后的 parameters（map 形态）。
// 消毒器返回全新副本、不改写入参，断言一律读返回值。
func sanitize1(params map[string]any) map[string]any {
	body := map[string]any{"tools": []any{
		map[string]any{"function": map[string]any{"parameters": params}},
	}}
	breakToolSchemaCycles(body)
	return body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
}

// 用例镜像线上 4027 报错路径：properties.format.anyOf 内的 $ref。
// 2026-10-13 23:50 实测：非环 $ref 原样保留仍被 Moonshot 拒（4027），
// 故策略为全量内联——消毒后 schema 中不允许残留任何 $ref。
func TestBreakToolSchemaCycles_AnyOfSelfRef(t *testing.T) {
	cleaned := sanitize1(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"format": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"$ref": "#/$defs/node"},
				},
			},
		},
		"$defs": map[string]any{
			"node": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"self": map[string]any{"$ref": "#/$defs/node"},
				},
			},
		},
	})

	if n := countRefs(cleaned); n != 0 {
		t.Fatalf("消毒后不应残留 $ref，剩 %d 个", n)
	}
	anyOf := cleaned["properties"].(map[string]any)["format"].(map[string]any)["anyOf"].([]any)
	// anyOf[1] 的 $ref 内联为 node 内容，展开链内成环点 self 截断为空 schema
	inlined := anyOf[1].(map[string]any)
	if inlined["type"] != "object" {
		t.Fatalf("$ref 应被内联为目标内容，得到 %v", inlined)
	}
	if self := inlined["properties"].(map[string]any)["self"].(map[string]any); len(self) != 0 {
		t.Fatalf("内联展开链成环点应截断为空 schema，得到 %v", self)
	}
	// anyOf[0] 不受影响
	if b0 := anyOf[0].(map[string]any)["type"]; b0 != "string" {
		t.Fatalf("非环分支不应被改，得到 %v", b0)
	}
	// $defs.node 原位走到的 self 被内联为 node 副本，副本内的环点 self 截断
	defSelf := cleaned["$defs"].(map[string]any)["node"].(map[string]any)["properties"].(map[string]any)["self"].(map[string]any)
	if defSelf["type"] != "object" {
		t.Fatalf("$defs 内成环点应被内联，得到 %v", defSelf)
	}
	if nested := defSelf["properties"].(map[string]any)["self"].(map[string]any); len(nested) != 0 {
		t.Fatalf("内联副本内的环点应截断为空 schema，得到 %v", nested)
	}
	assertClean(t, cleaned)
}

func TestBreakToolSchemaCycles_MutualRecursion(t *testing.T) {
	cleaned := sanitize1(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"data": map[string]any{"$ref": "#/$defs/a"},
		},
		"$defs": map[string]any{
			"a": map[string]any{"$ref": "#/$defs/b"},
			"b": map[string]any{"$ref": "#/$defs/a"},
		},
	})
	if n := countRefs(cleaned); n != 0 {
		t.Fatalf("互递归消毒后不应残留 $ref，剩 %d 个", n)
	}
	// data 内联 a→b→a 链，第三个 a 已在链上 → 整链收敛为空 schema
	if data := cleaned["properties"].(map[string]any)["data"].(map[string]any); len(data) != 0 {
		t.Fatalf("互递归入口应收敛为空 schema，得到 %v", data)
	}
	if a := cleaned["$defs"].(map[string]any)["a"].(map[string]any); len(a) != 0 {
		t.Fatalf("$defs.a 互递归应收敛为空 schema，得到 %v", a)
	}
	assertClean(t, cleaned)
}

func TestBreakToolSchemaCycles_NonCyclicRefInlined(t *testing.T) {
	cleaned := sanitize1(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x": map[string]any{"$ref": "#/$defs/name"},
			"y": map[string]any{"$ref": "#/$defs/name"},
		},
		"$defs": map[string]any{
			"name": map[string]any{"type": "string", "maxLength": 8},
		},
	})
	if n := countRefs(cleaned); n != 0 {
		t.Fatalf("消毒后不应残留 $ref，剩 %d 个", n)
	}
	x := cleaned["properties"].(map[string]any)["x"].(map[string]any)
	if x["type"] != "string" {
		t.Fatalf("非环 $ref 应被内联为目标内容，得到 %v", x)
	}
	// 菱形引用（x、y 指向同一目标）各自独立内联，互不污染
	y := cleaned["properties"].(map[string]any)["y"].(map[string]any)
	if y["type"] != "string" {
		t.Fatalf("菱形第二个引用同样内联，得到 %v", y)
	}
}

func TestBreakToolSchemaCycles_RootHashRef(t *testing.T) {
	cleaned := sanitize1(map[string]any{
		"type":       "object",
		"properties": map[string]any{"child": map[string]any{"$ref": "#"}},
	})
	child := cleaned["properties"].(map[string]any)["child"].(map[string]any)
	if len(child) != 0 {
		t.Fatalf("\"#\" 自引用应截断为空 schema，得到 %v", child)
	}
	if cleaned["type"] != "object" {
		t.Fatal("其余字段不应受影响")
	}
}

func TestBreakToolSchemaCycles_NonLocalRefDropped(t *testing.T) {
	cleaned := sanitize1(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"x": map[string]any{"$ref": "https://example.com/schema.json#/foo"},
		},
	})
	x := cleaned["properties"].(map[string]any)["x"].(map[string]any)
	if len(x) != 0 {
		t.Fatalf("无法内联的非本地 $ref 应截断为空 schema，得到 %v", x)
	}
}

func TestInlineSchemaRefs_BudgetExhausted(t *testing.T) {
	// n0 → n1 → … → n5 链式引用，预算 2：前两次内联成功，其后截断为空 schema
	defs := map[string]any{}
	for i := 0; i < 5; i++ {
		defs["n"+string(rune('0'+i))] = map[string]any{"$ref": "#/$defs/n" + string(rune('0'+i+1))}
	}
	defs["n5"] = map[string]any{"type": "string"}
	params := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"p": map[string]any{"$ref": "#/$defs/n0"},
		},
		"$defs": defs,
	}
	budget := 2
	cleaned := inlineSchemaRefs(params, params, map[string]bool{}, &budget).(map[string]any)
	if n := countRefs(cleaned); n != 0 {
		t.Fatalf("预算耗尽后不应残留 $ref，剩 %d 个", n)
	}
	if p := cleaned["properties"].(map[string]any)["p"].(map[string]any); len(p) != 0 {
		t.Fatalf("预算耗尽应截断为空 schema，得到 %v", p)
	}
}

func TestBreakToolSchemaCycles_NoToolsNoPanic(t *testing.T) {
	breakToolSchemaCycles(map[string]any{})
	breakToolSchemaCycles(map[string]any{"tools": "not-array"})
	breakToolSchemaCycles(map[string]any{"tools": []any{"str-item"}})
}

// 字符串形态：客户端把 parameters 预序列化下发（线上 23:36 绕过案例）。
func TestBreakToolSchemaCycles_StringParams(t *testing.T) {
	fn := map[string]any{"name": "render",
		"parameters": `{"type":"object","properties":{"format":{"anyOf":[{"type":"string"},{"$ref":"#"}]}}}`}
	breakToolSchemaCycles(map[string]any{"tools": []any{map[string]any{"function": fn}}})
	got, ok := fn["parameters"].(string)
	if !ok {
		t.Fatalf("parameters 应保持字符串，得到 %T", fn["parameters"])
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(got), &schema); err != nil {
		t.Fatalf("parameters 应被重写为合法 JSON: %v", err)
	}
	if n := countRefs(schema); n != 0 {
		t.Fatalf("字符串形态消毒后不应残留 $ref，剩 %d 个", n)
	}
	anyOf := schema["properties"].(map[string]any)["format"].(map[string]any)["anyOf"].([]any)
	if len(anyOf[1].(map[string]any)) != 0 {
		t.Fatalf("字符串形态的成环分支应截断为空 schema，得到 %v", anyOf[1])
	}
}

// 字符串形态但非 JSON 对象：原样透传，不得改写。
func TestBreakToolSchemaCycles_StringParamsNonObject(t *testing.T) {
	fn := map[string]any{"name": "render", "parameters": "true"}
	breakToolSchemaCycles(map[string]any{"tools": []any{map[string]any{"function": fn}}})
	if got := fn["parameters"]; got != "true" {
		t.Fatalf("非对象字符串应原样保留，得到 %v", got)
	}
}

// hasRefCycle 模拟 Moonshot 的 schema 展开：遇到 $ref 解析到目标继续展开，
// 展开栈上重复到达即环。栈按分支复制（兄弟分支互不污染）。
func hasRefCycle(node, root any, stack map[string]bool) bool {
	switch t := node.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			if ptr, isLocal := localRefPtr(ref); isLocal {
				if ptr == "" || stack[ptr] {
					return true
				}
				target := resolveLocalRef(root, ptr)
				if target == nil {
					return false
				}
				child := make(map[string]bool, len(stack)+1)
				for k := range stack {
					child[k] = true
				}
				child[ptr] = true
				return hasRefCycle(target, root, child)
			}
			return false
		}
		for _, v := range t {
			if hasRefCycle(v, root, stack) {
				return true
			}
		}
	case []any:
		for _, v := range t {
			if hasRefCycle(v, root, stack) {
				return true
			}
		}
	}
	return false
}

func assertClean(t *testing.T, params any) {
	t.Helper()
	if hasRefCycle(params, params, map[string]bool{}) {
		t.Fatal("消毒后展开仍有环")
	}
}

// 端到端：PrepareBody 对 kimi-k3 的 tools 完成消毒（$ref 清零）后再字符串化。
func TestPrepareBodyKimiToolSanitized(t *testing.T) {
	req := `{"model": "kimi-k3", "messages": [{"role": "user", "content": "hi"}], "tools": [{"type": "function", "function": {"name": "render", "description": "r", "parameters": {"type": "object", "properties": {"format": {"anyOf": [{"type": "string"}, {"$ref": "#"}]}}}}}]}`
	out := PrepareBody([]byte(req), Function)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出非法 JSON: %v", err)
	}
	tools := obj["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	ps, ok := fn["parameters"].(string)
	if !ok {
		t.Fatalf("parameters 应被字符串化，得到 %T", fn["parameters"])
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(ps), &schema); err != nil {
		t.Fatalf("parameters 字符串非法: %v", err)
	}
	if n := countRefs(schema); n != 0 {
		t.Fatalf("消毒后不应残留 $ref，剩 %d 个", n)
	}
	anyOf := schema["properties"].(map[string]any)["format"].(map[string]any)["anyOf"].([]any)
	if len(anyOf[1].(map[string]any)) != 0 {
		t.Fatalf("kimi 模型的成环分支应截断为空 schema，得到 %v", anyOf[1])
	}
}

// 端到端：非 kimi 命名的模型 + 字符串形态 parameters，同样消毒（不做模型名门控）。
func TestPrepareBodySanitizesAllModelsStringParams(t *testing.T) {
	req := `{"model":"Doubao-Seed-2.1-Pro","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"render","parameters":"{\"type\":\"object\",\"properties\":{\"child\":{\"$ref\":\"#\"}}}"}}]}`
	out := PrepareBody([]byte(req), Function)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出非法 JSON: %v", err)
	}
	fn := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
	ps, ok := fn["parameters"].(string)
	if !ok {
		t.Fatalf("parameters 应为字符串，得到 %T", fn["parameters"])
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(ps), &schema); err != nil {
		t.Fatalf("parameters 字符串非法: %v", err)
	}
	if n := countRefs(schema); n != 0 {
		t.Fatalf("消毒后不应残留 $ref，剩 %d 个", n)
	}
}

// 无 $ref 的 parameters 必须原样保留（两种形态都是）：重序列化会重排键序、改写数字
// 字面量（1e9 → 1e+09），白污染上游 prompt 缓存前缀；字符串形态旧实现还按工具数刷日志。
func TestBreakToolSchemaCycles_LeavesRefFreeParamsUntouched(t *testing.T) {
	raw := `{"type":"object","properties":{"n":{"type":"integer","default":1e9},"a":{"type":"string"}}}`
	mk := func(params any) map[string]any {
		return map[string]any{"model": "kimi-k3", "tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "t", "parameters": params}},
		}}
	}
	// 字符串形态
	obj := mk(raw)
	breakToolSchemaCycles(obj)
	if got := obj["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"]; got != raw {
		t.Fatalf("无 $ref 的字符串形态应原样保留，得到 %T %v", got, got)
	}
	// map 形态
	obj2 := mk(map[string]any{"type": "string"})
	breakToolSchemaCycles(obj2)
	if got := obj2["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"]; got == nil {
		t.Fatal("map 形态参数被丢弃")
	}
	// 空串 parameters 不记「str-unparsed」（无断言，仅确保不 panic 且原样留着）
	obj3 := mk("")
	breakToolSchemaCycles(obj3)
	if got := obj3["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"]; got != "" {
		t.Fatalf("空串 parameters 应保持空串，得到 %v", got)
	}
}
