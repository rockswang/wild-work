// checkin.go Loomy「宠物每日打工」奖励领取（2026-10-17 接入）。
//
// # 机制结论（先说清楚，避免误解这个包在做什么）
//
// Loomy 没有独立的「每日签到 / 每日领取」端点。全客户端（electron 源码 805 个文件
// + 渲染 bundle）里 "签到" / "daily-checkin" / "claimDaily" / "领取每日" 的命中数都是 0。
// 与「每日积分」相关的真实机制有两条，且都不是「登录就发」：
//
//	1. 每日赠送额度 5000（dailyBalance）—— 上游服务端惰性刷新。
//	   官方 UI 原文（dist/assets/main-*.js）：
//	     "每日赠送积分会在你每日登录后自动刷新为 5000"
//	   它按 dailyCycleDate（服务端业务日）滚动，用户无需、也没有任何"领取"动作。
//	   本包不碰它：读余额（GET /api/v1/points/records）就已能反映刷新结果。
//
//	2. 宠物每日打工奖励（0~1000 积分，按档发放）—— 这才是真正"需要领"的东西。
//	   端点（points-service.js:434-450 / :416-418）：
//	     GET  /api/v1/pet-work          -> 快照：enabled / businessDate / config / status
//	     POST /api/v1/pet-work/rewards  -> 领奖：{businessDate, configVersion, reachedTierId}
//	   发放判据由服务端计算（pet-work-service.js:1071-1124 在开轮前查快照取 enabled，
//	   达标后 :979-1069 发领奖请求），客户端只提交「业务日 + 配置版本 + 已达档位」。
//
// # 为什么本工具拿不到这份积分（核心限制，必须如实说明）
//
// 档位分 = sessionWeight*sessionIndex + depthWeight*depthIndex，输入是该账号的
// 有效对话轮次（recordValidTurn，pet-work-service.js:1227）。本工具是 API 网关：
// 它转发用户的对话请求，但不产生轮次归属，也拿不到用户在官方客户端里的轮次统计。
// 本机 pet-work.json 的实测旁证：三个账号在 2026-10-17 都是
// earnedPointsToday=0 / rewardedTierIds=[] / sessions={} —— 当天没有轮次就没有积分。
//
// 因此本包不假装能每天凭空领到积分：
//   - 我们只能把 businessDate + configVersion 如实上报，让上游按它自己的统计判；
//   - granted=false 且无档位 = 未达档位 -> 明确的可重试状态 + error，绝不报 claimed。
//
// # 鉴权（points-service.js:123-125）
//
// 积分面只发 token 头（不是 Authorization）；信封成功码是字符串 "000000"，
// 登录失效是 HTTP 200 + code 100002 —— 与本包 Classify 的既有口径一致。
package loomy

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

// PetWorkConfigVersion 上游活动配置版本。快照会下发权威值；这里只是兜底。
//
// 证据：本机 %APPDATA%\Loomy\pet-work.json 的 configCache.config.configVersion
// 与 __tests__/pet-work-reward-tiers.integration.test.js 的 CONFIG_VERSION 一致。
const PetWorkConfigVersion = "pet-work-v1"

// PetWorkTier1 最低档位 ID。拿不到快照里的档位表时的兜底。
const PetWorkTier1 = "tier_1"

// BusinessDateOf 取该账号当前的服务端业务日（读 pet-work 快照）。
//
// 供 DailyCheckinReport 内部两步式使用（先取业务日、再调 ClaimPetWorkReward）；
// 也可被外部单独调用，用于「只看今天有没有可领的活动、先不发领奖请求」。
// 活动未开启或快照不可用时返回空串（调用方据此跳过本轮，不去打领奖接口）。
func (c *Client) BusinessDateOf(a *auth.Auth) (string, error) {
	d, ok, err := c.PetWorkSnapshot(a)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	return d.BusinessDate, nil
}

