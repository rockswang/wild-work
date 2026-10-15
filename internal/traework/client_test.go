package traework

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

func TestDailyCheckinClaimsWhenNotCheckedIn(t *testing.T) {
	var statusCalls, claimCalls atomic.Int32
	var checked atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT at" || r.Header.Get("X-User-Region") != "CN" {
			t.Errorf("missing Trae UG headers: auth=%q region=%q", r.Header.Get("Authorization"), r.Header.Get("X-User-Region"))
		}
		switch r.URL.Path {
		case EpCheckinStatus:
			statusCalls.Add(1)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"checked_in":%t,"credits":200,"enable":true}`, checked.Load())))
		case EpCheckinClaim:
			claimCalls.Add(1)
			checked.Store(true)
			_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New()
	c.CheckinRetryDelay = 0
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at", DeviceID: "device"}); err != nil {
		t.Fatalf("daily checkin: %v", err)
	}
	if statusCalls.Load() != 2 || claimCalls.Load() != 1 {
		t.Fatalf("status calls=%d claim calls=%d", statusCalls.Load(), claimCalls.Load())
	}
}

func TestDailyCheckinSkipsClaimWhenAlreadyCheckedIn(t *testing.T) {
	var claimCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCheckinStatus {
			_, _ = w.Write([]byte(`{"checked_in":true,"credits":200,"enable":true}`))
			return
		}
		if r.URL.Path == EpCheckinClaim {
			claimCalls.Add(1)
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err == nil || err.Error() != "已签到" {
		t.Fatalf("err=%v, want 已签到", err)
	}
	if claimCalls.Load() != 0 {
		t.Fatalf("claim calls=%d", claimCalls.Load())
	}
}

func TestCheckinClaimBusinessError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == EpCheckinClaim {
			calls.Add(1)
			_, _ = w.Write([]byte(`{"code":9074,"message":"operation too frequent"}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := New()
	c.CheckinRetryDelay = 0
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.CheckinClaim(&auth.Auth{AccessToken: "at"}); err == nil || !strings.Contains(err.Error(), "9074") {
		t.Fatalf("err=%v, want business 9074 error", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("claim calls=%d, want retry", calls.Load())
	}
}

func TestCheckinClaimRetriesRateLimit(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpCheckinClaim {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"code":9074,"message":"operation too frequent"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":0,"message":"success"}`))
	}))
	defer srv.Close()

	c := New()
	c.CheckinRetryDelay = 0
	c.HTTP = srv.Client()
	c.UgHost = srv.URL
	if err := c.CheckinClaim(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("retry claim: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("claim calls=%d", calls.Load())
	}
}

// TestStreamClientHasNoTotalTimeout 守门：流式必须走**无总超时**的 client。
//
// http.Client.Timeout 是整请求上限，SSE 整个生成期都在读 body ⇒ 长思考请求会被从流中间
// 掐断。非流式 client 必须保留总超时（短请求的合理兜底）。
func TestStreamClientHasNoTotalTimeout(t *testing.T) {
	c := New()
	if c.StreamHTTP == nil {
		t.Fatal("StreamHTTP 必须存在")
	}
	if c.StreamHTTP.Timeout != 0 {
		t.Fatalf("StreamHTTP.Timeout = %v，必须为 0（流式不能有整请求上限）", c.StreamHTTP.Timeout)
	}
	if c.HTTP == nil || c.HTTP.Timeout <= 0 {
		t.Fatalf("非流式 HTTP.Timeout = %v，必须 > 0", c.HTTP.Timeout)
	}
	// 无总超时后必须靠 ResponseHeaderTimeout 兜底，否则连响应头都等不到会无限挂住。
	tr, ok := c.StreamHTTP.Transport.(*http.Transport)
	if !ok || tr == nil {
		t.Fatalf("StreamHTTP.Transport = %T，应为 *http.Transport", c.StreamHTTP.Transport)
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Fatal("StreamHTTP 必须有 ResponseHeaderTimeout 兜底")
	}
}

// TestIdleTimeoutDefaults 未注入配置时回落默认值（装配漏注入不应退化成「无兜底」）。
func TestIdleTimeoutDefaults(t *testing.T) {
	c := New()
	if got := c.idleTimeout(); got != DefaultIdleTimeout {
		t.Fatalf("idleTimeout() = %v, want %v", got, DefaultIdleTimeout)
	}
	c.IdleTimeout = 33 * time.Second
	if got := c.idleTimeout(); got != 33*time.Second {
		t.Fatalf("idleTimeout() = %v, want 33s", got)
	}
}

