// claims.go 从 refresh_token（JWS）里解出**账号级稳定身份**。
//
// 背景（2026-11-17 实证，本地真机账号）：
//
//	STS 响应体确实不含身份字段（协议 §2.7 只回 credentials.*），但换来的是**一枚 JWS 形态的
//	refresh_token**，它的 payload 里带 `user_profile`（再一层 base64url 的 JSON），其中
//	account_id / account_name / principal_id / principal_urn / principal_is_root_user 都在。
//
// 这些是**华为云账号级**的稳定身份：
//   - account_id  ：同一账号的**多套授权会话**（不同 family）共用同一个值；账号级常量。
//   - principal_id：IAM 主体；同账号下按 IAM 用户区分根用户与子用户。
//   - 而 uid / access_key_id / refresh_token 每次登录、每次续期都会换新（实测 counter 递增）。
//
// 于是「这枚凭据属于哪个账号」终于有了确定性判据 —— 不必再靠用户手填 Label，也不必再
// 退化成「复用该渠道已有账号的第一个 UID」（那正是多账号互相覆盖的根因，见 R159）。
package codearts

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// RefreshClaims 是 refresh_token 里与**账号身份**和**续期**相关的声明。
type RefreshClaims struct {
	// AccountID 华为云账号 ID（形如 01a1c44d5e2e70178c41d3b67b7eb1b5）；账号级常量。
	AccountID string
	// PrincipalID IAM 主体 ID（根用户与账号 ID 相近但不同）；同账号下按 IAM 用户区分。
	PrincipalID string
	// AccountName 账号名（形如 hw056401687181961）；面板诊断用，**不是**手机号。
	AccountName string
	// FamilyID 续期家族 ID（RT:<yyyymmddhh>:<rand>）；同一会话的多次续期 family 不变。
	FamilyID string
	// ExpiresAt refresh_token 自身到期时刻（Unix 秒）；0 = 未给。
	ExpiresAt int64
	// Counter 该 family 已轮转次数；每次续期 +1（实测 09:00→15:23 从 101 涨到 105）。
	Counter int64
	// OK 是否解出了可用身份（= AccountID 非空）。
	OK bool
}

// AccountKey 返回「判同用的账号级键」：account_id 优先，缺失时退回 principal_id。
//
// 空串表示「上游这次没给身份」——调用方必须退回旧口径，**不得**拿它当"同一账号"。
func (c RefreshClaims) AccountKey() string {
	if c.AccountID != "" {
		return c.AccountID
	}
	return c.PrincipalID
}

// ParseRefreshClaims 解析 refresh_token（JWS）payload 里的身份声明。
//
// 容错原则：不是 JWS / 段数不足 / base64 坏 / JSON 坏 / 没有 user_profile —— **一律返回零值**
// （OK=false），不返回错误。身份是「锦上添花」的判据：解不出来要能安静退回旧口径，
// 绝不能因为多解一层声明而让一次已经成功的登录失败。
func ParseRefreshClaims(token string) RefreshClaims {
	var out RefreshClaims
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return out
	}
	payload, ok := decodeB64URL(parts[1])
	if !ok {
		return out
	}
	var env struct {
		Exp      int64           `json:"exp"`
		FamilyID string          `json:"family_id"`
		Counter  int64           `json:"counter"`
		UserProf json.RawMessage `json:"user_profile"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return out
	}
	out.ExpiresAt, out.FamilyID, out.Counter = env.Exp, strings.TrimSpace(env.FamilyID), env.Counter
	if len(env.UserProf) == 0 {
		return out
	}
	// user_profile 实测是**再一层 base64url 的 JSON 字符串**；同时兼容上游某天改成内联对象。
	var prof []byte
	var asString string
	if err := json.Unmarshal(env.UserProf, &asString); err == nil {
		if dec, ok := decodeB64URL(asString); ok {
			prof = dec
		}
	} else {
		prof = env.UserProf
	}
	if len(prof) == 0 {
		return out
	}
	var up struct {
		AccountID   string `json:"account_id"`
		AccountName string `json:"account_name"`
		PrincipalID string `json:"principal_id"`
	}
	if err := json.Unmarshal(prof, &up); err != nil {
		return out
	}
	out.AccountID = strings.TrimSpace(up.AccountID)
	out.AccountName = strings.TrimSpace(up.AccountName)
	out.PrincipalID = strings.TrimSpace(up.PrincipalID)
	out.OK = out.AccountID != ""
	return out
}

// decodeB64URL 解 base64url，容忍「有无 = 补齐」与标准字符集（上游形态未成文）。
func decodeB64URL(s string) ([]byte, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding, base64.RawStdEncoding, base64.StdEncoding,
	} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}
