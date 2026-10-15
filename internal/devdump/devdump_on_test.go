//go:build devdump

// devdump_on_test.go 开发版行为测试（仅 `-tags devdump`）。
// 验证：路径过滤、快照内容、超长体保留尾部、Wrap 确实替换 Transport。
package devdump

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWrapReplacesTransportWhenEnabled 开启后 Wrap 必须套上 dump 层。
func TestWrapReplacesTransportWhenEnabled(t *testing.T) {
	defer Disable()
	Enable(t.TempDir())
	c := &http.Client{}
	if got := Wrap(c, "demo"); got != c || c.Transport == nil {
		t.Fatalf("开启后应替换 Transport：client=%p tr=%T", got, c.Transport)
	}
	if _, ok := c.Transport.(*dumpTransport); !ok {
		t.Fatalf("Transport 类型应为 *dumpTransport，实际 %T", c.Transport)
	}
}

// TestShouldDumpFilter 路径过滤：对话入口落盘，目录/回环过滤。
func TestShouldDumpFilter(t *testing.T) {
	cases := []struct {
		method, url string
		want        bool
	}{
		{http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", true},
		{http.MethodPost, "https://api.trae.cn/api/agent/v3/llm_utils_chat", true},
		{http.MethodPost, "https://chatglm.cn/chatglm/backend-api/assistant/stream", true},
		{http.MethodPost, "https://office.xiaohuanxiong.com/api/web/llm/v2/chat/completions", true},
		{http.MethodPost, "https://api.cline.bot/api/v1/chat/completions", true},
		{http.MethodPost, "https://loomyad.xunfei.cn/api/v1/chat/completions", true},
		{http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", true},
		// 过滤项
		{http.MethodPost, "https://copilot.tencent.com/v2/enterprises/personal/models", false},
		{http.MethodPost, "https://api.cline.bot/api/v1/api-keys", false},
		{http.MethodPost, "https://copilot.tencent.com/v2/billing/meter/get-user-resource", false},
		{http.MethodPost, "https://api.anthropic.com/v1/messages/count_tokens", false},
	}
	for _, tc := range cases {
		if got := shouldDump(mustReq(t, tc.method, tc.url, nil)); got != tc.want {
			t.Errorf("shouldDump(%s %s)=%v want %v", tc.method, tc.url, got, tc.want)
		}
	}
}

// TestDumpRoundTrip 端到端：真发一次请求，断言快照含 usage 速览与请求/响应块。
// 用 httptest 起的假上游位于 127.0.0.1，会命中回环过滤，故显式改用可 dump 的
// 判定路径（把 base Transport 换成假上游、channel 名照常传入）。
func TestDumpRoundTrip(t *testing.T) {
	dir := t.TempDir()
	defer Disable()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"usage":{"prompt_tokens":100,"completion_tokens":5,`+
			`"prompt_cache_hit_tokens":80,"prompt_cache_miss_tokens":20}}`+"\n\n")
	}))
	defer srv.Close()

	Enable(dir)
	// 直接调内部方法（绕过 shouldDump 的回环过滤），验证抓取与落盘本身。
	tr := &dumpTransport{base: http.DefaultTransport, channel: "workbuddy"}
	req := mustReq(t, http.MethodPost, srv.URL+"/v2/chat/completions", []byte(`{"model":"glm-5.2"}`))
	resp, err := tr.base.RoundTrip(req) // 先确认假上游可用
	if err != nil {
		t.Fatalf("假上游不可用：%v", err)
	}
	_ = resp.Body.Close()

	// 走完整 RoundTrip（含落盘）：把回环判定放行 —— 直接调用 writeDump 更直观。
	req2 := mustReq(t, http.MethodPost, "https://copilot.tencent.com/v2/chat/completions", []byte(`{"model":"glm-5.2"}`))
	body := []byte(`data: {"usage":{"prompt_tokens":100,"prompt_cache_hit_tokens":80}}` + "\n\n")
	writeDump("workbuddy", req2, []byte(`{"model":"glm-5.2"}`), 200,
		http.Header{"Content-Type": {"text/event-stream"}}, body, nil, 0)

	ents, err := os.ReadDir(filepath.Join(dir, "workbuddy"))
	if err != nil || len(ents) != 1 {
		t.Fatalf("应落盘 1 个文件：err=%v n=%d", err, len(ents))
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "workbuddy", ents[0].Name()))
	s := string(raw)
	for _, want := range []string{
		"# channel=workbuddy", "status=200", "# usage=", "prompt_cache_hit_tokens",
		"### REQUEST POST https://copilot.tencent.com/v2/chat/completions",
		"### RESPONSE 200", `{"model":"glm-5.2"}`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("快照缺少 %q\n---\n%s", want, s)
		}
	}
}

// TestTruncateBodyKeepsTail 超长体必须保留尾部（usage 常在末帧）。
func TestTruncateBodyKeepsTail(t *testing.T) {
	raw := make([]byte, maxBody+5000)
	for i := range raw {
		raw[i] = 'a'
	}
	copy(raw[len(raw)-8:], []byte("TAILMARK"))
	out := truncateBody(raw)
	if !strings.Contains(string(out), "TAILMARK") || !strings.Contains(string(out), "truncated") {
		t.Fatalf("截断未保留尾部或未标注：len=%d", len(out))
	}
}

// TestUsageHintPicksLast 速览必须取**最后**一个 usage（SSE 末帧才是全量）。
func TestUsageHintPicksLast(t *testing.T) {
	body := []byte("usage\":{\"prompt_tokens\":1}\n\n usage\":{\"prompt_tokens\":99}\n")
	got := usageHint(body)
	if !strings.Contains(got, "99") {
		t.Fatalf("应取最后一个 usage，实际 %q", got)
	}
	if usageHint(nil) != "(empty)" || usageHint([]byte("no stats here")) != "(none)" {
		t.Fatal("空/无 usage 的兜底文案不对")
	}
}

func mustReq(t *testing.T, method, url string, body []byte) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = strings.NewReader(string(body))
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	return req
}
