package officeace

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 华为云 AgentArts / 模型网关常量。
const (
	defaultGateway = "https://modelgw-0004.officeace.cn-southwest-2.huaweicloud-agentarts.com"
	oauthClientID  = "pdp5_for_agentarts"
	pointsAttrCode = "officeace_points"
)

// 生产上游地址。单独列成常量，供 httptest 用例结束时还原（防串测）。
const (
	productionAgentartsBase = "https://agentarts.cn-southwest-2.myhuaweicloud.com"
	productionStsTokenURL   = "https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens"
	productionAuthBase      = "https://auth.huaweicloud.com"
)

// 三个上游地址做成**变量**的唯一原因是可测性：登录链路（state → code → STS → validate）
// 必须能在 httptest 假上游上端到端回归——否则「新增账号不成功」这一类断点只能靠真实
// 华为账号实盘才能发现，而实盘会产生真实 state/令牌消耗与风控暴露（AGENTS.md §0.1）。
// 生产运行时一律保持默认值；**没有任何代码路径会改写它们**（仅 *_test.go 内临时替换）。
var (
	agentartsBase = productionAgentartsBase
	stsTokenURL   = productionStsTokenURL
	authBase      = productionAuthBase
)

// UserResource 返回该账号**可消耗积分**（套餐剩余 + 奖励剩余）。
// 查询失败必须返回 error（不能 return 0,nil）——否则 creditSnapshot 会把「查询失败」
// 当成「余额归零」，对上次快照的所有 key 记一条假 expire 事件污染 ledger 差分（R17），
// 且让 pool 误判余额见底把账号排序垫底。与其它渠道口径一致：失败即上抛、由调用方跳过本轮。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	ac := c.ensureCredential(a)
	rem, _, err := c.subscriptionRemain(ac)
	if err != nil {
		return 0, err
	}
	return rem, nil
}

// UserResourceDetail 返回可消耗余额 + 明细条目（失败同样上抛，理由见 UserResource）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	ac := c.ensureCredential(a)
	rem, items, err := c.subscriptionRemain(ac)
	if err != nil {
		return 0, nil, err
	}
	return rem, items, nil
}

// subscriptionRemain 查询订阅，汇总套餐与奖励积分。
func (c *Client) subscriptionRemain(ac *Account) (int64, []provider.ResourceItem, error) {
	sub, err := c.fetchSubscription(ac)
	if err != nil {
		return 0, nil, err
	}
	now := time.Now().UnixMilli()
	var items []provider.ResourceItem
	var total, used int64

	// 套餐额度
	for _, sku := range sub.SKUs {
		for _, q := range sku.Quotas {
			if q.ExpiredTime != "" {
				if t, ok := parseISO(q.ExpiredTime); ok && t <= now {
					continue
				}
			}
			if q.SKUAttrCode != pointsAttrCode {
				continue
			}
			tv := atoiSafe(q.SKUValue)
			cv := atoiSafe(q.CurrentValue)
			total += tv
			used += cv
			exp := ""
			if q.ExpiredTime != "" {
				if t, ok := parseISO(q.ExpiredTime); ok {
					exp = time.UnixMilli(t).In(expireLoc).Format(time.RFC3339)
				}
			}
			items = append(items, provider.ResourceItem{
				Name:     sku.SKUName + "·套餐",
				Total:    tv,
				Used:     cv,
				Remain:   maxInt64(0, tv-cv),
				ExpireAt: exp,
				Usable:   true,
			})
		}
	}
	// 奖励额度（签到/活动）
	for _, b := range sub.BonusSKUs {
		if t, ok := parseISO(b.ExpiredTime); ok && t <= now {
			continue
		}
		tv := atoiSafe(b.Points)
		cv := atoiSafe(b.CurrentValue)
		total += tv
		used += cv
		exp := ""
		if t, ok := parseISO(b.ExpiredTime); ok {
			exp = time.UnixMilli(t).In(expireLoc).Format(time.RFC3339)
		}
		items = append(items, provider.ResourceItem{
			Name:     firstNonEmpty(b.ActivityName, "奖励"),
			Total:    tv,
			Used:     cv,
			Remain:   maxInt64(0, tv-cv),
			ExpireAt: exp,
			Usable:   true,
		})
	}
	remain := total - used
	if remain < 0 {
		remain = 0
	}
	return remain, items, nil
}

