// login_test.go 登录包回归测试：**零真实网络**（STS 指向 httptest；回调走 127.0.0.1 本机）。
package login_codearts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/codearts"
)

// fakeSTS 假 STS：记录 form 与 DPoP 头，返回固定凭据。
type fakeSTS struct {
	*httptest.Server
	form   url.Values
	dpop   string
	status int
	body   string
	calls  int
}

func newFakeSTS(t *testing.T) *fakeSTS {
	fs := &fakeSTS{status: 200, body: `{"credentials":{"access_key_id":"AK-NEW","secret_access_key":"SK-NEW","security_token":"TOK-NEW","expiration":"2030-01-01T00:00:00Z"},"refresh_token":"rt-new"}`}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.calls++
		_ = r.ParseForm()
		fs.form = r.PostForm
		fs.dpop = r.Header.Get("DPoP")
		w.WriteHeader(fs.status)
		_, _ = io.WriteString(w, fs.body)
	}))
	t.Cleanup(fs.Close)
	return fs
}

// startLogin 起一次登录并返回 state 与授权 URL。
func startLogin(t *testing.T) (state, string, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")
	authURL, err := Start(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st, err := readState(statePath)
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	t.Cleanup(Shutdown)
	return st, authURL, statePath
}

// TestStartBuildsAuthorizeURL 授权页参数逐项核对（协议 §2.1）。
func TestStartBuildsAuthorizeURL(t *testing.T) {
	st, authURL, _ := startLogin(t)

	if !strings.HasPrefix(authURL, codearts.PortalAuthorizeURL+"?") {
		t.Fatalf("授权 URL 前缀不对: %s", authURL)
	}
	for _, want := range []string{
		"theme=2", "locale=zh-cn", "uri_scheme=codearts-agent", "client_id=codearts-agent",
		// ⚠️ 非标准取值：S256 会让 portal 静默回退旧 ticket 流程。
		"code_challenge_method=SHA-256",
		"plugin-name=snap_AIIDE", "plugin-version=5.2.0",
	} {
		if !strings.Contains(authURL, want) {
			t.Errorf("授权 URL 缺 %s\n%s", want, authURL)
		}
	}
	if st.CallbackPort < codearts.MinCallbackPort {
		t.Fatalf("回调端口 = %d，必须 ≥ %d（协议要求）", st.CallbackPort, codearts.MinCallbackPort)
	}
	if !strings.Contains(authURL, "port="+itoa(st.CallbackPort)) {
		t.Errorf("授权 URL 的 port 与 state 不一致:\n%s\nstate port=%d", authURL, st.CallbackPort)
	}
	if st.RedirectURI != "http://127.0.0.1:"+itoa(st.CallbackPort)+codearts.OAuthCallbackPath {
		t.Errorf("redirect_uri = %s", st.RedirectURI)
	}
	if st.Verifier == "" || len(st.Verifier) < 43 {
		t.Errorf("PKCE verifier 形态不对: %q", st.Verifier)
	}
	// challenge 必须是 verifier 的 SHA-256 base64url。
	sum := sha256.Sum256([]byte(st.Verifier))
	if !strings.Contains(authURL, "code_challenge="+base64.RawURLEncoding.EncodeToString(sum[:])) {
		t.Error("code_challenge 不是 verifier 的 SHA-256 base64url")
	}
	if _, err := codearts.ParseDPoPKey(st.DPoPJWK); err != nil {
		t.Fatalf("state 里的 DPoP 私钥不可解析: %v", err)
	}
	if st.TicketID == "" || st.CreatedAt == 0 {
		t.Error("ticket_id/created_at 缺失")
	}
	if strings.Contains(authURL, "code_verifier=") {
		t.Error("授权 URL 绝不能带 code_verifier（只能带 challenge）")
	}
}

// TestPollPendingThenSuccess 完整登录流程：Pending → 本机回调 → 换凭据成功。
func TestPollPendingThenSuccess(t *testing.T) {
	sts := newFakeSTS(t)
	old := stsURL
	stsURL = sts.URL
	defer func() { stsURL = old }()

	st, _, statePath := startLogin(t)

	// 尚未回调：Pending。
	if _, err := Poll(NewClient(), statePath); err != ErrPending {
		t.Fatalf("未回调应返回 ErrPending，得到 %v", err)
	}

	// 模拟浏览器回调（真发本机 HTTP 请求，走的是我们起的回调 server）。
	cb := "http://127.0.0.1:" + itoa(st.CallbackPort) + codearts.OAuthCallbackPath + "?code=code-123"
	resp, err := http.Get(cb)
	if err != nil {
		t.Fatalf("回调请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "登录成功") {
		t.Fatalf("回调页文案不对: %s", body)
	}

	res, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.AccessKeyID != "AK-NEW" || res.SecretAccessKey != "SK-NEW" || res.SecurityToken != "TOK-NEW" {
		t.Fatalf("凭据未取到: %+v", res)
	}
	if res.AccessToken != "AK-NEW" {
		t.Fatalf("AccessToken 必须承载 AK，得到 %q", res.AccessToken)
	}
	if res.RefreshToken != "rt-new" {
		t.Fatalf("refresh_token = %q", res.RefreshToken)
	}
	wantExp := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	if res.ExpiresAt != wantExp {
		t.Fatalf("ExpiresAt = %d, want %d（秒）", res.ExpiresAt, wantExp)
	}
	if res.UID != "AK-NEW" || res.DPoPPrivateJWK != st.DPoPJWK || res.CodeVerifier != st.Verifier {
		t.Fatalf("续期材料未带上: %+v", res)
	}

	// STS form 字段（协议 §2.1）。
	for k, want := range map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     codearts.OAuthClientID,
		"code":          "code-123",
		"code_verifier": st.Verifier,
		"redirect_uri":  st.RedirectURI,
	} {
		if got := sts.form.Get(k); got != want {
			t.Errorf("STS form[%s] = %q, want %q", k, got, want)
		}
	}
	// DPoP 证明必须可被其内嵌公钥验证，且 htu 指向 STS。
	payload := verifyDPoPProof(t, sts.dpop)
	if payload["htu"] != sts.URL || payload["htm"] != "POST" {
		t.Fatalf("DPoP 载荷不对: %+v", payload)
	}
	// 成功收尾：state 文件被清掉。
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("成功登录后 state 应被清理，err=%v", err)
	}
}

