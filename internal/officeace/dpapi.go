package officeace

import "strings"

// dpapiHeader 是 OfficeAce 加密文件的魔数首行。
const dpapiHeader = "OC-DPAPI-1"

// hasDPAPIHeader 报告文本是否以 DPAPI 魔数开头。
func hasDPAPIHeader(text string) bool { return strings.HasPrefix(strings.TrimSpace(text), dpapiHeader) }

// trimSpaceString 是 strings.TrimSpace 的薄封装（供带构建标签的文件共用）。
func trimSpaceString(s string) string { return strings.TrimSpace(s) }
