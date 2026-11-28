// identity.go ZCode 的**官方身份块**：`system` 里必须逐字内嵌的那段文本。
//
// # 这是准入门槛，不是业务提示词
//
// 上游对 `/zcode-plan/anthropic` 通道做**请求体内容检查**：顶层 `system` 缺少官方身份块
// 结构时直接回 `HTTP 405` + `{"code":3012,"msg":"request has been blocked due to unusual
// activity."}`（协议 §1.2.4 的实测矩阵）。判据与 HTTP 头、运行时（Electron/curl/Node）、
// 版本头、请求频率、多轮历史、`tools` 声明**都无关**——唯一的开关就是这段文本。
//
//	| system 内容                       | 结果          |
//	|-----------------------------------|---------------|
//	| 无 system                         | ✗ 405 + 3012  |
//	| 仅 cliPrefix                      | ✗ 405 + 3012  |
//	| cliPrefix + stable               | ✓ 200         |
//	| 三块（cliPrefix + stable + 第3块合并 dynamic/caller）  | ✓ 200（2026-10-23 实测）|
//
// ⇒ 本包取「cliPrefix + stable + 官方动态段（调用方 system 并入末块）」这一形态，
// **恰好 3 块**（2026-10-23 修正，原为 4 块）：magpie #343 判定明确「ZCode 的三个
// system 块，把 agent 的 system 末尾追加进第 3 块；**加第 4 块被拒**」。
//
// # 版本锁（2026-11-03 起，必须与 4.0.0 同版原子修改）
//
// 上游对 start-plan 通道把「版本头 `X-ZCode-App-Version`」与「system 前两块原文」
// 绑成一对：头新于正文 → 405 + 3012；两维都旧（本仓 2026-11-28 之前的 3.14.3 头 +
// 3.11.2 文本）→ **HTTP 200 + 零内容块的软拒**，比 3012 更难排。
// 所以 `constants.go` 的 `AppVersionFallback` 与 `identity_text.go` 的文本
// **必须一起改**（当前配对 4.0.0），只改一个就会被上面任一条命中。
//
// 4.0.0 的形态变化：# Environment / poweredBy / desktopContext **移出 system**，
// 进首条 user 轮的 `<system-reminder>`（meta_user，见 contextPrefixMessage）；
// system 第 3 块只剩官方动态段（dynamicBehavior / [sessionGuidance] /
// contextManagement / finishingWork / memoryNotes）+ 调用方 system。
// 三块全部打 `cache_control:ephemeral`（官方 dress；连同末消息那一个点共 4 个，
// 恰为 Anthropic 的断点上限 ⇒ tools 不得再打点）。
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
	identityCliRunes    = 35   // cliPrefix（ZCode 4.0.0）
	identityStableRunes = 1229 // stable 单段（4.0.0）
	identityTotalRunes  = 1264 // cliPrefix + stable
)

// envInfo `# Environment` 段所需的运行环境信息。
//
// 4.0.0 起该段只进 meta_user（不进 system），网关不掌握下游 agent 的
// cwd/git/shell，故只保留平台与 OS 版本两行。
type envInfo struct {
	// Platform / OSVersion 与身份头族保持同一口径（见 constants.go 的说明）。
	Platform  string
	OSVersion string
}

// officialStable 返回 stable 段（多段用 "\n\n" 连接；4.0.0 起为单段）。
func officialStable() string { return strings.Join(stableSections, "\n\n") }

// identitySystemFn 构造 system 块数组（含身份块）。
//
// 之所以是包级变量而不是直接调用：回归测试要能注入「缺身份块」的形态，
// 以验证 ChatStream 的**本地前置校验**真的会拦下请求（且一个包都没发出去）。
// **测试之外不得改写它。**
var identitySystemFn = buildIdentityBlocks

