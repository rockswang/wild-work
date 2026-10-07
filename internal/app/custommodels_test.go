package app

// custommodels_test.go 自定义模型管理端点（/api/custommodels）往返：
// 走真实的 App 装配（New + HandleAPI，等价 main.go 的 AttachAPI），
// 去掉端点即全红。全程临时目录，不碰真实网络。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/custommodels"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// newCustomApp 构造带自定义模型 Store 的最小 App（其余同 newPanelApp）。
// 返回 (App, 管理 mux, 数据目录)——目录用于验证端点确实落盘到 data/custom-models.json。
func newCustomApp(t *testing.T) (*App, *http.ServeMux, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir
	cfg.Listen = config.Listen{Host: "127.0.0.1", Port: 7863}

	p := pool.New(filepath.Join(dir, "state-workbuddy.json"))
	p.Add(&auth.Auth{Kind: "workbuddy", UID: "wb-1", AccessToken: "t", RefreshToken: "r"})

	store, err := custommodels.Load(dir)
	if err != nil {
		t.Fatalf("custommodels.Load: %v", err)
	}
	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: p, Upstream: &fakeUpstream{remain: 10}},
		},
		Custom: store,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a, panelMux(a), dir
}

// postAction POST 一条 action 请求（无 cookie，配合本文件其余场景默认无密码）。
func postAction(mux *http.ServeMux, body string) *httptest.ResponseRecorder {
	return doReq(mux, http.MethodPost, "/api/custommodels", body, nil)
}

// TestCustomModelsEndpointsCRUD 增删改查往返 + 持久化落盘验证。
func TestCustomModelsEndpointsCRUD(t *testing.T) {
	_, mux, dir := newCustomApp(t)

	// GET 初始为空
	w := doReq(mux, http.MethodGet, "/api/custommodels", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET 初始 = %d", w.Code)
	}
	var snap struct {
		Sources []custommodels.Source `json:"sources"`
		Models  []custommodels.Model  `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("解析初始快照: %v", err)
	}
	if len(snap.Sources) != 0 || len(snap.Models) != 0 {
		t.Fatalf("初始应为空表，实际 %d 源 %d 模型", len(snap.Sources), len(snap.Models))
	}

	// 增源
	w = postAction(mux, `{"action":"upsert_source","source":{"name":"ds","base_url":"https://api.example.com/","api_key":"dummy-key","enabled":true}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("upsert_source = %d: %s", w.Code, w.Body.String())
	}

	// 增模型：源不存在 → 400 且消息可读
	w = postAction(mux, `{"action":"upsert_model","model":{"name":"m1","source":"ghost","enabled":true}}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "不存在") {
		t.Fatalf("引用不存在源应 400，实际 %d: %s", w.Code, w.Body.String())
	}

	// 增模型：合法；保存响应应带回最新快照
	w = postAction(mux, `{"action":"upsert_model","model":{"name":"ds-chat","source":"ds","upstream_id":"ds-real","enabled":true,"note":"测试"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("upsert_model = %d: %s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("解析保存后快照: %v", err)
	}
	if len(snap.Sources) != 1 || snap.Sources[0].BaseURL != "https://api.example.com" {
		t.Fatalf("保存响应应含归一后的源: %+v", snap.Sources)
	}
	if len(snap.Models) != 1 || snap.Models[0].Name != "ds-chat" || snap.Models[0].UpstreamID != "ds-real" {
		t.Fatalf("保存响应应含新模型: %+v", snap.Models)
	}

	// 持久化往返：端点写的是 data/custom-models.json，重载可命中
	s2, err := custommodels.Load(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if _, ok := s2.Resolve("ds-chat"); !ok {
		t.Fatal("端点写入应持久化到磁盘并可重载命中")
	}

	// 改模型（停用）→ 重载后不再命中
	w = postAction(mux, `{"action":"upsert_model","model":{"name":"ds-chat","source":"ds","enabled":false}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("upsert_model 改 = %d: %s", w.Code, w.Body.String())
	}
	s3, err := custommodels.Load(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if _, ok := s3.Resolve("ds-chat"); ok {
		t.Fatal("停用后不应命中")
	}

	// 删源（被引用 → 拒绝；先删模型 → 成功）
	w = postAction(mux, `{"action":"delete_source","name":"ds"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "引用") {
		t.Fatalf("被引用的源删除应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	w = postAction(mux, `{"action":"delete_model","name":"ds-chat"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete_model = %d: %s", w.Code, w.Body.String())
	}
	w = postAction(mux, `{"action":"delete_source","name":"ds"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("delete_source = %d: %s", w.Code, w.Body.String())
	}
	s4, err := custommodels.Load(dir)
	if err != nil {
		t.Fatalf("重载: %v", err)
	}
	if srcs, mods := s4.Snapshot(); len(srcs) != 0 || len(mods) != 0 {
		t.Fatalf("删空后应为空表，实际 %d/%d", len(srcs), len(mods))
	}

	// 未知 action → 400
	w = postAction(mux, `{"action":"bogus"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("未知 action 应 400，实际 %d", w.Code)
	}
}

// TestCustomModelsDisabledWithoutStore 未装配 Store（nil）时端点回 disabled/400，不 panic。
func TestCustomModelsDisabledWithoutStore(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	mux := panelMux(a)

	w := doReq(mux, http.MethodGet, "/api/custommodels", "", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "disabled") {
		t.Fatalf("无 Store 时 GET 应回 disabled，实际 %d: %s", w.Code, w.Body.String())
	}
	w = postAction(mux, `{"action":"upsert_source","source":{"name":"x","base_url":"https://a.com","enabled":true}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("无 Store 时 POST 应 400，实际 %d", w.Code)
	}
}

// TestCustomModelsGuardedBySession R4：开管理密码后 /api/custommodels 随 /api/* 受会话守卫。
func TestCustomModelsGuardedBySession(t *testing.T) {
	a := newPanelApp(t, "0.0.0.0", "s3cret-pass")
	mux := panelMux(a)

	if w := doReq(mux, http.MethodGet, "/api/custommodels", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("匿名 GET 应 401，实际 %d", w.Code)
	}
	// 登录后可访问
	w := doReq(mux, http.MethodPost, "/api/auth/login", `{"password":"s3cret-pass"}`, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("登录 = %d", w.Code)
	}
	var ck *http.Cookie
	for _, c := range w.Result().Cookies() {
		ck = c
	}
	if ck == nil {
		t.Fatal("登录应下发会话 cookie")
	}
	if w := doReq(mux, http.MethodGet, "/api/custommodels", "", ck); w.Code != http.StatusOK {
		t.Fatalf("登录后 GET 应 200，实际 %d", w.Code)
	}
}
