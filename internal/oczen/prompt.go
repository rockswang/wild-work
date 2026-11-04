// prompt.go 官方 OpenCode 2.2.5 的 system 正文拼装与注入。
//
// 2026-11-04 本机抓包（官方 opencode-cli v2.2.5，匿名免费档）撸清了官方 system 的
// 真实构成与**顺序**：
//
//	base prompt（# Harness / # Communication / # Working in codebases / # Your Model /
//	# Code Mode / ## Code Mode tools …）
//	<available_skills>…</available_skills>
//	Today's date: Wed Nov 04 2026
//	Here is some useful information about the environment you are running in:
//	<env>
//	  Working directory: …
//	  Workspace root folder: …
//	  Is directory a git repo: no
//	  Platform: win32
//	  Prefer <tmp>\opencode over generic system temporary directories such as /tmp; …
//	</env>
//	When you create a worktree outside the current working directory and intend to use it
//	as your primary working directory, consider using `execute` to call
//	`tools.opencode.session_move` and make the worktree the session's working directory.
//
// 三条被本轮抓包**推翻**的旧结论（R152/R153 的错误前提）：
//   - 官方 2.2.5 的 prompt 开头是 `You are an AI agent running in OpenCode, a coding
//     agent harness.`，而**不是**资产里的 `You are opencode, an interactive CLI tool`
//     （R152 抄的是 opencode2api v1.4.6 的 1.x/2.0 快照）；
//   - `<env>` 块**不是**收尾段——块之后还有一句 worktree 说明（R153 据此把块挪到末尾，
//     方向反了）；
//   - 官方 prompt 含机型相关段（`# Your Model` 的模型名、`<available_skills>`、日期、
//     cwd），故上游**不可能**做全文字面比对；能判的只有形态族与关键标记。
//
// 因此本包不再自行拼「base + 自造 env 块 + 自造 skills」，而是**逐字节内嵌官方 2.2.5
// 正文**（assets/opencode_prompt_2.2.5.txt，8 个占位符），运行期只填真值：
// 模型名/模型 ID/日期/cwd/工作区根/是否 git 仓/平台/临时目录。
package oczen

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// officialPromptMarker 标记「system 已带官方 2.2.5 正文」，用于幂等：已带则不重复注入。
// 取官方正文的第一句（抓包逐字），比旧 marker 更能区分版本族。
const officialPromptMarker = "You are an AI agent running in OpenCode"

var workDirOnce = sync.OnceValue(func() string {
	dir, err := os.Getwd()
	if err != nil {
		return "/workspace"
	}
	dir = strings.TrimSpace(dir)
	if dir == "" || dir == "." {
		return "/workspace"
	}
	return dir
})

// workDir 返回官方 CLI 会写进 `<env>` 的工作目录（进程内解析一次）。
func workDir() string { return workDirOnce() }

// officialPlatform 返回官方 CLI 口径的平台名（Node 的 process.platform）。
func officialPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}

// officialTmpDir 返回官方环境块里点名的临时目录：系统临时目录下的 `opencode`
// （Windows 上是 `C:\Users\<用户>\AppData\Local\Temp\opencode`），与官方同口径。
func officialTmpDir() string {
	return filepath.Join(os.TempDir(), "opencode")
}

// environmentFacts 是环境块里要落盘探测的三项（工作目录、工作区根、是否 git 仓）。
// 工作目录在进程内固定，故只探测一次。
type environmentFacts struct {
	directory string
	root      string
	isRepo    bool
}

