package modelscope

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// 粘贴登录的校验层（票 04）：规整 → 形状 → 上游校验 → uid 派生。
//
// 每条断言都对应一种「用户粘完之后无从自查」的失败形态，故文案必须能被
// 逐条 grep 到关键自查项（区域 / 绑阿里云 / 实名 / 重新生成）。

const fakeToken = "ms-fake-token-1234"

// TestNormalizeToken：粘贴容错。整行 `Authorization: Bearer ms-…`、引号、换行都要能吃掉。
func TestNormalizeToken(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  ms-abc  ", "ms-abc"},
		{"\"ms-abc\"", "ms-abc"},
		{"`ms-abc`", "ms-abc"},
		{"Bearer ms-abc", "ms-abc"},
		{"bearer ms-abc", "ms-abc"},
		{"Authorization: Bearer ms-abc", "ms-abc"},
		{"Authorization:Bearer ms-abc\n", "ms-abc"},
		{"\n\tms-abc\t\n", "ms-abc"},
		{"", ""},
	} {
		if got := NormalizeToken(tc.in); got != tc.want {
			t.Errorf("NormalizeToken(%q) = %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

// TestUIDForPerKind：两个 kind 各自独立服务，**同一枚 token** 在两边必须是不同 uid
// （否则面板按 uid 定位账号时两个渠道的操作会串到一起）。
func TestUIDForPerKind(t *testing.T) {
	cn := UIDFor(provider.ModelScope, fakeToken)
	ai := UIDFor(provider.ModelScopeAI, fakeToken)
	if !strings.HasPrefix(cn, "mst-") {
		t.Errorf("国内站 uid 前缀应为 mst-，得到 %q", cn)
	}
	if !strings.HasPrefix(ai, "msai-") {
		t.Errorf("国际站 uid 前缀应为 msai-，得到 %q", ai)
	}
	if cn == ai {
		t.Errorf("同一枚 token 在两个渠道生成了同一个 uid %q（两区必须各自成账号）", cn)
	}
	if cn != UIDFor(provider.ModelScope, fakeToken) {
		t.Error("UIDFor 必须稳定：同一 token 两次派生得到不同 uid")
	}
	if strings.Contains(cn, "fake-token") {
		t.Errorf("uid 不得携带 token 明文：%q", cn)
	}
	// 不同 token ⇒ 不同 uid（同一渠道内不得撞号）
	if got := UIDFor(provider.ModelScope, "ms-other-token"); got == cn {
		t.Errorf("不同 token 派生出同一 uid %q", got)
	}
}

// TestValidateTokenRejectsShape：「不是 ms- 形态」必须在本地就拦下并说明去哪儿生成。
func TestValidateTokenRejectsShape(t *testing.T) {
	c := New(provider.ModelScope)
	c.Base = "http://127.0.0.1:1" // 不应被访问：形状检查在发请求之前
	for _, tok := range []string{"", "   ", "wps_sid=V02xxx", "sk-abc"} {
		_, err := c.ValidateToken(tok)
		if err == nil {
			t.Fatalf("ValidateToken(%q) 应报错", tok)
		}
	}
	_, err := c.ValidateToken("wps_sid=V02xxx")
	if !strings.Contains(err.Error(), "myaccesstoken") {
		t.Errorf("形状错误文案必须给出生成地址，得到：%v", err)
	}
	// 空白字符（从表格里复制常带多余的列）也要可行动
	if _, err := c.ValidateToken("ms-abc def"); err == nil || !strings.Contains(err.Error(), "空白") {
		t.Errorf("含空白的 token 必须明确报「含空白字符」，得到：%v", err)
	}
}

// TestValidateTokenDeniedIsActionable：401/403 的文案必须同时点明
// 「区域不匹配 / 绑阿里云实名 / 重新生成」三条自查项，并带上游原文。
func TestValidateTokenDeniedIsActionable(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer "+fakeToken {
				t.Errorf("校验请求的 Authorization = %q，期望 Bearer %s", got, fakeToken)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"invalid token for this region"}}`))
		}))

		c := New(provider.ModelScopeAI)
		c.Base = srv.URL
		_, err := c.ValidateToken(fakeToken)
		srv.Close()
		if err == nil {
			t.Fatalf("HTTP %d 必须报错", status)
		}
		msg := err.Error()
		for _, want := range []string{"区域", "绑定阿里云账号", "实名", "myaccesstoken", DefaultBaseAI, "invalid token for this region"} {
			if !strings.Contains(msg, want) {
				t.Errorf("HTTP %d 的错误文案缺 %q：%s", status, want, msg)
			}
		}
	}
}

// TestValidateTokenEnvelopeDenied：上游偶发用 HTTP 200 + 错误信封表达鉴权失败。
func TestValidateTokenEnvelopeDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":401,"message":"unauthorized"}`))
	}))
	defer srv.Close()

	c := New(provider.ModelScope)
	c.Base = srv.URL
	_, err := c.ValidateToken(fakeToken)
	if err == nil || !strings.Contains(err.Error(), "区域") {
		t.Fatalf("200 + code:401 信封必须判成鉴权失败，得到：%v", err)
	}
}

// TestValidateTokenAcceptsModelsBody：正常目录（无 code 字段）不能被误判成鉴权失败。
func TestValidateTokenAcceptsModelsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"Qwen/Qwen3.5-122B-A10B"}]}`))
	}))
	defer srv.Close()

	for _, kind := range []provider.Kind{provider.ModelScope, provider.ModelScopeAI} {
		c := New(kind)
		c.Base = srv.URL
		uid, err := c.ValidateToken("  " + fakeToken + "  ")
		if err != nil {
			t.Fatalf("%s: 正常目录不应报错：%v", kind, err)
		}
		if uid != UIDFor(kind, fakeToken) {
			t.Errorf("%s: uid = %q，期望 %q（规整后派生）", kind, uid, UIDFor(kind, fakeToken))
		}
	}
}

// TestValidateTokenNonJSON：被代理/网关拦成 HTML 时必须直说，而不是报「目录解析失败」。
func TestValidateTokenNonJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><body>captive portal</body></html>`))
	}))
	defer srv.Close()

	c := New(provider.ModelScope)
	c.Base = srv.URL
	_, err := c.ValidateToken(fakeToken)
	if err == nil || !strings.Contains(err.Error(), "不是 JSON") {
		t.Fatalf("非 JSON 响应必须报「不是 JSON」，得到：%v", err)
	}
}

// TestDefaultNickname：默认昵称 = 渠道显示名 + token 尾 4 位（可改，见 app 侧的保留逻辑）。
func TestDefaultNickname(t *testing.T) {
	if got := DefaultNickname(provider.ModelScope, fakeToken); got != "ModelScope 1234" {
		t.Errorf("DefaultNickname(modelscope) = %q", got)
	}
	if got := DefaultNickname(provider.ModelScopeAI, fakeToken); got != "ModelScopeAI 1234" {
		t.Errorf("DefaultNickname(modelscopeai) = %q", got)
	}
	if got := DefaultNickname(provider.ModelScope, "ms-a"); got != "ModelScope ms-a" {
		t.Errorf("过短 token 不得越界取值，得到 %q", got)
	}
}
