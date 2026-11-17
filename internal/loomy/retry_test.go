package loomy

// 票 02 回归：loomy 上游**瞬时**失败（502/503 + 快速传输层抖动）在渠道客户端内部被吸收。
//
// 只测对外可观察行为（洞：provider.Upstream.ChatStream 打到 httptest 假上游）：
// 尝试次数、返回的状态码 / 响应体 / 流是否为 nil，以及最终是否被吸收。
// 不测内部实现细节（循环变量、退避函数步骤、日志文本逐字格式）。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// fastChatRetry 把重试预算调到毫秒级（测试不得真睡满 7 秒），用例结束自动还原。
// 失败即用户可见错误的反向护栏。
func fastChatRetry(t *testing.T) {
	t.Helper()
	oldMax, oldBase, oldDeadline := chatMaxAttempts, chatRetryBaseDelay, chatRetryDeadline
	chatMaxAttempts, chatRetryBaseDelay, chatRetryDeadline = 4, time.Millisecond, 2*time.Second
	t.Cleanup(func() {
		chatMaxAttempts, chatRetryBaseDelay, chatRetryDeadline = oldMax, oldBase, oldDeadline
	})
}

func newChatStub(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New()
	c.Base = srv.URL
	return c
}

func chatBody() []byte {
	return []byte(`{"model":"` + DefaultModel + `","messages":[{"role":"user","content":"hi"}]}`)
}

func chatAuth() *auth.Auth {
	return &auth.Auth{Kind: "loomy", UID: "uid-1", AccessToken: "tok-abc"}
}

// sseOK 一次性写出一段可读的合法 SSE 头（模拟上游正常流）。
func sseOK(w http.ResponseWriter) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("data: {\"choices\":[]}\n\n"))
}

// TestChatRetryHealsTransient 判据 1/2：502、503 各一次后成功 ⇒ 仅两次尝试、
// 返回非 nil 流 + 200，**下游从不看到 502/503**。
func TestChatRetryHealsTransient(t *testing.T) {
	fastChatRetry(t)
	for _, code := range []int{http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("%d_then_200", code), func(t *testing.T) {
			var attempts int32
			c := newChatStub(t, func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&attempts, 1) == 1 {
					w.WriteHeader(code)
					_, _ = w.Write([]byte(`{"message":"transient"}`))
					return
				}
				sseOK(w)
			})
			rc, status, body, err := c.ChatStream(chatAuth(), chatBody())
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if status != http.StatusOK || body != nil {
				t.Fatalf("status=%d body=%q, want 200 + nil body（下游不得看到 %d）", status, body, code)
			}
			if rc == nil {
				t.Fatal("自愈后必须返回非 nil 流")
			}
			raw, _ := io.ReadAll(rc)
			_ = rc.Close()
			if !bytes.Contains(raw, []byte("choices")) {
				t.Fatalf("流内容丢失: %q", raw)
			}
			if got := atomic.LoadInt32(&attempts); got != 2 {
				t.Fatalf("尝试次数 = %d, want 2", got)
			}
		})
	}
}

// TestChatRetryExhaustedReturnsLast 判据 3：502 × 上限 ⇒ 恰好等于上限次尝试，
// 返回**最后一次**的状态与响应体（不包装、不改成别的状态码）。
func TestChatRetryExhaustedReturnsLast(t *testing.T) {
	fastChatRetry(t)
	var attempts int32
	c := newChatStub(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, `{"attempt":%d}`, n)
	})
	rc, status, body, err := c.ChatStream(chatAuth(), chatBody())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if rc != nil {
		t.Fatal("非 2xx 必须返回 nil 流")
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status=%d, want 502", status)
	}
	if want := fmt.Sprintf(`{"attempt":%d}`, chatMaxAttempts); string(body) != want {
		t.Fatalf("body=%q, want 最后一次响应体 %q", body, want)
	}
	if got := atomic.LoadInt32(&attempts); got != int32(chatMaxAttempts) {
		t.Fatalf("尝试次数 = %d, want %d", got, chatMaxAttempts)
	}
}

