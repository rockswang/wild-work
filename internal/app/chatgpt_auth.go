package app

// chatgpt_auth.go 落「面板粘贴网页凭据」这一条入口（票 05）：
//
//	面板 → POST /api/login/chatgpt_auth → App.SubmitChatGPTAuth
//	     → chatgpt.Client.Me（零配额校验）→ 落盘 auths/chatgpt-<uid>.json → 重载池 + 重拉模型
//
// 为什么必须两段输入：本渠道凭据是**整组网页会话**（Cookie 族）而不是单枚 token。
// 只贴其中一段能得到「校验也过、每次请求都 403」的账号（上游只认完整的浏览器会话），
// 是本渠道最难排障的失败形态 ⇒ 两段都缺一不可，错误文案要**点名缺的是哪一段**。
//
// 零明文纪律（不变量 2）：Cookie 与 access token 只落 0600 auth 文件；日志只打 uid；
// 返回值里只有 uid 与告警文案，绝不含凭据片段。

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/chatgpt"
	"wild-work/internal/provider"
)

// SubmitChatGPTAuth 提交面板粘贴的网页凭据（Cookie 头 + Authorization 头）。
//
// 返回 (uid, warn, error)。校验判据是 `GET /backend-api/me` 能通（零配额端点，spec §6），
// **不是**「Cookie 里有没有 session-token」——后者在未登录态也可能拿到预登录 Cookie（R128 同款纪律）。
// 校验失败即**不落盘**，并把上游原因如实回传（含分类提示：未登录 / 风控 / 没配代理）。
func (a *App) SubmitChatGPTAuth(cookieRaw, authRaw string) (string, string, error) {
	cookies, err := chatgpt.ParseCookieHeader(cookieRaw)
	if err != nil {
		return "", "", err
	}
	token, err := chatgpt.ParseAuthorizationHeader(authRaw)
	if err != nil {
		return "", "", err
	}

	rt := a.runtime(provider.ChatGPT)
	if rt == nil || rt.Upstream == nil {
		return "", "", errors.New("当前实例没有注册 ChatGPT 渠道（内建渠道未装配）")
	}
	client, ok := rt.Upstream.(*chatgpt.Client)
	if !ok {
		return "", "", errors.New("ChatGPT 渠道的客户端类型异常，无法校验凭据")
	}

	probe := &auth.Auth{
		Kind:        "chatgpt",
		AccessToken: token,
		Cookies:     cookies,
		DeviceID:    chatgpt.NewDeviceID(),
	}
	me, err := client.Me(probe)
	if err != nil {
		return "", "", chatgptAuthError(err)
	}

	uid := chatgptUID(me, token)
	if uid == "" {
		return "", "", errors.New("上游未返回账号 ID，无法确定这是哪个账号：" +
			"请确认粘贴的是**已登录** chatgpt.com 的 Cookie 与 Authorization（两段必须来自同一会话）")
	}

	prev := a.chatgptPrevNickname(uid)
	warn := ""
	if prev.seen {
		warn = fmt.Sprintf("这个 ChatGPT 账号（%s）已经添加过了，本次会用新凭据覆盖它。", chatgptUIDTail(uid))
	}

	aa := &auth.Auth{
		Kind:        "chatgpt",
		UID:         uid,
		Nickname:    chatgptNickname(me, token, uid, prev.nickname),
		AccessToken: token,
		Cookies:     cookies,
		DeviceID:    probe.DeviceID,
		// 真实到期时刻：解 access token 的 JWT exp。解不出来留 0（未知，不臆造、不写远期）——
		// 0 会让下一次续期（RefreshToken）补上真实值（不变量 21 的反面：写远期会让 90 天后静默死掉）。
		ExpiresAt: chatgpt.JWTExp(token),
		Extra:     chatgptExtra(token, me),
		FilePath:  filepath.Join(a.cfg.AuthDir, "chatgpt-"+uid+".json"),
	}

	if err := os.MkdirAll(a.cfg.AuthDir, 0o755); err != nil {
		return "", "", fmt.Errorf("创建凭据目录失败：%w", err)
	}
	if err := aa.SaveAtomic(); err != nil {
		return "", "", fmt.Errorf("保存 ChatGPT 凭据失败：%w", err)
	}
	// 日志零 token（不变量 2）：只打 uid 与「是否覆盖」。
	log.Printf("chatgpt 账号已添加（面板粘贴凭据）uid=%s 覆盖已有=%v", uid, prev.seen)
	a.reloadAccounts()
	// 模型目录立刻重拉 + 费率异步刷新（否则用户要等下一次 ticker 才在面板看到模型）。
	a.afterAccountAdded(provider.ChatGPT)
	return uid, warn, nil
}