// TestPollCallbackError 用户拒绝 / 回调异常 → 终态错误并清理 state。
func TestPollCallbackError(t *testing.T) {
	st, _, statePath := startLogin(t)
	cb := "http://127.0.0.1:" + itoa(st.CallbackPort) + codearts.OAuthCallbackPath + "?error=access_denied&error_description=user%20denied"
	resp, err := http.Get(cb)
	if err != nil {
		t.Fatalf("回调请求失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	_, err = Poll(NewClient(), statePath)
	if err == nil || err == ErrPending {
		t.Fatalf("应返回终态错误，得到 %v", err)
	}
	if !strings.Contains(err.Error(), "user denied") {
		t.Fatalf("错误文案应带上游描述: %v", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("终态错误后应清理 state")
	}
}

// TestPollMissingCode 回调缺 code → 终态错误。
func TestPollMissingCode(t *testing.T) {
	st, _, statePath := startLogin(t)
	cb := "http://127.0.0.1:" + itoa(st.CallbackPort) + codearts.OAuthCallbackPath
	resp, err := http.Get(cb)
	if err != nil {
		t.Fatalf("回调请求失败: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if _, err := Poll(NewClient(), statePath); err == nil || err == ErrPending {
		t.Fatalf("缺 code 应返回错误，得到 %v", err)
	}
}

// TestPollTimeout 超时（state 里的 createdAt 过旧）→ 明确错误 + 清理。
func TestPollTimeout(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "login-state.json")
	st := state{Verifier: "v", DPoPJWK: "{}", CallbackPort: 1, CreatedAt: time.Now().Add(-flowTTL - time.Minute).Unix()}
	if err := writeState(statePath, st); err != nil {
		t.Fatal(err)
	}
	_, err := Poll(NewClient(), statePath)
	if err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("应返回超时错误，得到 %v", err)
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Fatal("超时后应清理 state")
	}
}

// TestPollExchangeFailureKeepsState STS 5xx（非终态）不得清掉 state，让上层继续轮询。
func TestPollExchangeFailureKeepsState(t *testing.T) {
	sts := newFakeSTS(t)
	sts.status = 500
	sts.body = `{"error":"server_error"}`
	old := stsURL
	stsURL = sts.URL
	defer func() { stsURL = old }()

	st, _, statePath := startLogin(t)
	cb := "http://127.0.0.1:" + itoa(st.CallbackPort) + codearts.OAuthCallbackPath + "?code=c1"
	if resp, err := http.Get(cb); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	if _, err := Poll(NewClient(), statePath); err == nil {
		t.Fatal("STS 5xx 必须报错")
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("非终态错误不该清理 state: %v", err)
	}
	// 修好上游后重试同一 code 仍可成功。
	sts.status = 200
	sts.body = `{"credentials":{"access_key_id":"AK-NEW","secret_access_key":"SK-NEW","security_token":"TOK-NEW","expiration":"2030-01-01T00:00:00Z"},"refresh_token":"rt-new"}`
	res, err := Poll(NewClient(), statePath)
	if err != nil {
		t.Fatalf("恢复后应能成功: %v", err)
	}
	if res.AccessKeyID != "AK-NEW" {
		t.Fatalf("凭据 = %+v", res)
	}
}

// TestPollExchangeTerminalCleansState invalid_grant（终态）→ 清理 state。
func TestPollExchangeTerminalCleansState(t *testing.T) {
	sts := newFakeSTS(t)
	sts.status = 400
	sts.body = `{"error":"invalid_grant","error_description":"code expired"}`
	old := stsURL
	stsURL = sts.URL
	defer func() { stsURL = old }()

	st, _, statePath := startLogin(t)
	cb := "http://127.0.0.1:" + itoa(st.CallbackPort) + codearts.OAuthCallbackPath + "?code=c1"
	if resp, err := http.Get(cb); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	_, err := Poll(NewClient(), statePath)
	if err == nil {
		t.Fatal("invalid_grant 必须报错")
	}
	if _, statErr := os.Stat(statePath); !os.IsNotExist(statErr) {
		t.Fatal("终态错误应清理 state")
	}
	// 错误文案只透出上游自报的 error 字段，不回显响应体原文（成功响应体里就是凭据）。
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Fatalf("错误文案应带上游 error 字段: %v", err)
	}
	for _, leak := range []string{"secret_access_key", "security_token", "credentials"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误文案不得含凭据字段 %s: %v", leak, err)
		}
	}
}

