// handler_bodylimit_test.go 回归 issue #30 / #176：请求体超上限不得静默截断后误报
// invalid_model，应明确回 413 request_too_large。
//
// 上限沿革 8MiB → 32MiB（#30）→ 128MiB（#176：无状态客户端每轮重放整段历史，
// 12 张截图 33MiB 直接把 32MiB 打穿）。测试一律基于 MaxRequestBody 变量，
// 不写死数字，改上限后仍成立。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// oversizedBody 构造 n 字节的可解析 JSON 请求体（用大量填充消息撑体积）。
func oversizedBody(n int64) []byte {
	// {"model":"workbuddy/m","messages":[{"role":"user","content":"<pad>"}]}
	// padding 放进 content 字符串里，保证整体仍是合法 JSON
	head := `{"model":"workbuddy/test-model","messages":[{"role":"user","content":"`
	tail := `"}]}`
	pad := bytes.Repeat([]byte("a"), int(n)-len(head)-len(tail))
	if pad == nil {
		pad = []byte{}
	}
	return append(append([]byte(head), pad...), tail...)
}

// TestBodyOverLimit413 超上限 1 字节：应回 413 request_too_large，
// 而不是截断后误报 invalid_model（issue #30 的字节级复现场景）。
func TestBodyOverLimit413(t *testing.T) {
	h := NewHandler(Config{APIKey: ""})
	body := oversizedBody(MaxRequestBody + 1)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s want 413", rec.Code, rec.Body.String())
	}
	var errResp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &errResp)
	if errResp.Error.Code != "request_too_large" {
		t.Errorf("code=%q want request_too_large", errResp.Error.Code)
	}
	// 报错文案必须指向「请求体过大」，不得出现 invalid_model（误导排查）
	if strings.Contains(rec.Body.String(), "invalid_model") {
		t.Errorf("不得误报 invalid_model: %s", rec.Body.String())
	}
}

// TestBodyAtLimitOK 恰好等于上限：未超限，应正常通过读取阶段
// （无上游配置时停在账号选择失败，但绝不能是 413/invalid_model）。
func TestBodyAtLimitOK(t *testing.T) {
	h := NewHandler(Config{APIKey: ""})
	body := oversizedBody(MaxRequestBody)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("恰好上限不应回 413: %s", rec.Body.String())
	}
	// 无 runtime 配置时停在 "provider not configured" 属正常；
	// 只断言不得发生静默截断（截断后必然报 invalid_model 前缀格式错误或 invalid JSON）
	if strings.Contains(rec.Body.String(), "must use explicit prefix") || strings.Contains(rec.Body.String(), "invalid JSON") {
		t.Errorf("合法 JSON 不应报截断后的解析错误: %s", rec.Body.String())
	}
}

// TestBodyInvalidJSON413 非法 JSON：报 invalid_request 而非 invalid_model。
func TestBodyInvalidJSON413(t *testing.T) {
	h := NewHandler(Config{APIKey: ""})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model": truncat`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid JSON") || strings.Contains(rec.Body.String(), "invalid_model") {
		t.Errorf("应报 invalid JSON 而非 invalid_model: %s", rec.Body.String())
	}
}
