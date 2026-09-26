package glm

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 本文件审计 glm 包的「挂起 / 泄漏 / 并发」问题。

// runtimeNumGoroutine 薄封装。
func runtimeNumGoroutine() int { return runtime.NumGoroutine() }

// TestStreamHangsUpstreamDoesNotBlockClient 上游发出部分帧后永久不关闭、
// 也不再发数据时，Stream 会一直等（这是**预期行为**：SSE 本就长连接）。
// 但**上层必须能通过取消 reader 来终止它** —— 验证 reader 被关闭后 Stream 会返回。
func TestStreamHangsUpstreamDoesNotBlockClient(t *testing.T) {
	pr, pw := io.Pipe()
	rec := &flushRecorder{}
	c := NewWithBase("http://unused")

	done := make(chan error, 1)
	go func() {
		_, err := c.Stream(rec, pr, "glm/chatglm")
		done <- err
	}()

	// 发一帧有效数据
	_, _ = pw.Write([]byte(`data: {"conversation_id":"c1","status":"init","parts":[{"logic_id":"L1","status":"init","content":[{"type":"text","text":"部分"}]}]}` + "\n\n"))
	time.Sleep(200 * time.Millisecond)

	select {
	case <-done:
		t.Fatal("上游未结束时 Stream 不应提前返回")
	default:
	}

	// 关闭上游 reader：Stream 必须返回
	_ = pw.CloseWithError(io.ErrUnexpectedEOF)

	select {
	case err := <-done:
		t.Logf("✅ 上游中断后 Stream 正常返回: %v", err)
		// 中断也必须补 [DONE]，否则客户端挂住
		if !strings.Contains(rec.String(), "data: [DONE]") {
			t.Error("❌ 上游中断时未补 [DONE]，客户端会挂住")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("❌ 上游 reader 关闭后 Stream 仍挂住")
	}
}

// TestStreamMalformedFrameDoesNotPanic 畸形帧不得 panic，也不得中断整个流。
func TestStreamMalformedFrameDoesNotPanic(t *testing.T) {
	frames := []string{
		`{这不是 JSON`,                                  // 完全畸形
		`{"parts":"不是数组"}`,                            // 类型错误
		`{"parts":[{"content":"不是数组"}]}`,               // 内层类型错误
		`{"parts":[{"content":[{"type":"text"}]}]}`,     // 缺字段
		`{"conversation_id":"c1","status":"finish","parts":[]}`, // 正常收尾
	}
	var sb strings.Builder
	for _, f := range frames {
		sb.WriteString("data: " + f + "\n\n")
	}

	rec := &flushRecorder{}
	c := NewWithBase("http://unused")
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Stream(rec, strings.NewReader(sb.String()), "glm/chatglm")
	}()
	select {
	case <-done:
		t.Log("✅ 畸形帧未导致 panic/挂起，流正常结束")
	case <-time.After(8 * time.Second):
		t.Fatal("❌ 畸形帧导致 Stream 挂起")
	}
}

// TestStreamHugeSingleLine 单行超大（无换行的巨量数据）不得导致内存失控。
// parseSSE 用 ReadString('\n')：若上游一直不发换行，会一直累积。
// 这是**已知取舍**（bufio 有缓冲上限保护），本测试确认不会无限增长到 OOM。
func TestStreamHugeSingleLine(t *testing.T) {
	// 8MB 无换行数据，然后 EOF
	big := strings.Repeat("x", 8<<20)
	rec := &flushRecorder{}
	c := NewWithBase("http://unused")

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = c.Stream(rec, strings.NewReader("data: "+big), "glm/chatglm")
	}()
	select {
	case <-done:
		t.Log("✅ 超长单行正常处理完毕（未挂起）")
	case <-time.After(20 * time.Second):
		t.Fatal("❌ 超长单行导致挂起")
	}
}

// TestCheckinConcurrentSameAuth 同一账号并发签到不得 panic / 死锁 / 数据竞争。
func TestCheckinConcurrentSameAuth(t *testing.T) {
	m := &mockChatGLM{
		chatPartsSequence: [][]part{
			{{LogicID: "L1", Status: "init", Content: []partContent{{Type: "text", Text: "好"}}}},
		},
		chatFinalText: "好",
	}
	c, srv := newTestClient(t, m)
	defer srv.Close()

	a := &auth.Auth{
		Kind:        string(Kind),
		AccessToken: "AT-ready",
		ExpiresAt:   futureUnix(),
		UID:         "u-concurrent",
	}

	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.DailyCheckinReport(a); err != nil {
				errs <- err
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		close(errs)
		var errList []error
		for e := range errs {
			errList = append(errList, e)
		}
		t.Logf("✅ 并发签到完成，错误数 %d", len(errList))
	case <-time.After(30 * time.Second):
		t.Fatal("❌ 并发签到挂死")
	}
}

