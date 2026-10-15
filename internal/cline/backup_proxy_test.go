package cline

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/backuplink"
)

// 本文件锁死「地区限制 → 备用上游通道重放」这套编排（决议 R57）。
//
// 测试里三个角色：
//   - 假上游（一个 httptest server 同时充当国内直连与海外出口）：
//     经代理进来的请求带 X-Backup-Via 头，据此给出不同答复；
//   - 假转发代理（另一个 httptest server）：Go 的 Transport 对 http:// 目标 + Proxy
//     会发**绝对 URI**，故 handler 里直接把 r.URL 转发出去即可，无需 CONNECT；
//   - 渠道 client：Base 指向假上游，备用通道指向假代理。

// 上游答复形态开关（测试用同一假上游按角色切换）。
type fakeUpstream struct {
	hits         atomic.Int64 // 直连命中数
	backupHits   atomic.Int64 // 经备用通道命中数
	regionMode   atomic.Bool  // true = 直连被地区拒绝（备用不受限）
	regionBackup atomic.Bool  // true = 备用链路被地区拒绝（用于验证「学到走备用但备用后来看不了」的回落）
	regionEvery  atomic.Bool  // true = 两条链路都被地区拒绝
	capOnBackup  atomic.Bool  // true = 备用链路上回共享上限（另一种上游答复）
	bodyTag      string
}

func (f *fakeUpstream) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		via := r.Header.Get("X-Backup-Via") == "1"
		if via {
			f.backupHits.Add(1)
		} else {
			f.hits.Add(1)
		}
		region := (f.regionMode.Load() && !via) || (f.regionBackup.Load() && via) || f.regionEvery.Load()
		switch {
		case region:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `{"error":"access forbidden: cline-free/muse-spark-1.3-contributor is not available in your region (%s)","success":false}`, tagOf(via))
		case via && f.capOnBackup.Load():
			// 备用链路上游给了**不同**的答复（共享上限）⇒ 应以备用那份为准
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, nsBodyCapBackup)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok-%s\"}}]}\n\ndata: [DONE]\n\n", tagOf(via))
		}
	}
}

func tagOf(via bool) string {
	if via {
		return "backup"
	}
	return "direct"
}

// nsBodyCapBackup 是备用链路上的另一种上游失败（非地区）：外层 500 + 内层 429 日限。
const nsBodyCapBackup = `{"error":"inference request failed: failed to invoke model 'poolside/laguna-xs:free" +
	" from Openrouter: request failed with status 429: Daily limit reached. Credits don't affect this cap."}`

// newBackupProxy 起一个最小 HTTP 转发代理：按请求里的**绝对 URI** 原样转发，
// 并打上 X-Backup-Via 标记（假上游据此区分两条链路）。
func newBackupProxy(t *testing.T) *httptest.Server {
	t.Helper()
	tr := &http.Transport{Proxy: nil} // 代理自己不再套代理
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
}

// newBackupCase 装配一组：假上游 + 假代理 + 渠道 client（代理默认**不**配置）。
func newBackupCase(t *testing.T) (*fakeUpstream, *Client, string) {
	t.Helper()
	up := &fakeUpstream{}
	upSrv := httptest.NewServer(up.handler())
	t.Cleanup(upSrv.Close)
	pxSrv := newBackupProxy(t)
	t.Cleanup(pxSrv.Close)
	t.Cleanup(func() { _ = backuplink.Set("") })

	c := New()
	c.Base = upSrv.URL
	return up, c, pxSrv.URL
}

