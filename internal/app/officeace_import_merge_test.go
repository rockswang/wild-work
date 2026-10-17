package app

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"wild-work/internal/config"
)

// 本文件锁定「从客户端导入」**不得覆盖** OAuth 登录凭据（importOfficeAce → writeOfficeAceAuth 的按字段合并）。
//
// 缺陷链（实证，用户实例 C:\软件存储\wild-work）：
//
//	① 浏览器自助登录写出 auths/officeace-<uid>.json：临时 AK/SK、security_token、project_id、
//	   refresh_token、dpop_priv、cred_expires（internal/officeace/login.go）；
//	② 随后点一次「从客户端导入」无条件覆写同一路径 ⇒ extra.access_key / secret_key /
//	   security_token 变空、auth.refreshToken 变空、auth.expiresAt 变成 now+50 年（导入路径的指纹）；
//	③ 后果：积分/明细用**空 AK** 签名（订阅接口 401 APIG.0303 "app not found, appkey "），
//	   签到 status=no_token「无临时凭据，需先续期/登录」。
//
// 测试只用假值（AGENTS.md §0.2 零 token 落盘），且全部写在 t.TempDir() 内，不触碰真实凭据目录。

func newOfficeAceImportApp(t *testing.T) *App {
	t.Helper()
	cfg := config.Default()
	cfg.AuthDir = t.TempDir()
	return &App{cfg: cfg}
}

// fakeBasic 造一个假网关 Basic 头（绝不使用真实 AK/SK）。
func fakeBasic(tag string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte("ak-fake-"+tag+":sk-fake-"+tag))
}

// readOfficeAceFile 读回凭据文件，并顺带校验磁盘形态仍是嵌套形 {auth,account}（不变量 3）。
func readOfficeAceFile(t *testing.T, path string) (authDoc, map[string]json.RawMessage) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读凭据文件失败（%s）：%v", path, err)
	}
	var doc authDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("解析凭据文件失败（%s）：%v", path, err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("解析凭据文件顶层失败（%s）：%v", path, err)
	}
	if _, ok := top["auth"]; !ok {
		t.Errorf("凭据文件不是嵌套形（缺 auth 段，违反不变量 3）：%s", path)
	}
	if _, ok := top["account"]; !ok {
		t.Errorf("凭据文件不是嵌套形（缺 account 段，违反不变量 3）：%s", path)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("写盘后残留临时文件 %s.tmp（原子写未清理）", path)
	}
	return doc, top
}

// oauthLoginDoc 造一份「浏览器登录」写出的凭据（同 internal/officeace/login.go 的字段集）。
func oauthLoginDoc(uid, apiHost, rt string, credExpires int64) authDoc {
	return authDoc{
		Auth: authSection{
			AccessToken:  fakeBasic("oauth"),
			RefreshToken: rt,
			// 登录路径的 expiresAt 就是临时凭据到期（officeace/account.go 的 toAuth）。
			ExpiresAt: credExpires,
			ApiHost:   apiHost,
			Extra: map[string]string{
				"base_url":       apiHost,
				"account_id":     "acct-fake",
				"user_id":        "user-fake",
				"label":          "fake-label-old",
				"region":         "cn-southwest-2",
				"access_key":     "AKFAKEOLD",
				"secret_key":     "SKFAKEOLD",
				"security_token": "STSFAKEOLD",
				"project_id":     "proj-fake-old",
				"cred_expires":   strconv.FormatInt(credExpires, 10),
				"refresh_expire": strconv.FormatInt(credExpires+30*24*3600, 10),
				"dpop_priv":      `{"kty":"EC","crv":"P-256","kid":"fake"}`,
				"dead_tokens":    "rt-fake-dead-0001",
				"source":         "oauth-login",
			},
		},
		Account: accountSection{UID: uid, Nickname: "fake-label-old"},
	}
}

// importedDoc 造一份「从客户端导入」的凭据：只有网关 Basic / base_url / models / 身份元数据，
// 没有临时 AK/SK、没有 refresh_token、没有 dpop_priv（真实导入路径的典型形态）。
func importedDoc(uid, apiHost string, expiresAt int64) authDoc {
	return authDoc{
		Auth: authSection{
			AccessToken:  fakeBasic("imported"),
			RefreshToken: "",
			ExpiresAt:    expiresAt,
			ApiHost:      apiHost,
			Extra: map[string]string{
				"base_url":   apiHost,
				"account_id": "acct-fake",
				"user_id":    "user-fake",
				"label":      "fake-label-imported",
				"region":     "cn-southwest-2",
				"models":     "officeace-model-a\nofficeace-model-b",
				"source":     "disk",
			},
		},
		Account: accountSection{UID: uid, Nickname: "fake-label-imported"},
	}
}

