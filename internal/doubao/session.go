// session.go 会话焚化（随聊随删模式，2026-11-15 用户拍板，取代会话式复用）。
//
// 策略：每轮请求都是新会话（need_create_conversation=true + 全量压平），流结束后
// 0.8s 后台焚化豆包侧会话（/samantha/thread/delete）——豆包侧不留档（隐私最优）。
// 纯聊天场景的代价：每轮都携带完整历史（pi 无状态客户端每轮重发），用户已接受；
// 工具循环轮本来就是全量压平（文本协议决定），随删不增加任何成本。
//
// store 结构保留的原因：SSE_ACK 捕获 conversation_id 的入口在流 reader 里，
// 需要一个 (key→conv) 暂存点把 conv id 交给焚化回调；不再用于复用。
// 设计照搬 SeiShonagon520/doubao2api 即用即焚（temp/doubao-tools-delete-research2.md
// Part B）：流结束 +0.8s 后台删、失败仅日志不重试、删后清缓存。
package doubao

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"wild-work/internal/auth"
)

// 会话缓存参数（aimod-cc/Agent2API 同款思路：TTL + FIFO 容量，防长期运行泄漏）。
const (
	// sessionTTL 会话缓存过期时间。
	sessionTTL = 24 * time.Hour
	// sessionMaxKeys 容量上限，满时按最久未用淘汰。
	sessionMaxKeys = 2048
)

// sessionState 一个会话的上游锚点。
type sessionState struct {
	ConversationID string
	SectionID      string
	at             time.Time // 最近一次使用（TTL 与容量淘汰的依据）
}

// sessionStore 进程内会话缓存。key = accountUID + "|" + conversationKey。
type sessionStore struct {
	mu    sync.Mutex
	items map[string]sessionState
}

// 全局唯一实例：会话生命周期与进程一致，无需按渠道装配多份。
var globalSessions = &sessionStore{items: map[string]sessionState{}}

// lookup 命中并 touch；过期/容量淘汰惰性触发。
func (s *sessionStore) lookup(key string) (sessionState, bool) {
	if key == "" {
		return sessionState{}, false
	}
	s.mu.Lock()
	st, ok := s.items[key]
	if !ok {
		s.mu.Unlock()
		return sessionState{}, false
	}
	if time.Since(st.at) > sessionTTL {
		s.mu.Unlock()
		// 过期即孤儿：后台焚化豆包侧会话（不持锁调，forget 自己拿锁）
		s.forget(key)
		return sessionState{}, false
	}
	st.at = time.Now()
	s.items[key] = st
	s.mu.Unlock()
	return st, true
}

// remember 记录/更新会话锚点。conv 为空或占位 "0" 不记（SeiShonagon520 同款过滤）。
func (s *sessionStore) remember(key, conv, section string) {
	conv = strings.TrimSpace(conv)
	if key == "" || conv == "" || conv == "0" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// 容量治理：满时先淘汰过期，仍满再删最久未用。
	if _, exists := s.items[key]; !exists && len(s.items) >= sessionMaxKeys {
		var oldestKey string
		var oldest time.Time
		first := true
		for k, v := range s.items {
			if time.Since(v.at) > sessionTTL {
				// 过期即孤儿：先摘再焚化（不持锁调，goroutine 异步）
				conv := v.ConversationID
				delete(s.items, k)
				if conv != "" {
					go deleteConversationAsync(conv, k)
				}
				continue
			}
			if first || v.at.Before(oldest) {
				oldestKey, oldest, first = k, v.at, false
			}
		}
		if len(s.items) >= sessionMaxKeys && oldestKey != "" {
			// 容量淘汰的也是孤儿会话：焚化
			conv := s.items[oldestKey].ConversationID
			delete(s.items, oldestKey)
			if conv != "" {
				go deleteConversationAsync(conv, oldestKey)
			}
		}
	}
	st := s.items[key]
	st.ConversationID, st.SectionID, st.at = conv, section, time.Now()
	s.items[key] = st
}

// forget 清除一个会话并触发后台焚化（fire-and-forget）。
//
// 触发点 = 「这会话不会再被用了」的所有路径：TTL 过期、容量淘汰、复用失败降级。
// 活跃会话（缓存命中）不受影响——焚化与复用混合：客户端管理的活跃会话保留，
// 孤儿会话（过期/淘汰/降级后不再续用的）焚掉，不留档（temp/doubao-tools-delete-
// research2.md Part B：SeiShonagon520 / kitsch-9527 / ChatLink 三个独立实现同款）。
//
// 失败仅记日志（不重试、不影响主流程）；成功后无需回调——缓存条目本来就删了。
func (s *sessionStore) forget(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	st, ok := s.items[key]
	delete(s.items, key)
	s.mu.Unlock()
	if ok && st.ConversationID != "" {
		go deleteConversationAsync(st.ConversationID, key)
	}
}

