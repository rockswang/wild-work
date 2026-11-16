// e2e_modelscope_harness_test.go ModelScope 渠道**端到端**测试骨架：装配 + HTTP 辅助。
//
// 本层与单元测试的分工（spec `.scratch/modelscope-channel/spec.md` 的 user stories）：
//
//	单元测试：单个函数 / 单个渠道包的内部契约（已由 internal/modelscope、
//	          internal/server、internal/app 的既有 *_test.go 覆盖）。
//	本层：**真实进程内装配 + 真实 TCP 端口 + 真实 HTTP 客户端**跑通用户视角场景——
//	      面板粘贴 token 之后账号是否真的入池、/v1/models 是否真的列出模型、
//	      带斜杠的模型 ID 是否真的路由到上游、生图异步任务是否真的走完 submit+poll、
//	      两个区域的池是否真的互不兜底、面板"不适用"与平台概览表是否真的随之正确。
//
// 为什么必须打真实端口（而不是 httptest.NewRecorder 直调 handler）：
// 用户故事里的失败模式大量来自**中间件与装配**——hostGuard、管理 API 的会话/CSRF 中间件、
// 静态嵌入、Server 的读写超时、SSE 的 Flush 行为。直调 handler 全部绕过它们，
// 那正是"单元测试保证不了的整体正确性"。
//
// 装配方式与 cmd/wild-work/main.go 同构：auth 文件 → pool → modelscope.Client（Base 指到
// 假上游）→ server.Handler → App（同一份 runtimes + 同一份 ledger/stats）→ App.StartServer()。
//
// 粘贴入口 POST /api/login/modelscope_token 的**校验**不可被 Client.Base 覆盖（它走包级
// modelscope.ValidateToken → New(kind) → 正式基址），故 App 留了 msValidate 测试注入点：
// 本层把它接到「同一份 Client（Base=假上游）」，于是「粘贴即接入」成功链路与区域 401
// 文案都能离线跑通（产品路径不改，见 e2e-findings §5.1）。同理生图轮询的时限/间隔有
// SetPollLimitForTest / SetPollSleepForTest 两个测试专用钩子（e2e-findings §5.2）。

package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/glm"
	"wild-work/internal/ledger"
	"wild-work/internal/modelscope"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
	"wild-work/internal/scheduler"
	"wild-work/internal/server"
)

// ---------------------------------------------------------------------------
// 全栈环境（App + server.Handler + 真实端口）
// ---------------------------------------------------------------------------

type e2eEnv struct {
	t    *testing.T
	app  *App
	base string
	srv  *server.Handler
	cli  *http.Client

	cn, ai *msFake
	glm    *glmStub
	// site 主站假上游（魔粒余额/流水/今日用量/每日领取）。与对话假上游是**两个 host**，
	// 正是「双 host 双鉴权」这条判据唯一能成立的装配方式（credits.go D7）。
	site *msSite

	cnPool, aiPool *pool.Pool
	authDir        string
	stateDir       string

	// msUPs 两区装配出来的渠道客户端（Base 已指到假上游）。测试直接对它们调用
	// 测试专用钩子（如 SetPollLimitForTest）——不必再发明第二套注入参数。
	msUPs map[provider.Kind]*modelscope.Client
}

type e2eOptions struct {
	CN, AI    *msFake // 至少一个非 nil
	CNTokens  []string
	AITokens  []string
	GLM       *glmStub // 非 nil 时装配 glm 渠道（生图回归）
	StickyMax int

	// Site 非 nil 时装配主站假上游（两区共用）——额度/领取面全部落在它上面。
	Site *msSite
	// ExpiringDays 面板「临期」窗口（自然日，0 = 默认 1）。
	ExpiringDays int
	// CheckinMinutes 两区调度器的签到时刻（0 值取 []int{9*60}：必须是**非空**切片，
	// 否则 finishCheckin 会跳过签到状态落盘，面板永远看不到结果，见 R150）。
	CheckinMinutes []int
}

// 端口用本包既有的 freePort（session_test.go）：App.serveLocked 记的是配置里的地址串，
// 绑 :0 之后拿不回真实端口，客户端就没法发请求，故必须先占后放取具体端口。

