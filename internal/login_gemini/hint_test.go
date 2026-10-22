package login_gemini

import (
	"strings"
	"testing"

	"wild-work/internal/gemini"
)

// TestGeminiErrorHint 已知上游 OAuth 错误必须带可行动引导（issue #167）：
// Google 已停用旧 CLI client 对个人版 Code Assist 的授权，用户会按官方文案去装 CLI，
// 反复撞墙——引导文案要直说「上游政策、非本工具问题」。
func TestGeminiErrorHint(t *testing.T) {
	cases := []struct {
		envelope string
		wantSub  string // 引导里必须出现的关键词
	}{
		{"invalid_request: This client is no longer supported for Gemini Code Assist for individuals", "上游政策"},
		{"restricted_client: Unregistered scope(s) in the request", "scope"},
		{"access_denied: User denied the request", "拒绝"},
		{"invalid_grant: Code was already redeemed", ""}, // 未知错误不带引导
	}
	for _, c := range cases {
		hint := geminiErrorHint(c.envelope)
		if c.wantSub == "" {
			if hint != "" {
				t.Errorf("%q 不应带引导，实际 %q", c.envelope, hint)
			}
			continue
		}
		if !strings.Contains(hint, c.wantSub) {
			t.Errorf("%q 的引导缺关键词 %q：%q", c.envelope, c.wantSub, hint)
		}
	}
}

// TestScopesAntigravity 验证 OAuth scope 清单为最小必要集合（防 403 restricted_client）。
func TestScopesAntigravity(t *testing.T) {
	joined := strings.Join(gemini.Scopes, " ")
	for _, required := range []string{
		gemini.ScopeOpenID,
		gemini.ScopeCloudPlatform,
		gemini.ScopeUserInfoEmail,
		gemini.ScopeUserInfoProfile,
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("Scopes 缺少必要 scope %s", required)
		}
	}
	// 严禁包含 ScopeCCLog 和 ScopeExperiments，否则触发 Google OAuth 403 restricted_client
	for _, forbidden := range []string{
		gemini.ScopeCCLog,
		gemini.ScopeExperiments,
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("Scopes 包含未登记/会引发 restricted_client 的 forbidden scope: %s", forbidden)
		}
	}
}
