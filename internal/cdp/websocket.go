// Package cdp 最小 Chrome DevTools Protocol 客户端。
//
// 用途：为智谱清言渠道提供「自动获取登录凭据」能力——拉起一个独立 profile 的
// Edge/Chrome 实例，让用户在真实浏览器里正常登录，再从 CDP 读回 Cookie。
//
// 为什么需要 WebSocket：CDP 的 Network.getAllCookies 等命令只能经 WebSocket 调用，
// HTTP 端点（/json/*）仅用于发现目标。Go 标准库无 WebSocket，故此处实现 RFC 6455
// 的最小客户端——只覆盖本场景所需：客户端掩码文本帧、服务端文本帧、ping/pong、close。
// 不引入第三方依赖（与项目「最小依赖」取向一致）。
//
// 为什么不用「直接读浏览器 Cookie 数据库」：实测 Edge 运行时对
// `User Data/Default/Network/Cookies` 持**独占锁**（20 个进程），既不能读也不能复制；
// 且 Cookie 值是 DPAPI + AES-GCM 加密的。CDP 是唯一稳定路径，也是 Playwright/
// Puppeteer 等工具的通行做法。
package cdp

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// wsMagicGUID 是 RFC 6455 规定的握手魔数。
const wsMagicGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WebSocket 是最小 WebSocket 客户端连接。
type WebSocket struct {
	conn net.Conn
	br   *bufio.Reader

	// mu 保护连接状态（closed/err/pending/nextID）。**不得跨阻塞网络写持有**——
	// 否则对端停止读取时 writeFrame 永久持锁，Close 也拿不到锁 → 死锁
	// （见 hang_test.go TestWriteDoesNotHangClose）。
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan wsResponse
	closed  bool
	err     error

	// wmu 串行化帧写入（RFC 6455 要求帧不可交错），与 mu 分离：
	// 持 wmu 期间只做网络写，不碰连接状态。
	wmu sync.Mutex

	// writeTimeout 单次帧写入的时限。对端停止读取（TCP 发送缓冲写满）时
	// 靠它让写操作返回错误，而不是永久阻塞。
	writeTimeout time.Duration

	// onEvent 处理无 id 的事件帧（本场景未使用，保留扩展位）。
	onEvent func(method string, params json.RawMessage)
}

// wsResponse 一次 CDP 调用的响应。
type wsResponse struct {
	Result json.RawMessage
	Error  *wsError
}

type wsError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *wsError) Error() string { return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message) }