func museReq() []byte {
	return []byte(`{"model":"cline-free/muse-spark-1.3-contributor","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
}

func testAuth() *auth.Auth { return &auth.Auth{Kind: "cline", AccessToken: "sk_test"} }

// ① 未配置备用通道 ⇒ 地区限制原样透传（旧行为不变），且绝不偷偷发第二个请求。
func TestRegionBlockedWithoutBackupPassesThrough(t *testing.T) {
	up, c, _ := newBackupCase(t)
	up.regionMode.Store(true)

	rc, status, body, err := c.ChatStream(testAuth(), museReq())
	if err != nil || rc != nil {
		t.Fatalf("应透传而非报传输层错误：err=%v", err)
	}
	if status != http.StatusForbidden {
		t.Fatalf("状态码应为 403，实得 %d", status)
	}
	if !strings.Contains(string(body), "not available in your region (direct)") {
		t.Errorf("应透传直连那份原文：%s", body)
	}
	if got := up.backupHits.Load(); got != 0 {
		t.Errorf("未配置备用通道却有备用命中：%d", got)
	}
}

// ② 命中地区限制 + 已配备用 ⇒ 同一次请求内重放成功（对 handler/账号惩罚完全透明）。
func TestRegionBlockedRetriesViaBackup(t *testing.T) {
	up, c, px := newBackupCase(t)
	up.regionMode.Store(true)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}

	rc, status, body, err := c.ChatStream(testAuth(), museReq())
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || rc == nil {
		t.Fatalf("备用重试应成功：status=%d body=%s", status, body)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(got), "ok-backup") {
		t.Errorf("流内容应来自备用链路：%s", got)
	}
	if up.hits.Load() != 1 || up.backupHits.Load() != 1 {
		t.Errorf("应恰好「直连 1 次 + 备用 1 次」：direct=%d backup=%d", up.hits.Load(), up.backupHits.Load())
	}
}

// ③ 学习路由：同模型第二发**不再先白撞直连**（省一发注定失败的请求）。
func TestLearnedRouteSkipsDirectAttempt(t *testing.T) {
	up, c, px := newBackupCase(t)
	up.regionMode.Store(true)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.ChatStream(testAuth(), museReq()); err != nil {
		t.Fatal(err)
	}
	rc, status, _, err := c.ChatStream(testAuth(), museReq())
	if err != nil || status != http.StatusOK {
		t.Fatalf("第二发应直接经备用成功：status=%d err=%v", status, err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
	if got := up.hits.Load(); got != 1 {
		t.Errorf("直连只该撞一次，实得 %d", got)
	}
	if got := up.backupHits.Load(); got != 2 {
		t.Errorf("备用该被用两次，实得 %d", got)
	}
}

// ④ 备用链路同样被地区拒绝 ⇒ 透传**直连那份**（首因不被掩盖，R37），且不记学习结果。
func TestBackupAlsoBlockedKeepsFirstCauseAndDoesNotLearn(t *testing.T) {
	up, c, px := newBackupCase(t)
	up.regionEvery.Store(true)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}

	rc, status, body, err := c.ChatStream(testAuth(), museReq())
	if err != nil || rc != nil {
		t.Fatalf("应透传：err=%v", err)
	}
	if status != http.StatusForbidden || !strings.Contains(string(body), "(direct)") {
		t.Errorf("必须透传直连首因而非备用那份：status=%d body=%s", status, body)
	}
	// 再来一发：没学会 ⇒ 仍然先撞直连
	if _, _, _, err := c.ChatStream(testAuth(), museReq()); err != nil {
		t.Fatal(err)
	}
	if got := up.hits.Load(); got != 2 {
		t.Errorf("代理出口同样受限时不该记住「走备用」（否则永久钉死），直连应撞 2 次，实得 %d", got)
	}
}

// ⑤ 备用链路拿到了**不同的**上游答复 ⇒ 以它为准（链路可达、真因已变），并记学习结果。
func TestBackupDifferentErrorWins(t *testing.T) {
	up, c, px := newBackupCase(t)
	up.regionMode.Store(true)
	up.capOnBackup.Store(true)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}

	rc, status, body, err := c.ChatStream(testAuth(), museReq())
	if err != nil || rc != nil {
		t.Fatalf("err=%v", err)
	}
	if status != http.StatusTooManyRequests || !strings.Contains(string(body), "Daily limit reached") {
		// 内层 429 必须被 effectiveStatus 还原（R47），故期望 429 而非外层 500
		t.Errorf("应采用备用链路的权威答复（内层 429 经 effectiveStatus 还原）：status=%d body=%s", status, body)
	}
	if !c.isCapped("cline-free/muse-spark-1.3-contributor") {
		t.Error("备用答复命中共享上限文案 ⇒ 必须记模型级冷却（不罚账号）")
	}
}

// ⑥ 判据宁窄勿宽：非地区类 403 绝不走备用（误判的代价是把流量发到第三方代理）。
func TestNonRegionErrorNeverRetried(t *testing.T) {
	up, c, px := newBackupCase(t)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}
	// 换成「403 但文案不含地区」的风控形态
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up.hits.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":"API_REQUEST_ERROR_CODE","message":"access forbidden"}}`)
	}))
	defer upSrv.Close()
	c.Base = upSrv.URL

	_, status, body, err := c.ChatStream(testAuth(), museReq())
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusForbidden {
		t.Errorf("状态码应透传：%d %s", status, body)
	}
	if got := up.backupHits.Load(); got != 0 {
		t.Errorf("非地区限制不得重放到备用链路，备用命中=%d", got)
	}
}

