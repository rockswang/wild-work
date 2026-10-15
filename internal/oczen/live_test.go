//go:build live

// live_test.go 对真实 opencode.ai/zen 验证 oczen 免费档会话头端到端可用。
//
// mock 测试只能证明「实现自洽」；这里证明「我们签发的会话头真的被 Zen 免费档
// 接受」——2026-10-13 的 403 FreeTierError 就是「摘要段时间戳段被拒」这一
// 编码面假设在真线上不成立（见上游客服 PR #69/#70）。默认不跑：
//
//	OCZEN_LIVE=1 go test -tags live ./internal/oczen/ -v
package oczen

import (
	"io"
	"os"
	"strings"
	"testing"
)

// TestLiveFreeTierSession 证明 shipped 的 descending 编码经 ChatStream 端到端
// 通过免费档闸门（不再回 403 FreeTierError）。
func TestLiveFreeTierSession(t *testing.T) {
	if os.Getenv("OCZEN_LIVE") == "" {
		t.Skip("set OCZEN_LIVE=1 to run live probe")
	}
	c := New()
	body := []byte(`{"model":"big-pickle","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	rc, status, raw, err := c.ChatStream(AnonymousAuth(), body)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if rc != nil {
		defer rc.Close()
	}
	if status != 200 {
		if len(raw) > 400 {
			raw = raw[:400]
		}
		t.Fatalf("live status=%d body=%s", status, raw)
	}
	got, _ := io.ReadAll(io.LimitReader(rc, 4096))
	if strings.Contains(string(got), "FreeTierError") {
		t.Fatalf("still FreeTierError: %s", got)
	}
	t.Logf("live OK: status=%d bytes=%d", status, len(got))
}
