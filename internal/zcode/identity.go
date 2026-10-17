// identity.go ZCode 的**官方身份块**：`system` 里必须逐字内嵌的那段文本。
//
// # 这是准入门槛，不是业务提示词
//
// 上游对 `/zcode-plan/anthropic` 通道做**请求体内容检查**：顶层 `system` 缺少官方身份块
// 结构时直接回 `HTTP 405` + `{"code":3012,"msg":"request has been blocked due to unusual
// activity."}`（协议 §1.2.4 的实测矩阵）。判据与 HTTP 头、运行时（Electron/curl/Node）、
// 版本头、请求频率、多轮历史、`tools` 声明**都无关**——唯一的开关就是这段文本。
//
//	| system 内容                          | 字符数 | 结果          |
//	|--------------------------------------|--------|---------------|
//	| 无 system                            | 0      | ✗ 405 + 3012  |
//	| 仅 cliPrefix                         | 42     | ✗ 405 + 3012  |
//	| cliPrefix + stable（全部三段）        | 2898   | ✓ 200         |
//	| 完整四块（含 dynamic 段）             | 7599   | ✓ 200         |
//
// ⇒ 本包取「cliPrefix + stable + # Environment 段（+ 调用方 system）」这一最小充分形态：
// 官方完整身份块还含约 5KB 的 dynamic 段（`# Communicating with the user` /
// `# Context management`），那些是**给 ZCode 内建 coding agent 的行为指令**，与准入无关，
// 且会被放在 system 开头**压过调用方（本仓上层）自己的 prompt**，表现为「啰嗦、慢」，
// 故**不含** dynamic 段。
//
// # ⚠ 维护警告（来自参考实现，必须照抄）
//
// 3012 **有账号冷却惩罚**：30 分钟起步 → 24 小时内第 3 次起 24 小时 → 5 次停用。
// 因此**绝不要为了调试反复触发它**；也正因如此，本包在发请求前做一次
// **本地前置校验**（verifyIdentity / verifyOutboundIdentity）：一旦即将发出的 system
// 不含身份块（例如将来有人改了常量、或上层改写了 system 结构），本地就直接拦下并
// 如实标注「上游同条件会回 405 + 3012」，**不拿健康账号去撞风控**。
//
// 身份块文本本身在 identity_text.go（一次性程序化提取自参考实现，**不要手工编辑**）。
package zcode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// 身份块的长度自检值（协议 §1.2.4 的实测矩阵）。
//
// 为什么要把字符数写进代码：它是**准入判据的指纹**——文本被任何工具（格式化、
// 编辑器换行、复制粘贴丢掉一个空格）改动一个字符，长度就会变，于是本包的
// 前置校验会立刻拦下，而不是等到上游回 3012 并给账号记一次风控。
const (
	identityCliRunes    = 42   // cliPrefix
	identityStableRunes = 2856 // stable 三段用 "\n\n" 连接
	identityTotalRunes  = 2898 // cliPrefix + stable
)

// envInfo 构造 `# Environment` 段所需的运行环境信息。
type envInfo struct {
	// Cwd 主工作目录。官方实现声明「cwd is never "unknown" in real traffic」，故取真值。
	Cwd string
	// Provider 展示用的 provider id（默认 zcode）。
	Provider string
	// Model 模型名（进 `You are powered by the model named <provider>/<model>.`）。
	Model string
	// Platform / OSVersion 与身份头族保持同一口径（见 constants.go 的说明）。
	Platform  string
	OSVersion string
}

// officialStable 返回 stable 段（三段用 "\n\n" 连接，逐字复刻官方形态）。
func officialStable() string { return strings.Join(stableSections, "\n\n") }

// identitySystemFn 构造 system 块数组（含身份块）。
//
// 之所以是包级变量而不是直接调用：回归测试要能注入「缺身份块」的形态，
// 以验证 ChatStream 的**本地前置校验**真的会拦下请求（且一个包都没发出去）。
// **测试之外不得改写它。**
var identitySystemFn = buildIdentityBlocks

