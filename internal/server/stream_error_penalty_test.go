package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/traework"
)

// 本文件锁定「流内业务错误 → 冷却账号 → 下次请求换号」这一闭环（2026-10-02 修复）。
//
// 为什么必须闭环：流式请求的 HTTP 200 与响应头在读到上游 error 帧**之前**已发出，
// 协议上无法在同一请求内换号重试。唯一补救是把中招账号冷却掉，让客户端**下一次**
// 重试时由挑号逻辑换到别的账号。3004 是**账号级**限流（实测：同秒 8 账号仅 1 个中招），
// 所以换号确实能恢复——这就是用户问的「重试会切换账号吗」的答案。
//
// 修复前：solosse 把错误写成 content + 补 [DONE] + 返回 nil ⇒ handler 无从得知失败
// ⇒ 账号不冷却、粘性路由还把请求钉死在同一个限流号上（重试必再失败）。

// streamErrUpstream 模拟「SSE 流中途回 3004 限流」的上游。
type streamErrUpstream struct {
	mu       sync.Mutex
	calls    []string
	rateCode int64 // 非 0 时，Stream 返回该码的 *SOLOStreamError
}

func (u *streamErrUpstream) RefreshToken(*auth.Auth) error { return nil }

func (u *streamErrUpstream) ChatStream(a *auth.Auth, _ []byte) (io.ReadCloser, int, []byte, error) {
	u.mu.Lock()
	u.calls = append(u.calls, a.UID)
	u.mu.Unlock()
	// 返回 200 + 一段最小 SSE：真正的错误在 Stream 消费时才出现（复刻真实时序）
	return io.NopCloser(strings.NewReader("event:output\ndata:{\"response\":\"部分\"}\n\n")),
		http.StatusOK, nil, nil
}

func (u *streamErrUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) {
	return []provider.ModelInfo{{ID: "m"}}, nil
}
func (u *streamErrUpstream) FetchModelPricing(*auth.Auth) ([]provider.ModelPricing, error) {
	return nil, nil
}
func (u *streamErrUpstream) UserResource(*auth.Auth) (int64, error) { return 0, nil }
func (u *streamErrUpstream) UserResourceDetail(*auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}
func (u *streamErrUpstream) DailyCheckin(*auth.Auth) error { return nil }
func (u *streamErrUpstream) Classify(int, string) provider.ErrKind {
	return provider.ErrClient
}

// Stream 复刻「上游 error 帧」：透传已收内容后返回 *SOLOStreamError。
// 走真实的 traework SOLO 协议→OpenAI 转换与错误分类，不手搓错误对象。
func (u *streamErrUpstream) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	u.mu.Lock()
	code := u.rateCode
	u.mu.Unlock()
	if code == 0 {
		_, _ = io.Copy(io.Discard, r)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		return nil, nil
	}
	// 把上游帧（含 error 帧）+ 真实 solosse 解析串起来
	body := "event:output\ndata:{\"response\":\"部分\"}\n\n" +
		"event:error\ndata:" + soloErrJSON(code) + "\n\n"
	return traework.StreamWithModel(w, strings.NewReader(body), model)
}

func (u *streamErrUpstream) Aggregate(r io.Reader, _ string) (map[string]any, error) {
	_, _ = io.Copy(io.Discard, r)
	return map[string]any{"choices": []any{}}, nil
}

func soloErrJSON(code int64) string {
	b, _ := json.Marshal(map[string]any{
		"code":    code,
		"message": "We're sorry, your requests have exceeded the rate limit.",
	})
	return string(b)
}

func (u *streamErrUpstream) callCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func (u *streamErrUpstream) lastCall() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.calls) == 0 {
		return ""
	}
	return u.calls[len(u.calls)-1]
}