// DailyCheckinReport 实现 provider.CheckinReporter（**标准签到契约**，无业务日参数）。
//
// # 为什么是它，而不是另立一套「带业务日的接口」
//
// Loomy 的领奖请求体必须带服务端业务日，而上游业务日只能从 pet-work 快照读。
// 早先的实现思路是「另立 DatedCheckinReporter + 让调度器先取快照再调用」，但那要求改动
// 调度器与 app 的签到链路；那段接线最终没有落地 ⇒ 本渠道的签到实际上**没有任何入口**
//（面板按钮不可用、定时任务全关），功能等于没做。
//
// 现在改为：本方法自己完成「取业务日 → 领奖」两步，于是既有的三条签到链路
//（面板手动 / 全部签到 / 定时签到）**零改动**即可驱动 Loomy。
//
// # 代价与边界（如实说明）
//
//   - 每次签到 = 2 次上游往返（快照 + 领奖）。每天最多 3 次（各签到时刻一次），
//     合计个位数请求，完全在 §0.1「最小化上游请求」之内。
//   - 快照拿不到业务日（活动未开）时**不打领奖接口**，直接返回可重试状态。
//   - 快照本身失败（网络/401/形状异常）时如实返回错误；401 会带 ErrSessionDead
//     类型，由调度器的 isSessionDead 判定为终态（需人工重新登录），不在这里吞掉。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	date, err := c.BusinessDateOf(a)
	if err != nil {
		uid := ""
		if a != nil {
			uid = a.UID
		}
		log.Printf("loomy checkin uid=%s status=error msg=pet-work 快照失败 err=%v", uid, err)
		// 保留原始 err（可能是 *provider.Error{ErrSessionDead}）——调度器要靠 errors.As 判终态。
		return provider.CheckinReport{
			Status: provider.CheckinError,
			Msg:    "读 pet-work 快照失败：" + truncate(err.Error(), 200),
		}, err
	}
	return c.ClaimPetWorkReward(a, date)
}

// pointsEnvelope 积分面统一信封。Code 可能是字符串（"000000"）也可能是数字。
type pointsEnvelope struct {
	Code json.RawMessage `json:"code"`
	Desc string          `json:"desc"`
	Data json.RawMessage `json:"data"`
}

// decodePointsEnvelope 解析积分面信封。
//
// 把 Data 先紧凑化：标准库对 RawMessage 会保留源文本里的空白，直接二次
// json.Unmarshal 在缩进过的 JSON 上会失败。上游是否缩进不可控，故统一处理。
func decodePointsEnvelope(raw []byte) (pointsEnvelope, error) {
	var env pointsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, err
	}
	if len(env.Data) > 0 {
		var compact bytes.Buffer
		if err := json.Compact(&compact, env.Data); err != nil {
			return env, err
		}
		env.Data = compact.Bytes()
	}
	return env, nil
}

// ok 报告信封 code 是否为成功码。
//
// 官方客户端判 data.code 存在且 != "000000" 为错（points-service.js:208），
// 故空 code 也算成功（部分接口不带 code）；数字 0 / 200 宽容处理。
func (e pointsEnvelope) ok() bool {
	raw := strings.Trim(strings.TrimSpace(string(e.Code)), `"`)
	switch raw {
	case "", "000000", "0", "200":
		return true
	}
	return false
}

// codeText 信封 code 的可读形态（日志/错误信息用）。
func (e pointsEnvelope) codeText() string {
	s := strings.Trim(strings.TrimSpace(string(e.Code)), `"`)
	if s == "" {
		return "absent"
	}
	return truncate(s, 40)
}

// petWorkSnapshot 快照的 data 段（只保留被消费的字段）。
type petWorkSnapshot struct {
	Enabled      bool   `json:"enabled"`
	BusinessDate string `json:"businessDate"`
	// Config 活动配置。历史形状可能把 configVersion 平铺在 data，故两者都读。
	Config struct {
		Enabled       bool   `json:"enabled"`
		BusinessDate  string `json:"businessDate"`
		ConfigVersion string `json:"configVersion"`
		DailyMax      int64  `json:"dailyMaxPoints"`
		Tiers         []struct {
			TierID string `json:"tierId"`
			Score  int64  `json:"score"`
		} `json:"tiers"`
	} `json:"config"`
	ConfigVersion string `json:"configVersion"`
	Status        struct {
		EarnedPointsToday  int64    `json:"earnedPointsToday"`
		RewardedTierIDs    []string `json:"rewardedTierIds"`
		DailyRewardCapped  bool     `json:"dailyRewardCapped"`
		PendingReachedTier string   `json:"pendingReachedTierId"`
	} `json:"status"`
}

