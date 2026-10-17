// credits.go 积分账户、免费额度与「每日签到得积分」三步流程（协议 §2.6）。
//
// 三条必须分清的口径：
//
//  1. **benefit 免费额度**：按 **UTC+8 自然日**结算（不是滚动窗口），服务端报文不给重置
//     时刻 ⇒ 本地按「UTC+8 次日 00:00」推算并写明「预计」（见 sse.go 的 BenefitResetAt）。
//     命中后的处置是 ErrHardCredit（不可重试）——分类在 classify.go。
//  2. **积分账户**：`statistics/plugin` 的 `package.is_credit_package === true` 才是积分计费
//     账户；`is_token_package` 是旧 Token 账户，**不在每日签到活动范围**（签到返回 inactive）。
//     积分总额取 `usageTotalPackageCredit.package_credit_remain`、**不累加分类**，
//     总额缺失才回退分类求和；无任何 credit metric ≠ 余额 0（见下 creditDetail 注释）。
//  3. **签到三步**：查账户类型 → 读活动列表（找 `type == USER_LOGIN`）→ 幂等预检 →
//     claim（`campaignId` 必须**转成字符串**再回传）→ 必要时 confirm（漏掉会让积分停在
//     待确认而不入账；confirm 失败**不**把整体判失败）。
//
// 两种信封不能一刀切：`ops/*` 是 `{code,message,data}`（`code != 0` 即业务失败），
// `statistics/plugin` 是**裸对象**（protocol §2.6(b) 末条）。
package codearts

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// flexFloat 兼容上游把数值写成字符串（"800.00"）或数字两种形态。
// 解析不了时保持 0（**不**把它当失败：这些字段都是展示/记账用，不该让整次查询失败）。
type flexFloat float64

// UnmarshalJSON 实现 json.Unmarshaler。
func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		return nil
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		str = strings.TrimSpace(str)
		if str == "" {
			return nil
		}
		if v, err := strconv.ParseFloat(str, 64); err == nil {
			*f = flexFloat(v)
		}
		return nil
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		*f = flexFloat(v)
	}
	return nil
}

// creditLabels 积分 metric 的中文展示名（只用于面板文案）。
var creditLabels = map[string]string{
	creditTotal:    "积分总额",
	creditBasic:    "基础套餐积分",
	creditOnDemand: "按需积分",
	creditBonus:    "奖励积分",
}

// isCategoryCredit 报告该 metric 是否属「分类」（总额之外的那三类）。
func isCategoryCredit(name string) bool {
	switch name {
	case creditBasic, creditOnDemand, creditBonus:
		return true
	default:
		return false
	}
}

// pluginStatistics `GET {Base}/snap-manager/v1/statistics/plugin` 的响应（**裸对象**）。
type pluginStatistics struct {
	Package struct {
		PackageNameCN   string `json:"package_name_cn"`
		PackageNameEN   string `json:"package_name_en"`
		SpecCode        string `json:"spec_code"`
		Status          string `json:"status"`
		IsCreditPackage bool   `json:"is_credit_package"`
		IsTokenPackage  bool   `json:"is_token_package"`
	} `json:"package"`
	Metrics []struct {
		Name                string    `json:"name"`
		PackageCreditAmount flexFloat `json:"package_credit_amount"`
		PackageCreditUsed   flexFloat `json:"package_credit_used"`
		PackageCreditRemain flexFloat `json:"package_credit_remain"`
	} `json:"metrics"`
}

// opsEnvelope `ops/*` 的统一信封：`code != 0` 即业务失败。
type opsEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// campaignItem 活动条目（`data.items[]`）。
type campaignItem struct {
	Type string `json:"type"`
	// CampaignID 服务端下发为**数字**（实测 1），故用 RawMessage 承接再归一成字符串
	// （协议 §2.6(c) 第 4 步的踩坑点）。
	CampaignID json.RawMessage `json:"campaignId"`
	Name       string          `json:"name"`
	// Claimable 是指针：nil = 上游没给该字段（与显式 false 语义不同）。
	Claimable     *bool     `json:"claimable"`
	Status        string    `json:"status"`
	BenefitAmount flexFloat `json:"benefitAmount"`
}