// buildIdentityBlocks 构造官方形态的 `system` 块数组（协议 §1.4）：
//
//	block[0]     = cliPrefix（42 字符）        ← 准入必需
//	block[1]     = stable（2856 字符）         ← 准入必需
//	block[2]     = "# Environment" 段
//	block[3..]   = 调用方 system（**追加在最后**）
//
// ⚠ 调用方内容必须追加在**最后**：上游检查的是前缀结构，官方身份块必须处在开头位置。
// ⚠ 只在**最后一块**打 prompt caching 断点（协议 §1.4 实测/推导）：Anthropic 的
// 缓存断点是**前缀式**的（一个位于末尾的断点覆盖面等于每块各打一个），且单请求断点
// 有数量上限（4 个）——每块都打会用满预算，导致 `tools` 再也打不了点。
func buildIdentityBlocks(callerSystem string, env envInfo) []any {
	blocks := []any{
		textBlock(cliPrefix),
		textBlock(officialStable()),
		textBlock(environmentSection(env)),
	}
	// 幂等：调用方自己已经带了官方身份块（例如上游客户端直连本工具）时不重复追加。
	if cs := strings.TrimSpace(callerSystem); cs != "" && !strings.Contains(cs, cliPrefix) {
		blocks = append(blocks, textBlock(callerSystem))
	}
	if m, ok := blocks[len(blocks)-1].(map[string]any); ok {
		m["cache_control"] = map[string]any{"type": "ephemeral"}
	}
	return blocks
}

// environmentSection 构造官方形态的 `# Environment` 段。
//
// 官方每个会话都会告诉模型它运行在什么环境里；不发它的可观测后果是模型无法正确回答
// 「你是什么模型 / 工作目录在哪 / 该给 bash 还是 PowerShell 命令」
// （协议 §1.2.4 的注释：参考实现记录过这个现象）。
//
// ⚠ 平台取值与身份头族（`X-Platform: win32` / `X-Os-Category: windows`）保持同一口径：
// 本仓对外统一以官方 win32 客户端形态出站，环境段若写别的平台就与头族自相矛盾。
func environmentSection(env envInfo) string {
	cwd := strings.TrimSpace(env.Cwd)
	if cwd == "" {
		cwd = "."
	}
	provider := strings.TrimSpace(env.Provider)
	if provider == "" {
		provider = "zcode"
	}
	model := strings.TrimSpace(env.Model)
	if model == "" {
		model = upstreamModelName(Kind.String() + "/glm-5.3-flash")
	}
	platform := strings.TrimSpace(env.Platform)
	if platform == "" {
		platform = Platform
	}
	shell := "bash"
	if platform == Platform {
		shell = "powershell"
	}
	osVersion := strings.TrimSpace(env.OSVersion)
	if osVersion == "" {
		osVersion = platform + " amd64"
	}
	gitRepo := "no"
	if _, err := os.Stat(filepath.Join(cwd, ".git")); err == nil {
		gitRepo = "yes"
	}
	return strings.Join([]string{
		"# Environment",
		"You have been invoked in the following environment:",
		" - Primary working directory: " + cwd,
		" - Is a git repository: " + gitRepo,
		" - Platform: " + platform,
		" - Shell: " + shell,
		" - OS Version: " + osVersion,
		" - You are powered by the model named " + provider + "/" + model + ".",
	}, "\n")
}

// environment 返回默认环境信息（cwd 取进程工作目录，取不到时用 "."）。
func environment(model string) envInfo {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = "."
	}
	return envInfo{
		Cwd:       cwd,
		Provider:  Kind.String(),
		Model:     model,
		Platform:  Platform,
		OSVersion: Platform + " amd64",
	}
}

