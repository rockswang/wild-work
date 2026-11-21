package chatgpt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 前缀链存储层（convchain.go）的离线单测：命中/覆盖/上限淘汰/落盘恢复/坏文件兜底。
// 对话链路侧的「真的续上第二轮」由 convchain_wiring_test.go 覆盖。

// TestConvChainLookupAndPut 同键覆盖 + 未命中语义。
func TestConvChainLookupAndPut(t *testing.T) {
	cc := newConvChain("", 8)

	if _, ok := cc.Lookup("k0"); ok {
		t.Fatal("空链不该命中")
	}
	cc.putAt("k1", "c-1", "m-1", 100)
	e, ok := cc.Lookup("k1")
	if !ok || e.ConvID != "c-1" || e.ParentID != "m-1" {
		t.Fatalf("命中 = %+v ok=%v", e, ok)
	}

	// 同一前缀第二次建会话（客户端回到同一处重新开局）⇒ 覆盖而不是新增条目。
	cc.putAt("k1", "c-2", "m-2", 200)
	e, _ = cc.Lookup("k1")
	if e.ConvID != "c-2" || e.ParentID != "m-2" || e.Created != 100 {
		t.Fatalf("覆盖后 = %+v（Created 应保留首次创建时刻）", e)
	}
}

// TestConvChainRejectsHalfEntry 缺 conv_id / parent_id 的条目不写（否则下一轮带着半个续接点去打上游）。
func TestConvChainRejectsHalfEntry(t *testing.T) {
	cc := newConvChain("", 8)
	cc.putAt("k1", "c-1", "", 100)
	cc.putAt("", "c-1", "m-1", 100)
	if _, ok := cc.Lookup("k1"); ok {
		t.Fatal("缺 parent_id 的记录不该入链")
	}
}

// TestConvChainHitRefreshesLRU 命中刷新 LastHit：随后插入新条目时淘汰的是「最久未命中」的那个。
func TestConvChainHitRefreshesLRU(t *testing.T) {
	cc := newConvChain("", 2)
	cc.putAt("k1", "c-1", "m-1", 100)
	cc.putAt("k2", "c-2", "m-2", 200)
	if _, ok := cc.lookupAt("k1", 300); !ok { // k1 最近被命中
		t.Fatal("k1 该命中")
	}
	cc.putAt("k3", "c-3", "m-3", 400) // 超上限 ⇒ 淘汰 k2（LastHit=200 最小）

	if _, ok := cc.Lookup("k1"); !ok {
		t.Error("k1 刚被命中，不该被淘汰")
	}
	if _, ok := cc.Lookup("k2"); ok {
		t.Error("k2 是最久未命中者，应被淘汰")
	}
	if _, ok := cc.Lookup("k3"); !ok {
		t.Error("k3 刚写入，应还在")
	}
}

// TestConvChainEvictsToCap 条目数恒不超过上限（一次写很多也只留 max 条）。
func TestConvChainEvictsToCap(t *testing.T) {
	cc := newConvChain("", 3)
	for i, k := range []string{"a", "b", "c", "d", "e"} {
		cc.putAt(k, "c-"+k, "m-"+k, int64(100+i))
	}
	cc.mu.Lock()
	n := len(cc.entries)
	cc.mu.Unlock()
	if n != 3 {
		t.Fatalf("条目数 = %d，期望 3", n)
	}
	for _, k := range []string{"a", "b"} {
		if _, ok := cc.Lookup(k); ok {
			t.Errorf("%s 是最早的两条，应被淘汰", k)
		}
	}
}

// TestConvChainSurvivesRestart 重启后从落盘恢复（新对象读同一路径即可命中）。
func TestConvChainSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), convChainFileName)
	first := newConvChain(path, 8)
	first.putAt("k1", "c-1", "m-1", 100)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("没落盘: %v", err)
	}

	second := newConvChain(path, 8)
	e, ok := second.Lookup("k1")
	if !ok || e.ConvID != "c-1" || e.ParentID != "m-1" {
		t.Fatalf("重启后未恢复: %+v ok=%v", e, ok)
	}
}

// TestConvChainCorruptFileDegradesQuietly 坏文件当作空链（多轮是锦上添花，不能因此让渠道不可用）。
func TestConvChainCorruptFileDegradesQuietly(t *testing.T) {
	path := filepath.Join(t.TempDir(), convChainFileName)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	cc := newConvChain(path, 8)
	if _, ok := cc.Lookup("k1"); ok {
		t.Fatal("坏文件不该命中")
	}
	cc.putAt("k2", "c-2", "m-2", 100) // 写入仍然要能工作
	if _, ok := cc.Lookup("k2"); !ok {
		t.Fatal("坏文件恢复后仍应可写")
	}
	var f convChainFile
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &f) != nil {
		t.Fatalf("重写后的文件不是合法 JSON: %v", err)
	}
	if f.Version != convChainVersion || len(f.Entries) != 1 || f.Entries[0].Key != "k2" {
		t.Fatalf("落盘内容 = %+v", f)
	}
}

// TestMsgFingerprintDistinguishesPrefix 指纹判据：完整前缀参与、账号参与、顺序参与。
func TestMsgFingerprintDistinguishesPrefix(t *testing.T) {
	base := []historyMsg{{Role: "user", Text: "同一个问题"}}
	a1 := []historyMsg{{Role: "user", Text: "同一个问题"}, {Role: "assistant", Text: "甲"}}
	b1 := []historyMsg{{Role: "user", Text: "同一个问题"}, {Role: "assistant", Text: "乙"}}

	if msgFingerprint("uid:1", base) == msgFingerprint("uid:1", a1) {
		t.Fatal("首句相同但前缀不同必须不同键（否则两个对话互相串话）")
	}
	if msgFingerprint("uid:1", a1) == msgFingerprint("uid:1", b1) {
		t.Fatal("回复正文不同必须不同键")
	}
	if msgFingerprint("uid:1", a1) == msgFingerprint("uid:2", a1) {
		t.Fatal("账号不同必须不同键（会话 id 是账号级凭据）")
	}
	if msgFingerprint("uid:1", a1) != msgFingerprint("uid:1", a1) {
		t.Fatal("同输入必须同键")
	}
	if msgFingerprint("uid:1", a1) == msgFingerprint("uid:1", []historyMsg{{Role: "assistant", Text: "甲"}, {Role: "user", Text: "同一个问题"}}) {
		t.Fatal("顺序必须参与指纹")
	}
}
