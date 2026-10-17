// oauthclient.go 运行时获取 Google OAuth 客户端的 client_id / client_secret。
//
// 为什么不在源码里内嵌（2026-10-17 变更）：
// 这两枚值原本内嵌在 constants.go —— 它们是 Google 官方客户端内嵌的**公开**凭据
// （开源客户端里就有，任何人可提取），但 GitHub 的 Secret Scanning Push Protection
// **只看格式、不看是否公开**，会把它们判定为 secret 并**拒绝推送整个分支**（GH013）。
// 改为运行时获取，顺带与 glm / loomy / monkeycode 的「导入型」渠道保持同一思路。
//
// 获取顺序：
//  1. 环境变量 `CMDC_PAK_GOOGLE_CLIENT_ID` / `CMDC_PAK_GOOGLE_CLIENT_SECRET`（自建 OAuth 客户端或手动指定）；
//  2. 本机安装的官方 CLI 包（npm 全局 / npx 缓存 / 项目内 node_modules / Homebrew，见 cliRoots）；
//  3. 都拿不到 → 空串，调用方给出可操作错误（login_gemini.ErrOAuthClientMissing 会列出**实际查过的位置**）。
package gemini

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
)

// 两枚公开值的**形态**。正则刻意拆成「字符类 + 片段」，不含任何完整可识别的 secret 值 ——
// 否则本文件自己又会被 push protection 拦下（那正是这次要修的问题）。
var (
	reOAuthClientID     = regexp.MustCompile(`[0-9]{10,}-[a-z0-9]{20,}\.apps\.googleusercontent\.com`)
	reOAuthClientSecret = regexp.MustCompile(`GOCSPX-[A-Za-z0-9_\-]{20,}`)
)

// localOAuthClient 解析结果。
type localOAuthClient struct {
	ID     string
	Secret string
	// SearchRoots 记录本次**实际考察过的**位置（含不存在的），供报错文案如实告知用户
	// 「到底查了哪里」。修复前只报「请设置环境变量」，用户照做仍然失败且无从排查。
	SearchRoots []string
}

var (
	oauthClientOnce sync.Once
	oauthClientVal  localOAuthClient
)

// oauthClient 解析一次并缓存（提取要扫目录，不能每个请求都做）。
//
// env 只给了一半时，用本机 CLI 补另一半：自建客户端理应两枚都给，但宽松处理比直接报错友好。
func oauthClient() localOAuthClient {
	oauthClientOnce.Do(func() {
		oauthClientVal = localOAuthClient{
			ID:     strings.TrimSpace(os.Getenv(EnvClientID)),
			Secret: strings.TrimSpace(os.Getenv(EnvClientSecret)),
		}
		if oauthClientVal.ID == "" || oauthClientVal.Secret == "" {
			roots := cliRoots()
			id, secret := extractOAuthClientFromRoots(roots)
			if oauthClientVal.ID == "" {
				oauthClientVal.ID = id
			}
			if oauthClientVal.Secret == "" {
				oauthClientVal.Secret = secret
			}
			oauthClientVal.SearchRoots = roots
		}
	})
	return oauthClientVal
}

// ResetOAuthClientCacheForTest 清掉进程级解析缓存，让下一次 ClientID()/ClientSecret() 重新解析。
//
// **仅供测试**：解析结果被 sync.Once 缓存、进程内只算一次；跨包（login_gemini 的用例）
// 需要"按当时的 env 重新解析"时这是唯一手段。生产路径**不得**调用——那会让每次请求都扫一遍磁盘。
func ResetOAuthClientCacheForTest() {
	oauthClientOnce = sync.Once{}
	oauthClientVal = localOAuthClient{}
}

// OAuthClientSearchRoots 返回本机考察过的 CLI 位置（未取到凭据时用于报错文案）。
//
// 只暴露**路径**，不含任何凭据值：调用方可安全地把它拼进用户可见的错误文案。
func OAuthClientSearchRoots() []string {
	roots := oauthClient().SearchRoots
	out := make([]string, len(roots))
	copy(out, roots)
	return out
}

