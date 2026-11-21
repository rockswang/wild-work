package app

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// 本测试锁定一个**真实 bug**（2026-11-15 发现，豆包渠道接入时）：
//
//	在 main.go 的启动路径加了 `auth.LoadDoubaoDir` + `dbPool.Add`，
//	却漏了 `reloadAccounts()` 里的对应分支 ——
//	后果是「日志明明打了『doubao 账号已添加』，面板里就是不出现」。
//	账号落盘了、凭据是好的，但运行中的进程永远看不到它，直到重启。
//
// 这与 R34「新增渠道漏 `go xxxSch.Run`」是同一族疏漏：**静态可判定、运行时不报错**。
// 故用同样的静态检查兜住：reloadAccounts 必须覆盖所有「有 auth 目录的内建渠道」。
//
// 排除项（有明确理由，不是漏）：
//   - Oczen / Kilo：匿名/免密虚拟账号，由 main.go 装配时注入，没有 auth 目录；
//   - TraeCode：TraeWork 的**别名渠道**（同一批 auth 文件），没有自己的目录；
//   - 自定义渠道：凭据在 config.json 里，不走 auths/ 目录。
func TestReloadAccountsCoversAllAuthDirChannels(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("读 app.go 失败: %v", err)
	}
	code := string(src)

	// 只取 reloadAccounts 函数体（避免命中文件里其它地方的 provider.X 引用）。
	start := strings.Index(code, "func (a *App) reloadAccounts(")
	if start < 0 {
		t.Fatal("未找到 reloadAccounts 函数（重构了？本测试需同步更新）")
	}
	// 函数体到下一个顶层 func 为止（粗暴但足够：本文件函数都是顶层定义）。
	rest := code[start:]
	if end := strings.Index(rest, "\nfunc "); end > 0 {
		rest = rest[:end]
	}
	body := rest

	covered := map[string]bool{}
	for _, m := range regexp.MustCompile(`provider\.([A-Za-z]+)`).FindAllStringSubmatch(body, -1) {
		covered[m[1]] = true
	}

	// 键用 Kind.String()（小写），与下面的 name 同口径。
	excluded := map[string]bool{
		"oczen":    true, // 匿名虚拟账号，main 装配时注入
		"kilo":     true, // 免密免费池，无账号
		"traecode": true, // TraeWork 的别名渠道，共用同一批 auth 文件
	}

	// 遍历 DisplayNames 的键（Kind 值）——它是「内建渠道全集」的权威来源之一，
	// 与 builtinKindList 同源（见 provider.go 注释）。
	var missing []string
	for kind := range provider.DisplayNames {
		name := kind.String()
		if excluded[name] {
			continue
		}
		if !covered[identOf(kind)] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("reloadAccounts 漏了这些渠道（新增渠道必须在此加载，否则面板看不到账号）：%v", missing)
	}
}

// identOf Kind 的 Go 标识名（provider.Doubao → "Doubao"）。
//
// 为什么需要：BuiltinKinds()/DisplayNames 的键是 Kind（值是 String()），
// 而 reloadAccounts 里写的是 Go 标识名（provider.Doubao）。两者需要一座桥。
func identOf(k provider.Kind) string {
	// Kind 的 String() 与标识名的映射不在 provider 包导出，故用一张显式表兜底
	// （新增渠道时加一行；漏加会由上面的 missing 报出来，不会静默）。
	switch k.String() {
	case "workbuddy":
		return "WorkBuddy"
	case "workbuddyai":
		return "WorkBuddyAI"
	case "traework":
		return "TraeWork"
	case "traecode":
		return "TraeCode"
	case "qoder":
		return "Qoder"
	case "qodercn":
		return "QoderCN"
	case "qodercom":
		return "QoderCOM"
	case "qwenwork":
		return "QwenWork"
	case "glm":
		return "GLM"
	case "monkeycode":
		return "MonkeyCode"
	case "raccoon":
		return "Raccoon"
	case "loomy":
		return "Loomy"
	case "oczen":
		return "Oczen"
	case "kilo":
		return "Kilo"
	case "minimax":
		return "Minimax"
	case "lobsterai":
		return "LobsterAI"
	case "zcode":
		return "ZCode"
	case "gemini":
		return "Gemini"
	case "codearts":
		return "CodeArts"
	case "cline":
		return "Cline"
	case "officeace":
		return "OfficeAce"
	case "comate":
		return "Comate"
	case "lingxi":
		return "Lingxi"
	case "doubao":
		return "Doubao"
	case "modelscope":
		return "ModelScope"
	case "modelscopeai":
		return "ModelScopeAI"
	case "chatgpt":
		return "ChatGPT"
	}
	return ""
}
