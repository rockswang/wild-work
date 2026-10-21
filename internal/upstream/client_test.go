package upstream

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{402, ``, ErrHardCredit},
		{400, `{"code":1,"msg":"余额不足"}`, ErrHardCredit},
		{403, `insufficient credits`, ErrHardCredit},
		{200, `{"code":10001,"msg":"积分不足，请充值"}`, ErrHardCredit},
		{400, `{"code":1,"msg":"额度用尽"}`, ErrHardCredit},
		{429, ``, ErrSoftRate},
		// ——— 429/401 分类（本仓与上游 v2.9.0 并集）———
		// 429 + 业务码 14018 = 账号积分耗尽（上游把余额耗尽也走 429 返回）→ 硬冷却弃号。
		{429, `{"code":14018,"msg":"积分耗尽"}`, ErrHardCredit},
		{429, `{"code": 14018, "msg": "credit exhausted"}`, ErrHardCredit},
		// 嵌套形态（codebuddy/workbuddyai 生产实录；本渠道预防性同构特判）：误判软限流
		// 只冷 60s，降级链永不触发。
		{429, `{"error":{"data":{"code":14018,"msg":"Credits exhausted. Please visit the link below to purchase add-on packs and get more credits: https://www.codebuddy.ai/profile/usage ","requestId":"x"}}}`, ErrHardCredit},
		// 匹配防前缀误伤（S2）：code 140180 含子串 "code":14018，须保持软限流；
		// 尾形（code 后紧跟 }，无 msg）是真 14018，须命中。
		{429, `{"error":{"data":{"code":140180,"msg":"future code"}}}`, ErrSoftRate},
		{429, `{"error":{"data":{"code":14018}}}`, ErrHardCredit},
		// 429 无 14018 时仍是软限流：限流 body 高频带 "quota exceeded" 这类跨计费/限流
		// 两界的措辞，靠文案判会把限流误归硬冷却、白扔号约 12h（429 前置的原因）。
		{429, `{"code":10001,"msg":"积分不足"}`, ErrSoftRate},
		{429, `{"code":9999,"msg":"quota exceeded"}`, ErrSoftRate},
		// 回归：6004 模型级限流 / 14003 过频（构造，未实证）均保持软限流。
		{429, `{"code":6004,"msg":"usage exceeds frequency limit ... will reset at ..."}`, ErrSoftRate},
		{429, `{"code":14003,"msg":"too many requests"}`, ErrSoftRate},
		{429, `quota exceeded`, ErrSoftRate},
		{401, `Offline user session not found`, ErrSessionDead},
		{401, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		// 401 统一归登录态失效（#134）：上游拒绝我们的凭据 = 需重新登录
		{401, `{"code":9999,"msg":"bad token"}`, ErrSessionDead},
		// 图片格式/数据无效：请求内容决定，换号结果不变 → 请求级错误（不罚号不轮转）。
		// 第一条的 code 也是 11101，必须判成 ErrImageInvalid 而不是 ErrBadParams。
		{400, `{"code":11101,"msg":"Parse message failed: invalid image_url content at index 2: json: cannot unmarshal string into Go value of type v2.ImageContent"}`, ErrImageInvalid},
		{400, `{"code":11135,"msg":"invalid_image_data"}`, ErrImageInvalid},
		{400, `invalid_image_data`, ErrImageInvalid},
		{400, `{"code": 11135, "msg": "image rejected"}`, ErrImageInvalid},
		{400, `{"code": "11135", "msg": "image rejected"}`, ErrImageInvalid},
		{400, `{"error": {"code": 11135, "message": "image rejected"}}`, ErrImageInvalid},
		{400, `{"code": 11133, "msg": "other business error"}`, ErrClient},
		{400, `{"code": 111350, "msg": "longer code must not hit 11135"}`, ErrClient},
		// 11101 出站 body 畸形：请求级错误（原为 ErrClient，会被 NoteError 罚号）。
		{400, `{"code":11101,"msg":"Unmarshal chat params failed"}`, ErrBadParams},
		{400, `Unmarshal chat params failed`, ErrBadParams},
		// 11115 的 JSON 空白/引号容差（原字面量 marker 只认紧凑形态）。
		{400, `{"code": 11115, "msg": "prompt is too long"}`, ErrPromptTooLong},
		{400, `{"code":"11115"}`, ErrPromptTooLong},
		{404, `{"code": 11115}`, ErrPromptTooLong},
		// 会话头族缺失（11128 conversation id missing）= 上游**按账号灰度**的渠道校验，
		// 归账号级（软冷却 + 换号，见 R116）；同一个 11128 的另一形态（内容指纹
		// unapproved channel）才是请求级问题，归内容拦截不罚号。
		{400, `{"code":11128,"msg":"conversation id missing","requestId":"x"}`, ErrAccountFault},
		{400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, ErrContentBlocked},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{200, ``, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestCodeMarkerTolerance 业务码判定必须容忍 JSON 空白与引号形态，且不得前缀误命中。
//
// 背景：原实现用字面量 strings.Contains(body, `"code":11115`)，上游一旦返回
// `{"code": 11115}`（美化输出）就漏判 → 请求级错误退化成 ErrClient 并被 NoteError 罚号。
func TestCodeMarkerTolerance(t *testing.T) {
	hit := []string{
		`{"code":11135}`,
		`{"code": 11135}`,
		`{"code":"11135"}`,
		`{"code": "11135"}`,
		`{"code": "11135", "msg":"x"}`,
		`{"error":{"code": 11135}}`,
		`{"code":'11135'}`,
		`prefix {"code": 11135} suffix`,
	}
	miss := []string{
		`{"code":111350}`,   // 更长的码，前缀匹配不得命中
		`{"code":"11135a"}`, // 引号内的更长值
		`{"code": 1113}`,    // 更短
		`{"codex": 11135}`,  // 键名不是 code
		`code 11135`,        // 无引号键
		`{"msg":"11135"}`,   // 只出现在 msg 里
		``,                  // 空 body
	}
	for _, s := range hit {
		if !codeMarker(strings.ToLower(s), "11135") {
			t.Errorf("codeMarker(%q) = false, want true", s)
		}
	}
	for _, s := range miss {
		if codeMarker(strings.ToLower(s), "11135") {
			t.Errorf("codeMarker(%q) = true, want false", s)
		}
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:            &http.Client{Transport: fn},
		StreamHTTP:      &http.Client{Transport: fn},
		ChatBaseCN:      "https://chat.example",
		BillingBaseCN:   "https://billing.example",
		ChatBaseGlobal:  "https://gchat.example",
		BillingBaseGlob: "https://gbilling.example",
	}
}

func TestRefreshSuccess(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("X-Refresh-Token") != "oldrt" {
			return nil, errors.New("missing X-Refresh-Token")
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt <= 1 {
		t.Errorf("expiresAt not advanced: %d", a.ExpiresAt)
	}
}

func TestRefreshPreservesExpiryWhenOmitted(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1753600000 {
		t.Errorf("expiresAt should be preserved, got %d", a.ExpiresAt)
	}
	if a.RefreshToken != "rt" {
		t.Errorf("refreshToken should be preserved, got %s", a.RefreshToken)
	}
}

func TestRefreshSessionDead(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 401,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"Offline user session not found"}`)),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("want error")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Kind != ErrSessionDead {
		t.Errorf("kind=%v want ErrSessionDead", ue.Kind)
	}
}

func TestChatStreamSendsHeadersAndStreamTrue(t *testing.T) {
	var gotAuth, gotUID, gotProduct string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		gotProduct = r.Header.Get("X-Product")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	// X-Product 取官方 product.json 的 deploymentType（SaaS），见 R117。
	if gotAuth != "Bearer at" || gotUID != "u1" || gotProduct != "SaaS" {
		t.Errorf("headers: auth=%q uid=%q product=%q", gotAuth, gotUID, gotProduct)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) {
		t.Errorf("stream not forced: %s", gotBody)
	}
}

func TestChatStreamHardCreditError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(402, `{"code":1,"msg":"余额不足"}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`))
	if status != 402 {
		t.Errorf("status=%d", status)
	}
	if err != nil {
		t.Fatalf("hard credit should return body via status, not err: %v", err)
	}
	// caller classifies via returned body
	if Classify(status, string(respBody)) != ErrHardCredit {
		t.Errorf("body=%q not classified hard credit", respBody)
	}
}

func TestUserResourceAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ProductCode":"p_tcaca"`)) {
			return nil, errors.New("missing ProductCode: " + string(body))
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"TotalDosage":3000,"Accounts":[
			{"PackageName":"签到包","CapacitySize":2000,"CapacityRemain":1200,"CapacityUsed":800,"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800},
			{"PackageName":"体验包","CapacitySize":1000,"CapacityRemain":300,"CapacityUsed":700,"CycleCapacitySize":1000,"CycleCapacityRemain":300,"CycleCapacityUsed":700}
		]}}}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
}

