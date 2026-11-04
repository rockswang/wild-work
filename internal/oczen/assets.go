// assets.go 官方 OpenCode 2.2.5 客户端的工具集与系统提示词资产。
//
// 两份资产逐字节取自 2026-11-04 对**本机官方客户端**的抓包（本地 HTTP 桩 +
// `OPENCODE_CONFIG_CONTENT` 覆盖 provider baseURL，原始请求见
// <项目>\data\oczen-capture-20261104\req-003.json），**不得在本文件里手改**：
//   - assets/opencode_tools_2.2.5.json  官方 12 个工具（顺序/描述/parameters/strict 全同源）
//   - assets/opencode_prompt_2.2.5.txt  官方 2.2.5 的 system 正文（8 个占位符待运行期填充）
//
// 为什么换掉 R152 的资产：R152 用的是参考实现 opencode2api v1.4.6 的快照，属
// 1.x/2.0 形态——prompt 开头 `You are opencode, an interactive CLI tool`、13 个
// 工具（含官方 2.2.5 没有的 `patch`）。2026-11-04 实测：同一台机器、同一出口、
// 同一匿名免费档，官方 CLI 打 `opencode/big-pickle` **3.7 秒 200**，而本仓整形后的
// 请求恒 429。故以官方 2.2.5 抓包为准（R154）。
package oczen

import (
	_ "embed"
	"encoding/json"
	"runtime"
	"strings"
	"sync"
)

//go:embed assets/opencode_tools_2.2.5.json
var officialToolsJSON []byte

//go:embed assets/opencode_prompt_2.2.5.txt
var officialPromptTemplate string

// officialToolOrder 官方 2.2.5 抓包里的工具顺序。
// JSON 数组本身有序，这里显式列出便于断言与「追加顺序即指纹」的复核：
// 官方把 `execute`（Code Mode）放在**最末**，且**没有** `patch`。
var officialToolOrder = []string{
	"edit", "glob", "grep", "question", "read", "shell",
	"skill", "subagent", "webfetch", "websearch", "write", "execute",
}

// 官方 shell 工具描述里的宿主平台句（抓包本机为 Windows）。官方按宿主取词，
// 故其它平台替换成同义句，保证「工具描述 / `<env>` 的 Platform 行」讲同一个故事。
const (
	officialShellPhraseWindows = "Commands run on Windows using powershell."
	officialShellPhraseDarwin  = "Commands run on macOS using zsh."
	officialShellPhraseLinux   = "Commands run on Linux using bash."
)

var (
	officialToolsOnce sync.Once
	// officialToolDefs 官方工具定义，按 officialToolOrder 顺序。
	officialToolDefs []map[string]any
	// officialToolIndex 按工具名索引官方定义。
	officialToolIndex map[string]map[string]any
)

// loadOfficialTools 解析一次官方工具表。读不出来时退化成空表：名字仍会被追加
// （闸门先看名字与数量），只是描述与 schema 用占位。
func loadOfficialTools() ([]map[string]any, map[string]map[string]any) {
	officialToolsOnce.Do(func() {
		var parsed []map[string]any
		if err := json.Unmarshal(officialToolsJSON, &parsed); err != nil {
			parsed = nil
		}
		officialToolDefs = parsed
		officialToolIndex = make(map[string]map[string]any, len(parsed))
		for _, entry := range parsed {
			if name := chatToolName(entry); name != "" {
				officialToolIndex[name] = entry
			}
		}
	})
	return officialToolDefs, officialToolIndex
}

// chatToolName 取出一个 Chat 线工具条目里 `function.name`。
func chatToolName(entry map[string]any) string {
	fn, _ := entry["function"].(map[string]any)
	if fn == nil {
		return ""
	}
	name, _ := fn["name"].(string)
	return name
}

// officialShellDescription 把官方 shell 描述里的宿主平台句换成当前平台的同义句。
// 资产里存的是抓包时的 Windows 句，故三个已知句都先归一，再写回本机平台句——
// 这样换平台抓包资产也不会把别的平台的句子留下。
func officialShellDescription(base string) string {
	host := shellPhraseForHost()
	for _, phrase := range []string{officialShellPhraseWindows, officialShellPhraseDarwin, officialShellPhraseLinux} {
		if phrase != host {
			base = strings.ReplaceAll(base, phrase, host)
		}
	}
	return base
}

// officialTool 返回一个官方工具的**副本**（Chat 线 function 形状，各带 strict:false）。
//
// 描述与 parameters 逐字节取自 2.2.5 抓包；只有 shell 的宿主平台句按运行平台改写，
// 抓包本机是 Windows，故资产里存的也是 Windows 句，替换时三句都试一遍。
func officialTool(name string) map[string]any {
	_, index := loadOfficialTools()
	entry := index[name]
	if entry == nil {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": "Agent tool " + name,
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
				"strict":      false,
			},
		}
	}
	out := make(map[string]any, len(entry))
	for k, v := range entry {
		out[k] = v
	}
	fn, _ := entry["function"].(map[string]any)
	if fn != nil {
		next := make(map[string]any, len(fn))
		for k, v := range fn {
			next[k] = v
		}
		if name == "shell" {
			if desc, ok := next["description"].(string); ok {
				next["description"] = officialShellDescription(desc)
			}
		}
		out["function"] = next
	}
	return out
}

// shellPhraseForHost 返回本机平台对应的官方 shell 描述句。
func shellPhraseForHost() string {
	switch runtime.GOOS {
	case "windows":
		return officialShellPhraseWindows
	case "darwin":
		return officialShellPhraseDarwin
	}
	return officialShellPhraseLinux
}

// officialToolsetNames 返回官方工具名清单（只读副本，供测试与断言使用）。
func officialToolsetNames() []string {
	out := make([]string, len(officialToolOrder))
	copy(out, officialToolOrder)
	return out
}