// DialWebSocket 连接一个 ws:// 地址（仅支持本机 CDP，故不处理 wss/TLS）。
func DialWebSocket(rawURL string, timeout time.Duration) (*WebSocket, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("cdp: 非法 ws 地址: %w", err)
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}

	// 1) TCP 连接
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, fmt.Errorf("cdp: 连接 %s 失败: %w", host, err)
	}

	// 2) HTTP Upgrade 握手
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		conn.Close()
		return nil, fmt.Errorf("cdp: 生成握手 key 失败: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)

	path := u.Path
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", path, u.Host, key)

	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("cdp: 发送握手失败: %w", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("cdp: 读握手响应失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		conn.Close()
		return nil, fmt.Errorf("cdp: 握手被拒 HTTP %d", resp.StatusCode)
	}
	// 校验 Sec-WebSocket-Accept
	want := wsAccept(key)
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != want {
		conn.Close()
		return nil, fmt.Errorf("cdp: Sec-WebSocket-Accept 不匹配")
	}

	ws := &WebSocket{
		conn:    conn,
		br:      br,
		pending: map[int64]chan wsResponse{},
		// 写超时：对端停止读取时让写操作返回而非永久阻塞。
		// 取值与 CDP 命令超时同量级，避免误杀正常的大帧写入。
		writeTimeout: 15 * time.Second,
	}
	go ws.readLoop()
	return ws, nil
}

// wsAccept 计算期望的 Sec-WebSocket-Accept。
func wsAccept(key string) string {
	h := sha1.Sum([]byte(key + wsMagicGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// readLoop 持续读取帧并分派响应。
func (w *WebSocket) readLoop() {
	for {
		opcode, payload, err := w.readFrame()
		if err != nil {
			w.fail(err)
			return
		}
		switch opcode {
		case opText:
			w.dispatch(payload)
		case opPing:
			// 回 pong（尽力而为）
			_ = w.writeFrame(opPong, payload)
		case opClose:
			w.fail(io.EOF)
			return
		case opPong, opContinuation:
			// 忽略
		}
	}
}

// dispatch 解析一条 CDP 消息并唤醒等待者。
func (w *WebSocket) dispatch(payload []byte) {
	var msg struct {
		ID     *int64          `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *wsError        `json:"error"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(payload, &msg); err != nil {
		return
	}
	if msg.ID == nil {
		// 事件帧
		if w.onEvent != nil && msg.Method != "" {
			w.onEvent(msg.Method, msg.Params)
		}
		return
	}
	w.mu.Lock()
	ch := w.pending[*msg.ID]
	delete(w.pending, *msg.ID)
	w.mu.Unlock()
	if ch != nil {
		ch <- wsResponse{Result: msg.Result, Error: msg.Error}
	}
}

// fail 标记连接失败并唤醒所有等待者。
func (w *WebSocket) fail(err error) {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.err = err
	pend := w.pending
	w.pending = map[int64]chan wsResponse{}
	w.mu.Unlock()
	for _, ch := range pend {
		ch <- wsResponse{Error: &wsError{Code: -1, Message: err.Error()}}
	}
	_ = w.conn.Close()
}

// Call 发一条 CDP 命令并等响应。
func (w *WebSocket) Call(method string, params any, timeout time.Duration) (json.RawMessage, error) {
	w.mu.Lock()
	if w.closed {
		err := w.err
		w.mu.Unlock()
		if err == nil {
			err = io.EOF
		}
		return nil, fmt.Errorf("cdp: 连接已关闭: %w", err)
	}
	w.nextID++
	id := w.nextID
	ch := make(chan wsResponse, 1)
	w.pending[id] = ch
	w.mu.Unlock()

	var rawParams json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		rawParams = b
	}
	msg := map[string]any{"id": id, "method": method}
	if rawParams != nil {
		msg["params"] = rawParams
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return nil, err
	}
	if err := w.writeFrame(opText, body); err != nil {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
		return nil, err
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-time.After(timeout):
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
		return nil, fmt.Errorf("cdp: 命令 %s 超时", method)
	}
}

// Close 关闭连接。
//
// 关键：**不等待写锁**。若另一个 Call 正阻塞在网络写里（对端停止读取），
// 等写锁会让 Close 一起挂住。这里直接关底层连接——写操作会因连接关闭而返回错误，
// 从而释放写锁。close 帧尽力而为，不保证送达（对端已经不健康时才走到这里）。
func (w *WebSocket) Close() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return nil
	}
	w.closed = true
	w.mu.Unlock()

	// 用带极短超时的独立写尝试发 close 帧；失败无所谓。
	// 注意：不经过 writeFrame 的写锁，避免与阻塞中的写互相等待。
	func() {
		defer func() { _ = recover() }()
		if w.writeTimeout > 0 {
			_ = w.conn.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
		}
		// close 帧：FIN + opClose + 掩码长度 0
		_, _ = w.conn.Write([]byte{0x80 | opClose, 0x80, 0, 0, 0, 0})
	}()

	// 关闭底层连接：这会唤醒阻塞中的读/写，使其返回错误。
	return w.conn.Close()
}

// ---------------------------------------------------------------------------
// 帧编解码（RFC 6455）
// ---------------------------------------------------------------------------

const (
	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xA
)

// writeFrame 写一个客户端帧（必须掩码）。
//
// 并发安全：持 wmu 串行化帧写入（RFC 6455 要求帧不可交错）；
// 期间**不持有 mu**，故对端停止读取导致写阻塞时，Close 仍能取得 mu 并关闭连接。
// 另设写超时，保证写操作最终返回而不是永久阻塞。
func (w *WebSocket) writeFrame(opcode byte, payload []byte) error {
	var header []byte
	header = append(header, 0x80|opcode) // FIN=1

	n := len(payload)
	switch {
	case n < 126:
		header = append(header, 0x80|byte(n))
	case n < 65536:
		header = append(header, 0x80|126)
		var ext [2]byte
		binary.BigEndian.PutUint16(ext[:], uint16(n))
		header = append(header, ext[:]...)
	default:
		header = append(header, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		header = append(header, ext[:]...)
	}

	// 掩码 key + 掩码后的载荷
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	header = append(header, mask...)
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}

	// 持写锁（而非状态锁）完成两次写，并施加写超时。
	// 关键：不持 mu —— 否则对端不读时这里永久持锁，Close 拿不到锁即死锁。
	w.wmu.Lock()
	defer w.wmu.Unlock()

	if w.writeTimeout > 0 {
		_ = w.conn.SetWriteDeadline(time.Now().Add(w.writeTimeout))
		defer func() { _ = w.conn.SetWriteDeadline(time.Time{}) }()
	}
	if _, err := w.conn.Write(header); err != nil {
		return err
	}
	if n > 0 {
		if _, err := w.conn.Write(masked); err != nil {
			return err
		}
	}
	return nil
}

// readFrame 读一个完整帧（含分片重组）。返回 opcode 与完整载荷。
func (w *WebSocket) readFrame() (byte, []byte, error) {
	var (
		frameOp   byte
		assembled []byte
	)
	for {
		// 帧头
		var h [2]byte
		if _, err := io.ReadFull(w.br, h[:]); err != nil {
			return 0, nil, err
		}
		fin := h[0]&0x80 != 0
		opcode := h[0] & 0x0F
		masked := h[1]&0x80 != 0
		length := int64(h[1] & 0x7F)

		switch length {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				return 0, nil, err
			}
			length = int64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(w.br, ext[:]); err != nil {
				return 0, nil, err
			}
			length = int64(binary.BigEndian.Uint64(ext[:]))
		}
		if length < 0 || length > 64<<20 { // 64MB 上限，防御异常长度
			return 0, nil, fmt.Errorf("cdp: 帧长度异常 %d", length)
		}

		var maskKey [4]byte
		if masked {
			if _, err := io.ReadFull(w.br, maskKey[:]); err != nil {
				return 0, nil, err
			}
		}

		payload := make([]byte, length)
		if length > 0 {
			if _, err := io.ReadFull(w.br, payload); err != nil {
				return 0, nil, err
			}
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i%4]
			}
		}

		// 控制帧（ping/pong/close）不参与分片重组，直接返回
		if opcode == opPing || opcode == opPong || opcode == opClose {
			return opcode, payload, nil
		}

		if opcode != opContinuation {
			frameOp = opcode
		}
		assembled = append(assembled, payload...)
		if fin {
			return frameOp, assembled, nil
		}
	}
}