func TestUserResourceNegativeClamped(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":-50,"CycleCapacityUsed":150}
		]}}}}`), nil
	})
	remain, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 0 {
		t.Errorf("remain=%d err=%v, want 0 (clamped)", remain, err)
	}
}

func TestDailyCheckinAlready(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !strings.Contains(err.Error(), "已签到") {
		t.Errorf("err=%v", err)
	}
}

func TestRegionBases(t *testing.T) {
	c := testClient(nil)
	cn := &auth.Auth{Domain: ""}
	gl := &auth.Auth{Domain: "www.workbuddy.ai"}
	if c.chatBase(cn) != "https://chat.example" || c.billingBase(cn) != "https://billing.example" {
		t.Error("cn bases wrong")
	}
	if c.chatBase(gl) != "https://gchat.example" || c.billingBase(gl) != "https://gbilling.example" {
		t.Error("global bases wrong")
	}
}

// TestStreamClientHasNoTotalTimeout 守门：流式必须走**无总超时**的 client。
//
// 背景：http.Client.Timeout 是整请求上限，计时器在 Do() 返回后继续跑直到 body 读完；
// SSE 整个生成期都在读 body，故长思考请求会被从流中间掐断（实测 120.00s =
// config.upstream.timeout_seconds）。非流式 client 必须保留总超时（短请求的合理兜底）。
func TestStreamClientHasNoTotalTimeout(t *testing.T) {
	c := New()
	if c.StreamHTTP == nil {
		t.Fatal("StreamHTTP 必须存在")
	}
	if c.StreamHTTP.Timeout != 0 {
		t.Fatalf("StreamHTTP.Timeout = %v，必须为 0（流式不能有整请求上限）", c.StreamHTTP.Timeout)
	}
	if c.HTTP == nil || c.HTTP.Timeout <= 0 {
		t.Fatalf("非流式 HTTP.Timeout = %v，必须 > 0", c.HTTP.Timeout)
	}
	// 无总超时后必须靠 ResponseHeaderTimeout 兜底，否则连响应头都等不到会无限挂住。
	tr, ok := c.StreamHTTP.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("StreamHTTP.Transport = %T，应为 *http.Transport", c.StreamHTTP.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Fatal("StreamHTTP 必须有 ResponseHeaderTimeout 兜底")
	}
	if tr.TLSNextProto == nil {
		t.Fatal("应强制 HTTP/1.1（禁 h2）")
	}
}

// TestIdleTimeoutDefaults 未注入配置时回落默认值（装配漏注入不应退化成「无兜底」）。
func TestIdleTimeoutDefaults(t *testing.T) {
	c := New()
	if got := c.idleTimeout(); got != DefaultIdleTimeout {
		t.Fatalf("idleTimeout() = %v, want %v", got, DefaultIdleTimeout)
	}
	c.IdleTimeout = 33 * time.Second
	if got := c.idleTimeout(); got != 33*time.Second {
		t.Fatalf("idleTimeout() = %v, want 33s", got)
	}
}

// TestChatStreamUsesStreamClientAndIdleReader 守门：对话流必须走 StreamHTTP 且包看门狗。
func TestChatStreamUsesStreamClientAndIdleReader(t *testing.T) {
	httpCalled := false
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
			httpCalled = true
			return nil, errors.New("对话流不得走带总超时的 HTTP client")
		})},
		StreamHTTP: &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		})},
		ChatBaseCN: "https://chat.example",
	}
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()
	if httpCalled {
		t.Fatal("ChatStream 走了 HTTP（有总超时）而不是 StreamHTTP")
	}
	if _, ok := rc.(*provider.IdleReader); !ok {
		t.Fatalf("ChatStream 返回体未包 IdleReader（got %T），空闲卡死将无法兜底", rc)
	}
}
