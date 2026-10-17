// checkin.go MonkeyCode「签到领积分」——**三步无感验证码**流程（2026-10-17 实测重写）。
//
// # 为什么重写
//
// 旧实现是「POST + 空 body + 猜测字段名」，**必然失败**：上游回
//
//	{"code":10701,"message":"兑换验证码失败"}
//
// 这与本工具自身日志逐字一致（`data/app.log` 里三个账号全天都在报这一条），
// 也就是用户看到的「签到领不到积分」。
//
// # 真实契约（三步，全部实测）
//
//	① GET {console}/api/v1/users/wallet/checkin
//	     → {"code":0,"message":"success","data":{"checked_in":false}}
//	   `checked_in` 表示**今日是否已签到**。已签 = 幂等收工，不再打后面两步。
//
//	② 未签 → 领一张一次性验证码（Cap.js 工作量证明，**无需人工交互**，可自动完成）
//	     POST {console}/api/v1/public/captcha/challenge  body={}
//	       → {"challenge":{"c":50,"s":32,"d":3},"expires":…,"token":"…"}
//	     本地解 PoW（见 captcha.go）
//	     POST {console}/api/v1/public/captcha/redeem     {"token":…,"solutions":[…]}
//	       → {"success":true,"token":"<id>:<ver>","expires":…}
//
//	③ POST {console}/api/v1/users/wallet/checkin       {"captcha_token":"<② 的 token>"}
//	     → {"code":0,"message":"success","data":{"checked_in":true}}
//
// # 四条实测事实（直接决定实现形态，改动前务必先读）
//
//  1. **请求字段名是 `captcha_token`**（蛇形）。带 `captcha` / `token` / 空 body 一律 10701。
//  2. **回包不含金额**（首签与重复签都只有 `checked_in:true`）⇒ Amount 恒为 0，
//     金额一律交给调度器的 wallet 差分记账兜底（`CheckinClaimed` 允许 Amount=0）。
//  3. **POST 幂等且不可用作「是否刚领到」的判据**：今日已领时再 POST 仍回
//     `code=0, data.checked_in=true` 且余额不变。⇒「已领」只能看①的 `checked_in`。
//  4. 单账号首签实测让钱包 `balance` 从 100000 → 200000（**毫积分**）⇒ +100 积分，
//     与 `state-monkeycode.json` 的 credits 口径（balance/1000）自洽。
//
// # 鉴权
//
// 三步都只认控制台 Cookie（`monkeycode_ai_session`）；oma_ key 与 omas_ 签名均不参与。
// 401 仍按不变量 19 处理：用百智云会话现派生一份控制台会话，**只重试一次**。
//
// # 状态机（provider.CheckinReport）
//
//	① checked_in==true                  → CheckinAlready（今日已领，不再重试）
//	③ code==0 且 checked_in==true        → CheckinClaimed（Amount=0，差分记账）
//	可达但 checked_in 非真                → CheckinError（可重试；不冒充成功）
//	401（续期后仍拒）                     → CheckinError + *provider.Error{ErrSessionDead}
//	429                                  → 分类错误（ErrSoftRate 优先于余额类，R15）
//	5xx / 网络 / 形状突变                 → CheckinError（可重试）
//	无控制台 Cookie                       → CheckinNoToken（本地判定，不发请求）
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

// ---------------------------------------------------------------------------
// 回包形状
// ---------------------------------------------------------------------------

// checkinEnvelope 控制台统一信封（同 walletResponse 的形状）。
type checkinEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// checkinData 签到相关回执的 data 段。
//
// 只有 `checked_in` 一个字段——这是**实测**结论：官方客户端反编译里那个函数也只按
// 这一个键去遍历 JSON map，且 exe 中不存在 reward/amount/balance 等字段字面量
// （金额由前端从透传 JSON 里取，Rust 不解析）。
//
// 用指针是为了区分「上游没给这个字段」与「上游明确给了 false」——前者是形状突变，
// 不能当成「未签到」去盲目发起领取。
type checkinData struct {
	CheckedIn *bool `json:"checked_in"`
}

// captchaChallengeResp Cap.js 挑战回包（`POST /api/v1/public/captcha/challenge`）。
type captchaChallengeResp struct {
	Challenge struct {
		C int `json:"c"` // 挑战数（默认 50）
		S int `json:"s"` // salt 长度（hex 字符，默认 32）
		D int `json:"d"` // 难度（target 前缀 hex 位数；Cap 官方默认 4，MonkeyCode 实测下发 3）
	} `json:"challenge"`
	Expires int64  `json:"expires"`
	Token   string `json:"token"`
}

// captchaRedeemResp 验证码兑换回包（`POST /api/v1/public/captcha/redeem`）。
//
// 注意它不是控制台信封：**没有 code 字段**，用 `success` + `message` 表达结果。
type captchaRedeemResp struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Token   string `json:"token"`
	Expires int64  `json:"expires"`
}

