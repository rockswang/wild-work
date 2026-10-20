// lobsterai_test.go LobsterAI 渠道的回归测试。
//
// 纪律：**零真实网络请求**——每个用例都用 httptest 起假上游，并把 Client.Base 与
// Client.Version 同时指到它（ClientVersion 会去打版本号接口，漏掉就会真的出网）。
package lobsterai

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// 测试脚手架
// ---------------------------------------------------------------------------

// upstreamRec 记录假上游收到的每次请求（顺序、方法、路径、query、头、体）。
type upstreamRec struct {
	mu      sync.Mutex
	methods []string
	paths   []string
	queries []url.Values
	headers []http.Header
	bodies  [][]byte
}

func (u *upstreamRec) add(r *http.Request, body []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.methods = append(u.methods, r.Method)
	u.paths = append(u.paths, r.URL.Path)
	u.queries = append(u.queries, r.URL.Query())
	u.headers = append(u.headers, r.Header.Clone())
	u.bodies = append(u.bodies, body)
}

// apiPaths 返回除版本号接口（路径 "/"）之外的请求路径，便于断言调用顺序。
func (u *upstreamRec) apiPaths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]string, 0, len(u.paths))
	for _, p := range u.paths {
		if p == "/" {
			continue
		}
		out = append(out, p)
	}
	return out
}

// last 返回最近一次请求的快照（方法、路径、query、头、体）。
func (u *upstreamRec) last() (string, string, url.Values, http.Header, []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := len(u.paths)
	if n == 0 {
		return "", "", nil, nil, nil
	}
	n--
	return u.methods[n], u.paths[n], u.queries[n], u.headers[n], u.bodies[n]
}

// byPath 返回指定路径上最近一次请求的 (query, 头, 体)。
func (u *upstreamRec) byPath(path string) (url.Values, http.Header, []byte) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := len(u.paths) - 1; i >= 0; i-- {
		if u.paths[i] == path {
			return u.queries[i], u.headers[i], u.bodies[i]
		}
	}
	return nil, nil, nil
}

// count 返回除版本号接口之外的请求次数。
func (u *upstreamRec) count() int { return len(u.apiPaths()) }

// newUpstream 起一个假上游：路径 "/" 当版本号接口，其余交给 api 处理函数。
func newUpstream(t *testing.T, api http.HandlerFunc) (*Client, *upstreamRec) {
	t.Helper()
	rec := &upstreamRec{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rec.add(r, body)
		// 还原 body，让 api 处理函数仍能读（记录与处理共享一次读取）。
		r.Body = io.NopCloser(bytes.NewReader(body))
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":{"value":{"version":"2026.9.4"}}}`)
			return
		}
		api(w, r)
	}))
	t.Cleanup(srv.Close)
	clearVersionCache()
	c := NewWithBase(srv.URL)
	c.Version = srv.URL // 版本号也走假上游：测试零真实请求
	return c, rec
}

// clearVersionCache 清空进程内版本号缓存（用例之间互不污染）。
func clearVersionCache() {
	versionCache.mu.Lock()
	versionCache.value = ""
	versionCache.expiry = time.Time{}
	versionCache.mu.Unlock()
}

// testAuth 造一个带完整 keyfrom 四元组的假账号（全部是假值，非真实凭据）。
func testAuth() *auth.Auth {
	return &auth.Auth{
		Kind:         string(Kind),
		AccessToken:  "fake-access-token",
		RefreshToken: "fake-refresh-token",
		UID:          "uid-1",
		Nickname:     "测试账号",
		Extra: map[string]string{
			ExtraUUID:          "11111111-2222-3333-4444-555555555555",
			ExtraFirstKeyfrom:  "1700000000000",
			ExtraLatestKeyfrom: "1700000000001",
			ExtraUserID:        "yid-1",
		},
	}
}

func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

// ---------------------------------------------------------------------------
// ChatStream：请求头、body 改写、返回约定
// ---------------------------------------------------------------------------

func TestChatStreamRequestShape(t *testing.T) {
	c, rec := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"raw\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	body := []byte(`{"model":"kimi-k2.5","messages":[{"role":"user","content":"hi"}],` +
		`"stream":false,"tools":[],"tool_choice":"auto","prompt_cache_key":"k1","reasoning_effort":"high"}`)
	rc, status, respBody, err := c.ChatStream(testAuth(), body)
	if err != nil || status != 200 || respBody != nil {
		t.Fatalf("ChatStream = (%v,%d,%s,%v)，期望 200 且无错误体", rc, status, respBody, err)
	}
	defer rc.Close()

	_, path, _, h, gotBody := rec.last()
	if path != EpChatCompletions {
		t.Fatalf("对话路径 = %q，期望 %q", path, EpChatCompletions)
	}
	if got := h.Get("Authorization"); got != "Bearer fake-access-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := h.Get("Accept"); got != "text/event-stream, application/json" {
		t.Errorf("Accept = %q", got)
	}
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := h.Get("User-Agent"); got != UserAgent {
		t.Errorf("User-Agent = %q，期望 %q", got, UserAgent)
	}
	// 两个 X-LobsterAI-Client-* 头必需（缺 capabilities 会少模型、且 reasoning_effort=off 直接 500）。
	if got := h.Get(HeaderCapabilities); got != Capabilities {
		t.Errorf("%s = %q，期望 %q", HeaderCapabilities, got, Capabilities)
	}
	if got := h.Get(HeaderClientVersion); got != "2026.9.4" {
		t.Errorf("%s = %q，期望动态版本号", HeaderClientVersion, got)
	}

	var out map[string]any
	if err := json.Unmarshal(gotBody, &out); err != nil {
		t.Fatalf("请求体不是 JSON：%v（%s）", err, gotBody)
	}
	if out["stream"] != true {
		t.Errorf("stream = %v，必须恒为 true（上游 stream:false 直接 500）", out["stream"])
	}
	if _, has := out["tools"]; has {
		t.Errorf("空 tools 不应下发：%v", out["tools"])
	}
	if _, has := out["tool_choice"]; has {
		t.Errorf("tool_choice 不应下发（协议 §4）")
	}
	if _, has := out["prompt_cache_key"]; has {
		t.Errorf("prompt_cache_key 不应下发（协议 §4）")
	}
	// reasoning_effort 仅「调用方显式传入才透传」：有值时必须原样保留，不补档也不改写。
	if out["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort = %v，期望原样透传 high", out["reasoning_effort"])
	}
}