// PetWorkDaily 是本工具能看到的宠物打工当日状态（只读，给调用方判断用）。
type PetWorkDaily struct {
	Enabled       bool
	BusinessDate  string
	ConfigVersion string
	// EarnedPointsToday 今日已通过宠物打工获得的积分（上游口径）。
	EarnedPointsToday int64
	// RewardedTierIDs 今日已发放的档位。
	RewardedTierIDs []string
	// Capped 今日是否已封顶。
	Capped bool
	// NextTierID 当前可尝试申请的最低未领档位；空串表示没有可申请档位。
	NextTierID string
}

// nextTier 按快照里的档位表推出「当前应申请的档位」。
//
// 上游要的是 reachedTierId（已达档位）。我们拿不到该账号的轮次统计，故按
// 「已发档位的下一个」推进：已发 tier_1 -> 下次申请 tier_2。这样：
//   - 已领完 -> 空串（无需再发请求）；
//   - 一次都没领 -> tier_1（服务端会自行判定是否达标）。
func (s petWorkSnapshot) nextTier() string {
	tiers := s.Config.Tiers
	if len(tiers) == 0 {
		// 拿不到档位表：已发过 tier_1 就视为已完成（不猜更高档，绝不编造）。
		for _, id := range s.Status.RewardedTierIDs {
			if id == PetWorkTier1 {
				return ""
			}
		}
		return PetWorkTier1
	}
	done := make(map[string]bool, len(s.Status.RewardedTierIDs))
	for _, id := range s.Status.RewardedTierIDs {
		done[id] = true
	}
	for _, t := range tiers {
		if !done[t.TierID] {
			return t.TierID
		}
	}
	return ""
}

// configVersionOf 取快照里的配置版本（config.configVersion 优先，回退顶层，最后兜底常量）。
func (s petWorkSnapshot) configVersionOf() string {
	if v := strings.TrimSpace(s.Config.ConfigVersion); v != "" {
		return v
	}
	if v := strings.TrimSpace(s.ConfigVersion); v != "" {
		return v
	}
	return PetWorkConfigVersion
}

// dailyView 把快照收敛成对外结构。
func (s petWorkSnapshot) dailyView() PetWorkDaily {
	return PetWorkDaily{
		Enabled:           s.Enabled,
		BusinessDate:      strings.TrimSpace(s.BusinessDate),
		ConfigVersion:     s.configVersionOf(),
		EarnedPointsToday: s.Status.EarnedPointsToday,
		RewardedTierIDs:   append([]string{}, s.Status.RewardedTierIDs...),
		Capped:            s.Status.DailyRewardCapped,
		NextTierID:        s.nextTier(),
	}
}

// petWorkEnvelopeData 领奖回执的 data 段（包含 status）。
type petWorkEnvelopeData struct {
	BusinessDate   string   `json:"businessDate"`
	GrantBatchID   string   `json:"grantBatchId"`
	Granted        *bool    `json:"granted"`
	Duplicate      *bool    `json:"duplicate"`
	GrantedPoints  *int64   `json:"grantedPoints"`
	GrantedTierIDs []string `json:"grantedTierIds"`
	Status         struct {
		EarnedPointsToday int64 `json:"earnedPointsToday"`
		DailyRewardCapped bool  `json:"dailyRewardCapped"`
	} `json:"status"`
}

// ---------------------------------------------------------------------------
// 请求
// ---------------------------------------------------------------------------