// deleteClientFactory 焚化 client 工厂（测试可覆盖 baseURL 指向 httptest）。
var deleteClientFactory = func() *Client { return New() }

// deleteConversationAsync 后台删豆包侧会话（/samantha/thread/delete）。
//
// 协议（调研报告 B2）：body {"thread_id": conv_id}；query 身份参数照 kitsch 全套；
// 头必带 Cookie + x-tt-passport-csrf-token。key 是缓存键（accountUID|convKey），
// 从中取 UID 反查凭据——查不到凭据则静默放弃（账号可能已被删）。
// 延迟 0.8s：等上游落库，避免删竞态（SeiShonagon520 同款）。
func deleteConversationAsync(convID, key string) {
	time.Sleep(800 * time.Millisecond)
	uid := key
	if i := strings.Index(key, "|"); i >= 0 {
		uid = key[:i]
	}
	acct := lookupAuthByUID(uid)
	if acct == nil {
		return
	}
	c := deleteClientFactory()
	if err := c.deleteConversation(acct, convID); err != nil {
		log.Printf("doubao conversation delete failed uid=%s conv=%s（不影响主流程，会话留在豆包侧）err=%v",
			uid, maskConvID(convID), err)
	} else {
		log.Printf("doubao conversation deleted uid=%s conv=%s", uid, maskConvID(convID))
	}
}

// lookupAuthByUID 按 UID 查凭据（由 auth 层注入的回调；焚化在后台 goroutine 里，
// 不持有 pool 锁）。
var lookupAuthByUID = func(uid string) *auth.Auth { return nil }

// SetAuthLookup 注入按 UID 查凭据的回调（main.go 装配时调用）。
func SetAuthLookup(fn func(uid string) *auth.Auth) { lookupAuthByUID = fn }

// maskConvID 会话 id 脱敏展示（只留前 6 后 4）。
func maskConvID(id string) string {
	if len(id) <= 10 {
		return id
	}
	return id[:6] + "…" + id[len(id)-4:]
}

// sessionKeyOf 组合缓存键：账号 UID + 会话推导键。
//
// 掺 UID 的理由见包注释：conversation_id 是账号作用域的，不掺 UID 会在粘性路由
// 换号后拿旧号的 conv id 去新号上撞。
func sessionKeyOf(accountUID, convKey string) string {
	if convKey == "" {
		return ""
	}
	return accountUID + "|" + convKey
}

// conversationKeyOf 从客户端 OpenAI 请求推导稳定会话键（yyydsz 同款优先级）。
//
// 稳定性要求：同一个无状态客户端（pi/Codex）的同一线程每轮必须命中同一个键；
// 隔离性要求：不同线程不得串会话。故首选客户端显式 id，其次 user 字段，
// 最后退到「首条 user 消息哈希」——pi 每轮重发完整历史时首条 user 不变，
// 天然是线程标识；整包哈希（r:）只作最后兜底（对 pi 这类每轮变化的 body 会
// 永远 miss，等于退回旧压平行为，无害但不理想）。
func conversationKeyOf(rawBody []byte) string {
	var req struct {
		User     string         `json:"user"`
		Metadata map[string]any `json:"metadata"`
		Messages []chatMessage  `json:"messages"`
	}
	if json.Unmarshal(rawBody, &req) != nil {
		return bodyHashKey(rawBody)
	}
	// ① 客户端显式会话 id（metadata.conversation_id / conversationId / session_id）
	if req.Metadata != nil {
		for _, k := range []string{"conversation_id", "conversationId", "session_id"} {
			if v, ok := req.Metadata[k].(string); ok && v != "" {
				return "c:" + v
			}
		}
	}
	// ② user 字段
	if u := strings.TrimSpace(req.User); u != "" {
		return "u:" + u
	}
	// ③ 首条 user 消息哈希（无状态客户端每轮重发历史 ⇒ 首条不变 = 线程标识）
	for _, m := range req.Messages {
		if m.Role == "user" {
			if t := strings.TrimSpace(m.Text()); t != "" {
				sum := sha256.Sum256([]byte(t))
				return "h:" + hex.EncodeToString(sum[:12])
			}
			break
		}
	}
	// ④ 整包哈希兜底
	return bodyHashKey(rawBody)
}

func bodyHashKey(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "r:" + hex.EncodeToString(sum[:12])
}
