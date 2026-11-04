package oczen

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 官方 OpenCode 2.2.5 CLI（Bun 1.4.4）发往 opencode.ai 的 ClientHello 指纹（实测快照）。
const (
	wantJA3    = "771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49161-49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43,4588-29-23-24,0"
	wantJA3MD5 = "1523504b38f0fae0d881d4b6554aac1b"
)

// TestEmbeddedClientHelloAsset 校验嵌入的抓包资产本身就是官方指纹（资产被换掉时立刻失败）。
func TestEmbeddedClientHelloAsset(t *testing.T) {
	got, err := ja3OfClientHelloRecord(clientHelloBun)
	if err != nil {
		t.Fatalf("解析嵌入资产失败: %v", err)
	}
	if got != wantJA3 {
		t.Fatalf("嵌入资产的 JA3 与取证不一致:\n got %s\nwant %s", got, wantJA3)
	}
	if _, err := officialClientHelloSpec(); err != nil {
		t.Fatalf("officialClientHelloSpec 失败: %v", err)
	}
}

// TestOfficialClientHelloIsMimicked 是本次修复的核心回归：真实拨号（打到本地监听）
// 时，出站 ClientHello 必须与官方 CLI 的 JA3 完全一致。
//
// 拨号目标仍然写成 opencode.ai:443（只有底层 TCP 被换成本地监听），这样 SNI 与线上一致，
// JA3 的扩展集合才可比。
func TestOfficialClientHelloIsMimicked(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	record := make(chan []byte, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 2048)
		for {
			n, err := conn.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
				if total, ok := clientHelloRecordLen(buf); ok && len(buf) >= total {
					record <- buf[:total]
					return
				}
			}
			if err != nil {
				record <- buf
				return
			}
		}
	}()

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		// 把 TCP 目标换成本地监听，但 addr（→ SNI）保持 opencode.ai。
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, ln.Addr().String())
	}
	tlsDial := officialFingerprintDialer(dial)
	// 服务端不会完成握手（我们只收 ClientHello 就关），这里的错误可以忽略。
	_, _ = tlsDial(context.Background(), "tcp", "opencode.ai:443")

	var got []byte
	select {
	case got = <-record:
	case <-time.After(6 * time.Second):
		t.Fatal("没有收到 ClientHello")
	}

	ja3, err := ja3OfClientHelloRecord(got)
	if err != nil {
		t.Fatalf("解析出站 ClientHello 失败: %v", err)
	}
	if ja3 != wantJA3 {
		sum := md5.Sum([]byte(ja3))
		t.Fatalf("出站 ClientHello 指纹不是官方指纹（429 会复现）:\n got %s (md5 %s)\nwant %s (md5 %s)",
			ja3, hex.EncodeToString(sum[:]), wantJA3, wantJA3MD5)
	}
}

// TestNewWiresFingerprintDialer 确认生产默认客户端确实挂上了指纹拨号器，且诊断开关能关掉。
func TestNewWiresFingerprintDialer(t *testing.T) {
	tr, ok := New().HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatal("New() 的 Transport 不是 *http.Transport")
	}
	if tr.DialTLSContext == nil {
		t.Fatal("默认客户端未挂载指纹 DialTLSContext")
	}
	if tr.TLSClientConfig != nil {
		t.Fatal("不应设置 TLSClientConfig：指纹由 uTLS 接管")
	}

	t.Setenv("OCZEN_NO_FINGERPRINT", "1")
	tr2, ok := New().HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatal("New() 的 Transport 不是 *http.Transport")
	}
	if tr2.DialTLSContext != nil {
		t.Fatal("OCZEN_NO_FINGERPRINT=1 时应退回 Go 原生 ClientHello")
	}
}

