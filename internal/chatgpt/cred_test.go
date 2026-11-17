package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// ww3Credential 是 ww3 客户端写在磁盘上的那份文件（`auths/chatgpt-<hash>.json`）。
// 票 01 要求它**零转换**解析：不做任何字段重命名/搬迁。
const ww3Credential = `{
  "auth": {
    "accessToken": "eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjE3OTAwMDAwMDAsImh0dHBzOi8vYXBpLm9wZW5haS5jb20vYXV0aCI6eyJjaGF0Z3B0X2FjY291bnRfaWQiOiJhY2N0LTEiLCJjaGF0Z3B0X3VzZXJfaWQiOiJ1c2VyLTEifX0.sig",
    "expiresAt": 1790000000,
    "deviceId": "0f8f0f8f-1111-2222-3333-444455556666",
    "cookies": {
      "__Secure-next-auth.session-token.0": "tok0",
      "__Secure-next-auth.session-token.1": "tok1",
      "__Secure-next-auth.csrf-token": "csrf"
    },
    "userId": "user-1",
    "kind": "web",
    "planType": "plus",
    "accountId": "acct-1"
  },
  "account": {
    "uid": "user-1",
    "nickname": "tester"
  }
}`

func TestParseAuthZeroConversion(t *testing.T) {
	a, err := ParseAuth([]byte(ww3Credential))
	if err != nil {
		t.Fatalf("ParseAuth: %v", err)
	}
	if a.Kind != "chatgpt" {
		t.Errorf("Kind = %q，期望 chatgpt", a.Kind)
	}
	if a.AccessToken == "" {
		t.Error("accessToken 未读出")
	}
	if a.ExpiresAt != 1790000000 {
		t.Errorf("expiresAt = %d，期望 1790000000", a.ExpiresAt)
	}
	if a.DeviceID != "0f8f0f8f-1111-2222-3333-444455556666" {
		t.Errorf("deviceId = %q", a.DeviceID)
	}
	if a.UID != "user-1" {
		t.Errorf("uid = %q，期望 account.uid", a.UID)
	}
	if len(a.Cookies) != 3 || a.Cookies["__Secure-next-auth.session-token.0"] != "tok0" {
		t.Errorf("cookies 未零转换读出: %+v", a.Cookies)
	}
}

// TestParseAuthUIDFallbackToAuthUserID account.uid 缺失时回退 auth.userId，
// 而不是产出一个「无主」账号。
func TestParseAuthUIDFallbackToAuthUserID(t *testing.T) {
	raw := `{"auth":{"accessToken":"at","cookies":{"c":"1"},"userId":"user-9"},"account":{"nickname":"n"}}`
	a, err := ParseAuth([]byte(raw))
	if err != nil {
		t.Fatalf("ParseAuth: %v", err)
	}
	if a.UID != "user-9" {
		t.Errorf("uid = %q，期望回退到 auth.userId", a.UID)
	}
}

func TestParseAuthRejectsMissingFields(t *testing.T) {
	// 缺 accessToken：auth.Parse 的报错必须可见（而不是静默空账号）。
	_, err := ParseAuth([]byte(`{"auth":{"cookies":{"c":"1"}},"account":{"uid":"u"}}`))
	if err == nil || !strings.Contains(err.Error(), "accessToken") {
		t.Fatalf("缺 accessToken 的错误 = %v，期望提到 accessToken", err)
	}

	// 缺整组 cookies：网页会话靠它，必须显式拒绝（只有 token 会每次 403）。
	_, err = ParseAuth([]byte(`{"auth":{"accessToken":"at"},"account":{"uid":"u"}}`))
	if err == nil || !strings.Contains(err.Error(), "cookies") {
		t.Fatalf("缺 cookies 的错误 = %v，期望提到 cookies", err)
	}
}

func TestParseAuthFlatForm(t *testing.T) {
	a, err := ParseAuth([]byte(`{"accessToken":"at","expiresAt":5,"deviceId":"d","cookies":{"k":"v"},"uid":"u2"}`))
	if err != nil {
		t.Fatalf("flat ParseAuth: %v", err)
	}
	if a.UID != "u2" || a.DeviceID != "d" || len(a.Cookies) != 1 {
		t.Fatalf("扁平形解析结果: %+v", a)
	}
}

func TestParseCookieStringRoundTrip(t *testing.T) {
	m := ParseCookieString("a=1; __Secure-next-auth.session-token.0=xyz; spaced = v ; junk")
	if m["a"] != "1" || m["__Secure-next-auth.session-token.0"] != "xyz" || m["spaced"] != "v" {
		t.Fatalf("ParseCookieString = %+v", m)
	}
	if _, ok := m["junk"]; ok {
		t.Error("无等号的片段不得进 map")
	}
	if got := CookieString(map[string]string{"b": "2", "a": "1"}); got != "a=1; b=2" {
		t.Errorf("CookieString = %q，期望排序后 a=1; b=2", got)
	}
}

func TestJWTInfoAndExp(t *testing.T) {
	tok := testJWT(t, map[string]any{
		"exp": float64(1790000000),
		"iat": float64(1780000000),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-1",
			"chatgpt_user_id":    "user-1",
			"chatgpt_plan_type":  "plus",
		},
		"https://api.openai.com/profile": map[string]any{
			"email": "a@example.com",
			"name":  "Tester",
		},
	})
	if got := JWTExp(tok); got != 1790000000 {
		t.Errorf("JWTExp = %d", got)
	}
	if got := JWTUserID(tok); got != "user-1" {
		t.Errorf("JWTUserID = %q", got)
	}
	if got := JWTName(tok); got != "Tester" {
		t.Errorf("JWTName = %q", got)
	}
	email, plan, acctID, iat, exp := JWTInfo(tok)
	if email != "a@example.com" || plan != "plus" || acctID != "acct-1" || iat != 1780000000 || exp != 1790000000 {
		t.Errorf("JWTInfo = %q %q %q %d %d", email, plan, acctID, iat, exp)
	}
	if JWTExp("not-a-jwt") != 0 {
		t.Error("非 JWT 必须返回 0（未知，不臆造）")
	}
}

// TestCredentialsNeverPrinted 零 token 落盘纪律：掩码与错误文案都不得带出凭据明文。
func TestCredentialsNeverPrinted(t *testing.T) {
	secret := "SECRET-ACCESS-TOKEN-0123456789"
	if strings.Contains(Mask(secret), secret) {
		t.Errorf("Mask 泄漏了 token: %q", Mask(secret))
	}
	if Mask("") != "(empty)" || Mask("short") != "***" {
		t.Errorf("Mask 边界: %q %q", Mask(""), Mask("short"))
	}
	_, err := ParseAuth([]byte(`{"auth":{"cookies":{"c":"1"}},"account":{"uid":"u"}}`))
	if err != nil && strings.Contains(err.Error(), "at") && strings.Contains(err.Error(), secret) {
		t.Error("解析错误不得回显凭据")
	}
	if strings.Contains(Mask("x"), "x") {
		t.Errorf("短 token 掩码不得回显任何字符: %q", Mask("x"))
	}
}

// testJWT 构造一个无签名的 JWT（只用于解析测试，签名不参与校验）。
func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	head, _ := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT"})
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	return enc(head) + "." + enc(body) + ".sig"
}
