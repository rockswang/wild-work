//go:build live

// live_test.go 对真实 api.cline.bot 验证 cline 渠道端到端可用。
//
// mock 测试只能证明「实现自洽」；这里证明「我们的请求真的被 Cline 接受、
// 且四个承重假设在真线上仍然成立」：
//  1. sk_ Key 可直接对话（无需 OAuth、无需本机客户端）
//  2. `cline-free/*` 别名**必须**带非空 X-CLIENT-TYPE，去掉即 403
//  3. 免费模型逐请求 cost=0
//  4. 上游把限流压平成 HTTP 500 ⇒ 我们的 effectiveStatus 能还原内层码
//
// 默认不跑。需要真 Key（只经环境变量传入，绝不落盘/绝不打印）：
//
//	CLINE_KEY=sk_... go test -tags live ./internal/cline/ -v
package cline

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
)

func liveKey(t *testing.T) string {
	t.Helper()
	k := strings.TrimSpace(os.Getenv("CLINE_KEY"))
	if k == "" {
		t.Skip("未设 CLINE_KEY，跳过实盘验证")
	}
	return k
}

// liveProxy 实盘代理地址（api.cline.bot 直连在部分地区被地域拦截）。
// 可用 CLINE_PROXY 覆盖；设成 "direct" 即强制直连。
func liveProxy() string {
	if v := strings.TrimSpace(os.Getenv("CLINE_PROXY")); v != "" {
		return v
	}
	return "http://127.0.0.1:10809"
}