func TestChatStreamDoesNotInventReasoningEffort(t *testing.T) {
	c, rec := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	rc, _, _, err := c.ChatStream(testAuth(), []byte(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	_, _, _, _, gotBody := rec.last()
	var out map[string]any
	_ = json.Unmarshal(gotBody, &out)
	if _, has := out["reasoning_effort"]; has {
		t.Errorf("调用方未传 reasoning_effort 时**不得主动补档**：%v", out["reasoning_effort"])
	}
}

func TestChatStreamHTTPErrorPassthrough(t *testing.T) {
	const upstreamErr = `{"code":402,"msg":"免费额度已用完，请升级套餐"}`
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusPaymentRequired, upstreamErr)
	})
	rc, status, respBody, err := c.ChatStream(testAuth(), []byte(`{"model":"m","messages":[]}`))
	if err != nil || rc != nil {
		t.Fatalf("ChatStream = (%v,%d,%s,%v)", rc, status, respBody, err)
	}
	if status != http.StatusPaymentRequired {
		t.Fatalf("status = %d，期望原样返回 402", status)
	}
	if string(respBody) != upstreamErr {
		t.Fatalf("错误体被改写了：%s", respBody)
	}
}

func TestChatStreamLocalBadBodyIs400NotTransportError(t *testing.T) {
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("本地拒绝时不得请求上游")
	})
	rc, status, respBody, err := c.ChatStream(testAuth(), []byte(`{not json`))
	if err != nil || rc != nil {
		t.Fatalf("非法请求体应返回本地 400 而不是传输层错误：(%v,%d,%s,%v)", rc, status, respBody, err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d，期望 400", status)
	}
	if k := Classify(status, string(respBody)); k != provider.ErrBadParams {
		t.Fatalf("本地 400 的分类 = %v，期望 ErrBadParams（不罚号不轮转）", k)
	}
}

// ---------------------------------------------------------------------------
// keyfrom query（模型列表）与目录两种信封
// ---------------------------------------------------------------------------

func TestFetchModelsKeyfromQueryAndHeaders(t *testing.T) {
	c, rec := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"code":0,"data":[{"modelId":"kimi-k2.5"}]}`)
	})
	a := testAuth()
	if _, err := c.FetchModels(a); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	_, path, q, h, _ := rec.last()
	if path != EpModelsAvailable {
		t.Fatalf("目录路径 = %q", path)
	}
	// keyfrom 身份载荷：驼峰键名，空值键不发送，**不含 refreshToken**。
	want := map[string]string{
		"firstKeyfrom":  "1700000000000",
		"latestKeyfrom": "1700000000001",
		"version":       "2026.9.4",
		"uuid":          "11111111-2222-3333-4444-555555555555",
		"userId":        "yid-1",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("query %s = %q，期望 %q", k, got, v)
		}
	}
	if q.Has("refreshToken") {
		t.Errorf("模型列表 query **不得**带 refreshToken（协议 §1.3）")
	}
	if got := q.Get("version"); got == "" {
		t.Errorf("version 必须始终发送")
	}
	if h.Get(HeaderCapabilities) != Capabilities || h.Get(HeaderClientVersion) == "" {
		t.Errorf("模型列表的两个 X-LobsterAI-Client-* 头必需：%v", h)
	}
	if h.Get("Accept") != "application/json" {
		t.Errorf("目录 Accept = %q", h.Get("Accept"))
	}

	// 空值键不发送：只留 access_token 的账号，uuid/userId/firstKeyfrom 都不该出现。
	c2, rec3 := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"code":0,"data":[{"modelId":"a"}]}`)
	})
	bare := &auth.Auth{Kind: string(Kind), AccessToken: "fake-access-token", UID: "uid-2"}
	if _, err := c2.FetchModels(bare); err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	_, _, q2, _, _ := rec3.last()
	for _, k := range []string{"firstKeyfrom", "latestKeyfrom", "uuid", "userId"} {
		if q2.Has(k) {
			t.Errorf("空值键 %s 不应发送（值为 %q）", k, q2.Get(k))
		}
	}
}

func TestFetchModelsEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"单层 data 数组", `{"code":0,"data":[{"modelId":"a"},{"modelId":"b"}]}`, 2},
		{"双层 data.data 数组", `{"code":0,"data":{"data":[{"modelId":"a"},{"modelId":"b"},{"modelId":"c"}]}}`, 3},
		{"空数组 → 兜底", `{"code":0,"data":[]}`, len(staticModelIDs)},
		{"业务码非 0 → 兜底", `{"code":500,"msg":"boom","data":[]}`, len(staticModelIDs)},
		{"形状不符 → 兜底", `{"code":0,"data":{"foo":1}}`, len(staticModelIDs)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, 200, tc.body)
			})
			got, err := c.FetchModels(testAuth())
			if err != nil {
				t.Fatalf("FetchModels 不应报错（目录失败走兜底）：%v", err)
			}
			if len(got) != tc.want {
				t.Fatalf("模型数 = %d，期望 %d", len(got), tc.want)
			}
		})
	}
}

func TestFetchModelsNoAccountReturnsEmpty(t *testing.T) {
	c, rec := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("无凭据时不得请求上游")
	})
	for _, a := range []*auth.Auth{nil, {Kind: string(Kind)}} {
		got, err := c.FetchModels(a)
		if err != nil {
			t.Fatalf("目录门控：无账号必须返回 [] 且**不抛错**，实得 %v", err)
		}
		if got == nil || len(got) != 0 {
			t.Fatalf("目录门控：期望空表，实得 %v", got)
		}
	}
	if rec.count() != 0 {
		t.Fatalf("目录门控不应产生上游请求")
	}
}

func TestStaticModelsEstimated(t *testing.T) {
	got := StaticModels()
	if len(got) != 19 {
		t.Fatalf("静态兜底表 = %d 条，期望 19 条", len(got))
	}
	for _, m := range got {
		if m.ContextWindow != 131072 {
			t.Errorf("%s 的 contextWindow = %d，兜底表统一 131072", m.ID, m.ContextWindow)
		}
		if m.ContextFromAPI {
			t.Errorf("%s：兜底的 131072 是**本地估计值**，ContextFromAPI 必须为 false", m.ID)
		}
		if m.ID == "" || m.Name == "" {
			t.Errorf("兜底条目缺少 id/name：%+v", m)
		}
	}
}

