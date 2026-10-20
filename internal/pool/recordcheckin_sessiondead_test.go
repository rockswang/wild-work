package pool

import (
	"testing"
	"time"

	"wild-work/internal/auth"
)

// TestRecordCheckinFailureMarksSessionDead 回归测试（issue #157）：
// 签到失败且消息是「登录已失效」形态时，必须
//   ① last_checkin_ok = false（面板不能显示绿色「签到成功」）；
//   ② 打上 session_dead 标记（面板「需重新登录」徽标 + 挑号跳过）。
//
// 修复前此用例必失败：RecordCheckin 里 `if e, ok := p.byUID[uid]; ok` 的 ok
// 遮蔽了函数参数 ok bool ⇒ 恒为 true ⇒ lastCheckinOK 恒真、sessionDead 被清、
// `else if sessionDeadLike(msg)` 成死代码。
func TestRecordCheckinFailureMarksSessionDead(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.RecordCheckin("u1", false, "登录已失效，请重新登录（refresh failed: xxx）；余额查询失败")

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 应存在")
	}
	if st.LastCheckinOK {
		t.Errorf("last_checkin_ok 应为 false，实为 true（面板会显示绿色「签到成功」）")
	}
	if !st.SessionDead {
		t.Errorf("失败且消息含「登录已失效」时应标记 session_dead，实为未标记")
	}
	if st.SessionDeadReason == "" {
		t.Error("session_dead_reason 应保留失败原因")
	}

	// 反过来：一次失败的「非登录失效」消息（如网络抖动）不应误标 session_dead，
	// 但 last_checkin_ok 仍必须是 false —— 防止修遮蔽时把两条语义一起改坏。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u2"})
	p2.RecordCheckin("u2", false, "网络超时")
	st2, _ := p2.Status("u2")
	if st2.LastCheckinOK {
		t.Error("网络超时也应记为 last_checkin_ok=false")
	}
	if st2.SessionDead {
		t.Error("普通失败不应标记 session_dead")
	}
}

// TestRecordCheckinSuccessClearsSessionDead 成功签到要能清掉标记（#134 的另一半语义）。
func TestRecordCheckinSuccessClearsSessionDead(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u3"})
	p.MarkSessionDead("u3", "401 upstream")

	p.RecordCheckin("u3", true, "签到成功")

	st, _ := p.Status("u3")
	if !st.LastCheckinOK {
		t.Error("成功签到应记 last_checkin_ok=true")
	}
	if st.SessionDead {
		t.Error("成功签到应清除 session_dead")
	}
}

// TestMarkSessionDeadKeepsTimeAnchor 已标记时更新原因、但**不刷新时间锚**。
//
// 两个独立要求，方向相反，极易在后续改动中互相踩坏：
//   - 原因必须更新：失效原因会演进（`refresh: xxx` → 上游 `12153:refresh token is invalid`），
//     旧实现 `if e.sessionDead { return }` 让面板 tooltip 永远显示首次的旧原因（issue #157 附带瑕疵）；
//   - 时间锚（sessionDeadAt）**不得更新**：它是「重登后自动清除」的判据
//     （clearReloggedLocked 比较 auth 文件 mtime 与它）。若每次失败都刷成 now，用户在
//     重登后因任何一条陈旧路径再失败一次，锚就跑到 auth 文件之后 ⇒ 标记永不清除，
//     账号被永久跳过——正是 #134 修掉的死锁。
//
// 同一语义在 RecordCheckin 的失败分支里也要成立（与本函数同口径）。
func TestMarkSessionDeadKeepsTimeAnchor(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})

	p.MarkSessionDead("u1", "refresh: first")
	first, _ := p.Status("u1")
	if !first.SessionDead || first.SessionDeadAt.IsZero() {
		t.Fatal("首次标记必须同时写入原因与时间锚")
	}
	anchor := first.SessionDeadAt

	time.Sleep(5 * time.Millisecond) // 让「刷新时间」与「保留时间」可区分（Windows 时钟粒度约 1ms）
	p.MarkSessionDead("u1", "12153:refresh token is invalid")
	second, _ := p.Status("u1")
	if second.SessionDeadReason != "12153:refresh token is invalid" {
		t.Errorf("失效原因应更新为最新值，实为 %q", second.SessionDeadReason)
	}
	if !second.SessionDeadAt.Equal(anchor) {
		t.Errorf("时间锚不得刷新：首次 %v，现在 %v（会把重登后的自动清除判据顶到文件之后）",
			anchor, second.SessionDeadAt)
	}

	// RecordCheckin 的失败分支同口径：已标记时只更新原因。
	time.Sleep(5 * time.Millisecond)
	p.RecordCheckin("u1", false, "登录已失效，请重新登录（refresh failed: 12153）")
	third, _ := p.Status("u1")
	if !third.SessionDead {
		t.Fatal("登录失效形态的签到失败必须保持 session_dead")
	}
	if !third.SessionDeadAt.Equal(anchor) {
		t.Errorf("签到失败不得刷新时间锚：首次 %v，现在 %v", anchor, third.SessionDeadAt)
	}
}