// newStreamErrHandler 建一个 traework 运行时（两账号，A 分更高以确保首次选中 A）。
func newStreamErrHandler(t *testing.T, up *streamErrUpstream) (*Handler, *pool.Pool) {
	t.Helper()
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: "traework", AccessToken: "at-A", ExpiresAt: 4102444800, UID: "A"})
	p.Add(&auth.Auth{Kind: "traework", AccessToken: "at-B", ExpiresAt: 4102444800, UID: "B"})
	// PickExcluding 遍历 map（顺序随机），同分时选择不确定 ⇒ 给 A 更高分让排序确定。
	p.SetCreditDetail("A", 1000, 0, 0)
	p.SetCreditDetail("B", 500, 0, 0)

	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.TraeWork: {Kind: provider.TraeWork, Pool: p, Upstream: up,
				StaticModels: []provider.ModelInfo{{ID: "m"}}},
		},
		APIKey:       "",
		HardCooldown: 12 * 3_600_000_000_000,
		SoftCooldown: 60_000_000_000, // 60s，与生产默认一致
		ErrThreshold: 3,
		ErrCooldown:  600_000_000_000,
	})
	return h, p
}

const streamBody = `{"model":"traework/m","messages":[{"role":"user","content":"hi"}],"stream":true}`

// TestStreamRateLimitCoolsAccountAndSwitchesNextRequest 核心闭环：
//
//	请求 1：A 返回 3004 限流 → A 必须被**软冷却**（且不被禁用）
//	请求 2：A 冷却中 → 挑号必须换到 B，且请求成功
func TestStreamRateLimitCoolsAccountAndSwitchesNextRequest(t *testing.T) {
	up := &streamErrUpstream{rateCode: 3004}
	h, p := newStreamErrHandler(t, up)

	// --- 请求 1：命中 A，流内 3004 ---
	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	if got := up.lastCall(); got != "A" {
		t.Fatalf("请求1 应命中 A（给 A 更高分确保排序），实际命中 %q", got)
	}
	// ① 响应必须带错误帧，且**不得**补 [DONE]（失败不得伪装成正常收尾）
	out := rec1.Body.String()
	if strings.Contains(out, "[DONE]") {
		t.Errorf("流内限流不得补 [DONE]（会伪装成正常收尾）\nbody=%s", out)
	}
	if !strings.Contains(out, "upstream_rate_limited") {
		t.Errorf("应发出限流 error 帧\nbody=%s", out)
	}

	// ② A 必须被软冷却（这是"下次换号"的前提），且**不能**被禁用
	stA, _ := p.Status("A")
	if !stA.Cooling {
		t.Fatalf("账号 A 应因流内 3004 被软冷却（Cooling=false）——否则重试仍会选中它")
	}
	if stA.Disabled {
		t.Fatalf("3004 是限流不是账号故障，不得禁用账号（Disabled=true）")
	}
	if stA.Reason == "" {
		t.Errorf("冷却原因应透出到面板（Reason 为空）")
	}

	// ③ B 不受影响
	stB, _ := p.Status("B")
	if stB.Cooling {
		t.Errorf("B 未中招，不应被冷却")
	}

	// --- 请求 2：A 冷却中 → 必须换到 B 并成功 ---
	up.mu.Lock()
	up.rateCode = 0 // B 健康，正常返回
	up.mu.Unlock()

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	if got := up.lastCall(); got != "B" {
		t.Fatalf("请求2 应换到 B（A 冷却中），实际命中 %q —— 若仍是 A 说明冷却没生效，重试必然再失败", got)
	}
	if rec2.Code != http.StatusOK {
		t.Fatalf("请求2 状态码=%d want 200\nbody=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "[DONE]") {
		t.Errorf("成功的流仍应有 [DONE]\nbody=%s", rec2.Body.String())
	}

	// ④ 走了一圈：A 冷、B 通，且 B 拿走了请求
	if up.callCount() < 2 {
		t.Fatalf("应发生 2 次上游调用，实际 %d 次", up.callCount())
	}
}

// TestStreamHardCreditUsesHardCooldown 1005（权益不足）走的是长冷却，不是软冷却。
func TestStreamHardCreditUsesHardCooldown(t *testing.T) {
	up := &streamErrUpstream{rateCode: 1005}
	h, p := newStreamErrHandler(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))

	st, _ := p.Status("A")
	if !st.Cooling {
		t.Fatalf("1005 权益不足应冷却账号 A")
	}
	// 软冷却 60s vs 硬冷却 12h：用剩余时长区分档位
	if remain := time.Until(st.Until); remain < time.Hour {
		t.Errorf("1005 应走硬冷却（12h），实际剩余 %v 更像软冷却", remain)
	}
}

