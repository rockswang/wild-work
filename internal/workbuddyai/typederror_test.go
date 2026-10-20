package workbuddyai

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// R79（国际版同款）：目录/费率接口遇 HTTP ≥400 必须返回**类型化** *provider.Error，
// 否则上层 refreshIfSessionDead 的 errors.As 恒 false，401 自愈在模型面永远不生效。
func TestCatalogReturnTypedError(t *testing.T) {
	// fetchCatalog/fetchV3Catalog 共用**包级**缓存（10min TTL），其它用例可能已把它填满。
	// 这里显式清空，否则请求根本不会发出，用例恒「成功」而测不到任何东西。
	resetCatalogCache(t)

	c := &Client{HTTP: &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"Offline user session not found"}`)),
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
			t.Fatalf("%s：必须返回 *provider.Error（否则自愈拿不到分类），实得 %T: %v", name, err, err)
		}
		if pe.Kind != provider.ErrSessionDead {
			t.Fatalf("%s：kind=%v want session_dead", name, pe.Kind)
		}
	}
}

// resetCatalogCache 清空包级目录缓存（测试专用）。
func resetCatalogCache(t *testing.T) {
	t.Helper()
	cache.mu.Lock()
	cache.models = nil
	cache.promo = nil
	cache.badges = nil
	cache.colors = nil
	cache.fetched = time.Time{}
	cache.v3 = nil
	cache.v3OK = false
	cache.v3Fetched = time.Time{}
	cache.mu.Unlock()
	t.Cleanup(func() {
		cache.mu.Lock()
		cache.models = nil
		cache.v3 = nil
		cache.v3OK = false
		cache.fetched = time.Time{}
		cache.v3Fetched = time.Time{}
		cache.mu.Unlock()
	})
}

// rtFunc 便于在测试里内联 Transport。
type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
