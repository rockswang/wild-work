// identity_headers_test.go 锁定国内版出站的「客户端身份链」（R117）。
//
// 取证源：本机 /Applications/WorkBuddy.app 5.8.3 静态解包（main/sidecar-manager.js 的
// getCachedBaseEnv + cli.mjs 的 buildUserAgent/injectAttribution + cli/product.json）。
// 断言的每一个值都是从官方客户端代码里逐字取出的，不是猜的。
package upstream

import (
	"net/http"
	"testing"

	"wild-work/internal/auth"
)

// TestChatIdentityChain 官方桌面 5.8.3 + 内嵌 CLI 2.161.0 的完整身份串。
func TestChatIdentityChain(t *testing.T) {
	got := captureCNChat(t, "u1", cnConvBody)

	if ua := got.Get("User-Agent"); ua != "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0" {
		t.Fatalf("User-Agent = %q，与官方桌面形态不符", ua)
	}
	want := map[string]string{
		"X-IDE-Name":      "WorkBuddy",
		"X-IDE-Type":      "WorkBuddy",
		"X-IDE-Version":   "5.8.3", // 桌面版本，不是 CLI 版本
		"X-Product":       "SaaS",  // cli/product.json 的 deploymentType
		"X-Private-Data":  "true",
		"X-Agent-Intent":  "craft",
		"X-Agent-Purpose": "conversation",
	}
	for k, v := range want {
		if got := got.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if got.Get("X-CodeBuddy-Request") != "1" {
		t.Errorf("X-CodeBuddy-Request 缺失")
	}
}

// TestChatHasNoInventedIdentityHeaders 官方 chat 请求不发 X-Machine-ID / X-Session-ID
// （cli.mjs 只声明 SESSION_ID_HEADER 不赋值，桌面宿主也不发）；这两个头是本仓早年
// 自己造的，取值形态（36 位裸 hex）与真实 IOPlatformUUID（带连字符）还不一致。
func TestChatHasNoInventedIdentityHeaders(t *testing.T) {
	got := captureCNChat(t, "u1", cnConvBody)
	for _, k := range []string{"X-Machine-ID", "X-Session-ID"} {
		if v := got.Get(k); v != "" {
			t.Errorf("%s 不应出现（官方客户端不发）：%q", k, v)
		}
	}
}

// TestSessionFamilyHeadersNotOnRefresh refresh 只带 refresh token 与归属头，
// 不得携带会话头族（与 R116 的爆炸半径断言同源）。
func TestSessionFamilyHeadersNotOnRefresh(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://chat.example/x", nil)
	RefreshHeaders(req, &auth.Auth{Kind: "workbuddy", RefreshToken: "rt", UID: "u1"})
	if req.Header.Get("X-Conversation-ID") != "" {
		t.Fatalf("refresh 不得携带会话头族")
	}
	if req.Header.Get("User-Agent") != "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0" {
		t.Fatalf("refresh 的 UA 也必须是同一身份链")
	}
}
