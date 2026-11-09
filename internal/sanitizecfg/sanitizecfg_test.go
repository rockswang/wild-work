package sanitizecfg

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeTOML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sanitize.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// 段键与字段名一一对应：生产段读 sanitizeFeatures/sanitizeStringRewrites/sanitizeRegexpRewrites，
// 且不串到另一段。
func TestLoadProductionSection(t *testing.T) {
	path := writeTOML(t, `
["internal/sanitize/sanitize.go"]
sanitizeFeatures = ['prod feature alpha']
sanitizeStringRewrites = [['prod from alpha', 'prod to alpha']]
sanitizeRegexpRewrites = [['(?i)prod\s+bad', 'prod good']]

["internal/upstream/sanitize.go"]
sanitizeFeatures = ['upstream only feature']
`)
	rules, ok := Load(path, SanitizeSection)
	if !ok {
		t.Fatal("生产段应加载成功")
	}
	if len(rules.SanitizeFeatures) != 1 || rules.SanitizeFeatures[0] != "prod feature alpha" {
		t.Fatalf("Features=%v", rules.SanitizeFeatures)
	}
	if len(rules.SanitizeStringRewrites) != 1 || rules.SanitizeStringRewrites[0] != [2]string{"prod from alpha", "prod to alpha"} {
		t.Fatalf("StringRewrites=%v", rules.SanitizeStringRewrites)
	}
	if len(rules.SanitizeRegexpRewrites) != 1 || rules.SanitizeRegexpRewrites[0] != [2]string{`(?i)prod\s+bad`, "prod good"} {
		t.Fatalf("RegexpRewrites=%v", rules.SanitizeRegexpRewrites)
	}
	if strings.Contains(strings.Join(rules.SanitizeFeatures, "|"), "upstream only feature") {
		t.Fatal("读到了另一段的条目，段隔离失效")
	}
}