// ⑦ 备用配置一变 ⇒ 学习结果作废（否则换了代理还沿用旧结论）。
func TestBackupConfigChangeInvalidatesLearning(t *testing.T) {
	up, c, px := newBackupCase(t)
	up.regionMode.Store(true)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.ChatStream(testAuth(), museReq()); err != nil {
		t.Fatal(err)
	}
	if !c.regionLearned("cline-free/muse-spark-1.3-contributor") {
		t.Fatal("首发改道成功后应记下学习路由")
	}
	// 改地址（本例：关闭通道）⇒ generation 变，学习结果作废
	if err := backuplink.Set(""); err != nil {
		t.Fatal(err)
	}
	if c.regionLearned("cline-free/muse-spark-1.3-contributor") {
		t.Error("备用配置变更后学习结果必须作废")
	}
}

// ⑧ 已学到走备用，但上游/出口状态变了（备用现在被地区拒、直连反而能通）
// ⇒ 必须作废学习结果并**本轮回落直连**，否则最长要等 6h TTL，期间该模型一直报错。
func TestLearnedRouteFallsBackWhenBackupBecomesBlocked(t *testing.T) {
	up, c, px := newBackupCase(t)
	up.regionMode.Store(true)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}
	rc, status, _, err := c.ChatStream(testAuth(), museReq())
	if err != nil || status != http.StatusOK {
		t.Fatalf("预热应经备用成功：status=%d err=%v", status, err)
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()

	// 翻转：直连解除限制，备用出口反而被拒
	up.regionMode.Store(false)
	up.regionBackup.Store(true)

	rc, status, body, err := c.ChatStream(testAuth(), museReq())
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || rc == nil {
		t.Fatalf("应回落直连成功：status=%d body=%s", status, body)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(got), "ok-direct") {
		t.Errorf("内容应来自直连：%s", got)
	}
	if c.regionLearned("cline-free/muse-spark-1.3-contributor") {
		t.Error("备用被拒后必须作废学习结果（否则永久钉在坏链路上）")
	}
}

// ⑨ 主链路拨不动（直连网络故障）仍是**传输层错误**冒泡（不得被备用编排掩盖成 HTTP 错误）。
func TestDirectTransportErrorStillBubbles(t *testing.T) {
	_, c, px := newBackupCase(t)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}
	c.Base = "http://127.0.0.1:1/" // 必然拨不动
	if _, _, _, err := c.ChatStream(testAuth(), museReq()); err == nil {
		t.Fatal("直连拨不动必须冒泡传输层错误，交给 handler 按网络故障处理（计错/换号）")
	}
}

// ⑩ 已学到走备用，但备用随后不可达 ⇒ 回落直连一次并清除学习标记（不能让代理故障拖死可用模型）。
func TestBackupDownFallsBackToDirect(t *testing.T) {
	up, c, px := newBackupCase(t)
	up.regionMode.Store(true)
	if err := backuplink.Set(px); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := c.ChatStream(testAuth(), museReq()); err != nil {
		t.Fatal(err)
	}
	if !c.regionLearned("cline-free/muse-spark-1.3-contributor") {
		t.Fatal("应已学到备用路由")
	}

	// 上游解除地区限制（直连可通），但备用地址指向已关闭的端口 ⇒ 必须回落直连
	up.regionMode.Store(false)
	deadSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := deadSrv.URL
	deadSrv.Close() // 立刻关掉 ⇒ 拨号必失败
	if err := backuplink.Set(dead); err != nil {
		t.Fatal(err)
	}

	rc, status, body, err := c.ChatStream(testAuth(), museReq())
	if err != nil {
		t.Fatalf("备用不可达不该冒泡成传输层错误：%v", err)
	}
	if status != http.StatusOK || rc == nil {
		t.Fatalf("应回落直连成功：status=%d body=%s", status, body)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !strings.Contains(string(got), "ok-direct") {
		t.Errorf("内容应来自直连：%s", got)
	}
	if c.regionLearned("cline-free/muse-spark-1.3-contributor") {
		t.Error("备用不可达后必须清除该模型的学习路由")
	}
}
