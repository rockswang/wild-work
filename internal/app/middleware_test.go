package app

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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
	Enabled  bool     `json:"enabled"`
	BaseURL  string   `json:"base_url"`
	Channels []string `json:"channels"`
}

// TestMiddlewareEndpointRoundTrip POST /api/config/middleware 往返：
// 保存 → 落盘 → /api/state 回显 → 热更新回调被触发 → 部分更新 → 关闭复位。
func TestMiddlewareEndpointRoundTrip(t *testing.T) {
	a := newPanelApp(t, "127.0.0.1", "")
	mux := panelMux(a)

	var synced []config.Middleware
	a.SetMiddlewareSyncer(func(mw config.Middleware) { synced = append(synced, mw) })

	// 全量保存：channels 带空白项（应被清洗排序）
	w := doReq(mux, "POST", "/api/config/middleware",
		`{"enabled":true,"base_url":" http://127.0.0.1:8787/bili ","channels":["workbuddy"," traework ",""]}`, nil)
	if w.Code != 200 {
		t.Fatalf("保存应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(synced) != 1 || !synced[0].Enabled || synced[0].BaseURL != "http://127.0.0.1:8787/bili" {
		t.Fatalf("热更新回调应收到清洗后的配置, got %+v", synced)
	}

	got := mwState(t, mux)
	if !got.Enabled || got.BaseURL != "http://127.0.0.1:8787/bili" {
		t.Fatalf("state 回显异常: %+v", got)
	}
	if !reflect.DeepEqual(got.Channels, []string{"traework", "workbuddy"}) {
		t.Fatalf("channels 应清洗排序, got %v", got.Channels)
	}

	// 落盘核对：config.json 里 middleware 段已持久化
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(a.cfg.StateFile), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"middleware"`) || !strings.Contains(string(raw), "http://127.0.0.1:8787/bili") {
		t.Fatalf("config.json 应含 middleware 段（含 base_url）: %s", raw)
	}

	// 部分更新：只带 channels，其余保持现值
	w = doReq(mux, "POST", "/api/config/middleware", `{"channels":["glm","qoder"]}`, nil)
	if w.Code != 200 {
		t.Fatalf("部分更新应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	got = mwState(t, mux)
	if !got.Enabled || got.BaseURL != "http://127.0.0.1:8787/bili" {
		t.Fatalf("部分更新不得覆盖未携带字段: %+v", got)
	}
	if !reflect.DeepEqual(got.Channels, []string{"glm", "qoder"}) {
		t.Fatalf("部分更新后 channels 异常: %v", got.Channels)
	}

	// 关闭：全部渠道直连，热更新回调同步
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
	if w := doReq(mux, "POST", "/api/config/middleware",
		`{"enabled":true,"base_url":"http://127.0.0.1:8787","channels":["glm"]}`, ck); w.Code != 200 {
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
