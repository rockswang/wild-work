// wb_utils.go WorkBuddy 国际版渠道的通用纯函数：请求体改写、SSE/聚合、文案与倍率。
// Stream/Aggregate 委托 internal/upstream（共用已修复的实现，避免重复代码）。
package workbuddyai

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"wild-work/internal/sanitize"
	"wild-work/internal/upstream"
)

// ---------------------------------------------------------------------------
// 请求体改写
// ---------------------------------------------------------------------------

// PrepareBody 改写发往国际版上游的 chat 请求体。
func PrepareBody(src []byte) []byte {
	return prepareBodyInner(src)
}

func prepareBodyInner(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	translateMaxCompletionTokensWBAI(obj)
	if _, has := obj["stream_options"]; !has {
		obj["stream_options"] = map[string]any{"include_usage": true}
	}
	normalizeRoles(obj)
	normalizeToolChoice(obj)
	ensureLeadingSystemMessage(obj)
	// 出站脱敏（规则见 sanitize.go）
	sanitize.MessagesInPlace(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// ensureLeadingSystemMessage 保证首条消息为 system。
func ensureLeadingSystemMessage(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return
	}
	if first, ok := msgs[0].(map[string]any); ok {
		if r, _ := first["role"].(string); r == "system" {
			return
		}
	}
	obj["messages"] = append([]any{map[string]any{
		"role":    "system",
		"content": "You are a helpful assistant.",
	}}, msgs...)
}

// normalizeRoles 将 developer 改写为 system。
func normalizeRoles(obj map[string]any) {
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := mm["role"].(string); r == "developer" {
			mm["role"] = "system"
		}
	}
}

// normalizeToolChoice 按上游 string 类型要求改写 tool_choice。
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
		switch strings.ToLower(strings.TrimSpace(typ)) {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = strings.ToLower(strings.TrimSpace(typ))
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

func translateMaxCompletionTokensWBAI(obj map[string]any) {
	alias, has := obj["max_completion_tokens"]
	delete(obj, "max_completion_tokens")
	if !has {
		return
	}
	if _, explicit := obj["max_tokens"]; explicit {
		return
	}
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

// ---------------------------------------------------------------------------
// SSE 透传 / 聚合（委托 internal/upstream）
// ---------------------------------------------------------------------------

// StreamCapture 同 Stream，额外捕获末帧 usage（记账用）。
func StreamCapture(w http.ResponseWriter, r io.Reader, onUsage func(map[string]any)) error {
	return upstream.StreamCapture(w, r, onUsage)
}

// Aggregate 聚合 SSE 为单个响应。
func Aggregate(r io.Reader) (map[string]any, error) { return upstream.Aggregate(r) }

// ---------------------------------------------------------------------------
// 文案与倍率
// ---------------------------------------------------------------------------

// truncate 截断错误文案（保留可读头部，补 n<=0 守卫防 panic）。
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// parseCredits 解析目录 credits 字段为倍率数值。
func parseCredits(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	s = strings.TrimPrefix(s, "x")
	s = strings.TrimSuffix(s, "x")
	s = strings.TrimSuffix(s, " credits")
	s = strings.TrimSpace(s)
	var v float64
	fmt.Sscanf(s, "%f", &v)
	return v
}
