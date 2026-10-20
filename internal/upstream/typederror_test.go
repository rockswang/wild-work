package upstream

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"wild-work/internal/auth"
)

// 本文件锁定 R79：目录/费率接口遇 HTTP ≥400 必须返回**类型化** *Error。
//
// 为什么必须锁死：上层自愈链（app.refreshIfSessionDead → handler/app 的
// 401 重试）全靠 errors.As 取分类。这两个接口原来返回裸 fmt.Errorf
// （`models api status 401: …`），errors.As 恒 false ⇒ 401 自愈在模型面与
// 费率面**永远不可能生效**：本地 expiresAt 仍在未来而上游已拒绝时
// （token 在别处被轮换 / 上游临时吊销），费率与模型列表只能一直陈旧，
// 直到进程重启或 token 自然过期。
//
// 与「上游是否真有 bug」无关——这是本仓自己的缺陷，2026-10-17 排查
// WorkBuddyCN 401 时顺带发现。

// TestFetchModelsPricingReturnTypedError 两条路径都必须返回 *upstream.Error。
func TestFetchModelsPricingReturnTypedError(t *testing.T) {
	mk := func(status int, body string) *Client {
		return testClient(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})
	}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"models 401", http.StatusUnauthorized, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		{"models 500", http.StatusInternalServerError, `boom`, ErrServer},
		{"pricing 401", http.StatusUnauthorized, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		{"pricing 500", http.StatusInternalServerError, `boom`, ErrServer},
	}
	for _, tc := range cases {
		c := mk(tc.status, tc.body)
		var err error
		if strings.HasPrefix(tc.name, "models") {
			_, err = c.FetchModels(a)
		} else {
			_, err = c.FetchModelPricing(a)
		}
		if err == nil {
			t.Fatalf("%s：应返回错误", tc.name)
		}
		var ue *Error
		if !errors.As(err, &ue) {
			t.Fatalf("%s：必须返回 *upstream.Error（否则上层自愈拿不到分类），实得 %T: %v", tc.name, err, err)
		}
		if ue.Kind != tc.want {
			t.Fatalf("%s：kind=%v want %v", tc.name, ue.Kind, tc.want)
		}
		if ue.Status != tc.status {
			t.Fatalf("%s：status=%d want %d", tc.name, ue.Status, tc.status)
		}
	}
}

// TestFetchModelsGatewayHTML401IsSessionDead 网关 HTML 401（无业务信封）同样要能分类。
//
// 这是唯一需要靠状态码而非业务码判定的形态：body 里没有任何 sessionDeadMarkers
// 文案（只有 openresty 的 HTML），若只看文案会落到 ErrClient。
// 回滚点（R76）的 Classify 已按「所有 401 → ErrSessionDead」处理，本用例锁住它。
func TestFetchModelsGatewayHTML401IsSessionDead(t *testing.T) {
	html := `<html><head><title>401 Authorization Required</title></head>` +
		`<body><center><h1>401 Authorization Required</h1></center><hr><center>openresty</center></body></html>`
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"text/html"}},
			Body:       io.NopCloser(strings.NewReader(html)),
		}, nil
	})
	_, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("必须返回 *upstream.Error，实得 %T: %v", err, err)
	}
	if ue.Kind != ErrSessionDead {
		t.Fatalf("网关 HTML 401 应归 ErrSessionDead（否则 refresh 自愈不触发），实得 %v", ue.Kind)
	}
}
