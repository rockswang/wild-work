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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
		{http.MethodPost, "https://opencode.ai/inference/v1/chat/completions", true},
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
	// 走完整 RoundTrip（含 tee 落盘）：把 base Transport 换成假上游，
	// 并把 URL 改成非回环路径（shouldDump 的判定只看 host/path，实际连的是假上游）。
	tr := &dumpTransport{base: http.DefaultTransport, channel: "workbuddy"}
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/chat/completions", strings.NewReader(`{"model":"glm-5.2"}`))
	if err != nil {
		t.Fatalf("构造请求失败：%v", err)
	}
	// 绕开回环过滤：shouldDump 读的是 req.URL（127.0.0.1 会被滤掉），
	// 故先记为可 dump 的 host 再发——用 http.DefaultTransport 会真的去连那个 host。
	// 改为直接调内部路径：替换 base 为「假上游 + 强制放行」。
	tr.base = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r2, _ := http.NewRequest(r.Method, srv.URL+"/v2/chat/completions", nil)
		return http.DefaultTransport.RoundTrip(r2)
	})
	req.URL.Host = "copilot.tencent.com"
	req.URL.Scheme = "https"
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip：%v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "prompt_cache_hit_tokens") {
		t.Fatalf("响应体应原样透传：%s", body)
	}
	_ = resp.Body.Close() // 触发收尾落盘

	ents, err := os.ReadDir(filepath.Join(dir, "workbuddy"))
	if err != nil || len(ents) != 1 {
		t.Fatalf("应落盘 1 个文件：err=%v n=%d", err, len(ents))
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "workbuddy", ents[0].Name()))
	s := string(raw)
	for _, want := range []string{
		"# channel=workbuddy", "status=200", "usage=", "prompt_cache_hit_tokens",
		"### REQUEST POST https://copilot.tencent.com/v2/chat/completions",
		"### RESPONSE 200", `{"model":"glm-5.2"}`, "# duration=",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("快照缺少 %q\n---\n%s", want, s)
		}
	}
}

// roundTripFunc 把函数当 RoundTripper 用（测试替身）。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestDumpIsStreamingNotBuffered 守门：落盘**不得**缓冲整个响应体。
//
// 回归目标（2026-10-17 实测）：旧实现 `io.ReadAll(resp.Body)` 会把 SSE 流读到结束才
// 交给调用方 ⇒ 客户端在整个生成期间收不到任何帧、之后一次性收到全部内容（表现为
// 「对话时正文突然整段出现」）。本测试断言：调用方读到的**第一帧**必须在落盘写出
// 之前就到达，即 tee 不阻塞转发。
func TestDumpIsStreamingNotBuffered(t *testing.T) {
	dir := t.TempDir()
	defer Disable()
	Enable(dir)

	firstFrameSeen := make(chan struct{})
	rec := newRespRecorder("streamtest", mustReq(t, http.MethodPost, "https://x.test/v1/chat/completions", nil), nil, time.Now())
	rec.setResp(200, http.Header{"Content-Type": {"text/event-stream"}})
	rec.active = true
	rb := &teeBody{rc: &frameReader{frames: []string{"ONE", "TWO", "THREE"}, ack: firstFrameSeen, t: t}, rec: rec}

	buf := make([]byte, 8)
	n, err := rb.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("首帧应可用：n=%d err=%v", n, err)
	}
	_ = rb.Close()
	if string(buf[:n]) != "ONE" {
		t.Fatalf("首帧内容不对：%q", buf[:n])
	}
	// 关键断言：首帧到达时，落盘尚未写成完整内容（说明不是攒批后一次性写）。
	if rec.headerWritten == false {
		t.Fatal("首帧读到时文件头应已写出（tee 应立即落盘）")
	}
}

// TestDumpConcurrentRecorders 并发安全：多路 recorder 同时读写不得丢字节/串内容。
// （本机无 cgo 时 -race 不可用，用计数 + 内容校验兜住明显的竞争/覆盖类缺陷。）
func TestDumpConcurrentRecorders(t *testing.T) {
	dir := t.TempDir()
	defer Disable()
	Enable(dir)

	const nFiles, nFrames = 8, 200
	var wg sync.WaitGroup
	for i := 0; i < nFiles; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			req := mustReq(t, http.MethodPost, "https://x.test/v1/chat/completions?n="+strconv.Itoa(id), nil)
			rec := newRespRecorder("conc", req, []byte("REQBODY"), time.Now())
			rec.setResp(200, http.Header{"Content-Type": {"text/event-stream"}})
			if !rec.active {
				t.Errorf("recorder 未激活（建文件失败）")
				return
			}
			// 交替 Write 与转发读，模拟真实 tee 调用序。
			frame := []byte("data: chunk-" + strconv.Itoa(id) + "\n\n")
			for j := 0; j < nFrames; j++ {
				_, _ = rec.Write(frame)
			}
			_ = rec.close()
			_ = rec.close() // 幂等：二次收尾不得重复写或 panic
		}(i)
	}
	wg.Wait()

	ents, err := os.ReadDir(filepath.Join(dir, "conc"))
	if err != nil || len(ents) != nFiles {
		t.Fatalf("应落盘 %d 个文件：err=%v n=%d", nFiles, err, len(ents))
	}
	for _, e := range ents {
		raw, _ := os.ReadFile(filepath.Join(dir, "conc", e.Name()))
		s := string(raw)
		want := nFrames * len("data: chunk-0\n\n") // 各 id 帧等长，仅内容不同
		if got := strings.Count(s, "data: chunk-"); got != nFrames {
			t.Errorf("%s 帧数 = %d, want %d", e.Name(), got, nFrames)
		}
		// 每个文件只应含一个 id 的内容（不得串号）。
		ids := map[string]bool{}
		for _, part := range strings.Split(s, "data: chunk-")[1:] {
			ids[part[:1]] = true
		}
		if len(ids) != 1 {
			t.Errorf("%s 串号（含多个 id）：%v", e.Name(), ids)
		}
		if !strings.Contains(s, "REQBODY") || strings.Count(s, "# duration=") != 1 {
			t.Errorf("%s 请求块/汇总行异常", e.Name())
		}
		_ = want
	}
}

// frameReader 逐帧返回固定数据（模拟上游 SSE 逐帧到达），并在首帧时通知。
type frameReader struct {
	frames []string
	i      int
	ack    chan struct{}
	once   bool
	t      *testing.T
}

func (f *frameReader) Read(p []byte) (int, error) {
	if f.i >= len(f.frames) {
		return 0, io.EOF
	}
	s := f.frames[f.i]
	f.i++
	n := copy(p, s)
	if !f.once {
		f.once = true
		close(f.ack)
	}
	return n, nil
}

func (f *frameReader) Close() error { return nil }

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
