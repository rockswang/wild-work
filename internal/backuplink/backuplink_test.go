package backuplink

import (
	"net/http"
	"testing"
	"time"
)

// 本包只有一个职责：持有「备用上游通道」的地址并按需交出绑定它的 client。
// 测试锁三件事：判据单一来源、未配置时零影响、换地址后渠道能作废自己的学习结果。

func TestSetValidation(t *testing.T) {
	t.Cleanup(func() { _ = Set("") })

	if Enabled() {
		t.Fatal("默认应为未配置（不参与任何请求）")
	}
	if c := Client(nil); c != nil {
		t.Fatal("未配置时 Client 必须返回 nil，让调用方走直连")
	}

	for _, raw := range []string{"http://127.0.0.1:7890", "https://proxy.example:443", "socks5://127.0.0.1:1080"} {
		if err := Set(raw); err != nil {
			t.Fatalf("合法地址被拒 %q: %v", raw, err)
		}
		if Raw() != raw || URL() == nil {
			t.Fatalf("回显不一致：%q", Raw())
		}
	}

	// 协议白名单与形态校验（旧实现三处各写一份，这里收敛成唯一判据）
	for _, raw := range []string{"socks4://127.0.0.1:1080", "127.0.0.1:7890", "://x", "http://"} {
		if err := Set(raw); err == nil {
			t.Errorf("非法地址被放行：%q", raw)
		}
	}
	// 校验失败不得污染现值（否则一次误填就把可用链路清空）
	_ = Set("socks5://127.0.0.1:1080")
	if err := Set("ftp://x"); err == nil || Raw() != "socks5://127.0.0.1:1080" {
		t.Errorf("非法 Set 应报错且不改现值：raw=%q", Raw())
	}

	// 空串 = 关闭
	if err := Set(""); err != nil {
		t.Fatalf("关闭失败：%v", err)
	}
	if Enabled() || Raw() != "" || Client(nil) != nil {
		t.Error("关闭后一切应回到未配置")
	}
}

// Parse 是代理地址合法性的**唯一**判据（config.normalize 与面板保存都调它，避免两处漂移）。
func TestParseValidation(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:7890", "https://user:pass@proxy.example:443", "socks5://127.0.0.1:1080", "  socks5://127.0.0.1:1080  "} {
		if u, err := Parse(raw); err != nil || u == nil {
			t.Errorf("合法地址被判错 %q: u=%v err=%v", raw, u, err)
		}
	}
	for _, raw := range []string{"127.0.0.1:7890", "ftp://127.0.0.1:21", "socks4://127.0.0.1:1080", "http://", "://x"} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("非法地址被放行 %q", raw)
		}
	}
	if u, err := Parse(""); err != nil || u != nil {
		t.Errorf("空串应表示「关闭」（nil,nil）：u=%v err=%v", u, err)
	}
}

func TestGenerationBumpsOnSet(t *testing.T) {
	t.Cleanup(func() { _ = Set("") })
	g0 := Generation()
	if err := Set("http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	g1 := Generation()
	if g1 <= g0 {
		t.Fatalf("Set 必须递增 generation（渠道据此作废「该模型只能走备用」的学习结果）：%d → %d", g0, g1)
	}
	if err := Set(""); err != nil {
		t.Fatal(err)
	}
	g2 := Generation()
	if g2 <= g1 {
		t.Fatalf("关闭也要递增 generation（渠道据此作废学习结果）：%d → %d", g1, g2)
	}
	if err := Set("bad"); err == nil || Generation() != g2 {
		t.Error("校验失败不应递增 generation")
	}
}

// Client 必须复用连接池：同一 base 反复取用返回同一实例，且只改出口、不改其余参数。
func TestClientCachesByBase(t *testing.T) {
	t.Cleanup(func() { _ = Set("") })
	if err := Set("http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	base := &http.Transport{
		ResponseHeaderTimeout: 90 * time.Second,
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
	}
	c1, c2 := Client(base), Client(base)
	if c1 == nil || c1 != c2 {
		t.Fatal("同一 base 应复用同一 client（否则每发新建 Transport、连接池无法复用）")
	}
	if c1.Timeout != 0 {
		t.Errorf("备用 client 不得设 Client.Timeout（R35：流式必被掐断）：%v", c1.Timeout)
	}
	tr, ok := c1.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport 类型异常：%T", c1.Transport)
	}
	if tr == base {
		t.Error("必须克隆 base，不能就地改 Proxy（会污染渠道直连链路）")
	}
	if tr.Proxy == nil {
		t.Error("备用 Transport 未设 Proxy")
	}
	// 除出口外与出厂同构：换链路不得引入新的超时行为
	if tr.ResponseHeaderTimeout != 90*time.Second || tr.MaxIdleConns != 64 || tr.IdleConnTimeout != 90*time.Second {
		t.Errorf("base 参数未继承：%+v", tr)
	}

	// 另一个 base ⇒ 另一个实例（各渠道出厂参数不同，不能共用一份）
	other := &http.Transport{ResponseHeaderTimeout: 120 * time.Second}
	if c3 := Client(other); c3 == c1 {
		t.Error("不同 base 必须各建一份")
	} else if tr3, _ := c3.Transport.(*http.Transport); tr3.ResponseHeaderTimeout != 120*time.Second {
		t.Errorf("other base 的参数未继承：%v", tr3.ResponseHeaderTimeout)
	}
}

// 锁定实盘发现的坑：渠道出厂 Transport 常是「无 DialContext + 无 TLSClientConfig + 无 TLSNextProto」
// 形态（cline.newTransport 即如此），Clone 后不补禁 h2 就会让备用链路拿到 h2 帧而永远拨不通。
func TestClientDisablesHTTP2EvenWhenBaseLooksPlain(t *testing.T) {
	t.Cleanup(func() { _ = Set("") })
	if err := Set("http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	clineLike := &http.Transport{ // 与 cline.newTransport 同形：没设 DialContext / TLSNextProto
		DisableKeepAlives: false, ForceAttemptHTTP2: false, MaxIdleConns: 64, MaxIdleConnsPerHost: 32,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second, ResponseHeaderTimeout: 90 * time.Second,
	}
	c := Client(clineLike)
	tr := c.Transport.(*http.Transport)
	if tr.TLSNextProto == nil || tr.ForceAttemptHTTP2 || tr.DialContext == nil {
		t.Errorf("备用链路必须显式禁 h2 并自带拨号器：TLSNextProto==nil:%v forceH2:%v dialer:%v",
			tr.TLSNextProto == nil, tr.ForceAttemptHTTP2, tr.DialContext == nil)
	}
	// 不得就地污染渠道自己的直连 Transport
	if clineLike.Proxy != nil {
		t.Error("克隆时不得改 base 的 Proxy（会污染渠道直连链路）")
	}
}

// base 为 nil（渠道 client 没设 Transport）时退回内置保守底座，仍要绑上代理。
func TestClientNilBaseFallback(t *testing.T) {
	t.Cleanup(func() { _ = Set("") })
	if err := Set("socks5://127.0.0.1:1080"); err != nil {
		t.Fatal(err)
	}
	c := Client(nil)
	if c == nil {
		t.Fatal("已配置时必须返回 client")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("nil base 兜底 Transport 也应绑定代理")
	}
	if tr.TLSNextProto == nil {
		t.Error("兜底底座必须显式禁 HTTP/2（与各渠道出厂同构）")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("兜底底座必须有 ResponseHeaderTimeout，否则连响应头都等不到会无限挂")
	}
}