func TestModelOptionalFieldsNotFaked(t *testing.T) {
	// 只给 modelId：其余字段必须留未知（0 / false / nil），**绝不填 0/false 假值**。
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"code":0,"data":[{"modelId":"bare"}]}`)
	})
	models, err := c.FetchModels(testAuth())
	if err != nil || len(models) != 1 {
		t.Fatalf("FetchModels = (%v,%v)", models, err)
	}
	m := models[0]
	if m.ID != "bare" {
		t.Fatalf("ID = %q", m.ID)
	}
	if m.ContextWindow != 0 || m.MaxTokens != 0 {
		t.Errorf("缺失的容量字段不得填假值：ctx=%d max=%d", m.ContextWindow, m.MaxTokens)
	}
	if m.ContextFromAPI {
		t.Errorf("容量字段缺失时 ContextFromAPI 必须为 false")
	}
	if m.SupportsImages || m.SupportsReasoning {
		t.Errorf("能力字段缺失时不得置 true：img=%v think=%v", m.SupportsImages, m.SupportsReasoning)
	}
	if m.ReasoningEfforts != nil || m.DefaultReasoningEffort != "" || m.CanDisableThinking != nil {
		t.Errorf("没有 thinkingConfig 就不得声明档位：%v/%q/%v",
			m.ReasoningEfforts, m.DefaultReasoningEffort, m.CanDisableThinking)
	}

	// 倍率缺失 → 不产出定价条目（未知 ≠ 免费）。
	pricing, err := c.FetchModelPricing(testAuth())
	if err != nil {
		t.Fatalf("FetchModelPricing: %v", err)
	}
	if len(pricing) != 0 {
		t.Fatalf("无 costMultiplier 时不得产出定价条目：%+v", pricing)
	}
}

func TestModelFullMapping(t *testing.T) {
	body := `{"code":0,"data":[{
		"modelId":"glm-5.2","modelName":"GLM-5.2","contextWindow":200000,"maxTokens":8192,
		"supportsImage":true,"supportsThinking":true,"costMultiplier":0.05,
		"thinkingConfig":{"options":[
			{"level":"off","openclawLevel":"off"},
			{"level":"low","openclawLevel":"low"},
			{"level":"max","openclawLevel":"xhigh"}
		],"defaultLevel":"max"}
	}]}`
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, body)
	})
	models, err := c.FetchModels(testAuth())
	if err != nil || len(models) != 1 {
		t.Fatalf("FetchModels = (%v,%v)", models, err)
	}
	m := models[0]
	if m.ContextWindow != 200000 || m.MaxTokens != 8192 || !m.ContextFromAPI {
		t.Errorf("容量映射错误：%+v", m)
	}
	if !m.SupportsImages || !m.SupportsReasoning {
		t.Errorf("能力位映射错误：%+v", m)
	}
	// 档位表必须是 **wire 值**（没有 max），且 off 不进档位表（它是「可关闭」开关）。
	if len(m.ReasoningEfforts) != 2 || m.ReasoningEfforts[0] != "low" || m.ReasoningEfforts[1] != "xhigh" {
		t.Fatalf("档位表 = %v，期望 [low xhigh]（max→xhigh、off 不入表）", m.ReasoningEfforts)
	}
	if m.DefaultReasoningEffort != "xhigh" {
		t.Errorf("默认档位 = %q，期望 xhigh（max 的 wire 值）", m.DefaultReasoningEffort)
	}
	if m.CanDisableThinking == nil || !*m.CanDisableThinking {
		t.Errorf("options 含 off ⇒ 可关闭思考：%v", m.CanDisableThinking)
	}
	// 展示名带 `· x{倍率}`（协议 §6：写进 name）。
	if m.Name != "GLM-5.2 · x0.05" {
		t.Errorf("展示名 = %q，期望 `GLM-5.2 · x0.05`", m.Name)
	}
	pricing, err := c.FetchModelPricing(testAuth())
	if err != nil || len(pricing) != 1 {
		t.Fatalf("FetchModelPricing = (%+v,%v)", pricing, err)
	}
	if pricing[0].Rate != 0.05 || pricing[0].Model != "glm-5.2" || !pricing[0].IsExplicit() {
		t.Errorf("定价条目错误：%+v", pricing[0])
	}
}

func TestThinkingConfigOnlyOffMeansNoLevels(t *testing.T) {
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"code":0,"data":[{"modelId":"m","supportsThinking":true,`+
			`"thinkingConfig":{"options":[{"level":"off","openclawLevel":"off"}]}}]}`)
	})
	models, _ := c.FetchModels(testAuth())
	if len(models) != 1 {
		t.Fatalf("模型数 = %d", len(models))
	}
	m := models[0]
	if m.ReasoningEfforts != nil {
		t.Errorf("「只有 off 一档」视为无档位，实得 %v", m.ReasoningEfforts)
	}
	if m.CanDisableThinking == nil || !*m.CanDisableThinking {
		t.Errorf("off 存在 ⇒ 可关闭思考：%v", m.CanDisableThinking)
	}
}

