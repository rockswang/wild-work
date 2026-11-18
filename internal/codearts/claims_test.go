package codearts

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// rtOf 用给定 payload 造一枚 JWS 形态的 refresh_token（测试用假 token，无真实凭据）。
func rtOf(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

// TestParseRefreshClaimsFull 一次真实形态的解码：payload.user_profile 是**再一层 base64url 的 JSON**。
func TestParseRefreshClaimsFull(t *testing.T) {
	prof := `{"account_id":"01a1c44d5e2e70178c41d3b67b7eb1b5","account_name":"hw056401687181961","features_switches":{"enable_pdp5":true},"principal_id":"01a1c44d5e2e79038d6c91b547aedecf","principal_is_root_user":true,"principal_urn":"iam::01a1c44d5e2e70178c41d3b67b7eb1b5:user:hw056401687181961"}`
	token := rtOf(t, map[string]any{
		"exp":          1796891843,
		"iat":          1794877200,
		"iss":          "cn-north-4",
		"type":         "refreshToken",
		"family_id":    "RT:2026111008:1qZdgaqd1zIZfRuzEqVKBw",
		"counter":      102,
		"client_id":    "codearts-agent",
		"user_profile": b64(prof),
	})
	c := ParseRefreshClaims(token)
	if !c.OK {
		t.Fatal("应解出账号身份")
	}
	if c.AccountID != "01a1c44d5e2e70178c41d3b67b7eb1b5" {
		t.Errorf("account_id = %q", c.AccountID)
	}
	if c.PrincipalID != "01a1c44d5e2e79038d6c91b547aedecf" {
		t.Errorf("principal_id = %q", c.PrincipalID)
	}
	if c.AccountName != "hw056401687181961" {
		t.Errorf("account_name = %q", c.AccountName)
	}
	if c.FamilyID != "RT:2026111008:1qZdgaqd1zIZfRuzEqVKBw" {
		t.Errorf("family_id = %q", c.FamilyID)
	}
	if c.ExpiresAt != 1796891843 || c.Counter != 102 {
		t.Errorf("exp/counter = %d/%d", c.ExpiresAt, c.Counter)
	}
	if got := c.AccountKey(); got != c.AccountID {
		t.Errorf("AccountKey 应取 account_id，got %q", got)
	}
}

// TestParseRefreshClaimsInlineProfile 兼容分支：上游某天把 user_profile 改成内联对象。
func TestParseRefreshClaimsInlineProfile(t *testing.T) {
	token := rtOf(t, map[string]any{
		"family_id":    "RT:2026111008:x",
		"user_profile": map[string]any{"account_id": "acct-inline", "principal_id": "p-inline"},
	})
	c := ParseRefreshClaims(token)
	if !c.OK || c.AccountID != "acct-inline" || c.PrincipalID != "p-inline" {
		t.Fatalf("内联 user_profile 未解析：%+v", c)
	}
}

// TestParseRefreshClaimsNoProfile 没有 user_profile：仍能拿到续期声明，但 OK=false
// （调用方据此退回旧口径，不得当成"同一账号"）。
func TestParseRefreshClaimsNoProfile(t *testing.T) {
	token := rtOf(t, map[string]any{"exp": 1796891843, "family_id": "RT:x", "counter": 3})
	c := ParseRefreshClaims(token)
	if c.OK {
		t.Fatal("无 user_profile 时 OK 必须为 false")
	}
	if c.FamilyID != "RT:x" || c.Counter != 3 || c.ExpiresAt != 1796891843 {
		t.Fatalf("续期声明应照常解出：%+v", c)
	}
	if c.AccountKey() != "" {
		t.Fatalf("AccountKey 应为空，got %q", c.AccountKey())
	}
}

// TestParseRefreshClaimsGarbage 各种坏输入必须安静返回零值（登录不能因此失败）。
func TestParseRefreshClaimsGarbage(t *testing.T) {
	cases := map[string]string{
		"空串":            "",
		"非 JWS":         "plain-opaque-token",
		"两段":            "a.b",
		"payload 坏":     "a.!!!!!!.c",
		"payload 非JSON": "a." + b64("not-json") + ".c",
		"profile 坏":     "a." + b64(`{"user_profile":"!!!!"}`) + ".c",
	}
	for name, tok := range cases {
		c := ParseRefreshClaims(tok)
		if c.OK || c.AccountID != "" || c.AccountKey() != "" {
			t.Errorf("%s：应返回零值，实际 %+v", name, c)
		}
	}
}