// newE2EEnv 按 cmd/wild-work/main.go 的同构顺序装配：凭据文件 → pool → 渠道客户端 →
// server.Handler → App → StartServer。凭据文件由 auth.Auth.SaveAtomic 写入，
// 与面板粘贴路径落盘的**形态逐字一致**（modelscope-<uid>.json / modelscopeai-<uid>.json）。
func newE2EEnv(t *testing.T, opt e2eOptions) *e2eEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = filepath.Join(dir, "auths")
	cfg.AdminPass = "" // 面板不鉴权（环回 + 无密码 = 默认部署形态，R4）
	cfg.Listen = config.Listen{Host: "127.0.0.1", Port: freePort(t)}
	if opt.ExpiringDays > 0 {
		cfg.Schedule.ExpiringDays = opt.ExpiringDays
	}

	env := &e2eEnv{t: t, cn: opt.CN, ai: opt.AI, glm: opt.GLM, site: opt.Site,
		authDir: cfg.AuthDir, stateDir: filepath.Dir(cfg.StateFile),
		msUPs: map[provider.Kind]*modelscope.Client{}}

	seed := func(kind provider.Kind, tokens []string) {
		if len(tokens) == 0 {
			return
		}
		if err := os.MkdirAll(cfg.AuthDir, 0o755); err != nil {
			t.Fatalf("建凭据目录失败: %v", err)
		}
		for _, tok := range tokens {
			uid := modelscope.UIDFor(kind, tok)
			aa := &auth.Auth{
				Kind:        kind.String(),
				UID:         uid,
				Nickname:    modelscope.DefaultNickname(kind, tok),
				AccessToken: tok,
				ExpiresAt:   modelscope.FarFutureExpiry(), // 不为 0：0 会让 NeedsRefresh 恒真
				FilePath:    filepath.Join(cfg.AuthDir, kind.String()+"-"+uid+".json"),
			}
			if err := aa.SaveAtomic(); err != nil {
				t.Fatalf("写入 %s 凭据失败: %v", kind, err)
			}
		}
	}
	seed(provider.ModelScope, opt.CNTokens)
	seed(provider.ModelScopeAI, opt.AITokens)

	appRts := map[provider.Kind]*Runtime{}
	srvRts := map[provider.Kind]*server.Runtime{}
	// 流水记录器：与 App / scheduler 共用同一实例（main.go 同构）。提前建出来是为了让
	// 调度器在 App.New 之前就挂到 Runtime 上——启动后补挂会与统计喂入器并发读写同一结构。
	lg, err := ledger.New(filepath.Join(dir, "ledger"))
	if err != nil {
		t.Fatalf("建流水记录器失败: %v", err)
	}
	checkinMinutes := opt.CheckinMinutes
	if checkinMinutes == nil {
		checkinMinutes = []int{9 * 60}
	}
	addMS := func(kind provider.Kind, fake *msFake) {
		if fake == nil {
			return
		}
		var auths []*auth.Auth
		var err error
		if kind == provider.ModelScopeAI {
			auths, err = auth.LoadModelscopeAIDir(cfg.AuthDir)
			env.aiPool = pool.New(filepath.Join(dir, "state-modelscopeai.json"))
		} else {
			auths, err = auth.LoadModelscopeDir(cfg.AuthDir)
			env.cnPool = pool.New(filepath.Join(dir, "state-modelscope.json"))
		}
		if err != nil {
			t.Fatalf("加载 %s 凭据失败: %v", kind, err)
		}
		p := env.aiPool
		if kind == provider.ModelScope {
			p = env.cnPool
		}
		for _, aa := range auths {
			p.Add(aa)
		}
		up := modelscope.New(kind)
		up.Base = fake.URL() // 唯一的上游注入点（Client.Base 是显式测试口）
		if opt.Site != nil {
			// 额度面走主站（另一个 host）：不设它的会是**真** modelscope.cn——绝不能在测试里发生。
			up.SiteBase = opt.Site.URL()
		}
		up.HTTP.Timeout = 10 * time.Second
		up.StreamHTTP.Transport = up.HTTP.Transport
		env.msUPs[kind] = up
		rt := &Runtime{Kind: kind, Pool: p, Upstream: up}
		// 调度器：与 main.go 同构（FollowGlobalCheckin + KeepaliveHours 显式空），供
		// /api/account/checkin（票 04 每日领取三态）与「签到后刷余额」走真实链路。
		// 只在装配了主站假上游时才建——无额度面的用例不该多一个依赖。
		if opt.Site != nil {
			rt.Scheduler = scheduler.New(scheduler.Config{
				Pool:                p,
				Upstream:            up,
				Name:                kind.String(),
				CheckinMinutes:      append([]int{}, checkinMinutes...),
				KeepaliveHours:      []int{}, // ms- token 无 refresh 端点（R39 显式空切片）
				FollowGlobalCheckin: true,
				Ledger:              lg,
				ExpiringDays:        opt.ExpiringDays,
			})
		}
		appRts[kind] = rt
		srvRts[kind] = &server.Runtime{Kind: kind, Pool: p, Upstream: up, StaticModels: modelscope.StaticModels()}
	}
	addMS(provider.ModelScope, opt.CN)
	addMS(provider.ModelScopeAI, opt.AI)
	if opt.GLM != nil {
		gp := pool.New(filepath.Join(dir, "state-glm.json"))
		gp.Add(&auth.Auth{Kind: provider.GLM.String(), UID: "glm-e2e-uid", AccessToken: "glm-token",
			ExpiresAt: time.Now().Add(24 * time.Hour).Unix(), FilePath: filepath.Join(cfg.AuthDir, "glm-x.json")})
		appRts[provider.GLM] = &Runtime{Kind: provider.GLM, Pool: gp, Upstream: opt.GLM}
		// 静态目录喂给 server 层（与 main.go 同构）：glm 的 /v1/models 没有上游目录，
		// StaticModels 就是权威清单——包括本轮新增的 `glm/image`（带能出图标记）。
		srvRts[provider.GLM] = &server.Runtime{Kind: provider.GLM, Pool: gp, Upstream: opt.GLM, StaticModels: glm.StaticModels()}
	}

	a, err := New(Options{ConfigPath: filepath.Join(dir, "config.json"), Config: cfg, Runtimes: appRts, Ledger: lg})
	if err != nil {
		t.Fatalf("App.New: %v", err)
	}
	env.app = a
	// 粘贴入口的上游校验：把默认的 modelscope.ValidateToken（正式上游）换成「同一份
	// Client 但 Base 指假上游」。这是 e2e-findings §5.1 开的那道缝——产品路径与断言
	// 逐字不变，只是校验请求落到 httptest。
	a.msValidate = env.validateToken
	t.Cleanup(a.Stop)

	stickyMax := opt.StickyMax
	h := server.NewHandler(server.Config{
		Runtimes:          srvRts,
		Ledger:            lg,
		Stats:             a.Stats(), // 平台概览表 / 模型榜的数据面
		AttachAPI:         a.HandleAPI,
		SoftCooldown:      time.Minute,
		ErrThreshold:      3,
		ErrCooldown:       time.Minute,
		MaxRequestBody:    128 << 20,
		StickyMaxRequests: stickyMax,
	})
	env.srv = h
	a.SetHandler(h)
	a.SetRootHandler(h) // 无兼容层：本层只跑 /v1/* 与 /api/*（三接口兼容层有自己的测试）
	if err := a.StartServer(); err != nil {
		t.Fatalf("StartServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// 统计喂入器：平台行来自「账号快照」喂入（ApplyPoll），不跑它就只有 token 没有平台行。
	a.StartStatsFeeders(ctx)

	env.base = "http://" + cfg.Listen.Addr()
	env.cli = &http.Client{Timeout: 60 * time.Second}
	env.waitReady()
	return env
}

// validateToken 用「同一份渠道 Client、Base 指假上游」跑粘贴校验（product 路径逐字不变）。
func (e *e2eEnv) validateToken(kind provider.Kind, token string) (string, error) {
	up := e.msUPs[kind]
	if up == nil {
		return "", fmt.Errorf("e2e: %s 没有装配假上游（e2eOptions 需带对应的 fake）", kind)
	}
	return up.ValidateToken(token)
}

// msUP 取某区已装配的渠道客户端（测试设测试专用钩子用）。
func (e *e2eEnv) msUP(kind provider.Kind) *modelscope.Client { return e.msUPs[kind] }

// waitReady 等首个请求成功（StartServer 是异步起 goroutine 的）。
func (e *e2eEnv) waitReady() {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := e.cli.Get(e.base + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("服务未在 5s 内就绪（%s）", e.base)
}

// ---------------------------------------------------------------------------
// HTTP 辅助
// ---------------------------------------------------------------------------

func (e *e2eEnv) do(method, path string, payload any, hdr map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			e.t.Fatalf("序列化请求体失败: %v", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.base+path, body)
	if err != nil {
		e.t.Fatalf("构造请求失败: %v", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.cli.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s 失败: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatalf("读响应体失败（%s）：%v", path, err)
	}
	return resp, raw
}

func (e *e2eEnv) post(path string, payload any) (*http.Response, []byte) {
	e.t.Helper()
	return e.do(http.MethodPost, path, payload, nil)
}

func (e *e2eEnv) get(path string) (*http.Response, []byte) {
	e.t.Helper()
	return e.do(http.MethodGet, path, nil, nil)
}

// jsonMap 解析响应体为 map（断言失败时附带原文，便于排障）。
func jsonMap(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("响应不是合法 JSON: %v\n原文: %s", err, truncateForLog(raw))
	}
	return m
}

func truncateForLog(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 800 {
		return s[:800] + "…"
	}
	return s
}

// dig 取嵌套字段（路径为空/类型不符返回 nil，调用方自己判）。
func dig(m map[string]any, path ...string) any {
	var cur any = m
	for _, k := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[k]
	}
	return cur
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asFloat(v any) float64 {
	f, _ := v.(float64)
	return f
}

// appLog 读本次运行的 data/app.log（消息框/日志零 token 纪律的取证面）。
func (e *e2eEnv) appLog() string {
	e.t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.stateDir, "app.log"))
	if err != nil {
		e.t.Fatalf("读 app.log 失败: %v", err)
	}
	return string(raw)
}

