package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/config"
	"wild-work/internal/custom"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

func TestCustomChannelsAPIAndHotReload(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)

	a, err := New(Options{
		ConfigPath: cfgPath,
		Config:     cfg,
		Runtimes:   map[provider.Kind]*Runtime{},
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	var synced []custom.ChannelConfig
	a.SetCustomChannelsSyncer(func(channels []custom.ChannelConfig) {
		synced = channels
	})

	mux := http.NewServeMux()
	a.HandleAPI(mux)

	// 1. 测试 POST /api/config/custom_channel_test（假上游）
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"step-5-preview"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	testReq := map[string]any{
		"name":     "stepfun",
		"base_url": ts.URL,
		"api_key":  "test-key",
	}
	rawTest, _ := json.Marshal(testReq)
	req := httptest.NewRequest("POST", "/api/config/custom_channel_test", bytes.NewReader(rawTest))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("custom_channel_test status %d: %s", w.Code, w.Body.String())
	}
	var testResp struct {
		OK     bool     `json:"ok"`
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &testResp); err != nil || !testResp.OK {
		t.Fatalf("expected ok true with models, got: %s", w.Body.String())
	}
	if len(testResp.Models) != 1 || testResp.Models[0] != "step-5-preview" {
		t.Errorf("unexpected models: %v", testResp.Models)
	}

	// 2. 测试 POST /api/config/custom_channels 保存配置与触发回调
	saveReq := map[string]any{
		"channels": []map[string]any{
			{
				"name":     "stepfun",
				"base_url": ts.URL,
				"api_key":  "key1",
				"models":   []string{"step-5-preview"},
			},
		},
	}
	rawSave, _ := json.Marshal(saveReq)
	reqSave := httptest.NewRequest("POST", "/api/config/custom_channels", bytes.NewReader(rawSave))
	wSave := httptest.NewRecorder()
	mux.ServeHTTP(wSave, reqSave)
	if wSave.Code != 200 {
		t.Fatalf("custom_channels save status %d: %s", wSave.Code, wSave.Body.String())
	}
	if len(synced) != 1 || synced[0].Name != "stepfun" {
		t.Fatalf("expected custom channels syncer called with stepfun, got: %+v", synced)
	}

	// 3. 模拟 main.go 装配 custom runtimes 并测试 accountViews
	p := pool.New(tmpDir + "/state-custom.json")
	cClient := custom.New(synced[0])
	a.UpdateCustomRuntimes(map[provider.Kind]*Runtime{
		provider.Kind("stepfun"): {
			Kind:     provider.Kind("stepfun"),
			Pool:     p,
			Upstream: cClient,
			Custom:   true,
		},
	})

	if !a.creditsNAKind("stepfun") {
		t.Errorf("expected stepfun to be CreditsNA")
	}
	if !a.noExplicitCheckinKind(provider.Kind("stepfun")) {
		t.Errorf("expected stepfun to be noExplicitCheckinKind")
	}

	// 4. 测试 GET /api/state 包含 custom_channels（api_key 必须脱敏）
	st := a.GetState()
	if len(st.CustomChannels) != 1 || st.CustomChannels[0].Name != "stepfun" {
		t.Errorf("expected CustomChannels in state, got %+v", st.CustomChannels)
	}
	if st.CustomChannels[0].APIKey == "test-key" {
		t.Errorf("api_key 应以脱敏形式回显，不得明文送进面板载荷：%q", st.CustomChannels[0].APIKey)
	}
}

// TestCustomChannelKeyMaskedRoundTrip 验证 api_key 的「脱敏回显 → 原样保存 → 真实密钥不丢」链路。
// 面板回显的是脱敏值（含 …），用户没改动该渠道时提交回来的仍是脱敏串；
// 若原样落盘就把真实密钥换成了废值，下次请求全部 401。
func TestCustomChannelKeyMaskedRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	if err := a.SetCustomChannels([]custom.ChannelConfig{
		{Name: "stepfun", BaseURL: "https://api.stepfun.com/v1", APIKey: "sk-real-secret-1234"},
	}); err != nil {
		t.Fatalf("SetCustomChannels: %v", err)
	}

	// 模拟面板：取脱敏回显值，不改动直接回传（真实前端的常见路径）
	shown := a.GetState().CustomChannels
	if shown[0].APIKey == "sk-real-secret-1234" || !strings.Contains(shown[0].APIKey, "…") {
		t.Fatalf("回显应为脱敏形态：%q", shown[0].APIKey)
	}
	if err := a.SetCustomChannels(shown); err != nil {
		t.Fatalf("回存脱敏值应成功（内部找回真实密钥）: %v", err)
	}
	// 落盘与内存里的真实密钥必须原样保留
	if got := a.runtime(provider.Kind("stepfun")); got != nil {
		// runtime 由 syncer 装配，此测试未注入 syncer，跳过
		_ = got
	}
	saved, _ := config.Load(cfgPath)
	if len(saved.CustomChannels) != 1 || saved.CustomChannels[0].APIKey != "sk-real-secret-1234" {
		t.Fatalf("真实密钥被脱敏串覆盖了：%+v", saved.CustomChannels)
	}

	// 反向：显式传新密钥应当生效
	if err := a.SetCustomChannels([]custom.ChannelConfig{
		{Name: "stepfun", BaseURL: "https://api.stepfun.com/v1", APIKey: "sk-new-key-9999"},
	}); err != nil {
		t.Fatalf("SetCustomChannels(new key): %v", err)
	}
	saved2, _ := config.Load(cfgPath)
	if saved2.CustomChannels[0].APIKey != "sk-new-key-9999" {
		t.Fatalf("显式新密钥未生效：%+v", saved2.CustomChannels)
	}
}