// ① 已有 OAuth 登录凭据时：导入后凭据仍在，且 expiresAt 未被改成 +50 年。
func TestOfficeAceImportMergeKeepsOAuthCredentials(t *testing.T) {
	a := newOfficeAceImportApp(t)
	uid := "01a149d8913679e5ba6de6275047897f:01a149d8913672db88d7a95fb3382d4d"

	credExpires := time.Now().Add(2 * time.Hour).Unix()
	if _, err := a.writeAuthFile("officeace", uid, oauthLoginDoc(uid, "https://gw-old.example.invalid/v2", "rt-fake-old-0001", credExpires)); err != nil {
		t.Fatalf("准备既有 OAuth 凭据失败：%v", err)
	}

	importedExpires := time.Now().AddDate(50, 0, 0).Unix()
	imported := importedDoc(uid, "https://gw-imported.example.invalid/v2", importedExpires)
	file, info, err := a.writeOfficeAceAuth(uid, imported)
	if err != nil {
		t.Fatalf("合并导入失败：%v", err)
	}
	got, _ := readOfficeAceFile(t, file)
	ex := got.Auth.Extra

	// 核心：导入不得清空 OAuth 凭据（本项的原始缺陷）。
	if got.Auth.RefreshToken != "rt-fake-old-0001" {
		t.Errorf("auth.refreshToken 被导入清空/改写：%q（应保留既有值）", got.Auth.RefreshToken)
	}
	if got.Auth.ExpiresAt != credExpires {
		t.Errorf("auth.expiresAt=%d，应为既有 cred_expires=%d（导入的 +50 年哨兵=%d 不得覆盖真实到期）",
			got.Auth.ExpiresAt, credExpires, importedExpires)
	}
	if got.Auth.ExpiresAt == importedExpires {
		t.Errorf("auth.expiresAt 仍是导入侧的 +50 年哨兵值 %d —— 这正是「被导入覆盖」的指纹", importedExpires)
	}
	for key, want := range map[string]string{
		"access_key":     "AKFAKEOLD",
		"secret_key":     "SKFAKEOLD",
		"security_token": "STSFAKEOLD",
		"project_id":     "proj-fake-old",
		"cred_expires":   strconv.FormatInt(credExpires, 10),
		"dpop_priv":      `{"kty":"EC","crv":"P-256","kid":"fake"}`,
		"dead_tokens":    "rt-fake-dead-0001",
		"source":         "oauth-login",
	} {
		if ex[key] != want {
			t.Errorf("extra[%s]=%q，应为保留的旧值 %q（OAuth 凭据/取证标记不得被导入覆盖）", key, ex[key], want)
		}
	}

	// 导入路径是网关 Basic / base_url / models 的权威来源。
	if got.Auth.AccessToken != imported.Auth.AccessToken {
		t.Error("auth.accessToken（网关 Basic）应以导入值为准")
	}
	if got.Auth.ApiHost != imported.Auth.ApiHost {
		t.Errorf("auth.apiHost=%q，应以导入值 %q 为准", got.Auth.ApiHost, imported.Auth.ApiHost)
	}
	if ex["base_url"] != imported.Auth.Extra["base_url"] {
		t.Errorf("extra[base_url]=%q，应以导入值 %q 为准", ex["base_url"], imported.Auth.Extra["base_url"])
	}
	if ex["models"] != imported.Auth.Extra["models"] {
		t.Errorf("extra[models]=%q，应以导入值为准", ex["models"])
	}
	if got.Account.UID != uid {
		t.Errorf("account.uid=%q，应为 %q", got.Account.UID, uid)
	}

	if !info.Merged || !info.KeptCredential || !info.KeptRefreshToken || !info.KeptExpiresAt || !info.HasRefreshToken {
		t.Errorf("合并信息不符：%+v（期望 Merged/KeptCredential/KeptRefreshToken/KeptExpiresAt/HasRefreshToken 全为 true）", info)
	}
}