// oauthCLIPackageDirs 官方（及前后代）CLI 的**包目录名**，相对某个 node_modules。
//
// 为什么不止 `@google/gemini-cli`：
//   - `@google/antigravity`：2026-06-18 起官方 Gemini CLI 已被 Antigravity CLI 取代
//     （geminicli.com/docs/changelogs 原文「Gemini CLI was replaced by Antigravity CLI」）。
//     本渠道的身份头（`antigravity/4.3.0 (cmdc-pak)`）、端点（daily-cloudcode-pa）本就同源，
//     故新命令装出来的包才是**当前**最可能带这两枚值的本地来源。
//   - `gemini-cli`（无 scope）：社区 fork / 部分发行版的分发名，多扫一个目录代价可忽略。
var oauthCLIPackageDirs = [][]string{
	{"@google", "gemini-cli"},
	{"@google", "gemini-cli-core"},
	{"@google", "antigravity"},
	{"gemini-cli"},
}

// appendPackageRoots 把 base/node_modules/<pkg…> 逐个追加到 roots。
func appendPackageRoots(roots []string, base string) []string {
	if strings.TrimSpace(base) == "" {
		return roots
	}
	for _, pkg := range oauthCLIPackageDirs {
		roots = append(roots, filepath.Join(append([]string{base}, pkg...)...))
	}
	return roots
}

// cliRoots 官方 CLI 的常见安装位置。
//
// 只列**明确属于该 CLI**的目录：不扫整个 node_modules（动辄几十万文件，代价不可接受）。
//
// 2026-10-17 现场修复（逐条都有本机实测证据）：
//   - **Windows 的 npx 缓存不在 `~/.npm/_npx`**：npm 10+ 在 Windows 上用
//     `%LOCALAPPDATA%\npm-cache\_npx`（本机 `~/.npm` 根本不存在，而前者有 6568 个文件）。
//     旧代码只查后者 ⇒ Windows 上「npx 跑过的 CLI」这条来源等于白写。
//   - 补 `%LOCALAPPDATA%\npm\node_modules`：npm 在部分 Windows 配置下把全局前缀放在 LocalAppData
//     （与 Roaming 的 `%APPDATA%\npm` 并存），漏掉就查不到已装的 CLI。
//   - **Homebrew**：macOS 上 `brew install` 的 npm 全局前缀是 `/opt/homebrew/lib/node_modules`
//     （Apple Silicon）或 `/usr/local/lib/node_modules`（Intel），旧代码完全没覆盖。
//   - 包目录名见 oauthCLIPackageDirs（官方已换成 @google/antigravity）。
//
// 顺序即优先级：GEMINI_CLI_HOME（用户显式指定）> 全局安装 > npx 缓存 > 其它前缀。
func cliRoots() []string {
	var roots []string
	if v := strings.TrimSpace(os.Getenv("GEMINI_CLI_HOME")); v != "" {
		roots = append(roots, v)
	}

	// ---- 全局安装前缀（Windows 上 Roaming 与 Local 两种都可能）----
	if appdata := strings.TrimSpace(os.Getenv("APPDATA")); appdata != "" {
		roots = appendPackageRoots(roots, filepath.Join(appdata, "npm", "node_modules"))
	}
	if localApp := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); localApp != "" &&
		runtime.GOOS == "windows" {
		roots = appendPackageRoots(roots, filepath.Join(localApp, "npm", "node_modules"))
		// npm 10+ 的 npx 缓存真实位置（见上方说明）。
		roots = append(roots, filepath.Join(localApp, "npm-cache", "_npx"))
	}

	if home, err := os.UserHomeDir(); err == nil && home != "" {
		// 用户级全局前缀。
		roots = appendPackageRoots(roots, filepath.Join(home, ".npm-global", "lib", "node_modules"))
		roots = appendPackageRoots(roots, filepath.Join(home, ".local", "share", "npm", "node_modules"))
		// 项目内 / 家目录直装的 node_modules。
		roots = appendPackageRoots(roots, filepath.Join(home, "node_modules"))
		// npx 缓存：里面是 `_npx/<hash>/node_modules/@google/<pkg>`，多版本并存，整体下探。
		roots = append(roots, filepath.Join(home, ".npm", "_npx"))
	}

	// ---- 非 Windows 的 Homebrew / 系统级前缀 ----
	if runtime.GOOS != "windows" {
		roots = appendPackageRoots(roots, "/opt/homebrew/lib/node_modules")
		roots = appendPackageRoots(roots, "/usr/local/lib/node_modules")
		roots = appendPackageRoots(roots, "/usr/lib/node_modules")
	}

	return dedupeRoots(roots)
}

