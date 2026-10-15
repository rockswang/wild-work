// model_id_contract_test.go 锁死 Cline 渠道「上游命名空间直通」的对外模型 ID 契约（决议 R59）：
//
//   - /v1/models 只广告上游 ID 原样（cline-free/*），不再拼 cline/ 前缀；
//   - 路由同时接受新形态 cline-free/* 与旧形态 cline/cline-free/*（老配置兼容），
//     两种写法必须解析到同一 (渠道, 上游模型名)；
//   - 降级链规则里的 from 两种写法都能命中同一请求。
//
// 为什么单独一个文件：这条规则此前分散在「服务端拼接 + 面板两处拼接」，R52 就是漏了一处；
// R59 把判据收敛到 provider 之后，这里锁住端到端行为，避免将来又有人只改一半。
package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// newClineHandler 构造一个只有 cline 渠道（含一个账号）的 Handler。
func newClineHandler(models ...string) *Handler {
	p := pool.New("")
	p.Add(&auth.Auth{Kind: "cline", UID: "c1", AccessToken: "sk_test", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	infos := make([]provider.ModelInfo, 0, len(models))
	for _, m := range models {
		infos = append(infos, provider.ModelInfo{ID: m})
	}
	rt := &Runtime{Kind: provider.Cline, Pool: p, Upstream: echoUpstream{}, StaticModels: infos}
	return NewHandler(Config{Runtimes: map[provider.Kind]*Runtime{provider.Cline: rt}})
}

// /v1/models 对外只给上游 ID 原样（含命名空间），且 owned_by 仍是渠道 kind。
func TestClineModelsExposeUpstreamIDVerbatim(t *testing.T) {
	h := newClineHandler("cline-free/mimo-v2.6-flash", "cline-free/muse-spark-1.3-contributor")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /v1/models = %d", rec.Code)
	}
	var resp struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range resp.Data {
		got[m.ID] = m.OwnedBy
	}
	for _, want := range []string{"cline-free/mimo-v2.6-flash", "cline-free/muse-spark-1.3-contributor"} {
		if _, ok := got[want]; !ok {
			t.Errorf("应广告上游 ID 原样 %q，实得 %v", want, keysOf(got))
		}
	}
	if _, ok := got["cline/cline-free/mimo-v2.6-flash"]; ok {
		t.Error("不应再拼渠道前缀（旧双层路径只作路由兼容，不对外广告）")
	}
	if got["cline-free/mimo-v2.6-flash"] != "cline" {
		t.Errorf("owned_by 应为渠道 kind cline，实得 %q", got["cline-free/mimo-v2.6-flash"])
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// 两种写法都必须路由到同一 (渠道, 上游模型名)。
func TestClineModelIDBothFormsRoute(t *testing.T) {
	h := newClineHandler("cline-free/mimo-v2.6-flash")
	cases := []struct {
		in       string
		upstream string
	}{
		{"cline-free/mimo-v2.6-flash", "cline-free/mimo-v2.6-flash"},       // 新形态：整串即上游名
		{"cline/cline-free/mimo-v2.6-flash", "cline-free/mimo-v2.6-flash"}, // 旧形态：剥一层
	}
	for _, c := range cases {
		rt, model, err := h.runtimeForModel(c.in)
		if err != nil {
			t.Fatalf("runtimeForModel(%q) 失败：%v", c.in, err)
		}
		if rt.Kind != provider.Cline || model != c.upstream {
			t.Errorf("runtimeForModel(%q) = (%s, %q); want (cline, %q)", c.in, rt.Kind, model, c.upstream)
		}
	}
}

// 端到端：旧写法请求也能拿到 200，且发给上游的 model 是 cline-free/*（不是双层路径）。
func TestClineLegacyFormEndToEnd(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{Kind: "cline", UID: "c1", AccessToken: "sk_test", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	rec := &modelRecorderUpstream{ch: make(chan string, 1)}
	rt := &Runtime{Kind: provider.Cline, Pool: p, Upstream: rec}
	h := NewHandler(Config{Runtimes: map[provider.Kind]*Runtime{provider.Cline: rt}})

	hit := httptest.NewRecorder()
	body := `{"model":"cline/cline-free/mimo-v2.6-flash","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	h.ServeHTTP(hit, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if hit.Code != 200 {
		t.Fatalf("旧写法应可用，实得 %d %s", hit.Code, hit.Body.String())
	}
	select {
	case got := <-rec.ch:
		if got != "cline-free/mimo-v2.6-flash" {
			t.Errorf("发给上游的 model 应为 cline-free/*，实得 %q", got)
		}
	default:
		t.Fatal("上游未被调用")
	}

	// 新写法：同一渠道、同一上游名
	rec2 := &modelRecorderUpstream{ch: make(chan string, 1)}
	rt2 := &Runtime{Kind: provider.Cline, Pool: p, Upstream: rec2}
	h2 := NewHandler(Config{Runtimes: map[provider.Kind]*Runtime{provider.Cline: rt2}})
	body2 := `{"model":"cline-free/mimo-v2.6-flash","stream":false,"messages":[{"role":"user","content":"hi"}]}`
	hit2 := httptest.NewRecorder()
	h2.ServeHTTP(hit2, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body2)))
	if hit2.Code != 200 {
		t.Fatalf("新写法应可用，实得 %d %s", hit2.Code, hit2.Body.String())
	}
	if got := <-rec2.ch; got != "cline-free/mimo-v2.6-flash" {
		t.Errorf("新写法发给上游的 model 也应为 cline-free/*，实得 %q", got)
	}
}

// modelRecorderUpstream 记下 ChatStream 收到的 body 里的 model 字段 —— 那才是真正发给上游的模型名
// （handler 会先用 rewriteModel 把客户端请求名改写成解析后的上游名）。
// 其余方法靠内嵌 echoUpstream 提供，只需覆写 ChatStream。
type modelRecorderUpstream struct {
	echoUpstream
	ch chan string
}

func (u *modelRecorderUpstream) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	var obj struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &obj)
	select {
	case u.ch <- obj.Model:
	default:
	}
	return u.echoUpstream.ChatStream(a, body)
}