func TestLoadUpstreamSection(t *testing.T) {
	path := writeTOML(t, `
["internal/upstream/sanitize.go"]
sanitizeFeatures = ['upstream feature alpha']
sanitizeStringRewrites = [['upstream from alpha', 'upstream to alpha']]
sanitizeRegexpRewrites = [['(?i)upstream\s+bad', 'upstream good']]

["internal/sanitize/sanitize.go"]
sanitizeFeatures = ['prod only feature']
`)
	rules, ok := Load(path, UpstreamSection)
	if !ok {
		t.Fatal("upstream 段应加载成功")
	}
	if len(rules.SanitizeFeatures) != 1 || rules.SanitizeFeatures[0] != "upstream feature alpha" {
		t.Fatalf("Features=%v", rules.SanitizeFeatures)
	}
	if len(rules.SanitizeStringRewrites) != 1 ||
		rules.SanitizeStringRewrites[0] != [2]string{"upstream from alpha", "upstream to alpha"} {
		t.Fatalf("StringRewrites=%v", rules.SanitizeStringRewrites)
	}
	if len(rules.SanitizeRegexpRewrites) != 1 ||
		rules.SanitizeRegexpRewrites[0] != [2]string{`(?i)upstream\s+bad`, "upstream good"} {
		t.Fatalf("RegexpRewrites=%v", rules.SanitizeRegexpRewrites)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, ok := Load(filepath.Join(t.TempDir(), "not-exist.toml"), SanitizeSection); ok {
		t.Fatal("文件缺失应返回 ok=false")
	}
}

func TestLoadEmptyAndCommentOnly(t *testing.T) {
	for _, content := range []string{"", "   \n", "# 只有注释\n"} {
		rules, ok := Load(writeTOML(t, content), SanitizeSection)
		if strings.TrimSpace(content) == "" {
			if ok {
				t.Fatal("空文件应返回 ok=false")
			}
			continue
		}
		if !ok {
			t.Fatalf("纯注释文件应解析成功（空规则）: %q", content)
		}
		if len(rules.SanitizeFeatures)+len(rules.SanitizeStringRewrites)+len(rules.SanitizeRegexpRewrites) != 0 {
			t.Fatalf("纯注释文件不应产出规则: %+v", rules)
		}
	}
}

func TestLoadMalformed(t *testing.T) {
	if _, ok := Load(writeTOML(t, "sanitizeFeatures = [\n"), SanitizeSection); ok {
		t.Fatal("TOML 语法错误应返回 ok=false")
	}
}

func TestLoadUnknownSection(t *testing.T) {
	if _, ok := Load(writeTOML(t, `["x"]`), "x"); ok {
		t.Fatal("未知段应返回 ok=false")
	}
}

func TestLoadSkipsBadEntries(t *testing.T) {
	path := writeTOML(t, `
["internal/sanitize/sanitize.go"]
sanitizeFeatures = ['ok feature beta', '   ']
sanitizeStringRewrites = [['only-one'], ['ab', 'too-short-from'], ['ok from beta', ''], ['valid from beta', 'valid to beta']]
sanitizeRegexpRewrites = [['(?i)bad[', 'invalid regex'], ['(?i)valid\s+pattern', 'valid regex repl'], ['中文', 'two-rune-from']]
`)
	rules, ok := Load(path, SanitizeSection)
	if !ok {
		t.Fatal("坏条目应逐条跳过而不是整段失败")
	}
	if len(rules.SanitizeFeatures) != 1 || rules.SanitizeFeatures[0] != "ok feature beta" {
		t.Fatalf("Features=%v", rules.SanitizeFeatures)
	}
	if len(rules.SanitizeStringRewrites) != 1 || rules.SanitizeStringRewrites[0] != [2]string{"valid from beta", "valid to beta"} {
		t.Fatalf("StringRewrites=%v", rules.SanitizeStringRewrites)
	}
	// 正则编译由调用方负责：'(?i)bad[' 结构合法（长度 ≥4）会被保留，由 Load 侧编译失败后跳过；
	// '中文' 只有 2 个字符（6 字节），必须在这里按字符数拒绝。
	if len(rules.SanitizeRegexpRewrites) != 2 ||
		rules.SanitizeRegexpRewrites[0] != [2]string{`(?i)bad[`, "invalid regex"} ||
		rules.SanitizeRegexpRewrites[1] != [2]string{`(?i)valid\s+pattern`, "valid regex repl"} {
		t.Fatalf("RegexpRewrites=%v", rules.SanitizeRegexpRewrites)
	}
}

func TestLoadWrongValueTypes(t *testing.T) {
	path := writeTOML(t, `
["internal/sanitize/sanitize.go"]
sanitizeFeatures = 'not-an-array'
sanitizeStringRewrites = ['not-a-pair']
sanitizeRegexpRewrites = ['not-a-pair']
`)
	rules, ok := Load(path, SanitizeSection)
	if !ok {
		t.Fatal("错类型条目应逐条跳过")
	}
	if len(rules.SanitizeFeatures)+len(rules.SanitizeStringRewrites)+len(rules.SanitizeRegexpRewrites) != 0 {
		t.Fatalf("错类型条目不应产出规则: %+v", rules)
	}
}

func TestLoadToleratesUTF8BOM(t *testing.T) {
	path := writeTOML(t, "\xEF\xBB\xBF[\"internal/sanitize/sanitize.go\"]\nsanitizeFeatures = ['bom feature']\n")
	rules, ok := Load(path, SanitizeSection)
	if !ok || len(rules.SanitizeFeatures) != 1 || rules.SanitizeFeatures[0] != "bom feature" {
		t.Fatalf("BOM 文件未正确加载: ok=%v features=%v", ok, rules.SanitizeFeatures)
	}
}

func TestLoadSizeCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.toml")
	if err := os.WriteFile(path, []byte(strings.Repeat("#", maxLoadFile+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := Load(path, SanitizeSection); ok {
		t.Fatal("超过上限的文件应返回 ok=false")
	}
}

// 段级开关只管本段自定义规则是否加载：字段缺失或不是布尔值按默认 true，
// 只有显式 false 跳过加载；内置规则不在本开关管辖内。
func TestLoadEnabled(t *testing.T) {
	const section = "[\"internal/sanitize/sanitize.go\"]\n"
	cases := []struct {
		name    string
		enabled string
		want    bool
	}{
		{"缺失", "", true},
		{"显式 true", "enabled = true\n", true},
		{"显式 false", "enabled = false\n", false},
		{"字符串", "enabled = 'no'\n", true},
		{"整数", "enabled = 0\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rules, ok := Load(writeTOML(t, section+tc.enabled), SanitizeSection)
			if !ok {
				t.Fatal("文件应解析成功")
			}
			if rules.Enabled != tc.want {
				t.Fatalf("Enabled=%v want %v", rules.Enabled, tc.want)
			}
		})
	}

	// 整个段都不存在时，同样按默认 true。
	if rules, ok := Load(writeTOML(t, "[\"x\"]\n"), SanitizeSection); !ok || !rules.Enabled {
		t.Fatalf("段缺失应按默认 enabled=true: ok=%v Enabled=%v", ok, rules.Enabled)
	}
}

// 段内未知字段（如拼错的 enable）只告警、不改变加载结果与默认开关。
func TestLoadUnknownSectionField(t *testing.T) {
	path := writeTOML(t, `
["internal/sanitize/sanitize.go"]
enable = false
sanitizeFeature = ['typo field']
sanitizeFeatures = ['prod feature alpha']
`)
	rules, ok := Load(path, SanitizeSection)
	if !ok {
		t.Fatal("未知字段不应导致整段失败")
	}
	if !rules.Enabled {
		t.Fatal("未知字段不应改变 enabled 默认值")
	}
	if len(rules.SanitizeFeatures) != 1 || rules.SanitizeFeatures[0] != "prod feature alpha" {
		t.Fatalf("已知字段应正常加载: %v", rules.SanitizeFeatures)
	}
}

// 顶层标量/数组（常见于漏写段头）只忽略该键，不能拖垮整份配置。
func TestLoadTopLevelNonTableKeysIgnored(t *testing.T) {
	path := writeTOML(t, `
sanitizeFeatures = ['orphan feature']
other = 1

["internal/sanitize/sanitize.go"]
sanitizeFeatures = ['prod feature alpha']
`)
	rules, ok := Load(path, SanitizeSection)
	if !ok {
		t.Fatal("顶层非表键不应拖垮整份配置")
	}
	if len(rules.SanitizeFeatures) != 1 || rules.SanitizeFeatures[0] != "prod feature alpha" {
		t.Fatalf("段内规则应正常加载: %v", rules.SanitizeFeatures)
	}
}

// 特征条目首尾空白会被去掉，否则 ' x ' 永远不会命中 'x'。
func TestLoadTrimsFeatureWhitespace(t *testing.T) {
	path := writeTOML(t, `
["internal/sanitize/sanitize.go"]
sanitizeFeatures = ['  padded feature  ']
`)
	rules, ok := Load(path, SanitizeSection)
	if !ok || len(rules.SanitizeFeatures) != 1 || rules.SanitizeFeatures[0] != "padded feature" {
		t.Fatalf("首尾空白未去除: %v", rules.SanitizeFeatures)
	}
}

// 解析告警必须写进与 sanitize.toml 同目录的独立 sanitize.log（不进 data/app.log），
// 否则托盘形态启动时，改坏的配置不会有任何可见反馈。
func TestLoadWritesSanitizeLog(t *testing.T) {
	path := writeTOML(t, `
["internal/sanitize/sanitize.go"]
sanitizeRegexpRewrites = [[111, 'DSH']]
`)
	if _, ok := Load(path, SanitizeSection); !ok {
		t.Fatal("坏条目应逐条跳过，不影响整份配置")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "sanitize.log"))
	if err != nil {
		t.Fatalf("sanitize.log 未生成: %v", err)
	}
	if !strings.Contains(string(raw), "原文与替换文都必须是字符串") {
		t.Fatalf("sanitize.log 未记录坏条目告警: %q", raw)
	}
}

// 目录不能被当成可用的配置文件。
func TestEnsureFileRejectsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sanitize.toml")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if EnsureFile(path) {
		t.Fatal("目录不应视为可用配置")
	}
}

