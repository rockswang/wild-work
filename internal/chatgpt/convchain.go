package chatgpt

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 多轮续接：(A') 历史前缀哈希链（本仓原创，spec §9）。
//
// 上游自己维护会话历史，我们每轮只发**最后一条 user 消息**（chat.go 的入站压平），
// 于是「这一轮接上一轮」只能由渠道自己认出来：
//
//   - 键 = 完整前缀的指纹（账号 + 本轮请求里除最后一条 user 之外的全部消息 +
//     上一轮我们下发给客户端的回复正文）。**不得**用首句（两个对话首句相同会撞键
//     串话），也**不得**用客户端自报的会话 id（v2 客户端没有这个概念）。
//   - 命中 ⇒ 把上一轮解析出的上游续接点（conversation_id + parent_message_id）带上；
//   - 未命中（首轮 / 客户端裁掉了历史 / 换了账号）⇒ **安静新建会话**，绝不回 4xx。
//
// 为什么键里要包含「上一轮回复正文」：客户端下一轮重放的历史是
// `[…, 上一轮 user, 我们下发的 assistant 正文, 新 user]`，只有把回复正文一起算进键，
// 「首句相同的两个不同对话」才会因为回复不同而落到不同条目上（不串话）。
//
// 落盘是**独立文件**（spec §7：`<data>/chatgpt-convs.json`）：账号池状态文件由
// `pool.saveLocked` 整文件覆写，把链塞进去会在下一次积分刷新时被抹掉（R143 血案）。

const (
	// convChainFileName 是前缀链的独立状态文件名。
	convChainFileName = "chatgpt-convs.json"
	// convChainMaxEntries 是条目上限：超出后淘汰最久未命中者（LRU）。
	convChainMaxEntries = 512
	// convChainVersion 是落盘格式版本。
	convChainVersion = 1
)

// convEntry 是一条前缀链记录：键（前缀指纹）→ 上游续接点。
//
// Created/LastHit 是 Unix 秒；LastHit 只用于 LRU 淘汰，不参与命中判定。
type convEntry struct {
	Key      string `json:"key"`
	ConvID   string `json:"conv_id"`
	ParentID string `json:"parent_id"`
	Created  int64  `json:"created"`
	LastHit  int64  `json:"last_hit"`
}

// convChainFile 是落盘形态（带版本号，便于将来只增字段）。
type convChainFile struct {
	Version int          `json:"version"`
	Entries []*convEntry `json:"entries"`
}

// convChain 是前缀链的内存态 + 落盘态。
//
// path 为空 ⇒ 纯内存（未配置 data 目录；单测/无盘场景），语义不变、只是重启会丢。
type convChain struct {
	mu       sync.Mutex
	path     string
	max      int
	loaded  bool
	dirty   bool
	entries map[string]*convEntry
}

// newConvChain 构造（path 为空 = 纯内存；max<=0 = 默认上限）。
// 加载是**惰性**的：第一次 Lookup/Put 才读盘，避免启动时多一次 I/O。
func newConvChain(path string, max int) *convChain {
	if max <= 0 {
		max = convChainMaxEntries
	}
	return &convChain{path: path, max: max, entries: map[string]*convEntry{}}
}

// Path 返回落盘路径（空 = 纯内存）。
func (cc *convChain) Path() string { return cc.path }

// Lookup 查前缀指纹（命中会刷新 LastHit，用于 LRU）。
func (cc *convChain) Lookup(key string) (convEntry, bool) { return cc.lookupAt(key, time.Now().Unix()) }

// Put 记一条前缀链（同键覆盖；超出上限则淘汰最久未命中的条目）。
func (cc *convChain) Put(key, convID, parentID string) {
	cc.putAt(key, convID, parentID, time.Now().Unix())
}

// lookupAt / putAt 接受显式时刻：单测需要可控的 LRU 顺序（秒级精度）。
func (cc *convChain) lookupAt(key string, now int64) (convEntry, bool) {
	if key == "" {
		return convEntry{}, false
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.loadLocked()
	e := cc.entries[key]
	if e == nil {
		return convEntry{}, false
	}
	if now > e.LastHit {
		e.LastHit = now
		cc.dirty = true
	}
	return *e, true
}

func (cc *convChain) putAt(key, convID, parentID string, now int64) {
	key, convID, parentID = strings.TrimSpace(key), strings.TrimSpace(convID), strings.TrimSpace(parentID)
	if key == "" || convID == "" || parentID == "" {
		return // 上游没给续接点就不记：下一轮安静新建，绝不写一条半边记录
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.loadLocked()
	if e := cc.entries[key]; e != nil {
		e.ConvID, e.ParentID, e.LastHit = convID, parentID, now
	} else {
		cc.entries[key] = &convEntry{Key: key, ConvID: convID, ParentID: parentID, Created: now, LastHit: now}
	}
	cc.evictLocked()
	cc.dirty = true
	cc.saveLocked()
}

// evictLocked 把条目数压到上限以内：淘汰 LastHit 最小者（同刻按 Created、再按键，保证确定）。
func (cc *convChain) evictLocked() {
	for len(cc.entries) > cc.max {
		var victim *convEntry
		for _, e := range cc.entries {
			if victim == nil || e.LastHit < victim.LastHit ||
				(e.LastHit == victim.LastHit && (e.Created < victim.Created ||
					(e.Created == victim.Created && e.Key < victim.Key))) {
				victim = e
			}
		}
		if victim == nil {
			return
		}
		delete(cc.entries, victim.Key)
	}
}

// Flush 把脏状态落盘（进程退出/客户端 Close 时调用）。
func (cc *convChain) Flush() {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.loadLocked()
	if cc.dirty {
		cc.saveLocked()
	}
}

// loadLocked 惰性读盘。文件缺失/损坏/形状不对一律**当作空链**：
// 多轮续接是锦上添花，绝不能因为一个坏文件把整条渠道打成不可用。
func (cc *convChain) loadLocked() {
	if cc.loaded {
		return
	}
	cc.loaded = true
	if cc.path == "" {
		return
	}
	b, err := os.ReadFile(cc.path)
	if err != nil {
		return
	}
	var f convChainFile
	if json.Unmarshal(b, &f) != nil {
		return
	}
	for _, e := range f.Entries {
		if e == nil || strings.TrimSpace(e.Key) == "" || strings.TrimSpace(e.ConvID) == "" ||
			strings.TrimSpace(e.ParentID) == "" {
			continue
		}
		cc.entries[e.Key] = e
	}
	cc.evictLocked()
}

// saveLocked 原子落盘（tmp + rename）；失败只保留脏标记（下次再试），不阻塞主流程。
func (cc *convChain) saveLocked() {
	if cc.path == "" {
		cc.dirty = false // 纯内存模式：没有可写的目标
		return
	}
	f := convChainFile{Version: convChainVersion}
	for _, e := range cc.entries {
		f.Entries = append(f.Entries, e)
	}
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cc.path), 0o755); err != nil {
		return
	}
	tmp := cc.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil { // 不是凭据，但含会话 id，按 0600
		return
	}
	if err := os.Rename(tmp, cc.path); err != nil {
		return
	}
	cc.dirty = false
}

// ── 指纹与续接 ────────────────────────────────────────────────────────────

// historyMsg 是请求消息的**规范化**形态（角色小写 + 文本）。
//
// 指纹必须走规范化后的文本而不是原始 JSON 字节：客户端下一轮可能把同一条消息写成
// `content:"…"` 或 `content:[{type:"text",text:"…"}]`，字节不等则永远不会命中。
type historyMsg struct {
	Role string
	Text string
}

// msgFingerprint 把「账号 + 消息序列」压成前缀键。
//
// 掺账号是刻意的：conversation_id 是**账号级**凭据，A 号的会话不能给 B 号续（换号后
// 命不中 ⇒ 安静新建，而不是拿别人的会话去打）。
func msgFingerprint(account string, msgs []historyMsg) string {
	h := sha256.New()
	_, _ = h.Write([]byte("chatgpt/conv/v1\x00"))
	_, _ = h.Write([]byte(account))
	for _, m := range msgs {
		_, _ = h.Write([]byte("\x00"))
		_, _ = h.Write([]byte(m.Role))
		_, _ = h.Write([]byte("\x00"))
		_, _ = h.Write([]byte(m.Text))
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:16])
}

