// login_test.go 自助 OAuth 登录的**离线**回归测试：全程零真实网络。
//
// 三个上游（agentarts 控制面 / 华为 STS / 授权页）全部指向 httptest；
// 历史教训见文件末尾各用例注释——每一条都对应一个「用户点了新增账号却不成功」的真实断点。
package officeace

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 假上游
// ---------------------------------------------------------------------------

// fakeUpstream 是 AgentArts 控制面 + 华为 STS 的最小仿真。
// 它**按华为云真实契约**回应（而不是按本仓旧的错误假设），用于锁定客户端侧的一致性。
type fakeUpstream struct {
	*httptest.Server

	mu sync.Mutex

	// state 端点
	stateStatus int
	stateBody   string
	stateCalls  int

	// code 端点（轮询）
	codePending   int  // 前 N 次回 202
	codeHTTP      int  // 非 0 时覆盖：直接回这个状态码
	codeBody      string
	codeCalls     int
	codeQueries   []string
	codeGetField  string // 非 "" 时用 {"authorizationCode": ...} 这种驼峰字段回应
	codeAuthzCode string

	// STS 端点
	stsStatus    int
	stsBody      string
	stsForm      url.Values
	stsDPoP      string
	stsCalls     int
	stsDpopTries int

	// validate 端点
	valStatus int
	valBody   string
	valCalls  int
	valAuth   string
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{
		stateStatus: http.StatusOK,
		stateBody:   `{"state":"state-abc"}`,
		codePending: 1,
		stsStatus:   http.StatusOK,
		stsBody: `{"credentials":{"access_key_id":"AK-TMP","secret_access_key":"SK-TMP",` +
			`"security_token":"STS-TMP","project_id":"proj-1","expiration":"2030-01-01T00:00:00.000Z"},` +
			`"refresh_token":"rt-first","refresh_expires_at":"2031-01-01T00:00:00.000Z","id_token":"` +
			makeJWT(t, map[string]any{"sub": "user-7", "preferred_username": "zhangsan"}) + `"}`,
		valStatus: http.StatusOK,
		valBody: `{"account_id":"acct-9","principal_id":"user-7","subscription":{"model_info":{` +
			`"model_api_url_base":"https://modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com/v2",` +
			`"model_auth_info":{"model_app_key":"APPKEY","model_app_secret":"APPSECRET"}}}}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/claw/auth/state", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.stateCalls++
		status, body := f.stateStatus, f.stateBody
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/v1/claw/auth/code", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.codeCalls++
		q := r.URL.Query().Get("state")
		f.codeQueries = append(f.codeQueries, q)
		pending, httpOverride := f.codePending, f.codeHTTP
		if f.codePending > 0 {
			f.codePending--
		}
		body, ackField, ack := f.codeBody, f.codeGetField, f.codeAuthzCode
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if httpOverride != 0 {
			w.WriteHeader(httpOverride)
			_, _ = io.WriteString(w, body)
			return
		}
		if pending > 0 {
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{}`)
			return
		}
		if ack != "" {
			if ackField != "" {
				out, _ := json.Marshal(map[string]string{ackField: ack})
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(out)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"code":`+jsonStr(ack)+`,"authorization_code":`+jsonStr(ack)+`}`)
			return
		}
		out, _ := json.Marshal(map[string]string{"code": "CODE-1", "authorization_code": "CODE-1"})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/v1/oauth2/tokens", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.stsCalls++
		_ = r.ParseForm()
		f.stsForm = r.PostForm
		if r.Header.Get("DPoP") != "" {
			f.stsDpopTries++
			f.stsDPoP = r.Header.Get("DPoP")
		}
		status, body := f.stsStatus, f.stsBody
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("/v1/claw/client-permission-validate", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.valCalls++
		f.valAuth = r.Header.Get("Authorization")
		status, body := f.valStatus, f.valBody
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	t.Cleanup(func() {
		pointLoginAtProduction()
	})
	return f
}

// pointLoginAtProduction 把三个上游地址还原成生产值（t.Cleanup 兜底，防串测）。
func pointLoginAtProduction() {
	agentartsBase = productionAgentartsBase
	stsTokenURL = productionStsTokenURL
	authBase = productionAuthBase
}

// pointLoginAt 把登录链路的三个上游全部指向假服务。
func (f *fakeUpstream) pointLoginAt() {
	agentartsBase = f.URL
	stsTokenURL = f.URL + "/v1/oauth2/tokens"
	authBase = f.URL
}

func (f *fakeUpstream) counts() (state, code, sts, val int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateCalls, f.codeCalls, f.stsCalls, f.valCalls
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// makeJWT 造一个只用于解码的假 id_token（签名段是填充，本仓不验签）。
func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT"})
	p, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(p) + ".sig"
}

// waitStatus 等到状态离开 pending/exchanging（或超时），返回最终状态与错误。
func waitStatus(t *testing.T, s *LoginSession, want ...string) (string, error) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		st, err := s.Status()
		for _, w := range want {
			if st == w {
				return st, err
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, err := s.Status()
	t.Fatalf("登录会话停在 %q（err=%v），期望进入 %v —— 后台轮询可能死循环或状态机没推进", st, err, want)
	return st, err
}

func readAuthFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 正常路径：7 步全部走通
// ---------------------------------------------------------------------------

// TestLoginHappyPath 全链路：state → code（先 202 后 200）→ STS → validate → 落盘 → onDone。
// 用例本身也是「其它链路契约」的基线：任一环写错，这里先红。
func TestLoginHappyPath(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()
	dir := t.TempDir()

	var doneCalls int
	var mu sync.Mutex
	doneCh := make(chan LoginResult, 1)

	s, err := StartLogin(dir, func(r LoginResult) {
		mu.Lock()
		doneCalls++
		mu.Unlock()
		doneCh <- r
	})
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if !strings.Contains(s.URL(), "state-abc") {
		t.Fatalf("授权 URL 未携带云端 state: %s", s.URL())
	}
	if st, _ := s.Status(); st != "pending" {
		t.Fatalf("起始状态应为 pending，实得 %q", st)
	}

	waitStatus(t, s, "done")
	// onDone 在 run() 里同步调用（status 置 done 之后），这里等它真正返回再断言，
	// 否则读 got/doneCalls 会与 goroutine 的写形成数据竞争。
	select {
	case cb := <-doneCh:
		if cb.UID == "" || cb.Nickname != "zhangsan" {
			t.Fatalf("onDone 参数不对: %+v", cb)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("status=done 之后 onDone 一直没被调用 —— 账号不会被加入面板")
	}
	if res, ok := s.Result(); !ok || res.UID == "" {
		t.Fatalf("status=done 但 Result 不可用: %+v ok=%t", res, ok)
	}
	mu.Lock()
	calls := doneCalls
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("onDone 应恰好调用一次，实得 %d", calls)
	}

	files := readAuthFiles(t, dir)
	if len(files) != 1 || !strings.HasPrefix(files[0], "officeace-") {
		t.Fatalf("auths 目录应有 1 个 officeace-*.json，实得 %v", files)
	}

	stateCalls, codeCalls, stsCalls, valCalls := up.counts()
	if stateCalls != 1 || stsCalls != 1 || valCalls != 1 {
		t.Fatalf("上游调用次数异常 state=%d sts=%d validate=%d", stateCalls, stsCalls, valCalls)
	}
	if codeCalls < 2 {
		t.Fatalf("code 轮询应至少 2 次（先 202 后 200），实得 %d", codeCalls)
	}
	// PKCE：换令牌必须带 verifier，且 grant_type 正确。
	if up.stsForm.Get("grant_type") != "authorization_code" || up.stsForm.Get("code_verifier") == "" {
		t.Fatalf("STS 表单不对: %v", up.stsForm)
	}
	if up.stsForm.Get("code") != "CODE-1" {
		t.Fatalf("STS 表单 code 不对: %q", up.stsForm["code"])
	}
}

// TestLoginPersistsRefreshToken 落盘的凭据必须能续期：refresh_token + DPoP 私钥 + 网关 Basic 都要在。
// 缺 refresh_token ⇒ 登录当下看起来成功，~2h 后积分/签到静默失效（EXPORT 缺陷同源）。
func TestLoginPersistsRefreshToken(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()
	dir := t.TempDir()
	s, err := StartLogin(dir, nil)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	waitStatus(t, s, "done")

	raw, err := os.ReadFile(dir + string(os.PathSeparator) + readAuthFiles(t, dir)[0])
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Auth map[string]any `json:"auth"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("auth 文件不是嵌套 {auth:...} 格式: %v", err)
	}
	if doc.Auth["refreshToken"] != "rt-first" {
		t.Fatalf("refresh_token 未落盘: %v", doc.Auth["refreshToken"])
	}
	if !strings.HasPrefix(fmt.Sprint(doc.Auth["accessToken"]), "Basic ") {
		t.Fatalf("网关凭据（accessToken）应为 Basic 头值: %v", doc.Auth["accessToken"])
	}
	var extra map[string]string
	if m, ok := doc.Auth["extra"].(map[string]any); ok {
		extra = map[string]string{}
		for k, v := range m {
			extra[k] = fmt.Sprint(v)
		}
	}
	if extra["dpop_priv"] == "" {
		t.Fatalf("DPoP 私钥未落盘（下次续期会换钥 ⇒ 上游拒绝）: %v", doc.Auth["extra"])
	}
	if extra["account_id"] != "acct-9" || extra["user_id"] != "user-7" {
		t.Fatalf("账号身份字段未落盘: %v", extra)
	}
}

