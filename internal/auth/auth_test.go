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

	list, err := LoadWorkBuddyDir(dir, "cn")
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

// TestParseCookiesBothForms 锁住 chatgpt 渠道新加的一等字段：
// 嵌套形（ww3 客户端 `auths/*.json`）与扁平形都必须把整组网页会话 Cookie 读出来。
func TestParseCookiesBothForms(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"at","expiresAt":1753600000,"deviceId":"dev-1","cookies":{"__Secure-next-auth.session-token.0":"tok0","__Secure-next-auth.session-token.1":"tok1"}},"account":{"uid":"u-1"}}`)
	a, err := Parse(nested)
	if err != nil {
		t.Fatalf("nested parse: %v", err)
	}
	if a.DeviceID != "dev-1" {
		t.Errorf("deviceId = %q，期望 dev-1", a.DeviceID)
	}
	if len(a.Cookies) != 2 || a.Cookies["__Secure-next-auth.session-token.0"] != "tok0" {
		t.Errorf("cookies 未零转换读出: %+v", a.Cookies)
	}

	flat := []byte(`{"accessToken":"at","cookies":{"a":"1"},"uid":"u-2"}`)
	b, err := Parse(flat)
	if err != nil {
		t.Fatalf("flat parse: %v", err)
	}
	if len(b.Cookies) != 1 || b.Cookies["a"] != "1" {
		t.Errorf("扁平形 cookies 未读出: %+v", b.Cookies)
	}

	// 旧文件没有 cookies 字段 ⇒ nil，不得报错（向后兼容，不变量 5）。
	c, err := Parse([]byte(`{"auth":{"accessToken":"at","expiresAt":1},"account":{"uid":"u-3"}}`))
	if err != nil {
		t.Fatalf("legacy parse: %v", err)
	}
	if c.Cookies != nil {
		t.Errorf("旧文件 cookies 应为 nil，得到 %+v", c.Cookies)
	}
}

// TestSaveAtomicRoundtripCookies cookies 必须经 SaveAtomic 写盘并被重新读出，
// 否则续期拿到的 session token 下次启动就丢了（不变量 20 的同族）。
func TestSaveAtomicRoundtripCookies(t *testing.T) {
	fp := filepath.Join(t.TempDir(), "chatgpt-u1.json")
	a := &Auth{AccessToken: "at", ExpiresAt: 1753600000, DeviceID: "dev-1",
		Cookies: map[string]string{"k0": "v0", "k1": "v1"}, UID: "u1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if len(b.Cookies) != 2 || b.Cookies["k1"] != "v1" {
		t.Fatalf("roundtrip cookies = %+v", b.Cookies)
	}
	if b.DeviceID != "dev-1" {
		t.Fatalf("roundtrip deviceId = %q", b.DeviceID)
	}
}

// TestLoadChatGPTDirGlobBoundary chatgpt 前缀不得吞别的渠道，也不被吞。
func TestLoadChatGPTDirGlobBoundary(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"chatgpt-abc.json": `{"auth":{"accessToken":"at","expiresAt":1,"cookies":{"c":"1"}},"account":{"uid":"cg1"}}`,
		"chatgpt2-x.json":  `{"auth":{"accessToken":"other","expiresAt":1},"account":{"uid":"cg2"}}`,
		"cline-x.json":     `{"auth":{"accessToken":"cline","expiresAt":1},"account":{"uid":"cl"}}`,
		"chatgpt-bad.json": `{`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	list, err := LoadChatGPTDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 1 || list[0].UID != "cg1" {
		t.Fatalf("命中 %d 个（期望仅 chatgpt-abc.json）: %+v", len(list), list)
	}
	if list[0].Kind != "chatgpt" || list[0].FilePath == "" {
		t.Errorf("Kind/FilePath 未回填: %+v", list[0])
	}
	if len(list[0].Cookies) != 1 {
		t.Errorf("Loader 未读出 cookies: %+v", list[0].Cookies)
	}
}
