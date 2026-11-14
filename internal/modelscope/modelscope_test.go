package modelscope

import (
	"context"
	"net/http"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// TestBaseURLByKind 两区是独立服务：基址必须按 kind 二选一，绝不互相兜底
// （token 不通用，兜底只会换来一个误导性的 401）。
func TestBaseURLByKind(t *testing.T) {
	if got := BaseURL(provider.ModelScope); got != DefaultBaseCN {
		t.Errorf("BaseURL(modelscope) = %q，期望 %q", got, DefaultBaseCN)
	}
	if got := BaseURL(provider.ModelScopeAI); got != DefaultBaseAI {
		t.Errorf("BaseURL(modelscopeai) = %q，期望 %q", got, DefaultBaseAI)
	}
}

// TestNewRequestCarriesBearerToken 凭据模型是「一枚 ms- token = 一个账号」，
// 出站鉴权必须是 Authorization: Bearer <token>；基址仍按 kind 选。
func TestNewRequestCarriesBearerToken(t *testing.T) {
	const tok = "ms-test-token"
	for _, tc := range []struct {
		kind provider.Kind
		want string
	}{
		{provider.ModelScope, DefaultBaseCN + "/v1/models"},
		{provider.ModelScopeAI, DefaultBaseAI + "/v1/models"},
	} {
		c := New(tc.kind)
		req, err := c.newRequest(context.Background(), http.MethodGet, "/v1/models", &auth.Auth{AccessToken: tok}, nil)
		if err != nil {
			t.Fatalf("%s: newRequest: %v", tc.kind, err)
		}
		if got := req.URL.String(); got != tc.want {
			t.Errorf("%s: URL = %q，期望 %q", tc.kind, got, tc.want)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer "+tok {
			t.Errorf("%s: Authorization = %q，期望 %q", tc.kind, got, "Bearer "+tok)
		}
	}

	// Base 覆盖（测试注入 httptest 假上游）必须生效，且去尾斜杠。
	c := New(provider.ModelScope)
	c.Base = "http://127.0.0.1:1234/"
	req, err := c.newRequest(context.Background(), http.MethodPost, "/v1/chat/completions", nil, []byte(`{}`))
	if err != nil {
		t.Fatalf("newRequest: %v", err)
	}
	if got, want := req.URL.String(), "http://127.0.0.1:1234/v1/chat/completions"; got != want {
		t.Errorf("覆盖 Base 后 URL = %q，期望 %q", got, want)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("无凭据时不应发 Authorization，得到 %q", got)
	}
}

// TestStaticModelsMinimalCatalog 静态目录最小集：至少一款对话模型 + 一款生图模型，
// 且生图模型经 IsImageModel 可判（票 02/05 的对话路由与生图路由都靠这一处判据）。
// 能力字段一律不声明（R49：上游没说过的话不得替它说）。
func TestStaticModelsMinimalCatalog(t *testing.T) {
	ms := StaticModels()
	if len(ms) < 2 {
		t.Fatalf("静态目录条目过少：%d", len(ms))
	}
	var chat, image int
	for _, m := range ms {
		if m.ID == "" {
			t.Error("静态条目 ID 不得为空")
		}
		if m.SupportsTools || m.SupportsReasoning || m.ContextWindow != 0 {
			t.Errorf("%s 声明了未经验证的能力（R49 禁止编造）: %+v", m.ID, m)
		}
		if IsImageModel(m.ID) {
			image++
		} else {
			chat++
		}
	}
	if chat == 0 {
		t.Error("静态目录缺对话模型")
	}
	if image == 0 {
		t.Error("静态目录缺生图模型（image:true）")
	}
	// 「能看图」与「能出图」是两个维度：视觉理解模型不得被标成生图模型。
	if IsImageModel("Qwen/Qwen2.5-VL-72B-Instruct") {
		t.Error("视觉理解模型不得标 image:true（SupportsImages ≠ 生图）")
	}
}

// TestNoCreditsAreZeroWithoutError 额度口径「不适用」（D5）：上游无余额端点，
// 必须回 (0, nil) 而不是报错——报错会被面板渲染成「刷新积分失败」。
func TestNoCreditsAreZeroWithoutError(t *testing.T) {
	c := New(provider.ModelScope)
	if n, err := c.UserResource(nil); n != 0 || err != nil {
		t.Errorf("UserResource = (%d, %v)，期望 (0, nil)", n, err)
	}
	if n, items, err := c.UserResourceDetail(nil); n != 0 || items != nil || err != nil {
		t.Errorf("UserResourceDetail = (%d, %v, %v)，期望 (0, nil, nil)", n, items, err)
	}
	if err := c.RefreshToken(nil); err != nil {
		t.Errorf("RefreshToken 应为空实现，得到 %v", err)
	}
	if err := c.DailyCheckin(nil); err != nil {
		t.Errorf("DailyCheckin 应为空实现，得到 %v", err)
	}
}
