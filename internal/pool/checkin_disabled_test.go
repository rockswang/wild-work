package pool

import (
	"path/filepath"
	"testing"

	"wild-work/internal/auth"
)

// TestCheckinDisabledPersists 账号级签到暂停必须落 state.json，重启后仍是暂停态。
func TestCheckinDisabledPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})

	p.SetCheckinDisabled("u1", true)
	if st, _ := p.Status("u1"); !st.CheckinDisabled {
		t.Fatal("设置后应立刻生效")
	}
	if st, _ := p.Status("u2"); st.CheckinDisabled {
		t.Fatal("未设置的账号不该被暂停")
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Add(&auth.Auth{UID: "u2"})
	if st, _ := p2.Status("u1"); !st.CheckinDisabled {
		t.Fatal("重启后暂停态丢失")
	}
	if st, _ := p2.Status("u2"); st.CheckinDisabled {
		t.Fatal("未暂停账号重启后被误标暂停")
	}

	// 恢复也要持久化；且与路由停用互不影响。
	p2.SetCheckinDisabled("u1", false)
	p3 := New(fp)
	p3.Add(&auth.Auth{UID: "u1"})
	if st, _ := p3.Status("u1"); st.CheckinDisabled {
		t.Fatal("恢复签到的状态未持久化")
	}
}

// TestCheckinDisabledIndependentOfDisabled 签到暂停不影响账号作为 API 上游被选用。
func TestCheckinDisabledIndependentOfDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCheckinDisabled("u1", true)
	if picked := p.Pick(); picked == nil || picked.UID != "u1" {
		t.Fatal("暂停签到不该让账号退出 API 路由")
	}
	st, _ := p.Status("u1")
	if st.Disabled {
		t.Fatal("签到暂停不得改写账号停用状态")
	}
	p.SetDisabled("u1", true)
	if st, _ = p.Status("u1"); !st.Disabled || !st.CheckinDisabled {
		t.Fatalf("两个开关应互不覆盖，实际 %+v", st)
	}
}