// TestFingerprintDialerHandshakeTimeout 守住一个真实回归（2026-11-04 独立验证发现）：
// **走自定义 DialTLSContext 的连接不受 Transport.TLSHandshakeTimeout 约束**——net/http 只在
// 自己的 addTLS 分支里装握手定时器。若不在拨号器内自加超时，遇到只接受连接不回包的静默上游，
// oczen 的 StreamHTTP（无整体 Timeout）会无限挂住（实测 >25s，而原生路径 10.02s 超时）。
func TestFingerprintDialerHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() { // 静默上游：只接受连接，不读也不回
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				time.Sleep(3 * time.Second)
			}(conn)
		}
	}()

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, ln.Addr().String())
	}
	tlsDial := fingerprintDialer(dial, 300*time.Millisecond)
	start := time.Now()
	if _, err := tlsDial(context.Background(), "tcp", "opencode.ai:443"); err == nil {
		t.Fatal("静默上游应当握手失败，实际成功")
	} else if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("握手未被超时约束，耗时 %v（期望 ~300ms）", el)
	}
	if tlsHandshakeTimeout <= 0 {
		t.Fatal("生产握手超时必须为正")
	}
}

// TestOfficialFingerprintDialerTimesOutOnSilentUpstream 覆盖**生产接线**这一层。
//
// 上面那条测的是 fingerprintDialer 的机制（直接注入 300ms），**不经过**
// officialFingerprintDialer；于是把生产接线改成传 0（超时失效、静默上游无限挂住）时它仍然绿
// （2026-11-04 独立验证用变异 A 发现）。这条临时调小超时常量后调**生产入口**，
// 并用 4s 守卫断言"它必须自己返回"，接线被改坏立刻变红。
func TestOfficialFingerprintDialerTimesOutOnSilentUpstream(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() { // 静默上游：只接不回，且长时间不关（对端主动关闭会掩盖超时失效）
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				time.Sleep(30 * time.Second)
			}(conn)
		}
	}()

	old := tlsHandshakeTimeout
	tlsHandshakeTimeout = 300 * time.Millisecond
	defer func() { tlsHandshakeTimeout = old }()

	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, network, ln.Addr().String())
	}
	tlsDial := officialFingerprintDialer(dial) // ← 生产入口，不注入参数
	done := make(chan error, 1)
	go func() {
		_, err := tlsDial(context.Background(), "tcp", "opencode.ai:443")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("静默上游应当握手失败，实际成功")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("生产拨号器未在超时内返回：officialFingerprintDialer 的握手超时丢了")
	}
}

// TestJA3ParserDropsGrease 守住一个真实踩过的坑：JA3 必须对 cipher / extension /
// supported_groups 三处都剔 GREASE。旧解析漏了 curves，带 GREASE 的 hello（如 Chrome）
// 会算出"假不符"（2026-11-04 由独立交叉核对队友用 Chrome hello 变异时发现）。
func TestJA3ParserDropsGrease(t *testing.T) {
	body := []byte{0x03, 0x03}                                                      // legacy_version
	body = append(body, make([]byte, 32)...)                                        // random
	body = append(body, 0x00)                                                       // session_id 长度 0
	body = append(body, 0x00, 0x04)                                                 // cipher_suites 长度 = 4
	body = append(body, 0x0a, 0x0a)                                                 // GREASE cipher（须剔除）
	body = append(body, 0x13, 0x01)                                                 // TLS_AES_128_GCM_SHA256
	body = append(body, 0x01, 0x00)                                                 // compression: 长度 1，null
	exts := []byte{0x1a, 0x1a, 0x00, 0x00}                                          // GREASE 扩展（须剔除）
	exts = append(exts, 0x00, 0x0a, 0x00, 0x06, 0x00, 0x04, 0x7a, 0x7a, 0x00, 0x1d) // supported_groups: GREASE + x25519
	exts = append(exts, 0x00, 0x0b, 0x00, 0x02, 0x01, 0x00)                         // ec_point_formats: [0]
	body = append(body, byte(len(exts)>>8), byte(len(exts)))
	body = append(body, exts...)

	record := []byte{0x16, 0x03, 0x01, byte(len(body) >> 8), byte(len(body)), 0x01}
	record = append(record, byte(len(body)>>16), byte(len(body)>>8), byte(len(body)))
	record = append(record, body...)

	got, err := ja3OfClientHelloRecord(record)
	if err != nil {
		t.Fatalf("解析合成 ClientHello 失败: %v", err)
	}
	want := "771,4865,10-11,29,0" // GREASE cipher/ext/curve 全部剔除
	if got != want {
		t.Fatalf("GREASE 未剔除:\n got %s\nwant %s", got, want)
	}
}