// fetchSubscription 调 {gateway}/v1/subscription（V11 签名）。
func (c *Client) fetchSubscription(ac *Account) (*subscription, error) {
	gw, err := c.gatewayBase(ac)
	if err != nil {
		return nil, err
	}
	u := trimSlash(gw) + "/v1/subscription"
	body, status, err := c.doV11(ac, http.MethodGet, u, "", "")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("officeace: subscription HTTP %d: %s", status, truncate(body, 200))
	}
	var sub subscription
	if err := json.Unmarshal(body, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// gatewayBase 解析账号专属网关（**裸 host**）：配置里没有就读 client-permission-validate。
// 返回前一律过 bareGateway：额度/签到端点直接拼 `/v1/subscription`，不得带 `/v2`（#152）。
//
// 合并 upstream/master 的说明：本函数原用 `gatewayRootOf`，与上游 `bareGateway` 是同一归一的
// 两个名字（上游 issue #152 独立修掉了同一处缺陷）。合并时统一取上游名 `bareGateway`，
// 并把本地版更严的「退化输入不产出空 base / 大小写不敏感」守卫并进该函数（见 account.go）。
// 数据面由 `Account.dataBase()` 反向补 `/v2`。
func (c *Client) gatewayBase(ac *Account) (string, error) {
	if ac.BaseURL != "" {
		return bareGateway(ac.BaseURL), nil
	}
	// 无网关凭据时尝试用临时凭据问控制面。
	base, _, err := c.clientPermissionValidate(ac)
	if err != nil || base == "" {
		return bareGateway(defaultGateway), nil
	}
	return bareGateway(base), nil
}

// clientPermissionValidate 调 {agentartsBase}/v1/claw/client-permission-validate（V4 签名）。
// 返回网关地址 model_api_url_base 与 model_app_key/secret。
func (c *Client) clientPermissionValidate(ac *Account) (string, map[string]string, error) {
	if ac.AccessKeyID == "" || ac.SecretAccessKey == "" {
		return "", nil, fmt.Errorf("officeace: no temp credential")
	}
	u := agentartsBase + "/v1/claw/client-permission-validate"
	body, status, err := c.doV4(ac, http.MethodGet, u, "", "application/json;charset=utf8")
	if err != nil {
		return "", nil, err
	}
	if status != http.StatusOK {
		return "", nil, fmt.Errorf("officeace: cpv HTTP %d: %s", status, truncate(body, 200))
	}
	var resp struct {
		ModelInfo struct {
			ModelAPIURLBase string `json:"model_api_url_base"`
			ModelAuthInfo   struct {
				ModelAppKey    string `json:"model_app_key"`
				ModelAppSecret string `json:"model_app_secret"`
			} `json:"model_auth_info"`
		} `json:"model_info"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", nil, err
	}
	base := resp.ModelInfo.ModelAPIURLBase
	if !strings.HasPrefix(base, "http") && base != "" {
		base = "https://" + base
	}
	authInfo := map[string]string{
		"key":    resp.ModelInfo.ModelAuthInfo.ModelAppKey,
		"secret": resp.ModelInfo.ModelAuthInfo.ModelAppSecret,
	}
	return trimSlash(base), authInfo, nil
}

// doV11 执行 V11 签名的请求（GET 无 body；POST 传 bodyText）。
func (c *Client) doV11(ac *Account, method, rawURL, bodyText, contentType string) ([]byte, int, error) {
	extra := map[string]string{"x-subscription-type": "v2"}
	hdrs, err := SignV11(ac.AccessKeyID, ac.SecretAccessKey, ac.SecurityToken, ac.ProjectID,
		method, rawURL, bodyText, contentType, extra, time.Now())
	if err != nil {
		return nil, 0, err
	}
	return c.doReq(method, rawURL, bodyText, hdrs)
}

// doV4 执行 V4 签名的请求。
func (c *Client) doV4(ac *Account, method, rawURL, bodyText, contentType string) ([]byte, int, error) {
	hdrs, err := SignV4(ac.AccessKeyID, ac.SecretAccessKey, ac.SecurityToken, ac.ProjectID,
		method, rawURL, bodyText, contentType, nil, time.Now())
	if err != nil {
		return nil, 0, err
	}
	return c.doReq(method, rawURL, bodyText, hdrs)
}

func (c *Client) doReq(method, rawURL, bodyText string, hdrs map[string]string) ([]byte, int, error) {
	var rd io.Reader
	if bodyText != "" {
		rd = strings.NewReader(bodyText)
	}
	req, err := http.NewRequest(method, rawURL, rd)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	if bodyText != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return rb, resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// 签到（每日奖励积分）
// ---------------------------------------------------------------------------

// DailyCheckin 每日签到：POST {gateway}/v1/subscription/bonus/claim（V11 签名，body {}）。
// 上游对未带 body 的 POST 会回 500 OfficeAce.11020001，故必须带 Content-Type + {}。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	_, err := c.DailyCheckinReport(a)
	return err
}

// DailyCheckinReport 结构化上报签到结果。
func (c *Client) DailyCheckinReport(a *auth.Auth) (provider.CheckinReport, error) {
	uid := a.UID
	// 结构化日志行（供 stats 事件解析签到收入，格式与 qoder/raccoon 同构；R43）。
	// 只在 claimed 时带 amount；其余状态 amount=0。日志零 token。
	ret := func(st provider.CheckinStatus, claimed bool, amount int64, msg string, err error) (provider.CheckinReport, error) {
		log.Printf("officeace checkin bonus uid=%s status=%s claimed=%t amount=%d msg=%s err=%v",
			uid, st, claimed, amount, msg, err)
		return provider.CheckinReport{Status: st, Msg: msg, Amount: amount}, err
	}
	ac := c.ensureCredential(a)
	if ac.AccessKeyID == "" || ac.SecretAccessKey == "" {
		return ret(provider.CheckinNoToken, false, 0, "无临时凭据，需先续期/登录", nil)
	}
	gw, err := c.gatewayBase(ac)
	if err != nil {
		return ret(provider.CheckinError, false, 0, err.Error(), err)
	}
	u := trimSlash(gw) + "/v1/subscription/bonus/claim"
	body, status, err := c.doV11(ac, http.MethodPost, u, "{}", "application/json;charset=utf8")
	if err != nil {
		return ret(provider.CheckinError, false, 0, "签到请求失败："+err.Error(), err)
	}
	if status != http.StatusOK {
		return ret(provider.CheckinError, false, 0, fmt.Sprintf("签到被拒 HTTP %d：%s", status, truncate(body, 200)), nil)
	}
	var resp struct {
		BonusSKUs []bonusSKU `json:"bonus_skus"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return ret(provider.CheckinError, false, 0, "签到响应解析失败："+err.Error(), nil)
	}
	// 上游对已领过的号不报错，只回同样列表。判定「本次是否新发放」需对比余额：
	// 简化口径——只要存在 remaining>0 的奖励即视为已有额度（幂等），不去重发放。
	var gained int64
	for _, b := range resp.BonusSKUs {
		r := atoiSafe(b.Points) - atoiSafe(b.CurrentValue)
		if r > 0 {
			gained += r
		}
	}
	if gained > 0 {
		return ret(provider.CheckinClaimed, true, gained, fmt.Sprintf("签到成功，剩余奖励 %d", gained), nil)
	}
	return ret(provider.CheckinAlready, false, 0, "今日已领取或暂无奖励", nil)
}

// ---------------------------------------------------------------------------
// 响应结构
// ---------------------------------------------------------------------------

type subscription struct {
	SKUs      []sku      `json:"skus"`
	BonusSKUs []bonusSKU `json:"bonus_skus"`
}

type sku struct {
	SKUName     string  `json:"sku_name"`
	ExpiredTime string  `json:"expired_time"`
	Quotas      []quota `json:"quotas"`
}

type quota struct {
	SKUAttrCode  string `json:"sku_attr_code"`
	SKUValue     string `json:"sku_value"`
	CurrentValue string `json:"current_value"`
	ExpiredTime  string `json:"expired_time"`
}

type bonusSKU struct {
	ActivityName string `json:"activity_name"`
	Points       string `json:"points"`
	CurrentValue string `json:"current_value"`
	ExpiredTime  string `json:"expired_time"`
}

// parseISO 解析 ISO8601（含毫秒）为 Unix 毫秒。
func parseISO(s string) (int64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

func atoiSafe(s string) int64 {
	v, _ := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return v
}

func truncate(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

var expireLoc = time.FixedZone("UTC+8", 8*60*60)
