package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/config"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// 本文件锁定 **「缺 refresh_token」的持久徽标**（改进2，2026-10-17 用户实测要求）。
//
// 现场：OfficeAce 的凭据分两层——网关 Basic（不过期，对话用）与临时 AK/SK（约 2h，积分/签到用）。
// 「从客户端导入」只拿得到前者，后者必须由 refresh_token 经 DPoP 换取 ⇒ 完全没有
// refresh_token 的账号**永久无法积分/签到**，且**不会自愈**（不像 401 那样能 refresh 回来）。
// 旧版只在导入那一刻弹一次提示文字，事后面板上看起来一切正常 ⇒ 用户无从复查。
//
// 反向验证（已做，见交付说明）：把 refreshTokenMissing 改成恒返回 false，本用例变红；
// 把 app.js 的 missingRT 改成恒 true，前端那 4 条断言变红。两侧都由测试守住。

// newOfficeAceBadgeApp 构造一个含 OfficeAce 渠道的最小 App。
// accounts 的 refresh_token 由调用方决定（本文件只关心「有/无」两种形态）。
func newOfficeAceBadgeApp(t *testing.T) (*App, *pool.Pool) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.StateFile = filepath.Join(dir, "state.json")
	cfg.AuthDir = dir

	oaPool := pool.New(filepath.Join(dir, "state-officeace.json"))
	// 缺 refresh_token 的账号（真实形态：只有网关 Basic）。
	oaPool.Add(&auth.Auth{
		Kind: string(provider.OfficeAce), UID: "acct-missing:user-missing",
		Nickname: "缺凭据号", AccessToken: "Basic ZmFrZQ==", RefreshToken: "",
	})
	// 正常的账号（有 refresh_token）。
	oaPool.Add(&auth.Auth{
		Kind: string(provider.OfficeAce), UID: "acct-ok:user-ok",
		Nickname: "正常号", AccessToken: "Basic ZmFrZTI=", RefreshToken: "rt-fake-ok-0001",
	})
	// refresh_token 非空但**已被上游拒过**（在死名单里）；续期必然失败，故同样该亮徽标。
	// 2026-10-18 独立复核指出：只判「字段为空」会让这种账号在面板上看起来完全正常。
	oaPool.Add(&auth.Auth{
		Kind: string(provider.OfficeAce), UID: "acct-dead:user-dead",
		Nickname: "死凭据号", AccessToken: "Basic ZmFrZTM=", RefreshToken: "rt-fake-dead-0002",
		Extra: map[string]string{"dead_tokens": "rt-fake-dead-0000,rt-fake-dead-0002"},
	})
	// 别的渠道（无 refresh 概念）：不得被误标。
	trPool := pool.New(filepath.Join(dir, "state-traework.json"))
	trPool.Add(&auth.Auth{Kind: "traework", UID: "trae-1", Nickname: "别家号", AccessToken: "t1", RefreshToken: ""})

	a, err := New(Options{
		ConfigPath: filepath.Join(dir, "config.json"),
		Config:     cfg,
		Runtimes: map[provider.Kind]*Runtime{
			provider.OfficeAce: {Kind: provider.OfficeAce, Pool: oaPool, Upstream: &fakeUpstream{remain: 10}},
			provider.TraeWork:  {Kind: provider.TraeWork, Pool: trPool, Upstream: &fakeUpstream{remain: 10}},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(a.Close)
	return a, oaPool
}

// refreshTokenMissing 的判定口径：只有「OfficeAce 且真的没有 refresh_token」才为真。
func TestRefreshTokenMissing(t *testing.T) {
	a, _ := newOfficeAceBadgeApp(t)

	got, why := a.refreshTokenMissing("acct-missing:user-missing")
	if !got {
		t.Errorf("缺 refresh_token 的 OfficeAce 账号应判定为缺失（原样：%t）", got)
	}
	if !strings.Contains(why, "refresh_token") {
		t.Errorf("提示文案应点明缺的是 refresh_token，实际：%q", why)
	}
	// 文案必须给出**补救路径**（用户看到徽标后最想知道「我该做什么」）；
	// 两条路径都实测可用：客户端重登后重导入、或本工具浏览器登录。
	for _, want := range []string{"导入", "登录"} {
		if !strings.Contains(why, want) {
			t.Errorf("提示文案缺少补救路径关键词 %q：%q", want, why)
		}
	}

	if got, why := a.refreshTokenMissing("acct-ok:user-ok"); got || why != "" {
		t.Errorf("有 refresh_token 的账号不得报缺失：got=%t why=%q", got, why)
	}
	// refresh_token 非空但在死名单 ⇒ 续期必然失败，同样要报（否则面板看起来一切正常）。
	if got, why := a.refreshTokenMissing("acct-dead:user-dead"); !got {
		t.Errorf("refresh_token 已在死名单的账号应判定为缺失（续期必然失败）")
	} else if !strings.Contains(why, "拒绝") {
		t.Errorf("死名单场景的文案应点明「已被上游拒绝」，实际：%q", why)
	}
	if got, _ := a.refreshTokenMissing("trae-1"); got {
		t.Errorf("非 OfficeAce 渠道不得报「缺 refresh_token」（该渠道本就没有 refresh 概念）")
	}
	if got, _ := a.refreshTokenMissing("no-such-uid"); got {
		t.Errorf("未知账号不得报缺失")
	}
	// 反向验证：refreshTokenMissing 改为恒 false ⇒ 第一条断言变红。
}

// 前端 `RELOGIN_CHANNELS` 与后端登录入口必须对齐（2026-10-18 独立复核抓出的**死按钮**）。
//
// 现场：本轮把 `officeace` 加进前端的 RELOGIN_CHANNELS，却没在 `StartLoginFor` 的 switch 里
// 登记 ⇒ 卡片上的 ⟳ 点下去只弹 `unknown login provider officeace`（前端只断言「字符串存在」、
// 后端只断言「字段存在」，**没有任何测试把两侧对齐**，所以全绿）。
//
// 判据分两层，且都以**源码**为单一来源（不依赖网络）：
//  1. RELOGIN_CHANNELS 的每个成员都必须出现在 StartLoginFor 的 switch 里（登记）；
//  2. 对**有同步终态**的渠道（显式登记为「无需登录/两步流程」的那几个）逐一直调，
//     确认调用后行为符合预期——特别是不允许出现 `unknown login provider`。
//     （其余渠道会真的去起登录流程，这里只做第 1 层的源码比对。）
func TestReloginChannelSetMatchesBackend(t *testing.T) {
	js, err := os.ReadFile("../../cmd/wild-work/web/app.js")
	if err != nil {
		t.Fatalf("读 app.js 失败：%v", err)
	}
	m := regexp.MustCompile(`const RELOGIN_CHANNELS = new Set\(\[([^\]]*)\]\)`).FindSubmatch(js)
	if m == nil {
		t.Fatal("未能在 app.js 里定位 RELOGIN_CHANNELS")
	}
	var channels []string
	for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(string(m[1]), -1) {
		channels = append(channels, q[1])
	}
	if len(channels) < 5 {
		t.Fatalf("RELOGIN_CHANNELS 解析出 %d 个渠道，疑似解析失败：%v", len(channels), channels)
	}

	appGo, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("读 app.go 失败：%v", err)
	}
	src := string(appGo)
	// 定位 StartLoginFor 函数体（到下一个顶层 func 为止）。
	start := strings.Index(src, "func (a *App) StartLoginFor(")
	if start < 0 {
		t.Fatal("未找到 StartLoginFor")
	}
	rest := src[start:]
	if i := strings.Index(rest[1:], "\nfunc "); i > 0 {
		rest = rest[:i+1]
	}
	for _, ch := range channels {
		// 源码比对：switch 里必须出现该渠道（用 provider.Kind 常量名或字面量）。
		constName := map[string]string{
			"workbuddy": "provider.WorkBuddy", "workbuddyai": "provider.WorkBuddyAI",
			"traework": "provider.TraeWork", "qoder": "provider.Qoder",
			"qodercn": "provider.QoderCN", "qodercom": "provider.QoderCOM",
			"qwenwork": "provider.QwenWork", "raccoon": "provider.Raccoon",
			"officeace": "provider.OfficeAce",
		}[ch]
		if constName == "" {
			t.Errorf("RELOGIN_CHANNELS 里的 %q 没有在测试的常量映射里登记（请同步本测试）", ch)
			continue
		}
		if !strings.Contains(rest, constName) {
			t.Errorf("RELOGIN_CHANNELS 含 %q，但 StartLoginFor 的 switch 未登记 %s ⇒ "+
				"卡片上的 ⟳ 会弹 unknown login provider（死按钮）", ch, constName)
		}
	}

	// 第 2 层：有同步终态的渠道直调，错误信息不得是 unknown login provider。
	a, _ := newOfficeAceBadgeApp(t)
	for _, ch := range []string{"officeace", "monkeycode", "kilo", "oczen", "glm"} {
		_, err := a.StartLoginFor(ch)
		if err != nil && strings.Contains(err.Error(), "unknown login provider") {
			t.Errorf("StartLoginFor(%q) 落到 default 分支（未登记）：%v", ch, err)
		}
	}
	// OfficeAce 必须给出**可操作**的指引（两步流程），而不是一句 unknown。
	if _, err := a.StartLoginFor("officeace"); err == nil || !strings.Contains(err.Error(), "两步") {
		t.Errorf("StartLoginFor(\"officeace\") 应说明它是两步流程并指向浏览器登录入口，实际：%v", err)
	}
}

// csvContains 的边界（死名单判据的基础）。
func TestCSVContains(t *testing.T) {
	cases := []struct {
		csv, want string
		ok        bool
	}{
		{"a,b,c", "b", true},
		{"a, b , c", "b", true}, // 逐项 TrimSpace
		{"a,b,c", "d", false},
		{"", "a", false},
		{"a,b,c", "", false},  // 空 want 恒 false（不得把「空 token」当成命中）
		{"ab", "a", false},    // 必须整项相等，不做子串匹配
		{"a,b", "a,b", false}, // 逗号本身不是合法项
	}
	for _, c := range cases {
		if got := csvContains(c.csv, c.want); got != c.ok {
			t.Errorf("csvContains(%q, %q) = %t，want %t", c.csv, c.want, got, c.ok)
		}
	}
}

func TestAccountViewCarriesRefreshTokenMissing(t *testing.T) {
	a, _ := newOfficeAceBadgeApp(t)

	var missing, ok *AccountView
	views := a.accountViews()
	for i := range views {
		v := &views[i]
		switch v.UID {
		case "acct-missing:user-missing":
			missing = v
		case "acct-ok:user-ok":
			ok = v
		}
	}
	if missing == nil || ok == nil {
		t.Fatalf("账号视图未包含两个 OfficeAce 账号：missing=%v ok=%v", missing, ok)
	}
	if !missing.RefreshTokenMissing {
		t.Errorf("缺凭据账号的 refresh_token_missing 应为 true（前端徽标据此渲染）")
	}
	if missing.RefreshTokenMissingReason == "" {
		t.Errorf("缺凭据账号必须带 refresh_token_missing_reason（徽标 hover 文案）")
	}
	if ok.RefreshTokenMissing || ok.RefreshTokenMissingReason != "" {
		t.Errorf("正常账号不得带 refresh_token_missing：%+v", ok)
	}
	// 前端契约：app.js 读的字段名必须与 Go 侧 json tag 一致（历史上多次因改名而两边脱节）。
	// 判据取源码文本（同 monkeycode/loomy 的既有做法），避免「改了 tag 忘了前端」这类
	// 只在运行时才暴露的问题。
	src, err := os.ReadFile("../../cmd/wild-work/web/app.js")
	if err != nil {
		t.Fatalf("读 app.js 失败：%v", err)
	}
	for _, key := range []string{"refresh_token_missing", "refresh_token_missing_reason"} {
		if !strings.Contains(string(src), key) {
			t.Errorf("app.js 未引用字段 %q（Go 侧 json tag 与前端脱节）", key)
		}
	}
}
