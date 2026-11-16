package lingxi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// ---------------------------------------------------------------------------
// 真实 wire 形态（2026-10-28，经真实 wps_sid 直测 lingxi.kdocs.cn，
// 见 script_temp/probe_lingxi_chat_33.py / chat_frames_34.py）
// ---------------------------------------------------------------------------

// lingxiSSE 是一次真实对话的完整 9 帧序列（text 帧多帧增量）。
const lingxiSSE = "event: execution\n" +
	`data: {"type":"execution","data":{"status":"running"}}` + "\n\n" +
	"event: user\n" +
	`data: {"type":"user","data":{"text":"你好"}}` + "\n\n" +
	"event: text_start\n" +
	`data: {"type":"text_start","data":{}}` + "\n\n" +
	"event: text\n" +
	`data: {"type":"text","data":"你"}` + "\n\n" +
	"event: text\n" +
	`data: {"type":"text","data":"好"}` + "\n\n" +
	"event: text_end\n" +
	`data: {"type":"text_end","data":{}}` + "\n\n" +
	"event: aigc\n" +
	`data: {"type":"end","data":{"reason":"done"}}` + "\n\n" +
	"event: recommend_start\n" +
	`data: {"type":"recommend_start","data":{}}` + "\n\n" +
	"event: recommend\n" +
	`data: {"type":"recommend","data":["继续聊聊？"]}` + "\n\n" +
	"event: recommend_end\n" +
	`data: {"type":"recommend_end","data":{}}` + "\n\n" +
	"event: finish\n" +
	"data: \n\n"

// errorStreamSSE 是流内 error 帧的形态（magpie 源码支持；对象形态带 message）。
const errorStreamSSE = "event: text_start\n" +
	`data: {"type":"text_start","data":{}}` + "\n\n" +
	"event: error\n" +
	`data: {"type":"error","data":{"message":"积分不足"}}` + "\n\n"

// unauthorizedBody 是 tasks/plans 端点 200+UserUnauthorized 的真实形态。
const unauthorizedBody = `{"result":"UserUnauthorized","msg":"","data":null}`

// plansBody 是 plans 端点的目录信封（截取实测形态，2 模型）。
const plansBody = `{"result":"ok","v7code":0,"data":{"plans":[` +
	`{"key":"plan_pro","name":"专业版","max_context_tokens":1048576}],` +
	`"models":[` +
	`{"key":"deepseek-v4.1-flash","name":"DeepSeek V4.1 Flash","multiplier":"0.12x",` +
	`"max_context_tokens":1048576,"max_new_tokens":32768,"support_multimodal":false,` +
	`"reasoning_efforts":["low","high","max"]},` +
	`{"key":"qwen3.8_flash","name":"Qwen3.8 Flash","multiplier":"0.08x",` +
	`"max_context_tokens":1048576,"max_new_tokens":32768,"support_multimodal":true,` +
	`"reasoning_efforts":null}` +
	`]}}`

