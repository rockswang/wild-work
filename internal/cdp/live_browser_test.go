//go:build live

// live_browser_test.go 对真实 Edge 验证 CDP 全链路。
//
//	go test -tags live -run TestLiveBrowserLaunch ./internal/cdp/ -v -timeout 180s
//
// 验证点：拉起独立 profile 的 Edge → CDP 就绪 → 读回 Cookie（含域名过滤）。
// 不涉及任何账号登录，只验证机制可用。
package cdp

import (
	"context"
	"testing"
	"time"
)

func TestLiveBrowserLaunchAndCookies(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	b, err := Launch(ctx, Options{StartURL: "https://chatglm.cn"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Logf("✅ 浏览器已启动，调试端口=%d", b.Port())
	t.Logf("   profile=%s", b.ProfileDir())
	defer func() {
		if err := b.Close(); err != nil {
			t.Logf("Close: %v", err)
		} else {
			t.Logf("✅ 浏览器已关闭，profile 已清理")
		}
	}()

	// 等页面目标出现并读 Cookie（新 profile 未登录，Cookie 可能为空）
	var cookies []Cookie
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		cookies, err = b.AllCookies(ctx, "chatglm.cn")
		if err == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if err != nil {
		t.Fatalf("AllCookies: %v", err)
	}
	t.Logf("✅ 成功经 CDP 读取 Cookie，共 %d 个", len(cookies))
	for _, c := range cookies {
		v := c.Value
		if c.Name == "chatglm_refresh_token" {
			v = "[已捕获，长度不打印]"
		} else if len(v) > 30 {
			v = v[:30] + "…"
		}
		t.Logf("   %s (domain=%s) = %s", c.Name, c.Domain, v)
	}

	// 关键断言：机制可用（能读到 Cookie 集合，即使为空）
	// 未登录时 chatglm_refresh_token 不应存在——这正是自动捕获的判据
	hasRefresh := false
	for _, c := range cookies {
		if c.Name == "chatglm_refresh_token" && c.Value != "" {
			hasRefresh = true
		}
	}
	if hasRefresh {
		t.Log("⚠️ 全新 profile 竟然有 refresh_token（预期未登录应为空）")
	} else {
		t.Log("✅ 全新 profile 无 refresh_token —— 证明「登录后出现」可作为自动捕获判据")
	}

	// 顺带验证 WaitForCookie 在超时场景下正确报错（用极短超时）
	_, werr := b.WaitForCookie(ctx, "chatglm.cn", "chatglm_refresh_token", 3*time.Second)
	if werr == nil {
		t.Log("⚠️ WaitForCookie 未超时（若真已登录则正常）")
	} else {
		t.Logf("✅ WaitForCookie 超时行为正确: %v", werr)
	}
}
