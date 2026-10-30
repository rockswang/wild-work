package app

import (
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/config"
)

// 本文件锁定 Loomy 凭据 expiresAt 的**启动自愈**（healLoomyExpiry）：
// 历史版本的导入/登录路径把 expiresAt 写成了「估算值」（updatedAt+14d / now+14d），
// 而 Loomy 无 refresh 端点，估算值到期即触发必然失败的刷新（面板红字
// 「loomy: token refresh failed」）。自愈统一改写成远期哨兵，与 monkeycode/officeace 同口径。
//
// 全部写在 t.TempDir() 内，不触碰真实凭据目录。

func newHealTestApp(t *testing.T) *App {
	t.Helper()
	cfg := config.Default()
	cfg.AuthDir = t.TempDir()
	return &App{cfg: cfg}
}

// 估算的 14 天 expiresAt（旧口径）必须被自愈改写为远期哨兵。
func TestHealLoomyExpiryNormalizesGuessedExpiry(t *testing.T) {
	a := newHealTestApp(t)
	guessed := time.Now().Add(14 * 24 * time.Hour).Unix()
	if _, err := a.writeAuthFile("loomy", "u1", authDoc{
		Auth:    authSection{AccessToken: "sess-fake", ExpiresAt: guessed},
		Account: accountSection{UID: "u1"},
	}); err != nil {
		t.Fatalf("写 loomy 凭据失败：%v", err)
	}
	// 前置条件：估算值不应被当作远期哨兵（否则用例无效）。
	pre, err := auth.LoadLoomyDir(a.cfg.AuthDir)
	if err != nil || len(pre) != 1 {
		t.Fatalf("读取 loomy 凭据失败：err=%v n=%d", err, len(pre))
	}
	if isFarFutureExpiry(pre[0].ExpiresAt) {
		t.Fatal("前置条件错误：+14d 估算值不应被 isFarFutureExpiry 判为哨兵")
	}

	a.healLoomyExpiry()

	got, err := auth.LoadLoomyDir(a.cfg.AuthDir)
	if err != nil || len(got) != 1 {
		t.Fatalf("归一化后读取失败：err=%v n=%d", err, len(got))
	}
	if !isFarFutureExpiry(got[0].ExpiresAt) {
		t.Fatalf("估算 expiresAt 未被改写为远期哨兵：%d", got[0].ExpiresAt)
	}
	if got[0].ExpiresAt <= guessed {
		t.Fatalf("改写后的 expiresAt(%d) 应明显晚于估算值(%d)", got[0].ExpiresAt, guessed)
	}
	if got[0].AccessToken != "sess-fake" {
		t.Fatalf("自愈意外改动了 accessToken：%q", got[0].AccessToken)
	}
}

// 已是哨兵的凭据不得被改动（幂等）。
func TestHealLoomyExpiryLeavesSentinelUntouched(t *testing.T) {
	a := newHealTestApp(t)
	sentinel := time.Now().AddDate(50, 0, 0).Unix()
	if _, err := a.writeAuthFile("loomy", "u2", authDoc{
		Auth:    authSection{AccessToken: "sess-fake", ExpiresAt: sentinel},
		Account: accountSection{UID: "u2"},
	}); err != nil {
		t.Fatalf("写 loomy 凭据失败：%v", err)
	}

	a.healLoomyExpiry()

	got, err := auth.LoadLoomyDir(a.cfg.AuthDir)
	if err != nil || len(got) != 1 {
		t.Fatalf("读取失败：err=%v n=%d", err, len(got))
	}
	if got[0].ExpiresAt != sentinel {
		t.Fatalf("哨兵被误改：want %d, got %d", sentinel, got[0].ExpiresAt)
	}
}

// 自愈只作用于 loomy，不得误伤其它渠道的凭据。
func TestHealLoomyExpiryIgnoresOtherChannels(t *testing.T) {
	a := newHealTestApp(t)
	mcExpiry := time.Now().Add(14 * 24 * time.Hour).Unix()
	if _, err := a.writeAuthFile("monkeycode", "mc_x", authDoc{
		Auth:    authSection{AccessToken: "oma_fake", ExpiresAt: mcExpiry},
		Account: accountSection{UID: "mc_x"},
	}); err != nil {
		t.Fatalf("写 monkeycode 凭据失败：%v", err)
	}

	a.healLoomyExpiry()

	got, err := auth.LoadMonkeyCodeDir(a.cfg.AuthDir)
	if err != nil || len(got) != 1 {
		t.Fatalf("读取 monkeycode 失败：err=%v n=%d", err, len(got))
	}
	if got[0].ExpiresAt != mcExpiry {
		t.Fatalf("monkeycode 凭据被误改：want %d, got %d", mcExpiry, got[0].ExpiresAt)
	}
}