// ---------------------------------------------------------------------------
// 缺陷 A（本轮实测复现）：code 端点响应字段大小写不一致 ⇒ 永远取不到 code
// ---------------------------------------------------------------------------

// TestPollCodeClassify 纯函数级：202 必须被判为「还没好」且**不产生 error**。
//
// 历史断点：run() 原把 202 当成 err=nil 的死循环（没有 sleep 分支），且把 202 直接
// 计入 maxPollErrors 的邻居分支；一次「用户还没登完」就可能被推成 failed。
func TestPollCodeClassify(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()

	// 202 = pending
	if code, _, err := pollCode(&http.Client{Timeout: 5 * time.Second}, "state-abc"); code != "" || err != nil {
		t.Fatalf("202 应返回空 code 且无 error，实得 code=%q err=%v", code, err)
	}
}

// TestPollCodeAcceptsCamelCaseAuthorizationCode 锁定「授权码字段名」兼容性。
//
// 断点：华为云/AgentArts 侧字段是 `authorizationCode`（驼峰，DPoP 档案同源命名），
// 本仓原只认 snake_case 的 `authorization_code` ⇒ 202 之后一直把 200 解析成空 code，
// 表现为用户在浏览器里明明授权成功，面板却一直转圈直到 10 分钟超时。
func TestPollCodeAcceptsCamelCaseAuthorizationCode(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()
	up.codePending = 0
	up.codeGetField = "authorizationCode"

	code, _, err := pollCode(&http.Client{Timeout: 5 * time.Second}, "state-abc")
	if err != nil {
		t.Fatalf("pollCode: %v", err)
	}
	if code == "" {
		t.Fatal("200 响应带 authorizationCode（驼峰）时取不到 code —— 登录会一直转圈到超时")
	}
}

