package traework

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"wild-work/internal/auth"
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
	if gotReq.Load() != 1 {
		t.Fatalf("upstream 请求数=%d want 1", gotReq.Load())
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
