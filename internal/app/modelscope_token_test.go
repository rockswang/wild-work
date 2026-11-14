package app

// modelscope_token_test.go 锁死票 04 的「粘贴 Access Token 登录」链路：
//
//	面板 → POST /api/login/modelscope_token|modelscopeai_token → App.SubmitModelScopeToken
//	     → modelscope.ValidateToken（零费用校验）→ persistModelScopeAccount（落盘 + 重载池）
//
// 为什么要单独锁：这条链路横跨「路由 kind 绑定 / 文件前缀 / 远期 ExpiresAt / 昵称保留」四处，
// 每一处漏改都**不报错**——路由绑错 kind 会把国际站 token 存进国内站池（首战 401 用户无从排查），
// 文件名前缀写错会让 auth 加载器扫不到（账号添加成功但面板不出现），ExpiresAt 写 0 会让
// NeedsRefresh 恒真 → 每请求都尝试刷新（本渠道无 refresh 端点）→ 冷却。
//
// 上游校验的文案与分类在 internal/modelscope/validate_test.go 里逐条断言；
// 这里只管「App 侧接线与落盘形态」。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// newModelscopeApp 构造只含两个 ModelScope runtime 的最小 App（AuthDir 指向临时目录）。
func newModelscopeApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir
	cn := pool.New(filepath.Join(dir, "state-modelscope.json"))
	ai := pool.New(filepath.Join(dir, "state-modelscopeai.json"))
	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.ModelScope:   {Kind: provider.ModelScope, Pool: cn, Upstream: &fakeUpstream{remain: 0}},
			provider.ModelScopeAI: {Kind: provider.ModelScopeAI, Pool: ai, Upstream: &fakeUpstream{remain: 0}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// TestPersistModelScopeAccount 落盘形态：文件前缀按 kind 二选一、Kind/ExpiresAt 正确、
// 加载器互不吞噬、同 token 重复添加给告警（不扩额度）。
func TestPersistModelScopeAccount(t *testing.T) {
	a := newModelscopeApp(t)
	const tok = "ms-fake-token-1234"
	uid := "mst-0123456789abcdef"

	_, warn, err := a.persistModelScopeAccount(provider.ModelScope, tok, uid)
	if err != nil {
		t.Fatalf("persistModelScopeAccount: %v", err)
	}
	if warn != "" {
		t.Errorf("首次添加不应有告警，得到 %q", warn)
	}

	path := filepath.Join(a.cfg.AuthDir, "modelscope-"+uid+".json")
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("凭据未落盘到 %s：%v", path, statErr)
	}
	cn, err := auth.LoadModelscopeDir(a.cfg.AuthDir)
	if err != nil || len(cn) != 1 {
		t.Fatalf("LoadModelscopeDir = %d 个（err=%v），期望 1", len(cn), err)
	}
	got := cn[0]
	if got.Kind != "modelscope" {
		t.Errorf("Kind = %q，期望 modelscope", got.Kind)
	}
	if got.AccessTokenValue() != tok {
		t.Errorf("AccessToken 落盘值不对（零 token 纪律：这里只看是否原样保存）")
	}
	if got.ExpiresAt <= time.Now().AddDate(50, 0, 0).Unix() {
		t.Errorf("ExpiresAt = %d 必须是远期（0 或近期会让 NeedsRefresh 恒真）", got.ExpiresAt)
	}
	if !strings.HasPrefix(got.Nickname, "ModelScope") {
		t.Errorf("默认昵称应带渠道显示名，得到 %q", got.Nickname)
	}

	// 同一枚 token 重复添加：必须给「不扩额度」告警
	_, warn, err = a.persistModelScopeAccount(provider.ModelScope, tok, uid)
	if err != nil {
		t.Fatalf("重复添加不应报错：%v", err)
	}
	if !strings.Contains(warn, "不") || !strings.Contains(warn, uid) {
		t.Errorf("重复 token 必须告警并点名已有 uid，得到 %q", warn)
	}

	// 国际站：独立前缀 + 独立 kind，且不得被国内站加载器吞掉
	aiUID := "msai-0123456789abcdef"
	if _, _, err := a.persistModelScopeAccount(provider.ModelScopeAI, tok, aiUID); err != nil {
		t.Fatalf("persistModelScopeAccount(modelscopeai): %v", err)
	}
	ai, err := auth.LoadModelscopeAIDir(a.cfg.AuthDir)
	if err != nil || len(ai) != 1 || ai[0].Kind != "modelscopeai" {
		t.Fatalf("LoadModelscopeAIDir = %d 个（err=%v），期望 1 个 modelscopeai", len(ai), err)
	}
	if _, statErr := os.Stat(filepath.Join(a.cfg.AuthDir, "modelscopeai-"+aiUID+".json")); statErr != nil {
		t.Fatalf("国际站凭据文件名前缀必须是 modelscopeai-：%v", statErr)
	}
	if cn, _ := auth.LoadModelscopeDir(a.cfg.AuthDir); len(cn) != 1 {
		t.Errorf("国内站加载器被国际站文件污染：%d 个（glob 边界破了）", len(cn))
	}
}