// campaignIDString 归一 campaignId 为字符串。
func (c campaignItem) campaignIDString() string { return jsonStringOf(c.CampaignID) }

// claimResult claim 响应的 `data`。
type claimResult struct {
	ID            json.RawMessage `json:"id"`
	BenefitAmount flexFloat       `json:"benefitAmount"`
	Credit        flexFloat       `json:"credit"`
	Credits       flexFloat       `json:"credits"`
	CreditAmount  flexFloat       `json:"creditAmount"`
	Amount        flexFloat       `json:"amount"`
}

// amountOf 取本次发放积分（多级回退，协议 §2.6(c) 第 6 步）。
func (r claimResult) amountOf(fallback int64) int64 {
	for _, v := range []flexFloat{r.BenefitAmount, r.Credit, r.Credits, r.CreditAmount, r.Amount} {
		if v > 0 {
			return int64(v)
		}
	}
	return fallback
}

// ---------------------------------------------------------------------------
// provider.Upstream：积分 / 额度
// ---------------------------------------------------------------------------

// UserResource 返回可消耗积分余额（协议 §2.6(b)）。
//
// 查询失败必须返回 error（不能 return 0,nil）：否则 creditSnapshot 会把「查询失败」
// 当成「余额归零」，对上次快照的所有 key 记一条假 expire 事件污染 ledger 差分（R17），
// 并让 pool 按虚低余额排号。与 officeace/minimax 同口径。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	remain, _, err := c.creditDetail(a)
	return remain, err
}

// UserResourceDetail 返回余额与明细条目（失败同样上抛）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return c.creditDetail(a)
}

// creditDetail 单次请求同时产出总额与明细（口径同源，避免两处各算一遍）。
func (c *Client) creditDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	st, err := c.fetchStatistics(a)
	if err != nil {
		return 0, nil, err
	}
	var total, categorySum int64
	var items []provider.ResourceItem
	for _, m := range st.Metrics {
		name := strings.TrimSpace(m.Name)
		if name == "" {
			continue
		}
		remain := int64(m.PackageCreditRemain)
		item := provider.ResourceItem{
			Name:   firstNonEmpty(creditLabels[name], name),
			Total:  int64(m.PackageCreditAmount),
			Used:   int64(m.PackageCreditUsed),
			Remain: remain,
			Key:    name,
			Usable: true,
		}
		switch {
		case name == creditTotal:
			total = remain
			// 总额条目只作展示：分类条目已计入明细，再算一次就是重复计数
			// （InfoOnly 的语义正是「展示但不参与任何积分算术」）。
			item.InfoOnly = true
			items = append(items, item)
		case isCategoryCredit(name):
			categorySum += remain
			items = append(items, item)
		default:
			// 未识别的 metric：仍然展示，但不参与求和小计（InfoOnly）。
			item.InfoOnly = true
			items = append(items, item)
		}
	}
	if total == 0 && categorySum > 0 {
		// 总额缺失才回退分类求和（协议 §2.6(b)：**不累加分类**是常态口径）。
		total = categorySum
	}
	if len(st.Metrics) == 0 {
		// ⚠️ 无任何 credit metric ≠ 余额为 0：这种情况多半是「非积分计费账户」
		// （Token 套餐）。此时返回 0 是事实（确实没有积分余额），但**必须**给一条
		// 说明条目，否则面板会把它显示成「余额 0」而不是「该账户没有积分口径」。
		pkg := firstNonEmpty(st.Package.PackageNameCN, st.Package.PackageNameEN, st.Package.SpecCode, "未知套餐")
		items = append(items, provider.ResourceItem{
			Name:   "账户类型：" + pkg + "（非积分计费，无积分余额）",
			Usable: true, InfoOnly: true, Key: "package",
		})
	}
	return total, items, nil
}

