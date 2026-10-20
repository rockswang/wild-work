// oczen 的「地域限制 → 备用上游通道重放」回归（R62）。
//
// 编排实现在 internal/backuplink/route.go，与 cline 共用同一份；
// 本文件锁 oczen 侧的两件事：① 判据（403 + RegionError 文案，宁窄勿宽）；
// ② 接入正确（直连中招 → 同请求内换出口重放 → 成功；未配/拨不动 ⇒ 原样透传直连那份）。
package oczen

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"wild-work/internal/backuplink"
)

// regionBody 上游实测形态：403 + error.type=RegionError。
const regionBody = `{"type":"error","error":{"type":"RegionError","message":"This model is not available in your country."}}`

// otherBody403 是「403 但不是地域限制」的对照（如免费档形态校验失败）。
const otherBody403 = `{"type":"error","error":{"type":"FreeTierError","message":"invalid session id"}}`

// fakeOczen 假上游：经备用出口（带 X-Backup-Via 头）与直连返回不同结果。
type fakeOczen struct {
	direct      atomic.Int64
	backup      atomic.Int64
	regionMode  atomic.Bool // true = 直连被地域拒（备用正常出流）
	regionEvery atomic.Bool // true = 两条链路都被地域拒
	other403    atomic.Bool // true = 直连回 403 但非地域限制
}

func (f *fakeOczen) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		via := r.Header.Get("X-Backup-Via") == "1"
		if via {
			f.backup.Add(1)
		} else {
			f.direct.Add(1)
		}
		switch {
		case (f.regionMode.Load() && !via) || f.regionEvery.Load():
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, regionBody)
		case f.other403.Load():
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, otherBody403)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			tag := "direct"
			if via {
				tag = "backup"
			}
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok-"+tag+"\"}}]}\n\ndata: [DONE]\n\n")
		}
	}
}

