package traework

import (
	"encoding/json"
	"log"
	"strconv"
	"strings"
)

// trae solo 上游（含 Kimi/Moonshot 系模型）对 tools.function.parameters 的
// JSON Schema 校验极严：schema 含 $ref 即按「moonshot flavored json schema」
// 展开校验，展开不收敛就整体 400——「detected infinite recursion without
// termination condition」（2026-10-13 实测 kimi-k3 code=4027，路径
// properties.format.anyOf）。官方客户端不发 $ref schema，代理客户端
// （如经 MCP 接入的工具）会发。
//
// 消毒策略（两版演进）：
//  1. 仅打断成环 $ref、保留非环 $ref —— 23:50 实测被否：kimi-k3 对三个
//     form=map、ref_before=1 的 tool 原样放行后仍 4027，Moonshot 对任何
//     保留的 $ref 都不认账（其校验器对非环 $ref 的展开语义未知）。
//  2. 现行：全量内联——所有本地 $ref 就地替换为目标节点的消毒副本，
//     内联展开链成环处（含 "#"）截断为空 schema（JSON Schema 语义 =
//     接受任意值）；非本地/不可解析/超出预算的 $ref 同样截断。结果
//     schema 中一个 $ref 都不剩。语义损失仅限递归分支处，对容忍递归的
//     OpenAI/Anthropic 后端，展开后的有限 schema 依旧合法。
//
// 不按模型名门控：客户端可能用自定义模型别名（不含 kimi 字样）。
// 支持 parameters 预序列化为字符串的形态——原样透传会让消毒被绕过
// （2026-10-13 23:36 traecode 4027 复现）。
// 请求带 $ref 时打一行诊断日志（form / ref_before / ref_after），
// 便于定位漏网形态。
func breakToolSchemaCycles(obj map[string]any) {
	raw, ok := obj["tools"].([]any)
	if !ok {
		return
	}
	model, _ := obj["model"].(string)
	for _, item := range raw {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		switch params := fn["parameters"].(type) {
		case map[string]any:
			before := countRefs(params)
			if before == 0 {
				continue
			}
			fn["parameters"] = coerceObject(inlineSchemaRefs(params, params, map[string]bool{}, newInlineBudget()))
			log.Printf("traework schema scan: model=%s tool=%q form=map ref_before=%d ref_after=%d",
				model, name, before, countRefs(fn["parameters"]))
		case string:
			var m map[string]any
			if err := json.Unmarshal([]byte(params), &m); err != nil || m == nil {
				if strings.TrimSpace(params) != "" { // 空串是「无参数」的常见形态，不记日志
					log.Printf("traework schema scan: model=%s tool=%q form=str-unparsed bytes=%d",
						model, name, len(params))
				}
				continue
			}
			// 与 map 分支同口径：无 $ref 就不动原文（回写 Marshal 结果会重排键序、
			// 改写数字字面量，白白污染上游 prompt 缓存前缀，还会每请求刷一行日志）。
			before := countRefs(m)
			if before == 0 {
				continue
			}
			cleaned := coerceObject(inlineSchemaRefs(m, m, map[string]bool{}, newInlineBudget()))
			log.Printf("traework schema scan: model=%s tool=%q form=str ref_before=%d ref_after=%d",
				model, name, before, countRefs(cleaned))
			if s, err := json.Marshal(cleaned); err == nil {
				fn["parameters"] = string(s)
			}
		default:
			if _, present := fn["parameters"]; present {
				log.Printf("traework schema scan: model=%s tool=%q form=other type=%T",
					model, name, fn["parameters"])
			}
		}
	}
}

// inlineBudget 限制单个 tool 的 $ref 内联展开次数，防止菱形引用组合爆炸。
const inlineBudget = 4096

func newInlineBudget() *int { b := inlineBudget; return &b }

// coerceObject 保证消毒结果为 object schema（parameters 约定顶层是对象）；
// 内联目标若是标量/布尔 schema 等非常规形态，回落为空 schema。
func coerceObject(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

// inlineSchemaRefs 返回 node 的消毒副本：所有 $ref 就地内联（以目标内容为
// 基底继续内联，$ref 的兄弟键按 draft-07 语义丢弃），chain 记录当前展开链上
// 已入栈的 $ref 目标指针（分叉时复制，互不污染），链上重现即递归环——截断
// 为空 schema 终止。root 为整个 parameters 文档（$ref 解析基准）。budget
// 防组合爆炸，耗尽后剩余 $ref 一律截断为空 schema。不改写入参。
func inlineSchemaRefs(node, root any, chain map[string]bool, budget *int) any {
	switch t := node.(type) {
	case map[string]any:
		if ref, ok := t["$ref"].(string); ok {
			if ptr, isLocal := localRefPtr(ref); isLocal && ptr != "" && !chain[ptr] {
				if target := resolveLocalRef(root, ptr); target != nil && *budget > 0 {
					*budget--
					child := make(map[string]bool, len(chain)+1)
					for k := range chain {
						child[k] = true
					}
					child[ptr] = true
					return inlineSchemaRefs(target, root, child, budget)
				}
			}
			return map[string]any{} // "#" 必环 / 展开链成环 / 非本地 / 不可达 / 预算耗尽
		}
		out := make(map[string]any, len(t))
		for k, v := range t {
			out[k] = inlineSchemaRefs(v, root, chain, budget)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, v := range t {
			out[i] = inlineSchemaRefs(v, root, chain, budget)
		}
		return out
	default:
		return node
	}
}

// countRefs 统计 schema 子树中 $ref 键的个数（诊断用）。
func countRefs(node any) int {
	n := 0
	switch t := node.(type) {
	case map[string]any:
		if _, ok := t["$ref"]; ok {
			n++
		}
		for k, v := range t {
			if k == "$ref" {
				continue
			}
			n += countRefs(v)
		}
	case []any:
		for _, v := range t {
			n += countRefs(v)
		}
	}
	return n
}

// localRefPtr 解析本地 $ref："#/a/b" → "/a/b"；"#" → ""（必环）；非本地引用
// （外链 / 非 JSON Pointer 形态）返回 false 交由内联层截断。
func localRefPtr(ref string) (string, bool) {
	r := strings.TrimSpace(ref)
	if !strings.HasPrefix(r, "#") {
		return "", false
	}
	p := strings.TrimPrefix(r, "#")
	if p == "" {
		return "", true
	}
	if !strings.HasPrefix(p, "/") {
		return "", false
	}
	return p, true
}

// resolveLocalRef 按 JSON Pointer（RFC 6901）在文档内取目标节点；不可达返回 nil。
func resolveLocalRef(root any, ptr string) any {
	cur := root
	for _, seg := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		seg = strings.ReplaceAll(seg, "~1", "/")
		seg = strings.ReplaceAll(seg, "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			nxt, ok := c[seg]
			if !ok {
				return nil
			}
			cur = nxt
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(c) {
				return nil
			}
			cur = c[i]
		default:
			return nil
		}
	}
	return cur
}
