package workbuddyai

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/upstream"
)

// gateway401HTML 上游 APISIX 网关认证拦截页（与国内版同款，2026-10-17 现场）。
const gateway401HTML = `<html>
<head><title>401 Authorization Required</title></head>
<body>
<center><h1>401 Authorization Required</h1></center>
<hr><center>openresty</center>
<p><em>Powered by <a href="https://apisix.apache.org/">APISIX</a>.</em></p></body>
</html>`

// TestClassifyGatewayAuthPage 国际版同样要认网关 HTML 401（两渠道语义必须一致）。
// 不认时 ErrClient 一路静默：积分/模型/费率全部陈旧且不触发 refresh。
func TestClassifyGatewayAuthPage(t *testing.T) {
	if got := Classify(http.StatusUnauthorized, gateway401HTML); got != provider.ErrSessionDead {
		t.Fatalf("网关 HTML 401 应归 ErrSessionDead，实得 %v", got)
	}
	if got := Classify(http.StatusUnauthorized, `{"code":9999,"msg":"bad token"}`); got != provider.ErrClient {
		t.Fatalf("业务 401 应保持 ErrClient，实得 %v", got)
	}
}

// TestDoJSONGateway401Typed doJSON 遇网关页要返回类型化错误 + 可行动文案。
func TestDoJSONGateway401Typed(t *testing.T) {
	c := &Client{HTTP: &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(gateway401HTML)),
		}, nil
	})}}
	req, _ := http.NewRequest(http.MethodGet, "https://example.invalid/x", nil)
	_, err := c.doJSON(req)
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("应返回 *provider.Error，实得 %T: %v", err, err)
	}
	if pe.Kind != provider.ErrSessionDead {
		t.Fatalf("kind=%v want session_dead", pe.Kind)
	}
	if pe.Msg != upstream.GatewayAuthHint {
		t.Fatalf("Msg 应为可行动文案，实得 %q", pe.Msg)
	}
}

// TestCatalogGateway401Typed 目录/费率接口（直连 HTTP，不经 doJSON）同样要类型化。
//
// 修复前这两条路径返回裸 fmt.Errorf ⇒ errors.As 取不到分类 ⇒ 401 自愈在
// 模型面/费率面从来不生效（不变量 19 的同族缺口）。
func TestCatalogGateway401Typed(t *testing.T) {
	c := &Client{HTTP: &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(gateway401HTML)),
		}, nil
	})}}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	for name, call := range map[string]func() error{
		"fetchCatalog":   func() error { _, err := c.fetchCatalog(a); return err },
		"fetchV3Catalog": func() error { _, err := c.fetchV3Catalog(a); return err },
	} {
		err := call()
		var pe *provider.Error
		if !errors.As(err, &pe) {
			t.Fatalf("%s 必须返回 *provider.Error，实得 %T: %v", name, err, err)
		}
		if pe.Kind != provider.ErrSessionDead {
			t.Fatalf("%s kind=%v want session_dead", name, pe.Kind)
		}
	}
}

// rtFunc 便于在测试里内联 Transport。
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