// ---------------------------------------------------------------------------
// 最小 ClientHello 解析（只用于指纹断言，不参与生产逻辑）
// ---------------------------------------------------------------------------

func clientHelloRecordLen(buf []byte) (int, bool) {
	if len(buf) < 9 || buf[0] != 0x16 || buf[5] != 0x01 {
		return 0, false
	}
	hsLen := int(buf[6])<<16 | int(buf[7])<<8 | int(buf[8])
	return 5 + 4 + hsLen, true
}

func isGrease(v uint16) bool { return v&0x0f0f == 0x0a0a }

// ja3OfClientHelloRecord 按 JA3 规范（version,ciphers,extensions,curves,ec_point_formats）
// 计算指纹串；GREASE 值一律剔除。
func ja3OfClientHelloRecord(record []byte) (string, error) {
	total, ok := clientHelloRecordLen(record)
	if !ok || len(record) < total {
		return "", fmt.Errorf("不是完整 ClientHello（len=%d）", len(record))
	}
	b := record[9:total] // 跳过 record 头(5) + handshake 头(4)
	if len(b) < 34 {
		return "", fmt.Errorf("ClientHello 过短")
	}
	o := 0
	legacy := uint16(b[0])<<8 | uint16(b[1])
	o += 2 + 32
	sidLen := int(b[o])
	o += 1 + sidLen
	csLen := int(b[o])<<8 | int(b[o+1])
	o += 2
	var ciphers []uint16
	for i := 0; i < csLen; i += 2 {
		c := uint16(b[o+i])<<8 | uint16(b[o+i+1])
		if !isGrease(c) {
			ciphers = append(ciphers, c)
		}
	}
	o += csLen
	compLen := int(b[o])
	o++
	o += compLen
	if o+2 > len(b) {
		return "", fmt.Errorf("无扩展段")
	}
	extTotal := int(b[o])<<8 | int(b[o+1])
	o += 2
	end := o + extTotal
	if end > len(b) {
		end = len(b)
	}
	var exts []uint16
	var curves, points []uint16
	for o+4 <= end {
		id := uint16(b[o])<<8 | uint16(b[o+1])
		l := int(b[o+2])<<8 | int(b[o+3])
		data := b[o+4 : o+4+l]
		o += 4 + l
		if !isGrease(id) {
			exts = append(exts, id)
		}
		switch id {
		case 10: // supported_groups
			if len(data) >= 2 {
				n := int(data[0])<<8 | int(data[1])
				for i := 0; i+1 < n; i += 2 {
					g := uint16(data[2+i])<<8 | uint16(data[3+i])
					if !isGrease(g) { // JA3 对 curves 同样剔 GREASE
						curves = append(curves, g)
					}
				}
			}
		case 11: // ec_point_formats
			if len(data) >= 1 {
				n := int(data[0])
				for i := 0; i < n; i++ {
					points = append(points, uint16(data[1+i]))
				}
			}
		}
	}
	nums := func(vs []uint16) string {
		parts := make([]string, 0, len(vs))
		for _, v := range vs {
			parts = append(parts, fmt.Sprintf("%d", v))
		}
		return strings.Join(parts, "-")
	}
	return fmt.Sprintf("%d,%s,%s,%s,%s", legacy, nums(ciphers), nums(exts), nums(curves), nums(points)), nil
}
