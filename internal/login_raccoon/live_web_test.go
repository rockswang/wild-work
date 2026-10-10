//go:build live

// live_web_test.go 小浣熊「网页版自动登录」实盘探针。
//
// 它拉起一个**独立 profile** 的 Edge/Chrome，打开小浣熊网页版登录页，然后轮询 Cookie。
// 因为需要人工在浏览器里完成登录，本测试会等待较长时间（默认 10 分钟）。
//
// 运行方式（仓库根目录）：
//
//	go test -tags live ./internal/login_raccoon/ -run TestLiveWebLogin -v -timeout 900s
//
// 验证目标（R44）：
//  1. 浏览器能被拉起、CDP 能读到 Cookie；
//  2. 未登录时读到的是**访客**凭据（owner_type=visitors）——被正确识别为「尚未登录」；
//  3. 用户登录后捕获到 owner_type=users 的凭据，且 refresh 能换出可用 access_token。
package login_raccoon

import (
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/raccoon"
)

// authForTest 把捕获结果包成 auth.Auth（仅内存，不落盘）。
type authForTest = auth.Auth

func TestLiveWebLogin(t *testing.T) {
	if testing.Short() {
		t.Skip("live 探针跳过")
	}
	sess, err := StartWebLogin(nil)
	if err != nil {
		t.Fatalf("拉起浏览器失败（本机是否装了 Edge/Chrome？）: %v", err)
	}
	t.Logf("浏览器已拉起，登录页 = %s", WebLoginURL)
	t.Logf("请在弹出的独立浏览器窗口中完成小浣熊登录（扫码/手机号/微信均可）…")

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		st, serr := sess.Status()
		switch st {
		case "success":
			r, _ := sess.Result()
			t.Logf("✅ 捕获成功：access=%d 字符 refresh=%d 字符 exp=%s",
				len(r.AccessToken), len(r.RefreshToken), time.Unix(r.ExpiresAt, 0).Format("2006-01-02 15:04:05"))
			if r.AccessToken == "" || r.RefreshToken == "" {
				t.Fatal("凭据为空")
			}
			// 用捕获的凭据验证目录/余额（不落盘，避免污染 auths/）
			c := raccoon.New()
			a := &authForTest{AccessToken: r.AccessToken, RefreshToken: r.RefreshToken, ExpiresAt: r.ExpiresAt}
			if ms, err := c.FetchModels(a); err != nil {
				t.Errorf("用捕获凭据拉模型目录失败: %v", err)
			} else {
				t.Logf("✅ 用捕获凭据拉目录成功：%d 个模型", len(ms))
			}
			sess.Cancel()
			return
		case "failed":
			sess.Cancel()
			t.Fatalf("登录失败: %v", serr)
		case "cancelled":
			t.Skip("登录已取消")
		}
		time.Sleep(2 * time.Second)
	}
	sess.Cancel()
	t.Fatal("等待登录超时")
}
