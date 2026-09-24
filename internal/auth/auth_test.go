package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseNested(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"domain":""},"account":{"uid":"u1","enterpriseId":"e1","nickname":"n1"}}`)
	sa, err := Parse(raw)
	if err != nil {
		t.Fatalf("nested parse err: %v", err)
	}
	if sa.AccessToken != "at" || sa.RefreshToken != "rt" || sa.ExpiresAt != 1753600000 {
		t.Errorf("tokens: %+v", sa)
	}
	if sa.UID != "u1" || sa.EnterpriseID != "e1" || sa.Nickname != "n1" {
		t.Errorf("account: %+v", sa)
	}
	if sa.Region() != "cn" {
		t.Errorf("region want cn, got %s", sa.Region())
	}
}

func TestParseFlat(t *testing.T) {
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"uid":"u2","nickname":"n2"}`)
	sa, err := Parse(raw)
	if err != nil || sa.UID != "u2" || sa.AccessToken != "at" {
		t.Fatalf("flat: %+v %v", sa, err)
	}
}

func TestParseMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`{"uid":"u3"}`)); err == nil {
		t.Fatal("want error for missing accessToken")
	}
}

func TestGlobalRegion(t *testing.T) {
	for _, d := range []string{"workbuddy.ai", "www.workbuddy.ai", "api.workbuddy.ai", "WorkBuddy.AI"} {
		sa := &Auth{Domain: d}
		if sa.Region() != "global" {
			t.Errorf("domain %q want global, got %s", d, sa.Region())
		}
	}
	for _, d := range []string{"", "codebuddy.cn", "www.codebuddy.cn"} {
		sa := &Auth{Domain: d}
		if sa.Region() != "cn" {
			t.Errorf("domain %q want cn, got %s", d, sa.Region())
		}
	}
}

func TestSaveAtomicRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000,
		UID: "u1", EnterpriseID: "e1", Nickname: "n1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file should not remain")
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.AccessToken != "at" || b.UID != "u1" || b.EnterpriseID != "e1" {
		t.Errorf("roundtrip: %+v", b)
	}
}

func TestLoadDirFiltersRegion(t *testing.T) {
	dir := t.TempDir()
	cn := `{"auth":{"accessToken":"at1","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"cn1"}}`
	gl := `{"auth":{"accessToken":"at2","refreshToken":"r","expiresAt":1,"domain":"www.workbuddy.ai"},"account":{"uid":"g1"}}`
	bad := `not json`
	os.WriteFile(filepath.Join(dir, "workbuddy-cn1.json"), []byte(cn), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-g1.json"), []byte(gl), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"), []byte(bad), 0o600)

	list, err := LoadDir(dir, "cn")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 1 || list[0].UID != "cn1" {
		t.Fatalf("want 1 cn account, got %+v", list)
	}
	if list[0].FilePath == "" {
		t.Error("FilePath not set")
	}
}

func TestNeedsRefresh(t *testing.T) {
	a := &Auth{ExpiresAt: 0}
	if !a.NeedsRefresh(0) {
		t.Error("zero expiry should need refresh")
	}
	a.ExpiresAt = 9999999999
	if a.NeedsRefresh(0) {
		t.Error("far future should not need refresh")
	}
}

// TestAdoptJWTExpiry 验证用 JWT exp 校正偏短/缺失的本地 expiresAt，
// 且不把正常值（已晚于 JWT）改坏——回归：expires_in 单位误判导致 expiresAt 偏短 7 天。
func TestAdoptJWTExpiry(t *testing.T) {
	// exp=2000000000 的 JWT（payload 由测试内构造，签名不参与校验）
	const jwt = "aaa.eyJleHAiOiAyMDAwMDAwMDAwLCAic3ViIjogInUxIn0.bbb"

	a := &Auth{AccessToken: jwt, ExpiresAt: 1789972540} // 历史脏值（比 JWT 少 ~7 天）
	a.AdoptJWTExpiry()
	if a.ExpiresAt != 2000000000 {
		t.Fatalf("expiresAt = %d, want 2000000000（应采纳 JWT exp）", a.ExpiresAt)
	}

	// 已晚于 JWT：不得回退
	b := &Auth{AccessToken: jwt, ExpiresAt: 2100000000}
	b.AdoptJWTExpiry()
	if b.ExpiresAt != 2100000000 {
		t.Fatalf("expiresAt = %d, want 2100000000（不得回退）", b.ExpiresAt)
	}

	// 非 JWT：保持原值
	c := &Auth{AccessToken: "not-a-jwt", ExpiresAt: 5}
	c.AdoptJWTExpiry()
	if c.ExpiresAt != 5 {
		t.Fatalf("expiresAt = %d, want 5（非 JWT 不应改动）", c.ExpiresAt)
	}
}