// 首次生成的模板必须与仓库根的可读示例逐字一致，避免两处漂移；
// 归一化 CRLF，防止 Windows 检出时行尾转换导致误报。
func TestFileSeedMatchesExample(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "sanitize.example.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ReplaceAll(string(raw), "\r\n", "\n"); got != FileSeed {
		t.Fatal("FileSeed 与 sanitize.example.toml 不一致")
	}
}

// 首次生成的模板必须按 UTF-8 原样落盘且不带 BOM（中文注释逐字校验）。
func TestEnsureFileWritesUTF8(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sanitize.toml")
	if !EnsureFile(path) {
		t.Fatal("首次运行应创建并返回可用")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.Valid(raw) {
		t.Fatal("生成的 sanitize.toml 不是合法 UTF-8")
	}
	if bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) {
		t.Fatal("生成的 sanitize.toml 不应带 UTF-8 BOM")
	}
	if !bytes.Contains(raw, []byte("自定义脱敏规则")) {
		t.Fatal("中文注释未按 UTF-8 写入")
	}
}

// 生成的模板本身必须是合法 TOML：能被 Load 解析出空规则，且 enabled 默认为 true。
func TestFileSeedParses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sanitize.toml")
	if !EnsureFile(path) {
		t.Fatal("首次运行应创建并返回可用")
	}
	rules, ok := Load(path, SanitizeSection)
	if !ok {
		t.Fatal("模板应能正常解析")
	}
	if !rules.Enabled {
		t.Fatal("模板中的 enabled 应为 true")
	}
	if len(rules.SanitizeFeatures)+len(rules.SanitizeStringRewrites)+len(rules.SanitizeRegexpRewrites) != 0 {
		t.Fatalf("示例条目是注释，不应产出规则: %+v", rules)
	}
}

// 首次运行创建注释模板；已存在则原样保留，绝不截断用户配置。
func TestEnsureFile(t *testing.T) {
	dir := t.TempDir()

	path := filepath.Join(dir, "sanitize.toml")
	if !EnsureFile(path) {
		t.Fatal("首次运行应创建并返回可用")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != FileSeed {
		t.Fatalf("首次生成内容=%q want %q", raw, FileSeed)
	}

	// 已存在时不得覆盖用户内容
	want := "[\"internal/sanitize/sanitize.go\"]\nsanitizeFeatures = ['x']\n"
	if err := os.WriteFile(path, []byte(want), 0o600); err != nil {
		t.Fatal(err)
	}
	if !EnsureFile(path) {
		t.Fatal("已存在文件应返回可用")
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != want {
		t.Fatalf("已存在文件被改写: %q", raw)
	}
}
