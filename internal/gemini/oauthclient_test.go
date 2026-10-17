package gemini

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// testClientID / testClientSecret 测试进程用的**假**凭据。
//
// 刻意不匹配真实形态（client_id 缺 `[a-z0-9]{20,}` 段、secret 不带 `GOCSPX-` 前缀）——
// 否则**测试文件本身**又会被 GitHub 的 push protection 拦下（那正是本次要修的问题）。
const (
	testClientID     = "0000000000-testclient.apps.googleusercontent.com"
	testClientSecret = "test-placeholder-secret-not-real"
)

// TestMain 给测试进程注入假凭据。
//
// 为什么需要：源码已不再内嵌那两枚公开凭据（push protection 会拒绝推送整个分支），
// 于是干净环境里 ClientID()/ClientSecret() 返回空串 —— 那会让所有「构造 token 表单」的测试失败。
// 真值只在实际运行时从环境变量或本机官方 CLI 获取。
func TestMain(m *testing.M) {
	os.Setenv(EnvClientID, testClientID)
	os.Setenv(EnvClientSecret, testClientSecret)
	// 重置解析缓存：sync.Once 在进程内只解析一次，不重置就取不到这里设的值。
	oauthClientOnce = sync.Once{}
	oauthClientVal = localOAuthClient{}
	os.Exit(m.Run())
}

// resetOAuthClientCache 清掉 sync.Once 缓存，让下一次 oauthClient() 按**当时的 env** 重新解析。
//
// ⚠️ 这不是可有可无的卫生措施，而是**顺序依赖的唯一解法**：解析结果进程级只算一次，
// 任何一个用例把自己的 env（尤其是"空凭据"）钉进缓存，后续用例都会拿到别人的结果。
// 实测踩过两次：`TestExtractFromLocalCLI` 先跑会留下一个"空 CLI 结果 + env 值"的混合缓存，
// 让 `TestOAuthClientFallsBackToLocalCLI` 偶尔失败。
func resetOAuthClientCache(t *testing.T) {
	t.Helper()
	oauthClientOnce = sync.Once{}
	oauthClientVal = localOAuthClient{}
	t.Cleanup(func() {
		oauthClientOnce = sync.Once{}
		oauthClientVal = localOAuthClient{}
	})
}

// TestNoEmbeddedOAuthValueInSource 守门（2026-10-17）：本包**非测试**源码里不得再出现
// 那两枚 Google OAuth 公开凭据的完整字面量。
//
// 为什么值得一个测试：它们是**公开**值（官方 CLI 内嵌），但 GitHub 的 Secret Scanning
// Push Protection 只看格式，源码里一旦有完整值就会**拒绝推送整个分支**（GH013）。
// 这次踩过一次，用测试钉住，避免以后有人"为了省事"再贴回来。
func TestNoEmbeddedOAuthValueInSource(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("读包目录: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s: %v", name, err)
		}
		if reOAuthClientID.Match(b) {
			t.Errorf("%s 内嵌了完整的 OAuth client_id 字面量 —— push protection 会拒绝推送（GH013）", name)
		}
		if reOAuthClientSecret.Match(b) {
			t.Errorf("%s 内嵌了完整的 OAuth client_secret 字面量 —— push protection 会拒绝推送（GH013）", name)
		}
	}
}

// TestOAuthClientFromEnv 守门优先级：环境变量必须**优先于**本机 CLI 提取（自建客户端场景）。
func TestOAuthClientFromEnv(t *testing.T) {
	const (
		wantID     = "1111111111-fromenv.apps.googleusercontent.com"
		wantSecret = "from-env-secret-value"
	)
	t.Setenv(EnvClientID, wantID)
	t.Setenv(EnvClientSecret, wantSecret)
	oauthClientOnce = sync.Once{}
	oauthClientVal = localOAuthClient{}

	got := oauthClient()
	if got.ID != wantID {
		t.Errorf("ID = %q，期望取环境变量值", got.ID)
	}
	if got.Secret != wantSecret {
		t.Errorf("Secret = %q，期望取环境变量值", got.Secret)
	}
}

// TestExtractFromLocalCLI 边界：目录不存在 / 无匹配时**不得 panic、不得返回垃圾**。
// （本机没装 Gemini CLI 走的就是这条路径：结果为空串，由调用方给出可操作错误。）
func TestExtractFromLocalCLI(t *testing.T) {
	t.Setenv("GEMINI_CLI_HOME", filepath.Join(t.TempDir(), "definitely-not-here"))
	id, secret := extractOAuthClientFromLocalCLI()
	if id != "" || secret != "" {
		t.Errorf("不存在的目录应返回空串，实际 id=%q secret=%q", id, secret)
	}
}
