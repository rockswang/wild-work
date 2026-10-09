package raccoon

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// makeJWT 构造未签名的三段式 JWT（jwtName/jwtExp 只解 payload，签名段内容无所谓）。
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	b64 := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal claims: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return b64(map[string]any{"alg": "none", "typ": "JWT"}) + "." + b64(claims) + ".sig"
}

// writeClientAuth 落一个客户端 auth.json 到临时目录并指向它（BOX_AGENT_CONFIG_DIR）。
func writeClientAuth(t *testing.T, access, refresh string) {
	t.Helper()
	dir := t.TempDir()
	body, err := json.Marshal(map[string]string{
		"access_token": access, "refresh_token": refresh, "office_identity": "personal",
	})
	if err != nil {
		t.Fatalf("marshal auth.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), body, 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	t.Setenv("BOX_AGENT_CONFIG_DIR", dir)
}

// TestRescueFromClientSession_AdoptsMatchingUID uid 一致时采纳客户端最新令牌。
func TestRescueFromClientSession_AdoptsMatchingUID(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	access := makeJWT(t, map[string]any{"name": "U1", "exp": exp})
	refresh := makeJWT(t, map[string]any{"name": "U1", "exp": exp + 14*24*3600})
	writeClientAuth(t, access, refresh)

	a := &auth.Auth{UID: "U1", AccessToken: "stale", RefreshToken: "stale-rt", ExpiresAt: 1}
	if err := RescueFromClientSession(a); err != nil {
		t.Fatalf("RescueFromClientSession: %v", err)
	}
	if a.AccessToken != access || a.RefreshToken != refresh {
		t.Fatalf("令牌未采纳: access=%v refresh=%v", a.AccessToken == access, a.RefreshToken == refresh)
	}
	if a.ExpiresAt != exp {
		t.Fatalf("ExpiresAt = %d, want %d", a.ExpiresAt, exp)
	}
}

// TestRescueFromClientSession_RejectsUIDMismatch uid 不匹配必须拒绝（防多账号互相污染）。
func TestRescueFromClientSession_RejectsUIDMismatch(t *testing.T) {
	access := makeJWT(t, map[string]any{"name": "U2", "exp": time.Now().Add(time.Hour).Unix()})
	writeClientAuth(t, access, "rt-U2")

	a := &auth.Auth{UID: "U1", AccessToken: "stale", RefreshToken: "stale-rt"}
	if err := RescueFromClientSession(a); err == nil {
		t.Fatal("uid 不匹配时应报错，实际采纳成功")
	}
	if a.AccessToken != "stale" || a.RefreshToken != "stale-rt" {
		t.Fatal("拒绝采纳时不得改动账号令牌")
	}
}

// TestRescueFromClientSession_MissingClientFile 本机没有客户端会话时报错。
func TestRescueFromClientSession_MissingClientFile(t *testing.T) {
	t.Setenv("BOX_AGENT_CONFIG_DIR", t.TempDir()) // 目录存在但无 auth.json
	a := &auth.Auth{UID: "U1"}
	if err := RescueFromClientSession(a); err == nil {
		t.Fatal("无客户端会话时应报错")
	}
}

// TestRescueFromClientSession_MissingTokens 客户端会话缺令牌时报错（未登录态）。
func TestRescueFromClientSession_MissingTokens(t *testing.T) {
	writeClientAuth(t, "", "")
	a := &auth.Auth{UID: "U1"}
	if err := RescueFromClientSession(a); err == nil {
		t.Fatal("客户端令牌为空时应报错")
	}
}

// TestJWTName jwtName 对真实结构的解析与容错。
func TestJWTName(t *testing.T) {
	tok := makeJWT(t, map[string]any{"name": "RaccoonX", "exp": 123})
	if got := jwtName(tok); got != "RaccoonX" {
		t.Fatalf("jwtName = %q, want RaccoonX", got)
	}
	if got := jwtName("not-a-jwt"); got != "" {
		t.Fatalf("非 JWT 应返回空, got %q", got)
	}
}

// TestRescueFromClientSession_RejectsExpiredToken 客户端令牌已过期时不采纳。
func TestRescueFromClientSession_RejectsExpiredToken(t *testing.T) {
	access := makeJWT(t, map[string]any{"name": "U1", "exp": time.Now().Add(-time.Hour).Unix()})
	writeClientAuth(t, access, "rt-U1")

	a := &auth.Auth{UID: "U1", AccessToken: "stale", RefreshToken: "stale-rt"}
	if err := RescueFromClientSession(a); err == nil {
		t.Fatal("客户端令牌已过期时应报错")
	}
	if a.AccessToken != "stale" {
		t.Fatal("拒绝采纳时不得改动账号令牌")
	}
}
