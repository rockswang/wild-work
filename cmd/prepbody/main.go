// Command prepbody 把客户端 dump 的**入站**请求体，按 wild-work 线上同一条改写链
// 处理成「真正发给上游」的**出站**形态并落盘，供重放 / A-B 对照脚本使用。
//
// 用法：
//
//	go run ./cmd/prepbody <入站 dump.json> <出站 body.json> [model]
//
// 为什么需要它：dump 里的 *_client.json 是 prepareChatBody **之前**的原始体
// （devdump.DumpClientBody 刻意放在改写前，便于重放客户端真实请求），直接发上游会被拒——
// 实测 dump 里有 71 条 content 字段缺失的 assistant 消息，上游会整请求拒答。
//
// 处理链与线上逐段一致：
//
//	internal/server  prepareChatBody：reasoning 归一化 → tool 轮 content 补空串 → model 覆写
//	internal/workbuddyai PrepareBody：sanitize → stream 强制 → max_tokens 归一 → 档位投影
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"wild-work/internal/reasoning"
	"wild-work/internal/workbuddyai"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "用法: prepbody <入站dump.json> <出站body.json> [model]")
		os.Exit(2)
	}
	in, out := os.Args[1], os.Args[2]
	model := "deepseek-v4.1-flash"
	if len(os.Args) > 3 {
		model = os.Args[3]
	}

	raw, err := os.ReadFile(in)
	must(err)
	var obj map[string]any
	must(json.Unmarshal(raw, &obj))

	normalized, _, err := reasoning.NormalizeChat(obj, false)
	must(err)
	normalizeToolTurnContent(normalized)
	normalized["model"] = model

	mid, err := json.Marshal(normalized)
	must(err)
	final := workbuddyai.PrepareBody(mid)
	must(os.WriteFile(out, final, 0o600))

	var probe map[string]any
	_ = json.Unmarshal(final, &probe)
	keys := make([]string, 0, len(probe))
	for k := range probe {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Printf("写出 %s（%d bytes）\n", out, len(final))
	fmt.Printf("顶层字段   : %v\n", keys)
	fmt.Printf("model      : %v\n", probe["model"])
	fmt.Printf("max_tokens : %v\n", probe["max_tokens"])
	fmt.Printf("effort     : %v\n", probe["reasoning_effort"])
	fmt.Printf("stream     : %v\n", probe["stream"])
}

// normalizeToolTurnContent 与 internal/server 的同名函数逐字一致。
// 该函数未导出，故此处复制一份；两处行为必须保持同步（改动请同时改两边）。
func normalizeToolTurnContent(obj map[string]any) {
	msgs, _ := obj["messages"].([]any)
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "assistant", "tool":
		default:
			continue
		}
		if c, exists := m["content"]; !exists || c == nil {
			m["content"] = ""
		}
	}
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}