// pointsRequest 发一次积分面请求（只发 token 头，与官方客户端一致）。
//
// 与 applyAuth 的区别是刻意的：applyAuth 用于推理网关（Authorization + token 双写）；
// 积分面官方只发 token，照抄推理面的双写没有证据支撑，故这里单发。
func (c *Client) pointsRequest(method, path string, a *auth.Auth, body []byte) (*http.Response, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.baseURL()+path, rdr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("token", session(a))
	req.Header.Set("traceparent", traceparent())
	req.Header.Set("loomy-version", AppVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, raw, rerr
}

// PetWorkSnapshot 读一次宠物打工快照（只读，无副作用）。
//
// 返回 ok=false 表示活动关闭或快照不可用；err 只在传输/形状错误时非 nil。
func (c *Client) PetWorkSnapshot(a *auth.Auth) (PetWorkDaily, bool, error) {
	if session(a) == "" {
		return PetWorkDaily{}, false, nil
	}
	resp, raw, err := c.pointsRequest(http.MethodGet, EpPetWork, a, nil)
	if err != nil {
		return PetWorkDaily{}, false, err
	}
	if resp.StatusCode != http.StatusOK {
		return PetWorkDaily{}, false, &provider.Error{
			Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 200),
		}
	}
	if k := businessErrKind(string(raw)); k != provider.ErrNone {
		return PetWorkDaily{}, false, &provider.Error{Kind: k, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	env, envErr := decodePointsEnvelope(raw)
	if envErr != nil || !env.ok() {
		return PetWorkDaily{}, false, nil
	}
	var snap petWorkSnapshot
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &snap); err != nil {
			return PetWorkDaily{}, false, nil
		}
	}
	d := snap.dailyView()
	return d, d.Enabled, nil
}

// ClaimPetWorkReward 实现「带业务日的领奖」：申请当前该账号最低的未领档位。
//
// businessDate 必须来自快照（服务端业务日），不得用本机时钟猜：
// 上游显式校验 YYYY-MM-DD 且与快照自身比对（pet-work-service.js:960
// payload.businessDate !== daily.businessDate 即视为过期）。
//
// 状态映射（provider.CheckinStatus）：
//
//	enabled=false / 无快照           -> CheckinNoCampaign（活动可能未开，窗口内重试）
//	已全部领完 / duplicate / capped  -> CheckinAlready（当日完成）
//	granted=true 且 grantedPoints>0  -> CheckinClaimed（Amount=发放额）
//	granted=false 且无档位           -> CheckinError + error（未达档位，不得冒充领到）
//	401 / code 100002                -> *provider.Error{ErrSessionDead}
//	429 / 5xx / 形状突变             -> CheckinError（429 在 Classify 里优先于余额类判定）
func (c *Client) ClaimPetWorkReward(a *auth.Auth, businessDate string) (provider.CheckinReport, error) {
	uid := ""
	if a != nil {
		uid = a.UID
	}
	ret := func(st provider.CheckinStatus, amount int64, msg string, err error) (provider.CheckinReport, error) {
		log.Printf("loomy checkin uid=%s status=%s claimed=%t amount=%d msg=%s err=%v",
			uid, st, st == provider.CheckinClaimed, amount, msg, err)
		return provider.CheckinReport{Status: st, Msg: msg, Amount: amount}, err
	}

	if session(a) == "" {
		return ret(provider.CheckinNoToken, 0, "账号缺少 session，无法领取（请重新登录或从本机客户端导入）", nil)
	}
	businessDate = strings.TrimSpace(businessDate)
	if businessDate == "" {
		// 没有业务日就不发请求：服务端会以 100001 拒绝（grantPetWorkReward 的必填校验），
		// 白发一次只消耗上游风控额度。
		return ret(provider.CheckinNoCampaign, 0, "暂时拿不到 Loomy 服务端业务日，跳过本轮（稍后重试）", nil)
	}
	return c.grantPetWorkRewards(a, businessDate, ret)
}

