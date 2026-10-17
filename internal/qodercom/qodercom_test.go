package qodercom

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
	"wild-work/internal/reasoning"
)

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		"Qwen3.8-Max":      "qwen3.8-max",
		"DeepSeek-V4-Pro":  "deepseek-v4-pro",
		"GLM-5.3":          "glm-5.3",
		"Kimi-K2.7-Code":   "kimi-k2.7-code",
		"MiniMax-M2.7":     "minimax-m2.7",
		"Auto":             "auto",
		"Qwen3.8 Max Test": "qwen3.8-max-test",
	}
	for in, want := range cases {
		if got := NormalizeModelName(in); got != want {
			t.Errorf("NormalizeModelName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParseDynamicModelsScenesFallback 验证 assistant→developer→chat 三级回退。
func TestParseDynamicModelsScenesFallback(t *testing.T) {
	// chat 缺失 → developer 回退
	raw := map[string]json.RawMessage{
		"developer": json.RawMessage(`[{"key":"dk","enable":true,"display_name":"Dev"}]`),
	}
	ms, err := parseDynamicModels(raw)
	if err != nil || len(ms) != 1 || ms[0].Key != "dk" {
		t.Errorf("developer fallback: %v %v", ms, err)
	}
	// 三场景都空 → 错误
	raw = map[string]json.RawMessage{"chat": json.RawMessage(`[]`)}
	if _, err := parseDynamicModels(raw); err == nil {
		t.Errorf("empty scenes should error")
	}
	// assistant 优先于 chat
	raw = map[string]json.RawMessage{
		"chat":      json.RawMessage(`[{"key":"ck","enable":true}]`),
		"assistant": json.RawMessage(`[{"key":"ak","enable":true}]`),
	}
	ms, err = parseDynamicModels(raw)
	if err != nil || len(ms) != 1 || ms[0].Key != "ak" {
		t.Errorf("assistant precedence: %v %v", ms, err)
	}
}

// TestParseDynamicModelsContextWindow 验证解析端真的把 context_config 读进 ModelEntry。
// 回归：此前 ContextWindow 是 json:"-" 且解析只 Unmarshal 到 ModelEntry，
// context_config 被整段丢弃，导致永远回退到 max_input_tokens（恒为 180000 兜底）。
func TestParseDynamicModelsContextWindow(t *testing.T) {
	raw := map[string]json.RawMessage{
		"assistant": json.RawMessage(`[
			{"key":"m1","display_name":"M1","enable":true,"is_default":true,
			 "is_reasoning":true,"is_vl":true,"max_input_tokens":180000,"price_factor":0.5,
			 "context_config":{"default":{"is_default":true,"token_count":1000000},
			                   "compact":{"is_default":false,"token_count":180000}}},
			{"key":"m2","display_name":"M2","enable":true,"max_input_tokens":96000,"price_factor":0.1},
			{"key":"m3","display_name":"M3","enable":true,"max_input_tokens":180000,
			 "context_config":{"c":{"token_count":500000}}},
			{"key":"m4","display_name":"M4","enable":true,"max_input_tokens":180000,
			 "context_config":[{"token_count":7}]},
			{"key":"m5","display_name":"M5","enable":true,"max_input_tokens":180000,
			 "context_config":{"a":{"is_default":true,"token_count":400000},
			                   "b":{"is_default":true,"token_count":272000}}},
			{"key":"off","display_name":"OFF","enable":false,
			 "context_config":{"d":{"is_default":true,"token_count":9}}}
		]`),
	}
	ms, err := parseDynamicModels(raw)
	if err != nil {
		t.Fatalf("parseDynamicModels: %v", err)
	}
	if len(ms) != 5 {
		t.Fatalf("enabled len = %d, want 5 (off 应被过滤)", len(ms))
	}
	// ① is_default 那项胜出，而不是 max_input_tokens 的 180000
	if ms[0].ContextWindow != 1000000 {
		t.Errorf("is_default token_count not parsed: %+v", ms[0])
	}
	// ② 无 context_config → 0，留给 toModelInfos 回退 max_input_tokens
	if ms[1].ContextWindow != 0 {
		t.Errorf("absent context_config should stay 0: %+v", ms[1])
	}
	// ③ **无 is_default 标记时不猜**：返回 0，让上层回退 max_input_tokens。
	// 回归：曾按 label 字典序取最小项，而字典序首位是 "1M"，会取到**最大档**（不安全方向）。
	if ms[2].ContextWindow != 0 {
		t.Errorf("unmarked context_config must not be guessed: %+v", ms[2])
	}
	// ④ 形状不符（数组）只损失本字段，不应让整批模型解析失败
	if ms[3].Key != "m4" || ms[3].ContextWindow != 0 {
		t.Errorf("malformed context_config should degrade to 0: %+v", ms[3])
	}
	// ⑤ 多个档同时标默认 → 取最小值（确定性 + 保守，不依赖 map 迭代序）
	if ms[4].ContextWindow != 272000 {
		t.Errorf("multi-default should take min: %+v", ms[4])
	}
	// ⑥ 一路落到对外展示的 ContextWindow：宁高勿低 → max(档位表)
	infos := toModelInfos(ms)
	if infos[0].ContextWindow != 1000000 || !infos[0].ContextFromAPI {
		t.Errorf("toModelInfos[0]: %+v", infos[0])
	}
	if infos[1].ContextWindow != 96000 || !infos[1].ContextFromAPI {
		t.Errorf("toModelInfos[1] max_input fallback: %+v", infos[1])
	}
	// 无 is_default 但有档位表 → 广告最大档（而非回退 max_input_tokens=180000）
	if infos[2].ContextWindow != 500000 {
		t.Errorf("toModelInfos[2] should advertise max tier: %+v", infos[2])
	}
}

// TestBuildAgentBodyFormatSourceFromUpstream 验证 model_config 的 format/source 走**上游真值**，
// 而非硬编码常量。P0b 实测两渠道 204/204 条目的 format/source 分别为 "openai"/"system"，
// 故改前改后线上行为一致；本测试用非默认值证明真的是“取自上游”而非“恰好写对”。
func TestBuildAgentBodyFormatSourceFromUpstream(t *testing.T) {
	body, err := buildAgentBody(
		[]map[string]any{{"role": "user", "content": "hi"}},
		&ModelEntry{Key: "k1", DisplayName: "D1", Format: "up-format", Source: "up-source"},
		nil, reasoningSpec{}, 0, "", 0)
	if err != nil {
		t.Fatalf("buildAgentBody: %v", err)
	}
	var parsed struct {
		ModelConfig map[string]any `json:"model_config"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if parsed.ModelConfig["format"] != "up-format" {
		t.Errorf("model_config.format = %v, want upstream value", parsed.ModelConfig["format"])
	}
	if parsed.ModelConfig["source"] != "up-source" {
		t.Errorf("model_config.source = %v, want upstream value", parsed.ModelConfig["source"])
	}

	// 上游未下发时才走兜底常量
	body, err = buildAgentBody([]map[string]any{{"role": "user", "content": "hi"}},
		&ModelEntry{Key: "k2"}, nil, reasoningSpec{}, 0, "", 0)
	if err != nil {
		t.Fatalf("buildAgentBody(fallback): %v", err)
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal body(fallback): %v", err)
	}
	if parsed.ModelConfig["format"] != defaultFormat || parsed.ModelConfig["source"] != defaultSource {
		t.Errorf("empty upstream values should use fallback: %+v", parsed.ModelConfig)
	}
}

// TestModelCacheNoStaticFallback 验证「上次成功缓存」语义：动态拉取失败回退缓存，无缓存报错。
func TestModelCacheNoStaticFallback(t *testing.T) {
	c := New()
	// 无缓存：未命中模型 key 原样返回（无静态兜底）
	if got := c.modelKey("deepseek-v4-pro"); got != "deepseek-v4-pro" {
		t.Errorf("no-static fallback: got %q, want passthrough", got)
	}
	// 写入缓存后命中
	c.setCache([]ModelEntry{{Key: "gmodel", DisplayName: "GLM-5.3"}})
	if got := c.modelKey("glm-5.3"); got != "gmodel" {
		t.Errorf("cache hit: got %q", got)
	}
	if cached := c.cachedModels(); len(cached) != 1 {
		t.Errorf("cachedModels len = %d", len(cached))
	}
}

// TestToModelInfosContextWindow 验证广告口径：max(档位表) > is_default > max_input_tokens。
func TestToModelInfosContextWindow(t *testing.T) {
	dyn := []ModelEntry{
		{Key: "m1", DisplayName: "M1", MaxInputTokens: 100000, ContextWindow: 200000,
			AvailableWindows: []int64{200000, 400000, 1000000}},
		{Key: "m2", DisplayName: "M2", MaxInputTokens: 150000},
		{Key: "m3", DisplayName: "M3", IsVL: true, IsReasoning: true},
	}
	infos := toModelInfos(dyn)
	if len(infos) != 3 {
		t.Fatalf("len = %d", len(infos))
	}
	// 有档位表 → 广告最大档（而非 is_default=200000）
	if infos[0].ContextWindow != 1000000 || !infos[0].ContextFromAPI {
		t.Errorf("max tier should win: %+v", infos[0])
	}
	if infos[1].ContextWindow != 150000 {
		t.Errorf("max_input fallback: %+v", infos[1])
	}
	if !infos[2].SupportsImages || !infos[2].SupportsReasoning {
		t.Errorf("capabilities: %+v", infos[2])
	}
}

// newAuth 测试凭据（已含机器指纹）。
func newAuth() *auth.Auth {
	return &auth.Auth{
		Kind: "qodercn", AccessToken: "dt-test", RefreshToken: "drt-test",
		UID: "u1", Nickname: "tester",
		MachineID: "mid", MachineToken: "mtok", MachineType: "mtype",
	}
}

// TestCheckinCampaignsClaimable 端到端：campaigns 主路径领取成功。
func TestCheckinCampaignsClaimable(t *testing.T) {
	var claimCalled int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCampaigns:
			// 校验桌面端签到头（cosy-clienttype=10 是关键）
			if r.Header.Get("cosy-clienttype") != "10" {
				t.Errorf("cosy-clienttype = %q, want 10", r.Header.Get("cosy-clienttype"))
			}
			if r.Header.Get("authorization") != "Bearer dt-test" {
				t.Errorf("authorization = %q", r.Header.Get("authorization"))
			}
			w.Write([]byte(`{"campaigns":[{"campaignId":"c1","campaignKey":"act-20260920-549","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE"}]}`))
		case EpCampaigns + "/c1/claim":
			claimCalled++
			w.Write([]byte(`{"status":"CLAIMED","replayed":false,"benefit":{"amount":100}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	oldHost := checkinHost
	checkinHost = srv.URL
	defer func() { checkinHost = oldHost }()

	a := newAuth()
	rep, err := checkin(a)
	if err != nil {
		t.Fatalf("checkin: %v", err)
	}
	if rep.Status != provider.CheckinClaimed || rep.Amount != 100 {
		t.Errorf("report = %+v, want claimed +100", rep)
	}
	if claimCalled != 1 {
		t.Errorf("claim called %d times", claimCalled)
	}
}

// TestCheckinAlreadyClaimed 幂等：409 / CLAIMED / replayed 都算成功。
func TestCheckinAlreadyClaimed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCampaigns:
			w.Write([]byte(`{"campaigns":[{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMED"}]}`))
		default:
			t.Errorf("unexpected claim on already-claimed: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	oldHost := checkinHost
	checkinHost = srv.URL
	defer func() { checkinHost = oldHost }()

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("already-claimed should be success: %v", err)
	}
	if rep.Status != provider.CheckinAlready || rep.Status.Retryable() {
		t.Errorf("report = %+v, want already_claimed (not retryable)", rep)
	}
}

// TestCheckinNoCampaignIsRetryable 活动列表为空 → no_campaign 且可重试（不当日完成）。
func TestCheckinNoCampaignIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCampaigns:
			w.Write([]byte(`{"showCampaign":true,"claimable":false,"campaigns":[]}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	oldHost := checkinHost
	checkinHost = srv.URL
	defer func() { checkinHost = oldHost }()

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("no_campaign is not an error: %v", err)
	}
	if rep.Status != provider.CheckinNoCampaign || !rep.Status.Retryable() {
		t.Errorf("report = %+v, want no_campaign (retryable)", rep)
	}
}

// TestCheckinCampaignsUnavailable campaigns 不可用（404）：报告 error 且可重试（COM 无回退路径）。
func TestCheckinCampaignsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	oldHost := checkinHost
	checkinHost = srv.URL
	defer func() { checkinHost = oldHost }()

	rep, err := checkin(newAuth())
	if err != nil {
		t.Fatalf("404 is a status, not a transport error: %v", err)
	}
	if rep.Status != provider.CheckinError || !rep.Status.Retryable() {
		t.Errorf("report = %+v, want error (retryable)", rep)
	}
}

// TestConstantsCOMOnly 确认 COM 域名表与 daily-check-in 端点已删除。
func TestConstantsCOMOnly(t *testing.T) {
	if OpenAPIBase != "https://openapi.qoder.sh" {
		t.Errorf("OpenAPIBase = %s", OpenAPIBase)
	}
	if GatewayBase != "https://api1.qoder.sh" {
		t.Errorf("GatewayBase = %s", GatewayBase)
	}
	if ModelsBase != "https://api2.qoder.sh" {
		t.Errorf("ModelsBase = %s", ModelsBase)
	}
	// EpCheckinSt/EpCheckinCl 在 COM 不应存在（编译期保证，此处是文档性断言）
}

// TestCheckin401SessionDead 401 必须是类型化的 ErrSessionDead（不变式 19）。
func TestCheckin401SessionDead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"TOKEN_EXPIRE"}`))
	}))
	defer srv.Close()
	oldHost := checkinHost
	checkinHost = srv.URL
	defer func() { checkinHost = oldHost }()

	_, err := checkin(newAuth())
	if err == nil {
		t.Fatal("401 should propagate error")
	}
	var ue *provider.Error
	if !errors.As(err, &ue) || ue.Kind != provider.ErrSessionDead {
		t.Errorf("err = %v (%T), want *provider.Error with ErrSessionDead", err, err)
	}
}