// apiState 拉面板状态（/api/state）。
func (e *e2eEnv) apiState() map[string]any {
	e.t.Helper()
	resp, raw := e.get("/api/state")
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("/api/state = %d：%s", resp.StatusCode, truncateForLog(raw))
	}
	return jsonMap(e.t, raw)
}

// accounts 返回 /api/state 的账号视图（按 group 过滤；group 为空表示不过滤）。
func (e *e2eEnv) accounts(group string) []map[string]any {
	e.t.Helper()
	st := e.apiState()
	list, _ := st["accounts"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, it := range list {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if group != "" && asString(m["group"]) != group {
			continue
		}
		out = append(out, m)
	}
	return out
}

// modelsIDs 取 /v1/models 的 id 列表。
func (e *e2eEnv) modelsIDs() []string {
	e.t.Helper()
	resp, raw := e.get("/v1/models")
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("/v1/models = %d：%s", resp.StatusCode, truncateForLog(raw))
	}
	m := jsonMap(e.t, raw)
	data, _ := m["data"].([]any)
	out := make([]string, 0, len(data))
	for _, it := range data {
		if mm, ok := it.(map[string]any); ok {
			out = append(out, asString(mm["id"]))
		}
	}
	return out
}

func containsID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func mustContain(t *testing.T, ids []string, want string) {
	t.Helper()
	if !containsID(ids, want) {
		t.Fatalf("/v1/models 缺 %q；实际列出 %v", want, ids)
	}
}