// grantPetWorkRewards 先读快照确定档位与配置版本，再发一次领奖请求。
func (c *Client) grantPetWorkRewards(a *auth.Auth, businessDate string,
	ret func(provider.CheckinStatus, int64, string, error) (provider.CheckinReport, error),
) (provider.CheckinReport, error) {
	// 1) 快照：拿 configVersion 与档位表（这些字段以服务端值为权威）。
	sResp, sRaw, err := c.pointsRequest(http.MethodGet, EpPetWork, a, nil)
	if err != nil {
		return ret(provider.CheckinError, 0, "读取宠物打工快照失败："+truncate(err.Error(), 80), err)
	}
	if sResp.StatusCode != http.StatusOK {
		perr := &provider.Error{Kind: Classify(sResp.StatusCode, string(sRaw)), Status: sResp.StatusCode,
			Msg: truncate(string(sRaw), 200)}
		if perr.Kind == provider.ErrSessionDead {
			return ret(provider.CheckinError, 0, "session 已失效，待重新登录后重试", perr)
		}
		return ret(provider.CheckinError, 0, fmt.Sprintf("读取宠物打工快照失败 HTTP %d", sResp.StatusCode), perr)
	}
	if k := businessErrKind(string(sRaw)); k == provider.ErrSessionDead {
		return ret(provider.CheckinError, 0, "session 已失效，待重新登录后重试",
			&provider.Error{Kind: k, Status: sResp.StatusCode, Msg: truncate(string(sRaw), 200)})
	}
	sEnv, sErr := decodePointsEnvelope(sRaw)
	if sErr != nil || !sEnv.ok() {
		return ret(provider.CheckinError, 0, "宠物打工快照无法解析（上游形状可能已变更）",
			errCheckinShape("snapshot", string(sRaw)))
	}
	var snap petWorkSnapshot
	if len(sEnv.Data) > 0 {
		if err := json.Unmarshal(sEnv.Data, &snap); err != nil {
			return ret(provider.CheckinError, 0, "宠物打工快照 data 无法解析（上游形状可能已变更）",
				errCheckinShape("snapshot.data", string(sEnv.Data)))
		}
	}
	if !snap.Enabled {
		return ret(provider.CheckinNoCampaign, 0, "Loomy 宠物打工活动当前未开启", nil)
	}
	if want := strings.TrimSpace(snap.BusinessDate); want != "" && want != businessDate {
		// 业务日已跨天：用服务端口径重来一轮，而不是拿旧日期去撞校验。
		return ret(provider.CheckinNoCampaign, 0,
			fmt.Sprintf("业务日已切换（%s -> %s），本轮跳过，下一轮按新业务日重试", businessDate, want), nil)
	}

	// 2) 已领完就不发请求（duplicate 请求换一次 HTTP 往返没有意义）。
	tier := snap.nextTier()
	if snap.Status.DailyRewardCapped || tier == "" {
		return ret(provider.CheckinAlready, 0,
			fmt.Sprintf("今日宠物打工积分已领取（%d 分）", snap.Status.EarnedPointsToday), nil)
	}

	// 3) 领奖：只发服务端定义的三个字段（points-service.js:434-450）。
	body, _ := json.Marshal(map[string]string{
		"businessDate":  businessDate,
		"configVersion": snap.configVersionOf(),
		"reachedTierId": tier,
	})
	resp, raw, err := c.pointsRequest(http.MethodPost, EpPetWorkRewards, a, body)
	if err != nil {
		return ret(provider.CheckinError, 0, "领奖请求失败："+truncate(err.Error(), 80), err)
	}
	return interpretReward(resp.StatusCode, raw, snap.Status.EarnedPointsToday, ret)
}

