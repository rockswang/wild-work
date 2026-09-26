package cdp

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestWSAccept 校验握手 Accept 计算（RFC 6455 示例向量）。
func TestWSAccept(t *testing.T) {
	// RFC 6455 §1.3 的标准示例
	got := wsAccept("dGhlIHNhbXBsZSBub25jZQ==")
	want := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got != want {
		t.Errorf("wsAccept = %q, want %q", got, want)
	}
}

// echoServer 起一个最小 WebSocket 服务端（仅测试用），把收到的文本帧回显。
// 用于验证客户端的握手、掩码编码、帧解析与响应分派。
func echoServer(t *testing.T) (addr string, stop func()) {
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
			go handleEcho(conn)
		}
	}()

	return ln.Addr().String(), func() { ln.Close() }
}

// handleEcho 处理一次 WebSocket 会话：握手 + 回显。
func handleEcho(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	key := req.Header.Get("Sec-WebSocket-Key")
	h := sha1.Sum([]byte(key + wsMagicGUID))
	accept := base64.StdEncoding.EncodeToString(h[:])

	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		return
	}

	for {
		opcode, payload, err := readClientFrame(br)
		if err != nil {
			return
		}
		if opcode == opClose {
			return
		}
		if opcode != opText {
			continue
		}
		// 解析客户端命令，回一条 CDP 风格的响应
		var msg struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(payload, &msg); err != nil {
			continue
		}
		var result any
		switch msg.Method {
		case "Network.getAllCookies":
			result = map[string]any{
				"cookies": []map[string]any{
					{"name": "chatglm_refresh_token", "value": "RT-TEST-VALUE", "domain": ".chatglm.cn"},
					{"name": "other", "value": "x", "domain": ".chatglm.cn"},
				},
			}
		case "Fail":
			// 回一条错误响应
			errResp := fmt.Sprintf(`{"id":%d,"error":{"code":-32000,"message":"boom"}}`, msg.ID)
			_ = writeServerFrame(conn, opText, []byte(errResp))
			continue
		default:
			result = map[string]any{"ok": true}
		}
		body, _ := json.Marshal(map[string]any{"id": msg.ID, "result": result})
		if err := writeServerFrame(conn, opText, body); err != nil {
			return
		}
	}
}

