package app

import (
	"strconv"
	"testing"
	"time"
)

// 本文件锁定 **auth.expiresAt 与「手里这批凭据」必须自洽**（改进1，2026-10-17 用户实测要求）。
//
// 现场（用户实例 C:\软件存储\wild-work\auths\officeace-01a140491….json）：
//
//	auth.expiresAt      = 3370173304  ← 2076 年（导入写的「+50 年远期哨兵」）
//	extra.cred_expires  = 1792257230  ← 2026-10-18 01:13（真实临时凭据，约 2h）
//	extra.refresh_expire= 1794842030  ← 2026-11-16（refresh_token 到期，+30 天）
//
// 自相矛盾的后果：`auth.NeedsRefresh()` 只看 expiresAt ⇒ 对 officeace **永不触发**；
// 真正在兜底的是 refresh.go 的 `ensureCredential`（另按 cred_expires 判定）。两者长期不一致，
// 面板上的账号健康状态也就不可读。根因是合并规则在「没有 cred_expires 可对齐」时回落到
// **旧的 expiresAt**——而旧值恰恰就是哨兵 ⇒ 哨兵被永久固化。
//
// 三个测试分别锁：① 有临时凭据 ⇒ 用 cred_expires；② 无临时凭据但有 refresh_token ⇒ 用
// refresh_expire（不再固化哨兵）；③ 纯网关 Basic ⇒ 才允许哨兵（且保持确定性）。
// 全部假值，写在 t.TempDir()，不触碰真实凭据目录（AGENTS.md §0.2 / §0.1）。

// ④a 有临时凭据：expiresAt 必须等于 cred_expires（而不是导入侧的 +50 年哨兵）。
func TestOfficeAceImportAlignsExpiresAtToCredExpires(t *testing.T) {
	a := newOfficeAceImportApp(t)
	uid := "acct-align:user-align"
	apiHost := "https://gw-align.example.invalid/v2"

	credExpires := time.Now().Add(2 * time.Hour).Unix()
	prev := oauthLoginDoc(uid, apiHost, "rt-fake-align-0001", credExpires)
	if _, err := a.writeAuthFile("officeace", uid, prev); err != nil {
		t.Fatalf("准备旧凭据失败：%v", err)
	}

	imported := importedDoc(uid, apiHost, time.Now().AddDate(50, 0, 0).Unix())
	file, _, err := a.writeOfficeAceAuth(uid, imported)
	if err != nil {
		t.Fatalf("合并导入失败：%v", err)
	}
	got, _ := readOfficeAceFile(t, file)

	if got.Auth.ExpiresAt != credExpires {
		t.Errorf("expiresAt = %d，want %d（= extra.cred_expires，即手里这批临时凭据的到期）",
			got.Auth.ExpiresAt, credExpires)
	}
	if isFarFutureExpiry(got.Auth.ExpiresAt) {
		t.Errorf("落盘后 expiresAt 仍是远期哨兵（%d）——与 cred_expires=%d 自相矛盾",
			got.Auth.ExpiresAt, credExpires)
	}
	// 反向验证（已做）：把 mergeOfficeAceDoc 里 `if ce := parseUnixSec(...); ce > 0` 分支删掉，
	// 本用例立即红（expiresAt 变回 +50 年哨兵）。
}

