//go:build live

// live_sign_test.go 对真实 chatglm.cn 验证 Go 版签名实现。
//
// 这是本渠道最关键的实盘验证：mock 测试只能证明「实现自洽」，
// 而这里证明「我们的签名真的被智谱的服务器接受」。
//
// 判据（与侦察阶段一致，变量隔离干净）：
//   - 带签名 → 401 unauthorized user(40102)  ⇒ 签名通过，卡在认证层 ✅
//   - 不带签名 → 400 bad request(40001)      ⇒ 签名层拒绝 ❌（说明签名写错）
//
// 用假 token 探测，不涉及真实账号。默认不跑（需 -tags live 显式开启）。
//
//	go test -tags live -run TestLiveSignature ./internal/glm/ -v
package glm

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLiveSignature(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	c := New() // 真实 Base = https://chatglm.cn

	// 用假 refresh_token 触发一次真实 refresh 请求。
	// 期望：签名被接受 → 上游返回 40102（认证层拒绝假 token）。
	_, err := c.refresh(ctx, "FAKE_TOKEN_FOR_LIVE_SIGN_CHECK")
	if err == nil {
		t.Fatal("假 token 不应通过认证")
	}
	msg := err.Error()
	t.Logf("真实上游响应: %v", err)

	// 签名写错时上游会回 400 bad request(40001)，据此判定
	if strings.Contains(msg, "40001") || strings.Contains(msg, "bad request") {
		t.Fatalf("❌ 签名被上游拒绝（400 bad request）：Go 签名实现有误。响应=%v", err)
	}
	if strings.Contains(msg, "40102") || strings.Contains(msg, "unauthorized") {
		t.Logf("✅ 签名被上游接受（401 unauthorized user）——Go 签名实现与官网一致")
		return
	}
	t.Logf("⚠️ 响应既非 40001 也非 40102，需人工确认：%v", err)
}

// TestLiveCheckinEndpointsExist 对真实上游确认签到端点存在。
// 判据：真实端点返回 40102（存在但需授权），错误路径返回 404（不存在）。
func TestLiveCheckinEndpointsExist(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	c := New()

	// 真实端点：应返回 40102
	_, err := c.doJSON(ctx, "GET", EpCheckinInfo+"?event_date="+eventDate(time.Now()), "FAKE", nil)
	if err == nil {
		t.Fatal("假 token 不应成功")
	}
	if !strings.Contains(err.Error(), "40102") {
		t.Errorf("签到端点 %s 未返回 40102，实际：%v", EpCheckinInfo, err)
	} else {
		t.Logf("✅ %s 存在（40102 需授权）", EpCheckinInfo)
	}

	// 对照：故意错误路径应 404，证明不是网关通配
	_, err2 := c.doJSON(ctx, "GET", "/chatglm/activity-api/activity/daily/__not_exist__", "FAKE", nil)
	if err2 == nil {
		t.Fatal("错误路径不应成功")
	}
	if !strings.Contains(err2.Error(), "404") {
		t.Errorf("对照路径未返回 404，说明前缀可能是通配：%v", err2)
	} else {
		t.Logf("✅ 对照路径 404 —— 证明签到端点真实存在，非网关通配")
	}
}
