package loomy

// points_test.go 锁死「余额响应字段全缺 ≠ 余额为 0」。
//
// 2026-11-09 03:34 生产实录：上游偶发回 HTTP 200 但不带余额字段，旧实现当 0 返回，
// 面板余额 36941 → 0，账本把两个池都记成 expire；下一轮回正常又记一条 36941 的
// 「存量额度」earn，形成「过期 + 发放」成对刷屏。真 0 余额仍带字段，不该被拦。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/auth"
)

func newPointsStub(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return &Client{HTTP: srv.Client(), Base: srv.URL}
}

func TestUserResourceDetailRejectsMissingBalanceFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"data 为空对象", `{"code":0,"data":{}}`},
		{"只有业务码", `{"code":0}`},
		{"字段全 null", `{"data":{"balance":null,"dailyBalance":null}}`},
		{"非 JSON", `<html>maintenance</html>`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newPointsStub(t, http.StatusOK, tc.body)
			remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "t"})
			if err == nil {
				t.Fatalf("应返回错误（否则余额会被记成 0）：remain=%d items=%d", remain, len(items))
			}
			if !strings.Contains(err.Error(), "缺少余额字段") && !strings.Contains(err.Error(), "解析失败") {
				t.Fatalf("错误信息应指出协议问题，得到 %v", err)
			}
		})
	}
}

func TestUserResourceDetailAcceptsRealZero(t *testing.T) {
	c := newPointsStub(t, http.StatusOK, `{"code":0,"data":{"balance":0,"dailyBalance":0}}`)
	remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "t"})
	if err != nil {
		t.Fatalf("字段齐全的 0 余额是正常响应，不应报错：%v", err)
	}
	if remain != 0 || len(items) != 2 {
		t.Fatalf("remain=%d items=%d，期望 0 / 2", remain, len(items))
	}
}

func TestUserResourceDetailSumsPools(t *testing.T) {
	c := newPointsStub(t, http.StatusOK, `{"code":0,"data":{"balance":31941,"dailyBalance":5000}}`)
	remain, items, err := c.UserResourceDetail(&auth.Auth{AccessToken: "t"})
	if err != nil {
		t.Fatalf("正常响应不应报错：%v", err)
	}
	if remain != 36941 {
		t.Fatalf("remain=%d，期望 36941（两池求和）", remain)
	}
	if len(items) != 2 {
		t.Fatalf("items=%d，期望 2（积分 + 每日积分）", len(items))
	}
}

// TestUserResourceDetailPrefersAvailableBalance 上游给了 availableBalance 就用它，
// 不再自己求和（口径以上游为准）。
func TestUserResourceDetailPrefersAvailableBalance(t *testing.T) {
	c := newPointsStub(t, http.StatusOK, `{"code":0,"data":{"balance":100,"dailyBalance":50,"availableBalance":140}}`)
	remain, _, err := c.UserResourceDetail(&auth.Auth{AccessToken: "t"})
	if err != nil {
		t.Fatalf("正常响应不应报错：%v", err)
	}
	if remain != 140 {
		t.Fatalf("remain=%d，期望 140（availableBalance 优先）", remain)
	}
}
