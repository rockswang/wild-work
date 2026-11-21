package chatgpt

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"wild-work/internal/auth"
)

func testAuth(cookies map[string]string) *auth.Auth {
	return &auth.Auth{AccessToken: "old-token", UID: "user-1", DeviceID: "dev-1", Cookies: cookies}
}

// TestMeSendsIdentityHeadersAndParses.
func TestMeSendsIdentityHeadersAndParses(t *testing.T) {
	var gotAuth, gotDevice, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotDevice, gotPath = r.Header.Get("Authorization"), r.Header.Get("oai-device-id"), r.URL.Path
		_, _ = w.Write([]byte(`{"email":"a@example.com","id":"user-1","name":"Tester"}`))
	}))
	defer srv.Close()

	c := NewWithConfig(Config{Base: srv.URL})
	defer c.Close()
	u, err := c.Me(testAuth(map[string]string{"session": "s1"}))
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if u.Email != "a@example.com" || u.ID != "user-1" || u.Name != "Tester" {
		t.Errorf("UserInfo = %+v", u)
	}
	if gotPath != PathMe {
		t.Errorf("path = %q，期望 %q", gotPath, PathMe)
	}
	if gotAuth != "Bearer old-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotDevice != "dev-1" {
		t.Errorf("oai-device-id = %q，期望凭据里的 deviceId", gotDevice)
	}
}

// TestModelsBothShapes 两种目录形状（models 数组 / categories 分组）都要能读。
func TestModelsBothShapes(t *testing.T) {
	shapeOne := `{"models":[{"slug":"gpt-6","title":"GPT-6","max_tokens":32768,"tags":["a"]},{"slug":"gpt-6-mini"}]}`
	shapeTwo := `{"categories":[{"id":"gpt_5_auto","models":[{"slug":"gpt-6"},{"slug":"gpt-6"}]},{"slug":"image","models":[{"slug":"gpt-image"}]}]}`

	for name, body := range map[string]string{"shape1": shapeOne, "shape2": shapeTwo} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, PathModels) {
				t.Errorf("path = %q", r.URL.Path)
			}
			if r.URL.RawQuery == "" {
				t.Error("模型端点必须带 iim/is_gizmo 查询串")
			}
			_, _ = w.Write([]byte(body))
		}))
		models, err := NewWithConfig(Config{Base: srv.URL}).Models(testAuth(map[string]string{"session": "s"}))
		srv.Close()
		if err != nil {
			t.Fatalf("%s Models: %v", name, err)
		}
		if len(models) == 0 || models[0].Slug != "gpt-6" {
			t.Fatalf("%s models = %+v", name, models)
		}
	}
	// 分组形状必须去重、并把分组 id 记进 Categories。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(shapeTwo))
	}))
	defer srv.Close()
	models, err := NewWithConfig(Config{Base: srv.URL}).Models(testAuth(map[string]string{"session": "s"}))
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("分组形状未去重: %+v", models)
	}
	if len(models[0].Categories) != 1 || models[0].Categories[0] != "gpt_5_auto" {
		t.Errorf("Categories = %+v", models[0].Categories)
	}
}