func TestThinkingConfigRejectsBadOptions(t *testing.T) {
	// 非法产品侧名 / 非法 wire 值 / off 一致性冲突 / 重复档位：逐档丢弃，不拖垮整表。
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"code":0,"data":[{"modelId":"m","thinkingConfig":{"options":[
			{"level":"nonsense","openclawLevel":"low"},
			{"level":"high","openclawLevel":"nonsense"},
			{"level":"off","openclawLevel":"high"},
			{"level":"low","openclawLevel":"low"},
			{"level":"low","openclawLevel":"low"}
		],"defaultLevel":"medium"}}]}`)
	})
	models, _ := c.FetchModels(testAuth())
	if len(models) != 1 {
		t.Fatalf("模型数 = %d", len(models))
	}
	m := models[0]
	if len(m.ReasoningEfforts) != 1 || m.ReasoningEfforts[0] != "low" {
		t.Fatalf("只应留下一个合法档位 low：%v", m.ReasoningEfforts)
	}
	if m.DefaultReasoningEffort != "" {
		t.Fatalf("defaultLevel 未落在 options 内时必须丢弃，实得 %q", m.DefaultReasoningEffort)
	}
}

// ---------------------------------------------------------------------------
// 版本号：缓存与失败兜底
// ---------------------------------------------------------------------------

func TestClientVersionCacheAndFallback(t *testing.T) {
	var mu sync.Mutex
	hits, fail := 0, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		bad := fail
		mu.Unlock()
		if bad {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if r.Header.Get("Authorization") != "" {
			t.Errorf("版本号查询不得带 Authorization")
		}
		writeJSON(w, 200, `{"data":{"value":{"version":"2026.10.1"}}}`)
	}))
	defer srv.Close()
	clearVersionCache()
	c := NewWithBase(srv.URL)
	c.Version = srv.URL

	if got := c.ClientVersion(); got != "2026.10.1" {
		t.Fatalf("ClientVersion = %q", got)
	}
	if got := c.ClientVersion(); got != "2026.10.1" {
		t.Fatalf("第二次 ClientVersion = %q", got)
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got != 1 {
		t.Fatalf("版本号查询次数 = %d，期望 1（命中 12h 缓存）", got)
	}

	// 格式非法的版本号 → 兜底常量，且**不缓存兜底值**。
	clearVersionCache()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"data":{"value":{"version":"latest"}}}`)
	}))
	defer bad.Close()
	c2 := NewWithBase(bad.URL)
	c2.Version = bad.URL
	if got := c2.ClientVersion(); got != FallbackClientVersion {
		t.Fatalf("非法版本号应回退 %q，实得 %q", FallbackClientVersion, got)
	}
	if v := cachedClientVersion(); v != "" {
		t.Fatalf("失败不得缓存兜底值，缓存里却有 %q", v)
	}

	// 失败后恢复：下一次调用应重新取到真值（证明确实没缓存兜底值）。
	mu.Lock()
	fail = true
	mu.Unlock()
	clearVersionCache()
	if got := c.ClientVersion(); got != FallbackClientVersion {
		t.Fatalf("HTTP 500 应回退兜底值，实得 %q", got)
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if got := c.ClientVersion(); got != "2026.10.1" {
		t.Fatalf("失败后恢复应重新取真值，实得 %q（说明兜底值被缓存了）", got)
	}
}

func TestParseClientVersion(t *testing.T) {
	if _, err := parseClientVersion([]byte(`{"data":{"value":{"version":"1.2.3-beta.1"}}}`)); err != nil {
		t.Errorf("预发布后缀应被接受：%v", err)
	}
	if _, err := parseClientVersion([]byte(`{"data":{}}`)); err == nil {
		t.Errorf("缺 data.value.version 应报错")
	}
	if _, err := parseClientVersion([]byte(`{"data":{"value":{"version":"2026.9.4 "}}}`)); err != nil {
		t.Errorf("首尾空白应被 trim 后接受：%v", err)
	}
}

// ---------------------------------------------------------------------------
// 积分：总额、明细、空判据
// ---------------------------------------------------------------------------

func TestCreditSummary(t *testing.T) {
	body := `{"code":0,"msg":"ok","data":{
		"totalCreditsRemaining":5297.72,
		"creditItems":[
			{"creditsRemaining":"100.00","type":"daily_login","label":"每日登录奖励","expiresAt":"2026-10-31T23:59:59+08:00"},
			{"creditsRemaining":50,"type":"daily_login","label":"每日登录奖励","expiresAt":"2026-11-30T23:59:59+08:00"},
			{"creditsRemaining":"300","type":"new_user","expiresAt":"2026-12-31T00:00:00Z"}
		]}}`
	c, rec := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, body)
	})
	total, items, err := c.UserResourceDetail(testAuth())
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if total != 5297 {
		t.Errorf("总额 = %d，期望 5297（小数截断）", total)
	}
	if len(items) != 3 {
		t.Fatalf("明细条数 = %d，期望 3", len(items))
	}
	if items[0].Name != "每日登录奖励" {
		t.Errorf("包名应优先 label：%q", items[0].Name)
	}
	// 面值推断：同组（同 label）取最大剩余量 100 当面值。
	if items[0].Total != 100 || items[1].Total != 100 {
		t.Errorf("同组面值应为组内最大剩余量 100：%+v", items)
	}
	if items[0].Used != 0 || items[1].Used != 50 {
		t.Errorf("Used = 面值 − 剩余量：%+v", items)
	}
	if items[2].Name != "new_user" {
		t.Errorf("无 label 应回退 type：%q", items[2].Name)
	}
	if !strings.HasPrefix(items[0].ExpireAt, "2026-10-31T") || !strings.HasPrefix(items[2].ExpireAt, "2026-12-31T") {
		t.Errorf("到期时刻未保留时分秒（RFC3339/UTC+8）：%q / %q", items[0].ExpireAt, items[2].ExpireAt)
	}
	if !items[0].Usable {
		t.Errorf("积分包必须标 Usable=true")
	}
	// 只发 4 个基础头：不得带客户端能力头（协议 §2.1）。
	_, _, _, h, _ := rec.last()
	if h.Get(HeaderCapabilities) != "" || h.Get(HeaderClientVersion) != "" {
		t.Errorf("积分接口不得带 X-LobsterAI-Client-* 头：%v", h)
	}
}