// TestCheckinNoToken 无 dt- 时返回 no_token（可重试，不报 error）。
func TestCheckinNoToken(t *testing.T) {
	rep, err := checkin(&auth.Auth{Kind: "qodercom", UID: "u1"})
	if err != nil {
		t.Fatalf("no token should not error: %v", err)
	}
	if rep.Status != provider.CheckinNoToken || !rep.Status.Retryable() {
		t.Errorf("report = %+v, want no_token (retryable)", rep)
	}
}

// TestBuildAgentBodyBodyShape 验证请求体模板形态（session_type=qoder 等差异字段）。
func TestBuildAgentBodyShape(t *testing.T) {
	mc := &ModelEntry{Key: "gmodel", DisplayName: "GLM-5.3", MaxInputTokens: 180000}
	raw, err := buildAgentBody(
		[]map[string]any{{"role": "developer", "content": "sys"}, {"role": "user", "content": "hi"}},
		mc, nil, reasoningSpec{Enabled: true}, 0, "personal_standard", 0)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["session_type"] != "qoder" {
		t.Errorf("session_type = %v, want qoder", body["session_type"])
	}
	if body["agent_id"] != "agent_common" || body["chat_task"] != "FREE_INPUT" {
		t.Errorf("agent/chat_task = %v/%v", body["agent_id"], body["chat_task"])
	}
	if body["task_id"] != "common" || body["version"] != "3" {
		t.Errorf("qoder2api fields missing: task_id=%v version=%v", body["task_id"], body["version"])
	}
	params, _ := body["parameters"].(map[string]any)
	if params["max_tokens"] != float64(32768) {
		t.Errorf("default max_tokens = %v", params["max_tokens"])
	}
	mcfg, _ := body["model_config"].(map[string]any)
	if mcfg["key"] != "gmodel" || mcfg["is_reasoning"] != true || mcfg["source"] != "system" {
		t.Errorf("model_config = %v", mcfg)
	}
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d", len(msgs))
	}
	if m0 := msgs[0].(map[string]any); m0["role"] != "system" { // developer → system
		t.Errorf("developer should be rewritten to system, got %v", m0["role"])
	}
}