// interpretReward 把领奖回执翻译成结构化状态。
//
// 顺序是刻意的：业务码 -> 状态码 -> 形状 -> 已领标记 -> 发放额。错一步就是假成功。
func interpretReward(st int, raw []byte, earnedBefore int64,
	ret func(provider.CheckinStatus, int64, string, error) (provider.CheckinReport, error),
) (provider.CheckinReport, error) {
	// 1) 登录失效优先：本渠道把它塞在 HTTP 200 里（code 100002）。
	if k := businessErrKind(string(raw)); k == provider.ErrSessionDead {
		return ret(provider.CheckinError, 0, "session 已失效，待重新登录后重试",
			&provider.Error{Kind: k, Status: st, Msg: truncate(string(raw), 200)})
	}
	// 2) 429 必须先于余额类判定（不变量 15）：限流 body 常带 quota 字样。
	if st == http.StatusTooManyRequests {
		return ret(provider.CheckinError, 0, "领取被限流，稍后重试",
			&provider.Error{Kind: provider.ErrSoftRate, Status: st, Msg: truncate(string(raw), 200)})
	}
	// 3) 其它状态码错误。
	if st >= 400 {
		perr := &provider.Error{Kind: Classify(st, string(raw)), Status: st, Msg: truncate(string(raw), 200)}
		if perr.Kind == provider.ErrSessionDead {
			return ret(provider.CheckinError, 0, "session 已失效，待重新登录后重试", perr)
		}
		return ret(provider.CheckinError, 0, fmt.Sprintf("领奖失败 HTTP %d", st), perr)
	}

	// 4) 形状：必须是能解析的信封。
	env, envErr := decodePointsEnvelope(raw)
	if envErr != nil {
		return ret(provider.CheckinError, 0, "领奖回执无法解析（上游形状可能已变更）",
			errCheckinShape("rewards", string(raw)))
	}
	if !env.ok() {
		// 业务失败码：原文留痕，可重试。
		return ret(provider.CheckinError, 0, "领奖未成功（上游业务码 "+env.codeText()+"）",
			fmt.Errorf("loomy：领奖业务失败 code=%s desc=%s", env.codeText(), truncate(env.Desc, 120)))
	}
	if len(env.Data) == 0 {
		return ret(provider.CheckinError, 0, "领奖回执缺少 data（上游形状可能已变更）",
			errCheckinShape("rewards.data", "missing"))
	}

	var d petWorkEnvelopeData
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return ret(provider.CheckinError, 0, "领奖回执 data 无法解析（上游形状可能已变更）",
			errCheckinShape("rewards.data", string(env.Data)))
	}

	// 5) 已领：duplicate 或 capped 是上游明确的「今日已处理」。
	if d.Duplicate != nil && *d.Duplicate {
		return ret(provider.CheckinAlready, 0, "今日宠物打工积分已领取", nil)
	}
	if d.Status.DailyRewardCapped {
		return ret(provider.CheckinAlready, 0,
			fmt.Sprintf("今日宠物打工积分已封顶（%d 分）", d.Status.EarnedPointsToday), nil)
	}

	// 6) 发放额。
	granted := d.Granted != nil && *d.Granted
	pts := int64(0)
	if d.GrantedPoints != nil {
		pts = *d.GrantedPoints
	}
	if granted && pts > 0 {
		return ret(provider.CheckinClaimed, pts, fmt.Sprintf("宠物打工奖励 +%d 积分", pts), nil)
	}

	// 7) 未达档位（本工具最常见的结果，必须如实上报）。
	if len(d.GrantedTierIDs) == 0 && d.Granted != nil && !*d.Granted {
		msg := "Loomy 宠物打工未达发放档位（上游按该账号的有效对话轮次计算），本次未获得积分"
		if earnedBefore > 0 {
			msg = fmt.Sprintf("今日已获得 %d 分，未达到下一档位，本次未获得积分", earnedBefore)
		}
		return ret(provider.CheckinError, 0, msg,
			fmt.Errorf("loomy：宠物打工未达档位（granted=%v duplicate=%v earnedBefore=%d）",
				boolText(d.Granted), boolText(d.Duplicate), earnedBefore))
	}

	// 8) 有档位但没给金额：领到了、金额未知（不编造，按快照差分）。
	if len(d.GrantedTierIDs) > 0 {
		delta := d.Status.EarnedPointsToday - earnedBefore
		if delta > 0 {
			return ret(provider.CheckinClaimed, delta,
				fmt.Sprintf("宠物打工奖励 +%d 积分（按快照差分推算）", delta),
				fmt.Errorf("loomy：领奖回执未回填 grantedPoints，按 earnedPointsToday 差分记账"))
		}
		return ret(provider.CheckinClaimed, 0,
			"宠物打工奖励已发放（上游未回填金额，按差分记账）",
			fmt.Errorf("loomy：领奖回执有档位但无金额，形状假设可能过时（%s）", truncate(string(env.Data), 160)))
	}

	return ret(provider.CheckinError, 0, "领奖结果无法判定（上游形状可能已变更）",
		errCheckinShape("rewards.semantics", string(env.Data)))
}

// boolText 三态布尔的可读形态（nil = 上游没给）。
func boolText(p *bool) string {
	switch {
	case p == nil:
		return "absent"
	case *p:
		return "true"
	default:
		return "false"
	}
}

// errCheckinShape 形状突变错误的统一构造（带分类，调度器日志能分辨根因）。
func errCheckinShape(where, detail string) error {
	return &provider.Error{Kind: provider.ErrClient,
		Msg: fmt.Sprintf("loomy checkin 回执形状异常(%s)：%s", where, truncate(detail, 160))}
}