// TestFeesOrderIncludesDynamicChannels 验证费率表会展示动态（自定义）渠道。
// feeChannelOrder 是静态表，若不把 modelsByKind 里的未登记渠道追加进去，
// 面板费率表永远不出该渠道 tab（R66）。
func TestFeesOrderIncludesDynamicChannels(t *testing.T) {
	models := map[provider.Kind][]provider.ModelInfo{
		provider.WorkBuddy:       {{ID: "glm-5.2"}},
		provider.Kind("stepfun"): {{ID: "step-5-preview"}},
		provider.Kind("acustom"): {{ID: "x"}}, // 验证排序稳定
	}
	order := feeChannelOrderWithDynamic(models)
	last := order[len(order)-3:]
	want := []provider.Kind{provider.Kind("acustom"), provider.Kind("stepfun")}
	// 静态表末尾之後应接按名排序的动态渠道
	if len(order) != len(feeChannelOrder)+2 {
		t.Fatalf("want %d channels, got %d: %v", len(feeChannelOrder)+2, len(order), order)
	}
	if last[1] != want[0] || last[2] != want[1] {
		t.Errorf("动态渠道应追加到末尾且按名排序，got tail %v", last)
	}
	// 无动态渠道时原样返回静态表
	if got := feeChannelOrderWithDynamic(map[provider.Kind][]provider.ModelInfo{provider.WorkBuddy: {{ID: "x"}}}); len(got) != len(feeChannelOrder) {
		t.Errorf("无动态渠道时应返回静态表，got %d", len(got))
	}
}

// TestRuntimeTableConcurrentAccess 并发安全：保存自定义渠道（写 runtimes 表）与
// 后台统计喂入/面板取数（读 runtimes 表）并发时不得触发
// `fatal error: concurrent map read and map write`（不可 recover，daemon 直接死）。
// 原始实现里 UpdateCustomRuntimes 在 a.mu 下写 map，而 allStatuses/runtime 等
// 9 处读点全程不加锁 —— 这是本仓首个 post-init 写（此前 runtimes 只在 init 赋值）。
func TestRuntimeTableConcurrentAccess(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	mk := func(name string) map[provider.Kind]*Runtime {
		return map[provider.Kind]*Runtime{
			provider.Kind(name): {
				Kind:   provider.Kind(name),
				Pool:   pool.New(tmpDir + "/state-" + name + ".json"),
				Custom: true,
			},
		}
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// 写：反复替换自定义渠道集合（模拟面板反复保存）
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			a.UpdateCustomRuntimes(mk([]string{"a", "b", "c"}[i%3]))
		}
	}()
	// 读：并发遍历运行时表（模拟 30s 统计喂入器与 /api/state）
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = a.allStatuses()
				_ = a.runtimesSnapshot()
				_ = a.runtime(provider.Kind("a"))
				_ = a.creditsNAKind("a")
			}
		}()
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestCustomChannelTestEndpointResolvesMaskedKey 面板「测试连通」必须能在 api_key 为
// 脱敏回显态时用真实密钥（否则未改动过的渠道测试恒 401，同 #117 的探针不同源问题）。
// 端点在收到含 「…」 的 key 时按 name 从配置找回真实密钥。
func TestCustomChannelTestEndpointResolvesMaskedKey(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = tmpDir + "/state.json"
	cfgPath := tmpDir + "/config.json"
	_ = config.Save(cfg, cfgPath)
	a, err := New(Options{ConfigPath: cfgPath, Config: cfg, Runtimes: map[provider.Kind]*Runtime{}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	defer a.Close()

	// 假上游：记录收到的 Authorization 头，据此断言端点用的是真实密钥
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer ts.Close()

	if err := a.SetCustomChannels([]custom.ChannelConfig{
		{Name: "stepfun", BaseURL: ts.URL, APIKey: "sk-real-secret-abcdefgh"},
	}); err != nil {
		t.Fatalf("SetCustomChannels: %v", err)
	}

	mux := http.NewServeMux()
	a.HandleAPI(mux)
	body, _ := json.Marshal(map[string]any{
		"name":     "stepfun",
		"base_url": ts.URL,
		"api_key":  maskKey("sk-real-secret-abcdefgh"), // 脱敏回显态
	})
	req := httptest.NewRequest("POST", "/api/config/custom_channel_test", bytes.NewReader(body))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if gotAuth != "Bearer sk-real-secret-abcdefgh" {
		t.Fatalf("端点应用真实密钥而非脱敏值，got %q", gotAuth)
	}
}