// TestPollCodeCaseInsensitiveJSON 其它大小写变体同样必须认得（上游契约未文档化，一律兼容）。
func TestPollCodeCaseInsensitiveJSON(t *testing.T) {
	for _, field := range []string{"authorizationCode", "AuthorizationCode", "authorization_code", "code"} {
		up := newFakeUpstream(t)
		up.pointLoginAt()
		up.codePending = 0
		up.codeGetField = field
		code, _, err := pollCode(&http.Client{Timeout: 5 * time.Second}, "state-abc")
		if err != nil || code == "" {
			t.Errorf("字段 %q 无法解析：code=%q err=%v", field, code, err)
		}
	}
}

// TestPollCode404IsTerminal 404 必须是终态（state 失效），不得无限重试。
func TestPollCode404IsTerminal(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()
	up.codeHTTP = http.StatusNotFound
	_, _, err := pollCode(&http.Client{Timeout: 5 * time.Second}, "state-abc")
	if err == nil {
		t.Fatal("404 应返回错误（state 失效）")
	}
}

// ---------------------------------------------------------------------------
// 缺陷 B（本轮实测复现）：run() 空转 —— 没有 sleep、fails 不增长
// ---------------------------------------------------------------------------

// TestRunDoesNotSpinOnAccepted 上游持续 202 时，后台不得忙等 CPU（也不能 10 分钟内毫无状态变化）。
//
// 断点：原实现的 sleep select 在 `default` 分支里（永远命中），pollInterval 形同不存在；
// 上游回 202 时 run() 会以全速发请求——既是 CPU 空转，也是对上游的无节制压测（违反 §0.1）。
func TestRunDoesNotSpinOnAccepted(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()
	up.codePending = 1 << 30 // 永不就绪
	s, err := StartLogin(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	defer s.Cancel()

	if !strings.HasPrefix(s.URL(), up.URL) {
		t.Fatalf("授权 URL 应指向假授权页: %s", s.URL())
	}
	time.Sleep(300 * time.Millisecond)
	_, codeCalls, _, _ := up.counts()
	// 300ms 内按 2.5s 间隔最多 2 次（启动即查一次 + 边界一次）。
	if codeCalls > 3 {
		t.Fatalf("pollInterval 未生效：300ms 内轮询了 %d 次（忙等 + 对上游的无节制请求）", codeCalls)
	}
	if st, _ := s.Status(); st != "pending" {
		t.Fatalf("202 期间应保持 pending（前端继续转圈），实得 %q", st)
	}
}

// ---------------------------------------------------------------------------
// 缺陷 C（本轮修复）：会话终态后上一轮「幽灵会话」卡住后续新增
// ---------------------------------------------------------------------------

// TestStartLoginAfterPreviousSessionEnded 上一轮已终态（done/failed/expired/canceled）后必须能重新发起。
// 断点：app 层只把 pending/exchanging 视为「进行中」，但上层若不换新会话，
// 面板轮询仍读到旧终态 ⇒ 新流程永远不显示。此用例锁定 officeace 侧语义：
// 每次 StartLogin 都是**独立会话**，互不串状态。
func TestStartLoginAfterPreviousSessionEnded(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()

	first, err := StartLogin(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("第一次 StartLogin: %v", err)
	}
	waitStatus(t, first, "done")

	// 第二次：state 端点换成失败，确认新会话是全新的（不继承上一轮 done）。
	up.mu.Lock()
	up.stateStatus = http.StatusBadGateway
	up.stateBody = `{"error":"upstream down"}`
	up.mu.Unlock()

	if _, err := StartLogin(t.TempDir(), nil); err == nil {
		t.Fatal("取 state 失败时必须把错误返回给调用方（前端要能显示明确原因）")
	} else if !strings.Contains(err.Error(), "502") {
		t.Fatalf("state 失败错误应带状态码与上游正文: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 缺陷 D：上游 4xx 必须如实透传（不能变成「未知失败」）
// ---------------------------------------------------------------------------

// TestExchangeFailureSurfacesUpstreamBody 换令牌 400（DPoP/PKCE 被拒）要带上游原因。
func TestExchangeFailureSurfacesUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()
	up.stsStatus = http.StatusBadRequest
	up.stsBody = `{"error":"invalid_dpop_proof","error_description":"htm/htu mismatch"}`

	s, err := StartLogin(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	st, serr := waitStatus(t, s, "failed")
	if st != "failed" || serr == nil {
		t.Fatalf("状态=%q err=%v", st, serr)
	}
	if !strings.Contains(serr.Error(), "invalid_dpop_proof") {
		t.Fatalf("错误未透传上游原因（用户无法自查）: %v", serr)
	}
	if !strings.Contains(serr.Error(), "400") {
		t.Fatalf("错误未带 HTTP 状态码: %v", serr)
	}
}

// TestValidateFailureSurfacesUpstreamBody 取网关凭据 4xx/5xx 同理。
func TestValidateFailureSurfacesUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()
	up.valStatus = http.StatusForbidden
	up.valBody = `{"error_code":"APIG.1009","error_msg":"token expired"}`

	s, err := StartLogin(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	_, serr := waitStatus(t, s, "failed")
	if serr == nil || !strings.Contains(serr.Error(), "APIG.1009") {
		t.Fatalf("validate 失败未透传原因: %v", serr)
	}
	if files := readAuthFiles(t, t.TempDir()); len(files) != 0 {
		t.Fatalf("失败路径不得落盘: %v", files)
	}
}

// TestValidateNoServiceGiveHint 未开通服务（缺 model_app_key）时给出可操作提示。
func TestValidateNoServiceGiveHint(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()
	up.valBody = `{"account_id":"acct-9","principal_id":"user-7"}`

	s, err := StartLogin(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	_, serr := waitStatus(t, s, "failed")
	if serr == nil || !strings.Contains(serr.Error(), "邀请码") {
		t.Fatalf("未开通服务应提示兑换邀请码，实得: %v", serr)
	}
}

// TestStateFailureIsExplicit state 端点失败要让调用方拿到明确错误（不是空 URL）。
func TestStateFailureIsExplicit(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()
	up.stateStatus = http.StatusServiceUnavailable
	up.stateBody = `{"error":"maintenance"}`
	if _, err := StartLogin(t.TempDir(), nil); err == nil {
		t.Fatal("state 503 时必须报错")
	} else if !strings.Contains(err.Error(), "503") || !strings.Contains(err.Error(), "maintenance") {
		t.Fatalf("错误应带状态码与上游正文: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 取消：不得落盘、不得调 onDone
// ---------------------------------------------------------------------------

func TestCancelBeforeCodeStopsSession(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()
	up.codePending = 1 << 30
	dir := t.TempDir()
	var done int
	s, err := StartLogin(dir, func(LoginResult) { done++ })
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	s.Cancel()
	waitStatus(t, s, "canceled")
	time.Sleep(100 * time.Millisecond)
	if done != 0 {
		t.Fatalf("取消后不得回调 onDone")
	}
	if files := readAuthFiles(t, dir); len(files) != 0 {
		t.Fatalf("取消后不得落盘: %v", files)
	}
}

// TestCancelIsIdempotent 取消可重复调用（前端 confirm/cancel 两条路都会调）。
func TestCancelIsIdempotent(t *testing.T) {
	up := newFakeUpstream(t)
	up.pointLoginAt()
	up.codePending = 1 << 30
	s, err := StartLogin(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	s.Cancel()
	s.Cancel() // 必须不 panic（重复 close chan 会 panic）
	if st, _ := s.Status(); st != "canceled" {
		t.Fatalf("状态=%q 期望 canceled", st)
	}
}

// ---------------------------------------------------------------------------
// 授权 URL 契约
// ---------------------------------------------------------------------------

// TestAuthorizeURLShape 授权页参数逐项核对：PKCE 用 SHA-256、state 透传、redirect_uri 固定。
func TestAuthorizeURLShape(t *testing.T) {
	u := buildAuthorizeURL("st-1", "chal-1")
	outer, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	svc := outer.Query().Get("service")
	if svc == "" {
		t.Fatalf("外层缺少 service 参数: %s", u)
	}
	inner, err := url.Parse(svc)
	if err != nil {
		t.Fatal(err)
	}
	q := inner.Query()
	want := map[string]string{
		"client_id":            "pdp5_for_agentarts",
		"code_challenge":       "chal-1",
		"code_challenge_method": "SHA-256",
		"state":                "st-1",
		"redirect_uri":         redirectURI,
		"response_type":        "code",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("授权参数 %s = %q 期望 %q", k, q.Get(k), v)
		}
	}
	if strings.Contains(u, "code_verifier") {
		t.Error("授权 URL 绝不能携带 code_verifier")
	}
}
