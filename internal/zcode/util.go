// util.go 本包共用的小工具：JSON 宽松取值、业务信封解析、随机 id。
//
// 这里刻意集中一处：同一包内同名 helper 只能有一份，散落各文件极易在后续维护中
// 出现「两份口径略微不同」的实现（例如 intOf 一处认字符串、一处不认）。
package zcode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// sha256Sum 返回 s 的 sha256 十六进制小写摘要（32 字节 → 64 字符）。
func sha256Sum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// strOf 宽松取字符串（JSON 里偶发数值/布尔时也给一个稳定文本，避免 nil 拼接）。
func strOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// numOf 取数值字段；**键存在且是有限数值**才算命中（负数钳 0 但仍算「已上报」）。
// usage 搬运与额度解析共用：坏数据不该让整行退化成「—」。
func numOf(m map[string]any, key string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		if n < 0 {
			return 0, true
		}
		return int64(n), true
	case int:
		if n < 0 {
			return 0, true
		}
		return int64(n), true
	case int64:
		if n < 0 {
			return 0, true
		}
		return n, true
	}
	return 0, false
}

// intOf 取整数（缺失 → 0）。
func intOf(m map[string]any, key string) int64 {
	v, _ := numOf(m, key)
	return v
}

// intOfRaw 宽松取整数：**数字或数字字符串都认**。
//
// 为什么必须认字符串：上游业务码会出现 `{"code":"3012"}` 这种纯数字字符串形态
// （协议 §1.7.3 明确要求解析出来），只认 float64 会把风控码读成 0。
func intOfRaw(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if f, err := parseFloat(strings.TrimSpace(s)); err == nil {
			return int64(f)
		}
	}
	return 0
}

// amountOf 解析额度数值：上游既可能给数字也可能给字符串（`total_units` 在实测里是数字，
// 但同族字段出现过字符串形态），两种都认；小数按截断取整（面板是整数口径）。
func amountOf(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if f, err := parseFloat(strings.TrimSpace(s)); err == nil {
			return int64(f)
		}
	}
	return 0
}

// trimJSONString 取 JSON 字符串（非字符串时给空串，用于 msg / status_msg）。
func trimJSONString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// parseFloat 解析浮点（strconv 的薄封装，集中一处便于阅读与测试）。
func parseFloat(s string) (float64, error) { return strconv.ParseFloat(strings.TrimSpace(s), 64) }

// contains 判定字符串切片是否含某值（大小写敏感：档位名 / 头名都是协议里的字面量）。
func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// max64 取较大值（累积口径：只前进不回退，防上游乱序帧把总量改小）。
func max64(a, b int64) int64 {
	if b > a {
		return b
	}
	return a
}

// randomHex 返回 n 字节的随机十六进制串（失败时退化为纳秒时间戳，绝不 panic）。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

// snippet 截一段响应体用于错误信息（排障线索，不含凭据——服务端错误体里不会有 token）。
func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return "空响应"
	}
	return s
}

// decodeObject 把 JSON 解成「键 → 原始值」。
func decodeObject(raw []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("不是 JSON 对象")
	}
	return m, nil
}

// businessCode 取业务信封的 `code`（宽松：数字或纯数字字符串都认）。
//
// 本渠道的失败形态是**两层**的：HTTP 状态码 + 信封里的 `code`（如 401 时 body 是
// `{"code":1002,...}`，405 时是 `{"code":3012,...}`）。只读 HTTP 状态会把
// 「405 + 3012（风控）」当成普通的 405，从而漏掉账号级处置。
func businessCode(top map[string]json.RawMessage) (int64, string, bool) {
	if top == nil {
		return 0, "", false
	}
	raw, ok := top["code"]
	if !ok {
		return 0, "", false
	}
	return intOfRaw(raw), trimJSONString(top["msg"]), true
}
