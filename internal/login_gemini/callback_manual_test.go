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
	cases := []struct {
		name     string
		input    string
		wantCode string
	}{
		{
			name:     "bare code",
			input:    "4/0AXlqoi5oy1234567890",
			wantCode: "4/0AXlqoi5oy1234567890",
		},
		{
			name:     "full url with query code (matching state)",
			input:    "https://antigravity.google/oauth-callback?code=4/0AXquery123",
			wantCode: "4/0AXquery123",
		},
		{
			name:     "full url with hash fragment code",
			input:    "https://antigravity.google/oauth-callback#code=4/0AXhash123&scope=email",
			wantCode: "4/0AXhash123",
		},
		{
			name:     "query string alone",
			input:    "code=4/0AXqueryonly&scope=openid",
			wantCode: "4/0AXqueryonly",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _ = startLogin(t)
			defer Shutdown()

			err := SubmitCallbackURL(tc.input)
			if err != nil {
				t.Fatalf("SubmitCallbackURL(%q) 失败: %v", tc.input, err)
			}
			sess := currentSession()
			if sess == nil {
				t.Fatal("sess 丢失")
			}
			code, _, ok := sess.snapshot()
			if !ok || code != tc.wantCode {
				t.Errorf("得到 code %q，期望 %q", code, tc.wantCode)
			}
		})
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
	err := SubmitCallbackURL("4/0AXpkce123")
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
