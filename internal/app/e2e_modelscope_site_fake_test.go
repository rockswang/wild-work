// e2e_modelscope_site_fake_test.go ModelScope **主站**假上游（魔粒余额/流水/今日用量/每日领取）。
//
// 为什么与 e2e_modelscope_fake_test.go 分两份：真实上游的额度面挂在**主站**
// （modelscope.cn / modelscope.ai），与对话基址（api-inference.modelscope.*）是**两个 host**
// （credits.go D7）。分成两个 httptest 服务器才能验证「双 host 双鉴权」这件事本身——
// 合成一个 server 时，把 balance 错发到对话基址（或漏配 SiteBase）也照样通过。
//
// 四个面照抄探针实测形状（.scratch/modelscope-credits/probe-findings.md）：
//
//	GET  /openapi/v1/magicubes/balance        Bearer  → {"success":true,"data":{total_balance,available_balance,frozen_amount}}
//	GET  /openapi/v1/magicubes/transactions   Bearer  → {"success":true,"data":{page,page_size,total,records[]}}
//	GET  /api/v1/inference/rate-limit         Cookie  → {"Code":200,"Data":{"currentUsagePerDay":N},"Success":true}（首字母大写信封）
//	GET  /api/v1/users/login/info             Cookie  → 幂等「领取」动作；本层在发放额 >0 时追加一条当日 EARN
//
// 计数器是核心证据面：票 04 的「领取前先复读流水」只有数请求才能验（claimed 必须打出
// login/info，already_claimed 必须**一次都不打**）。

package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// msSiteEarn 一条 EARN 流水（金额 + 上游原格式的到期串）。
type msSiteEarn struct {
	GmtCreated string
	Amount     int64
	ExpireAt   string // "2006-01-02 15:04:05"（UTC+8 墙钟，无时区后缀）
}

type msBalance struct {
	Total     int64
	Available int64
	Frozen    int64
}

// msSite 主站假上游（两区共用一台，按 token 区分账号——token 本身就是账号标识）。
type msSite struct {
	mu sync.Mutex

	// tokens 非 nil 时只接受这些凭据（模拟「区域不通用」）。
	tokens map[string]bool

	balance   map[string]msBalance
	balStatus map[string]int

	tx       map[string][]msSiteEarn
	txStatus map[string]int

	usage       map[string]int64
	usageStatus map[string]int

	// grant 领取时发放的魔粒（>0 才追加当日 EARN；0 = 上游这天没有可领的活动）。
	grant map[string]int64
	// loginStatus 按 token 定制 login/info 的状态码（0 = 200）。
	loginStatus map[string]int
	// loginCalls 每个 token 被调了几次 login/info。
	loginCalls map[string]int

	reqs []msReq
	srv  *httptest.Server
}

const msSiteExpireLayout = "2006-01-02 15:04:05"

// msCreditLoc 魔粒墙钟口径（与 internal/modelscope 的 creditLoc 同源：UTC+8）。
var msCreditLoc = time.FixedZone("UTC+8", 8*60*60)

func newMSSite(t *testing.T) *msSite {
	t.Helper()
	s := &msSite{
		balance:     map[string]msBalance{},
		balStatus:   map[string]int{},
		tx:          map[string][]msSiteEarn{},
		txStatus:    map[string]int{},
		usage:       map[string]int64{},
		usageStatus: map[string]int{},
		grant:       map[string]int64{},
		loginStatus: map[string]int{},
		loginCalls:  map[string]int{},
	}
	s.srv = httptest.NewServer(s.handler())
	t.Cleanup(s.srv.Close)
	return s
}

func (s *msSite) URL() string { return s.srv.URL }

// acceptOnly 限定可接受的凭据集合。
func (s *msSite) acceptOnly(tokens ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = make(map[string]bool, len(tokens))
	for _, t := range tokens {
		s.tokens[t] = true
	}
}

// setBalance 设置某账号的余额三元组。
func (s *msSite) setBalance(token string, total, available, frozen int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balance[token] = msBalance{Total: total, Available: available, Frozen: frozen}
}

// setBalanceStatus 让某账号的余额端点回指定状态码（0 = 恢复正常）。
func (s *msSite) setBalanceStatus(token string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balStatus[token] = status
}

// setTxStatus 让某账号的流水端点回指定状态码（0 = 正常）。
// 用于验证「transactions 失败只降级、不把余额刷新搞挂」。
func (s *msSite) setTxStatus(token string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.txStatus[token] = status
}

// setUsage 设置今日用量（token 计）。
func (s *msSite) setUsage(token string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usage[token] = n
}

// setUsageStatus 让今日用量端点回指定状态码（0 = 正常）。
func (s *msSite) setUsageStatus(token string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usageStatus[token] = status
}

// setGrant 设置「领取时发放多少魔粒」（0 = 当日无活动 ⇒ no_campaign）。
func (s *msSite) setGrant(token string, amount int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.grant[token] = amount
}

// setLoginStatus 让 login/info 回指定状态码（0 = 200）。
func (s *msSite) setLoginStatus(token string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loginStatus[token] = status
}

// addEarn 预置一条 EARN（可指定 gmt_created，用于「当日已领取」与「非当日」两类场景）。
func (s *msSite) addEarn(token string, e msSiteEarn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(e.GmtCreated) == "" {
		e.GmtCreated = time.Now().In(msCreditLoc).Format("2006-01-02")
	}
	s.tx[token] = append(s.tx[token], e)
}