var environmentOnce = sync.OnceValue(func() environmentFacts {
	dir := workDir()
	for probe := dir; ; {
		if _, err := os.Stat(filepath.Join(probe, ".git")); err == nil {
			return environmentFacts{directory: dir, root: probe, isRepo: true}
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	return environmentFacts{directory: dir, root: dir}
})

// officialModelID 把请求体里的模型名归一成官方口径的裸模型 ID
// （去掉本仓的 `渠道/` 前缀；官方 body 里就是裸 ID，如 `big-pickle`）。
func officialModelID(model string) string {
	model = strings.TrimSpace(model)
	if i := strings.LastIndex(model, "/"); i >= 0 {
		model = model[i+1:]
	}
	if model == "" {
		return "unknown"
	}
	return model
}

// officialModelName 把模型 ID 渲染成官方 `# Your Model` 里那种人类可读名
// （`big-pickle` → `Big Pickle`、`mimo-v2.6-flash-free` → `Mimo V2.6 Flash Free`）。
// 官方对每个模型有专门显示名，本仓按 ID 以 `-`/`_` 分词后逐词首字母大写派生
// （点号留在词内，避免 `v2.6` 被拆成两段）。
func officialModelName(modelID string) string {
	if modelID == "" {
		return "Unknown"
	}
	parts := strings.FieldsFunc(modelID, func(r rune) bool { return r == '-' || r == '_' })
	for i, part := range parts {
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, " ")
}

// officialDateFormat 官方 `Today's date:` 的格式（抓包原文 `Wed Nov 04 2026`）。
const officialDateFormat = "Mon Jan 02 2006"

// officialSystemPrompt 渲染官方 2.2.5 的 system 正文（含 `<env>` 块与块后的 worktree 句）。
//
// 模板来自 assets/opencode_prompt_2.2.5.txt，8 个占位符各出现一次；缺任何占位符
// 都不会报错（ReplaceAll 原样保留），但回归测试会断言全部被替换。
func officialSystemPrompt(model string) string {
	tpl := officialPromptTemplate
	if tpl == "" {
		return ""
	}
	facts := environmentOnce()
	git := "no"
	if facts.isRepo {
		git = "yes"
	}
	modelID := officialModelID(model)
	replacements := []string{
		"{{MODEL_NAME}}", officialModelName(modelID),
		"{{MODEL_ID}}", modelID,
		"{{DATE}}", time.Now().Format(officialDateFormat),
		"{{CWD}}", facts.directory,
		"{{ROOT}}", facts.root,
		"{{GIT}}", git,
		"{{PLATFORM}}", officialPlatform(),
		"{{TMPDIR}}", officialTmpDir(),
	}
	return strings.NewReplacer(replacements...).Replace(tpl)
}

// textFragments 把一个 content 值摊平成文本片段（字符串 / 分片数组 / 分片对象）。
func textFragments(value any) []string {
	switch v := value.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			out = append(out, textFragments(item)...)
		}
		return out
	case map[string]any:
		if text, ok := v["text"].(string); ok {
			return textFragments(text)
		}
		if content, ok := v["content"]; ok {
			return textFragments(content)
		}
	}
	return nil
}

// carriesOfficialPrompt 报告一段 content 里是否已经含有官方 2.2.5 正文。
func carriesOfficialPrompt(value any) bool {
	for _, part := range textFragments(value) {
		if strings.Contains(part, officialPromptMarker) {
			return true
		}
	}
	return false
}

// injectOfficialPrompt 把官方 system 正文前置进第一条 system/developer 消息。
//
// 官方正文**整体连续**（含 env 块与其后的 worktree 句），因此只在它后面接调用方文本，
// 不再把 env 块单独挪到末尾（R153 的反向修法已被抓包推翻）。已带 marker 的请求体
// 原样不动（重复整形不叠加）；没有 system 消息时在最前面补一条。**保留调用方的分片
// 与 cache_control 标记**：分片内容只在其前面插一个新的 text 分片。
func injectOfficialPrompt(obj map[string]any) bool {
	if obj == nil {
		return false
	}
	model, _ := obj["model"].(string)
	prompt := officialSystemPrompt(model)
	if prompt == "" {
		return false
	}
	messages, _ := obj["messages"].([]any)
	anchor := -1
	for i, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "system" && role != "developer" {
			continue
		}
		if carriesOfficialPrompt(msg["content"]) {
			return false
		}
		if anchor == -1 {
			anchor = i
		}
	}
	if anchor == -1 {
		obj["messages"] = append([]any{map[string]any{"role": "system", "content": prompt}}, messages...)
		return true
	}
	target := messages[anchor].(map[string]any)
	switch content := target["content"].(type) {
	case []any:
		target["content"] = append([]any{map[string]any{"type": "text", "text": prompt}}, content...)
	case string:
		if strings.TrimSpace(content) == "" {
			target["content"] = prompt
		} else {
			target["content"] = prompt + "\n\n" + content
		}
	default:
		target["content"] = prompt
	}
	return true
}