// ② 目标不存在时：行为与现在一致（全新导入，原样落盘）。
func TestOfficeAceImportFreshWriteMatchesImportedDoc(t *testing.T) {
	a := newOfficeAceImportApp(t)
	uid := "acct-fresh:user-fresh"

	importedExpires := time.Now().AddDate(50, 0, 0).Unix()
	imported := importedDoc(uid, "https://gw-fresh.example.invalid/v2", importedExpires)
	imported.Auth.RefreshToken = "rt-fake-new-0002"

	file, info, err := a.writeOfficeAceAuth(uid, imported)
	if err != nil {
		t.Fatalf("全新导入失败：%v", err)
	}
	got, _ := readOfficeAceFile(t, file)
	if !reflect.DeepEqual(got, imported) {
		t.Errorf("全新导入应原样落盘（不做任何合并）：\n got=%+v\nwant=%+v", got.Auth, imported.Auth)
	}
	if info.Merged || info.KeptCredential || info.KeptRefreshToken || info.KeptExpiresAt {
		t.Errorf("全新导入不应报告任何「保留旧值」：%+v", info)
	}
	if !info.HasRefreshToken {
		t.Errorf("导入侧带 refresh_token 时应报告 HasRefreshToken=true：%+v", info)
	}
}

// ③ 导入值有、旧值为空时：以导入值为准（合并不能反过来把导入的新凭据丢掉）。
func TestOfficeAceImportMergeTakesImportedWhenOldEmpty(t *testing.T) {
	a := newOfficeAceImportApp(t)
	uid := "acct-empty:user-empty"

	// 旧文件存在，但凭据字段全空（例如已被旧版导入清空过一次）。
	emptyPrev := authDoc{
		Auth: authSection{
			AccessToken:  fakeBasic("old-empty"),
			RefreshToken: "",
			ExpiresAt:    0,
			ApiHost:      "",
			Extra: map[string]string{
				"base_url":       "",
				"access_key":     "",
				"secret_key":     "",
				"security_token": "",
				"dead_tokens":    "",
				"source":         "oauth-login",
			},
		},
		Account: accountSection{UID: uid},
	}
	if _, err := a.writeAuthFile("officeace", uid, emptyPrev); err != nil {
		t.Fatalf("准备旧凭据失败：%v", err)
	}

	importedExpires := time.Now().AddDate(50, 0, 0).Unix()
	// ⚠️ cred_expires 必须与导入侧的 +50 年哨兵**不同值**，否则本用例无法区分
	// 「按 cred_expires 对齐」与「照抄哨兵」两种行为（独立复核曾指出这是自证式断言）。
	realCredExpires := time.Now().Add(2 * time.Hour).Unix()
	imported := importedDoc(uid, "https://gw-empty.example.invalid/v2", importedExpires)
	imported.Auth.Extra["access_key"] = "AKFAKENEW"
	imported.Auth.Extra["secret_key"] = "SKFAKENEW"
	imported.Auth.Extra["security_token"] = "STSFAKENEW"
	imported.Auth.Extra["project_id"] = "proj-fake-new"
	imported.Auth.Extra["cred_expires"] = strconv.FormatInt(realCredExpires, 10)
	imported.Auth.RefreshToken = "rt-fake-new-0003"

	file, info, err := a.writeOfficeAceAuth(uid, imported)
	if err != nil {
		t.Fatalf("合并导入失败：%v", err)
	}
	got, _ := readOfficeAceFile(t, file)

	for key, want := range map[string]string{
		"access_key":     "AKFAKENEW",
		"secret_key":     "SKFAKENEW",
		"security_token": "STSFAKENEW",
		"project_id":     "proj-fake-new",
	} {
		if got.Auth.Extra[key] != want {
			t.Errorf("extra[%s]=%q，应为导入值 %q（旧值为空 ⇒ 以导入为准）", key, got.Auth.Extra[key], want)
		}
	}
	if got.Auth.RefreshToken != "rt-fake-new-0003" {
		t.Errorf("auth.refreshToken=%q，应为导入值（旧值为空）", got.Auth.RefreshToken)
	}
	// 导入侧的临时凭据本次生效 ⇒ 到期时间必须与它们自己的 cred_expires 对齐（~2h），
	// 而**不是**导入结构体里那个 +50 年哨兵（网关 Basic 不过期的表达）。
	if got.Auth.ExpiresAt != realCredExpires {
		t.Errorf("auth.expiresAt=%d，应为 cred_expires=%d（临时凭据生效时必须与它对齐）",
			got.Auth.ExpiresAt, realCredExpires)
	}
	if got.Auth.ExpiresAt == importedExpires {
		t.Errorf("auth.expiresAt 等于导入侧的 +50 年哨兵（%d）⇒ 通用自动续期路径对 officeace 永不触发（P1 回归）",
			importedExpires)
	}
	// 本次保留了导入侧的临时凭据 ⇒ 到期时间必须与它们匹配（cred_expires）。
	if got.Auth.Extra["cred_expires"] != imported.Auth.Extra["cred_expires"] {
		t.Errorf("extra[cred_expires]=%q，应为导入值 %q", got.Auth.Extra["cred_expires"], imported.Auth.Extra["cred_expires"])
	}
	if !info.Merged || info.KeptCredential || info.KeptRefreshToken || !info.KeptExpiresAt || !info.HasRefreshToken {
		t.Errorf("合并信息不符：%+v（期望 Merged/HasRefreshToken/KeptExpiresAt 为 true，KeptCredential/KeptRefreshToken 为 false）", info)
	}
}