func mustNotContain(t *testing.T, ids []string, unwanted string) {
	t.Helper()
	if containsID(ids, unwanted) {
		t.Fatalf("/v1/models 不应包含 %q；实际列出 %v", unwanted, ids)
	}
}

// sseDeltas 解析 SSE 正文里的正文增量（非 data 行与 [DONE] 忽略）。
func sseDeltas(raw []byte) (text string, models map[string]bool) {
	models = map[string]bool{}
	var sb strings.Builder
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(payload), &m) != nil {
			continue
		}
		if s := asString(m["model"]); s != "" {
			models[s] = true
		}
		chs, _ := m["choices"].([]any)
		for _, c := range chs {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			d, _ := cm["delta"].(map[string]any)
			sb.WriteString(asString(d["content"]))
		}
	}
	return sb.String(), models
}

// statsSnapshot 拉运行统计快照。
func (e *e2eEnv) statsSnapshot() map[string]any {
	e.t.Helper()
	resp, raw := e.get("/api/stats")
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("/api/stats = %d：%s", resp.StatusCode, truncateForLog(raw))
	}
	return jsonMap(e.t, raw)
}

// platformRow 取平台表里某个渠道的行。
func (e *e2eEnv) platformRow(group string) map[string]any {
	e.t.Helper()
	snap := e.statsSnapshot()
	rows, _ := snap["platforms"].([]any)
	for _, it := range rows {
		m, ok := it.(map[string]any)
		if ok && asString(m["group"]) == group {
			return m
		}
	}
	return nil
}

// eventually 轮询直到 cond 为真（上限 3s）：统计喂入器与异步落盘不是同步的。
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}
