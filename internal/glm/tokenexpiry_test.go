package glm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// 本文件锁定一个真实 bug（2026-09-26 从运行日志发现）：
//
//	上游 refresh **不返回 expires_in** → 早期实现把 expiresAt 落成 0
//	→ NeedsRefresh 恒为真 → **每次请求都刷 token**
//	（日志实测：同一账号 9 秒内刷两次；危害是与 App 端高频互踩、可能触发风控）
//
// 修复：expires_in 缺失时用 access_token 的 JWT exp 兜底。

// jwtWithExp 造一个带指定 exp 的假 JWT（签名无关，只取 payload）。
func jwtWithExp(exp int64) string {
	header := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9" // {"alg":"HS256","typ":"JWT"}
	payload, _ := json.Marshal(map[string]any{"exp": exp, "iat": exp - 86400})
	body := strings.TrimRight(base64URLEncode(payload), "=")
	return header + "." + body + ".fakesig"
}

// base64URLEncode 标准库的 URL-safe base64（去 padding 由调用方处理）。
func base64URLEncode(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var sb strings.Builder
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := 0
		for j := 0; j < 3 && i+j < len(b); j++ {
			chunk[j] = b[i+j]
			n++
		}
		sb.WriteByte(alphabet[chunk[0]>>2])
		sb.WriteByte(alphabet[(chunk[0]&0x03)<<4|chunk[1]>>4])
		if n > 1 {
			sb.WriteByte(alphabet[(chunk[1]&0x0F)<<2|chunk[2]>>6])
		}
		if n > 2 {
			sb.WriteByte(alphabet[chunk[2]&0x3F])
		}
	}
	return sb.String()
}

// TestJWTExpiryParsing 校验 JWT exp 解析。
func TestJWTExpiryParsing(t *testing.T) {
	want := time.Now().Add(24 * time.Hour).Unix()
	tok := jwtWithExp(want)
	if got := jwtExpiry(tok); got != want {
		t.Errorf("jwtExpiry = %d, want %d", got, want)
	}
	// 非 JWT 必须返回 0（不 panic）
	if got := jwtExpiry("not-a-jwt"); got != 0 {
		t.Errorf("非 JWT 应返回 0, got %d", got)
	}
	if got := jwtExpiry(""); got != 0 {
		t.Errorf("空串应返回 0, got %d", got)
	}
	if got := jwtExpiry("a.b"); got != 0 {
		t.Errorf("坏 payload 应返回 0, got %d", got)
	}
}