// TestClassifyOrder 429 优先于 hardMarkers（不变式 15）。
func TestClassifyOrder(t *testing.T) {
	if got := Classify(http.StatusTooManyRequests, `{"isQuotaExceeded":true}`); got != provider.ErrSoftRate {
		t.Errorf("429 + quota text should be soft rate, got %v", got)
	}
	if got := Classify(402, "x"); got != provider.ErrHardCredit {
		t.Errorf("402 should be hard credit, got %v", got)
	}
	if got := Classify(401, `{"code":"TOKEN_EXPIRE"}`); got != provider.ErrSessionDead {
		t.Errorf("TOKEN_EXPIRE should be session dead, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// 思考档位：能力解析 → 投影 → body 字段（realm 为 RealmQoderCOM）
// ---------------------------------------------------------------------------

// toModelInfos 必须把上游 thinking_config 的 ladder 带进 provider.ModelInfo。
func TestToModelInfosCarriesThinkingCaps(t *testing.T) {
	dyn := []ModelEntry{
		{
			Key: "gmodel", DisplayName: "GLM-5.3", Enable: true, IsReasoning: true,
			ThinkingConfig: json.RawMessage(`{"disabled":{"description":"x"},` +
				`"enabled":{"efforts":{"low":{},"high":{"is_default":true}}}}`),
		},
		{Key: "nomodel", DisplayName: "No-Ladder", Enable: true},
	}
	infos := toModelInfos(dyn)
	if len(infos) != 2 {
		t.Fatalf("infos len = %d", len(infos))
	}
	withLadder := infos[0]
	if len(withLadder.SupportedEfforts) != 2 || withLadder.DefaultEffort != "high" ||
		!withLadder.ReasoningCanDisable {
		t.Errorf("档位能力未透出：%+v", withLadder)
	}
	noLadder := infos[1]
	if len(noLadder.SupportedEfforts) != 0 || noLadder.DefaultEffort != "" || noLadder.ReasoningCanDisable {
		t.Errorf("未声明 thinking_config 的模型不该暴露档位：%+v", noLadder)
	}
}

// 思考投影：只认 RealmQoderCOM 面（与 Qoder / QoderCN 的同名模型互不串味）。
//
// 三个面的 ladder 刻意设成不同值：串味会立刻体现在降级结果上
// （COM 是 low/max、CN 是 low/medium/xhigh、Qoder 只有 low）。
func TestReasoningSpecForProjection(t *testing.T) {
	old := reasoning.Caps
	reasoning.Caps = reasoning.NewCatalog()
	t.Cleanup(func() { reasoning.Caps = old })

	reasoning.Caps.SetRemote(reasoning.RealmQoderCOM, map[string]reasoning.Cap{
		"probe-ladder":     {Efforts: []string{"low", "max"}, DefaultEffort: "low", SupportsDisable: true},
		"probe-nodisable":  {Efforts: []string{"low", "high", "max"}, DefaultEffort: "max"},
		"probe-switchonly": {SupportsDisable: true},
	})
	reasoning.Caps.SetRemote(reasoning.RealmQoderCN, map[string]reasoning.Cap{
		"probe-ladder": {Efforts: []string{"low", "medium", "xhigh"}, DefaultEffort: "medium"},
	})
	reasoning.Caps.SetRemote(reasoning.RealmQoder, map[string]reasoning.Cap{
		"probe-ladder": {Efforts: []string{"low"}},
	})

	cases := []struct {
		name     string
		model    string
		effort   string
		thinking *thinkingParam
		wantOn   bool
		wantEff  string
	}{
		{"未表达", "probe-ladder", "", nil, false, ""},
		{"显式关闭（可关模型）", "probe-ladder", "none", nil, false, "none"},
		{"显式关闭 off（可关模型）", "probe-ladder", "off", nil, false, "none"},
		{"显式关闭（不可关模型→降最低档）", "probe-nodisable", "none", nil, true, "low"},
		{"显式关闭（只有开关的模型）", "probe-switchonly", "none", nil, false, "none"},
		{"显式关闭（能力未知→只关开关）", "probe-unknown", "none", nil, false, ""},
		{"指定档位命中", "probe-ladder", "max", nil, true, "max"},
		{"指定档位就近降级（realm 隔离：COM 面 low/max，不串 CN 的 medium）", "probe-ladder", "medium", nil, true, "low"},
		{"指定档位超上限", "probe-ladder", "ultra", nil, true, "max"},
		{"指定档位低于下限", "probe-nodisable", "minimal", nil, true, "low"},
		{"只说开思考→补默认档（COM 面 low）", "probe-ladder", "", &thinkingParam{Type: "enabled"}, true, "low"},
		{"adaptive→补默认档", "probe-ladder", "", &thinkingParam{Type: "adaptive"}, true, "low"},
		{"thinking disabled 兜底", "probe-ladder", "", &thinkingParam{Type: "disabled"}, false, "none"},
		// 保守守卫：能力未知（目录未下发 thinking_config）时不下发档位字段。
		{"指定档位（能力未知→只翻开关）", "probe-unknown", "high", nil, true, ""},
		{"只说开思考（能力未知）", "probe-unknown", "", &thinkingParam{Type: "enabled"}, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reasoningSpecFor(tc.model, tc.effort, tc.thinking)
			if got.Enabled != tc.wantOn || got.Effort != tc.wantEff {
				t.Errorf("reasoningSpecFor(%q, %q) = {on:%v effort:%q}, want {on:%v effort:%q}",
					tc.model, tc.effort, got.Enabled, got.Effort, tc.wantOn, tc.wantEff)
			}
		})
	}
}

// body 层：档位与开关同源落在 model_config 与 parameters 两处。
func TestBuildAgentBodyReasoningFields(t *testing.T) {
	mc := &ModelEntry{Key: "gmodel", DisplayName: "GLM-5.3", MaxInputTokens: 180000}
	msgs := []map[string]any{{"role": "user", "content": "hi"}}

	decode := func(t *testing.T, spec reasoningSpec) map[string]any {
		t.Helper()
		raw, err := buildAgentBody(msgs, mc, nil, spec, 0, "personal_standard", 0)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	body := decode(t, reasoningSpec{Enabled: true, Effort: "max"})
	params, _ := body["parameters"].(map[string]any)
	if params["reasoning_effort"] != "max" || params["enable_thinking"] != true {
		t.Errorf("parameters 档位字段错：%v", params)
	}
	mcfg, _ := body["model_config"].(map[string]any)
	if mcfg["is_reasoning"] != true {
		t.Errorf("model_config.is_reasoning 应为 true：%v", mcfg)
	}
	extra, _ := body["chat_context"].(map[string]any)["extra"].(map[string]any)
	lite, _ := extra["modelConfig"].(map[string]any)
	if lite["is_reasoning"] != true {
		t.Errorf("chat_context.extra.modelConfig 未同源：%v", lite)
	}

	// 显式关闭：effort=none + enable_thinking=false + is_reasoning=false
	body = decode(t, reasoningSpec{Enabled: false, Effort: "none"})
	params, _ = body["parameters"].(map[string]any)
	if params["reasoning_effort"] != "none" || params["enable_thinking"] != false {
		t.Errorf("关闭形态错：%v", params)
	}

	// 未给档位：不下发 reasoning_effort（不打扰上游默认档），
	// 但 enable_thinking **必须恒下发**并与 is_reasoning 同源 ——
	// 2026-09-22 在 QoderCN 侧实测：缺它时上游关不掉思考（只发 is_reasoning=false 无效）。
	body = decode(t, reasoningSpec{Enabled: true})
	params, _ = body["parameters"].(map[string]any)
	if _, has := params["reasoning_effort"]; has {
		t.Errorf("无档位时不该下发 reasoning_effort：%v", params)
	}
	if params["enable_thinking"] != true {
		t.Errorf("无档位（开思考）时 enable_thinking 必须恒为 true：%v", params)
	}
	if params["max_tokens"] != float64(32768) {
		t.Errorf("max_tokens 默认值被破坏：%v", params["max_tokens"])
	}

	// 未表达（Enabled=false 且无档位）是最常见的默认形态：必须同时下发
	// enable_thinking=false，否则上游关不掉思考（实测思考爆炸到 180s 超时）。
	body = decode(t, reasoningSpec{})
	params, _ = body["parameters"].(map[string]any)
	if params["enable_thinking"] != false {
		t.Errorf("未表达时 enable_thinking 必须为 false（否则上游关不掉思考）：%v", params)
	}
	if _, has := params["reasoning_effort"]; has {
		t.Errorf("未表达时不该下发 reasoning_effort：%v", params)
	}
	mcfg, _ = body["model_config"].(map[string]any)
	if mcfg["is_reasoning"] != false {
		t.Errorf("未表达时 is_reasoning 应为 false：%v", mcfg)
	}
}

// TestResolveContextWindow 验证上下文档位解析（TZ 校验 + 本仓默认最大档）。
func TestResolveContextWindow(t *testing.T) {
	mc := &ModelEntry{
		Key: "m", MaxInputTokens: 180000,
		ContextWindow:    200000,
		AvailableWindows: []int64{200000, 400000, 1000000},
	}
	if got := resolveContextWindow(400000, mc); got != 400000 {
		t.Errorf("valid tier: got %d, want 400000", got)
	}
	// 非法值与未指定 → 最大档（本仓默认，非官方 is_default）
	if got := resolveContextWindow(300000, mc); got != 1000000 {
		t.Errorf("invalid tier → max: got %d, want 1000000", got)
	}
	if got := resolveContextWindow(0, mc); got != 1000000 {
		t.Errorf("no hint → max: got %d, want 1000000", got)
	}
	if got := resolveContextWindow(0, nil); got != 0 {
		t.Errorf("nil entry: got %d, want 0", got)
	}
}

// TestBuildAgentBodyContextLength 验证 context_length 注入。
func TestBuildAgentBodyContextLength(t *testing.T) {
	mc := &ModelEntry{Key: "k", DisplayName: "K", MaxInputTokens: 180000}
	raw, err := buildAgentBody([]map[string]any{{"role": "user", "content": "hi"}},
		mc, nil, reasoningSpec{}, 0, "", 1000000)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Parameters  map[string]any `json:"parameters"`
		ModelConfig map[string]any `json:"model_config"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body.Parameters["context_length"] != float64(1000000) {
		t.Errorf("context_length = %v", body.Parameters["context_length"])
	}
	if body.ModelConfig["max_input_tokens"] != float64(1000000) {
		t.Errorf("max_input_tokens = %v", body.ModelConfig["max_input_tokens"])
	}
}

// TestStreamClientHasNoTotalTimeout 守门：流式必须走**无总超时**的 client。
//
// 背景（2026-09-25 实测）：http.Client.Timeout 是整请求上限，计时器在 Do() 返回后继续跑
// 直到 body 读完；SSE 整个生成期都在读 body，故长思考请求会被从流中间掐断 ——
// 同源渠道日志中断恰好 120.00s（= config.upstream.timeout_seconds），客户端表现为「突然无响应」。
// 非流式 client 必须保留总超时（短请求的合理兜底）。
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

// TestStreamTruncationEmitsFrames 守门：流中断必须补一帧 error，且不得补 [DONE]。
//
// 此前 parseNestedSSE 报错时直接 return，客户端收到一条无收尾的截断流，只能一直等。
// 但补 [DONE] 同样不对（R36 / issue #42）：[DONE] 表示正常结束，与 error 帧一起
// 发出等于把截断伪装成成功收尾，客户端会拿着半截 tool_call arguments 去解析。
func TestStreamTruncationEmitsFrames(t *testing.T) {
	// 前半段正常，随后读错误（模拟空闲超时/连接被切断）。
	in := "data: " + `{"body":"{\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}"}` + "\n\n" +
		"data: " + `{"body":"{\"choices\":[{\"index\":0,\"delta\":{\"content\":\"there\"}}]}"}` + "\n\n"
	rc := &errAfterReader{data: in, err: provider.ErrIdleTimeout}

	rec := httptest.NewRecorder()
	_, err := Stream(rec, rc, "qodercom/qwen3.8-flash")
	if err == nil {
		t.Fatal("读错误必须上抛给 handler（用于日志）")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"upstream_timeout"`) {
		t.Fatalf("缺少 upstream_timeout 错误帧: %s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("截断不得补 [DONE]: %s", body)
	}
	// 已成功透传的部分不能丢。
	if !strings.Contains(body, "hi") || !strings.Contains(body, "there") {
		t.Fatalf("已透传内容丢失: %s", body)
	}
}

// errAfterReader 先吐完 data，再返回指定错误（模拟「读到一半流断了」）。
type errAfterReader struct {
	data string
	err  error
	off  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}