// newOczenBackupCase 起假上游 + 假转发代理（代理默认不配置）。
func newOczenBackupCase(t *testing.T) (*fakeOczen, *Client, string) {
	t.Helper()
	up := &fakeOczen{}
	upSrv := httptest.NewServer(up.handler())
	t.Cleanup(upSrv.Close)
	// 最小 HTTP 转发代理：按请求里的绝对 URI 原样转发，并打上标记。
	tr := &http.Transport{Proxy: nil}
	pxSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequest(r.Method, r.URL.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		for k, vs := range r.Header {
			req.Header[k] = vs
		}
		req.Header.Set("X-Backup-Via", "1")
		resp, err := tr.RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(pxSrv.Close)
	t.Cleanup(func() { _ = backuplink.Set("") })

	c := NewWithBase(upSrv.URL)
	c.IdleTimeout = 0 // 测试禁用流空闲监控（无需真实计时）
	return up, c, pxSrv.URL
}

func oczenReq() []byte {
	return []byte(`{"model":"muse-spark-1.3-contributor-free","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
}

// ① 未配备用通道：地域限制原样透传，且不发第二发。
func TestOczenRegionBlockedWithoutBackupPassesThrough(t *testing.T) {
	up, c, _ := newOczenBackupCase(t)
	up.regionMode.Store(true)

	rc, status, body, err := c.ChatStream(AnonymousAuth(), oczenReq())
	if err != nil || rc != nil {
		t.Fatalf("应透传：err=%v", err)
	}
	if status != http.StatusForbidden || !strings.Contains(string(body), "RegionError") {
		t.Errorf("应透传上游原文：status=%d body=%s", status, body)
	}
	if up.backup.Load() != 0 {
		t.Errorf("未配置备用通道却有备用命中：%d", up.backup.Load())
	}
}

// ② 已配备用通道：同请求内重放成功（客户端无感）。
func TestOczenRegionBlockedRetriesViaBackup(t *testing.T) {
	up, c, px := newOczenBackupCase(t)
	up.regionMode.Store(true)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}

	rc, status, body, err := c.ChatStream(AnonymousAuth(), oczenReq())
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || rc == nil {
		t.Fatalf("应经备用出口成功：status=%d body=%s", status, body)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(got), "ok-backup") {
		t.Errorf("内容应来自备用出口：%s", got)
	}
	if up.direct.Load() != 1 || up.backup.Load() != 1 {
		t.Errorf("应恰好直连 1 次 + 备用 1 次：direct=%d backup=%d", up.direct.Load(), up.backup.Load())
	}
}

// ③ 学习路由：同模型第二发不再白撞直连。
func TestOczenLearnedRouteSkipsDirect(t *testing.T) {
	up, c, px := newOczenBackupCase(t)
	up.regionMode.Store(true)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.ChatStream(AnonymousAuth(), oczenReq()); err != nil {
		t.Fatal(err)
	}
	rc, status, _, err := c.ChatStream(AnonymousAuth(), oczenReq())
	if err != nil || status != http.StatusOK {
		t.Fatalf("第二发应直接经备用成功：status=%d err=%v", status, err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
	if up.direct.Load() != 1 {
		t.Errorf("直连只该撞一次（学习路由生效），实得 %d", up.direct.Load())
	}
}

// ④ 两条链路都被地域拒 ⇒ 透传**直连那份**，且不记学习结果（不把模型钉在坏出口）。
func TestOczenBothBlockedKeepsDirectCause(t *testing.T) {
	up, c, px := newOczenBackupCase(t)
	up.regionEvery.Store(true)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}

	rc, status, body, err := c.ChatStream(AnonymousAuth(), oczenReq())
	if err != nil || rc != nil {
		t.Fatalf("应透传：err=%v", err)
	}
	if status != http.StatusForbidden || !strings.Contains(string(body), "RegionError") {
		t.Errorf("必须透传直连首因：status=%d body=%s", status, body)
	}
	// 没学会 ⇒ 第二发仍先撞直连
	if _, _, _, err := c.ChatStream(AnonymousAuth(), oczenReq()); err != nil {
		t.Fatal(err)
	}
	if up.direct.Load() != 2 {
		t.Errorf("代理出口同样受限时不该记住走备用，直连应撞 2 次，实得 %d", up.direct.Load())
	}
}

// ⑤ 判据宁窄勿宽：403 但非地域限制（FreeTierError）绝不重放到第三方出口。
func TestOczenNonRegionErrorNeverRetried(t *testing.T) {
	up, c, px := newOczenBackupCase(t)
	up.other403.Store(true)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}

	_, status, _, err := c.ChatStream(AnonymousAuth(), oczenReq())
	if err != nil || status != http.StatusForbidden {
		t.Fatalf("应透传 403：status=%d err=%v", status, err)
	}
	if up.backup.Load() != 0 {
		t.Errorf("非地域限制不得重放：backup=%d", up.backup.Load())
	}
}

// ⑥ 备用通道拨不动 ⇒ 仍交出直连那份（不掩盖首因）。
func TestOczenBackupDialFailureKeepsDirectCause(t *testing.T) {
	up, c, px := newOczenBackupCase(t)
	up.regionMode.Store(true)
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 立刻关掉 ⇒ 拨号必失败
	_ = px
	if err := backuplink.Set(deadURL); err != nil {
		t.Fatal(err)
	}

	rc, status, body, err := c.ChatStream(AnonymousAuth(), oczenReq())
	if err != nil || rc != nil {
		t.Fatalf("不该冒泡传输层错误：err=%v", err)
	}
	if status != http.StatusForbidden || !strings.Contains(string(body), "RegionError") {
		t.Errorf("应透传直连 403 原文：status=%d body=%s", status, body)
	}
}

// ⑧ 改道成功后返回的流必须可完整读完（cancel 所有权 + Monitor 包装链路）。
// 锁两件事：① 200 分支的流在「本请求内发生过重放」后仍能读完并带 [DONE]；
// ② 空闲兜底确实包在了返回给上层的 body 上（曾把包装放在外层 Wrap 回调里、
// 靠共享变量取 cancel；该写法在 ErrNoBackup 提前返回路径还会漏调 cancel）。
// 说明：这是防回归的收尾断言，不是「曾导致线上故障」——原共享变量写法在常规
// 路径恰好正确（最后一次 attempt 就是成功那次），故不宣称它引起过实际故障。
func TestOczenStreamNotCancelledByLaterAttempt(t *testing.T) {
	up, c, px := newOczenBackupCase(t)
	up.regionMode.Store(true) // 首发必撞直连后重放，正好走「两次 attempt」路径
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}

	rc, status, _, err := c.ChatStream(AnonymousAuth(), oczenReq())
	if err != nil || status != http.StatusOK || rc == nil {
		t.Fatalf("应经备用出口成功：status=%d err=%v", status, err)
	}
	got, rerr := io.ReadAll(rc) // 完整读取：取消权/包装链路异常会在这里表现为读错/截断
	_ = rc.Close()
	if rerr != nil {
		t.Fatalf("流读取失败（取消权/包装链路异常）：%v", rerr)
	}
	if !strings.Contains(string(got), "ok-backup") || !strings.Contains(string(got), "[DONE]") {
		t.Errorf("流内容不完整：%q", got)
	}
}

// ⑨ TestKey（面板「测试」按钮）也走与真实对话同源的编排（issue #117）：
// 直连被地区拒 → 同请求内自动改走备用通道，并把「答复是否来自备用出口」如实回传面板。
// 原先 TestKey 直接用 c.HTTP 直连，用户配了代理点测试仍看到 403，也无从判断代理有没有生效。
func TestOczenTestKeyUsesBackupOnRegionBlock(t *testing.T) {
	up, c, px := newOczenBackupCase(t)
	up.regionMode.Store(true) // big-pickle 直连被地区拒（模拟用户只能靠代理的场景）
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}
	status, _, via, err := c.TestKey()
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || !via {
		t.Fatalf("测试应经备用出口成功：status=%d via=%v", status, via)
	}
	if up.backup.Load() != 1 {
		t.Errorf("应恰好经备用出口 1 次：%d", up.backup.Load())
	}
}

// ⑩ 未配备用通道时 TestKey 如实透传直连 403 且 via=false（面板据此提示“代理未生效”）。
func TestOczenTestKeyWithoutBackupReportsDirect(t *testing.T) {
	up, c, _ := newOczenBackupCase(t)
	up.regionMode.Store(true)
	status, body, via, err := c.TestKey()
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusForbidden || via {
		t.Fatalf("未配备用通道应透传直连 403 且 via=false：status=%d via=%v", status, via)
	}
	if !strings.Contains(body, "RegionError") {
		t.Errorf("应透传原文：%s", body)
	}
	if up.backup.Load() != 0 {
		t.Errorf("未配备用通道却有备用命中：%d", up.backup.Load())
	}
}

// ⑦ 判据单测：只有 403/451 + 地域文案才算，其余一律不算。
func TestOczenIsRegionBlocked(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{http.StatusForbidden, regionBody, true},
		{451, `{"error":{"type":"RegionError"}}`, true},
		{http.StatusForbidden, `{"error":"not available in your region"}`, true},
		{http.StatusForbidden, otherBody403, false},                   // 同状态码但非地域
		{http.StatusTooManyRequests, regionBody, false},               // 限流不是地域
		{http.StatusInternalServerError, regionBody, false},           // 5xx 不重放
		{http.StatusForbidden, `{"error":"access forbidden"}`, false}, // 宽文案不得触发重放
		// 出口 IP 级封禁（FreeTierError）：换出口 IP 可解 ⇒ 必须触发备用重放。
		// 2026-10-19 实盘：多节点命中此文案，换美国出口后同一实现 200。
		{http.StatusForbidden, `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free models aren't available from your current network because we've detected abusive traffic from this IP address. This isn't related to your account."}}`, true},
		// 客户端伪装不完整（can only be used from within OpenCode）：与 IP 无关，
		// 换出口无效，**不得**触发备用重放（否则每发都白走一次代理）。
		{http.StatusForbidden, `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode. Download it at https://opencode.ai and keep building."}}`, false},
	}
	for _, c := range cases {
		if got := IsRegionBlocked(c.status, c.body); got != c.want {
			t.Errorf("IsRegionBlocked(%d, %s) = %v; want %v", c.status, c.body, got, c.want)
		}
	}
}
