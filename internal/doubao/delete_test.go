package doubao

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
)

func overrideDeleteClient(t *testing.T, url string) {
	t.Helper()
	old := deleteClientFactory
	t.Cleanup(func() { deleteClientFactory = old })
	deleteClientFactory = func() *Client {
		c := New()
		c.baseURL = url
		return c
	}
}

// TestForgetTriggersDeleteAsync forget 触发后台焚化：假上游收到 /samantha/thread/delete，
// body {"thread_id": conv_id}，头带 csrf。
func TestForgetTriggersDeleteAsync(t *testing.T) {
	var mu sync.Mutex
	var gotBody string
	var gotCSRF string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/samantha/thread/delete") {
			w.WriteHeader(404)
			return
		}
		mu.Lock()
		gotCSRF = r.Header.Get("x-tt-passport-csrf-token")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		mu.Unlock()
		w.Write([]byte(`{"code":0}`))
	}))
	defer srv.Close()
	overrideDeleteClient(t, srv.URL)

	// 注入凭据查找（指向假上游的 Client）
	old := lookupAuthByUID
	defer func() { lookupAuthByUID = old }()
	lookupAuthByUID = func(uid string) *auth.Auth {
		if uid != "del-uid" {
			return nil
		}
		a := &auth.Auth{Kind: "doubao", UID: uid,
			Extra: map[string]string{ExtraCookie: "sessionid=abc; passport_csrf_token=tok123"}}
		return a
	}

	s := newIsolatedStore()
	s.remember("del-uid|k1", "conv-to-delete", "")
	s.forget("del-uid|k1")
	// forget 应立即清缓存
	if _, ok := s.lookup("del-uid|k1"); ok {
		t.Fatal("forget 后缓存应清空")
	}
	// 后台焚化有 0.8s 延迟
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		b := gotBody
		mu.Unlock()
		if b != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotBody == "" {
		t.Fatal("焚化请求未到达假上游")
	}
	var body map[string]string
	if json.Unmarshal([]byte(gotBody), &body) != nil || body["thread_id"] != "conv-to-delete" {
		t.Fatalf("delete body 形状错误：%s", gotBody)
	}
	if gotCSRF != "tok123" {
		t.Fatalf("csrf 头缺失/错误：%q", gotCSRF)
	}
}

// TestLookupMissNoPanic 凭据查不到（账号已删）时焚化静默放弃。
func TestLookupMissNoPanic(t *testing.T) {
	old := lookupAuthByUID
	defer func() { lookupAuthByUID = old }()
	lookupAuthByUID = func(uid string) *auth.Auth { return nil }
	s := newIsolatedStore()
	s.remember("ghost|k", "conv-x", "")
	s.forget("ghost|k") // 不应 panic
	time.Sleep(1100 * time.Millisecond) // 等 0.8s 延迟 + 余量
}

// TestTTLExpiryTriggersDelete TTL 过期 = 孤儿会话 → 焚化。
func TestTTLExpiryTriggersDelete(t *testing.T) {
	var mu sync.Mutex
	deleted := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/samantha/thread/delete") {
			mu.Lock()
			deleted = "yes"
			mu.Unlock()
			w.Write([]byte(`{"code":0}`))
		}
	}))
	defer srv.Close()
	overrideDeleteClient(t, srv.URL)

	old := lookupAuthByUID
	defer func() { lookupAuthByUID = old }()
	lookupAuthByUID = func(uid string) *auth.Auth {
		return &auth.Auth{Kind: "doubao", UID: uid, Extra: map[string]string{ExtraCookie: "sessionid=x"}}
	}
	s := newIsolatedStore()
	s.items["ttl-uid|k"] = sessionState{ConversationID: "conv-old", at: time.Now().Add(-sessionTTL - time.Minute)}
	if _, ok := s.lookup("ttl-uid|k"); ok {
		t.Fatal("过期应 miss")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		d := deleted
		mu.Unlock()
		if d != "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("TTL 过期未触发焚化")
}

// TestCapacityEvictionTriggersDelete 容量淘汰 = 孤儿会话 → 焚化。
func TestCapacityEvictionTriggersDelete(t *testing.T) {
	var mu sync.Mutex
	deleted := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/samantha/thread/delete") {
			mu.Lock()
			deleted = "yes"
			mu.Unlock()
			w.Write([]byte(`{"code":0}`))
		}
	}))
	defer srv.Close()
	overrideDeleteClient(t, srv.URL)

	old := lookupAuthByUID
	defer func() { lookupAuthByUID = old }()
	lookupAuthByUID = func(uid string) *auth.Auth {
		return &auth.Auth{Kind: "doubao", UID: uid, Extra: map[string]string{ExtraCookie: "sessionid=x"}}
	}
	s := newIsolatedStore()
	// 灌满（最久未用的一条带 conv id）
	s.items["victim|k"] = sessionState{ConversationID: "conv-victim", at: time.Now().Add(-time.Hour)}
	for i := 0; i < sessionMaxKeys-1; i++ {
		s.remember("f"+string(rune('a'+i%26))+string(rune('a'+i/26)), "c", "")
	}
	// 插入新键触发容量治理
	s.remember("new-key", "new-conv", "")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		d := deleted
		mu.Unlock()
		if d != "" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("容量淘汰未触发焚化")
}

// TestDeleteConversationUpstreamError 上游报错（code!=0 / HTTP 4xx）不重试不影响主流程。
func TestDeleteConversationUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte(`{"code":1100,"msg":"denied"}`))
	}))
	defer srv.Close()
	c := New()
	c.baseURL = srv.URL
	a := &auth.Auth{Kind: "doubao", Extra: map[string]string{ExtraCookie: "sessionid=x"}}
	if err := c.deleteConversation(a, "conv-err"); err == nil {
		t.Fatal("HTTP 403 必须报错（但不重试）")
	}
}
