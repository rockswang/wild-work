package app

import (
	"path/filepath"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// newAliasApp 构造 TraeWork（主渠道）+ TraeCode（别名，复用同一 Pool）的最小 App。
// 用于回归 issue #36：共用账号池不得在 /api/state 账号列表中重复计数。
func newAliasApp(t *testing.T) (*App, *pool.Pool) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir

	trPool := pool.New(filepath.Join(dir, "state-traework.json"))
	trPool.Add(&auth.Auth{Kind: "traework", UID: "trae-1", Nickname: "一号", AccessToken: "t1", RefreshToken: "r1"})
	trPool.Add(&auth.Auth{Kind: "traework", UID: "trae-2", Nickname: "二号", AccessToken: "t2", RefreshToken: "r2"})
	trPool.Add(&auth.Auth{Kind: "traework", UID: "trae-3", Nickname: "三号", AccessToken: "t3", RefreshToken: "r3"})

	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.TraeWork: {Kind: provider.TraeWork, Pool: trPool, Upstream: &fakeUpstream{remain: 10}},
			provider.TraeCode: {Kind: provider.TraeCode, Pool: trPool, Upstream: &fakeUpstream{remain: 10}, Alias: true},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a, trPool
}

// TestAliasAccountsNotDuplicated 别名渠道（TraeCode）不得让共享账号在状态聚合中重复出现。
func TestAliasAccountsNotDuplicated(t *testing.T) {
	a, _ := newAliasApp(t)

	st := a.GetState()
	if len(st.Accounts) != 3 {
		t.Fatalf("账号数应为 3（去重后），实际 %d：%+v", len(st.Accounts), st.Accounts)
	}
	seen := map[string]int{}
	for _, v := range st.Accounts {
		seen[v.UID]++
		if v.Group != provider.TraeWork.String() {
			t.Errorf("uid=%s group 应为 traework，实际 %s", v.UID, v.Group)
		}
	}
	for uid, n := range seen {
		if n != 1 {
			t.Errorf("uid=%s 出现 %d 次，应仅 1 次", uid, n)
		}
	}
	if n := a.totalAccounts(); n != 3 {
		t.Errorf("totalAccounts 应为 3，实际 %d", n)
	}
}

// TestFindRuntimeAuthPrefersMainChannel 按 uid 反查必须命中主渠道（TraeWork），
// 否则账号级操作会误用别名渠道的 upstream（function=solo_agent）。
func TestFindRuntimeAuthPrefersMainChannel(t *testing.T) {
	a, _ := newAliasApp(t)

	rt, au := a.findRuntimeAuth("trae-1")
	if rt == nil || au == nil {
		t.Fatal("反查应命中账号")
	}
	if rt.Kind != provider.TraeWork {
		t.Errorf("反查应命中主渠道 traework，实际 %s", rt.Kind)
	}
	if g := a.accountGroup("trae-1"); g != provider.TraeWork.String() {
		t.Errorf("accountGroup 应为 traework，实际 %s", g)
	}
}
