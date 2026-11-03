// payload.go 改写发往上游的 chat 请求体：
//  1. 强制 stream:true（上游拒绝非流式）
//  2. tool_choice 归一化（上游该字段是 string，对象形式会 400 code=11101）
package upstream

import (
	"encoding/json"
	"log"
	"strings"

	"wild-work/internal/sanitize"
)

// PrepareBody 单 pass 改写；无法解析时原样返回。
// 出站脱敏在 sanitize.Messages 内完成（只动 messages 结构化字段，不做整包替换——
// 上游对 tools 等非 messages 字段的品牌串实测放行，整包兜底已于 2026-11-02 删除）。
func PrepareBody(src []byte) []byte {
	return sanitize.Messages(prepareBodyInner(src))
}

// prepareBodyInner 同 PrepareBody 但不含脱敏（供内部调用）。
func prepareBodyInner(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	// max_completion_tokens → max_tokens 翻译（吸收 PR #116，Closes #117）：
	// OpenAI 规范里 max_tokens 已 deprecated、max_completion_tokens 是新字段
	// （o-series 起引入）；DeepSeek Harness 等新客户端只发别名。WorkBuddy 上游
	//（CN /v2 与 global /console 同源，见 context_catalog「两区是同一套 API 的两次
	// 部署」实测结论）只认 max_tokens——别名透传会被上游忽略后回落默认输出上限
	//（实测 32000），长流任务被截。
	//   - 显式 max_tokens 存在 → 原样保留（显式优先，别名只删不译）；
	//   - 别名值为 0/null/负数/非数值 → 不翻译（0/null 语义是「未设置」，走上游
	//     默认；负数是非法值，翻译等于把垃圾搬进 max_tokens）；
	//   - 翻译后删别名字段（上游 Go struct 未知字段宽松，但留着徒增 body 体积与
	//     排障噪音）。
	translateMaxCompletionTokens(obj)
	// stream_options 仅当 body 未显式带时补 {include_usage: true}（D7）：
	// 官方 CLI 流式必发该字段，上游据此在末帧返回 usage 用量；显式带则不覆盖。
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeToolChoice(obj)
	normalizeRoles(obj) // developer → system（上游对 developer 角色触发内容过滤误杀）

	// tool 配对两步（见 tool_pairing.go）：先重排再清理。所有模型一律执行（独立于
	// deepseek-only 的 sanitize 开关）。这是「让请求通过」的安全网——不完整配对的
	// tool_calls/tool 结果会让上游对之后每条消息都返 400，必须先行剔除；
	// 插在结果中间的非 tool 消息（Codex image_resize_notice）同样判配对断裂，
	// 先 repack 挪后，再 cleanup 删孤儿，两侧同口径。
	if msgs, ok := obj["messages"].([]any); ok {
		msgs, _ = repackToolResultBlocks(msgs)
		msgs, _ = cleanupOrphanToolCalls(msgs)
		// 无改动时两步都返回原 slice，这里回写等于零操作；任一步重排/删除
		// （哪怕后续步骤零改动）也必须落到 obj——不能只在「最后一步改动」时回写，
		// 否则 repack 单独生效的结果会被原 slice 覆盖丢失。
		obj["messages"] = msgs
	}
	// 出站脱敏（全改写完成后、Marshal 前）：剥离上游内容审核黑名单指纹
	// （Claude Code / Codex CLI 注入的模板句、billing header、11128 等，见 sanitize.go）。
	if msgs, ok := obj["messages"].([]any); ok {
		sanitizeMessages(msgs)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// translateMaxCompletionTokens 把 OpenAI 别名 max_completion_tokens 翻译为上游
// 认的 max_tokens（吸收 PR #116）。调用点在 PrepareBodyOptWithEffortsAndDefault
// 管线 stream 强制之后（同一预处理管线挂载，任务书 prompt-too-long §3）。
// 规则：显式 max_tokens 优先（别名只删）；别名非正数值（0/null/负数）不翻译；
// 非数值别名（字符串等畸形）不翻译（原样透传由上游报 11101 参数错）。
// 两域同口径：CN /v2 与 global /console 是同一套 API（见 context_catalog 文件头
// 实测结论），翻译不分 realm——global 域上游同样只认 max_tokens。
func translateMaxCompletionTokens(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens") // 无论翻译与否，别名一律删（见上方注释）
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return // 显式 max_tokens 优先：别名只删不译
	}
	// json.Unmarshal 数字 → float64（整数去整后回写，避免 1.28e5 科学计数法/小数尾
	// 巴进上游 body）；其他数值类型防御性兼容（int 家族——手构造 map 的调用方）。
	switch v := alias.(type) {
	case float64:
		if v > 0 && v == float64(int64(v)) {
			obj["max_tokens"] = int64(v)
		}
	case int64:
		if v > 0 {
			obj["max_tokens"] = v
		}
	case int:
		if v > 0 {
			obj["max_tokens"] = int64(v)
		}
	}
}

// normalizeRoles 把 messages 里的 developer 角色归一为 system。
//
// 背景：上游对 messages 的 role 字段做白名单校验，developer 不在白名单内，
// 命中即 HTTP 400 code=11128。developer 是 OpenAI 新规范里 system 的别名
// （Codex / Cursor 等新客户端用它承载 system 级指令），改写为 system 不丢语义。
//
// 此归一化是「协议兼容」（补上游 role 白名单），不是「内容脱敏」，
// 因此有意与 SanitizeFingerprints / sanitize 参数解耦：即使 sanitize=false 也照常归一。
//
// 只认 developer 这一个值：其余 role（system/user/assistant/tool/任意未知值）一律原样保留，
// 不合并、不重排、不删除任何消息（上游对多 system 的行为尚未实测，合并会引入新变量）。
func NormalizeRoles(obj map[string]any) {
	normalizeRoles(obj)
}

// normalizeRoles 是 NormalizeRoles 的实现体（保留原签名供包内 PrepareBody 调用）。
//
// 2026-10-18 导出 NormalizeRoles 的原因：本仓此前有 10 处渠道各自复制了这段逻辑，
// 而 officeace 漏做 ⇒ DSH 一发 developer 角色就必 400 ModelArts.81001
// （role 白名单不含 developer）。凡「不走 upstream.PrepareBody」的渠道，
// 请直接调 NormalizeRoles，不要再复制第 11 份。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for i, m := range msgs {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(role), "developer") {
			msg["role"] = "system"
			log.Printf("[upstream] role normalized developer->system idx=%d", i)
		}
	}
}

// normalizeToolChoice 按上游 Go struct（string 类型）改写 OpenAI tool_choice。
//   - "none"            → 删 tool_choice + 删 tools/functions
//   - {"type":"none"}   → 同上
//   - {"type":"auto"/"required"} → 字符串 "auto"/"required"
//   - {"type":"function","function":{"name":"x"}} → 字符串 "x"
//   - 其他对象/非标量 → 删 tool_choice
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}