// fetchStatistics 拉账户/套餐 + 积分（裸对象响应）。
func (c *Client) fetchStatistics(a *auth.Auth) (*pluginStatistics, error) {
	ac := accountFromAuth(a)
	if ac.AccessKeyID == "" || ac.SecretAccessKey == "" {
		return nil, &provider.Error{Kind: provider.ErrSessionDead,
			Msg: "codearts：凭据不完整（缺 access_key_id / secret_access_key）"}
	}
	status, raw, err := c.doSigned(c.httClient(), ac, http.MethodGet, c.base()+EpStatisticsPlugin, "", nil, nil, controlHeaders())
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, &provider.Error{Kind: Classify(status, string(raw)), Status: status, Msg: snippet(raw)}
	}
	var st pluginStatistics
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("codearts：statistics/plugin 响应解析失败: %w", err)
	}
	return &st, nil
}

// ---------------------------------------------------------------------------
// provider.Upstream / CheckinReporter / CheckinGranter：每日签到
// ---------------------------------------------------------------------------

// DailyCheckin 实现 provider.Upstream（幂等语义：今日已领取不算失败）。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.DailyCheckinReport(a)
	return err
}

// DailyCheckinReport 实现 provider.CheckinReporter：返回结构化状态，让调度器能把
// 「本次领到 / 今天已领过 / 没有活动 / 真错误」分开处置（error 通道区分不了后两者）。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	uid := ""
	if a != nil {
		uid = a.UID
	}
	// 结构化日志行（供 stats 事件解析签到收入，格式与 qoder/officeace 同构；R43）。
	// 只在 claimed 时带 amount；日志零 token。
	ret := func(st provider.CheckinStatus, amount int64, msg string, err error) (provider.CheckinReport, error) {
		log.Printf("codearts checkin uid=%s status=%s amount=%d msg=%s err=%v", uid, st, amount, msg, err)
		return provider.CheckinReport{Status: st, Msg: msg, Amount: amount}, err
	}

	ac := accountFromAuth(a)
	if ac.AccessKeyID == "" || ac.SecretAccessKey == "" {
		return ret(provider.CheckinNoToken, 0, "无可用凭据（缺 AK/SK），需重新登录", nil)
	}

	// 第 1 步：账户类型（查询失败 → error；非积分账户 → inactive）。
	st, err := c.fetchStatistics(a)
	if err != nil {
		return ret(provider.CheckinError, 0, "账户类型查询失败："+err.Error(), err)
	}
	if !st.Package.IsCreditPackage {
		return ret(provider.CheckinNoCampaign, 0, "非积分计费账户（Token 套餐），不在每日签到活动范围", nil)
	}

	// 第 2 步：活动列表（只认 type == USER_LOGIN）。
	items, err := c.fetchCampaigns(ac)
	if err != nil {
		return ret(provider.CheckinError, 0, "活动列表查询失败："+err.Error(), err)
	}
	camp, ok := findUserLoginCampaign(items)
	if !ok {
		return ret(provider.CheckinNoCampaign, 0, "暂无每日签到活动（其它类型活动不属于每日签到）", nil)
	}
	id := camp.campaignIDString()
	if id == "" {
		return ret(provider.CheckinError, 0, "活动缺 campaignId，放弃领取", nil)
	}

	// 第 3 步：幂等预检（唯一幂等手段：无幂等键、无「今天已签到」业务码）。
	if camp.Claimable != nil && !*camp.Claimable {
		switch strings.ToUpper(strings.TrimSpace(camp.Status)) {
		case campaignStatusClaimed, campaignStatusConfirmed, campaignStatusConsumed:
			return ret(provider.CheckinAlready, 0, "今日已领取（status="+camp.Status+"）", nil)
		default:
			return ret(provider.CheckinNoCampaign, 0, "活动当前不可领取（status="+camp.Status+"）", nil)
		}
	}

	// 第 4 步：claim（campaignId 必须转字符串）。
	res, err := c.postClaim(ac, id)
	if err != nil {
		return ret(provider.CheckinError, 0, "领取失败："+err.Error(), err)
	}
	// 第 5 步：confirm（仅当 claim 响应带 data.id；失败**不**判整体失败）。
	if claimID := jsonStringOf(res.ID); claimID != "" {
		if err := c.postConfirm(ac, id); err != nil {
			log.Printf("codearts checkin confirm failed uid=%s campaign=%s err=%v（不影响本次签到结论）", uid, id, err)
		}
	}
	amount := res.amountOf(int64(camp.BenefitAmount))
	return ret(provider.CheckinClaimed, amount, fmt.Sprintf("签到成功 +%d 积分", amount), nil)
}