func TestCreditSummaryEdgeCases(t *testing.T) {
	// 负数 clamp 到 0；明细存在 → 是「余额 0」而不是「查不到」。
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"code":0,"data":{"totalCreditsRemaining":-12,`+
			`"creditItems":[{"creditsRemaining":0,"type":"t","label":"包"}]}}`)
	})
	total, items, err := c.UserResourceDetail(testAuth())
	if err != nil || total != 0 || len(items) != 1 {
		t.Fatalf("负数总额应 clamp 到 0 且保留明细：(%d,%v,%v)", total, items, err)
	}

	// 总额 0 且无明细 → 「查不到」，必须与「余额确实为 0」区分：返回类型化错误。
	c2, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"code":0,"data":{"totalCreditsRemaining":0,"creditItems":[]}}`)
	})
	_, _, err = c2.UserResourceDetail(testAuth())
	if err == nil {
		t.Fatalf("「总额 0 且无明细」必须能区分于「余额为 0」")
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrPassthrough {
		t.Fatalf("空判据应为不罚号的类型化错误，实得 %v", err)
	}
	// 有明细时**不得**报错（哪怕总额为 0）。
	c3, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"code":0,"data":{"totalCreditsRemaining":0,"creditItems":[{"creditsRemaining":0,"label":"已用完"}]}}`)
	})
	if _, items, err := c3.UserResourceDetail(testAuth()); err != nil || len(items) != 1 {
		t.Fatalf("明细非空时不应报错：(%v,%v)", items, err)
	}

	// HTTP 401 → ErrSessionDead（不变式 19 的自愈前提）。
	c4, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, `{"code":40100,"msg":"token rejected"}`)
	})
	_, _, err = c4.UserResourceDetail(testAuth())
	if !errors.As(err, &pe) || pe.Kind != provider.ErrSessionDead {
		t.Fatalf("积分接口 401 应为 ErrSessionDead，实得 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 签到：三步流程、幂等、积分回退
// ---------------------------------------------------------------------------

var uuid4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestDailyCheckinThreeSteps(t *testing.T) {
	c, rec := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == EpActivitySlot:
			if got := r.URL.Query().Get("platform"); got != "win32" {
				t.Errorf("platform = %q，必须伪装成 win32（协议 §1.3）", got)
			}
			if r.URL.Query().Get("placement") != ActivityPlacement ||
				r.URL.Query().Get("containerApiVersion") != "2" ||
				r.URL.Query().Get("clientVersion") == "" {
				t.Errorf("槽位 query 不完整：%s", r.URL.RawQuery)
			}
			writeJSON(w, 200, `{"code":0,"data":{"slotState":"available",`+
				`"activity":{"activityCode":"act-1","configRevision":7}}}`)
		case strings.HasSuffix(r.URL.Path, "/context"):
			if got := r.URL.Query().Get("configRevision"); got != "7" {
				t.Errorf("context configRevision = %q，期望 7", got)
			}
			writeJSON(w, 200, `{"code":0,"data":{"state":{"claimedToday":false},`+
				`"actions":["check_in","other"]}}`)
		case strings.HasSuffix(r.URL.Path, "/actions/check_in"):
			body, _ := io.ReadAll(r.Body)
			var in map[string]any
			if err := json.Unmarshal(body, &in); err != nil {
				t.Fatalf("签到请求体不是 JSON：%v", err)
			}
			if in["configRevision"] != float64(7) {
				t.Errorf("configRevision 应原样回传数字 7：%v", in["configRevision"])
			}
			key, _ := in["idempotencyKey"].(string)
			if !uuid4Re.MatchString(key) {
				t.Errorf("idempotencyKey 不是 uuid4：%q", key)
			}
			if p, ok := in["payload"].(map[string]any); !ok || len(p) != 0 {
				t.Errorf("payload 应为空对象：%v", in["payload"])
			}
			writeJSON(w, 200, `{"code":0,"data":{"result":{"creditsGranted":100}}}`)
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
			writeJSON(w, 404, `{}`)
		}
	})
	rep, err := c.DailyCheckinReport(testAuth())
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinClaimed || rep.Amount != 100 {
		t.Fatalf("签到结果 = %+v，期望 claimed/100", rep)
	}
	got := rec.apiPaths()
	want := []string{EpActivitySlot,
		"/api/client-activities/act-1/context",
		"/api/client-activities/act-1/actions/check_in"}
	if len(got) != len(want) {
		t.Fatalf("请求路径 = %v，期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 步 = %q，期望 %q", i+1, got[i], want[i])
		}
	}
	// 签到/积分端点只发 4 个基础头。
	_, h, _ := rec.byPath(EpActivitySlot)
	if h.Get("Authorization") != "Bearer fake-access-token" {
		t.Errorf("签到缺少 Authorization：%v", h)
	}
	if h.Get(HeaderCapabilities) != "" {
		t.Errorf("签到不得带客户端能力头")
	}
}

func TestDailyCheckinReportStates(t *testing.T) {
	// ① 今日已领取（幂等，不算错误）
	c1, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpActivitySlot {
			writeJSON(w, 200, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"a","configRevision":1}}}`)
			return
		}
		writeJSON(w, 200, `{"code":0,"data":{"state":{"claimedToday":true},"actions":["check_in"]}}`)
	})
	rep, err := c1.DailyCheckinReport(testAuth())
	if err != nil || rep.Status != provider.CheckinAlready {
		t.Fatalf("claimedToday=true 应为 already：(%+v,%v)", rep, err)
	}

	// ② actions 不含 check_in → inactive（可重试，Msg 要透出原因）
	c2, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpActivitySlot {
			writeJSON(w, 200, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"a","configRevision":1}}}`)
			return
		}
		writeJSON(w, 200, `{"code":0,"data":{"state":{"claimedToday":false},"actions":["browse"]}}`)
	})
	rep, err = c2.DailyCheckinReport(testAuth())
	if err != nil || rep.Status != provider.CheckinNoCampaign || !strings.Contains(rep.Msg, "check_in") {
		t.Fatalf("inactive 应为 no_campaign 且 Msg 透出原因：(%+v,%v)", rep, err)
	}

	// ③ 槽位不可用 → no_campaign（活动可能还没上线），且不得继续后续步骤
	c3, rec3 := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"code":0,"data":{"slotState":"hidden"}}`)
	})
	rep, err = c3.DailyCheckinReport(testAuth())
	if err != nil || rep.Status != provider.CheckinNoCampaign || !strings.Contains(rep.Msg, "hidden") {
		t.Fatalf("槽位不可用应为 no_campaign：(%+v,%v)", rep, err)
	}
	if len(rec3.apiPaths()) != 1 {
		t.Fatalf("槽位不可用时不得继续后续步骤：%v", rec3.apiPaths())
	}

	// ④ 无凭据 → no_token（不是错误）
	c4, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("无凭据不得请求上游") })
	rep, err = c4.DailyCheckinReport(&auth.Auth{Kind: string(Kind)})
	if err != nil || rep.Status != provider.CheckinNoToken {
		t.Fatalf("无凭据应为 no_token：(%+v,%v)", rep, err)
	}

	// ⑤ DailyCheckin 与 Report 同源（claimed 时无错误）
	c5, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpActivitySlot {
			writeJSON(w, 200, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"a","configRevision":1}}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/actions/check_in") {
			writeJSON(w, 200, `{"code":0,"data":{"result":{"credits":30}}}`)
			return
		}
		writeJSON(w, 200, `{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`)
	})
	if err := c5.DailyCheckin(testAuth()); err != nil {
		t.Fatalf("DailyCheckin: %v", err)
	}
}

