package chatgpt

import (
	"errors"
	"testing"

	"wild-work/internal/provider"
)

// TestClassifyFullTable 锁住 spec §10 的错误分类全表（顺序敏感）。
func TestClassifyFullTable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		{"401 会话失效", 401, `{"detail":"not login"}`, provider.ErrSessionDead},
		{"403 异常活动 → 账号级", 403, `{"detail":"Unusual activity has been detected from your device"}`, provider.ErrAccountFault},
		{"403 其它 → 原文透传", 403, `<html>cloudflare</html>`, provider.ErrPassthrough},
		{"413 输入超限", 413, `{"detail":"input_too_large"}`, provider.ErrPromptTooLong},
		{"200 内嵌超限文案", 200, `{"detail":{"code":"message_length_exceeds_limit"}}`, provider.ErrPromptTooLong},
		{"伪装 404 的请求质量不足", 404, `{"detail":"Model not found"}`, provider.ErrUpstream},
		{"429 限流", 429, `{"detail":"rate limited"}`, provider.ErrSoftRate},
		{"5xx 不换号", 502, `bad gateway`, provider.ErrServer},
		{"402 权益不足", 402, `{"detail":"payment required"}`, provider.ErrHardCredit},
		{"其它 4xx", 400, `{"detail":"bad request"}`, provider.ErrClient},
		{"普通 404", 404, `{"detail":"no such route"}`, provider.ErrNotFound},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("%s: Classify(%d, %q) = %v，期望 %v", c.name, c.status, c.body, got, c.want)
		}
	}
}

// TestClassify429BeatsHardMarkers 不变量 15：限流报文高频带余额/额度字样，
// 429 必须优先于任何文案判定（否则限流被当成余额耗尽 → 长冷却）。
func TestClassify429BeatsHardMarkers(t *testing.T) {
	body := `{"detail":"quota exceeded, payment required, 余额不足, input_too_large"}`
	if got := Classify(429, body); got != provider.ErrSoftRate {
		t.Fatalf("429 + 余额/超限文案 = %v，期望 soft_rate（429 优先）", got)
	}
}

// TestStatusErrorIsTyped 目录/费率面的 401 自愈依赖 `errors.As(*provider.Error)`（R79）。
func TestStatusErrorIsTyped(t *testing.T) {
	err := StatusError("GET", "/backend-api/models", 401, []byte(`{"detail":"not login"}`))
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("StatusError 不是 *provider.Error: %T", err)
	}
	if pe.Kind != provider.ErrSessionDead || pe.Status != 401 {
		t.Fatalf("Kind/Status = %v/%d", pe.Kind, pe.Status)
	}
	if pe.Msg == "" {
		t.Error("Msg 不应为空")
	}
}