// TestChatStreamWrapsIdleReader 守门：对话流返回体必须包空闲看门狗。
func TestChatStreamWrapsIdleReader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")
	}))
	defer srv.Close()
	c := &Client{HTTP: &http.Client{}, StreamHTTP: &http.Client{}, AgentHost: srv.URL, Function: Function}
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at"}, []byte(`{"model":"x","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()
	if _, ok := rc.(*provider.IdleReader); !ok {
		t.Fatalf("ChatStream 返回体未包 IdleReader（got %T），空闲卡死将无法兜底", rc)
	}
}

// TestSolosseStreamTruncationEmitsFrames 守门：流中断必须补一帧 error，且不得补 [DONE]。
//
// 此前读错误直接 return，客户端收到无收尾的截断流，只能一直等。但补 [DONE] 同样
// 不对（R36 / issue #42）：[DONE] 表示正常结束，与 error 帧一起发出等于把截断伪装
// 成成功收尾，客户端会拿着半截 tool_call arguments 去解析。
func TestSolosseStreamTruncationEmitsFrames(t *testing.T) {
	in := "event:output\ndata:{\"response\":\"hi\"}\n\n"
	rc := &errAfterReader{data: in, err: provider.ErrIdleTimeout}

	rec := httptest.NewRecorder()
	err := Stream(rec, rc)
	if err == nil {
		t.Fatal("读错误必须上抛给 handler（用于日志）")
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"upstream_timeout"`) {
		t.Fatalf("缺少 upstream_timeout 错误帧: %s", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("截断不得补 [DONE]: %s", body)
	}
	if !strings.Contains(body, "hi") {
		t.Fatalf("已透传内容丢失: %s", body)
	}
}

// errAfterReader 先吐完 data，再返回指定错误（模拟「读到一半流断了」）。
type errAfterReader struct {
	data string
	err  error
	off  int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, r.err
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}

// TestFetchModelsHandlesLargeCatalog 目录接口响应可能超过 1MB —— 实测 solo_agent
// 的 get_detail_param 返回 1.28MB，旧上限（1<<20）会把 JSON 截成半截，解析失败后
// 静默回退静态兜底表（面板只显示 16 个模型，issue #41）。
//
// 本用例用一个 >1MB 的合法响应断言完整解析：若上限被改回 1MB，这里会因
// "models parse" 失败而红。
func TestFetchModelsHandlesLargeCatalog(t *testing.T) {
	// 造一个超过 1MB 的合法目录：条目数 + 每条填充让总量跨过 1<<20。
	const filler = 4096
	names := make([]string, 0, 300)
	for i := 0; len(names) < 300; i++ {
		fill := strings.Repeat("x", filler)
		names = append(names, fmt.Sprintf(`{"config_name":"model-%03d-%s","display_config":{"display_name":"M%03d"}}`, i, fill, i))
	}
	body := `{"config_info_list":[` + strings.Join(names, ",") + `]}`
	if len(body) <= 1<<20 {
		t.Fatalf("测试数据没超过 1MB（%d），用例失去意义", len(body))
	}

	var gotReq atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != EpModels {
			http.NotFound(w, r)
			return
		}
		gotReq.Add(1)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.AgentHost = srv.URL

	out, err := c.FetchModels(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("FetchModels 应能解析 >1MB 目录，却失败：%v", err)
	}
	if len(out) != 300 {
		t.Fatalf("模型数=%d want 300（响应 %d 字节）", len(out), len(body))
	}
	// 双池合并：FetchModels 会对两个 function 各拉一次（同路径）
	if gotReq.Load() != 2 {
		t.Fatalf("upstream 请求数=%d want 2", gotReq.Load())
	}
}

// TestFetchModelsRejectsOversizedResponse 超过 maxJSONBody 的响应必须显式报错，
// 而不是被截成半截后由 json.Unmarshal 抛语法错误 —— 后者会把「我们自己截断了」
// 伪装成「上游发了坏 JSON」，正是 issue #41 长期没被发现的原因。
func TestFetchModelsRejectsOversizedResponse(t *testing.T) {
	// 合法但超限的 JSON（用空白填充，避免构造出非法 JSON 干扰判断）。
	body := `{"config_info_list":[` + strings.Repeat(" ", maxJSONBody+1024) + `]}`
	if len(body) <= maxJSONBody {
		t.Fatalf("测试数据没超过上限（%d）", len(body))
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.AgentHost = srv.URL

	_, err := c.FetchModels(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("超限响应应报错")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("应为显式超限错误，实际：%v", err)
	}
	if strings.Contains(err.Error(), "models parse") {
		t.Fatalf("不应退化成 JSON 解析错误：%v", err)
	}
}
