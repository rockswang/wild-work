// Package sanitizecfg 负责工作目录下 sanitize.toml 的生命周期：首次运行生成完整模板，
// 之后解析其中用户自定义的脱敏规则，为两套脱敏表提供同一份读取实现。
//
// 本包只做「建文件 + 读文件 + 校验 + 解析」，不接触任何脱敏表：两张表分别在
// internal/sanitize（国际版生产路径）与 internal/upstream（国内版）的包内，
// 变量是包私有的，由两包各自的 Load 做 append 接线。
//
// 设计约束：只追加、不覆盖、不删除内置规则；不隐式补预检。
// 文件不存在、读失败、超限或语法错误只记日志并保留内置规则；
// 这些日志统一写入 sanitize.toml 同目录的 sanitize.log，不混入 data/app.log。
package sanitizecfg

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

const (
	// SanitizeSection internal/sanitize/sanitize.go 段。
	SanitizeSection = "internal/sanitize/sanitize.go"
	// UpstreamSection internal/upstream/sanitize.go 段。
	UpstreamSection = "internal/upstream/sanitize.go"

	// 段内字段名与本包外的同名包级变量逐字一致，不另起别名。
	// 两段的字段集合相同，因此无需段 → 字段的映射表。
	sanitizeFeatures       = "sanitizeFeatures"
	sanitizeStringRewrites = "sanitizeStringRewrites"
	sanitizeRegexpRewrites = "sanitizeRegexpRewrites"

	// enabled 只控制本段自定义规则是否加载：字段缺失或不是布尔值时按默认值 true。
	enabled = "enabled"

	sanitizeTomlLogTag = "[sanitize.toml:config]"
	// maxLoadFile 启动期读取上限：脱敏规则是几十行的小配置，超过即视为异常。
	maxLoadFile = 1 << 20

	// sanitizeLogName 日志文件名：固定放在 sanitize.toml 所在目录，
	// 使用户改坏配置时无论进程以托盘还是控制台形态启动，都能在配置旁看到告警。
	sanitizeLogName = "sanitize.log"
)

// Logf 把一条消息写入 sanitize.toml 同目录的 sanitize.log，供本包与
// internal/sanitize、internal/upstream 的接线层共用；文件打不开时退回 stderr。
// 配置只在启动期读取一次、日志条数很少，故逐条开关文件，不长期持有句柄。
func Logf(tomlPath, format string, args ...any) {
	f, err := os.OpenFile(filepath.Join(filepath.Dir(tomlPath), sanitizeLogName),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.New(os.Stderr, "", log.LstdFlags|log.Lmicroseconds).Printf(format, args...)
		return
	}
	defer f.Close()
	log.New(f, "", log.LstdFlags|log.Lmicroseconds).Printf(format, args...)
}

// FileSeed 首次生成的 sanitize.toml 内容：完整模板（含两段的 enabled 与全部字段），
// 与仓库根的 sanitize.example.toml 保持一致；纯注释也是合法 TOML。
const FileSeed = `# 自定义脱敏规则（可选），改完重启程序生效。
#
# 三个字段都在原有规则后面追加，不覆盖、不删除：
#   sanitizeFeatures       触发词：文本里出现任意一个，才会执行下面的替换
#   sanitizeStringRewrites 整句替换：['原文', '改写后']
#   sanitizeRegexpRewrites 正则替换：['正则', '改写后']
#
# 填写要点：
#   - 示例条目已写在各自的 [ ] 里，去掉行首的 # 即可生效，不要重复写字段名。
#   - 普通内容优先用英文单引号包住（反斜杠原样保留）；改用双引号须自行转义反斜杠，正则请务必用单引号。
#   - 新增指纹时，把指纹原文同时写进 sanitizeFeatures 和替换列表。
#   - 触发词不区分大小写。(另外两个字段仍然区分大小写)
#   - 国际版填第一段，国内版填第二段；不确定就两段都填。
#   - enabled = false 只停用本段自定义规则，内置规则照常生效。
#
# 如果不会填，可以让 AI 帮你填：直接告诉 AI「去配置这个文件」即可。
#
# Are you an AI agent? Follow these steps to fill this file:
# 1. Read the built-in rules:
#    https://raw.githubusercontent.com/rockswang/wild-work/master/internal/upstream/sanitize.go
# 2. Ask the user which text is being blocked or should be rewritten.
# 3. Add matching entries to sanitizeFeatures and the two rewrite lists below.
# 4. Tell the user to restart the program, then confirm the new rules work.

# 国际版
["internal/sanitize/sanitize.go"]
enabled = true
sanitizeFeatures = [
    # 'you are an ai agent powered by deepseek harness',
]
sanitizeStringRewrites = [
    # ['You are an AI agent powered by DeepSeek Harness', 'You are an AI agent powered by DSH'],
]
sanitizeRegexpRewrites = [
    # ['(?i)deepseek[\s\-_]*harness', 'DSH'],
]

# 国内版
["internal/upstream/sanitize.go"]
enabled = true
sanitizeFeatures = [
    # 'you are an ai agent powered by deepseek harness',
]
sanitizeStringRewrites = [
    # ['You are an AI agent powered by DeepSeek Harness', 'You are an AI agent powered by DSH'],
]
sanitizeRegexpRewrites = [
    # ['(?i)deepseek[\s\-_]*harness', 'DSH'],
]
`