// ④b 没有临时凭据、但保留了 refresh_token：expiresAt 取 refresh_expire，
// **不得**沿用旧的哨兵值（这正是现场那个账号被固化的路径）。
func TestOfficeAceImportAlignsToRefreshExpireWhenNoTempCredential(t *testing.T) {
	a := newOfficeAceImportApp(t)
	uid := "acct-noak:user-noak"
	apiHost := "https://gw-noak.example.invalid/v2"

	now := time.Now().Unix()
	refreshExpire := now + 30*24*3600 // refresh_token 还有 30 天
	// 旧文件：有 refresh_token 与 refresh_expire，但临时凭据已被上游轮换掉（AK/SK 空），
	// 而 expiresAt 是**上一版导入留下的哨兵**（现场形态）。
	prev := authDoc{
		Auth: authSection{
			AccessToken:  fakeBasic("noak"),
			RefreshToken: "rt-fake-noak-0003",
			ExpiresAt:    time.Now().AddDate(50, 0, 0).Unix(), // 哨兵（旧导入写的）
			ApiHost:      apiHost,
			Extra: map[string]string{
				"base_url":       apiHost,
				"account_id":     "acct-fake",
				"user_id":        "user-fake",
				"region":         "cn-southwest-2",
				"access_key":     "", // 临时凭据已被轮换掉
				"secret_key":     "",
				"security_token": "",
				"refresh_expire": strconv.FormatInt(refreshExpire, 10),
				"dpop_priv":      `{"kty":"EC","crv":"P-256","kid":"fake"}`,
				"source":         "disk",
			},
		},
		Account: accountSection{UID: uid},
	}
	if _, err := a.writeAuthFile("officeace", uid, prev); err != nil {
		t.Fatalf("准备旧凭据失败：%v", err)
	}

	imported := importedDoc(uid, apiHost, time.Now().AddDate(50, 0, 0).Unix())
	file, info, err := a.writeOfficeAceAuth(uid, imported)
	if err != nil {
		t.Fatalf("合并导入失败：%v", err)
	}
	got, _ := readOfficeAceFile(t, file)

	if !info.KeptRefreshToken {
		t.Fatalf("前置条件不成立：应当保留旧 refresh_token（info=%+v）", info)
	}
	if got.Auth.ExpiresAt != refreshExpire {
		t.Errorf("expiresAt = %d，want %d（= extra.refresh_expire）——不得回落到 +50 年哨兵而把它永久固化",
			got.Auth.ExpiresAt, refreshExpire)
	}
	// 反向验证（已做）：把 tier② 分支去掉（回到「沿用旧 expiresAt」），本用例变红
	// （expiresAt 会等于旧的哨兵值）。
}

