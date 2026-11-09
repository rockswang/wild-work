package upstream

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// snapshotTables 保存并恢复三张表，避免 Load 的追加结果污染同包其他用例
// （逐字节快照与 behavior/parity 用例都依赖内置表的原样内容与顺序）。
func snapshotTables(t *testing.T) {
	t.Helper()
	features := append([]string(nil), sanitizeFeatures...)
	rewrites := append([][2]string(nil), sanitizeStringRewrites...)
	regexps := append([]regexpRewriteStruct(nil), sanitizeRegexpRewrites...)
	t.Cleanup(func() {
		sanitizeFeatures = features
		sanitizeStringRewrites = rewrites
		sanitizeRegexpRewrites = regexps
	})
}

func writeSanitizeTOML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sanitize.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Load 只读本包段、按顺序追加三张表，且追加后的规则经 sanitizeText 真正生效。
func TestLoadSanitizeAppendsRulesAndTakesEffect(t *testing.T) {
	snapshotTables(t)
	path := writeSanitizeTOML(t, `
["internal/upstream/sanitize.go"]
sanitizeFeatures = ['custom fingerprint alpha']
sanitizeStringRewrites = [['deepseek bad alpha', 'safe alpha']]
sanitizeRegexpRewrites = [['(?i)deepseek\s+bad\s+beta', 'safe beta']]

["internal/sanitize/sanitize.go"]
sanitizeFeatures = ['prod only feature']
`)
	nFeatures, nRewrites, nRegexps := len(sanitizeFeatures), len(sanitizeStringRewrites), len(sanitizeRegexpRewrites)
	Load(path)

	if len(sanitizeFeatures) != nFeatures+1 || sanitizeFeatures[nFeatures] != "custom fingerprint alpha" {
		t.Fatalf("sanitizeFeatures 追加失败: %v", sanitizeFeatures[nFeatures:])
	}
	if len(sanitizeStringRewrites) != nRewrites+1 ||
		sanitizeStringRewrites[nRewrites] != [2]string{"deepseek bad alpha", "safe alpha"} {
		t.Fatalf("sanitizeStringRewrites 追加失败: %v", sanitizeStringRewrites[nRewrites:])
	}
	if len(sanitizeRegexpRewrites) != nRegexps+1 {
		t.Fatalf("sanitizeRegexpRewrites 条数=%d want %d", len(sanitizeRegexpRewrites), nRegexps+1)
	}
	if got := sanitizeRegexpRewrites[nRegexps]; got.regexp.String() != `(?i)deepseek\s+bad\s+beta` || got.replace != "safe beta" {
		t.Fatalf("sanitizeRegexpRewrites 追加结果=%q→%q", got.regexp.String(), got.replace)
	}
	if strings.Contains(strings.Join(sanitizeFeatures, "|"), "prod only feature") {
		t.Fatal("读到了另一段（internal/sanitize）的条目，段隔离失效")
	}

	in := "custom fingerprint alpha: deepseek bad alpha, deepseek  bad  beta"
	out := sanitizeText(in)
	if !strings.Contains(out, "safe alpha") || !strings.Contains(out, "safe beta") {
		t.Fatalf("自定义规则未生效: %q", out)
	}
}

// enabled=false 只跳过本段自定义规则加载：内置规则照常生效。
func TestLoadDisabledSkipsCustomRules(t *testing.T) {
	snapshotTables(t)
	path := writeSanitizeTOML(t, `
["internal/upstream/sanitize.go"]
enabled = false
sanitizeFeatures = ['custom fingerprint alpha']
sanitizeStringRewrites = [['custom bad alpha', 'custom safe alpha']]
`)
	nFeatures := len(sanitizeFeatures)
	Load(path)

	if len(sanitizeFeatures) != nFeatures {
		t.Fatalf("enabled=false 时不应追加自定义规则: %v", sanitizeFeatures[nFeatures:])
	}
	// 自定义规则未加载 ⇒ 只有它能触发的文本不被改写。
	in := "custom fingerprint alpha: custom bad alpha"
	if out := sanitizeText(in); out != in {
		t.Fatalf("enabled=false 时自定义规则不应生效: %q", out)
	}
	// 内置规则不受影响。
	const fingerprint = "You are Claude Code, Anthropic's official CLI for Claude"
	obj := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": fingerprint},
	}}
	if !sanitizeMessagesInPlace(obj) {
		t.Fatal("enabled=false 不应关闭内置规则")
	}
	if got := obj["messages"].([]any)[0].(map[string]any)["content"]; got == fingerprint {
		t.Fatalf("内置脱敏未生效: %q", got)
	}
}

// 非法正则逐条跳过，合法条目照常追加。
func TestLoadSanitizeSkipsInvalidRegexp(t *testing.T) {
	snapshotTables(t)
	path := writeSanitizeTOML(t, `
["internal/upstream/sanitize.go"]
sanitizeRegexpRewrites = [['(?i)bad[', 'invalid regex never applied'], ['(?i)valid\s+pattern', 'valid regex repl']]
`)
	nRegexps := len(sanitizeRegexpRewrites)
	Load(path)

	if len(sanitizeRegexpRewrites) != nRegexps+1 {
		t.Fatalf("sanitizeRegexpRewrites 条数=%d want %d", len(sanitizeRegexpRewrites), nRegexps+1)
	}
	if got := sanitizeRegexpRewrites[nRegexps]; got.regexp.String() != `(?i)valid\s+pattern` || got.replace != "valid regex repl" {
		t.Fatalf("合法正则未追加: %q→%q", got.regexp.String(), got.replace)
	}
}