// TestSaveAuthRoundTripAndGlobBoundary 落盘往返 + Extra 键 + glob 前缀边界。
func TestSaveAuthRoundTripAndGlobBoundary(t *testing.T) {
	dir := t.TempDir()
	res := Result{
		AccessToken: "AK1", AccessKeyID: "AK1", SecretAccessKey: "SK1", SecurityToken: "TOK1",
		RefreshToken: "rt-1", ExpiresAt: time.Now().Add(2 * time.Hour).Unix(),
		UID: "user-1", Nickname: "小明", CodeVerifier: "ver-1", DPoPPrivateJWK: `{"kty":"EC"}`,
		DomainID: "dom-1", UserID: "user-1", UserName: "小明", ProjectID: "p-1", Region: "cn-north-4",
	}
	fp, err := SaveAuth(dir, res)
	if err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}
	if filepath.Base(fp) != "codearts-user-1.json" {
		t.Fatalf("文件名 = %s", filepath.Base(fp))
	}
	list, err := auth.LoadCodeArtsDir(dir)
	if err != nil {
		t.Fatalf("LoadCodeArtsDir: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("加载到 %d 条，want 1", len(list))
	}
	a := list[0]
	if a.Kind != "codearts" || a.UID != "user-1" || a.Nickname != "小明" {
		t.Fatalf("通用字段不对: %+v", a)
	}
	if a.AccessToken != "AK1" || a.RefreshToken != "rt-1" || a.ExpiresAt != res.ExpiresAt {
		t.Fatalf("顶层凭据字段不对: %+v", a)
	}
	for k, want := range map[string]string{
		codearts.ExtraAccessKeyID:       "AK1",
		codearts.ExtraSecretAccessKey:   "SK1",
		codearts.ExtraSecurityToken:     "TOK1",
		codearts.ExtraCodeVerifier:      "ver-1",
		codearts.ExtraDPoPPrivateKeyJWK: `{"kty":"EC"}`,
		codearts.ExtraDomainID:          "dom-1",
		codearts.ExtraUserID:            "user-1",
		codearts.ExtraUserName:          "小明",
		codearts.ExtraProjectID:         "p-1",
		codearts.ExtraRegion:            "cn-north-4",
		codearts.ExtraMaaSType:          codearts.MaaSTypeBenefit,
	} {
		if got := a.Extra[k]; got != want {
			t.Errorf("Extra[%s] = %q, want %q（全部 Extra=%v）", k, got, want, a.Extra)
		}
	}
	// 不得重复塞通用字段。
	for _, dup := range []string{"access_token", "uid", "nickname", "expires_at"} {
		if _, has := a.Extra[dup]; has {
			t.Errorf("Extra 不得重复塞 %s", dup)
		}
	}

	// glob 边界：`codearts2-*.json` 不该被收录；别的渠道前缀也不受影响。
	if err := os.WriteFile(filepath.Join(dir, "codearts2-user.json"), []byte(`{"auth":{"accessToken":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cline-abc.json"), []byte(`{"auth":{"accessToken":"x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	list, err = auth.LoadCodeArtsDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("glob 边界失效：加载到 %d 条（应只收 codearts-*）", len(list))
	}

	// 校验与清洗。
	if _, err := SaveAuth(dir, Result{UID: "", AccessKeyID: "a", SecretAccessKey: "b"}); err == nil {
		t.Error("缺 uid 必须报错")
	}
	if _, err := SaveAuth(dir, Result{UID: "u", AccessKeyID: "", SecretAccessKey: "b"}); err == nil {
		t.Error("缺 AK 必须报错")
	}
	evil, err := SaveAuth(dir, Result{UID: "../evil", AccessKeyID: "a", SecretAccessKey: "b"})
	if err != nil {
		t.Fatalf("SaveAuth(危险 uid): %v", err)
	}
	if strings.Contains(filepath.Base(evil), "..") || strings.Contains(filepath.Base(evil), "/") {
		t.Fatalf("危险 uid 未被清洗: %s", filepath.Base(evil))
	}
}

// TestShutdownIdempotent Shutdown 幂等，且未 Start 时也不 panic。
func TestShutdownIdempotent(t *testing.T) {
	Shutdown()
	Shutdown()
	_, _, _ = startLogin(t)
	Shutdown()
	Shutdown()
	// 关掉后再 Poll：会话已无，返回 Pending（上层会按超时/取消处理）。
	if sess := currentSession(); sess != nil {
		t.Fatal("Shutdown 后不应残留会话")
	}
}

// verifyDPoPProof 用 JWS 头里内嵌的公钥校验 ES256 签名（独立检查 DPoP 实现）。
func verifyDPoPProof(t *testing.T, proof string) map[string]any {
	t.Helper()
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("DPoP 证明不是三段 JWS: %q", proof)
	}
	hRaw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		t.Fatalf("解 JWS 头失败: %v", err)
	}
	var head struct {
		Typ string            `json:"typ"`
		Alg string            `json:"alg"`
		JWK map[string]string `json:"jwk"`
	}
	if err := json.Unmarshal(hRaw, &head); err != nil {
		t.Fatalf("JWS 头不是 JSON: %v", err)
	}
	if head.Typ != "dpop+jwt" || head.Alg != "ES256" {
		t.Fatalf("JWS 头 = %+v", head)
	}
	xb, _ := base64.RawURLEncoding.DecodeString(head.JWK["x"])
	yb, _ := base64.RawURLEncoding.DecodeString(head.JWK["y"])
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		t.Fatalf("签名长度非法: %v len=%d", err, len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("DPoP 签名校验失败")
	}
	pRaw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var payload map[string]any
	if err := json.Unmarshal(pRaw, &payload); err != nil {
		t.Fatalf("DPoP 载荷不是 JSON: %v", err)
	}
	return payload
}

// itoa 十进制转换（避免为测试引入 strconv 的可读性噪音）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