// dedupeRoots 去重并去掉空串（同一路径可能被多个前缀命中，重复扫描纯属浪费）。
func dedupeRoots(roots []string) []string {
	seen := make(map[string]struct{}, len(roots))
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		key := strings.ToLower(filepath.Clean(r))
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, r)
	}
	return out
}

// 扫描硬上限（每个 root 独立计）：
const (
	// scanMaxFiles 单个 root 最多读多少个文件。
	scanMaxFiles = 4000
	// scanMaxBytes 单个文件最大字节数（超过即跳过，不读进内存）。
	scanMaxBytes = 8 << 20
	// scanByteBudget 单个 root 的总读取字节预算（防止"4000 个 8MB 文件"把内存吃光）。
	scanByteBudget = 256 << 20
)

// extractOAuthClientFromLocalCLI 在本机 CLI 包里找这两枚值；找不到（或目录不存在）返回空串。
func extractOAuthClientFromLocalCLI() (id, secret string) {
	return extractOAuthClientFromRoots(cliRoots())
}

// extractOAuthClientFromRoots 在给定的候选根目录里找这两枚值。
//
// 扫描有硬上限：文件数、单文件字节数、总字节预算、扩展名白名单 —— 避免在意外巨大的目录上
// 卡住主流程或吃光内存。上限按 **root 独立计**：某个 root 特别大时，不该牺牲后续 root 的机会。
func extractOAuthClientFromRoots(roots []string) (id, secret string) {
	for _, root := range roots {
		if id != "" && secret != "" {
			return id, secret
		}
		if _, err := os.Stat(root); err != nil {
			continue // 未安装 / 无权限：跳过，不当错误
		}
		files, budget := 0, scanByteBudget
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "test", "tests", "docs", "coverage", "__tests__", "node_modules":
					// ⚠️ 只跳过 root **之下**的同名目录：root 自身可能就叫 node_modules
					// （GEMINI_CLI_HOME 指向某个 node_modules、或 %APPDATA%\npm\node_modules 这类前缀），
					// 那时跳过等于把整棵树都跳过。判据用相对深度，不用名字。
					if p == root {
						return nil
					}
					// 嵌套的 node_modules 要跳过：CLI 包自身依赖成百上千个包，真正的值只在
					// 自己的 dist/ 里；扁平化的重复目录只会白白吃掉文件数与字节预算。
					return fs.SkipDir
				}
				return nil
			}
			if files >= scanMaxFiles || budget <= 0 {
				return fs.SkipAll
			}
			switch strings.ToLower(filepath.Ext(p)) {
			// .ts 必须在白名单里：官方 CLI 的部分产物（types / 未打包的分发）以 .ts 落盘。
			// 它不是"顺手多扫一种"，而是本机假 CLI 用例实测出来的真实缺口。
			case ".js", ".mjs", ".cjs", ".json", ".ts":
			default:
				return nil
			}
			fi, err := d.Info()
			if err != nil || fi.Size() > scanMaxBytes || fi.Size() > int64(budget) {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			files++
			budget -= len(b)
			s := string(b)
			if id == "" {
				id = reOAuthClientID.FindString(s)
			}
			if secret == "" {
				secret = reOAuthClientSecret.FindString(s)
			}
			if id != "" && secret != "" {
				return fs.SkipAll
			}
			return nil
		})
	}
	return id, secret
}