// ④c 纯网关 Basic 导入（既无临时凭据、也无 refresh_token）：
// 哨兵是**正确**取值——网关 Basic 确实不过期，写它可避免误触必然失败的刷新。
//
// 另外两条口径（都是反向验证**抓出来的覆盖缺口**，补上后才算真守住）：
//  1. tier② 必须同时要求「有 refresh_expire」**且**「有 refresh_token」——
//     只有 refresh_expire 而没有 refresh_token 是**无意义状态**（续期入口本身就不存在，
//     写一个「30 天后要续期」的值只会误导）。变异「去掉 refresh_token 非空判断」在本用例
//     加入前后都不红，说明当时没测到；现补 refresh_expire 但无 refresh_token 的形态。
//  2. 确定性：同一导入文档两次落盘结果必须一致。历史写法每次重算 now+50 年，**跨秒**会差
//     1 秒；故此处显式**跨过一秒边界**再写第二次，把「跨秒漂移」变成确定性可复现的失败，
//     而不是靠运气。
func TestOfficeAceImportKeepsSentinelForGatewayOnly(t *testing.T) {
	a := newOfficeAceImportApp(t)
	uid := "acct-gwonly:user-gwonly"
	apiHost := "https://gw-only.example.invalid/v2"

	importedExpires := time.Now().AddDate(50, 0, 0).Unix()
	imported := importedDoc(uid, apiHost, importedExpires)
	file, _, err := a.writeOfficeAceAuth(uid, imported)
	if err != nil {
		t.Fatalf("全新导入失败：%v", err)
	}
	got, _ := readOfficeAceFile(t, file)
	if got.Auth.ExpiresAt != importedExpires {
		t.Errorf("纯网关 Basic 导入应原样保留哨兵：got=%d want=%d", got.Auth.ExpiresAt, importedExpires)
	}
	if !isFarFutureExpiry(got.Auth.ExpiresAt) {
		t.Errorf("纯网关 Basic 导入的 expiresAt 应被判为远期哨兵：%d", got.Auth.ExpiresAt)
	}

	// 口径 1：**合并路径**下，只有 refresh_expire 而没有 refresh_token ⇒ 仍应保留哨兵
	// （不得采用 refresh_expire）。
	//
	// 注意必须走合并：tier② 的条件含 `info.KeptRefreshToken`，而该标志只在「旧文件里有
	// refresh_token、本次导入没带」时才为真 —— 全新写入到不了这里（反向验证第一版就是因为
	// 这一点而没测到：变异打上了却没人红）。
	{
		uid2 := "acct-gwonly-no-rt:user-gwonly-no-rt"
		refreshExpire := time.Now().Unix() + 30*24*3600
		// 旧文件：expiresAt 已是过期时间（**非哨兵**）、没有临时凭据、**没有 refresh_token**，
		// 但残留一个 refresh_expire。
		prev2 := authDoc{
			Auth: authSection{
				AccessToken:  fakeBasic("no-rt"),
				RefreshToken: "",
				ExpiresAt:    time.Now().Unix() - 3600, // 已过期且不是哨兵
				ApiHost:      apiHost,
				Extra:        map[string]string{"base_url": apiHost, "refresh_expire": strconv.FormatInt(refreshExpire, 10)},
			},
			Account: accountSection{UID: uid2},
		}
		if _, err := a.writeAuthFile("officeace", uid2, prev2); err != nil {
			t.Fatalf("准备旧文件（无 refresh_token）失败：%v", err)
		}
		imported2 := importedDoc(uid2, apiHost, importedExpires)
		file2, _, err := a.writeOfficeAceAuth(uid2, imported2)
		if err != nil {
			t.Fatalf("导入（无 refresh_token 但有 refresh_expire）失败：%v", err)
		}
		got2, _ := readOfficeAceFile(t, file2)
		if got2.Auth.ExpiresAt != importedExpires {
			t.Errorf("无 refresh_token 时不得采用 refresh_expire：expiresAt=%d want=%d（哨兵）",
				got2.Auth.ExpiresAt, importedExpires)
		}
		// 反向验证（已做）：把 tier② 的 `&& strings.TrimSpace(out.Auth.RefreshToken) != ""`
		// 去掉，本段变红（expiresAt 会变成 refresh_expire）。
	}

	// 口径 2：确定性——跨过一秒边界再写一次，结果必须与首次完全一致。
	time.Sleep(1100 * time.Millisecond)
	file3, _, err := a.writeOfficeAceAuth(uid, imported)
	if err != nil {
		t.Fatalf("二次导入失败：%v", err)
	}
	got3, _ := readOfficeAceFile(t, file3)
	if got3.Auth.ExpiresAt != got.Auth.ExpiresAt {
		t.Errorf("跨秒重写后 expiresAt 漂移：%d → %d（说明用「重算 now+50 年」而非沿用导入值）",
			got.Auth.ExpiresAt, got3.Auth.ExpiresAt)
	}
	// 反向验证（已做）：把 else 分支改成 `officeAceImportedSentinel()`（重算 now+50 年），
	// 本段因跨秒确定性变红。
}