// buildIdentityBlocks 构造官方形态的 system 块数组（ZCode 4.0.0，2026-11-28 跟进）:
//
//	block[0] = cliPrefix                                    ← 准入必需
//	block[1] = stable（单段）                                ← 准入必需
//	block[2] = "\n\n" + 动态段（dynamicBehavior / [sessionGuidance] /
//	           contextManagement / finishingWork / memoryNotes）+ 调用方 system
//
// ⚠ **必须恰好 3 块**（magpie #343 判定沿用）：调用方 system 只许并入末块，加第 4 块被拒。
// ⚠ **版本锁**（2026-11-03 起实测，zcode2api prompt.mjs 注释）：版本头
//
//	X-ZCode-App-Version 必须与前两块文本**同版**（当前配对 4.0.0）；头新于正文 → 405/3012。
//
// ⚠ 三块全部打 cache_control ephemeral（4.0.0 dress 同构）。
// ⚠ 4.0.0 起 # Environment / poweredBy / desktopContext **移出 system**，进首条 user 轮的
//
//	<system-reminder>（见 contextPrefixMessage / environmentText）。
func buildIdentityBlocks(callerSystem string, hasSkill bool) []any {
	caller := strings.TrimSpace(callerSystem)
	if caller != "" && !strings.Contains(caller, cliPrefix) {
		// 幂等：调用方自己已经带了官方身份块（例如上游客户端直连本工具）时不重复。
		caller = "\n\n" + caller
	} else {
		caller = ""
	}
	dynamic := []string{dynamicBehaviorText}
	if hasSkill && sessionGuidanceText != "" {
		dynamic = append(dynamic, sessionGuidanceText)
	}
	dynamic = append(dynamic, contextManagementText)
	for _, v := range []string{finishingWorkText, memoryNotesText} {
		if v != "" {
			dynamic = append(dynamic, v)
		}
	}
	blocks := []any{
		textBlock(cliPrefix),
		textBlock(officialStable()),
		textBlock("\n\n" + strings.Join(dynamic, "\n\n") + caller),
	}
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok {
			m["cache_control"] = map[string]any{"type": "ephemeral"}
		}
	}
	return blocks
}

// environmentText 构造 4.0.0 形态的 # Environment 段（首条 user 轮 meta_user 用）。
//
// 4.0.0 起该段**不再进 system**，官方把它放进首条 user 轮的 <system-reminder>。
// 网关沿用参考实现的 hub 极简形态：不掌握下游 agent 的 cwd/git/shell，省略这三行，
// 只保留 Platform / OS Version / untrustedNote；平台取值与身份头族（X-Platform: win32）
// 保持同一口径，不与头族自相矛盾。
func environmentText(env envInfo) string {
	platform := strings.TrimSpace(env.Platform)
	if platform == "" {
		platform = Platform
	}
	osVersion := strings.TrimSpace(env.OSVersion)
	if osVersion == "" {
		osVersion = platform + " amd64"
	}
	lines := []string{
		envHeading,
		envInvokedLine,
		" - " + envPlatformLabel + ": " + platform,
		" - " + envOSVersionLabel + ": " + osVersion,
	}
	if envUntrustedNote != "" {
		lines = append(lines, " - "+envUntrustedNote)
	}
	return strings.Join(lines, "\n")
}

// environment 返回默认环境信息。
func environment() envInfo {
	return envInfo{Platform: Platform, OSVersion: Platform + " amd64"}
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
// 首条 user 轮的 <system-reminder> 上下文（ZCode 4.0.0 meta_user 形态）
// ---------------------------------------------------------------------------

// formatLocalISODate 本地时区的 ISO 日期（官方用**本地**日期，不是 UTC）。
func formatLocalISODate(t time.Time) string { return t.Format("2006-01-02") }

// contextPrefixMessage 构造 4.0.0 的 meta_user 轮：**新起一条 user 消息**（不再是往
// 首条消息的 content 里插块），内容依序为 desktopContext / # Environment / poweredBy /
// 当天日期，整体包在 <system-reminder> 里（zcode2api apply_current_date_turn 同构，
// 对齐官方 4.0.0 dress）。poweredBy 的 provider 段按参考实现固定发 zai。
//
// ⚠ 它是**照发但非准入判据**（历史实测：去掉它、带完整身份块的请求照样 200）。
// 不要据此把「缺它 → 一定 3012」写成结论。
func contextPrefixMessage(env envInfo, model string, now time.Time) map[string]any {
	parts := make([]string, 0, 4)
	if desktopContextText != "" {
		parts = append(parts, desktopContextText)
	}
	parts = append(parts, environmentText(env))
	if model = strings.TrimSpace(model); model != "" {
		powered := strings.ReplaceAll(poweredByTemplate, "{provider}", "zai")
		powered = strings.ReplaceAll(powered, "{model}", model)
		if powered != "" {
			parts = append(parts, powered)
		}
	}
	parts = append(parts, strings.ReplaceAll(currentDateLineTemplate, "{date}", formatLocalISODate(now)))
	text := "<system-reminder>\n" + strings.Join(parts, "\n\n") + "\n</system-reminder>"
	return map[string]any{"role": "user", "content": []any{textBlock(text)}}
}

// withContextPrefix 在消息数组最前面补 meta_user 轮（幂等：首条已是 system-reminder
// 轮则原样返回）。4.0.0 起官方是**前置独立 user 消息**，不再插进首条消息内部。
func withContextPrefix(msgs []any, model string, now time.Time) []any {
	if len(msgs) > 0 {
		if first, ok := msgs[0].(map[string]any); ok && strOf(first["role"]) == "user" {
			if blocks, ok := first["content"].([]any); ok && len(blocks) > 0 && strings.HasPrefix(blockText(blocks[0]), "<system-reminder>") {
				return msgs
			}
		}
	}
	return append([]any{contextPrefixMessage(environment(), model, now)}, msgs...)
}
