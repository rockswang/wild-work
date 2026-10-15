// cache_test.go 缓存利用率聚合口径回归。
//
// 三条必须锁死的语义：
//  1. **只把「上报了该维度」的请求计入分母**——未上报的请求整条排除，
//     不能用「不知情」的样本稀释命中率；
//  2. cr_cov==0 ⇒ 不产出任何缓存数字（面板显示「—」），而不是 0%；
//  3. **旧流水（无 cr/crc 字段）向后兼容**：读出来就是「未上报」，不报错。
package ledger

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestQueryCacheUtilization 上报/未上报混合时只按上报样本算（加总口径）。
func TestQueryCacheUtilization(t *testing.T) {
	dir := t.TempDir()
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	// 三个请求：两个上报（命中 80/100、0/300），一个未上报（100 pt，不计入）
	l.AppendUsage(UsageEntry{Ch: "workbuddy", UID: "u1", Model: "m1", PT: 100, CT: 10, CR: 80, CRC: true})
	l.AppendUsage(UsageEntry{Ch: "workbuddy", UID: "u1", Model: "m1", PT: 300, CT: 20, CR: 0, CRC: true})
	l.AppendUsage(UsageEntry{Ch: "glm", UID: "u2", Model: "m2", PT: 100, CT: 30}) // 未上报

	st := l.Query(1, nil)
	// 加总口径：命中 80 / 分母 400 = 20%
	if st.Token.CR != 80 {
		t.Errorf("全渠道 CR=%d want 80", st.Token.CR)
	}
	if st.Token.CRCov != 400 {
		t.Errorf("全渠道 CRCov=%d want 400（未上报的 100 不得进分母）", st.Token.CRCov)
	}
	if st.Token.CRReqs != 2 {
		t.Errorf("上报请求数=%d want 2", st.Token.CRReqs)
	}

	// 按模型：m1 有样本(80/400)，m2 无样本(0/0) —— 后者必须在面板显示「—」
	byModel := map[string]ModelStat{}
	for _, m := range st.Token.ByModel {
		byModel[m.Model] = m
	}
	if m := byModel["m1"]; m.CR != 80 || m.CRCov != 400 {
		t.Errorf("m1 cr/cr_cov = %d/%d want 80/400", m.CR, m.CRCov)
	}
	if m := byModel["m2"]; m.CRCov != 0 || m.CR != 0 {
		t.Errorf("m2 应无缓存样本，实际 cr/cr_cov = %d/%d（前端据此显示「—」）", m.CR, m.CRCov)
	}
	// 未上报请求的 token 仍要进总量（缓存维度不影响原有统计）
	if st.Token.PT != 500 {
		t.Errorf("PT=%d want 500（缓存列不得动原有统计）", st.Token.PT)
	}
}

// TestQueryOldEntriesBackwardCompatible 旧流水（无 cr/crc 键）必须被读成「未上报」。
// 不变量 5「data/*.json 只增不减字段，向后兼容」的落地验证。
func TestQueryOldEntriesBackwardCompatible(t *testing.T) {
	dir := t.TempDir()
	// 手写一份「旧版本格式」的流水（只有 ts/ch/uid/model/pt/ct/src）
	old := `{"ts":` + itoa(time.Now().Unix()) + `,"ch":"workbuddy","uid":"u1","model":"m1","pt":120,"ct":30,"src":"upstream"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, monthName(time.Now())+".jsonl"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	// 保留 New 的迁移/裁剪行为：确认旧行在 New 的裁剪里不被误删
	l, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	st := l.Query(1, nil)
	if st.Token.PT != 120 || st.Token.CT != 30 {
		t.Fatalf("旧流水统计丢失：pt=%d ct=%d", st.Token.PT, st.Token.CT)
	}
	if st.Token.CRCov != 0 || st.Token.CR != 0 {
		t.Errorf("旧流水应无缓存样本（读成未上报），实际 cr/cov=%d/%d", st.Token.CR, st.Token.CRCov)
	}
	// 关键：不能因为缺 cr/crc 就报错或整行丢弃（Requests 仍计 1）
	if st.Token.Requests != 1 {
		t.Errorf("旧流水 Requests=%d want 1", st.Token.Requests)
	}
}

// TestUsageEntryJSONOmitsZeroCache CRC=false 时不得写出 cr/crc 键——
// 与旧版本产出的行**逐字节相同**，避免「升级后所有旧行看起来都变了」。
func TestUsageEntryJSONOmitsZeroCache(t *testing.T) {
	raw, err := json.Marshal(UsageEntry{Ts: 1, Ch: "c", UID: "u", Model: "m", PT: 1, CT: 2, Src: "upstream"})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if _, has := m["cr"]; has {
		t.Error("未上报时不应写出 cr 键（老格式兼容）")
	}
	if _, has := m["crc"]; has {
		t.Error("未上报时不应写出 crc 键（老格式兼容）")
	}
}

// TestUsageEntryJSONRoundTripReportedZero 「上报但命中 0」必须能落盘并原样读回。
// 这是「0%」与「—」可区分的前提：若 crc/0 被 omitempty 吃掉，就退化成「未上报」。
func TestUsageEntryJSONRoundTripReportedZero(t *testing.T) {
	raw, err := json.Marshal(UsageEntry{Ts: 1, Ch: "c", UID: "u", Model: "m", PT: 10, CT: 2, CR: 0, CRC: true})
	if err != nil {
		t.Fatal(err)
	}
	var back UsageEntry
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !back.CRC {
		t.Fatalf("CRC=true 未落盘（cr=%d crc=%v raw=%s）", back.CR, back.CRC, raw)
	}
	if back.CR != 0 {
		t.Fatalf("CR=%d want 0", back.CR)
	}
}

func monthName(t time.Time) string { return "usage-" + t.Format("200601") }

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