// errCheckinParsed 区分「没拿到可解析的成功回执」与其它 IO 错误。
//
// 用带类型的 *provider.Error{ErrClient} 而不是裸 errors.New：签到失败会进调度器日志，
// 有分类才能一眼看出是「本工具的形状假设过时」还是「上游真故障」。
// ErrClient 不进 ErrSessionDead 分支（调度器不会为它去续期会话）。
func errCheckinParsed(detail string) error {
	return &provider.Error{Kind: provider.ErrClient, Msg: shortText(detail, 160)}
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

	// ① 查今日状态。已签即收工——这条既是幂等短路，也是「已领」的唯一可信判据。
	st, raw, state, err := c.fetchCheckinState(a, cookie)
	if err != nil {
		return ret(provider.CheckinError, 0, "签到状态查询失败："+shortText(err.Error(), 80), err)
	}
	if st >= 400 {
		return c.reportTransportError(ret, st, raw)
	}
	if state == nil {
		return ret(provider.CheckinError, 0, "签到状态回执无法解析（上游形状可能已变更）",
			errCheckinParsed(string(raw)))
	}
	if state.CheckedIn == nil {
		// 信封成功但没有 checked_in 字段：形状突变，宁可重试也不猜。
		return ret(provider.CheckinError, 0, "签到状态回执缺少 checked_in（上游形状可能已变更）",
			errCheckinParsed("data="+string(raw)))
	}
	if *state.CheckedIn {
		return ret(provider.CheckinAlready, 0, "今日已领取", nil)
	}

	// ② 未签 → 领一张一次性验证码（无感 PoW，无需人工交互）。
	verify, cerr := c.obtainCaptchaToken(a, cookie)
	if cerr != nil {
		if pe, ok := cerr.(*provider.Error); ok && pe.Kind == provider.ErrSessionDead {
			return ret(provider.CheckinError, 0, "控制台会话失效，待刷新后重试", pe)
		}
		return ret(provider.CheckinError, 0, "取签到验证码失败："+shortText(cerr.Error(), 80), cerr)
	}

	// ③ 带验证码领取。字段名实测为 captcha_token，写错就是 10701。
	payload, _ := json.Marshal(map[string]string{"captcha_token": verify})
	st, raw, err = c.consoleAuthed(a, cookie, http.MethodPost, epWalletCheckin, payload)
	if err != nil {
		return ret(provider.CheckinError, 0, "签到请求失败："+shortText(err.Error(), 80), err)
	}
	if st >= 400 {
		return c.reportTransportError(ret, st, raw)
	}
	var env checkinEnvelope
	if json.Unmarshal(raw, &env) != nil {
		return ret(provider.CheckinError, 0, "签到回执无法解析（上游形状可能已变更）",
			errCheckinParsed(string(raw)))
	}
	if env.Code != codeOK {
		perr := &provider.Error{Kind: c.Classify(st, string(raw)), Status: st, Msg: shortText(string(raw), 200)}
		return ret(provider.CheckinError, 0, fmt.Sprintf("签到失败 code=%d", env.Code), perr)
	}
	var d checkinData
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return ret(provider.CheckinError, 0, "签到回执 data 无法解析（上游形状可能已变更）",
				errCheckinParsed("data="+string(env.Data)))
		}
	}
	if d.CheckedIn == nil || !*d.CheckedIn {
		// code=0 但没把状态置为已签：上游没真正发放。不得报成功。
		return ret(provider.CheckinError, 0, "签到未生效（上游回执 checked_in 非真）",
			fmt.Errorf("monkeycode：签到 code=0 但 checked_in 未置真（%s）", shortText(string(raw), 160)))
	}
	// 金额：上游回包不含金额，恒为 0 ⇒ 由调度器的 wallet 差分记账兜底（既有机制）。
	return ret(provider.CheckinClaimed, 0, "签到成功（金额由积分差分记账）", nil)
}

// reportTransportError 处理 HTTP >= 400 的签到请求（按 Classify 分类，401 走类型化 session dead）。
func (c *Client) reportTransportError(
	ret func(provider.CheckinStatus, int64, string, error) (provider.CheckinReport, error),
	st int, raw []byte,
) (provider.CheckinReport, error) {
	kind := c.Classify(st, string(raw))
	perr := &provider.Error{Kind: kind, Status: st, Msg: shortText(string(raw), 200)}
	if kind == provider.ErrSessionDead {
		return ret(provider.CheckinError, 0, "控制台会话失效，待刷新后重试", perr)
	}
	return ret(provider.CheckinError, 0, fmt.Sprintf("签到失败 HTTP %d", st), perr)
}

