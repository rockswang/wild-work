package chatgpt

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件是离线 mock 上游/代理的测试基建（票 01 要求单测全离线）。

// okJSON 是标准成功响应（Content-Length + Connection: close，便于裸 TCP 上游使用）。
func okJSON(body string) string {
	return "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: " +
		itoa(len(body)) + "\r\nConnection: close\r\n\r\n" + body
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// readHead 读到一个完整的请求头块（不含 body）。
func readHead(br *bufio.Reader) (string, error) {
	var sb strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return sb.String(), err
		}
		sb.WriteString(line)
		if line == "\r\n" || line == "\n" {
			return sb.String(), nil
		}
	}
}

// rawUpstream 是裸 TCP 假上游：记录每个请求头块原文，回一份固定响应。
// 用它才能断言**头的顺序**（net/http 服务端解析后顺序信息就丢了）。
type rawUpstream struct {
	addr string
	ln   net.Listener

	mu   sync.Mutex
	reqs []string
}

func startRawUpstream(t *testing.T, reply string) *rawUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	u := &rawUpstream{addr: ln.Addr().String(), ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				head, err := readHead(br)
				if err != nil {
					return
				}
				u.mu.Lock()
				u.reqs = append(u.reqs, head)
				u.mu.Unlock()
				_, _ = c.Write([]byte(reply))
			}(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return u
}

func (u *rawUpstream) base() string { return "http://" + u.addr }

func (u *rawUpstream) requests() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.reqs...)
}

func (u *rawUpstream) wait(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if r := u.requests(); len(r) > 0 {
			return r[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("假上游没收到请求")
	return ""
}

// helloCapture 抓 ClientHello（用于断言 TLS 指纹档真的生效）。
// GetConfigForClient 记录后直接返回错误中止握手——不需要证书，也不需要完成请求。
type helloCapture struct {
	addr string
	ch   chan *tls.ClientHelloInfo
}

func startHelloCapture(t *testing.T) *helloCapture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	hc := &helloCapture{addr: ln.Addr().String(), ch: make(chan *tls.ClientHelloInfo, 4)}
	cfg := &tls.Config{GetConfigForClient: func(chi *tls.ClientHelloInfo) (*tls.Config, error) {
		select {
		case hc.ch <- chi:
		default:
		}
		return nil, errors.New("captured by test")
	}}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { defer c.Close(); _ = tls.Server(c, cfg).Handshake() }(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return hc
}

func (hc *helloCapture) wait(t *testing.T) *tls.ClientHelloInfo {
	t.Helper()
	select {
	case chi := <-hc.ch:
		return chi
	case <-time.After(5 * time.Second):
		t.Fatal("没抓到 ClientHello")
		return nil
	}
}

// silentUpstream 接受连接但永不回包（测等响应头看门狗）。
type silentUpstream struct {
	addr string
	ln   net.Listener
}

func startSilentUpstream(t *testing.T) *silentUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// 保持连接打开但不写任何字节
			t.Cleanup(func() { _ = c.Close() })
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return &silentUpstream{addr: ln.Addr().String(), ln: ln}
}

// proxyCapture 是裸 TCP 代理记录器：按 proto 走 CONNECT / SOCKS5 握手，
// 记录隧道内看到的 HTTP 请求头块，然后回一份固定响应。
type proxyCapture struct {
	addr    string
	proto   string // "http" | "socks5"
	ln      net.Listener
	mu      sync.Mutex
	reqs    []string
	connect []string
}

func startProxy(t *testing.T, proto, reply string) *proxyCapture {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &proxyCapture{addr: ln.Addr().String(), proto: proto, ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c, reply)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *proxyCapture) serve(c net.Conn, reply string) {
	defer c.Close()
	br := bufio.NewReader(c)
	if p.proto == "socks5" {
		if !p.socksHandshake(c, br) {
			return
		}
	} else {
		head, err := readHead(br)
		if err != nil {
			return
		}
		p.mu.Lock()
		p.connect = append(p.connect, head)
		p.mu.Unlock()
		if !strings.HasPrefix(head, "CONNECT ") {
			return
		}
		_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	}
	head, err := readHead(br)
	if err != nil {
		return
	}
	p.mu.Lock()
	p.reqs = append(p.reqs, head)
	p.mu.Unlock()
	_, _ = c.Write([]byte(reply))
}

// socksHandshake 走最小 SOCKS5「无认证 + CONNECT」握手；失败返回 false。
func (p *proxyCapture) socksHandshake(c net.Conn, br *bufio.Reader) bool {
	hdr := make([]byte, 2)
	if _, err := readFull(br, hdr); err != nil || hdr[0] != 0x05 {
		return false
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := readFull(br, methods); err != nil {
		return false
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return false
	}
	req := make([]byte, 4)
	if _, err := readFull(br, req); err != nil || req[1] != 0x01 {
		return false
	}
	var addrLen int
	switch req[3] {
	case 0x01:
		addrLen = 4
	case 0x03:
		n, err := br.ReadByte()
		if err != nil {
			return false
		}
		addrLen = int(n)
	case 0x04:
		addrLen = 16
	default:
		return false
	}
	if _, err := readFull(br, make([]byte, addrLen+2)); err != nil {
		return false
	}
	_, err := c.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
	return err == nil
}

func readFull(br *bufio.Reader, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := br.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (p *proxyCapture) proxyURL() string { return p.proto + "://" + p.addr }

func (p *proxyCapture) waitRequest(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		if len(p.reqs) > 0 {
			r := p.reqs[0]
			p.mu.Unlock()
			return r
		}
		p.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("代理没收到隧道内请求")
	return ""
}
