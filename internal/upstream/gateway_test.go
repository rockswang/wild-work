package upstream

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"wild-work/internal/auth"
)

// gateway401HTML 是上游 APISIX 网关在认证失败时返回的真实页面（2026-10-17 现场抓取，
// 两个 WorkBuddy 账号 refresh token 被吊销后，账单/目录/对话接口一律回这个）。
const gateway401HTML = `<html>
<head><title>401 Authorization Required</title></head>
<body>
<center><h1>401 Authorization Required</h1></center>
<hr><center>openresty</center>
<p><em>Powered by <a href="https://apisix.apache.org/">APISIX</a>.</em></p></body>
</html>`

// TestClassifyGatewayAuthPage 网关 HTML 401 必须归 ErrSessionDead。
//
// 为什么这条必须锁死：归成 ErrClient 时 `app.refreshIfSessionDead` / handler 的
// `recoverSession` 都不会触发，401 自愈静默失效 —— 账号 token 在别处被轮换后，
// 面板积分、模型列表、费率全部永久陈旧，且日志里只有一条「http 401」。
func TestClassifyGatewayAuthPage(t *testing.T) {
	if got := Classify(http.StatusUnauthorized, gateway401HTML); got != ErrSessionDead {
		t.Fatalf("网关 HTML 401 应归 ErrSessionDead，实得 %v", got)
	}
	// 业务码形态不受影响（原本就正确）
	if got := Classify(http.StatusUnauthorized, `{"code":12153,"msg":"Offline user session not found"}`); got != ErrSessionDead {
		t.Fatalf("业务码 12153 仍应为 ErrSessionDead，实得 %v", got)
	}
	// R76 把**所有** 401 统一归 ErrSessionDead（原 `{"code":9999,"msg":"bad token"}`
	// 归 ErrClient 会走错惩罚路径）。故这里断言的是「401 无例外」，而
	// IsGatewayAuthPage 只负责「要不要把 Msg 换成可行动文案」。
	if got := Classify(http.StatusUnauthorized, `{"code":9999,"msg":"bad token"}`); got != ErrSessionDead {
		t.Fatalf("业务 401 也应归 ErrSessionDead（R76），实得 %v", got)
	}
	// 反例：403 的网关页不是「登录态失效」的判据（403 有独立语义，交 WAF 族处理）
	if got := Classify(http.StatusForbidden, gateway401HTML); got == ErrSessionDead {
		t.Fatal("403 不得归 ErrSessionDead（会把账号永久 Disable）")
	}
}

// TestIsGatewayAuthPage 判据必须窄而确定：宁可漏判退回 ErrClient，也不误判成
// ErrSessionDead —— 后者会 pool.Disable 永久禁用（不变量 19/22 的代价不对称）。
func TestIsGatewayAuthPage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"真实 APISIX 401 页", gateway401HTML, true},
		{"紧凑形态", `<html><head><title>401 Authorization Required</title></head></html>`, true},
		{"业务 JSON 带 code", `{"code":12153,"msg":"401 Authorization Required"}`, false},
		{"JSON 里含 html 字样", `{"error":"<html>401 Authorization Required</html>"}`, false},
		{"空体", ``, false},
		{"纯文本", `401 Authorization Required`, false},
		{"其它 HTML 错误页", `<html><body>502 Bad Gateway</body></html>`, false},
		{"上游自己的 401 JSON", `{"message":"Unauthorized"}`, false},
	}
	for _, c := range cases {
		if got := IsGatewayAuthPage(c.body); got != c.want {
			t.Errorf("%s: IsGatewayAuthPage=%v want %v（body=%q）", c.name, got, c.want, c.body)
		}
	}
}

// TestErrorFromBodyGatewayHint 网关页的 Msg 必须换成可行动文案：
// 原始 HTML 会被 shortErr 截成半截 `<html><head><title>401 Authori…`，
// 面板 toast 与日志都读不出「该重登」这个结论。
func TestErrorFromBodyGatewayHint(t *testing.T) {
	err := errorFromBody(http.StatusUnauthorized, gateway401HTML)
	if err.Kind != ErrSessionDead {
		t.Fatalf("kind=%v want ErrSessionDead", err.Kind)
	}
	if err.Msg != GatewayAuthHint {
		t.Fatalf("Msg 应为可行动文案，实得 %q", err.Msg)
	}
	if strings.Contains(err.Msg, "<html") {
		t.Error("Msg 不得包含原始 HTML")
	}
	// 业务错误保留原文（不吞上游细节，不变量 8）
	biz := errorFromBody(http.StatusTooManyRequests, `{"code":6004,"msg":"重置时间 2026-10-18 11:07:20"}`)
	if biz.Kind != ErrSoftRate || !strings.Contains(biz.Msg, "6004") {
		t.Fatalf("业务错误应保留原文，实得 kind=%v msg=%q", biz.Kind, biz.Msg)
	}
}

// TestFetchModelsPricingReturnTypedError 目录/费率接口遇 401 必须返回**类型化**错误。
//
// 修复前它们返回裸 `fmt.Errorf("models api status 401: …")`，`errors.As` 取不到
// *provider.Error ⇒ 上层 401 自愈（不变量 19）在模型面与费率面从来不生效，
// 面板费率/模型只能等 token 自然过期后重登才恢复。
func TestFetchModelsPricingReturnTypedError(t *testing.T) {
	page := func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(gateway401HTML)),
		}, nil
	}
	c := testClient(page)
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	for name, call := range map[string]func() error{
		"FetchModels":       func() error { _, err := c.FetchModels(a); return err },
		"FetchModelPricing": func() error { _, err := c.FetchModelPricing(a); return err },
	} {
		err := call()
		if err == nil {
			t.Fatalf("%s 应返回错误", name)
		}
		var ue *Error
		if !errors.As(err, &ue) {
			t.Fatalf("%s 必须返回 *upstream.Error（否则上层自愈拿不到分类），实得 %T: %v", name, err, err)
		}
		if ue.Kind != ErrSessionDead {
			t.Fatalf("%s kind=%v want ErrSessionDead", name, ue.Kind)
		}
	}
}
