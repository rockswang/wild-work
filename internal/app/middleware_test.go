package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wild-work/internal/config"
)

// mwState 从 /api/state 响应里解出 middleware 段。
func mwState(t *testing.T, mux *http.ServeMux) mwSection {
	t.Helper()
	var st struct {
		Middleware mwSection `json:"middleware"`
	}
	if err := json.Unmarshal(doReq(mux, "GET", "/api/state", "", nil).Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	return st.Middleware
}

// mwSection 与 State.Middleware 同构的解析目标。
type mwSection struct {
	Enabled bool   `json:"enabled"`
	BaseURL string `json:"base_url"`
}

// TestMiddlewareEndpointRoundTrip POST /api/config/middleware 往返：
// 保存 → 落盘 → /api/state 回显 → 热更新回调被触发 → 部分更新 → 关闭复位。
// 启用状态必须由「探测可达的中间层」承载（SetMiddleware 启用前探测），
// 故用 httptest 起真监听，不得依赖测试机上有真实中间层服务。
func TestMiddlewareEndpointRoundTrip(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	mux := panelMux(a)

	mwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`)) // 服务根返回状态即可（探测不读体）
	}))
	defer mwSrv.Close()

	var synced []config.Middleware
	a.SetMiddlewareSyncer(func(mw config.Middleware) { synced = append(synced, mw) })

	// 全量保存：base_url 带首尾空白（应被清洗）
	w := doReq(mux, "POST", "/api/config/middleware",
		`{"enabled":true,"base_url":" `+mwSrv.URL+` "}`, nil)
	if w.Code != 200 {
		t.Fatalf("保存应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(synced) != 1 || !synced[0].Enabled || synced[0].BaseURL != mwSrv.URL {
		t.Fatalf("热更新回调应收到清洗后的配置, got %+v", synced)
	}

	got := mwState(t, mux)
	if !got.Enabled || got.BaseURL != mwSrv.URL {
		t.Fatalf("state 回显异常: %+v", got)
	}

	// 落盘核对：config.json 里 middleware 段已持久化
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(a.cfg.StateFile), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"middleware"`) || !strings.Contains(string(raw), mwSrv.URL) {
		t.Fatalf("config.json 应含 middleware 段（含 base_url）: %s", raw)
	}

	// 部分更新：只带 base_url，enabled 保持现值（换一个活着的中间层）
	mwSrv2 := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer mwSrv2.Close()
	w = doReq(mux, "POST", "/api/config/middleware", `{"base_url":"`+mwSrv2.URL+`"}`, nil)
	if w.Code != 200 {
		t.Fatalf("部分更新应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(synced) != 2 || !synced[1].Enabled || synced[1].BaseURL != mwSrv2.URL {
		t.Fatalf("部分更新应触发热更新回调且不改 enabled, got %+v", synced)
	}
	got = mwState(t, mux)
	if !got.Enabled || got.BaseURL != mwSrv2.URL {
		t.Fatalf("部分更新不得覆盖未携带的 enabled: %+v", got)
	}

	// 关闭：全局直连，热更新回调同步
	w = doReq(mux, "POST", "/api/config/middleware", `{"enabled":false}`, nil)
	if w.Code != 200 {
		t.Fatalf("关闭应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(synced) != 3 || synced[2].Enabled {
		t.Fatalf("关闭后回调应收到 enabled=false, got %+v", synced)
	}
	if got = mwState(t, mux); got.Enabled {
		t.Fatal("关闭后 state 应显示 disabled")
	}
}

// TestMiddlewareEndpointGuard 会话守卫：设置密码后匿名访问 401，登录后可保存。
func TestMiddlewareEndpointGuard(t *testing.T) {
	a := newPanelApp(t, "0.0.0.0", "s3cret-pass")
	mux := panelMux(a)

	if w := doReq(mux, "POST", "/api/config/middleware", `{"enabled":true}`, nil); w.Code != 401 {
		t.Fatalf("匿名保存应 401，实际 %d", w.Code)
	}
	if w := doReq(mux, "GET", "/api/state", "", nil); w.Code != 401 {
		t.Fatalf("匿名 /api/state 应 401，实际 %d", w.Code)
	}
	ck := doReq(mux, "POST", "/api/auth/login", `{"password":"s3cret-pass"}`, nil).Result().Cookies()[0]
	mwSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer mwSrv.Close()
	if w := doReq(mux, "POST", "/api/config/middleware",
		`{"enabled":true,"base_url":"`+mwSrv.URL+`"}`, ck); w.Code != 200 {
		t.Fatalf("登录后保存应 200，实际 %d: %s", w.Code, w.Body.String())
	}
}

// TestMiddlewareEndpointValidation 校验判据：开启但缺基址 / 非法协议 → 400，
// 且失败保存不落盘、不触发热更新回调。
func TestMiddlewareEndpointValidation(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	mux := panelMux(a)
	var synced int
	a.SetMiddlewareSyncer(func(config.Middleware) { synced++ })

	for _, bad := range []string{
		`{"enabled":true}`,
		`{"enabled":true,"base_url":"127.0.0.1:8787"}`,
		`{"enabled":true,"base_url":"ftp://127.0.0.1:8787"}`,
	} {
		w := doReq(mux, "POST", "/api/config/middleware", bad, nil)
		if w.Code != 400 {
			t.Errorf("%s 应 400，实际 %d: %s", bad, w.Code, w.Body.String())
		}
	}
	if synced != 0 {
		t.Fatalf("失败保存不得触发热更新回调（%d 次）", synced)
	}
	// 失败保存不落盘：config.json 中不得出现 enabled:true 的中间层段
	raw, _ := os.ReadFile(filepath.Join(filepath.Dir(a.cfg.StateFile), "config.json"))
	if strings.Contains(string(raw), `"enabled": true`) {
		t.Fatalf("失败保存不得落盘: %s", raw)
	}
}

// TestMiddlewareEnableBlockedWhenServiceDown 启用前探测：未检测到中间层服务
// 就不允许启用（不落盘、不触发热更）；服务已启动（任意 HTTP 响应含 404）才放行；
// 已启用状态下服务中途挂掉后的部分更新同样被拒（最终态 enabled 必须服务可达）。
func TestMiddlewareEnableBlockedWhenServiceDown(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	mux := panelMux(a)
	var synced int
	a.SetMiddlewareSyncer(func(config.Middleware) { synced++ })

	// 死地址：起一个真监听再关掉，拿到「必然连接拒绝」的端口（不依赖测试机端口策略）
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL + "/mw"
	dead.Close()

	// 1) 未检测到服务 → 启用被拒：400 + 明确原因，且不落盘不触发热更
	w := doReq(mux, "POST", "/api/config/middleware",
		`{"enabled":true,"base_url":"`+deadURL+`"}`, nil)
	if w.Code != 400 {
		t.Fatalf("服务未启动时启用应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "未检测到中间层服务") {
		t.Fatalf("拒绝原因应指向服务未启动，实际: %s", w.Body.String())
	}
	if got := mwState(t, mux); got.Enabled {
		t.Fatal("被拒的启用不得落盘生效")
	}
	if synced != 0 {
		t.Fatalf("被拒的启用不得触发热更新回调（%d 次）", synced)
	}

	// 2) 服务已启动（根路径 404 也算「活」）→ 放行
	up := httptest.NewServer(http.NotFoundHandler()) // 任意 HTTP 响应均证明监听者存在
	defer up.Close()
	w = doReq(mux, "POST", "/api/config/middleware",
		`{"enabled":true,"base_url":"`+up.URL+`"}`, nil)
	if w.Code != 200 {
		t.Fatalf("服务已启动时启用应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if got := mwState(t, mux); !got.Enabled {
		t.Fatal("服务已启动时启用应生效")
	}
	if synced != 1 {
		t.Fatalf("成功启用应触发热更新回调，实际 %d 次", synced)
	}

	// 3) 已启用状态下服务挂掉：部分更新（只换 base_url）同样被拒——
	// 合并后的最终态 enabled 为真，保存前必须探测可达
	up.Close()
	w = doReq(mux, "POST", "/api/config/middleware", `{"base_url":"`+deadURL+`"}`, nil)
	if w.Code != 400 {
		t.Fatalf("已启用但服务已挂时保存应 400，实际 %d: %s", w.Code, w.Body.String())
	}
	if got := mwState(t, mux); !got.Enabled || got.BaseURL != up.URL {
		t.Fatalf("被拒的部分更新不得改现值: %+v", got)
	}
	if synced != 1 {
		t.Fatalf("被拒的部分更新不得触发热更新回调（%d 次）", synced)
	}

	// 4) 关闭操作不探测：服务挂着也能随时关掉
	w = doReq(mux, "POST", "/api/config/middleware", `{"enabled":false}`, nil)
	if w.Code != 200 {
		t.Fatalf("关闭不应被探测拦下，实际 %d: %s", w.Code, w.Body.String())
	}
}
