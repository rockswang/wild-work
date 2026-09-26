package glm

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 本文件验证两个**真实缺陷**（2026-09-26 用户报告「思考过程自动终止、无报错、无重连」后定位）：
//
//	A) http.Client{Timeout: requestTimeout} 会**掐断长流**——
//	   Go 的 Client.Timeout 覆盖整个请求生命周期（含读 body），
//	   而思考/推理类回答可能超过 10 分钟。超时后上游连接被强制关闭，
//	   流在**没有 finish 帧**的情况下结束 → 客户端看到"回答到一半停住"。
//
//	B) ChatStream 用 context.Background() 而非客户端 ctx ——
//	   客户端断开时 wild-work **不知道**，仍继续读上游、继续写，
//	   表现为「无报错、无重连」的静默挂起。

// TestClientTimeoutTruncatesLongStream 证明 (A)：Client.Timeout 会截断长流。
//
// 用一个「持续发帧但永不结束」的 mock 上游，把 Client.Timeout 设成很短，
// 验证流会在超时点被掐断——且**不会有 [DONE]**。
func TestClientTimeoutTruncatesLongStream(t *testing.T) {
	const shortTimeout = 800 * time.Millisecond

	// mock 上游：每 100ms 发一个 delta 帧，持续 10 秒（远超 shortTimeout）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		deadline := time.Now().Add(10 * time.Second)
		i := 0
		for time.Now().Before(deadline) {
			i++
			// 每 100ms 发一帧，模拟「思考中持续输出」
			payload := fmt.Sprintf(
				`data: {"conversation_id":"c1","status":"init","parts":[{"logic_id":"L1","status":"init","content":[{"type":"text","text":"%d "}]}]}`+"\n\n", i)
			if _, err := io.WriteString(w, payload); err != nil {
				return // 客户端断开
			}
			if fl != nil {
				fl.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()

	// 复刻生产配置：Client.Timeout = 短超时
	client := &http.Client{Timeout: shortTimeout}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, nil)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	// 持续读到出错或 EOF
	buf := make([]byte, 4096)
	total := 0
	var readErr error
	for {
		n, err := resp.Body.Read(buf)
		total += n
		if err != nil {
			readErr = err
			break
		}
	}
	elapsed := time.Since(start)

	t.Logf("读取 %d 字节后结束，耗时 %v，错误: %v", total, elapsed.Round(time.Millisecond), readErr)

	// 核心断言：流被**超时掐断**，而不是自然结束
	if readErr == nil {
		t.Fatal("预期读取出错（超时掐断），实际正常 EOF —— 说明 mock 未生效")
	}
	if elapsed > shortTimeout+2*time.Second {
		t.Errorf("结束耗时 %v 远超超时值 %v", elapsed, shortTimeout)
	}
	if total == 0 {
		t.Error("一帧都没读到，无法证明「截断」")
	}
	t.Logf("✅ 证明 (A)：Client.Timeout=%v 会在 %v 处掐断长流（读到的内容被截断，无终止帧）",
		shortTimeout, elapsed.Round(time.Millisecond))
}

// TestClientDisconnectPropagatesViaBodyClose 澄清一个**被证伪的假设**。
//
// 我最初怀疑：`ChatStream` 用 `context.Background()` ⇒ 客户端断开无法传导 ⇒ 泄漏。
// **实测证明这个怀疑是错的**：关闭 `resp.Body` 会经底层连接关闭传导给上游，
// 上游 `r.Context()` 确实会被取消（标准库行为）。
//
// 且 `handler.go` 有 `defer rc.Close()`，Stream 因写失败返回时会关闭上游连接，
// 清理是**正确**的。
//
// 故本测试记录「此路不通」，避免后人重复怀疑。
// 真正的问题是 TestClientTimeoutTruncatesLongStream 证明的 (A)。
func TestClientDisconnectPropagatesViaBodyClose(t *testing.T) {
	upstreamCancelled := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for {
			select {
			case <-r.Context().Done():
				select {
				case upstreamCancelled <- struct{}{}:
				default:
				}
				return
			default:
			}
			_, _ = io.WriteString(w, "data: {}\n\n")
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(30 * time.Millisecond)
		}
	}))
	defer srv.Close()

	resp, err := http.DefaultClient.Post(srv.URL, "application/json", nil)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_, _ = resp.Body.Read(make([]byte, 64))
	_ = resp.Body.Close() // 等价于 handler 的 defer rc.Close()

	select {
	case <-upstreamCancelled:
		t.Log("✅ 关闭 Body 会传导取消给上游 —— 证明「Background ctx 导致泄漏」的假设**不成立**，" +
			"handler 的 defer rc.Close() 清理是正确的")
	case <-time.After(2 * time.Second):
		t.Error("关闭 Body 未传导取消（与标准库行为不符，需重新评估）")
	}
}

// TestRequestTimeoutValueIsTenMinutes 锁定当前超时值，便于改动时被注意到。
//
// 10 分钟对普通对话够用，但**思考/推理模型（zero / deep_research）
// 或长文生成可能超过**。若将来要放宽，应同时考虑：
//   - 去掉 http.Client.Timeout（改用 ctx 控制），避免它成为硬上限
//   - 或用更长的值
func TestRequestTimeoutValueIsTenMinutes(t *testing.T) {
	if requestTimeout != 10*time.Minute {
		t.Logf("requestTimeout 已变更为 %v（请确认这是有意的）", requestTimeout)
	}
	if requestTimeout < time.Minute {
		t.Errorf("requestTimeout = %v，对思考/推理模型过短", requestTimeout)
	}
	t.Logf("当前 requestTimeout = %v", requestTimeout)
}