// TestRefreshUsesJWTExpWhenNoExpiresIn 是本 bug 的核心回归测试。
//
// 上游不返回 expires_in 时，expiresAt 必须来自 JWT exp，
// **不得为 0**（为 0 会导致每次请求都刷 token）。
func TestRefreshUsesJWTExpWhenNoExpiresIn(t *testing.T) {
	jwtExp := time.Now().Add(24 * time.Hour).Unix()
	token := jwtWithExp(jwtExp)

	// mock 上游：refresh 响应**故意不含 expires_in**（复刻真实行为）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "user/refresh") {
			body, _ := json.Marshal(map[string]any{
				"status": 0, "message": "success",
				"result": map[string]any{
					"user_id":       "u-1",
					"access_token":  token,
					"refresh_token": "RT-new",
					// 注意：**没有 expires_in** —— 这是真实上游行为
				},
			})
			_, _ = w.Write(body)
			return
		}
		_, _ = w.Write([]byte(`{"status":0,"message":"success","result":{}}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	res, err := c.refresh(t.Context(), "RT-old")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	if res.ExpiresAt == 0 {
		t.Fatal("❌ ExpiresAt = 0 —— 这正是 bug：会让 NeedsRefresh 恒为真、每次请求都刷 token")
	}
	// 应等于 JWT exp（允许几秒误差）
	if diff := res.ExpiresAt - jwtExp; diff > 2 || diff < -2 {
		t.Errorf("ExpiresAt = %d, want ≈ %d（应取自 JWT exp）", res.ExpiresAt, jwtExp)
	}
	t.Logf("✅ expires_in 缺失时用 JWT exp 兜底：ExpiresAt = %d", res.ExpiresAt)
}

// TestNoRefreshStorm 修复后的关键行为：token 未过期时**不得再刷**。
//
// 复刻日志里的场景：连续多次取 token，refresh 只应发生一次。
func TestNoRefreshStorm(t *testing.T) {
	jwtExp := time.Now().Add(24 * time.Hour).Unix()
	token := jwtWithExp(jwtExp)

	var mu sync.Mutex
	refreshCalls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "user/refresh") {
			mu.Lock()
			refreshCalls++
			mu.Unlock()
			body, _ := json.Marshal(map[string]any{
				"status": 0, "message": "success",
				"result": map[string]any{
					"user_id": "u-1", "access_token": token, "refresh_token": "RT-new",
				},
			})
			_, _ = w.Write(body)
			return
		}
		_, _ = w.Write([]byte(`{"status":0,"message":"success","result":{}}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	dir := t.TempDir()
	a := &auth.Auth{
		Kind: "glm", AccessToken: token, RefreshToken: "RT-old",
		ExpiresAt: 0, UID: "u-1", FilePath: dir + "/glm-u-1.json",
	}

	// 第一次取 token：应触发一次 refresh（因为本地 expiresAt=0）
	if _, err := c.acquireToken(t.Context(), a); err != nil {
		t.Fatalf("首次 acquireToken: %v", err)
	}
	mu.Lock()
	afterFirst := refreshCalls
	mu.Unlock()
	if afterFirst != 1 {
		t.Fatalf("首次应刷新 1 次，实际 %d 次", afterFirst)
	}

	// 再取 5 次：token 已新鲜，**不应再刷**
	for i := 0; i < 5; i++ {
		if _, err := c.acquireToken(t.Context(), a); err != nil {
			t.Fatalf("第 %d 次 acquireToken: %v", i+2, err)
		}
	}
	mu.Lock()
	total := refreshCalls
	mu.Unlock()

	if total != 1 {
		t.Errorf("❌ 刷新风暴：共刷新 %d 次，应只有 1 次（token 未过期时不得再刷）", total)
	} else {
		t.Log("✅ 无刷新风暴：首次刷新 1 次，后续 5 次复用缓存 token")
	}
}

// TestAdoptJWTExpirySelfHeals 校验历史脏数据自愈。
//
// 老凭证的 expiresAt=0（bug 造成），加载时应被 JWT exp 原地修好。
func TestAdoptJWTExpirySelfHeals(t *testing.T) {
	jwtExp := time.Now().Add(24 * time.Hour).Unix()
	token := jwtWithExp(jwtExp)

	a := &auth.Auth{Kind: "glm", AccessToken: token, ExpiresAt: 0, UID: "u-1"}
	if !a.NeedsRefresh(10 * time.Minute) {
		t.Fatal("前置条件：expiresAt=0 时 NeedsRefresh 应为 true")
	}

	AdoptJWTExpiry(a)

	if a.NeedsRefresh(10 * time.Minute) {
		t.Errorf("❌ 自愈失败：NeedsRefresh 仍为 true（ExpiresAt=%d）", a.ExpiresAt)
	}
	if a.ExpiresAt != jwtExp {
		t.Errorf("ExpiresAt = %d, want %d", a.ExpiresAt, jwtExp)
	}
	t.Logf("✅ 历史脏数据已自愈：ExpiresAt 0 → %d，NeedsRefresh 变 false", a.ExpiresAt)
}

// TestRefreshKeepsExpiresInWhenPresent 上游将来若返回 expires_in，应优先用它。
func TestRefreshKeepsExpiresInWhenPresent(t *testing.T) {
	jwtExp := time.Now().Add(24 * time.Hour).Unix()
	token := jwtWithExp(jwtExp)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "user/refresh") {
			body, _ := json.Marshal(map[string]any{
				"status": 0, "message": "success",
				"result": map[string]any{
					"user_id": "u-1", "access_token": token, "refresh_token": "RT-new",
					"expires_in": 3600, // 显式给出（比 JWT 的 24h 短）
				},
			})
			_, _ = w.Write(body)
			return
		}
		_, _ = w.Write([]byte(`{"status":0,"message":"success","result":{}}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	res, err := c.refresh(t.Context(), "RT-old")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	want := time.Now().Add(3600 * time.Second).Unix()
	if diff := res.ExpiresAt - want; diff > 5 || diff < -5 {
		t.Errorf("ExpiresAt = %d, want ≈ %d（应优先用 expires_in）", res.ExpiresAt, want)
	}
	t.Log("✅ expires_in 存在时优先使用它（JWT 仅作兜底）")
}