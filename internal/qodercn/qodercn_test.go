package qodercn

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
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

func TestEncodeDecodeRoundtrip(t *testing.T) {
	plain := []byte(`{"a":1,"b":"中文内容 😀","c":[true,null,3.14]}`)
	enc := qoderEncode(plain)
	if enc == "" {
		t.Fatal("empty encode")
	}
	dec, err := qoderDecode(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(dec) != string(plain) {
		t.Fatalf("roundtrip mismatch")
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
		nil, false, 0, "", 0)
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
		&ModelEntry{Key: "k2"}, nil, false, 0, "", 0)
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

// TestCheckinClaimReplayed claim 返回 replayed=true：幂等成功，不再重试。
func TestCheckinClaimReplayed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCampaigns:
			w.Write([]byte(`{"campaigns":[{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE"}]}`))
		case EpCampaigns + "/c1/claim":
			w.Write([]byte(`{"status":"CLAIMED","replayed":true}`))
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
		t.Fatalf("replayed should be success: %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Errorf("report = %+v, want already_claimed", rep)
	}
}

// TestCheckinConflict409 claim 返回 409：幂等成功。
func TestCheckinConflict409(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCampaigns:
			w.Write([]byte(`{"campaigns":[{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE"}]}`))
		case EpCampaigns + "/c1/claim":
			w.WriteHeader(http.StatusConflict)
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
		t.Fatalf("409 should be success: %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Errorf("report = %+v, want already_claimed", rep)
	}
}

// TestCheckinNoCampaignIsRetryable 活动列表为空（10:00 整点尚未创建）→ no_campaign 且可重试。
// 这是上游 ae3d42f 修的核心问题：不能把 no_campaign 当作「当日已完成」。
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

// TestCheckinLegacyNeverClaims legacy daily-check-in 端点绝不被用于领取。
// 背景：该端点已 DISABLED 却对未领取日恒返回 409，走它会造成「假成功零积分」
// （上游 99ab022 的结论，本项目抓包同款）。
func TestCheckinLegacyNeverClaims(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpCampaigns:
			w.Write([]byte(`{"campaigns":[{"campaignId":"c1","actionType":"CLAIM_BENEFIT","claimStatus":"CLAIMABLE"}]}`))
		case EpCampaigns + "/c1/claim":
			w.Write([]byte(`{"status":"CLAIMED","replayed":false,"benefit":{"amount":100}}`))
		default:
			// EpCheckinCl / EpCheckinSt 命中即失败
			t.Errorf("legacy endpoint must not be used: %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	oldHost := checkinHost
	checkinHost = srv.URL
	defer func() { checkinHost = oldHost }()

	if _, err := checkin(newAuth()); err != nil {
		t.Fatalf("checkin: %v", err)
	}
}

// TestCheckinCampaignsUnavailable campaigns 非 200 且非 401：报告 error 且可重试（无 legacy 回退）。
// 与上游一致：该情形不是 Go error（无 401/网络层异常），而是结果状态，由调度器决定重试。
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

// TestCheckin401SessionDead 401 必须是类型化的 ErrSessionDead，
// 否则调度器的 isSessionDead（errors.As 类型断言）恒为 false，自愈失效（不变式 19）。
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
	rep, err := checkin(&auth.Auth{Kind: "qodercn", UID: "u1"})
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
		mc, nil, true, 0, "personal_standard", 0)
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

// TestResolveContextWindow 验证上下文档位解析（TZ 校验 + 本仓默认最大档）。
func TestResolveContextWindow(t *testing.T) {
	mc := &ModelEntry{
		Key: "m", MaxInputTokens: 180000,
		ContextWindow:    200000, // is_default 档
		AvailableWindows: []int64{200000, 400000, 1000000},
	}
	// ① 客户端指定档位 ∈ 表 → 用指定值
	if got := resolveContextWindow(400000, mc); got != 400000 {
		t.Errorf("valid tier: got %d, want 400000", got)
	}
	// ② 客户端指定不在表内 → 落最大档（本仓默认，非官方 is_default）
	if got := resolveContextWindow(300000, mc); got != 1000000 {
		t.Errorf("invalid tier → max: got %d, want 1000000", got)
	}
	// ③ 未指定 → 最大档
	if got := resolveContextWindow(0, mc); got != 1000000 {
		t.Errorf("no hint → max: got %d, want 1000000", got)
	}
	// ④ 无档位表：≤ max_input_tokens 接受
	mc2 := &ModelEntry{Key: "m2", MaxInputTokens: 180000}
	if got := resolveContextWindow(128000, mc2); got != 128000 {
		t.Errorf("no table, within max: got %d, want 128000", got)
	}
	if got := resolveContextWindow(200000, mc2); got != 180000 {
		t.Errorf("no table, over max → fallback max: got %d, want 180000", got)
	}
	// ⑤ 无条目 → 0（不注入）
	if got := resolveContextWindow(0, nil); got != 0 {
		t.Errorf("nil entry: got %d, want 0", got)
	}
}

// TestBuildAgentBodyContextLength 验证 context_length 注入 parameters + model_config.max_input_tokens。
func TestBuildAgentBodyContextLength(t *testing.T) {
	mc := &ModelEntry{Key: "k", DisplayName: "K", MaxInputTokens: 180000}
	// 指定档位
	raw, err := buildAgentBody([]map[string]any{{"role": "user", "content": "hi"}},
		mc, nil, false, 0, "", 400000)
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
	if body.Parameters["context_length"] != float64(400000) {
		t.Errorf("parameters.context_length = %v, want 400000", body.Parameters["context_length"])
	}
	if body.ModelConfig["max_input_tokens"] != float64(400000) {
		t.Errorf("model_config.max_input_tokens = %v, want 400000", body.ModelConfig["max_input_tokens"])
	}
	// 不指定 → 不注入 context_length
	raw, err = buildAgentBody([]map[string]any{{"role": "user", "content": "hi"}},
		mc, nil, false, 0, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	body = struct {
		Parameters  map[string]any `json:"parameters"`
		ModelConfig map[string]any `json:"model_config"`
	}{}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if _, has := body.Parameters["context_length"]; has {
		t.Errorf("context_length should be absent when window=0: %v", body.Parameters)
	}
	// max_tokens 始终存在
	if body.Parameters["max_tokens"] != float64(32768) {
		t.Errorf("max_tokens = %v", body.Parameters["max_tokens"])
	}
}

// TestParseContextWindowHint 两种字段名别名。
func TestParseContextWindowHint(t *testing.T) {
	if got := parseContextWindowHint([]byte(`{"context_length":1000000}`)); got != 1000000 {
		t.Errorf("context_length: got %d", got)
	}
	if got := parseContextWindowHint([]byte(`{"context_window":400000}`)); got != 400000 {
		t.Errorf("context_window: got %d", got)
	}
	// context_length 优先
	if got := parseContextWindowHint([]byte(`{"context_length":200000,"context_window":400000}`)); got != 200000 {
		t.Errorf("priority: got %d", got)
	}
	if got := parseContextWindowHint([]byte(`{"model":"x"}`)); got != 0 {
		t.Errorf("absent: got %d", got)
	}
}