// ---------------------------------------------------------------------------
// 三步流程的各个子步骤
// ---------------------------------------------------------------------------

// fetchCheckinState 第①步：GET 签到状态。返回 (HTTP 状态, 原始回包, 解析后的 data)。
//
// data 为 nil 表示信封或 data 段解析失败（调用方按形状突变处理）。
func (c *Client) fetchCheckinState(a *auth.Auth, cookie string) (int, []byte, *checkinData, error) {
	st, raw, err := c.consoleAuthed(a, cookie, http.MethodGet, epWalletCheckin, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	if st >= 400 {
		return st, raw, nil, nil
	}
	var env checkinEnvelope
	if json.Unmarshal(raw, &env) != nil || env.Code != codeOK {
		return st, raw, nil, nil
	}
	var d checkinData
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &d); err != nil {
			return st, raw, nil, nil
		}
	}
	return st, raw, &d, nil
}

// obtainCaptchaToken 第②步：解 Cap.js 工作量证明并兑换一枚一次性验证码。
//
// 返回的字符串直接作为 `captcha_token` 提交（形态 `<id>:<ver>`）。
// 全程不需要人工交互——这不是图形验证码，而是可计算的工作量证明（见 captcha.go）。
func (c *Client) obtainCaptchaToken(a *auth.Auth, cookie string) (string, error) {
	st, raw, err := c.consoleAuthed(a, cookie, http.MethodPost, epCaptchaChallenge, []byte("{}"))
	if err != nil {
		return "", err
	}
	if st >= 400 {
		return "", &provider.Error{Kind: c.Classify(st, string(raw)), Status: st,
			Msg: "验证码挑战失败：" + shortText(string(raw), 160)}
	}
	var ch captchaChallengeResp
	if err := json.Unmarshal(raw, &ch); err != nil {
		return "", errCheckinParsed("challenge=" + string(raw))
	}
	if ch.Token == "" || ch.Challenge.C <= 0 {
		return "", errCheckinParsed("challenge 缺字段：" + string(raw))
	}

	solutions, err := capSolve(ch.Token, ch.Challenge.C, ch.Challenge.S, ch.Challenge.D)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(map[string]any{"token": ch.Token, "solutions": solutions})
	if err != nil {
		return "", err
	}
	st, raw, err = c.consoleAuthed(a, cookie, http.MethodPost, epCaptchaRedeem, payload)
	if err != nil {
		return "", err
	}
	var rd captchaRedeemResp
	if st >= 400 || json.Unmarshal(raw, &rd) != nil || !rd.Success || rd.Token == "" {
		// 兑换失败时把原始回包留在 error 里（不含凭据），便于定位是形状变了还是被风控。
		return "", &provider.Error{Kind: c.Classify(st, string(raw)), Status: st,
			Msg: "验证码兑换失败：" + shortText(string(raw), 160)}
	}
	return rd.Token, nil
}

// consoleAuthed 发一次控制台请求；遇 401 用百智云会话现派生一份再**只重试一次**（不变量 19）。
//
// 三步流程共用：控制台会话只有约 6 天寿命，而派生它的百智云会话约 29 天，
// 所以这里自愈能显著减少「用户什么都不做就永久 401」的情况。
func (c *Client) consoleAuthed(a *auth.Auth, cookie, method, path string, body []byte) (int, []byte, error) {
	st, raw, err := c.consoleRequest(cookie, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	if st != http.StatusUnauthorized {
		return st, raw, nil
	}
	fresh, derr := c.deriveConsoleSession(a)
	if derr != nil {
		return st, raw, &provider.Error{Kind: provider.ErrSessionDead, Status: st,
			Msg: fmt.Sprintf("控制台会话已失效（%s）；自动续期失败：%v", shortText(string(raw), 80), derr)}
	}
	adoptConsoleCookie(a, fresh)
	st, raw, err = c.consoleRequest(fresh, method, path, body)
	if err != nil {
		return 0, nil, err
	}
	if st == http.StatusUnauthorized {
		return st, raw, &provider.Error{Kind: provider.ErrSessionDead, Status: st,
			Msg: "控制台会话续期后仍被拒绝：" + shortText(string(raw), 120)}
	}
	return st, raw, nil
}

// consoleRequest 发一次控制台 HTTP 请求，返回 (HTTP 状态, 原始回包)。
//
// 只用 c.HTTP（有总超时）：签到是一条短请求链，不像对话那样需要无总超时。
func (c *Client) consoleRequest(cookie, method, path string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.console()+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Cookie", CookieNameConsole+"="+cookie)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// 空 body 的请求也显式声明长度，免得个别网关把「无 Content-Length 的请求」当分块请求拒掉。
	req.ContentLength = int64(len(body))

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// codeOK 控制台信封的成功码（wallet 与签到实测均为 0）。
const codeOK = 0

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