// readClientFrame 读一个客户端帧（带掩码）。
func readClientFrame(br *bufio.Reader) (byte, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return 0, nil, err
	}
	opcode := h[0] & 0x0F
	masked := h[1]&0x80 != 0
	length := int64(h[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(br, mask[:]); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(br, payload); err != nil {
			return 0, nil, err
		}
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}

// writeServerFrame 写一个服务端帧（无掩码）。
func writeServerFrame(conn net.Conn, opcode byte, payload []byte) error {
	var header []byte
	header = append(header, 0x80|opcode)
	n := len(payload)
	switch {
	case n < 126:
		header = append(header, byte(n))
	case n < 65536:
		header = append(header, 126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		header = append(header, ext[:]...)
	default:
		header = append(header, 127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, ext[:]...)
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err := conn.Write(payload)
	return err
}

// TestWebSocketCallAndCookies 端到端验证：握手 → 调用 → 解析响应。
func TestWebSocketCallAndCookies(t *testing.T) {
	addr, stop := echoServer(t)
	defer stop()

	ws, err := DialWebSocket("ws://"+addr+"/devtools/page/TEST", 5*time.Second)
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	// 普通调用
	raw, err := ws.Call("Network.enable", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("Network.enable: %v", err)
	}
	var ok struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &ok); err != nil || !ok.OK {
		t.Errorf("enable 响应异常: %s (err=%v)", raw, err)
	}

	// 取 Cookie
	raw, err = ws.Call("Network.getAllCookies", nil, 5*time.Second)
	if err != nil {
		t.Fatalf("getAllCookies: %v", err)
	}
	var out struct {
		Cookies []Cookie `json:"cookies"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析 cookies: %v", err)
	}
	if len(out.Cookies) != 2 {
		t.Fatalf("cookies 数 = %d, want 2", len(out.Cookies))
	}
	var found string
	for _, c := range out.Cookies {
		if c.Name == "chatglm_refresh_token" {
			found = c.Value
		}
	}
	if found != "RT-TEST-VALUE" {
		t.Errorf("refresh_token = %q, want RT-TEST-VALUE", found)
	}
}

// TestWebSocketErrorResponse CDP 错误必须转成 Go error。
func TestWebSocketErrorResponse(t *testing.T) {
	addr, stop := echoServer(t)
	defer stop()

	ws, err := DialWebSocket("ws://"+addr+"/x", 5*time.Second)
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	_, err = ws.Call("Fail", nil, 5*time.Second)
	if err == nil {
		t.Fatal("错误响应应返回 error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("错误信息未透出: %v", err)
	}
}

// TestWebSocketConcurrentCalls 并发调用必须各自拿到正确响应（id 分派）。
func TestWebSocketConcurrentCalls(t *testing.T) {
	addr, stop := echoServer(t)
	defer stop()

	ws, err := DialWebSocket("ws://"+addr+"/x", 5*time.Second)
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	const n = 20
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := ws.Call("Network.enable", nil, 5*time.Second)
			errs <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-errs; err != nil {
			t.Errorf("并发调用失败: %v", err)
		}
	}
}

// TestWebSocketLargePayload 大载荷必须走 16 位长度分支且内容完整。
func TestWebSocketLargePayload(t *testing.T) {
	addr, stop := echoServer(t)
	defer stop()

	ws, err := DialWebSocket("ws://"+addr+"/x", 5*time.Second)
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	// 构造一个超过 125 字节的参数（触发 126 长度分支）
	big := strings.Repeat("x", 5000)
	raw, err := ws.Call("Echo", map[string]any{"data": big}, 5*time.Second)
	if err != nil {
		t.Fatalf("大载荷调用失败: %v", err)
	}
	var ok struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(raw, &ok); err != nil || !ok.OK {
		t.Errorf("大载荷响应异常: %v", err)
	}
}

// TestWebSocketTimeout 无响应时必须超时而非永久挂起。
func TestWebSocketTimeout(t *testing.T) {
	// 一个只握手不回包的服务器
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
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		h := sha1.Sum([]byte(req.Header.Get("Sec-WebSocket-Key") + wsMagicGUID))
		accept := base64.StdEncoding.EncodeToString(h[:])
		conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n" +
			"Connection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"))
		time.Sleep(10 * time.Second) // 故意不回响应
	}()

	ws, err := DialWebSocket("ws://"+ln.Addr().String()+"/x", 5*time.Second)
	if err != nil {
		t.Fatalf("DialWebSocket: %v", err)
	}
	defer ws.Close()

	start := time.Now()
	_, err = ws.Call("NeverResponds", nil, 800*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("应超时返回 error")
	}
	if !strings.Contains(err.Error(), "超时") {
		t.Errorf("错误信息应说明超时: %v", err)
	}
	if elapsed > 3*time.Second {
		t.Errorf("超时耗时 %s 过长（应接近 800ms）", elapsed)
	}
}

// TestDialWebSocketRejected 非 101 响应必须报错。
func TestDialWebSocketRejected(t *testing.T) {
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
		defer conn.Close()
		conn.Write([]byte("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n"))
	}()

	if _, err := DialWebSocket("ws://"+ln.Addr().String()+"/x", 3*time.Second); err == nil {
		t.Fatal("非 101 应报错")
	}
}

// TestFindBrowser 本机应能找到 Edge 或 Chrome（找不到时跳过，不算失败）。
func TestFindBrowser(t *testing.T) {
	p, err := FindBrowser()
	if err != nil {
		t.Skipf("本机无 Edge/Chrome，跳过: %v", err)
	}
	if p == "" {
		t.Error("返回了空路径")
	}
	t.Logf("找到浏览器: %s", p)
}

// TestFreePort 分配的端口必须可再次监听（确实空闲）。
func TestFreePort(t *testing.T) {
	p, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
	if err != nil {
		t.Fatalf("分配的端口 %d 实际不可用: %v", p, err)
	}
	ln.Close()
}
