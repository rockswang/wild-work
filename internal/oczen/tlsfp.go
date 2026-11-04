package oczen

import (
	"context"
	_ "embed"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

// clientHelloBun 是官方 OpenCode 2.2.5 CLI（Bun 1.4.4 / BoringSSL）发往
// opencode.ai:443 的**原始 TLS ClientHello**（完整 TLS record，1502 B）。
//
// 取证（2026-11-04，实测）：免费档闸门判的是 **ClientHello 指纹**，不是请求内容。
// 同一份「整形后的出站体」（wildwork-shaped-body.json，24041 B）：
//
//	Go 默认 Transport（Go crypto/tls，被 TLSNextProto 钉在 HTTP/1.1） → 429 FreeUsageLimitError
//	Node https.request（OpenSSL，HTTP/1.1）                        → 429
//	Chrome 同源 fetch（BoringSSL，h2）                              → 429
//	uTLS HelloChrome_133 预设（h1.1）                               → 429
//	uTLS 复刻本文件的 ClientHello（h1.1）                            → **200 + 真实 SSE 流**
//
// 官方 CLI 自己发同样的体 → 200。故请求体/头/路径/会话 id/Bearer public 均非判据，
// 只有 ClientHello（JA3 771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49161-
// 49171-49162-49172-156-157-47-53,0-23-65281-10-11-35-16-5-13-18-51-45-43,4588-29-23-24,0
// / md5 1523504b38f0fae0d881d4b6554aac1b）通过。
//
// 抓取方式：本机 CONNECT 嗅探器（只回 200 并记录首个 TLS record，不落地 MITM）+ 官方 CLI
// 经 HTTPS_PROXY 发起，因此 SNI 就是 opencode.ai，与真实请求逐字节同源。
//
// 维护提示：这份指纹是"官方客户端当前长什么样"的快照。若上游更新客户端并更换闸门白名单，
// 需要重新抓一份（scripts/ 未提供，抓取脚本见运维项目 dev/oczen-tls-test/hello-snoop.js）。
//
//go:embed assets/clienthello_bun_2.2.5.bin
var clientHelloBun []byte

// officialClientHelloSpec 把嵌入的 ClientHello 解析成 uTLS 规范。
// 用 AllowBluntMimicry 让未识别的扩展原样透传，保证扩展集合/顺序与官方一致。
func officialClientHelloSpec() (*utls.ClientHelloSpec, error) {
	fp := &utls.Fingerprinter{AllowBluntMimicry: true}
	spec, err := fp.RawClientHello(clientHelloBun)
	if err != nil {
		return nil, fmt.Errorf("解析官方 ClientHello: %w", err)
	}
	return spec, nil
}

// tlsHandshakeTimeout 指纹握手的上限，与 Transport.TLSHandshakeTimeout 取同值。
//
// 必须自己加：net/http 只在自己实现的 addTLS 分支里给握手套定时器，
// 走自定义 DialTLSContext 的连接**不受 TLSHandshakeTimeout 约束**（Go transport.go
// 的 customDialTLS 分支直接调用，无定时器）。oczen 的 StreamHTTP 又没有整体 Timeout，
// 于是遇到只接受连接不回包的静默上游会无限挂住（2026-11-04 独立验证实测：>25s 不返回，
// 而原生路径 10.02s 超时）。
// 用 var 而非 const：测试要临时调小它，才能把「生产接线」也纳入超时断言的覆盖
// ——officialFingerprintDialer 传的就是这个值，若只测可注入参数的 fingerprintDialer，
// 把接线改成传 0（超时失效、静默上游无限挂住）时测试仍然绿（2026-11-04 独立验证发现）。
var tlsHandshakeTimeout = 10 * time.Second

// officialFingerprintDialer 返回一个 DialTLSContext：先按原样 TCP 建连，
// 再用官方客户端的 ClientHello 握手（uTLS HelloCustom + ApplyPreset）。
//
// 每次连接都重新解析一遍 spec：ApplyPreset 的 slice/指针字段会跨连接共享，
// 复用同一份 spec 有并发与状态污染风险，而解析 1.5 KB 的代价可以忽略。
func officialFingerprintDialer(dialContext func(ctx context.Context, network, addr string) (net.Conn, error)) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return fingerprintDialer(dialContext, tlsHandshakeTimeout)
}

// fingerprintDialer 同 officialFingerprintDialer，但握手超时可注入（测试用）。
func fingerprintDialer(dialContext func(ctx context.Context, network, addr string) (net.Conn, error), handshakeTimeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		spec, err := officialClientHelloSpec()
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		uc := utls.UClient(conn, &utls.Config{
			ServerName: host,
			// 官方 CLI 的 ALPN 只有 http/1.1（实测），与本 Transport 的
			// TLSNextProto 空表一致；写明确值避免与 spec 内的 ALPN 扩展打架。
			NextProtos: []string{"http/1.1"},
		}, utls.HelloCustom)
		if err := uc.ApplyPreset(spec); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("应用官方 ClientHello: %w", err)
		}
		hctx := ctx
		if handshakeTimeout > 0 {
			var cancel context.CancelFunc
			hctx, cancel = context.WithTimeout(ctx, handshakeTimeout)
			defer cancel()
		}
		if err := uc.HandshakeContext(hctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return uc, nil
	}
}

// fingerprintDisabled 诊断开关：OCZEN_NO_FINGERPRINT=1 时退回 Go 原生 ClientHello，
// 用于 A/B 复现「429 只由指纹决定」（关掉即恢复 429）。生产不要设置。
func fingerprintDisabled() bool {
	v := strings.TrimSpace(os.Getenv("OCZEN_NO_FINGERPRINT"))
	return v == "1" || strings.EqualFold(v, "true")
}