// chatgptAuthError 把校验失败的上游错误翻成可行动的文案。
//
// 三类高发失败必须各有明确指引（否则用户只会反复试同一组凭据）：
//   - 未登录（401/not_login）→ 两段凭据无效或来自不同会话；
//   - 403 风控 → 同一账号不重试（重复请求放大命中面），换代理/换号；
//   - 其它 403 → 本渠道 chatgpt.com 境内不可达，多半是**没配代理**（spec §15 代价披露）。
func chatgptAuthError(err error) error {
	var ue *provider.Error
	if !errors.As(err, &ue) {
		return fmt.Errorf("上游校验凭据失败（网络/代理不可达？本渠道必须能连通 chatgpt.com）：%w", err)
	}
	switch {
	case ue.Kind == provider.ErrSessionDead:
		return fmt.Errorf("上游判定这组凭据未登录：Cookie 与 Authorization 必须来自**同一个已登录会话**，"+
			"请重新从 chatgpt.com 的 DevTools 复制两段后重试（%v）", err)
	case ue.Kind == provider.ErrAccountFault:
		return fmt.Errorf("上游风控判定异常活动，凭据已通过校验但不被采信："+
			"请换出口代理后重试（同一组凭据重复提交只会放大风控命中面）（%v）", err)
	case ue.Status == 403:
		return fmt.Errorf("上游 403：本渠道必须能连通 chatgpt.com，**多半是没配代理**（HTTPS_PROXY / HTTP_PROXY）"+
			"或代理出口被地区限制（%v）", err)
	}
	return fmt.Errorf("上游校验凭据失败：%w", err)
}

// chatgptUID 决定账号 uid：优先上游 /backend-api/me 的 id，退回 access token 里的用户 id。
//
// 全空时返空串（调用方报错），**不得**退回随机值/时间戳：uid 就是 auth 文件名与池键，
// 编一个会让同一账号每次添加都变成新条目（积分/状态看似凭空翻倍）。
func chatgptUID(me *chatgpt.UserInfo, token string) string {
	if me != nil && strings.TrimSpace(me.ID) != "" {
		return strings.TrimSpace(me.ID)
	}
	if u := chatgpt.JWTUserID(token); u != "" {
		return u
	}
	return ""
}

// chatgptNickname 决定保存凭据时写的昵称。
//
// 优先级：用户改过的名字 > 上游身份（name / email）> uid 尾号。
// 判据是「旧昵称 ≠ 旧默认名」而不是「有旧昵称」——重贴同一账号时旧值恰好等于默认名，
// 那不算用户改名（与 modelscopeNickname 同款纪律）。
func chatgptNickname(me *chatgpt.UserInfo, token, uid, prev string) string {
	def := chatgptDefaultNickname(uid)
	if p := strings.TrimSpace(prev); p != "" && p != def {
		return p
	}
	if me != nil {
		if n := strings.TrimSpace(me.Name); n != "" {
			return n
		}
		if e := strings.TrimSpace(me.Email); e != "" {
			return e
		}
	}
	if n := chatgpt.JWTName(token); n != "" {
		return n
	}
	// 网页 JWT 常只带 email（profile.name 不一定有）—— 用它比「uid 尾号」更能认出是谁的号。
	if email, _, _, _, _ := chatgpt.JWTInfo(token); email != "" {
		return email
	}
	return def
}

// chatgptDefaultNickname 是占位昵称（「ChatGPT Web ab12ef」）。
// 它同时是「用户没改过名字」的判据，格式变更会让旧账号的默认名被当成用户定制（永不更新）。
func chatgptDefaultNickname(uid string) string {
	tail := chatgptUIDTail(uid)
	if tail == "" {
		return "ChatGPT Web"
	}
	return "ChatGPT Web " + tail
}

// chatgptUIDTail 取 uid 尾 6 位（短标识；uid 本身可能是 `user-xxxx` 这类长串）。
func chatgptUIDTail(uid string) string {
	s := strings.TrimSpace(uid)
	if len(s) <= 6 {
		return s
	}
	return s[len(s)-6:]
}

// chatgptExtra 组装 Extra 里**非凭据**的身份字段：accountId 决定上游 `ChatGPT-Account-Id`
// 头（网页家庭号必须带），email 只用于面板排障。
//
// accountId 只从 JWT 取，**绝不**拿 /me 的 user id 顶替：那是两个不同的概念
// （一个是用户、一个是 team/workspace），混用会被上游判成无权限账号；缺失时宁可不发该头。
func chatgptExtra(token string, me *chatgpt.UserInfo) map[string]string {
	email, _, accID, _, _ := chatgpt.JWTInfo(token)
	extra := map[string]string{}
	if accID != "" {
		extra["accountId"] = accID
	}
	if me != nil {
		if e := strings.TrimSpace(me.Email); e != "" {
			email = e
		}
	}
	if email != "" {
		extra["email"] = email
	}
	return extra
}

// chatgptPrev 是「同 uid 已入库账号」的只读快照。
type chatgptPrev struct {
	seen     bool
	nickname string
}

// chatgptPrevNickname 查同 uid 是否已存在（用于覆盖告警与昵称保留）。
func (a *App) chatgptPrevNickname(uid string) chatgptPrev {
	existing, err := auth.LoadChatGPTDir(a.cfg.AuthDir)
	if err != nil {
		return chatgptPrev{}
	}
	for _, e := range existing {
		if e != nil && e.UID == uid {
			return chatgptPrev{seen: true, nickname: e.Nickname}
		}
	}
	return chatgptPrev{}
}