// addEarnIn 预置一条「days 天后到期」的 EARN（发放日 = 今天，便于断言逐笔到期）。
func (s *msSite) addEarnIn(token string, amount int64, d time.Duration) {
	s.addEarn(token, msSiteEarn{
		Amount:   amount,
		ExpireAt: time.Now().Add(d).In(msCreditLoc).Format(msSiteExpireLayout),
	})
}

// loginInfoCount 某账号的 login/info 调用次数（票 04 三态判据）。
func (s *msSite) loginInfoCount(token string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loginCalls[token]
}

// reqsByPath 取某路径的全部请求快照。
func (s *msSite) reqsByPath(path string) []msReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]msReq, 0, len(s.reqs))
	for _, r := range s.reqs {
		if r.Path == path {
			out = append(out, r)
		}
	}
	return out
}

// total 收到的请求总数。
func (s *msSite) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func (s *msSite) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, msReq{
		Method: r.Method, Path: r.URL.Path,
		Token:  bearerOrCookie(r),
		Header: r.Header.Clone(),
	})
}

// bearerOrCookie 主站两种鉴权形态各取一份（断言「双 host 双鉴权」用）。
func bearerOrCookie(r *http.Request) string {
	if v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); v != "" {
		return v
	}
	if c := r.Header.Get("Cookie"); c != "" {
		for _, kv := range strings.Split(c, ";") {
			kv = strings.TrimSpace(kv)
			if strings.HasPrefix(kv, "m_session_id=") {
				return strings.TrimPrefix(kv, "m_session_id=")
			}
		}
	}
	return ""
}

func (s *msSite) authorized(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		return true
	}
	return s.tokens[token]
}

func (s *msSite) handler() http.Handler {
	mux := http.NewServeMux()

	// 魔粒余额（Bearer）。小写信封。
	mux.HandleFunc("/openapi/v1/magicubes/balance", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		tok := bearerOrCookie(r)
		if !s.authorized(tok) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"code":"401","message":"invalid access token"}`))
			return
		}
		s.mu.Lock()
		status, bal := s.balStatus[tok], s.balance[tok]
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"success":false,"message":"balance unavailable"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
			"total_balance": bal.Total, "available_balance": bal.Available, "frozen_amount": bal.Frozen,
		}})
	})

	// 魔粒逐笔流水（Bearer）。EARN 行逐笔带 expire_at。
	mux.HandleFunc("/openapi/v1/magicubes/transactions", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		tok := bearerOrCookie(r)
		if !s.authorized(tok) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"success":false,"code":"401"}`))
			return
		}
		s.mu.Lock()
		status := s.txStatus[tok]
		earns := append([]msSiteEarn(nil), s.tx[tok]...)
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"success":false,"message":"transactions unavailable"}`))
			return
		}
		records := make([]map[string]any, 0, len(earns))
		for _, e := range earns {
			records = append(records, map[string]any{
				"gmt_created": e.GmtCreated, "type": "EARN", "rule_key": "daily_active",
				"total_amount": e.Amount, "count": 1, "expire_at": e.ExpireAt,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": map[string]any{
			"page": 1, "page_size": 20, "total": len(records), "records": records,
		}})
	})

	// 今日用量（Cookie）。**首字母大写**旧式信封——与 balance 的小写不同，两个端点各解析一份。
	mux.HandleFunc("/api/v1/inference/rate-limit", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		tok := bearerOrCookie(r)
		if !s.authorized(tok) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"Code":401,"Success":false}`))
			return
		}
		s.mu.Lock()
		status, usage := s.usageStatus[tok], s.usage[tok]
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"Code":500,"Success":false}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Code": 200, "Message": "success", "Success": true,
			"Data": map[string]any{"currentUsagePerDay": usage},
		})
	})

	// 每日登录/领取（Cookie）：幂等。发放额 > 0 时追加一条当日 EARN——这正是上游
	// 「领取」之后流水里才出现当日发放的真实形态（checkin.go 的三态判据全靠它）。
	mux.HandleFunc("/api/v1/users/login/info", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		tok := bearerOrCookie(r)
		s.mu.Lock()
		s.loginCalls[tok]++
		status, amount := s.loginStatus[tok], s.grant[tok]
		s.mu.Unlock()
		if !s.authorized(tok) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"Code":401,"Success":false}`))
			return
		}
		if status >= 400 {
			w.WriteHeader(status)
			_, _ = fmt.Fprintf(w, `{"Code":%d,"Success":false,"Message":"login info failed"}`, status)
			return
		}
		if amount > 0 {
			s.addEarn(tok, msSiteEarn{
				Amount:   amount,
				ExpireAt: time.Now().Add(24 * time.Hour).In(msCreditLoc).Format(msSiteExpireLayout),
			})
		}
		// 响应体里**没有**任何魔粒字段（探针 §4.1 实证）——三态只能借道流水。
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Code": 200, "Success": true, "Message": "success",
			"Data": map[string]any{"userId": strings.TrimPrefix(tok, "ms-"), "requestId": "req-e2e"},
		})
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"success":false,"message":"not found"}`)
	})

	return mux
}