// Rules 一个段解析出的原始规则（未编译正则，由调用方按自己的表类型编译）。
// 字段名与 internal/sanitize / internal/upstream 的同名包级变量对齐，
// 首字母大写只是为了满足导出规则。
type Rules struct {
	// Enabled 段级开关：false 时调用方跳过本段自定义规则的追加，内置规则不受影响。
	Enabled                bool
	SanitizeFeatures       []string
	SanitizeStringRewrites [][2]string
	SanitizeRegexpRewrites [][2]string
}

// EnsureFile 确保 path 是一个可用的普通文件：首次运行写入 FileSeed，已存在则原样保留。
// 目录、悬空符号链接、创建/写入/关闭失败都返回 false 并记日志。
func EnsureFile(path string) bool {
	if info, err := os.Stat(path); err == nil {
		if info.Mode().IsRegular() {
			return true
		}
		Logf(path, "检查 %s 失败：不是普通文件", path)
		return false
	} else if !errors.Is(err, fs.ErrNotExist) {
		Logf(path, "检查 %s 失败：%v", path, err)
		return false
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) { // 并发进程刚创建，或路径是悬空符号链接
			return isRegularFile(path)
		}
		Logf(path, "创建 %s 失败：%v", path, err)
		return false
	}
	// Go 字符串按源码的 UTF-8 字节原样写入，不经任何转码，落盘即为 UTF-8 且无 BOM。
	if _, err := f.WriteString(FileSeed); err != nil {
		Logf(path, "写入 %s 失败：%v", path, err)
		_ = f.Close()
		removePartial(path)
		return false
	}
	if err := f.Close(); err != nil {
		Logf(path, "关闭 %s 失败：%v", path, err)
		removePartial(path)
		return false
	}
	return true
}

// isRegularFile 报告 path 是否是存在的普通文件（跟随符号链接）。
func isRegularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

// removePartial 删除未写成功的文件，避免下次启动把空/半截文件当成可用配置。
func removePartial(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		Logf(path, "清理未写完的 %s 失败：%v", path, err)
	}
}

// Load 读取 path 中 section 段的规则。返回 ok=false 表示文件缺失/读失败/超限/
// TOML 语法错误（此时调用方应保留内置规则）；段不存在或为空表时返回
// Enabled=true 的空规则。
func Load(path, section string) (Rules, bool) {
	switch section {
	case SanitizeSection, UpstreamSection:
	default:
		Logf(path, "%s 未知段 %q，已跳过", sanitizeTomlLogTag, section)
		return Rules{}, false
	}
	raw, ok := readFile(path)
	if !ok {
		return Rules{}, false
	}

	// 解析到 map[string]any 而不是 map[string]map[string]any：顶层出现标量/数组
	// （例如漏写段头的 sanitizeFeatures = [...]）时只忽略该键，不能拖垮整份配置。
	var sections map[string]any
	if err := toml.Unmarshal(raw, &sections); err != nil {
		Logf(path, "%s 解析 %s 失败，保留内置规则: %v", sanitizeTomlLogTag, path, err)
		return Rules{}, false
	}
	for key := range sections {
		if key != SanitizeSection && key != UpstreamSection {
			Logf(path, "%s %s 出现未知顶层键 %q，已忽略（自定义规则必须写在段内）",
				sanitizeTomlLogTag, path, key)
		}
	}

	secValue, ok := sections[section]
	if !ok {
		return Rules{Enabled: true}, true // 段不存在：合法的空配置，开关按默认 true
	}
	sec, ok := secValue.(map[string]any)
	if !ok {
		Logf(path, "%s %s 段 %s 不是表，已跳过本段", sanitizeTomlLogTag, path, section)
		return Rules{Enabled: true}, true
	}

	scope := path + " 段 " + section
	// 拼错或旧名的字段不会生效：未知键只告警，不影响已知键的加载。
	for key := range sec {
		switch key {
		case enabled, sanitizeFeatures, sanitizeStringRewrites, sanitizeRegexpRewrites:
		default:
			Logf(path, "%s %s：未知字段 %q，已忽略（请检查拼写）", sanitizeTomlLogTag, scope, key)
		}
	}
	var rules Rules
	rules.Enabled = true // 默认加载本段自定义规则
	if v, ok := sec[enabled]; ok {
		rules.Enabled = decodeEnabled(v, path, scope, enabled)
	}
	if v, ok := sec[sanitizeFeatures]; ok {
		rules.SanitizeFeatures = decodeFeatures(v, path, scope, sanitizeFeatures)
	}
	if v, ok := sec[sanitizeStringRewrites]; ok {
		rules.SanitizeStringRewrites = decodeRewrites(v, path, scope, sanitizeStringRewrites)
	}
	if v, ok := sec[sanitizeRegexpRewrites]; ok {
		rules.SanitizeRegexpRewrites = decodeRewrites(v, path, scope, sanitizeRegexpRewrites)
	}
	return rules, true
}