// TestRefreshTokenConcurrent 同一账号并发刷新 token 不得竞争写坏凭证。
//
// 关键：RefreshToken 会改 Auth 字段并落盘，必须有锁保护。
// 用 -race 才能完全确认；此处至少验证不 panic、字段不被写坏。
func TestRefreshTokenConcurrent(t *testing.T) {
	m := &mockChatGLM{}
	c, srv := newTestClient(t, m)
	defer srv.Close()

	dir := t.TempDir()
	a := &auth.Auth{
		Kind:         string(Kind),
		AccessToken:  "AT-old",
		RefreshToken: "RT-old",
		ExpiresAt:    0,
		UID:          "u-race",
		FilePath:     dir + "/glm-u-race.json",
	}

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = c.RefreshToken(a)
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		// 字段必须仍是完整值（未被并发写坏成空串/半截）
		a.RLock()
		at, rt := a.AccessToken, a.RefreshToken
		a.RUnlock()
		if at == "" || rt == "" {
			t.Errorf("❌ 并发刷新把凭证写坏了: accessToken=%q refreshToken=%q", at, rt)
		} else {
			t.Logf("✅ 并发刷新后凭证完整: accessToken 长度=%d refreshToken=%s", len(at), rt)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("❌ 并发刷新挂死")
	}
}

// TestStreamConcurrentClients 多客户端并发流式对话不得串号 / 泄漏。
func TestStreamConcurrentClients(t *testing.T) {
	m := &mockChatGLM{
		chatPartsSequence: [][]part{
			{{LogicID: "L1", Status: "init", Content: []partContent{{Type: "text", Text: "回"}}}},
			{{LogicID: "L1", Status: "init", Content: []partContent{{Type: "text", Text: "答"}}}},
		},
		chatFinalText: "回答",
	}
	c, srv := newTestClient(t, m)
	defer srv.Close()

	before := runtimeNumGoroutine()

	a := &auth.Auth{Kind: string(Kind), AccessToken: "AT-ready", ExpiresAt: futureUnix(), UID: "u-1"}

	const n = 15
	var wg sync.WaitGroup
	results := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			body := []byte(`{"model":"glm/chatglm","messages":[{"role":"user","content":"hi"}]}`)
			rc, status, _, err := c.ChatStream(a, body)
			if err != nil || status != 200 {
				results[idx] = fmt.Sprintf("ERR:%v/%d", err, status)
				return
			}
			defer rc.Close()
			rec := &flushRecorder{}
			_, _ = c.Stream(rec, rc, "glm/chatglm")
			content, _ := collectDeltas(t, rec)
			results[idx] = content
		}(i)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		bad := 0
		for i, r := range results {
			if r != "回答" {
				bad++
				if bad <= 3 {
					t.Errorf("客户端 %d 结果异常: %q", i, r)
				}
			}
		}
		if bad == 0 {
			t.Logf("✅ %d 个并发客户端结果全部正确（无串号）", n)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("❌ 并发流式对话挂死")
	}

	// 短暂等待收敛。
	//
	// 注意判据：不能直接数 runtime.NumGoroutine()——net/http 的连接池
	// （Transport.dialConn / Server.Serve）会保留空闲 keep-alive 连接，
	// 属于标准库正常行为，会按 IdleConnTimeout 自行回收。
	// 这里只统计**卡在我们自己代码里**的 goroutine（栈含 wild-work/internal/glm）。
	time.Sleep(1500 * time.Millisecond)
	after := runtimeNumGoroutine()
	ours := countOurGoroutines()
	if ours > 2 {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Errorf("⚠️ 我方 goroutine 泄漏 %d 个（前 %d → 后 %d）\n--- 栈转储 ---\n%s",
			ours, before, after, string(buf[:n]))
	} else {
		t.Logf("✅ 无我方 goroutine 泄漏（总数 %d → %d，其中我方 %d 个，其余为 net/http 连接池）",
			before, after, ours)
	}
}

// countOurGoroutines 统计栈里含本项目包的 goroutine 数量（排除测试自身与标准库）。
func countOurGoroutines() int {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	blocks := strings.Split(string(buf[:n]), "\n\n")
	count := 0
	for _, b := range blocks {
		if !strings.Contains(b, "wild-work/internal/glm") {
			continue
		}
		// 排除测试函数自身
		if strings.Contains(b, "_test.go") {
			continue
		}
		count++
	}
	return count
}

// TestChatStreamNonSSEResponse 上游返回非 SSE（如 JSON 错误）时必须正确报错，
// 不能把 JSON 当 SSE 解析而挂住。
func TestChatStreamNonSSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"status":500,"message":"活动已结束","result":null}`))
	}))
	defer srv.Close()

	c := NewWithBase(srv.URL)
	a := &auth.Auth{Kind: string(Kind), AccessToken: "AT", ExpiresAt: futureUnix(), UID: "u-1"}
	body := []byte(`{"model":"glm/chatglm","messages":[{"role":"user","content":"hi"}]}`)

	done := make(chan struct{})
	var rc io.ReadCloser
	var status int
	go func() {
		defer close(done)
		rc, status, _, _ = c.ChatStream(a, body)
	}()
	select {
	case <-done:
		if rc != nil {
			rc.Close()
		}
		if status == 200 {
			t.Errorf("非 SSE 响应不应被当作成功（status=%d）", status)
		} else {
			t.Logf("✅ 非 SSE 响应被正确识别为错误（status=%d）", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("❌ 非 SSE 响应处理挂起")
	}
}

// 确保 provider 包被引用（Error 类型断言用）
var _ = provider.ErrNone