// ④d 历史遗留文件：哨兵 + 真实凭据并存时，**一次「从客户端导入」就要把它纠正回来**。
// 这条覆盖「文件早于本次修复就被写脏」的存量账号（现场那个账号正是这种）。
//
// 为什么用「导入」而不是「登录后写盘」来测：登录路径写出的文档里 expiresAt 本身就等于
// cred_expires（officeace/account.go 的 toAuth），不存在哨兵可纠；真正会把哨兵写下去的
// 只有导入路径，而**纠正它的位置就是合并规则**（早先我曾在 writeOfficeAceAuth 末尾加过一段
// 「落盘护栏」，反向验证证明那是**走不到的死代码**——合并已经办完了，故删掉，只留一处真相）。
func TestOfficeAceMergeCorrectsLegacySentinel(t *testing.T) {
	a := newOfficeAceImportApp(t)
	uid := "acct-legacy:user-legacy"
	apiHost := "https://gw-legacy.example.invalid/v2"

	now := time.Now().Unix()
	credExpires := now + 2*3600
	// 直接写入一份「带哨兵但临时凭据齐全」的脏文件（模拟旧版合并的产物）。
	dirty := oauthLoginDoc(uid, apiHost, "rt-fake-legacy-0004", credExpires)
	dirty.Auth.ExpiresAt = time.Now().AddDate(50, 0, 0).Unix()
	if _, err := a.writeAuthFile("officeace", uid, dirty); err != nil {
		t.Fatalf("准备脏文件失败：%v", err)
	}
	// 前置：脏文件确实是哨兵（确保用例真的在测这条路径，而不是自证式断言）。
	if pre, _ := readOfficeAceFile(t, a.authFilePath("officeace", uid)); !isFarFutureExpiry(pre.Auth.ExpiresAt) {
		t.Fatalf("前置条件不成立：脏文件 expiresAt=%d 不是哨兵", pre.Auth.ExpiresAt)
	}

	// 走真实导入路径：导入文档只带网关 Basic，临时凭据与被写脏的 expiresAt 都留在旧文件里。
	file, info, err := a.writeOfficeAceAuth(uid, importedDoc(uid, apiHost, time.Now().AddDate(50, 0, 0).Unix()))
	if err != nil {
		t.Fatalf("合并导入失败：%v", err)
	}
	got, _ := readOfficeAceFile(t, file)

	if !info.KeptCredential {
		t.Fatalf("前置条件不成立：应当保留旧临时凭据（info=%+v）", info)
	}
	if got.Auth.ExpiresAt != credExpires {
		t.Errorf("存量哨兵未被纠正：expiresAt = %d，want %d（= 保留凭据的 extra.cred_expires）",
			got.Auth.ExpiresAt, credExpires)
	}
	if isFarFutureExpiry(got.Auth.ExpiresAt) {
		t.Errorf("合并后 expiresAt 仍是远期哨兵（%d）", got.Auth.ExpiresAt)
	}
	// 反向验证（已做）：删掉 mergeOfficeAceDoc 的 tier①（cred_expires 对齐），本用例变红。
}

// ④e 「有临时凭据却缺 cred_expires」不得落哨兵（2026-10-18 独立复核抓出的 HIGH 缺口）。
//
// 现实产生者：`officeace/account.go` 只在 `CredExpiresAt>0` 时才写 `cred_expires`，而
// login.go 的 `parseISO(credentials.expiration)`、档案的 `expires_at` 都可能解析不出来 ⇒
// 「AK/SK 在手、到期时刻未知」。此时写 +50 年等于宣称一个约 2h 就过期的凭据永不过期，
// 而 `NeedsRefresh()` 也就永不触发。约定：取一个**短线**到期（1 小时），尽快让续期去补真值。
func TestOfficeAceKeepsCredButNoExpiryOutOfSentinel(t *testing.T) {
	a := newOfficeAceImportApp(t)
	uid := "acct-cred-noexp:user-cred-noexp"
	apiHost := "https://gw-cred-noexp.example.invalid/v2"

	// 旧文件被直接写入：临时凭据齐全，但**没有** cred_expires（模拟解析失败）。
	prev := authDoc{
		Auth: authSection{
			AccessToken:  fakeBasic("cred-noexp"),
			RefreshToken: "rt-fake-cred-noexp-0001",
			// 注意：这个旧 expiresAt 是真值（2h 内），不是哨兵——用来确认新分支不被旧值左右。
			ExpiresAt: time.Now().Unix() + 600,
			ApiHost:   apiHost,
			Extra: map[string]string{
				"base_url":       apiHost,
				"access_key":     "AKFAKEREAL",
				"secret_key":     "SKFAKEREAL",
				"security_token": "STSFAKEREAL",
				"dpop_priv":      `{"kty":"EC","crv":"P-256","kid":"fake"}`,
				"source":         "oauth-login",
			},
		},
		Account: accountSection{UID: uid},
	}
	if _, err := a.writeAuthFile("officeace", uid, prev); err != nil {
		t.Fatalf("准备旧凭据失败：%v", err)
	}

	imported := importedDoc(uid, apiHost, time.Now().AddDate(50, 0, 0).Unix())
	file, info, err := a.writeOfficeAceAuth(uid, imported)
	if err != nil {
		t.Fatalf("合并导入失败：%v", err)
	}
	got, _ := readOfficeAceFile(t, file)

	if !info.KeptCredential {
		t.Fatalf("前置条件不成立：应当保留旧临时凭据（info=%+v）", info)
	}
	if isFarFutureExpiry(got.Auth.ExpiresAt) {
		t.Errorf("手里有真实 AK/SK 时不得落远期哨兵（会宣称它永不过期）：expiresAt=%d", got.Auth.ExpiresAt)
	}
	// 短线到期：应落在「现在 + 1 小时」附近（允许 5 分钟执行窗口）。
	want := time.Now().Unix() + officeAceCredWithoutExpiryGrace
	if d := got.Auth.ExpiresAt - want; d > 300 || d < -300 {
		t.Errorf("缺 cred_expires 时应取短线到期（now+%d），实际 %d（差 %d 秒）",
			officeAceCredWithoutExpiryGrace, got.Auth.ExpiresAt, d)
	}
	// 反向验证（已做）：把 tier① 的条件改成恒假，本用例变红（会落到哨兵分支）。
}

