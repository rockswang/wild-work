package modelscope

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// captureLog 抓取本包日志输出（默认串行执行，无并发污染）。
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

// TestNoTokenInLogs 零 token 落盘纪律（不变量 2）：目录降级、对话错误等**任何**
// 渠道侧日志都不得出现 `ms-` token 明文（面板/日志/消息框同标准）。
// 这里用带 `ms-` 前缀的假 token 走一遍会打日志的路径。
func TestNoTokenInLogs(t *testing.T) {
	const secret = "ms-0123456789abcdef0123456789abcdef"
	logs := captureLog(t)

	// 目录失败 → 本包打一条降级日志（最容易夹带凭据的路径）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()
	c := New(provider.ModelScope)
	c.Base = srv.URL
	if _, err := c.FetchModels(&auth.Auth{AccessToken: secret}); err != nil {
		t.Fatalf("FetchModels 应降级而不是报错：%v", err)
	}
	// 对话错误同样不得把凭据写进日志/错误体。
	_, _, respBody, err := c.ChatStream(&auth.Auth{AccessToken: secret}, []byte(`{"model":"Qwen/x"}`))
	if err != nil {
		t.Fatalf("ChatStream 上游 500 应原样透传状态码：%v", err)
	}
	if strings.Contains(string(respBody), secret) {
		t.Errorf("错误体泄露 token：%s", respBody)
	}

	out := logs.String()
	if strings.Contains(out, secret) {
		t.Fatalf("日志泄露了 token 明文：\n%s", out)
	}
	if strings.Contains(out, "Bearer") {
		t.Fatalf("日志出现了鉴权头形态：\n%s", out)
	}
	if !strings.Contains(out, "/v1/models 拉取失败") {
		t.Fatalf("目录降级日志没打出来（说明失败被静默吞掉），实际输出：\n%s", out)
	}
}
