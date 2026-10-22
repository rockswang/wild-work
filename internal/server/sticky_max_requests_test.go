package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// TestStickyMaxRequestsFromConfig 粘性轮换阈值来自 Handler 配置，而不是包内硬编码。
func TestStickyMaxRequestsFromConfig(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "A", AccessToken: "at-A", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	p.Add(&auth.Auth{UID: "B", AccessToken: "at-B", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	p.SetCreditDetail("A", 1000, 0, 0, time.Time{})
	p.SetCreditDetail("B", 500, 0, 0, time.Time{})
	up := &kindedUpstream{status: http.StatusOK}
	up.succeed = func(string) bool { return true }
	rt := &Runtime{Kind: provider.GLM, Pool: p, Upstream: up}
	h := NewHandler(Config{
		Runtimes:          map[provider.Kind]*Runtime{provider.GLM: rt},
		StickyMaxRequests: 2,
	})

	if got := h.pickWithSticky(rt, "m"); got == nil || got.UID != "A" {
		t.Fatalf("首次应选余额更高的 A，实际 %+v", got)
	}
	if got := h.StickySnapshot()["glm"]; got.UID != "A" || got.Max != 2 {
		t.Fatalf("粘性记录应使用配置阈值 2，实际 %+v", got)
	}

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"glm/m","messages":[{"role":"user","content":"hi"}],"stream":false}`))
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次请求应成功，实际 status=%d body=%s", i+1, rec.Code, rec.Body.String())
		}
	}
	h.stickyMu.RLock()
	count := h.sticky[rt.Kind.String()].reqCount
	h.stickyMu.RUnlock()
	if count != 2 {
		t.Fatalf("连续成功 2 次后粘性计数应达到阈值 2")
	}
	if got := h.pickWithSticky(rt, "m"); got == nil {
		t.Fatal("阈值耗尽后应重新选号")
	}
	if got := h.StickySnapshot()["glm"]; got.Max != 2 || got.Count != 0 {
		t.Fatalf("阈值耗尽后应重建计数并继续使用配置阈值 2，实际 %+v", got)
	}
}

// TestStickyMaxRequestsDefaultForLegacyHandlerCall 直接构造 Handler 的旧调用方
// 未传新字段时仍保持原有默认 50。
func TestStickyMaxRequestsDefaultForLegacyHandlerCall(t *testing.T) {
	h := NewHandler(Config{})
	if h.cfg.StickyMaxRequests != config.DefaultStickyMaxRequests {
		t.Fatalf("未注入时应回落默认值 %d，实际 %d",
			config.DefaultStickyMaxRequests, h.cfg.StickyMaxRequests)
	}
}