// ④ 只保留旧 refresh_token（旧文件无临时凭据）时：expiresAt 与那批凭据对齐，不被 +50 年覆盖。
func TestOfficeAceImportMergeKeepsExpiryForRetainedRefreshToken(t *testing.T) {
	a := newOfficeAceImportApp(t)
	uid := "acct-rt:user-rt"

	realExpires := time.Now().Add(20 * 24 * time.Hour).Unix()
	prev := authDoc{
		Auth: authSection{
			AccessToken:  fakeBasic("old-rt"),
			RefreshToken: "rt-fake-old-0004",
			ExpiresAt:    realExpires,
			ApiHost:      "https://gw-old.example.invalid/v2",
			Extra:        map[string]string{"base_url": "https://gw-old.example.invalid/v2", "source": "oauth-login"},
		},
		Account: accountSection{UID: uid},
	}
	if _, err := a.writeAuthFile("officeace", uid, prev); err != nil {
		t.Fatalf("准备旧凭据失败：%v", err)
	}

	importedExpires := time.Now().AddDate(50, 0, 0).Unix()
	file, info, err := a.writeOfficeAceAuth(uid, importedDoc(uid, "https://gw-imported.example.invalid/v2", importedExpires))
	if err != nil {
		t.Fatalf("合并导入失败：%v", err)
	}
	got, _ := readOfficeAceFile(t, file)

	if got.Auth.RefreshToken != "rt-fake-old-0004" {
		t.Errorf("auth.refreshToken=%q，应保留旧值", got.Auth.RefreshToken)
	}
	if got.Auth.ExpiresAt != realExpires {
		t.Errorf("auth.expiresAt=%d，应沿用旧值 %d（与保留的 refresh_token 对齐）", got.Auth.ExpiresAt, realExpires)
	}
	if !info.KeptRefreshToken || !info.KeptExpiresAt || info.KeptCredential {
		t.Errorf("合并信息不符：%+v（期望 KeptRefreshToken/KeptExpiresAt=true、KeptCredential=false）", info)
	}
}

// ⑤ 目标文件存在但无法解析：必须报错中止，绝不静默覆盖（要求 5）。
func TestOfficeAceImportRefusesUnparsableExistingFile(t *testing.T) {
	a := newOfficeAceImportApp(t)
	uid := "acct-broken:user-broken"
	path := a.authFilePath("officeace", uid)

	garbage := []byte("{ this is not json")
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatalf("写入损坏凭据失败：%v", err)
	}

	importedExpires := time.Now().AddDate(50, 0, 0).Unix()
	if _, _, err := a.writeOfficeAceAuth(uid, importedDoc(uid, "https://gw-broken.example.invalid/v2", importedExpires)); err == nil {
		t.Fatal("已有凭据无法解析时必须报错中止导入，否则会把用户的东西静默清掉")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读回损坏凭据失败：%v", err)
	}
	if string(after) != string(garbage) {
		t.Errorf("损坏文件被改写了（应保持原样）：%q", string(after))
	}
}