// liveClient 造一个带代理的客户端。
// 代理注入等价于生产路径的 SetTransportProxy（那个助手在 cmd/wild-work 包内，渠道包不可引）。
func liveClient() *Client {
	c := New()
	tr, _ := c.HTTP.Transport.(*http.Transport)
	if tr != nil && liveProxy() != "direct" {
		if u, err := url.Parse(liveProxy()); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	c.StreamHTTP.Transport = tr // 共用 Transport 复用连接池，但不设总超时（R35）
	return c
}

func TestLiveValidateKey(t *testing.T) {
	key := liveKey(t)
	res, err := liveClient().probeKey(key)
	if err != nil {
		t.Fatalf("真实 Key 校验失败：%v", err)
	}
	if !strings.HasPrefix(res.UID, "apk-") {
		t.Errorf("uid 应是官方 apk- id，实得 %q", res.UID)
	}
	t.Logf("uid=%s name=%s prefix=%s 同账号兄弟Key=%d", res.UID, res.AccountName, res.KeyPrefix, len(res.SiblingKeys))
}

func TestLiveCatalog(t *testing.T) {
	key := liveKey(t)
	c := liveClient()
	a := &auth.Auth{Kind: "cline", UID: "live", AccessToken: key, ExpiresAt: FarFutureExpiry()}
	models, err := c.FetchModels(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) == 0 {
		t.Fatal("暴露集为空")
	}
	for _, m := range models {
		// 暴露集必须逐字来自 free[]：ID 带 cline-free/ 前缀、非订阅池、非 OpenRouter 长尾
		if !strings.HasPrefix(m.ID, freePrefix) {
			t.Errorf("暴露集混入了 free[] 之外的模型：%s", m.ID)
		}
		if m.Name == "" {
			t.Errorf("%s 缺展示名（上游 name 缺失时应已兑底 prettifyID）", m.ID)
		}
		t.Logf("暴露 %s (%s)", m.ID, m.Name)
	}
	// 校验集 == 暴露集：透传出来的都能过本地校验，free[] 之外的一概拒
	for _, m := range models {
		if !c.knownModel(m.ID) {
			t.Errorf("暴露的 %s 未通过本地校验（集合应一致）", m.ID)
		}
	}
	if c.knownModel("vendor/not-in-free") {
		t.Error("free[] 之外的模型不应通过本地校验")
	}
}

// TestLiveChatRealModel 真打一次对话：必须拿到非空内容，且记录 cost 与路由。
func TestLiveChatRealModel(t *testing.T) {
	key := liveKey(t)
	c := liveClient()
	a := &auth.Auth{Kind: "cline", UID: "live", AccessToken: key, ExpiresAt: FarFutureExpiry()}
	body, _ := json.Marshal(map[string]any{
		"model":    "cline-free/mimo-v2.6-flash",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "Reply with exactly: OK"}},
	})
	rc, status, raw, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("请求失败：%v", err)
	}
	if status != 200 {
		t.Fatalf("应 200，实得 %d body=%.200s", status, raw)
	}
	defer rc.Close()
	_ = rc

	// 非流式对照：读完整帧序列，验证 cost 与路由前缀
	body2, _ := json.Marshal(map[string]any{
		"model":    "cline-free/mimo-v2.6-flash",
		"stream":   false,
		"messages": []map[string]string{{"role": "user", "content": "What is 17*8? Reply only the number."}},
	})
	req, _ := http.NewRequest(http.MethodPost, c.base()+EpChat, strings.NewReader(string(body2)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderClientType, HeaderClientTypeValue)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var d struct {
		Data struct {
			ID      string `json:"id"`
			Model   string `json:"model"`
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
			Usage struct {
				Cost       float64 `json:"cost"`
				CostDetail struct {
					Upstream float64 `json:"upstream_inference_cost"`
				} `json:"cost_details"`
			} `json:"usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatalf("响应解析失败：%v\n%.300s", err, out)
	}
	got := ""
	if len(d.Data.Choices) > 0 {
		got = d.Data.Choices[0].Message.Content
	}
	t.Logf("内容=%q model=%s 路由=%s cost=%v 上游成本=%v",
		got, d.Data.Model, RouteOf(d.Data.ID), d.Data.Usage.Cost, d.Data.Usage.CostDetail.Upstream)
	if !strings.Contains(got, "136") {
		t.Errorf("未拿到预期答案：%.120q", got)
	}
	// cost 逐请求抽签（实测同 ID 有 0 也有非 0），只断言量级极小，不断言为 0
	if d.Data.Usage.Cost > 0.01 {
		t.Errorf("cost 异常偏大：%v（本渠道定位是免费池）", d.Data.Usage.Cost)
	}
}

// TestLiveAliasNeedsClientType 锁住「别名必须带头」：直接删头应被 403。
// 这是那条「删掉就全渠道免费模型静默消失」的实盘证据。
func TestLiveAliasNeedsClientType(t *testing.T) {
	key := liveKey(t)
	c := liveClient()
	body, _ := json.Marshal(map[string]any{
		"model":    "cline-free/step-5-preview",
		"stream":   false,
		"messages": []map[string]string{{"role": "user", "content": "say ok"}},
	})
	send := func(hdr map[string]string) int {
		req, _ := http.NewRequest(http.MethodPost, c.base()+EpChat, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := c.httpClient().Do(req)
		if err != nil {
			return -1
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return resp.StatusCode
	}
	with := send(map[string]string{HeaderClientType: HeaderClientTypeValue})
	without := send(nil)
	empty := send(map[string]string{HeaderClientType: ""})
	t.Logf("带头=%d 不带头=%d 空值=%d", with, without, empty)
	if with != 200 {
		t.Errorf("带头应 200，实得 %d", with)
	}
	if without == 200 || empty == 200 {
		t.Error("别名在不带/空值 X-CLIENT-TYPE 下竟然成功了 —— 头判据可能已变，需重估")
	}
}

// TestLiveFlattenedStatusIsUnflattened 验证压平 500 的还原逻辑在真线上有意义。
// 手段：故意发一个会触发上游 400 的极小 max_tokens（实测 8 → 500 内层 400）。
func TestLiveFlattenedStatusIsUnflattened(t *testing.T) {
	key := liveKey(t)
	c := liveClient()
	body, _ := json.Marshal(map[string]any{
		"model":      "cohere/north-mini-code:free",
		"stream":     false,
		"max_tokens": 8,
		"messages":   []map[string]string{{"role": "user", "content": "say ok"}},
	})
	// 绕过本端 clamp（clamp 会把 8 抬到 64），直接裸打上游拿原始形态
	req, _ := http.NewRequest(http.MethodPost, c.base()+EpChat, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	inner := InnerStatus(string(raw))
	t.Logf("外层=%d 内层=%d body=%.200s", resp.StatusCode, inner, raw)
	if resp.StatusCode >= 500 && inner != 0 && inner != resp.StatusCode {
		t.Logf("确认压平形态存在：effectiveStatus 会把它还原成 %d", effectiveStatus(resp.StatusCode, string(raw)))
	}
}

// TestLiveIdleTimeoutNotKillingLongStream 长流不应被 StreamHTTP 的总超时掐断（R35 实盘）。
func TestLiveIdleTimeoutNotKillingLongStream(t *testing.T) {
	key := liveKey(t)
	c := liveClient()
	c.HTTP.Timeout = 3 * time.Second // 故意短：若流式误用 HTTP 必被掐断
	a := &auth.Auth{Kind: "cline", UID: "live", AccessToken: key, ExpiresAt: FarFutureExpiry()}
	body, _ := json.Marshal(map[string]any{
		"model":    "stepfun/step-5-preview",
		"stream":   true,
		"messages": []map[string]string{{"role": "user", "content": "List 40 one-line facts about the ocean."}},
	})
	rc, status, _, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("流式请求失败（可能被总超时掐断）：%v", err)
	}
	if status != 200 {
		t.Fatalf("应 200，实得 %d", status)
	}
	defer rc.Close()
	n, err := io.Copy(io.Discard, io.LimitReader(rc, 8<<20))
	t.Logf("读完 %d 字节 err=%v（耗时应远超 HTTP.Timeout）", n, err)
	if n < 2000 {
		t.Errorf("流被过早切断，只读到 %d 字节", n)
	}
}