// verifyIdentity 校验 top-level `system` 块数组里真的内嵌了官方身份块。
//
// 这是**本地准入门槛**：上游同条件会回 405 + code 3012，而 3012 会给账号记风控冷却，
// 所以「宁可本地失败，也不拿账号去撞」。
func verifyIdentity(blocks []any) error {
	if len(blocks) < 2 {
		return fmt.Errorf("system 只有 %d 块，缺官方身份块（上游同条件回 405 + code 3012）", len(blocks))
	}
	if got := blockText(blocks[0]); got != cliPrefix {
		return fmt.Errorf("system 第 1 块不是官方 cliPrefix（上游同条件回 405 + code 3012）")
	}
	stable := blockText(blocks[1])
	if stable != officialStable() {
		return fmt.Errorf("system 第 2 块与官方 stable 段不一致（上游同条件回 405 + code 3012）")
	}
	if n := utf8.RuneCountInString(cliPrefix + stable); n != identityTotalRunes {
		return fmt.Errorf("身份块字符数 %d ≠ 官方实测值 %d（上游同条件回 405 + code 3012）", n, identityTotalRunes)
	}
	return nil
}

// blockText 取文本块的 text 字段（形状不符时给空串）。
func blockText(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	return strOf(m["text"])
}

// verifyOutboundIdentity 在即将发出的请求体上执行身份块前置校验。
func verifyOutboundIdentity(body []byte) error {
	var probe struct {
		System []any `json:"system"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return fmt.Errorf("出站请求体不是合法 JSON：%w", err)
	}
	return verifyIdentity(probe.System)
}

// ---------------------------------------------------------------------------
// `<system-reminder>` 日期块
// ---------------------------------------------------------------------------

// contextPrefixIntro / contextPrefixOutro 逐字复刻官方形态（outro 前有 6 个空格缩进）。
const (
	contextPrefixIntro = "As you answer the user's questions, you can use the following context:"
	contextPrefixOutro = "      IMPORTANT: this context may or may not be relevant to your tasks. " +
		"You should not respond to this context unless it is highly relevant to your task."
)

// formatLocalISODate 本地时区的 ISO 日期（官方用**本地**日期，不是 UTC）。
func formatLocalISODate(t time.Time) string { return t.Format("2006-01-02") }

// contextPrefixBlock 构造 `<system-reminder>` 上下文块（日期块）。
//
// ⚠ 它是**照发但非准入判据**：参考实现早期称它是「3012 的最后一个开关」，
// 2026-10-03 实测已推翻（去掉它、带完整身份块的请求照样 200）。仍照发是因为官方如此、
// 且零成本；**不要**据此把「缺日期块 → 一定 3012」写成结论。
func contextPrefixBlock(now time.Time) map[string]any {
	body := strings.Join([]string{
		contextPrefixIntro,
		"# currentDate\nToday's date is " + formatLocalISODate(now) + ".",
		"",
		contextPrefixOutro,
	}, "\n")
	return textBlock("<system-reminder>" + body + "</system-reminder>")
}

// withContextPrefix 给**首轮 user 消息**的 content 数组最前面插入日期块（幂等）。
//
// 规则（官方行为，逐条照抄）：
//   - 只处理第一条消息，且它必须是 user 角色；
//   - 已以 `<system-reminder>` 开头则不重复插（幂等）；
//   - 纯文本 content 会被转成**块数组**（官方就是数组形态）——不是拼进文本字符串。
func withContextPrefix(msgs []any, now time.Time) []any {
	if len(msgs) == 0 {
		return msgs
	}
	first, ok := msgs[0].(map[string]any)
	if !ok || strOf(first["role"]) != "user" {
		return msgs
	}
	blocks, _ := first["content"].([]any)
	if len(blocks) > 0 && strings.HasPrefix(blockText(blocks[0]), "<system-reminder>") {
		return msgs
	}
	out := make([]any, 0, len(blocks)+1)
	out = append(out, contextPrefixBlock(now))
	out = append(out, blocks...)
	first["content"] = out
	return msgs
}