// TestChatNoRetryOnNonRetryableStatus 判据 4/7/8：429（首次）、504、500 一律只发一次。
func TestChatNoRetryOnNonRetryableStatus(t *testing.T) {
	fastChatRetry(t)
	for _, code := range []int{http.StatusTooManyRequests, http.StatusGatewayTimeout, http.StatusInternalServerError} {
		t.Run(fmt.Sprintf("status_%d", code), func(t *testing.T) {
			var attempts int32
			c := newChatStub(t, func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&attempts, 1)
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{"message":"nope"}`))
			})
			_, status, body, err := c.ChatStream(chatAuth(), chatBody())
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if status != code || string(body) != `{"message":"nope"}` {
				t.Fatalf("status=%d body=%q, want %d + 上游原文", status, body, code)
			}
			if got := atomic.LoadInt32(&attempts); got != 1 {
				t.Fatalf("尝试次数 = %d, want 1（%d 不可重试）", got, code)
			}
		})
	}
}

// TestChatRetryStopsOnMidSequenceNonRetryable 判据 5/6：逐次独立判定 ——
// 序列中间改口说 429 / 400 时立即返回该响应，不再重试。
func TestChatRetryStopsOnMidSequenceNonRetryable(t *testing.T) {
	fastChatRetry(t)
	for _, mid := range []int{http.StatusTooManyRequests, http.StatusBadRequest} {
		t.Run(fmt.Sprintf("502_then_%d", mid), func(t *testing.T) {
			var attempts int32
			c := newChatStub(t, func(w http.ResponseWriter, r *http.Request) {
				if atomic.AddInt32(&attempts, 1) == 1 {
					w.WriteHeader(http.StatusBadGateway)
					_, _ = w.Write([]byte(`{"message":"transient"}`))
					return
				}
				w.WriteHeader(mid)
				_, _ = fmt.Fprintf(w, `{"message":"mid-%d"}`, mid)
			})
			_, status, body, err := c.ChatStream(chatAuth(), chatBody())
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if status != mid || string(body) != fmt.Sprintf(`{"message":"mid-%d"}`, mid) {
				t.Fatalf("status=%d body=%q, want %d + 上游原文", status, body, mid)
			}
			if got := atomic.LoadInt32(&attempts); got != 2 {
				t.Fatalf("尝试次数 = %d, want 2（中间改口即停）", got)
			}
		})
	}
}

// TestChatRetryTransportReset 判据 9：假上游中途关连接（快速传输层抖动）⇒ 被重试，
// 第二次成功时返回 200。
func TestChatRetryTransportReset(t *testing.T) {
	fastChatRetry(t)
	var attempts int32
	c := newChatStub(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&attempts, 1) == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("假上游不支持 Hijack")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack 失败: %v", err)
				return
			}
			_ = conn.Close() // 不写任何字节直接断开 → 客户端拿到 EOF / 连接重置
			return
		}
		sseOK(w)
	})
	rc, status, _, err := c.ChatStream(chatAuth(), chatBody())
	if err != nil {
		t.Fatalf("传输层抖动应被吸收，仍返回 err: %v", err)
	}
	if status != http.StatusOK || rc == nil {
		t.Fatalf("status=%d rc=%v, want 200 + 非 nil 流", status, rc)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()
	if got := atomic.LoadInt32(&attempts); got != 2 {
		t.Fatalf("尝试次数 = %d, want 2（第一次断开被重试）", got)
	}
}

// TestChatRetryDeadlinePrecheck 判据 10：死线前置 —— 退避前越线即放弃，
// 尝试次数**少于**上限；且首次尝试自身越线的慢失败天然不重试。
func TestChatRetryDeadlinePrecheck(t *testing.T) {
	fastChatRetry(t)
	t.Run("退避越线即放弃", func(t *testing.T) {
		chatRetryBaseDelay, chatRetryDeadline = 200*time.Millisecond, time.Millisecond
		var attempts int32
		c := newChatStub(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			w.WriteHeader(http.StatusBadGateway)
		})
		_, status, _, _ := c.ChatStream(chatAuth(), chatBody())
		if status != http.StatusBadGateway {
			t.Fatalf("status=%d, want 502", status)
		}
		if got := atomic.LoadInt32(&attempts); got >= int32(chatMaxAttempts) {
			t.Fatalf("尝试次数 = %d, want < %d（死线前置应阻止新尝试）", got, chatMaxAttempts)
		}
	})
	t.Run("首次尝试越线不重试", func(t *testing.T) {
		chatRetryBaseDelay, chatRetryDeadline = time.Millisecond, 20*time.Millisecond
		var attempts int32
		c := newChatStub(t, func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&attempts, 1)
			time.Sleep(60 * time.Millisecond) // 一次尝试自身就超过总死线
			w.WriteHeader(http.StatusBadGateway)
		})
		_, status, _, _ := c.ChatStream(chatAuth(), chatBody())
		if status != http.StatusBadGateway {
			t.Fatalf("status=%d, want 502", status)
		}
		if got := atomic.LoadInt32(&attempts); got != 1 {
			t.Fatalf("尝试次数 = %d, want 1（慢失败不重试）", got)
		}
	})
}

// TestChatRetryRequestBodyIntactEachAttempt 判据 11：每次尝试都读到**完整且相同**的
// 请求体字节（不得复用被消费过的 reader）。
func TestChatRetryRequestBodyIntactEachAttempt(t *testing.T) {
	fastChatRetry(t)
	want := chatBody()
	var mu sync.Mutex
	var seen [][]byte
	c := newChatStub(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("读请求体失败: %v", err)
		}
		mu.Lock()
		seen = append(seen, raw)
		n := len(seen)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		sseOK(w)
	})
	rc, status, _, err := c.ChatStream(chatAuth(), want)
	if err != nil || status != http.StatusOK || rc == nil {
		t.Fatalf("status=%d rc=%v err=%v, want 200 + 非 nil 流", status, rc, err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("尝试次数 = %d, want 2", len(seen))
	}
	for i, raw := range seen {
		if !bytes.Equal(raw, want) {
			t.Fatalf("第 %d 次尝试的请求体 = %q, want 完整原文 %q", i+1, raw, want)
		}
	}
}

// TestChatRetryReusesTraceparentAndAuth 判据 12：同一次逻辑请求的多次尝试携带
// **同一个** traceparent，且每次鉴权头（Authorization / token）都存在且正确。
func TestChatRetryReusesTraceparentAndAuth(t *testing.T) {
	fastChatRetry(t)
	const tok = "tok-abc"
	var mu sync.Mutex
	var tps []string
	c := newChatStub(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tps = append(tps, r.Header.Get("traceparent"))
		n := len(tps)
		mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer "+tok {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("token"); got != tok {
			t.Errorf("token = %q", got)
		}
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		sseOK(w)
	})
	rc, status, _, err := c.ChatStream(chatAuth(), chatBody())
	if err != nil || status != http.StatusOK || rc == nil {
		t.Fatalf("status=%d rc=%v err=%v, want 200 + 非 nil 流", status, rc, err)
	}
	_, _ = io.ReadAll(rc)
	_ = rc.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(tps) != 3 {
		t.Fatalf("尝试次数 = %d, want 3", len(tps))
	}
	for i, tp := range tps {
		if tp == "" {
			t.Fatalf("第 %d 次尝试缺 traceparent", i+1)
		}
		if tp != tps[0] {
			t.Fatalf("第 %d 次尝试的 traceparent = %q, want 与首次相同 %q", i+1, tp, tps[0])
		}
	}
}

// TestChatRetryableJudgement 判据表：retry.go 的单点函数逐类锁定 ——
// 可重试（502/503 + 快速瞬态）与不可重试（500/504/429/其它 4xx/超时类）各覆盖一类。
func TestChatRetryableJudgement(t *testing.T) {
	retryable := []struct {
		name   string
		status int
		err    error
	}{
		{"502", http.StatusBadGateway, nil},
		{"503", http.StatusServiceUnavailable, nil},
		{"EOF", 0, io.EOF},
		{"unexpected EOF", 0, io.ErrUnexpectedEOF},
		{"连接被重置(ECONNRESET)", 0, syscall.ECONNRESET},
		{"连接被拒(ECONNREFUSED)", 0, syscall.ECONNREFUSED},
		{"broken pipe(EPIPE)", 0, syscall.EPIPE},
		{"传输层原文-连接重置", 0, errors.New(`Post "https://x/api/v1/chat/completions": read tcp: connection reset by peer`)},
		{"传输层原文-broken pipe", 0, errors.New("write tcp: broken pipe")},
	}
	for _, c := range retryable {
		if !chatRetryable(c.status, c.err) {
			t.Errorf("%s 应可重试（status=%d err=%v）", c.name, c.status, c.err)
		}
	}
	notRetryable := []struct {
		name   string
		status int
		err    error
	}{
		{"500", http.StatusInternalServerError, nil},
		{"504", http.StatusGatewayTimeout, nil},
		{"429", http.StatusTooManyRequests, nil},
		{"400", http.StatusBadRequest, nil},
		{"401", http.StatusUnauthorized, nil},
		{"200", http.StatusOK, nil},
		{"等响应头超时", 0, errors.New("net/http: timeout awaiting response headers")},
		{"dial i/o timeout", 0, errors.New("dial tcp 1.2.3.4:443: i/o timeout")},
		{"context deadline", 0, context.DeadlineExceeded},
		{"net.Error timeout", 0, &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}},
		{"证书错误", 0, errors.New("x509: certificate signed by unknown authority")},
		{"协议错误", 0, errors.New("malformed HTTP response")},
	}
	for _, c := range notRetryable {
		if chatRetryable(c.status, c.err) {
			t.Errorf("%s 不得重试（status=%d err=%v）", c.name, c.status, c.err)
		}
	}
}

// TestChatNoRetryOnHeaderTimeout 判据：**等响应头超时**（慢失败）在真实 HTTP 边界上
// 只发一次 —— 上游可能已开始生成，重发有双扣分风险（工单判据「每一类各有一个用例锁定」）。
func TestChatNoRetryOnHeaderTimeout(t *testing.T) {
	fastChatRetry(t)
	var attempts int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		<-release // 永不回响应头，逼客户端走 ResponseHeaderTimeout
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	c := New()
	c.Base = srv.URL
	c.StreamHTTP = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 50 * time.Millisecond}}

	rc, status, _, err := c.ChatStream(chatAuth(), chatBody())
	if err == nil {
		t.Fatal("等响应头超时必须按既有契约作为 err 返回")
	}
	if rc != nil || status != 0 {
		t.Fatalf("rc=%v status=%d, want nil/0", rc, status)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Fatalf("尝试次数 = %d, want 1（慢失败不重试）", got)
	}
}