func TestDailyCheckinPointFallbacks(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int64
	}{
		{"creditsGranted 优先", `{"code":0,"data":{"result":{"creditsGranted":11,"rewardCredits":22,"credits":33}}}`, 11},
		{"回退 rewardCredits", `{"code":0,"data":{"result":{"rewardCredits":22,"credits":33}}}`, 22},
		{"回退 credits", `{"code":0,"data":{"result":{"credits":33}}}`, 33},
		{"都没有 → 0", `{"code":0,"data":{"result":{}}}`, 0},
		{"creditsGranted 为 0 时**不**继续回退", `{"code":0,"data":{"result":{"creditsGranted":0,"credits":33}}}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == EpActivitySlot:
					writeJSON(w, 200, `{"code":0,"data":{"slotState":"available","activity":{"activityCode":"a","configRevision":1}}}`)
				case strings.HasSuffix(r.URL.Path, "/context"):
					writeJSON(w, 200, `{"code":0,"data":{"state":{"claimedToday":false},"actions":["check_in"]}}`)
				default:
					writeJSON(w, 200, tc.body)
				}
			})
			rep, err := c.DailyCheckinReport(testAuth())
			if err != nil || rep.Status != provider.CheckinClaimed {
				t.Fatalf("(%+v,%v)", rep, err)
			}
			if rep.Amount != tc.want {
				t.Fatalf("积分 = %d，期望 %d", rep.Amount, tc.want)
			}
		})
	}
}

func TestDailyCheckinSessionDead(t *testing.T) {
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, `{"code":40100,"msg":"token rejected"}`)
	})
	_, err := c.DailyCheckinReport(testAuth())
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrSessionDead {
		t.Fatalf("签到 401 应为 ErrSessionDead（不变式 19/23），实得 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 续期：删键、latestKeyfrom 不变、落盘、终态判定
// ---------------------------------------------------------------------------

func TestRefreshTokenBodyAndPersist(t *testing.T) {
	c, rec := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"code":0,"data":{"accessToken":"new-access","refreshToken":"new-refresh","expiresIn":3600}}`)
	})
	a := testAuth()
	a.FilePath = filepath.Join(t.TempDir(), "lobsterai-1.json")
	before := time.Now().Unix()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	_, path, _, h, body := rec.last()
	if path != EpAuthRefresh {
		t.Fatalf("续期路径 = %q", path)
	}
	if h.Get("Authorization") != "" {
		t.Errorf("续期**不带** Authorization（凭据在请求体里）")
	}
	if h.Get("User-Agent") != UserAgent || h.Get("Content-Type") != "application/json" {
		t.Errorf("续期头不完整：%v", h)
	}
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatalf("续期请求体不是 JSON：%v", err)
	}
	if in["refreshToken"] != "fake-refresh-token" {
		t.Errorf("refreshToken = %v", in["refreshToken"])
	}
	if in["firstKeyfrom"] != "1700000000000" {
		t.Errorf("firstKeyfrom = %v", in["firstKeyfrom"])
	}
	// ⚠️ latestKeyfrom 必须仍是凭据里存的原值（**刻意不更新为当前时刻**）。
	if in["latestKeyfrom"] != "1700000000001" {
		t.Errorf("latestKeyfrom = %v，必须保持凭据里的原值", in["latestKeyfrom"])
	}
	if in["uuid"] != "11111111-2222-3333-4444-555555555555" || in["userId"] != "yid-1" {
		t.Errorf("uuid/userId = %v / %v", in["uuid"], in["userId"])
	}
	if in["version"] == "" {
		t.Errorf("version 必须带上")
	}
	if a.AccessToken != "new-access" || a.RefreshToken != "new-refresh" {
		t.Errorf("内存态未更新：%+v", a)
	}
	if a.ExpiresAt < before+3500 {
		t.Errorf("expiresAt = %d，期望 now+expiresIn", a.ExpiresAt)
	}
	// 不变式 20：refresh 轮换必须落盘（否则下次启动拿旧 refresh token）。
	onDisk, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatalf("续期未落盘：%v", err)
	}
	var doc struct {
		Auth struct {
			AccessToken  string            `json:"accessToken"`
			RefreshToken string            `json:"refreshToken"`
			Extra        map[string]string `json:"extra"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(onDisk, &doc); err != nil {
		t.Fatalf("落盘格式不是嵌套 auth 形：%v（%s）", err, onDisk)
	}
	if doc.Auth.AccessToken != "new-access" || doc.Auth.RefreshToken != "new-refresh" {
		t.Errorf("落盘内容未轮换：%+v", doc.Auth)
	}
	if doc.Auth.Extra[ExtraLatestKeyfrom] != "1700000000001" {
		t.Errorf("落盘后 latest_keyfrom 也不得被改成当前时刻：%v", doc.Auth.Extra)
	}
}

func TestRefreshTokenDeletesEmptyKeys(t *testing.T) {
	c, rec := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, `{"code":0,"data":{"accessToken":"new-access"}}`)
	})
	// 只有 refresh_token 的账号：uuid / userId **缺省时删键**而不是发空串。
	a := &auth.Auth{Kind: string(Kind), AccessToken: "old", RefreshToken: "fake-refresh-token"}
	a.FilePath = filepath.Join(t.TempDir(), "lobsterai-2.json")
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken: %v", err)
	}
	_, _, _, _, body := rec.last()
	var in map[string]any
	_ = json.Unmarshal(body, &in)
	for _, k := range []string{"uuid", "userId", "firstKeyfrom", "latestKeyfrom"} {
		if v, has := in[k]; has {
			t.Errorf("缺省键 %s 应被**删除**而不是发空串，实得 %v", k, v)
		}
	}
	// 响应没回 refresh_token 时保留旧的，不得回退成空串。
	if a.RefreshToken != "fake-refresh-token" {
		t.Errorf("refreshToken = %q，应保留旧值", a.RefreshToken)
	}
}

func TestRefreshTokenTerminalAndRetryable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		{"HTTP 401", http.StatusUnauthorized, `{"msg":"unauthorized"}`, provider.ErrSessionDead},
		{"HTTP 403", http.StatusForbidden, `{"msg":"forbidden"}`, provider.ErrSessionDead},
		{"session-dead 标记", http.StatusOK, `{"code":500,"msg":"token rejected"}`, provider.ErrSessionDead},
		{"HTTP 500 可重试", http.StatusInternalServerError, `{"msg":"boom"}`, provider.ErrServer},
		{"HTTP 429 可重试", http.StatusTooManyRequests, `{"msg":"slow down"}`, provider.ErrSoftRate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, tc.status, tc.body)
			})
			a := testAuth()
			a.FilePath = filepath.Join(t.TempDir(), "lobsterai.json")
			err := c.RefreshToken(a)
			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("续期失败必须返回 *provider.Error，实得 %v", err)
			}
			if pe.Kind != tc.want {
				t.Fatalf("Kind = %v，期望 %v（%v）", pe.Kind, tc.want, err)
			}
			if tc.want == provider.ErrSessionDead && a.AccessToken != "fake-access-token" {
				t.Errorf("终态失败不得改写内存态：%v", a.AccessToken)
			}
		})
	}
}

func TestRefreshTokenWithoutRefreshToken(t *testing.T) {
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("无 refresh_token 不应请求上游") })
	err := c.RefreshToken(&auth.Auth{Kind: string(Kind), AccessToken: "x"})
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrSessionDead {
		t.Fatalf("无 refresh_token 应为 ErrSessionDead，实得 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 流内错误帧（HTTP 200 + {error:{message}}）
// ---------------------------------------------------------------------------

const streamErrFrame = "data:{\"error\":{\"message\":\"今日免费额度已用完，请升级套餐\"}}\n\n"

func TestStreamInStreamErrorFrame(t *testing.T) {
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamErrFrame)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	rc, status, _, err := c.ChatStream(testAuth(), []byte(`{"model":"kimi-k2.5","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("ChatStream = (%d,%v)，期望 HTTP 200（错误在流内）", status, err)
	}
	defer rc.Close()

	rec := httptest.NewRecorder()
	_, serr := c.Stream(rec, rc, "lobsterai/kimi-k2.5")
	var se *StreamError
	if !errors.As(serr, &se) {
		t.Fatalf("流内错误帧必须返回 *StreamError（handler 据此冷却账号），实得 %v", serr)
	}
	if se.Kind() != provider.ErrHardCredit {
		t.Fatalf("「免费额度已用完」应归 ErrHardCredit，实得 %v", se.Kind())
	}
	var sec provider.StreamErrorClassifier
	if !errors.As(serr, &sec) || sec.Kind() != provider.ErrHardCredit {
		t.Fatalf("必须实现 provider.StreamErrorClassifier")
	}
	body := rec.Body.String()
	// ① 错误帧必须透出（不得被吞成「干净的空回答」，R40）
	if !strings.Contains(body, "免费额度已用完") {
		t.Fatalf("客户端未看到上游错误帧：%q", body)
	}
	// ② 不得伪造成功收尾
	if strings.Contains(body, "finish_reason") {
		t.Fatalf("流内错误不得伪造正常收尾帧：%q", body)
	}
	if se.Model != "lobsterai/kimi-k2.5" {
		t.Errorf("Model = %q", se.Model)
	}
}

func TestStreamInStreamErrorWithoutTrailingNewline(t *testing.T) {
	// 上游在最后一帧中途断流（错误帧没有尾换行）：仍必须识别出错误帧，
	// 否则「额度用尽」会被当成一次干净的空回答。
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data:{"error":{"message":"余额不足"}}`)
	})
	rc, status, _, err := c.ChatStream(testAuth(), []byte(`{"model":"m","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("ChatStream = (%d,%v)", status, err)
	}
	defer rc.Close()
	rec := httptest.NewRecorder()
	_, serr := c.Stream(rec, rc, "lobsterai/m")
	var se *StreamError
	if !errors.As(serr, &se) || se.Kind() != provider.ErrHardCredit {
		t.Fatalf("无尾换行的错误帧同样要能分类，实得 %v", serr)
	}
}

func TestStreamInStreamErrorOtherKind(t *testing.T) {
	// 未命中 hard-credit / session-dead 的流内错误 → ErrClient（其余一律 client）。
	se := parseStreamError(`{"error":{"message":"boom"}}`, "m", 200)
	if se == nil || se.Kind() != provider.ErrClient {
		t.Fatalf("其余流内错误应归 ErrClient：%+v", se)
	}
	se2 := parseStreamError(`{"error":{"message":"token rejected"}}`, "m", 200)
	if se2 == nil || se2.Kind() != provider.ErrSessionDead {
		t.Fatalf("流内 session-dead 标记应归 ErrSessionDead：%+v", se2)
	}
	if parseStreamError(`{"choices":[]}`, "m", 200) != nil {
		t.Fatalf("非 error 帧不得被判为流内错误")
	}
	// 正文里出现 error 字样不得误判（否则会冷却健康账号）。
	if parseStreamError(`{"choices":[{"delta":{"content":"this code has an error"}}]}`, "m", 200) != nil {
		t.Fatalf("正文含 error 字样不得误判为错误帧")
	}
}

func TestAggregateInStreamError(t *testing.T) {
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, streamErrFrame)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	rc, _, _, err := c.ChatStream(testAuth(), []byte(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	_, err = c.Aggregate(rc, "lobsterai/m")
	if err == nil {
		t.Fatalf("聚合路径同样必须识别流内错误帧（否则客户端看到空回答且不报错）")
	}
	var se *StreamError
	if !errors.As(err, &se) || se.Kind() != provider.ErrHardCredit {
		t.Fatalf("聚合路径的流内错误应带分类，实得 %v", err)
	}
}

func TestStreamSuccessPassthroughAndModelRewrite(t *testing.T) {
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"object\":\"chat.completion.chunk\",\"model\":\"kimi-k2.5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"object\":\"chat.completion.chunk\",\"model\":\"kimi-k2.5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	rc, _, _, err := c.ChatStream(testAuth(), []byte(`{"model":"lobsterai/kimi-k2.5","messages":[]}`))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer rc.Close()
	rec := httptest.NewRecorder()
	usage, err := c.Stream(rec, rc, "lobsterai/kimi-k2.5")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if usage == nil || usage["prompt_tokens"] != float64(7) {
		t.Fatalf("usage 未捕获：%v", usage)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"model":"lobsterai/kimi-k2.5"`) {
		t.Fatalf("model 未回填成客户端请求名（R14）：%q", out)
	}
	if !strings.Contains(out, "你") || !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("正文或结束标记丢失：%q", out)
	}
}

func TestChatStreamNoTokenIsSessionDead(t *testing.T) {
	c, _ := newUpstream(t, func(w http.ResponseWriter, r *http.Request) { t.Fatal("无凭据不得请求上游") })
	_, _, _, err := c.ChatStream(&auth.Auth{Kind: string(Kind)}, []byte(`{"model":"m"}`))
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrSessionDead {
		t.Fatalf("无 access_token 应为 ErrSessionDead，实得 %v", err)
	}
}

// ---------------------------------------------------------------------------
// Classify 优先级顺序（协议 §5.1）
// ---------------------------------------------------------------------------

func TestClassifyOrder(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
		why    string
	}{
		{"402", 402, `{"msg":"whatever"}`, provider.ErrHardCredit, "§5.1 第 1 条：402 优先于一切"},
		{"余额不足中文", 200, `{"msg":"余额不足"}`, provider.ErrHardCredit, "§5.1 第 2 条"},
		{"免费额度文案", 200, `{"msg":"免费额度已用完，请升级套餐"}`, provider.ErrHardCredit, "§5.1 第 2 条（2026-09 补充文案）"},
		{"余额不足英文", 500, `{"error":"insufficient credits"}`, provider.ErrHardCredit, "§5.1 第 2 条优先于 ≥500"},
		{"40100", 200, `{"code":40100,"msg":"x"}`, provider.ErrSessionDead, "§5.1 第 3 条"},
		{"40101", 200, `{"code":40101,"msg":"x"}`, provider.ErrSessionDead, "§5.1 第 3 条"},
		{"token rejected", 400, `{"msg":"token rejected"}`, provider.ErrSessionDead, "§5.1 第 3 条优先于 ≥400"},
		{"refresh token was rejected", 200, `{"msg":"Refresh token was rejected"}`, provider.ErrSessionDead, "§5.1 第 3 条"},
		{"请求里的 40100 不算命中", 200, `{"request_id":"a40100b"}`, provider.ErrNone, "裸 Contains 会误命中 request_id"},
		{"429", 429, `{"msg":"slow down"}`, provider.ErrSoftRate, "§5.1 第 4 条"},
		{"429 且带余额字样仍归限流", 429, `{"error":"quota exceeded"}`, provider.ErrSoftRate,
			"不变式 15：限流 body 常带 quota exceeded，先判余额会误判成 12h 硬冷却"},
		{"404", 404, `{}`, provider.ErrNotFound, "§5.1 第 5 条"},
		{"500", 500, `{}`, provider.ErrServer, "§5.1 第 6 条"},
		{"400", 400, `{}`, provider.ErrClient, "§5.1 第 7 条"},
		{"200", 200, `{}`, provider.ErrNone, "§5.1 第 8 条"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.body); got != tc.want {
				t.Fatalf("Classify(%d,%s) = %v，期望 %v（%s）", tc.status, tc.body, got, tc.want, tc.why)
			}
			// 实例方法必须与包级函数同源。
			if got := (&Client{}).Classify(tc.status, tc.body); got != tc.want {
				t.Fatalf("实例方法 Classify = %v，期望 %v", got, tc.want)
			}
		})
	}
}

func TestClassifyStreamOrder(t *testing.T) {
	cases := []struct {
		msg  string
		want provider.ErrKind
	}{
		{"余额不足", provider.ErrHardCredit},
		{"免费额度已用完，请升级套餐", provider.ErrHardCredit},
		{"token rejected", provider.ErrSessionDead},
		{"daily limit reached", provider.ErrClient},
	}
	for _, tc := range cases {
		if got := ClassifyStream(tc.msg); got != tc.want {
			t.Errorf("ClassifyStream(%q) = %v，期望 %v", tc.msg, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 杂项
// ---------------------------------------------------------------------------

func TestActivityPaths(t *testing.T) {
	if got := ActivityContextPath("act-1"); got != "/api/client-activities/act-1/context" {
		t.Errorf("context 路径 = %q", got)
	}
	if got := ActivityCheckInPath("act-1"); got != "/api/client-activities/act-1/actions/check_in" {
		t.Errorf("签到路径 = %q", got)
	}
	// 带特殊字符的 activityCode 必须转义，不能拼出跨段路径。
	if got := ActivityCheckInPath("a/b"); strings.Contains(got, "/a/b/") {
		t.Errorf("activityCode 未转义：%q", got)
	}
}

// TestDateOfISO 守门：归一结果必须是**保留时分秒的 RFC3339**（UTC+8）。
//
// 历史（2026-10-19 变更）：旧版截到日期粒度（"2026-10-31"），面板按日期分桶，
// 今晚 00:00 失效的包会被误报成「明日到期」。现在保留精确时刻。
// 同时锁住时区语义：无时区后缀的串按 **UTC+8 墙钟**解析（旧实现用 time.Parse
// 按 UTC 解析，"2026-10-31 12:00:00" 会变成 20:00+08:00，偏 8 小时）。
func TestDateOfISO(t *testing.T) {
	cases := map[string]string{
		"2026-10-31T23:59:59+08:00": "2026-10-31T23:59:59+08:00",
		"2026-11-01T00:00:00Z":      "2026-11-01T08:00:00+08:00", // UTC 零点 = 北京 08:00
		"2026-10-31":                "2026-10-31T00:00:00+08:00",
		"2026-10-31 12:00:00":       "2026-10-31T12:00:00+08:00", // 不得按 UTC 解析成 20:00
		"":                          "",
		"not-a-date":                "",
	}
	for in, want := range cases {
		if got := dateOfISO(in); got != want {
			t.Errorf("dateOfISO(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestNewUUID4Shape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		got, err := newUUID4()
		if err != nil {
			t.Fatalf("newUUID4: %v", err)
		}
		if !uuid4Re.MatchString(got) {
			t.Fatalf("不是 uuid4：%q", got)
		}
		if seen[got] {
			t.Fatalf("幂等键重复：%q", got)
		}
		seen[got] = true
	}
}

func TestIdleTimeoutFallback(t *testing.T) {
	if got := (&Client{}).idleTimeout(); got != 90*time.Second {
		t.Errorf("空闲兜底 = %v，期望 90s", got)
	}
	if got := (&Client{IdleTimeout: time.Second}).idleTimeout(); got != time.Second {
		t.Errorf("注入值未生效：%v", got)
	}
}
