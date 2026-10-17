// checkin.go MonkeyCode「签到领积分」（2026-10-17 接入）。
//
// # 取证（离线，未打真实上游；AGENTS §0.1）
//
// 端点、方法与域**有证据**：
//
//	POST {consoleBase}/api/v1/users/wallet/checkin
//
// 来源 1（端点全路径）：`%LOCALAPPDATA%\MonkeyCode\monkeycode-desktop.exe`
// （Tauri 壳，Rust 字符串）明文含 `/api/v1/users/wallet/checkin`，
// 且与 `/api/v1/users/wallet`（本包 wallet 已实测可用）、`/api/v1/users/subscription`
// 相邻——三者同属「控制台用户接口」，故**域 = 控制台域**（不是代理域、不是百智云域）。
//
// 来源 2（调用入口）：同一二进制含 Tauri 命令名 `mc_checkin` 与 IPC 权限项
// `allow-mc-checkin`——签到是**客户端主动发起的命令**（不是被动拉取钱包时顺带领取，）
//
// 来源 3（交互文案）：同机 WebView2 的 V8 字节码缓存
// `%LOCALAPPDATA%\com.chaitin.baizhi.monkeycode\EBWebView\Default\Code Cache\js\3bc38ab58a8847fe_0`
// 含 i18n 键 `account.usage.checkin` → "Check in +{reward}"、
// `account.usage.checkinBusy` → "Checking in…"、`account.usage.checkedIn` → "Checked in today"。
// 即：回执含本次发放额（展示文案里的 reward），且存在「今日已签」的独立终态。
//
// 鉴权：控制台 Cookie（`monkeycode_ai_session`）——与 wallet 同域同 Cookie；签到不涉及对话，
// 故 oma_ key 与 omas_ 签名都**不参与**。
//
// # 未确认（不得当成已知事实；改动前请先补一份真实抓包）
//
//  1. **方法**。二进制里只有路径串，没有方法。按「领红包/领奖励」的 REST 惯例取 POST；
//     若上游实际是 GET，表现为 404/405（落成 CheckinError，可重试，**不会假成功**），
//     不会被误判成已领或领到。改这里只需动 sendCheckin 的一行。
//  2. **请求体**。取空 body：钱包接口是同站 GET+Cookie、无 body；签到不需要参数。
//  3. **响应字段名**。下面的 `checkinData` 是一次**兼容式建模**：金额、已领标记、活动标记
//     各给了 2~3 个候选字段名（reward/amount/credit；already_checked_in/checked_in_today；
//     active/enabled），取值时「有一个算一个」。这样做不是拍脑袋——是为了在字段名猜错时
//     **安全降级**：金额全缺 → 判「领到但金额未知」（Amount=0，调度器降级差分记账）；
//     已领标记全缺 → 判「已领」；活动标记全缺 → 不判无活动。三条都不会把失败说成成功。
//     **但真正的字段名仍需一次抓包确认**（尤其「已领」标记：猜错会让每天都显示已领）。
//  4. **成功码**。按同站信封取 `code == 0` 为成功；`code == 0` 之外的 0/200 之类宽容值
//     见 okCode。若上游用 HTTP 状态表达失败、body 无 code，则取 HTTP 2xx 成功。
//  5. **毫积分单位**。金额按钱包口径当**毫积分**（console 前端 `balance/1e3` 再 ceil）处理，
//     与 UserResourceDetail 的「积分」展示口径一致。
//
// # 状态机（provider.CheckinReport）
//
//	领取成功 granted>0                 → CheckinClaimed（Amount=换算后积分）
//	领到但回执无金额字段（字段名过时） → CheckinClaimed（Amount=0，调度器回退差分；带 error 留痕）
//	回执**明确**声明发放额 0           → CheckinError（可重试；没拿到积分不冒充领到）
//	已领标记 / HTTP 409 / 业务码 409   → CheckinAlready（当日完成，不再重试）
//	无活动（标记 / 业务码 404 / HTTP404）→ CheckinNoCampaign（窗口内重试）
//	5xx / 429 / 网络 / 形状突变        → CheckinError + error（可重试）
//	其它 4xx（403/422…）               → CheckinError（可重试；**带分类** error）
//	401（控制台会话失效）              → *provider.Error{ErrSessionDead}
//	   （先用百智云会话现派生一份再**只重试一次**；派生失败才把 401 抛出去）
//	无控制台 Cookie                    → CheckinNoToken（本地判定，不发请求）
package monkeycode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// errCheckinParsed 区分「没拿到可解析的成功回执」与其它 IO 错误。
//
// 用带类型的 *provider.Error{ErrClient} 而不是裸 errors.New：签到失败会进调度器日志，
// 有分类才能一眼看出是「本工具的形状假设过时」还是「上游真故障」。
// ErrClient 不进 ErrSessionDead 分支（调度器不会为它去续期会话）。
func errCheckinParsed(detail string) error {
	return &provider.Error{Kind: provider.ErrClient, Msg: shortText(detail, 160)}
}