// readFile 读取配置原文；不存在、读失败、过大、空文件均返回 false。
func readFile(path string) ([]byte, bool) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			Logf(path, "%s %s 不存在，跳过自定义规则（内置规则照常生效）", sanitizeTomlLogTag, path)
		} else {
			Logf(path, "%s 读取 %s 失败，保留内置规则: %v", sanitizeTomlLogTag, path, err)
		}
		return nil, false
	}
	if info.Size() > maxLoadFile {
		Logf(path, "%s %s 超过 %d 字节，忽略", sanitizeTomlLogTag, path, maxLoadFile)
		return nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		Logf(path, "%s 读取 %s 失败，保留内置规则: %v", sanitizeTomlLogTag, path, err)
		return nil, false
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxLoadFile+1))
	if err != nil {
		Logf(path, "%s 读取 %s 失败，保留内置规则: %v", sanitizeTomlLogTag, path, err)
		return nil, false
	}
	// Stat 与 Open 之间文件可能增长或被换靶，实际读取也限制为上限+1。
	if len(raw) > maxLoadFile {
		Logf(path, "%s %s 实际读取 %d 字节，超过 %d 字节上限，忽略",
			sanitizeTomlLogTag, path, len(raw), maxLoadFile)
		return nil, false
	}
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) // 容忍 UTF-8 BOM
	if len(bytes.TrimSpace(raw)) == 0 {
		Logf(path, "%s %s 为空，跳过自定义规则（内置规则照常生效）", sanitizeTomlLogTag, path)
		return nil, false
	}
	return raw, true
}

// decodeEnabled 解析段级开关：字段缺失由调用方按 true 处理，这里只处理
// 出现但类型不对的形态——非布尔值同样退回默认 true，并记一条日志。
func decodeEnabled(v any, tomlPath, scope, key string) bool {
	b, ok := v.(bool)
	if !ok {
		Logf(tomlPath, "%s %s：%s 不是布尔值，按默认 true 处理", sanitizeTomlLogTag, scope, key)
		return true
	}
	return b
}

// decodeFeatures 逐条解析特征串：非数组、非字符串或去空白后为空的条目跳过，
// 不影响其余条目。入表前去掉首尾空白，否则 ' x ' 永远匹配不到 'x'。
func decodeFeatures(v any, tomlPath, scope, key string) []string {
	list, ok := v.([]any)
	if !ok {
		Logf(tomlPath, "%s %s：%s 必须是字符串数组，已跳过", sanitizeTomlLogTag, scope, key)
		return nil
	}
	out := make([]string, 0, len(list))
	for i, item := range list {
		s, ok := item.(string)
		if !ok {
			Logf(tomlPath, "%s %s：%s[%d] 不是字符串，已跳过", sanitizeTomlLogTag, scope, key, i)
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" {
			Logf(tomlPath, "%s %s：%s[%d] 去空白后为空，已跳过", sanitizeTomlLogTag, scope, key, i)
			continue
		}
		out = append(out, s)
	}
	return out
}

// decodeRewrites 逐条解析 [原文, 替换文]：形状错误、过短原文、空替换文均跳过。
func decodeRewrites(v any, tomlPath, scope, key string) [][2]string {
	list, ok := v.([]any)
	if !ok {
		Logf(tomlPath, "%s %s：%s 必须是二元数组的数组，已跳过", sanitizeTomlLogTag, scope, key)
		return nil
	}
	out := make([][2]string, 0, len(list))
	for i, item := range list {
		pair, ok := item.([]any)
		if !ok || len(pair) != 2 {
			Logf(tomlPath, "%s %s：%s[%d] 需要恰好两项 [原文, 替换文]，已跳过",
				sanitizeTomlLogTag, scope, key, i)
			continue
		}
		from, ok1 := pair[0].(string)
		to, ok2 := pair[1].(string)
		if !ok1 || !ok2 {
			Logf(tomlPath, "%s %s：%s[%d] 原文与替换文都必须是字符串，已跳过",
				sanitizeTomlLogTag, scope, key, i)
			continue
		}
		// 过短原文会让 strings.ReplaceAll 误伤整段文本（如 "a"），必须拒绝。
		// 按字符数判断：两个中文字是 6 字节，用 len 会误放行。
		if utf8.RuneCountInString(from) < 4 || strings.TrimSpace(from) == "" {
			Logf(tomlPath, "%s %s：%s[%d] 原文为空或短于 4 字符，已跳过",
				sanitizeTomlLogTag, scope, key, i)
			continue
		}
		// 本层语义是最小改写；空替换文等于整段删除，不允许。
		if to == "" {
			Logf(tomlPath, "%s %s：%s[%d] 替换文为空（本层不做删除），已跳过",
				sanitizeTomlLogTag, scope, key, i)
			continue
		}
		out = append(out, [2]string{from, to})
	}
	return out
}
