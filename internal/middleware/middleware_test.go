package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestRewriteFormAgainstBiliswitch 锁定拼接形态：与生产环境 biliswitch.Wrap
// （prefix = TrimRight(TrimSpace(base), "/") + "/bili/"；wrapped = prefix + 原完整URL）
// 完全同构——差异只在专属路径段 "/bili" 由 base_url 自带，本包只补分隔符 "/"。
func TestRewriteFormAgainstBiliswitch(t *testing.T) {
	// biliswitch.Wrap("http://127.0.0.1:8787", "https://api.trae.cn/v1/chat?x=1")
	// = "http://127.0.0.1:8787/bili/https://api.trae.cn/v1/chat?x=1"
	// 本包等价写法：base_url 直接带 "/bili" 路径段。
	const target = "https://api.trae.cn/v1/chat/completions?x=1&y=%20z"
	cases := []struct {
		name string
		base string
		want string
	}{
		{"基址带路径段（bili 同款形态）", "http://127.0.0.1:8787/bili", "http://127.0.0.1:8787/bili/" + target},
		{"基址尾部斜杠归一", "http://127.0.0.1:8787/bili/", "http://127.0.0.1:8787/bili/" + target},
		{"基址两侧空白归一", "  http://127.0.0.1:8787/bili  ", "http://127.0.0.1:8787/bili/" + target},
		{"根路径基址", "http://127.0.0.1:8787", "http://127.0.0.1:8787/" + target},
		{"https 基址", "https://mw.example.com/prefix", "https://mw.example.com/prefix/" + target},
	}
	for _, c := range cases {
		if got := Rewrite(c.base, target); got != c.want {
			t.Errorf("%s: Rewrite(%q) = %q, want %q", c.name, c.base, got, c.want)
		}
	}
	// query 原样保留（含已转义序列）
	if got, want := Rewrite("http://127.0.0.1:8787/bili", "https://h.example/a%20b?q=1"), "http://127.0.0.1:8787/bili/https://h.example/a%20b?q=1"; got != want {
		t.Errorf("转义序列应原样保留: %q, want %q", got, want)
	}
}

// TestRewriteIdempotent 幂等与边界：空 target 原样返回；已含前缀原样返回（防双重包裹）。
func TestRewriteIdempotent(t *testing.T) {
	if got := Rewrite("http://127.0.0.1:8787/bili", ""); got != "" {
		t.Errorf("空 target 应原样返回, got %q", got)
	}
	wrapped := Rewrite("http://127.0.0.1:8787/bili", "https://api.trae.cn/v1/chat?x=1")
	if got := Rewrite("http://127.0.0.1:8787/bili", wrapped); got != wrapped {
		t.Errorf("已含前缀应原样返回（幂等）: %q, want %q", got, wrapped)
	}
	if !Prefixed("http://127.0.0.1:8787/bili", wrapped) {
		t.Error("Prefixed 应识别已包裹形态")
	}
	if Prefixed("http://127.0.0.1:8787/bili", "https://api.trae.cn/v1/chat") {
		t.Error("Prefixed 不应误报未包裹形态")
	}
}

// TestRoundTripRewritesAndPassesResponse 端到端：请求经包装器改写后落到中间层
// httptest 服务（路径 = 前缀 + 原完整 URL、query 保留、Host 跟随基址），响应原样透传。
func TestRoundTripRewritesAndPassesResponse(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotQuery, gotHost, gotRequestURI string
	mws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotQuery, gotHost, gotRequestURI = r.URL.Path, r.URL.RawQuery, r.Host, r.RequestURI
		mu.Unlock()
		w.Header().Set("X-Mw-Marker", "via-middleware")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`) // 中间层转发上游后的响应体
	}))
	defer mws.Close()

	base := mws.URL + "/bili" // 中间层基址（带路径段，bili 同款形态）
	client := &http.Client{Transport: Wrap(base, &http.Transport{})}
	resp, err := client.Get("https://upstream.example/v1/chat/completions?x=1&y=2")
	if err != nil {
		t.Fatalf("请求经中间层应成功: %v", err)
	}
	defer resp.Body.Close()

	if gotPath != "/bili/https://upstream.example/v1/chat/completions" {
		t.Errorf("中间层收到的路径应 = 前缀 + 原完整URL, got %q", gotPath)
	}
	// 请求行（RequestURI）= 改写后完整 URL 的 path?query 形态
	if gotRequestURI != "/bili/https://upstream.example/v1/chat/completions?x=1&y=2" {
		t.Errorf("请求行应携带改写后的完整 URL（含 query）, got %q", gotRequestURI)
	}
	if gotQuery != "x=1&y=2" {
		t.Errorf("query 应原样保留, got %q", gotQuery)
	}
	if gotHost != mws.URL[len("http://"):] {
		t.Errorf("Host 头应跟随中间层基址（同 biliswitch 字符串拼接语义）, got %q, want %q", gotHost, mws.URL[len("http://"):])
	}
	if resp.Header.Get("X-Mw-Marker") != "via-middleware" {
		t.Error("响应头应原样透传")
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Errorf("响应体应原样透传, got %q", body)
	}
}

// TestRoundTripPrefixedPassthrough 已带前缀的请求不再二次包裹（幂等）。
func TestRoundTripPrefixedPassthrough(t *testing.T) {
	var mu sync.Mutex
	var gotRequestURI string
	mws := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotRequestURI = r.RequestURI
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	defer mws.Close()

	base := mws.URL + "/bili"
	client := &http.Client{Transport: Wrap(base, &http.Transport{})}
	// 请求 URL 已含前缀（如中间层 30x 回跳）：不得再包一层
	resp, err := client.Get(base + "/https://upstream.example/v1/chat")
	if err != nil {
		t.Fatalf("已包裹请求应透传: %v", err)
	}
	defer resp.Body.Close()
	if gotRequestURI != "/bili/https://upstream.example/v1/chat" {
		t.Errorf("已带前缀的请求不得二次包裹, got %q", gotRequestURI)
	}
}

// TestWrapReplacesNotNests Wrap 幂等替换（不嵌套）+ Unwrap 边界。
func TestWrapReplacesNotNests(t *testing.T) {
	tr := &http.Transport{}
	w1 := Wrap("http://127.0.0.1:8787/bili", tr)
	if Unwrap(w1) != tr {
		t.Error("Unwrap(包装器) 应返回原底座")
	}
	// 换基址热更新：旧壳被替换而非嵌套（剥一层即到底座）
	w2 := Wrap("http://127.0.0.1:9999/", w1)
	if Unwrap(w2) != tr {
		t.Error("重复 Wrap 应替换旧壳（不嵌套），Unwrap 一层即原底座")
	}
	// 非本包包装器原样返回
	if Unwrap(tr) != tr {
		t.Error("Unwrap(非包装器) 应原样返回")
	}
	// nil 底座回退默认 Transport（与 http.Client 的 nil 语义一致）
	if Unwrap(Wrap("http://127.0.0.1:8787", nil)) != http.DefaultTransport {
		t.Error("Wrap(base, nil) 应回退 http.DefaultTransport")
	}
}