// turnRecorder 是「本轮读完后把续接点落进链」的钩子。
//
// 为什么不在 ChatStream 里自己解析：分帧/补丁解析**只有 consumeTurn 一处**入口
// （Stream 与 Aggregate 共用），在别处再解一遍就是第二份逻辑（漂移风险）；
// 而且只有调用方真的读完才知道这轮是否成功（截断的回复绝不能当续接点）。
type turnRecorder interface {
	recordTurn(st *turnState, reply string, ok bool)
}

// convCapture 包住上游响应体，把本轮续接点在流读完时落进前缀链。
type convCapture struct {
	io.ReadCloser
	chain   *convChain
	account string
	// through 是本轮请求的消息（**含**最后一条 user）：落盘键 = 它 + 本轮回复正文。
	through []historyMsg
}

// recordTurn 实现 turnRecorder（ok=false ⇒ 本轮失败，什么都不记）。
func (cc *convCapture) recordTurn(st *turnState, reply string, ok bool) {
	if !ok || cc == nil || cc.chain == nil || st == nil {
		return
	}
	next := make([]historyMsg, 0, len(cc.through)+1)
	next = append(next, cc.through...)
	next = append(next, historyMsg{Role: "assistant", Text: reply})
	cc.chain.Put(msgFingerprint(cc.account, next), st.ConvID, st.MsgID)
}

// recordTurnOf 是出口侧的对称钩子（Stream/Aggregate 解析完后调用）。
//
// 用接口断言而不是把参数一路传下去：`provider.Upstream` 的 Stream/Aggregate 签名
// 是全仓共享契约（20+ 渠道），为一条渠道的续接点去改契约会波及所有渠道。
func recordTurnOf(r io.Reader, st *turnState, reply string, ok bool) {
	if rec, ok2 := r.(turnRecorder); ok2 {
		rec.recordTurn(st, reply, ok)
	}
}