// TestModelScopeNicknamePreserved 用户改过的名字不得被重贴冲掉；默认名不算「改过」。
func TestModelScopeNicknamePreserved(t *testing.T) {
	a := newModelscopeApp(t)
	const tok = "ms-fake-token-1234"
	uid := "mst-0123456789abcdef"

	if _, _, err := a.persistModelScopeAccount(provider.ModelScope, tok, uid); err != nil {
		t.Fatal(err)
	}
	// 模拟用户在面板上改名
	list, _ := auth.LoadModelscopeDir(a.cfg.AuthDir)
	list[0].Nickname = "我的魔搭号"
	if err := list[0].SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.persistModelScopeAccount(provider.ModelScope, tok, uid); err != nil {
		t.Fatal(err)
	}
	after, _ := auth.LoadModelscopeDir(a.cfg.AuthDir)
	if len(after) != 1 || after[0].Nickname != "我的魔搭号" {
		t.Fatalf("重贴 token 冲掉了用户改的名字：%q", after[0].Nickname)
	}
}

// TestSubmitModelScopeTokenGuards kind 白名单与空 token 必须在**打上游之前**拦下
// （白名单是路由之外的第二道闸：将来若有人把两个 kind 合并成带 kind 字段的单端点，这里兜住）。
func TestSubmitModelScopeTokenGuards(t *testing.T) {
	a := newModelscopeApp(t)

	if _, _, err := a.SubmitModelScopeToken(provider.Kilo, "ms-abc"); err == nil ||
		!strings.Contains(err.Error(), "modelscope") {
		t.Errorf("非白名单 kind 必须被拒并说明只接受两区，得到：%v", err)
	}
	if _, _, err := a.SubmitModelScopeToken(provider.ModelScope, "   "); err == nil ||
		!strings.Contains(err.Error(), "不能为空") {
		t.Errorf("空 token 必须报「不能为空」，得到：%v", err)
	}
}

// TestModelscopeTokenEndpoints 两个端点各自绑定一个 kind，且形状错误在本地就回 400
// （打上游的路径由 modelscope 包的单测覆盖；这里只验路由存在与接线方向）。
func TestModelscopeTokenEndpoints(t *testing.T) {
	a := newModelscopeApp(t)
	mux := panelMux(a)

	for _, path := range []string{"/api/login/modelscope_token", "/api/login/modelscopeai_token"} {
		w := doReq(mux, "POST", path, `{}`, nil)
		if w.Code != 400 || !strings.Contains(w.Body.String(), "不能为空") {
			t.Errorf("POST %s 空 token 应 400「不能为空」，得到 %d %s", path, w.Code, w.Body.String())
		}
	}
	// 形状检查在发请求之前 ⇒ 不碰网络（这条同时证明端点没有把 token 直接送去上游）
	w := doReq(mux, "POST", "/api/login/modelscopeai_token", `{"token":"sk-not-a-ms-token"}`, nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "myaccesstoken") {
		t.Errorf("非 ms- 形态应 400 并给出生成地址，得到 %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "sk-not-a-ms-token") {
		t.Errorf("错误响应不得回显用户粘贴的凭据：%s", w.Body.String())
	}
	// 反向护栏：未注册的登录路径仍是 404（证明上面命中的是真端点而不是兜底）
	if w := doReq(mux, "POST", "/api/login/modelscope_tokens", `{}`, nil); w.Code != 404 {
		t.Errorf("未注册路径应为 404，得到 %d", w.Code)
	}
}

// TestLoginFlowsCoverModelScope 两区都必须在 loginFlows 里登记
// （否则卡片上的 ⟳ 重登入口会变成死按钮：后端只回 unknown login provider）。
func TestLoginFlowsCoverModelScope(t *testing.T) {
	for _, kind := range []provider.Kind{provider.ModelScope, provider.ModelScopeAI} {
		f, ok := loginFlows[kind]
		if !ok {
			t.Errorf("provider.%s 未在 loginFlows 登记（⟳ 入口会是死按钮）", kind)
			continue
		}
		if f.noFlowReason == "" || !strings.Contains(f.noFlowReason, "粘贴") {
			t.Errorf("provider.%s 的说明应指向粘贴层，得到 %q", kind, f.noFlowReason)
		}
	}
}