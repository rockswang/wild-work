package login_qwenwork

// device_flow_test.go 锁死设备流的形状：入口参数、轮询语义、绑定校验。
//
// 这一层特别容易「静默回归」：把 /device/selectAccounts 换回自拼的 /oauth2/auth，
// 代码照样编译、用例不报错，只有真去登录才会撞 INVALID_OAUTH_CALLBACK。

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildDeviceURL 入口必须是 /device/selectAccounts 且带齐 6 个参数。
func TestBuildDeviceURL(t *testing.T) {
	raw := buildDeviceURL("chal-1", "nonce-1", "machine-1")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("URL 无法解析: %v", err)
	}
	if u.Scheme != "https" || u.Host != "gateway.qwenwork.cn" || u.Path != DevicePath {
		t.Fatalf("入口地址不对: %s", raw)
	}
	for k, want := range map[string]string{
		"challenge":        "chal-1",
		"challenge_method": "S256",
		"nonce":            "nonce-1",
		"machine_id":       "machine-1",
		"client_id":        ClientID,
		"redirect_uri":     RedirectURI,
	} {
		if got := u.Query().Get(k); got != want {
			t.Errorf("%s = %q，期望 %q", k, got, want)
		}
	}
}

// TestStartWritesState Start 只拼地址（不预取，否则用户浏览器会撞 DEVICE_FLOW_REPLAYED），
// 并把 verifier/challenge/nonce 落到 state 文件供 Poll 使用。
func TestStartWritesState(t *testing.T) {
	sp := filepath.Join(t.TempDir(), "login-state-qwenwork.json")

	authURL, err := Start(nil, sp)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	st, err := readState(sp)
	if err != nil {
		t.Fatalf("读 state: %v", err)
	}
	if st.Nonce == "" || st.Verifier == "" || st.Challenge == "" || st.MachineID == "" {
		t.Fatalf("state 字段缺失: %+v", st)
	}
	if st.AuthURL != authURL {
		t.Error("state.AuthURL 与返回值不一致")
	}
	if !strings.Contains(authURL, st.Nonce) || !strings.Contains(authURL, st.Challenge) {
		t.Errorf("入口地址未带 nonce/challenge: %s", authURL)
	}
	// challenge 必须是 verifier 的 S256（把 makePKCE 的配对关系钉死）
	if want := pkceChallenge(st.Verifier); st.Challenge != want {
		t.Error("challenge 不是 verifier 的 S256")
	}
}

// TestMachineIDStable 机器标识必须跨登录复用（否则账号设备列表里会堆重复项）。
func TestMachineIDStable(t *testing.T) {
	sp := filepath.Join(t.TempDir(), "login-state-qwenwork.json")
	a, err := ensureMachineID(sp)
	if err != nil {
		t.Fatalf("ensureMachineID: %v", err)
	}
	b, err := ensureMachineID(sp)
	if err != nil {
		t.Fatalf("ensureMachineID 第二次: %v", err)
	}
	if a != b {
		t.Fatalf("机器标识不稳定: %q vs %q", a, b)
	}
}

// TestPollPending 404（x-device-flow-state: pending）必须映射成 ErrPending，而不是失败。
func TestPollPending(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != PollPath {
			t.Errorf("轮询路径不对: %s", r.URL.Path)
		}
		if r.URL.Query().Get("challenge_method") != "S256" {
			t.Error("challenge_method 必须是 S256")
		}
		w.Header().Set("x-device-flow-state", "pending")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	defer swapGateway(srv.URL)()

	sp, _ := writeTestState(t)
	if _, err := Poll(nil, sp); !errors.Is(err, ErrPending) {
		t.Fatalf("404 应返回 ErrPending，得到 %v", err)
	}
	if _, statErr := os.Stat(sp); statErr != nil {
		t.Error("pending 时不得删除 state 文件")
	}
}

// TestPollSuccess 200 时按 token/refresh_token 组结果，并从 JWT 解出 uid。
func TestPollSuccess(t *testing.T) {
	jwt := makeJWT(t, `{"user_id":"uid-9","username":"nick"}`)
	sp, st := writeTestState(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":                    jwt,
			"refresh_token":            "rt-1",
			"expires_in":               604800,
			"refresh_token_expires_in": 3600,
			"nonce":                    st.Nonce,
			"code_challenge":           st.Challenge,
			"code_challenge_method":    "S256",
		})
	}))
	defer srv.Close()
	defer swapGateway(srv.URL)()

	res, err := Poll(nil, sp)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if res.AccessToken != jwt || res.RefreshToken != "rt-1" || res.ExpiresIn != 604800 {
		t.Fatalf("结果字段不对: %+v", res)
	}
	if res.UID != "uid-9" || res.Nickname != "nick" {
		t.Fatalf("JWT 身份解析不对: uid=%q nick=%q", res.UID, res.Nickname)
	}
	if _, statErr := os.Stat(sp); !os.IsNotExist(statErr) {
		t.Error("成功后应删除 state 文件")
	}
}

// TestPollBindingMismatch 响应 nonce 与本次不一致时必须报错（防拿到别人的凭据）。
func TestPollBindingMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": makeJWT(t, `{"sub":"u"}`), "nonce": "someone-else",
		})
	}))
	defer srv.Close()
	defer swapGateway(srv.URL)()

	sp, _ := writeTestState(t)
	if _, err := Poll(nil, sp); err == nil || !strings.Contains(err.Error(), "nonce") {
		t.Fatalf("nonce 不匹配应报错，得到 %v", err)
	}
}

// TestPollExpired 超过 FlowTTL 直接失败并清理，不再打上游。
func TestPollExpired(t *testing.T) {
	sp, st := writeTestState(t)
	st.CreatedAt = st.CreatedAt - int64(FlowTTL.Seconds()) - 1
	if err := writeState(sp, st); err != nil {
		t.Fatal(err)
	}
	if _, err := Poll(nil, sp); err == nil || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("过期应报超时，得到 %v", err)
	}
}

// TestSaveAuthExpiryFromJWT 到期时间取 JWT exp（服务端判据），缺失才回退 expires_in。
func TestSaveAuthExpiryFromJWT(t *testing.T) {
	jwt := makeJWT(t, `{"user_id":"uid-1","exp":1794290137}`)
	fp, err := SaveAuth(t.TempDir(), Result{AccessToken: jwt, RefreshToken: "rt", UID: "uid-1"})
	if err != nil {
		t.Fatalf("SaveAuth: %v", err)
	}
	var doc struct {
		Auth struct {
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"auth"`
	}
	raw, _ := os.ReadFile(fp)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Auth.ExpiresAt != 1794290137 {
		t.Fatalf("expiresAt = %d，期望取 JWT exp", doc.Auth.ExpiresAt)
	}
}

// ---- 测试辅助 ----

func swapGateway(base string) func() {
	old := gatewayBase
	gatewayBase = base
	return func() { gatewayBase = old }
}

// writeTestState 用真实 PKCE 对写一份 state，返回路径与内容。
func writeTestState(t *testing.T) (string, state) {
	t.Helper()
	verifier, challenge := makePKCE()
	sp := filepath.Join(t.TempDir(), "login-state-qwenwork.json")
	st := state{
		Verifier: verifier, Challenge: challenge,
		Nonce: uuid4Hex(), MachineID: "m-1", CreatedAt: time.Now().Unix(),
	}
	if err := writeState(sp, st); err != nil {
		t.Fatal(err)
	}
	return sp, st
}

func makeJWT(t *testing.T, payload string) string {
	t.Helper()
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return enc(`{"alg":"HS256"}`) + "." + enc(payload) + ".sig"
}