// ④f tier③ 取更早的真值：旧 expiresAt 是真值（2h）而 refresh_expire 在 30 天后时，
// **不得**把 expiresAt 往后推（那会制造「欠刷新窗口」）。
func TestOfficeAceImportKeepsEarlierRealExpiry(t *testing.T) {
	a := newOfficeAceImportApp(t)
	uid := "acct-earlier:user-earlier"
	apiHost := "https://gw-earlier.example.invalid/v2"

	now := time.Now().Unix()
	prevExp := now + 2*3600           // 旧真值：2 小时后
	refreshExpire := now + 30*24*3600 // refresh_token：30 天后
	// 旧文件：**没有**临时凭据（access_key 空）、**没有** cred_expires，但有真值 expiresAt
	// 与 refresh_expire，且保留了 refresh_token。
	prev := authDoc{
		Auth: authSection{
			AccessToken:  fakeBasic("earlier"),
			RefreshToken: "rt-fake-earlier-0001",
			ExpiresAt:    prevExp,
			ApiHost:      apiHost,
			Extra: map[string]string{
				"base_url":       apiHost,
				"access_key":     "", // 临时凭据已被上游轮换掉
				"secret_key":     "",
				"refresh_expire": strconv.FormatInt(refreshExpire, 10),
				"dpop_priv":      `{"kty":"EC","crv":"P-256","kid":"fake"}`,
				"source":         "disk",
			},
		},
		Account: accountSection{UID: uid},
	}
	if _, err := a.writeAuthFile("officeace", uid, prev); err != nil {
		t.Fatalf("准备旧凭据失败：%v", err)
	}

	file, _, err := a.writeOfficeAceAuth(uid, importedDoc(uid, apiHost, time.Now().AddDate(50, 0, 0).Unix()))
	if err != nil {
		t.Fatalf("合并导入失败：%v", err)
	}
	got, _ := readOfficeAceFile(t, file)

	if got.Auth.ExpiresAt != prevExp {
		t.Errorf("应取更早的真值：expiresAt = %d，want %d（旧真值）而非 refresh_expire=%d",
			got.Auth.ExpiresAt, prevExp, refreshExpire)
	}
	// 反向验证（已做）：把 tier③ 改成直接取 refresh_expire，本用例变红。
}

// ④g 判定函数本身：边界口径（>10 年为哨兵；0/负值不算哨兵）。
func TestIsFarFutureExpiry(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		name string
		exp  int64
		want bool
	}{
		{"0（无到期时间）不算哨兵", 0, false},
		{"负值不算哨兵", -1, false},
		{"已过期的时间戳不算哨兵", now - 3600, false},
		{"真实凭据（2h）不算哨兵", now + 2*3600, false},
		{"真实 refresh_token（30 天）不算哨兵", now + 30*24*3600, false},
		{"恰好 10 年（边界）不算哨兵", now + officeAceSentinelMinSeconds, false},
		{"10 年 + 1 秒算哨兵", now + officeAceSentinelMinSeconds + 1, true},
		{"导入写的 +50 年哨兵算哨兵", now + 50*365*24*3600, true},
		{"现场那个账号的绝对值也算哨兵", 3370173304, true},
	}
	for _, c := range cases {
		if got := isFarFutureExpiry(c.exp); got != c.want {
			t.Errorf("%s：isFarFutureExpiry(%d) = %t，want %t", c.name, c.exp, got, c.want)
		}
	}
}