// DailyCheckinGrant 实现 provider.CheckinGranter：把本次发放额交给调度器记账
// （避免完全依赖快照差分）。amount<=0 时调度器自行降级回差分口径。
func (c *Client) DailyCheckinGrant(a *auth.Auth) (int64, error) {
	rep, err := c.DailyCheckinReport(a)
	if err != nil {
		return 0, err
	}
	if rep.Status != provider.CheckinClaimed {
		return 0, nil
	}
	return rep.Amount, nil
}

// fetchCampaigns 拉活动列表（`GET /v1/ops/delivery?channel=IDE`）。
func (c *Client) fetchCampaigns(ac *Account) ([]campaignItem, error) {
	env, err := c.opsGet(ac, c.base()+EpOpsDelivery+"?channel="+ChannelIDE)
	if err != nil {
		return nil, err
	}
	var data struct {
		Items []campaignItem `json:"items"`
	}
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &data); err != nil {
			return nil, fmt.Errorf("活动列表 data 解析失败: %w", err)
		}
	}
	return data.Items, nil
}

// findUserLoginCampaign 在活动列表里找每日签到活动（type == USER_LOGIN）。
func findUserLoginCampaign(items []campaignItem) (campaignItem, bool) {
	for _, it := range items {
		if strings.EqualFold(strings.TrimSpace(it.Type), CampaignTypeUserLogin) {
			return it, true
		}
	}
	return campaignItem{}, false
}

// postClaim 领取积分（`POST /v1/ops/claim`，body `{campaignId, channel:"IDE"}`）。
func (c *Client) postClaim(ac *Account, campaignID string) (claimResult, error) {
	body, _ := json.Marshal(map[string]string{"campaignId": campaignID, "channel": ChannelIDE})
	env, err := c.opsPost(ac, c.base()+EpOpsClaim, body)
	if err != nil {
		return claimResult{}, err
	}
	var res claimResult
	if len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, &res); err != nil {
			return claimResult{}, fmt.Errorf("claim data 解析失败: %w", err)
		}
	}
	return res, nil
}

// postConfirm 领取确认（`POST /v1/ops/confirm`，body `{campaignId}`）。
func (c *Client) postConfirm(ac *Account, campaignID string) error {
	body, _ := json.Marshal(map[string]string{"campaignId": campaignID})
	_, err := c.opsPost(ac, c.base()+EpOpsConfirm, body)
	return err
}

// opsGet 发一次带信封的 GET。
func (c *Client) opsGet(ac *Account, url string) (opsEnvelope, error) {
	return c.opsDo(http.MethodGet, ac, url, nil)
}

// opsPost 发一次带信封的 POST（JSON 体，content-type 参与签名）。
func (c *Client) opsPost(ac *Account, url string, body []byte) (opsEnvelope, error) {
	return c.opsDo(http.MethodPost, ac, url, body)
}

// opsDo 执行 `ops/*` 请求并解信封（`code != 0` 即业务失败）。
func (c *Client) opsDo(method string, ac *Account, url string, body []byte) (opsEnvelope, error) {
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	status, raw, err := c.doSigned(c.httClient(), ac, method, url, contentType, body, nil, controlHeaders())
	if err != nil {
		return opsEnvelope{}, err
	}
	if status >= 400 {
		return opsEnvelope{}, &provider.Error{Kind: Classify(status, string(raw)), Status: status, Msg: snippet(raw)}
	}
	var env opsEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return opsEnvelope{}, fmt.Errorf("ops 响应解析失败: %w", err)
	}
	if env.Code != 0 {
		return env, &provider.Error{Kind: provider.ErrClient, Status: status,
			Msg: fmt.Sprintf("ops 业务失败 code=%d message=%s", env.Code, snippet([]byte(env.Message)))}
	}
	return env, nil
}