// ---------------------------------------------------------------------------
// Classify：会话失效 → 智点耗尽 → 模型不可用 → 状态码
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   provider.ErrKind
	}{
		{"401", 401, `{"error":{"message":"unauthorized"}}`, provider.ErrSessionDead},
		{"403", 403, `{"error":{"message":"forbidden"}}`, provider.ErrSessionDead},
		{"200+UserUnauthorized", 200, unauthorizedBody, provider.ErrSessionDead},
		{"429", 429, `{"error":{"message":"too many requests"}}`, provider.ErrSoftRate},
		{"402", 402, `{"error":{"message":"payment required"}}`, provider.ErrHardCredit},
		{"200+14018 智点耗尽", 200, `{"error":{"message":"Credits exhausted","code":14018}}`, provider.ErrHardCredit},
		{"200+智点不足", 200, `{"error":{"message":"智点不足，请前往会员中心充值"}}`, provider.ErrHardCredit},
		{"200+指定模型暂不可用", 200, `{"msg":"指定模型暂不可用，请切换其他模型后重试"}`, provider.ErrNotFound},
		{"404", 404, `{"error":{"message":"not found"}}`, provider.ErrNotFound},
		{"500", 500, `{"error":{"message":"internal"}}`, provider.ErrServer},
		{"502", 502, `bad gateway`, provider.ErrServer},
		{"未知形态透传", 400, `{"error":{"message":"bad request"}}`, provider.ErrClient},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.status, tc.body); got != tc.want {
				t.Fatalf("Classify(%d, %q) = %v, want %v", tc.status, tc.body, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 消息压缩（buildChatRequest / compressMessages）
// ---------------------------------------------------------------------------

func TestBuildChatRequestSingleTurn(t *testing.T) {
	body := chatBody("deepseek-v4.1-flash", []map[string]any{
		{"role": "user", "content": "你好"},
	})
	out, err := buildChatRequest(body)
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	var req chatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Question != "你好" || req.UserInput.Text != "你好" {
		t.Fatalf("question = %q user_input.text = %q，单轮应为原内容", req.Question, req.UserInput.Text)
	}
	if req.Model != "deepseek-v4.1-flash" {
		t.Fatalf("model = %q", req.Model)
	}
	if req.ReasoningEffort != "" {
		t.Fatalf("未传 effort 应省略字段，got %q", req.ReasoningEffort)
	}
}

func TestBuildChatRequestMultiTurn(t *testing.T) {
	body := chatBody("qwen3.8_max", []map[string]any{
		{"role": "system", "content": "你是助手"},
		{"role": "user", "content": "第一问"},
		{"role": "assistant", "content": "第一答"},
		{"role": "user", "content": "第二问"},
	})
	out, err := buildChatRequest(body)
	if err != nil {
		t.Fatalf("buildChatRequest: %v", err)
	}
	var req chatRequest
	if err := json.Unmarshal(out, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := "[System: 你是助手]\n\nUser: 第一问\n\nAssistant: 第一答\n\nUser: 第二问"
	if req.Question != want {
		t.Fatalf("question = %q\nwant    = %q", req.Question, want)
	}
}

func TestMapEffort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"low", "0"}, {"0", "0"},
		{"medium", "1"}, {"high", "1"}, {"1", "1"},
		{"max", "2"}, {"2", "2"},
		{"", ""}, {"ultra", ""},
	}
	for _, tc := range cases {
		if got := mapEffort(tc.in); got != tc.want {
			t.Fatalf("mapEffort(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// ChatStream：session 先建 + completions 三专用头 + SSE 透传
// ---------------------------------------------------------------------------

func TestChatStreamSendsSessionAndHeaders(t *testing.T) {
	var (
		gotSID        string
		gotCcVersion  string
		gotClientType string
		gotEntrance   string
		gotCookie     string
		gotPath       string
		gotQuestion   string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpSessions:
			gotCookie = r.Header.Get("Cookie")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"result":"ok","data":{"session_id":"sess-123"}}`))
		default:
			if !strings.HasPrefix(r.URL.Path, EpSessions+"/") {
				t.Errorf("unexpected path %q", r.URL.Path)
			}
			gotPath = r.URL.Path
			gotSID = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, EpSessions+"/"), "/completions")
			gotCcVersion = r.Header.Get("X-Cc-Version")
			gotClientType = r.Header.Get("X-Client-Type")
			gotEntrance = r.Header.Get("X-Cc-Entrance")
			buf := make([]byte, 4096)
			n, _ := r.Body.Read(buf)
			var req chatRequest
			_ = json.Unmarshal(buf[:n], &req)
			gotQuestion = req.Question
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(lingxiSSE))
		}
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	rc, status, _, err := c.ChatStream(fakeAuth(), chatBody("deepseek-v4.1-flash", []map[string]any{
		{"role": "user", "content": "你好"},
	}))
	if err != nil || status != 200 {
		t.Fatalf("ChatStream: err=%v status=%d", err, status)
	}
	defer rc.Close()

	if gotSID != "sess-123" {
		t.Fatalf("completions 用的 session_id = %q, want sess-123", gotSID)
	}
	if gotPath != EpSessions+"/sess-123/completions" {
		t.Fatalf("completions path = %q", gotPath)
	}
	if gotCcVersion != "1" || gotClientType != "lingxi-web" || gotEntrance != "default" {
		t.Fatalf("三专用头缺失: %q %q %q", gotCcVersion, gotClientType, gotEntrance)
	}
	if gotCookie != CookieName+"="+fakeSID {
		t.Fatalf("Cookie = %q", gotCookie)
	}
	if gotQuestion != "你好" {
		t.Fatalf("question = %q", gotQuestion)
	}

	// 流体原样透传（形态转换在 Stream/Aggregate）；读取到 EOF 属正常收尾，
	// 关键是已读到的内容含 text 帧。
	buf := make([]byte, 8192)
	var sb strings.Builder
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			sb.Write(buf[:n])
		}
		if rerr != nil {
			break
		}
	}
	if !strings.Contains(sb.String(), `"type":"text"`) {
		t.Fatalf("stream content = %q，应含 text 帧", sb.String())
	}
}

// 每**请求**新建 session：两次 ChatStream 必须各建一次。
func TestChatStreamNewSessionPerRequest(t *testing.T) {
	var sessionCreates int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpSessions {
			sessionCreates++
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"result":"ok","data":{"session_id":"sess-` + jsonInt(sessionCreates) + `"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(lingxiSSE))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	for i := 0; i < 2; i++ {
		rc, status, _, err := c.ChatStream(fakeAuth(), chatBody("deepseek-v4.1-flash", []map[string]any{
			{"role": "user", "content": "问"},
		}))
		if err != nil || status != 200 {
			t.Fatalf("ChatStream #%d: err=%v status=%d", i, err, status)
		}
		rc.Close()
	}
	if sessionCreates != 2 {
		t.Fatalf("session 创建次数 = %d, want 2（每请求新建）", sessionCreates)
	}
}

// 本地目录拦截必须是 400（R141）：404 会被 Classify 归成 ErrNotFound，handler 遂换号 +
// 记账号错误，换到的号照样被本地拒 ⇒ 两号各记两笔后整条渠道冷却。
func TestChatStreamUnknownModelIs400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("不该打上游：本地校验就应该拦下，path=%q", r.URL.Path)
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	c.knownIDs = map[string]string{"deepseek-v4.1-flash": "DeepSeek V4.1 Flash"} // 目录已拉到，非 fail-open

	_, status, body, err := c.ChatStream(fakeAuth(), chatBody("ghost-model", []map[string]any{{"role": "user", "content": "问"}}))
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（404 会换号+罚号，R141）", status)
	}
	if !strings.Contains(string(body), "model_not_found") || !strings.Contains(string(body), "ghost-model") {
		t.Fatalf("body = %s，应含 model_not_found 与实际模型名", body)
	}

	// 护栏：目录里有它时必须放行（404 修成 400 不能把已知模型也拦了）。
	if !c.knownModel("deepseek-v4.1-flash") {
		t.Fatal("knownModel 应放行目录内模型")
	}
}

// ---------------------------------------------------------------------------
// Aggregate：9 帧序列 → 单个 OpenAI 响应；recommend/execution 丢弃
// ---------------------------------------------------------------------------

func TestAggregate(t *testing.T) {
	c := New()
	out, err := c.Aggregate(strings.NewReader(lingxiSSE), "lingxi/deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices = %d", len(choices))
	}
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Fatalf("content = %v（两帧 text 增量应拼接）, want 你好", msg["content"])
	}
	if out["model"] != "lingxi/deepseek-v4.1-flash" {
		t.Fatalf("model = %v（应回填客户端请求名 R14）", out["model"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
}

// ppt_outline_text 是灵犀已实测的正文变体，不能按未知帧丢弃。
const outlineSSE = "event: execution\n" +
	`data: {"type":"ppt_outline_start","data":{}}` + "\n\n" +
	"event: execution\n" +
	`data: {"type":"ppt_outline_text","data":"# 第一行\n"}` + "\n\n" +
	"event: execution\n" +
	`data: {"type":"ppt_outline_text","data":"第二行"}` + "\n\n" +
	"event: execution\n" +
	`data: {"type":"ppt_outline_end","data":{}}` + "\n\n" +
	"event: aigc\n" +
	`data: {"type":"end","data":null}` + "\n\n"

func TestAggregatePPTOutlineText(t *testing.T) {
	out, err := New().Aggregate(strings.NewReader(outlineSSE), "lingxi/kimi-k3.1-pro")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices, _ := out["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg["content"] != "# 第一行\n第二行" {
		t.Fatalf("content = %q, want outline deltas joined", msg["content"])
	}
}

func TestStreamPPTOutlineText(t *testing.T) {
	w := httptest.NewRecorder()
	if _, err := New().Stream(w, strings.NewReader(outlineSSE), "lingxi/kimi-k3.1-pro"); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	body := w.Body.String()
	for _, want := range []string{`"content":"# 第一行\n"`, `"content":"第二行"`, `"finish_reason":"stop"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q in %s", want, body)
		}
	}
}

// 流内 error 帧 → StreamError（不是把错误当正文吐出）。
// W1 回归（2026-10-29 审查）：文案「积分不足」必须分类为 ErrHardCredit——
// 修复前 kind 恒 ErrPassthrough，流内耗尽/失效帧永远不触发换号与罚号。
func TestAggregateErrorFrame(t *testing.T) {
	c := New()
	_, err := c.Aggregate(strings.NewReader(errorStreamSSE), "lingxi/qwen3.8_max")
	if err == nil {
		t.Fatal("error 帧必须转成 error")
	}
	se, ok := err.(*StreamError)
	if !ok {
		t.Fatalf("err = %T (%v), want *lingxi.StreamError", err, err)
	}
	if !strings.Contains(se.Error(), "积分不足") {
		t.Fatalf("StreamError msg = %q", se.Error())
	}
	if se.Kind() != provider.ErrHardCredit {
		t.Fatalf("kind = %v（积分不足应 ErrHardCredit，W1 回归）", se.Kind())
	}
}

// newStreamError 分类表驱动：命中 markers 填 kind，未命中走 ErrPassthrough。
func TestNewStreamErrorClassification(t *testing.T) {
	cases := []struct {
		msg  string
		want provider.ErrKind
	}{
		{"请重新登录", provider.ErrSessionDead},
		{"not_login", provider.ErrSessionDead},
		{"UserUnauthorized", provider.ErrSessionDead},
		{"智点不足，请前往会员中心充值", provider.ErrHardCredit},
		{"积分不足", provider.ErrHardCredit},
		{"credit exhausted (14018)", provider.ErrHardCredit},
		{"指定模型暂不可用，请切换其他模型后重试", provider.ErrNotFound},
		{"some unknown upstream error", provider.ErrPassthrough},
	}
	for _, tc := range cases {
		se := newStreamError("lingxi/x", tc.msg)
		if got := se.Kind(); got != tc.want {
			t.Fatalf("newStreamError(%q).Kind() = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// reasoning 帧 → reasoning_content（magpie 支持，实测未复现但保守支持）。
func TestAggregateReasoningFrame(t *testing.T) {
	const s = "event: reasoning\n" +
		`data: {"type":"reasoning","data":"思考片段"}` + "\n\n" +
		"event: text\n" +
		`data: {"type":"text","data":"正文"}` + "\n\n" +
		"event: aigc\n" +
		`data: {"type":"end","data":{}}` + "\n\n"
	c := New()
	out, err := c.Aggregate(strings.NewReader(s), "lingxi/deepseek-v4.1-flash")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices, _ := out["choices"].([]any)
	choice, _ := choices[0].(map[string]any)
	msg, _ := choice["message"].(map[string]any)
	if msg["reasoning_content"] != "思考片段" {
		t.Fatalf("reasoning_content = %v", msg["reasoning_content"])
	}
	if msg["content"] != "正文" {
		t.Fatalf("content = %v", msg["content"])
	}
}

// ---------------------------------------------------------------------------
// FetchModels：plans 透传 + 缓存兜底 + 冷启动放行
// ---------------------------------------------------------------------------

func TestFetchModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpPlans {
			_, _ = w.Write([]byte(plansBody))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	mis, err := c.FetchModels(fakeAuth())
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(mis) != 2 {
		t.Fatalf("models = %d, want 2", len(mis))
	}
	// 排序字典序：deepseek < qwen
	ds := mis[0]
	if ds.ID != "deepseek-v4.1-flash" {
		t.Fatalf("ds ID = %q（不得改名）", ds.ID)
	}
	if ds.ContextWindow != 1048576 || !ds.ContextFromAPI {
		t.Fatalf("ds ContextWindow = %d ContextFromAPI = %v", ds.ContextWindow, ds.ContextFromAPI)
	}
	if !ds.SupportsReasoning || len(ds.ReasoningEfforts) != 3 {
		t.Fatalf("ds reasoning = %v（3 档）", ds.ReasoningEfforts)
	}
	if !strings.Contains(ds.Name, "0.12x") {
		t.Fatalf("ds Name = %q（倍率应进展示名）", ds.Name)
	}
	qw := mis[1]
	if !qw.SupportsImages {
		t.Fatal("qwen support_multimodal 应置 SupportsImages")
	}
	if qw.SupportsReasoning {
		t.Fatal("qwen reasoning_efforts=null 不得置 SupportsReasoning")
	}

	// knownIDs 已记住：目录端点挂掉时兜底返回缓存。
	c.Base = srv.URL + "/broken"
	cached, err := c.FetchModels(fakeAuth())
	if err != nil || len(cached) != 2 {
		t.Fatalf("缓存兜底失败: err=%v n=%d", err, len(cached))
	}
}

// 凭据失效（200+UserUnauthorized）必须报错，不得当空目录。
func TestFetchModelsUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(unauthorizedBody))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	if _, err := c.FetchModels(fakeAuth()); err == nil {
		t.Fatal("UserUnauthorized 必须报错")
	}
}

// 默认推理档（发现D 回归，2026-10-29 审查）：两档取低、三档取中、一档取自身。
// 修复前 [len/2] 对两档取了高档，与注释矛盾且费智点。
func TestFetchModelsDefaultEffort(t *testing.T) {
	cases := []struct {
		name    string
		efforts string
		want    string
	}{
		{"两档取低", `["low","high"]`, "low"},
		{"三档取中", `["low","high","max"]`, "high"},
		{"一档取自身", `["low"]`, "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"result":"ok","v7code":0,"data":{"models":[` +
				`{"key":"m","name":"M","reasoning_efforts":` + tc.efforts + `}]}}`
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()
			c := New()
			c.Base = srv.URL
			mis, err := c.FetchModels(fakeAuth())
			if err != nil || len(mis) != 1 {
				t.Fatalf("FetchModels: err=%v n=%d", err, len(mis))
			}
			if mis[0].DefaultReasoningEffort != tc.want {
				t.Fatalf("DefaultReasoningEffort = %q, want %q", mis[0].DefaultReasoningEffort, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ValidateSID：零智点消耗（走 plans）+ uid 稳定 + 401 人话错误
// ---------------------------------------------------------------------------

func TestValidateSID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != CookieName+"="+fakeSID {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
			return
		}
		_, _ = w.Write([]byte(plansBody))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	uid, err := c.validateSID(fakeSID)
	if err != nil {
		t.Fatalf("validateSID: %v", err)
	}
	// uid 带渠道前缀 lxsid-：comate 与 lingxi 可能共用同一 wps_sid，
	// 若同用 "sid-<hash>" 会撞键，面板按 uid 的账号级操作会串渠道。
	if uid == "" || !strings.HasPrefix(uid, "lxsid-") {
		t.Fatalf("uid = %q, want lxsid-<sha256[:16]>", uid)
	}
	uid2, _ := c.validateSID(fakeSID)
	if uid2 != uid {
		t.Fatalf("uid 不稳定: %q vs %q", uid, uid2)
	}
	// bogus sid → 人话错误
	if _, err := c.validateSID("bogus"); err == nil || !strings.Contains(err.Error(), "无效") {
		t.Fatalf("bogus sid err = %v, want 含「无效」", err)
	}
	// 整段 Cookie 粘贴形态（wps_sid=V02…）自动提取
	uid3, err := c.validateSID(CookieName + "=" + fakeSID)
	if err != nil || uid3 != uid {
		t.Fatalf("整段粘贴提取失败: uid3=%q err=%v", uid3, err)
	}
	// S-3 回归（2026-10-29 审查）：大写 WPS_SID= 前缀同样必须剥掉。
	uid4, err := c.validateSID("WPS_SID=" + fakeSID)
	if err != nil || uid4 != uid {
		t.Fatalf("大写前缀提取失败: uid4=%q err=%v（S-3）", uid4, err)
	}
	// S-3 扩展：wps_sid 混在多 Cookie 串中间（F12 复制整行 Cookie 头）。
	uid5, err := c.validateSID("device_id=abc123; " + CookieName + "=" + fakeSID + "; path=/")
	if err != nil || uid5 != uid {
		t.Fatalf("多 Cookie 串提取失败: uid5=%q err=%v", uid5, err)
	}
}

// ---------------------------------------------------------------------------
// 签到：已签/未签/无活动
// ---------------------------------------------------------------------------

func TestDailyCheckinAlready(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpTasks {
			_, _ = w.Write([]byte(`{"result":"ok","data":{"tasks":[` +
				`{"task_key":"daily_check_in","status":"claimed","reward_amount":100,` +
				`"extra":{"consecutive_days":5}}]}}`))
			return
		}
		t.Errorf("已签状态不应再调用 %q", r.URL.Path)
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	rep, err := c.DailyCheckinReport(fakeAuth())
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinAlready {
		t.Fatalf("status = %v, want already_claimed", rep.Status)
	}
}

func TestDailyCheckinClaim(t *testing.T) {
	var claimQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case EpTasks:
			_, _ = w.Write([]byte(`{"result":"ok","data":{"tasks":[` +
				`{"task_key":"daily_check_in","status":"pending","reward_amount":100,` +
				`"extra":{"consecutive_days":4}}]}}`))
		case EpClaim:
			claimQuery = r.URL.RawQuery
			if r.Method != http.MethodPost {
				t.Errorf("claim method = %s, want POST", r.Method)
			}
			_, _ = w.Write([]byte(`{"result":"ok","data":{"extra":{"today_reward":120},` +
				`"total_claimed_credits":2899}}`))
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	rep, err := c.DailyCheckinReport(fakeAuth())
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinClaimed {
		t.Fatalf("status = %v, want claimed", rep.Status)
	}
	if rep.Amount != 120 {
		t.Fatalf("amount = %d, want 120（claim 回执的 today_reward 优先）", rep.Amount)
	}
	if !strings.HasPrefix(claimQuery, "date=") || len(claimQuery) != len("date=YYYY-MM-DD") {
		t.Fatalf("claim query = %q, want date=YYYY-MM-DD", claimQuery)
	}
}

func TestDailyCheckinNoCampaign(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":"ok","data":{"tasks":[]}}`))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	rep, err := c.DailyCheckinReport(fakeAuth())
	if err != nil {
		t.Fatalf("DailyCheckinReport: %v", err)
	}
	if rep.Status != provider.CheckinNoCampaign {
		t.Fatalf("status = %v, want no_campaign", rep.Status)
	}
}

// ---------------------------------------------------------------------------
// 余额：total_balance + bonus_credits 明细
// ---------------------------------------------------------------------------

func TestUserResourceDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpBalance {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"result":"ok","data":{"total_balance":2899,` +
			`"bonus_summary":{"total_value":800,"total_consumed":0},` +
			`"bonus_credits":[{"balance":500,"expire_time":"2026-11-30T00:00:00+08:00"},` +
			`{"balance":300,"expire_time":"2026-12-31T00:00:00+08:00"}]}}`))
	}))
	defer srv.Close()

	c := New()
	c.Base = srv.URL
	usable, items, err := c.UserResourceDetail(fakeAuth())
	if err != nil {
		t.Fatalf("UserResourceDetail: %v", err)
	}
	if usable != 2899 {
		t.Fatalf("usable = %d, want 2899", usable)
	}
	// 基础智点（2899-800=2099）+ 2 个赠送包。
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3（基础+2 赠送包）", len(items))
	}
	var hasBase, bonusCount int
	for _, it := range items {
		if it.Key == "base" && it.Remain == 2099 {
			hasBase = 1
		}
		if it.Key == "bonus" {
			bonusCount++
			if !it.Usable {
				t.Fatal("bonus 应 Usable=true")
			}
		}
	}
	if hasBase != 1 || bonusCount != 2 {
		t.Fatalf("items 结构错: base=%d bonus=%d", hasBase, bonusCount)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

const fakeSID = "V02FakeSidForLingxiUnitTests0000000000000001"

func fakeAuth() *auth.Auth {
	return &auth.Auth{Kind: "lingxi", UID: "sid-test", AccessToken: fakeSID}
}

func chatBody(model string, messages []map[string]any) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   true,
		"messages": messages,
	})
	return b
}

func jsonInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