// TestStreamNormalAnswerNotCooled 正常收尾不得误罚账号（防"修 A 坏 B"）。
func TestStreamNormalAnswerNotCooled(t *testing.T) {
	up := &streamErrUpstream{rateCode: 0} // 正常返回
	h, p := newStreamErrHandler(t, up)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	if rec.Code != http.StatusOK {
		t.Fatalf("正常请求状态码=%d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Errorf("正常流应有 [DONE]\nbody=%s", rec.Body.String())
	}
	st, _ := p.Status("A")
	if st.Cooling {
		t.Errorf("正常收尾不得冷却账号（Cooling=true，reason=%s）", st.Reason)
	}
}

// TestStreamRateLimitSingleAccountNotCooled 单号渠道（loomy/小浣熊等仅 1 个账号）的
// 流内限流**不冷却**：冷却 = 整渠道下线（无号可换），客户端已通过 SSE 收到上游原始
// 限流错误会自行退避重试，透传反而恢复更快（与非流式「单账号不罚账号」口径一致）。
// 这是二开与官方原版的分歧点，升级合并时必须保留本测试（官方原版此处会冷却）。
func TestStreamRateLimitSingleAccountNotCooled(t *testing.T) {
	up := &streamErrUpstream{rateCode: 3004}
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: "traework", AccessToken: "at-A", ExpiresAt: 4102444800, UID: "A"})
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.TraeWork: {Kind: provider.TraeWork, Pool: p, Upstream: up,
				StaticModels: []provider.ModelInfo{{ID: "m"}}},
		},
		APIKey:       "",
		HardCooldown: 12 * 3_600_000_000_000,
		SoftCooldown: 60_000_000_000,
		ErrThreshold: 3,
		ErrCooldown:  600_000_000_000,
	})

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	if got := up.lastCall(); got != "A" {
		t.Fatalf("单号渠道请求应命中唯一账号 A，实际 %q", got)
	}
	// 限流 error 帧照常透传给客户端（客户端据此自行退避重试）
	if !strings.Contains(rec1.Body.String(), "upstream_rate_limited") {
		t.Errorf("应透传限流 error 帧\nbody=%s", rec1.Body.String())
	}
	// 单号渠道：不冷却、不禁用（冷却=整渠道 503 窗口，透传反而能让客户端重试成功）
	st, _ := p.Status("A")
	if st.Cooling || st.Disabled {
		t.Fatalf("单号渠道流内限流不得冷却/禁用唯一账号（Cooling=%v Disabled=%v reason=%s）",
			st.Cooling, st.Disabled, st.Reason)
	}

	// 后续请求立即恢复可用（无需等冷却结束）
	up.mu.Lock()
	up.rateCode = 0
	up.mu.Unlock()
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	if got := up.lastCall(); got != "A" {
		t.Fatalf("请求2 应仍命中 A（未被冷却），实际 %q", got)
	}
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), "[DONE]") {
		t.Errorf("请求2 应正常成功\nbody=%s", rec2.Body.String())
	}
}

// TestStreamRateLimitSingleAccountFlagCools SingleAccount 渠道（oczen）官方已有完整豁免
// 语义（请求期透传）。本仓库的流内运行时单号豁免明确**不覆盖**它：带 SingleAccount
// 标志的渠道流内限流仍走官方冷却路径，行为与官方 v2.6.3 完全一致——运行时豁免只补
// 官方 SingleAccount 的盲区，不与已有语义重叠。
func TestStreamRateLimitSingleAccountFlagCools(t *testing.T) {
	up := &streamErrUpstream{rateCode: 3004}
	p := pool.New(t.TempDir() + "/state.json")
	p.Add(&auth.Auth{Kind: "traework", AccessToken: "at-A", ExpiresAt: 4102444800, UID: "A"})
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.TraeWork: {Kind: provider.TraeWork, Pool: p, Upstream: up,
				SingleAccount: true,
				StaticModels:  []provider.ModelInfo{{ID: "m"}}},
		},
		APIKey:       "",
		HardCooldown: 12 * 3_600_000_000_000,
		SoftCooldown: 60_000_000_000,
		ErrThreshold: 3,
		ErrCooldown:  600_000_000_000,
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody)))
	st, _ := p.Status("A")
	if !st.Cooling {
		t.Fatalf("SingleAccount 单号渠道流内限流应保持官方冷却行为（Cooling=%v reason=%s）",
			st.Cooling, st.Reason)
	}
}
