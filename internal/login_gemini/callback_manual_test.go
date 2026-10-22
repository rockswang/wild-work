package login_gemini

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"wild-work/internal/gemini"
)

func TestSubmitCallbackURLFormats(t *testing.T) {
	_, _, st := startLogin(t)
	defer Shutdown()

	// 1. 裸 code 支持（Google 授权完成页面直接复制的授权码）
	bareCode := "4/0AXbarecode123"
	if err := SubmitCallbackURL(bareCode); err != nil {
		t.Fatalf("SubmitCallbackURL(%q) 期望支持裸 code，但报错: %v", bareCode, err)
	}
	sess := currentSession()
	if sess == nil {
		t.Fatal("sess 丢失")
	}
	code, _, ok := sess.snapshot()
	if !ok || code != bareCode {
		t.Errorf("得到 code %q，期望 %q", code, bareCode)
	}

	// 2. 携带错误 state 必须被拒绝（防旧 URL 串号）
	badStateURL := "https://antigravity.google/oauth-callback?code=4/0AXquery123&state=wrong-state"
	if err := SubmitCallbackURL(badStateURL); err == nil {
		t.Fatalf("SubmitCallbackURL(%q) 期望拒绝，但成功了", badStateURL)
	}

	// 3. 携带正确 state 的 URL 必须通过
	goodURL := fmt.Sprintf("https://antigravity.google/oauth-callback?code=4/0AXquery123&state=%s", st.State)
	if err := SubmitCallbackURL(goodURL); err != nil {
		t.Fatalf("SubmitCallbackURL(%q) 失败: %v", goodURL, err)
	}
	code, _, ok = sess.snapshot()
	if !ok || code != "4/0AXbarecode123" { // 幂等性：保留首个
		t.Errorf("得到 code %q，期望 4/0AXbarecode123", code)
	}
}

func TestExchangeSendsCodeVerifier(t *testing.T) {
	var gotForm url.Values
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotForm, _ = url.ParseQuery(string(mustReadAll(r.Body)))
		_, _ = w.Write([]byte(fmt.Sprintf(
			`{"access_token":"ya29.ACCESS","refresh_token":"1//REFRESH","expires_in":3600,"id_token":%q}`,
			fakeIDToken("sub-pkce", "pkce@test.com"),
		)))
	}))
	defer tokenSrv.Close()
	withEndpoints(t, tokenSrv.URL, "")

	_, statePath, st := startLogin(t)
	goodURL := fmt.Sprintf("https://antigravity.google/oauth-callback?code=4/0AXpkce123&state=%s", st.State)
	err := SubmitCallbackURL(goodURL)
	if err != nil {
		t.Fatalf("SubmitCallbackURL 失败: %v", err)
	}

	res, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll 失败: %v", err)
	}

	if gotForm.Get("code_verifier") != st.CodeVerifier || gotForm.Get("code_verifier") == "" {
		t.Errorf("token 兑换请求必须携带 code_verifier，实际: %q, 期望: %q", gotForm.Get("code_verifier"), st.CodeVerifier)
	}
	if gotForm.Get("redirect_uri") != gemini.AntigravityRedirectURI {
		t.Errorf("token 兑换请求 redirect_uri 错误: %q", gotForm.Get("redirect_uri"))
	}
	if res.UID != "sub-pkce" {
		t.Errorf("解析结果 UID 错误: %q", res.UID)
	}
}

func mustReadAll(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}