// checkinEnvelope 控制台统一信封（同 walletResponse 的形状）。
type checkinEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// checkinData 签到回执的 data 段（⚠️ 字段名为兼容式候选，见文件头「未确认 ③」）。
//
// 金额与三个标记全用**指针**：必须能区分「上游没给这个字段」与「上游明确给了 0 / false」。
// 这两者在签到语义里结论完全不同（见 interpretCheckin ④）——裸 int64 会把二者揉成 0。
type checkinData struct {
	// 本次发放额（毫积分）。三个候选名任一非 nil 即视为上游作了声明。
	Reward *int64 `json:"reward"`
	Amount *int64 `json:"amount"`
	Credit *int64 `json:"credit"`
	// 已领标记：为 true 表示本次没有新发放（今天已经领过）。
	AlreadyCheckedIn *bool `json:"already_checked_in"`
	CheckedInToday   *bool `json:"checked_in_today"`
	// 去重后的**当日是否已领**标记（部分网关换名字下发）。
	Already   *bool `json:"already"`
	AlreadyUp *bool `json:"already_claimed"`
	// 活动标记：显式 false 表示当前没有可领的签到活动。
	Active  *bool `json:"active"`
	Enabled *bool `json:"enabled"`
	// Balance 回执里顺带下发的余额（毫积分），仅用于文案，不参与判定。
	Balance int64 `json:"balance"`
}

// grantMillis 本次发放额（毫积分），以及「上游到底有没有声明金额」。
//
// 后者是关键：声明了（哪怕声明成 0）就按声明判；没声明（字段全缺）说明本项目
// 对这些字段名的假设可能过时了 —— 那就按「领到了但金额未知」处理，绝不编造金额。
func (d checkinData) grantMillis() (millis int64, declared bool) {
	for _, p := range []*int64{d.Reward, d.Amount, d.Credit} {
		if p == nil {
			continue
		}
		declared = true
		if *p > 0 {
			return *p, true
		}
	}
	return 0, declared
}

// already 报告回执是否明确表达「今日已领」（任一标记为 true）。
func (d checkinData) already() bool {
	for _, p := range []*bool{d.AlreadyCheckedIn, d.CheckedInToday, d.Already, d.AlreadyUp} {
		if p != nil && *p {
			return true
		}
	}
	return false
}

