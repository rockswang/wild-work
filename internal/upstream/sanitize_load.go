// sanitize_load.go 把 sanitize.toml 中本包段的用户规则追加到内置表。
//
// 解析与校验由 internal/sanitizecfg 共享实现；本文件只负责接线：
// 特征/字符串改写直接 append，正则规则逐条编译后 append。
// 只追加、不覆盖、不删除，不做隐式预检补齐；文件缺失或解析失败时保留内置规则。
// 段内 enabled = false 只跳过本段自定义规则的加载，内置规则照常生效。
package upstream

import (
	"regexp"

	"wild-work/internal/sanitizecfg"
)

const sanitizeTomlLogTag = "[sanitize.toml:internal/upstream]"

// Load 读取 path，把 "internal/upstream/sanitize.go" 段的条目追加到
// sanitizeFeatures / sanitizeStringRewrites / sanitizeRegexpRewrites 末尾。
func Load(path string) {
	rules, ok := sanitizecfg.Load(path, sanitizecfg.UpstreamSection)
	if !ok {
		return
	}
	if !rules.Enabled {
		sanitizecfg.Logf(path, "%s sanitize.toml 中 enabled = false，跳过本段自定义规则（内置规则照常生效）", sanitizeTomlLogTag)
		return
	}

	featureStart := len(sanitizeFeatures)
	sanitizeFeatures = append(sanitizeFeatures, rules.SanitizeFeatures...)
	rewriteStart := len(sanitizeStringRewrites)
	sanitizeStringRewrites = append(sanitizeStringRewrites, rules.SanitizeStringRewrites...)

	addedRegexps := 0
	regexpStart := len(sanitizeRegexpRewrites)
	for i, pair := range rules.SanitizeRegexpRewrites {
		re, err := regexp.Compile(pair[0])
		if err != nil {
			sanitizecfg.Logf(path, "%s 自定义正则第 %d 条编译失败，已跳过: %v（pattern=%q）",
				sanitizeTomlLogTag, i+1, err, pair[0])
			continue
		}
		sanitizeRegexpRewrites = append(sanitizeRegexpRewrites, regexpRewriteStruct{regexp: re, replace: pair[1]})
		addedRegexps++
	}

	if n := len(rules.SanitizeFeatures) + len(rules.SanitizeStringRewrites) + addedRegexps; n > 0 {
		sanitizecfg.Logf(path, "%s 已追加自定义规则共 %d 条：sanitizeFeatures %d 条、sanitizeStringRewrites %d 条、sanitizeRegexpRewrites %d 条",
			sanitizeTomlLogTag, n, len(rules.SanitizeFeatures), len(rules.SanitizeStringRewrites), addedRegexps)
		sanitizecfg.Logf(path, "%s 生效下标：sanitizeFeatures=%v、sanitizeStringRewrites=%v、sanitizeRegexpRewrites=%v",
			sanitizeTomlLogTag,
			indexRange(featureStart, len(rules.SanitizeFeatures)),
			indexRange(rewriteStart, len(rules.SanitizeStringRewrites)),
			indexRange(regexpStart, addedRegexps))
	}
}

// indexRange 返回 [start, start+count) 的下标列表，用于日志定位追加位置。
func indexRange(start, count int) []int {
	if count == 0 {
		return nil
	}
	idx := make([]int, count)
	for i := range idx {
		idx[i] = start + i
	}
	return idx
}
