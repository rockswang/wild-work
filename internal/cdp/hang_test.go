package cdp

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"net"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
)

// runtimeNumGoroutine 是 runtime.NumGoroutine 的薄封装（便于本文件内复用）。
func runtimeNumGoroutine() int { return runtime.NumGoroutine() }

// 本文件专门审计「挂起 / 死锁 / goroutine 泄漏」这类靠单测不易发现的问题。
// 手法：构造恶意/异常对端（不回包、不读包、中途断开），断言操作在有限时间内返回。

// handshakeOnly 起一个只完成握手、之后完全不读也不写的服务端。
// 用于模拟「对端停止读取」——客户端的 Write 会因 TCP 发送缓冲写满而阻塞。
func handshakeOnly(t *testing.T) (addr string, stop func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				h := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + wsMagicGUID))
				accept := base64.StdEncoding.EncodeToString(h[:])
				c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n" +
					"Connection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"))
				// 关键：之后既不读也不写，只挂着（模拟对端卡死）
				time.Sleep(30 * time.Second)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

// TestWriteDoesNotHangClose 对端停止读取时，Write 必须因写超时返回，
// 且 Close 不能永久阻塞。
//
// 这是真实风险：writeFrame 持有 w.mu 做阻塞网络写；若没有写超时，
// 一旦对端不读（TCP 缓冲写满），writeFrame 永久阻塞并持锁 →
// Close() 也调 writeFrame → 死锁，整个渠道卡住。
func TestWriteDoesNotHangClose(t *testing.T) {
	addr, stop := handshakeOnly(t)
	defer stop()

	ws, err := DialWebSocket("ws://"+addr+"/x", 5*time.Second)
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}

	// 灌足够大的载荷把 TCP 发送缓冲写满（对端不读）
	big := make([]byte, 4<<20) // 4MB
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 忽略错误：我们只关心它是否「能返回」
		_, _ = ws.Call("Fill", map[string]any{"data": string(big)}, 3*time.Second)
	}()

	select {
	case <-done:
		t.Log("✅ 写操作在对端不读时正常返回（有超时保护）")
	case <-time.After(12 * time.Second):
		t.Fatal("❌ 写操作挂死超过 12s：对端不读时无超时保护")
	}

	// Close 也必须在有限时间内返回
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_ = ws.Close()
	}()
	select {
	case <-closed:
		t.Log("✅ Close 正常返回")
	case <-time.After(10 * time.Second):
		t.Fatal("❌ Close 挂死：writeFrame 持锁阻塞导致死锁")
	}
}

// TestConcurrentCallAndClose 并发 Call 与 Close 不得 panic 或死锁。
func TestConcurrentCallAndClose(t *testing.T) {
	addr, stop := handshakeOnly(t)
	defer stop()

	ws, err := DialWebSocket("ws://"+addr+"/x", 5*time.Second)
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = ws.Call("Ping", nil, 800*time.Millisecond)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		time.Sleep(100 * time.Millisecond)
		_ = ws.Close()
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		t.Log("✅ 并发 Call + Close 正常结束，无 panic/死锁")
	case <-time.After(15 * time.Second):
		t.Fatal("❌ 并发 Call + Close 挂死")
	}
}

// TestServerAbruptClose 对端在握手中途断开时，Dial 必须报错而非挂起。
func TestServerAbruptClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		conn.Close() // 立刻断开
	}()

	done := make(chan error, 1)
	go func() {
		_, err := DialWebSocket("ws://"+ln.Addr().String()+"/x", 3*time.Second)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("对端断开时不应握手成功")
		} else {
			t.Logf("✅ 对端断开时正确报错: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("❌ 对端断开时 Dial 挂死")
	}
}

// TestCallAfterClose Call 在 Close 之后必须立即报错，不得挂起。
func TestCallAfterClose(t *testing.T) {
	addr, stop := handshakeOnly(t)
	defer stop()

	ws, err := DialWebSocket("ws://"+addr+"/x", 5*time.Second)
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	_ = ws.Close()

	done := make(chan error, 1)
	go func() {
		_, err := ws.Call("AfterClose", nil, 3*time.Second)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Close 之后 Call 应报错")
		} else {
			t.Logf("✅ Close 后 Call 立即报错: %v", err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("❌ Close 后 Call 挂死")
	}
}

// TestNoGoroutineLeak Close 之后读循环 goroutine 必须退出。
//
// 注意：本测试用的是**正常回包的 echo 服务端**，避免把测试服务端自己的
// 每连接 goroutine 误算成「客户端泄漏」（handshakeOnly 的 30s sleep 会污染计数）。
func TestNoGoroutineLeak(t *testing.T) {
	addr, stop := echoServer(t)
	defer stop()

	before := runtimeNumGoroutine()

	for i := 0; i < 20; i++ {
		ws, err := DialWebSocket("ws://"+addr+"/x", 5*time.Second)
		if err != nil {
			t.Fatalf("DialWebSocket #%d: %v", i, err)
		}
		_ = ws.Close()
	}

	// 给 goroutine 一点退出时间
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if runtimeNumGoroutine() <= before+5 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	after := runtimeNumGoroutine()
	if after > before+5 {
		t.Errorf("❌ goroutine 泄漏：前 %d → 后 %d（创建了 20 个连接）", before, after)
	} else {
		t.Logf("✅ 无 goroutine 泄漏：前 %d → 后 %d", before, after)
	}
}