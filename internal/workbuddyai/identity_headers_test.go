// identity_headers_test.go 锁定国际版出站的「客户端身份链」（R117）。
//
// 与国内版同源：官方桌面 5.8.3 内嵌 CLI 2.161.0（本机 /Applications/WorkBuddy.app
// 静态解包取证）。两区分开各测一份，互不依赖。
package workbuddyai

import "testing"

// TestChatIdentityChainWBAI 官方桌面 5.8.3 + 内嵌 CLI 2.161.0 的完整身份串。
func TestChatIdentityChainWBAI(t *testing.T) {
	got := captureChat(t, "u1", convBody)

	if ua := got.Get("User-Agent"); ua != "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0" {
		t.Fatalf("User-Agent = %q，与官方桌面形态不符", ua)
	}
	want := map[string]string{
		"X-IDE-Name":      "WorkBuddy",
		"X-IDE-Type":      "WorkBuddy",
		"X-IDE-Version":   "5.8.3",
		"X-Product":       "SaaS",
		"X-Private-Data":  "true",
		"X-Agent-Intent":  "craft",
		"X-Agent-Purpose": "conversation",
	}
	for k, v := range want {
		if got := got.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if got.Get("X-Machine-ID") != "" || got.Get("X-Session-ID") != "" {
		t.Errorf("官方 chat 请求不发 X-Machine-ID / X-Session-ID")
	}
}
