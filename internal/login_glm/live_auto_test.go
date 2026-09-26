//go:build live

// live_auto_test.go 对真实清言验证「访客凭据」行为——这是自动登录的关键前提。
//
//	go test -tags live -run TestLiveGuestTokenBehavior ./internal/login_glm/ -v -timeout 240s
//
// 背景：清言对新访客会自动下发访客凭据。若自动登录以「Cookie 出现」为完成判据，
// 会抓到一个不可用的访客 token。本测试确认：
//  1. 全新 profile 打开 chatglm.cn 后确实会出现 chatglm_refresh_token（访客）
//  2. 该访客 token 调 user/refresh 会被拒绝（IsGuest / 访客文案）
//
// 只有这两点都成立，才证明「以验证通过为完成判据」是必要的。
package loginglm

import (
	"context"
	"strings"
	"testing"
	"time"

	"wild-work/internal/cdp"
)

func TestLiveGuestTokenBehavior(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	b, err := cdp.Launch(ctx, cdp.Options{StartURL: "https://chatglm.cn"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer b.Close()

	// 等 Cookie 出现（访客凭据）
	var token string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		tok, err := currentRefreshToken(ctx, b)
		if err == nil && tok != "" {
			token = tok
			break
		}
		time.Sleep(2 * time.Second)
	}

	if token == "" {
		t.Skip("全新 profile 未出现 refresh_token（上游行为可能已变，或页面未加载完）")
	}
	t.Logf("✅ 全新 profile 出现了 chatglm_refresh_token（长度 %d）—— 证明「Cookie 出现 ≠ 已登录」", len(token))

	// 尝试验证：访客凭据应被拒绝
	r, verr := Validate(token)
	if verr == nil {
		t.Logf("⚠️ 访客凭据竟然验证通过：uid=%s nickname=%s", r.UID, r.Nickname)
		t.Log("   （若上游不再下发访客凭据，则「Cookie 出现即登录」也成立，可简化实现）")
		return
	}

	t.Logf("✅ 访客凭据验证被拒绝: %v", verr)
	msg := verr.Error()
	if strings.Contains(msg, "访客") || strings.Contains(msg, "guest") || strings.Contains(msg, "401") {
		t.Log("✅ 拒绝原因符合预期（访客/未授权）—— 确认「以验证通过为完成判据」是必要的")
	} else {
		t.Logf("⚠️ 拒绝原因与预期不同，需人工确认: %s", msg)
	}
}