// TestMeClassifiesStatus 目录/账号端点的错误必须是类型化的（R79 自愈链依赖）。
func TestMeClassifiesStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"detail":"not login"}`))
	}))
	defer srv.Close()
	_, err := NewWithConfig(Config{Base: srv.URL}).Me(testAuth(map[string]string{"session": "s"}))
	if err == nil || !strings.Contains(err.Error(), "session_dead") {
		t.Fatalf("401 应归 session_dead，得到 %v", err)
	}
}

// TestRefreshSessionSliding 续期：不代 Bearer、捕获 Set-Cookie、到期时刻用 JWT 真值。
func TestRefreshSessionSliding(t *testing.T) {
	jwt := testJWT(t, map[string]any{"exp": float64(1790000000)})
	var sawAuth, sawCookie string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth, sawCookie = r.Header.Get("Authorization"), r.Header.Get("Cookie")
		w.Header().Add("Set-Cookie", "__Secure-next-auth.session-token.0=new0; Path=/; Max-Age=7776000")
		w.Header().Add("Set-Cookie", "__Secure-next-auth.session-token.1=new1; Path=/")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": jwt,
			"expires":     "2027-01-01T00:00:00Z",
			"account":     map[string]any{"id": "acct-1", "planType": "plus"},
			"user":        map[string]any{"id": "user-1", "email": "a@example.com", "name": "Tester"},
		})
	}))
	defer srv.Close()

	a := testAuth(map[string]string{"session": "old"})
	c := NewWithConfig(Config{Base: srv.URL})
	defer c.Close()
	res, err := c.RefreshSession(a)
	if err != nil {
		t.Fatalf("RefreshSession: %v", err)
	}
	if sawAuth != "" {
		t.Errorf("/api/auth/session 不得带 Authorization（上游判异常），得到 %q", sawAuth)
	}
	if !strings.Contains(sawCookie, "session=old") {
		t.Errorf("续期必须带账号 cookie，得到 %q", sawCookie)
	}
	if res.Cookies["__Secure-next-auth.session-token.0"] != "new0" || res.Cookies["__Secure-next-auth.session-token.1"] != "new1" {
		t.Errorf("Set-Cookie 未捕获: %+v", res.Cookies)
	}
	if res.ExpiresAt != 1790000000 {
		t.Errorf("ExpiresAt = %d，期望 JWT exp（真实值，不写远期）", res.ExpiresAt)
	}
	if res.AccountID != "acct-1" || res.PlanType != "plus" || res.Nickname != "Tester" {
		t.Errorf("续期结果 = %+v", res)
	}
	// jar 里应已换成新 cookie。
	got, err := c.Cookies(a)
	if err != nil {
		t.Fatalf("Cookies: %v", err)
	}
	if got["__Secure-next-auth.session-token.0"] != "new0" {
		t.Errorf("jar 未更新: %+v", got)
	}
}

// TestRefreshTokenPersistsToDisk 不变量 20：滑动续期会自动轮换 session cookie，
// 不落盘 = 下次启动用旧凭据的死锁。
func TestRefreshTokenPersistsToDisk(t *testing.T) {
	jwt := testJWT(t, map[string]any{"exp": float64(1790000001)})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Add("Set-Cookie", "__Secure-next-auth.session-token.0=rotated0; Path=/")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": jwt,
			"expires":     "2027-01-01T00:00:00Z",
			"account":     map[string]any{"id": "acct-9"},
			"user":        map[string]any{"id": "user-1", "name": "Tester"},
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	fp := filepath.Join(dir, "chatgpt-user-1.json")
	raw := `{"auth":{"accessToken":"old","expiresAt":1,"deviceId":"d","cookies":{"__Secure-next-auth.session-token.0":"old0"}},"account":{"uid":"user-1"}}`
	if err := os.WriteFile(fp, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := ParseAuth([]byte(raw))
	if err != nil {
		t.Fatalf("ParseAuth: %v", err)
	}
	a.FilePath = fp

	c := NewWithConfig(Config{Base: srv.URL})
	defer c.Close()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	if a.AccessToken != jwt {
		t.Error("内存里的 accessToken 未更新")
	}
	if a.ExpiresAt != 1790000001 {
		t.Errorf("ExpiresAt = %d，期望 JWT exp", a.ExpiresAt)
	}

	// 重新读盘（模拟下次启动）：新 token 与新 cookie 都必须在。
	onDisk, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseAuth(onDisk)
	if err != nil {
		t.Fatalf("重新解析落盘凭据: %v", err)
	}
	if b.AccessToken != jwt {
		t.Error("落盘凭据里的 accessToken 不是续期后的新值")
	}
	if b.Cookies["__Secure-next-auth.session-token.0"] != "rotated0" {
		t.Errorf("落盘凭据里的 cookie 不是滚动后的新值: %+v", b.Cookies)
	}
	if b.ExpiresAt != 1790000001 {
		t.Errorf("落盘 expiresAt = %d", b.ExpiresAt)
	}
	if b.Extra["accountId"] != "acct-9" {
		t.Errorf("accountId 未落 Extra: %+v", b.Extra)
	}
	if fi, err := os.Stat(fp); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("凭据文件权限 = %v，期望 0600", fi.Mode().Perm())
	}
}

// TestSessionIsolationByAccount cookie jar 必须随账号：A 号的 Set-Cookie 不得
// 出现在 B 号的请求里（共用 jar 会把两个号串成一个会话）。
func TestSessionIsolationByAccount(t *testing.T) {
	var mu sync.Mutex
	seen := map[string][]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who := r.URL.Query().Get("who")
		mu.Lock()
		seen[who] = append(seen[who], r.Header.Get("Cookie"))
		mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "server_set", Value: who})
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()

	c := NewWithConfig(Config{Base: srv.URL})
	defer c.Close()
	aA := &auth.Auth{AccessToken: "tok-a", UID: "ua", DeviceID: "da", Cookies: map[string]string{"session": "A"}}
	aB := &auth.Auth{AccessToken: "tok-b", UID: "ub", DeviceID: "db", Cookies: map[string]string{"session": "B"}}

	// 通过 Do 发到带 who 查询串的路径（Me 的路径固定，这里直接验证 jar 行为）。
	if _, _, err := c.Do(aA, "GET", "/backend-api/me?who=A", nil, nil); err != nil {
		t.Fatalf("A 请求: %v", err)
	}
	if _, _, err := c.Do(aB, "GET", "/backend-api/me?who=B", nil, nil); err != nil {
		t.Fatalf("B 请求: %v", err)
	}
	// 再各发一次，验证各自 jar 里的 Set-Cookie 只进自己的请求。
	if _, _, err := c.Do(aA, "GET", "/backend-api/me?who=A", nil, nil); err != nil {
		t.Fatalf("A 二次请求: %v", err)
	}
	if _, _, err := c.Do(aB, "GET", "/backend-api/me?who=B", nil, nil); err != nil {
		t.Fatalf("B 二次请求: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen["A"]) != 2 || len(seen["B"]) != 2 {
		t.Fatalf("请求数不对: %+v", seen)
	}
	if !strings.Contains(seen["A"][0], "session=A") || strings.Contains(seen["A"][0], "session=B") {
		t.Errorf("A 首次请求 cookie = %q", seen["A"][0])
	}
	if !strings.Contains(seen["B"][0], "session=B") || strings.Contains(seen["B"][0], "session=A") {
		t.Errorf("B 首次请求 cookie = %q", seen["B"][0])
	}
	if !strings.Contains(seen["A"][1], "server_set=A") {
		t.Errorf("A 的 jar 没有记住自己的 Set-Cookie: %q", seen["A"][1])
	}
	if strings.Contains(seen["B"][1], "server_set=A") {
		t.Errorf("A 的 Set-Cookie 串进了 B 的请求: %q", seen["B"][1])
	}
	if !strings.Contains(seen["B"][1], "server_set=B") {
		t.Errorf("B 的 jar 没有记住自己的 Set-Cookie: %q", seen["B"][1])
	}
}
