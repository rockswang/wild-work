package updater

import (
	"strconv"
	"strings"
	"unicode"
)

// CleanVersion 去除前导 'v' 或 'V'，去除多余空白字符。
func CleanVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return strings.TrimSpace(v)
}

// CompareVersions 比较两个版本号字符串（支持带或不带 'v' 前缀）。
// 返回值：
//   -1: v1 < v2
//    0: v1 == v2
//    1: v1 > v2
//
// 语义规则（符合 SemVer 2.0.0 规范）：
// 1. 去除 'v' 前缀。
// 2. 点分数值段逐段比较：如 "2.10.0" > "2.9.4"，"2.10" 等价于 "2.10.0"。
// 3. 非纯数字段（如 "2.9a"）进行数值与字符分词比较，避免 strconv.Atoi 错误导致当 0。
// 4. 预发布版本（如 "2.9.4-rc1"）低于同主版本号的正式版（"2.9.4"）。
// 5. 预发布段之间按点分隔逐段比对（纯数字按数值比，混合字符按 ASCII 比）。
func CompareVersions(v1, v2 string) int {
	s1 := CleanVersion(v1)
	s2 := CleanVersion(v2)
	if s1 == s2 {
		return 0
	}
	if s1 == "" && s2 == "" {
		return 0
	}
	if s1 == "" {
		return -1
	}
	if s2 == "" {
		return 1
	}

	// 剥离 build metadata（+号后不参与版本优先比较）
	s1 = stripBuildMetadata(s1)
	s2 = stripBuildMetadata(s2)

	// 拆分主版本段与预发布段（如 2.9.4-rc1）
	core1, pre1 := splitPreRelease(s1)
	core2, pre2 := splitPreRelease(s2)

	parts1 := strings.Split(core1, ".")
	parts2 := strings.Split(core2, ".")

	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}

	for i := 0; i < maxLen; i++ {
		var p1, p2 string
		if i < len(parts1) {
			p1 = strings.TrimSpace(parts1[i])
		}
		if i < len(parts2) {
			p2 = strings.TrimSpace(parts2[i])
		}
		if p1 == "" {
			p1 = "0"
		}
		if p2 == "" {
			p2 = "0"
		}

		cmp := compareSegment(p1, p2)
		if cmp != 0 {
			return cmp
		}
	}

	// 主版本段完全一致的情况下，判断预发布标识
	// 正式版高于任何预发布版本
	if pre1 == "" && pre2 != "" {
		return 1
	}
	if pre1 != "" && pre2 == "" {
		return -1
	}
	if pre1 != "" && pre2 != "" {
		return comparePreRelease(pre1, pre2)
	}

	return 0
}

func stripBuildMetadata(s string) string {
	if idx := strings.Index(s, "+"); idx >= 0 {
		return s[:idx]
	}
	return s
}

func splitPreRelease(s string) (core, pre string) {
	if idx := strings.Index(s, "-"); idx >= 0 {
		return s[:idx], s[idx+1:]
	}
	return s, ""
}

// compareSegment 比较主版本的单个段（通常为纯数字，防守处理带字母段）
func compareSegment(s1, s2 string) int {
	n1, err1 := strconv.ParseInt(s1, 10, 64)
	n2, err2 := strconv.ParseInt(s2, 10, 64)
	if err1 == nil && err2 == nil {
		if n1 < n2 {
			return -1
		}
		if n1 > n2 {
			return 1
		}
		return 0
	}
	// 如果不是纯数字段，进行自然排序（拆分数字与非数字块）
	return naturalCompare(s1, s2)
}

// comparePreRelease 按 Semver 规范对预发布段进行点分比对
// 例如: "alpha" < "alpha.1" < "alpha.beta" < "beta" < "beta.2" < "beta.11" < "rc.1"
func comparePreRelease(pre1, pre2 string) int {
	parts1 := strings.Split(pre1, ".")
	parts2 := strings.Split(pre2, ".")

	maxLen := len(parts1)
	if len(parts2) > maxLen {
		maxLen = len(parts2)
	}

	for i := 0; i < maxLen; i++ {
		if i >= len(parts1) {
			// pre1 比较短，如 "alpha" vs "alpha.1" -> pre1 < pre2
			return -1
		}
		if i >= len(parts2) {
			return 1
		}

		p1 := parts1[i]
		p2 := parts2[i]

		n1, err1 := strconv.ParseInt(p1, 10, 64)
		n2, err2 := strconv.ParseInt(p2, 10, 64)

		if err1 == nil && err2 == nil {
			if n1 < n2 {
				return -1
			}
			if n1 > n2 {
				return 1
			}
			continue
		}
		// SemVer: 纯数字段的优先级低于非纯数字段
		if err1 == nil && err2 != nil {
			return -1
		}
		if err1 != nil && err2 == nil {
			return 1
		}

		// 两者都是非纯数字，按 ASCII 字典序比较
		if p1 < p2 {
			return -1
		}
		if p1 > p2 {
			return 1
		}
	}

	return 0
}

func naturalCompare(s1, s2 string) int {
	c1 := tokenize(s1)
	c2 := tokenize(s2)
	min := len(c1)
	if len(c2) < min {
		min = len(c2)
	}
	for i := 0; i < min; i++ {
		t1 := c1[i]
		t2 := c2[i]
		n1, err1 := strconv.ParseInt(t1, 10, 64)
		n2, err2 := strconv.ParseInt(t2, 10, 64)
		if err1 == nil && err2 == nil {
			if n1 < n2 {
				return -1
			}
			if n1 > n2 {
				return 1
			}
			continue
		}
		if t1 < t2 {
			return -1
		}
		if t1 > t2 {
			return 1
		}
	}
	if len(c1) < len(c2) {
		return -1
	}
	if len(c1) > len(c2) {
		return 1
	}
	return 0
}

func tokenize(s string) []string {
	var tokens []string
	var cur strings.Builder
	var lastDigit bool

	for i, r := range s {
		isDigit := unicode.IsDigit(r)
		if i > 0 && isDigit != lastDigit {
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
		}
		cur.WriteRune(r)
		lastDigit = isDigit
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens
}