// inactive 报告回执是否明确表达「没有可领的活动」（任一标记为 false，且**没有**已领标记）。
//
// 只有指针非 nil 才算结论：字段缺失 ≠ 无活动（形状突变时宁可判未知、去重试）。
func (d checkinData) inactive() bool {
	for _, p := range []*bool{d.Active, d.Enabled} {
		if p != nil && !*p {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// provider.Upstream / CheckinReporter
// ---------------------------------------------------------------------------

// DailyCheckin 实现 provider.Upstream。语义按结构化状态：claimed/already → nil，
// 其余可重试状态 → error（与 raccoon / codearts 同口径）。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	rep, err := c.DailyCheckinReport(a)
	if err != nil {
		return err
	}
	if rep.Status.Retryable() {
		return fmt.Errorf("monkeycode 签到未达成: %s", rep.Msg)
	}
	return nil
}

// DailyCheckinReport 实现 provider.CheckinReporter：签到并返回结构化状态。
//
// 结构化的理由同 qodercn：error 通道区分不了 no_campaign 与 error——两者都不是「已签到」，
// 却都需要在当日窗口内继续重试；而 401 又必须走类型化 error 让调度器续期（不变量 19）。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	uid := ""
	if a != nil {
		uid = a.UID
	}
	// 结构化日志行：与 qodercn / raccoon / codearts 同构（stats 的签到收入解析器按它取实发金额，
	// 字段顺序/命名不得随意改动）。日志零 token——只打印 uid 与业务字段。
	ret := func(st provider.CheckinStatus, amount int64, msg string, err error) (provider.CheckinReport, error) {
		log.Printf("monkeycode checkin uid=%s status=%s claimed=%t amount=%d msg=%s err=%v",
			uid, st, st == provider.CheckinClaimed, amount, msg, err)
		return provider.CheckinReport{Status: st, Msg: msg, Amount: amount}, err
	}

	cookie := consoleCookieOf(a)
	if cookie == "" {
		// 老凭据（只导入了 oma_ key）：本地就判 no_token，不去打上游换 401。
		return ret(provider.CheckinNoToken, 0,
			"账号缺少控制台 Cookie，无法签到（在客户端重新登录后再次导入即可）", nil)
	}

	st, raw, err := c.sendCheckin(cookie)
	if err != nil {
		return ret(provider.CheckinError, 0, "签到请求失败："+shortText(err.Error(), 80), err)
	}
	// 401 → 控制台会话可能只是过期（≈6 天），用百智云会话（≈29 天）现派生一份再**重试一次**。
	// 这是纯 HTTP 两步（见 deriveConsoleSession），不涉及跨端伪装。
	if st == http.StatusUnauthorized {
		fresh, derr := c.deriveConsoleSession(a)
		if derr != nil {
			// 派生不可用：401 按类型化 session dead 抛出，让调度器/用户知道要重新登录。
			perr := &provider.Error{Kind: provider.ErrSessionDead, Status: st,
				Msg: fmt.Sprintf("控制台会话已失效（%s）；自动续期失败：%v", shortText(string(raw), 80), derr)}
			return ret(provider.CheckinError, 0, "控制台会话失效，待刷新后重试", perr)
		}
		adoptConsoleCookie(a, fresh)
		if st, raw, err = c.sendCheckin(fresh); err != nil {
			return ret(provider.CheckinError, 0, "签到请求失败："+shortText(err.Error(), 80), err)
		}
		if st == http.StatusUnauthorized {
			perr := &provider.Error{Kind: provider.ErrSessionDead, Status: st,
				Msg: "控制台会话续期后仍被拒绝：" + shortText(string(raw), 120)}
			return ret(provider.CheckinError, 0, "控制台会话失效，待刷新后重试", perr)
		}
	}

	return c.interpretCheckin(st, raw, ret)
}

// interpretCheckin 把一次签到的 (HTTP 状态, 原始回包) 翻译成结构化状态。
//
// 判定顺序是刻意的（错一步就是假成功或假失败）：
//
//	① HTTP 409 / 业务码 409            → 已领（唯一能被信任的「已领」信号）
//	② HTTP 401 / 429 / 5xx / 其它 4xx  → 分类错误（429 在 Classify 里优先于余额类，R15）
//	③ 解析信封失败                     → 形状突变，可重试错误
//	④ 业务码为成功码：看已领标记 → 无活动标记 → 发放额
//	⑤ 业务码 404                       → 无活动（可重试）
//	⑥ 其它业务码                       → 可重试错误
func (c *Client) interpretCheckin(st int, raw []byte,
	ret func(provider.CheckinStatus, int64, string, error) (provider.CheckinReport, error),
) (provider.CheckinReport, error) {
	var env checkinEnvelope
	parsed := json.Unmarshal(raw, &env) == nil

	// ① 已领：HTTP 409 优先于一切（不在下面任何分支里就可能被 4xx 分支吃掉）。
	if st == http.StatusConflict {
		return ret(provider.CheckinAlready, 0, "今日已领取", nil)
	}
	if parsed && env.Code == codeAlready {
		return ret(provider.CheckinAlready, 0, "今日已领取", nil)
	}

	// ② 传输层/状态码错误（成功码不在此列：有些网关以 HTTP 200 + 非零 code 表达业务结果）。
	if st >= 400 && !(parsed && okCode(env.Code)) {
		if st == http.StatusNotFound && noCampaignCode(env.Code, parsed) {
			return ret(provider.CheckinNoCampaign, 0, "暂无签到活动（活动可能尚未开放）", nil)
		}
		kind := c.Classify(st, string(raw))
		perr := &provider.Error{Kind: kind, Status: st,
			Msg: shortText(string(raw), 200)}
		if st == http.StatusNotFound && !parsed {
			// 404 且无信封：多半是端点/方法不对（见文件头「未确认 ①」），提示到根因上。
			return ret(provider.CheckinError, 0, "签到接口返回 404（端点或方法可能已变更，需重新取证）", perr)
		}
		if kind == provider.ErrSessionDead {
			return ret(provider.CheckinError, 0, "控制台会话失效，待刷新后重试", perr)
		}
		return ret(provider.CheckinError, 0, fmt.Sprintf("签到失败 HTTP %d", st), perr)
	}

	// ③ 信封解析失败：不允许往下猜（宁可重试，不可假成功）。
	if !parsed {
		return ret(provider.CheckinError, 0, "签到回执无法解析（上游形状可能已变更）",
			errCheckinParsed(string(raw)))
	}

	// ④ 成功码。
	if okCode(env.Code) {
		var d checkinData
		if len(env.Data) > 0 {
			if err := json.Unmarshal(env.Data, &d); err != nil {
				// data 存在但不是对象/字段类型不符：同样按形状突变处理。
				return ret(provider.CheckinError, 0, "签到回执 data 无法解析（上游形状可能已变更）",
					errCheckinParsed("data="+string(env.Data)))
			}
		}
		if d.already() {
			return ret(provider.CheckinAlready, 0, alreadyMsg(d), nil)
		}
		if d.inactive() {
			return ret(provider.CheckinNoCampaign, 0, "暂无签到活动（活动可能尚未开放）", nil)
		}
		millis, declared := d.grantMillis()
		if granted := milliCredits(millis); granted > 0 {
			return ret(provider.CheckinClaimed, granted, fmt.Sprintf("签到成功 +%d 积分", granted), nil)
		}
		// data 整个缺失：形状突变（连「领没领到」都无从判断）。
		if len(env.Data) == 0 {
			return ret(provider.CheckinError, 0, "签到回执缺少 data（上游形状可能已变更）",
				errCheckinParsed("data 缺失"))
		}
		if !declared {
			// 上游没声明任何金额字段：按「领到了，但金额读不出来」处理。
			// claimed + Amount=0 正是 provider.CheckinClaimed 允许的语义（调度器降级差分记账）；
			// 仍带一条 error 留痕，提示字段名假设可能已过时。**绝不编造金额。**
			return ret(provider.CheckinClaimed, 0, "签到成功（上游未回填发放额，按差分记账）",
				fmt.Errorf("monkeycode：签到 code=0 但回执无金额字段，形状假设可能过时（%s）",
					shortText(string(env.Data), 160)))
		}
		// 上游**明确**给了 0：没拿到积分（额度已满/奖励已领走）。不得报 claimed。
		return ret(provider.CheckinError, 0, "签到未发放积分（上游回执金额为 0）",
			fmt.Errorf("monkeycode：签到 code=0 但声明发放额为 0（%s）", shortText(string(raw), 160)))
	}

	// ⑤ 无活动：业务码 404（含 HTTP 2xx 信封里的 404）。
	if noCampaignCode(env.Code, true) {
		return ret(provider.CheckinNoCampaign, 0, "暂无签到活动（活动可能尚未开放）", nil)
	}

	// ⑥ 其它业务码：可重试错误，原文留痕。
	return ret(provider.CheckinError, 0, fmt.Sprintf("签到失败 code=%d", env.Code),
		fmt.Errorf("monkeycode：签到业务失败 code=%d message=%s", env.Code, shortText(env.Message, 120)))
}

// alreadyMsg 已领时的文案（带上回执里的余额，方便用户直观确认积分仍在）。
func alreadyMsg(d checkinData) string {
	if d.Balance > 0 {
		return fmt.Sprintf("今日已领取（积分余额 %d）", milliCredits(d.Balance))
	}
	return "今日已领取"
}

// ---------------------------------------------------------------------------
// 请求
// ---------------------------------------------------------------------------

// sendCheckin 发一次签到请求，返回 (HTTP 状态, 原始回包)。
//
// 只用 c.HTTP（有总超时）：签到是一次性的短请求，不像对话那样需要无总超时。
func (c *Client) sendCheckin(cookie string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, c.console()+epWalletCheckin, bytes.NewReader(nil))
	if err != nil {
		return 0, nil, err
	}
	// 请求头与 wallet 同构：同域同 Cookie，Accept 用 JSON。空 body 也显式声明长度，
	// 免得个别网关把「无 Content-Length 的 POST」当分块请求拒掉。
	req.Header.Set("Cookie", CookieNameConsole+"="+cookie)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	req.ContentLength = 0

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// okCode 报告业务码是否为「成功」。
//
// 同站钱包接口的成功码是 0（实测）；这里额外宽容 200（部分网关直接复用 HTTP 码）。
// **不宽容 1**：拿不准时宁可走 ⑥ 变成可重试错误，也不要冒险把失败当成功。
func okCode(code int) bool { return code == 0 || code == http.StatusOK }

// code Already 控制台信封里「今日已签」的业务码（推断：与 HTTP 409 同义）。
const codeAlready = http.StatusConflict

// noCampaignCode 业务码是否表达「无活动」。
func noCampaignCode(code int, parsed bool) bool {
	return parsed && code == http.StatusNotFound
}

// shortText 截断文本用于日志/错误信息（脱敏：签到回包不含凭据，但保守截断）。
func shortText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	if s == "" {
		return "空响应"
	}
	return s
}