// ⑥ 提示文案必须能区分「导入本来就没有 refresh_token」与「已保留既有凭据」（要求 3）。
//
// ⚠️ 本用例是**源码文案检查**（grep import_local.go 的格式串），**不是行为守门人**——
// 2026-10-17 独立复核指出它是恒真式断言（改文案即红，但改行为不红）。
// 真正的行为守门人是 ①（KeptCredential 时凭据仍在）+ ③（导入凭据生效时 expiresAt 与
// cred_expires 对齐）；本用例只保证两段文案都不被误删，别把它当成覆盖率。
//
// 2026-10-17 晚更新：保留凭据那句补了「到期时间已按保留凭据对齐」——因为改进1 之后
// expiresAt 会被**纠正**（不再原样沿用旧值），原措辞会与此不符。文案系行为的一部分：
// 它是对用户解释「我为什么没覆盖你的凭据」的唯一出口。
func TestOfficeAceImportNoteDistinguishesKeptCredentials(t *testing.T) {
	src, err := os.ReadFile("import_local.go")
	if err != nil {
		t.Fatalf("读 import_local.go 失败：%v", err)
	}
	code := string(src)
	for _, want := range []string{
		"保留了本工具已保存的 OAuth 凭据",                 // 已保留既有凭据
		"其到期时间已按保留凭据自己的到期对齐",              // 且说明了 expiresAt 被纠正（改进1）
		"未取得 refresh_token，积分/签到需在本工具重新登录一次", // 导入本来就没有
	} {
		if !strings.Contains(code, want) {
			t.Errorf("导入提示缺少文案片段 %q（两种情形必须可区分）", want)
		}
	}
}

// ⑦ 0 字节/纯空白文件没有任何可保留的凭据 ⇒ 按「全新导入」处理（不能因此把用户永久挡住）。
func TestOfficeAceImportTreatsEmptyFileAsAbsent(t *testing.T) {
	a := newOfficeAceImportApp(t)
	uid := "acct-emptyfile:user-emptyfile"
	path := a.authFilePath("officeace", uid)
	if err := os.WriteFile(path, []byte("\n  \n"), 0o600); err != nil {
		t.Fatalf("写入空壳凭据失败：%v", err)
	}

	importedExpires := time.Now().AddDate(50, 0, 0).Unix()
	imported := importedDoc(uid, "https://gw-emptyfile.example.invalid/v2", importedExpires)
	file, info, err := a.writeOfficeAceAuth(uid, imported)
	if err != nil {
		t.Fatalf("空壳文件不应阻断导入：%v", err)
	}
	got, _ := readOfficeAceFile(t, file)
	if !reflect.DeepEqual(got, imported) {
		t.Errorf("空壳文件应按全新导入落盘：\n got=%+v\nwant=%+v", got.Auth, imported.Auth)
	}
	if info.Merged {
		t.Errorf("空壳文件不算合并：%+v", info)
	}
}

// ⑧ 解析器必须认得两种磁盘形态（嵌套形为准，扁平形兼容）。
//
// 若扁平形解析不出来，旧文件会被当成「空」⇒ 合并等于无条件覆盖（正是本项要修的缺陷形态），
// 所以这条要有回归保护。
func TestParseAuthDocHandlesNestedAndFlat(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"tok-fake-nested","refreshToken":"rt-fake-nested",` +
		`"extra":{"access_key":"AKFAKENESTED","source":"oauth-login"}},` +
		`"account":{"uid":"u-fake-1","nickname":"n-fake-1"}}`)
	doc, err := parseAuthDoc(nested)
	if err != nil {
		t.Fatalf("嵌套形解析失败：%v", err)
	}
	if doc.Auth.AccessToken != "tok-fake-nested" || doc.Auth.RefreshToken != "rt-fake-nested" ||
		doc.Auth.Extra["access_key"] != "AKFAKENESTED" || doc.Account.UID != "u-fake-1" ||
		doc.Account.Nickname != "n-fake-1" {
		t.Errorf("嵌套形解析结果不符：%+v / %+v", doc.Auth, doc.Account)
	}

	flat := []byte(`{"accessToken":"tok-fake-flat","refreshToken":"rt-fake-flat","uid":"u-fake-2",` +
		`"nickname":"n-fake-2","extra":{"access_key":"AKFAKEFLAT"}}`)
	doc2, err := parseAuthDoc(flat)
	if err != nil {
		t.Fatalf("扁平形解析失败：%v", err)
	}
	if doc2.Auth.AccessToken != "tok-fake-flat" || doc2.Auth.RefreshToken != "rt-fake-flat" ||
		doc2.Auth.Extra["access_key"] != "AKFAKEFLAT" || doc2.Account.UID != "u-fake-2" ||
		doc2.Account.Nickname != "n-fake-2" {
		t.Errorf("扁平形解析结果不符：%+v / %+v", doc2.Auth, doc2.Account)
	}

	if _, err := parseAuthDoc([]byte("{ this is not json")); err == nil {
		t.Error("非法 JSON 必须返回错误（调用方据此中止导入，不得静默覆盖）")
	}
}
