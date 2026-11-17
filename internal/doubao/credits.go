// credits.go 豆包额度查询。
//
// 端点：POST /alice/commerce/sale/subscription/quota/summary/（body {}）
//
// 上游把额度拆成多个「窗口限额」（window_limits），每个有 total_amount /
// used_amount / used_percent / end_time。本渠道**递归累加**所有窗口，
// remain = Σtotal − Σused，与参考实现同口径。
//
// ⚠️ 豆包的额度是「窗口限额」不是积分计价，故：
//   - 不参与任何「积分临期先用」排序（条目不带 ExpireAt，面板到期列显示「—」）；
//   - 费率面返回空（FetchModelPricing nil），不编造倍率（R49）。
package doubao

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// quotaWindow 一个窗口限额。
type quotaWindow struct {
	Name    string
	Total   float64
	Used    float64
	EndTime int64
}

// quotaSummary 累加结果。
type quotaSummary struct {
	Windows []quotaWindow
	Total   float64
	Used    float64
}

// fetchQuota 拉额度汇总。
func (c *Client) fetchQuota(a *auth.Auth) (quotaSummary, error) {
	var out quotaSummary
	cookie, dev, webID, fp := devOf(a)
	if cookie == "" {
		return out, &provider.Error{Kind: provider.ErrSessionDead, Msg: "doubao：账号没有 Cookie，请重新扫码登录"}
	}
	target, err := c.identityURL(QuotaPath, cookie, dev, webID, fp)
	if err != nil {
		return out, err
	}
	resp, raw, err := c.do(http.MethodPost, target, cookie, []byte("{}"))
	if err != nil {
		return out, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return out, &provider.Error{Kind: provider.ErrSessionDead, Status: resp.StatusCode, Msg: "doubao 额度 401: " + truncateStr(string(raw), 200)}
	}
	if resp.StatusCode != http.StatusOK {
		return out, &provider.Error{Kind: c.Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: "doubao 额度上游 " + resp.Status + ": " + truncateStr(string(raw), 200)}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return out, &provider.Error{Kind: provider.ErrClient, Msg: "doubao 额度响应解析失败: " + truncateStr(string(raw), 200)}
	}
	collectQuota(payload, &out)
	return out, nil
}

// collectQuota 递归找出所有 window_limits 并累加（参考实现同款）。
func collectQuota(v any, out *quotaSummary) {
	switch t := v.(type) {
	case map[string]any:
		if limits, ok := t["window_limits"].([]any); ok {
			gname, _ := t["feature_group_name"].(string)
			for _, l := range limits {
				lm, ok := l.(map[string]any)
				if !ok {
					continue
				}
				total := numOf(lm["total_amount"])
				used := numOf(lm["used_amount"])
				if total <= 0 && used <= 0 {
					continue
				}
				out.Total += total
				out.Used += used
				out.Windows = append(out.Windows, quotaWindow{
					Name: gname, Total: total, Used: used,
					EndTime: int64(numOf(lm["end_time"])),
				})
			}
		}
		for _, child := range t {
			collectQuota(child, out)
		}
	case []any:
		for _, child := range t {
			collectQuota(child, out)
		}
	}
}

// UserResource 实现 provider.Upstream：剩余额度。
func (c *Client) UserResource(a *auth.Auth) (int64, error) {
	q, err := c.fetchQuota(a)
	if err != nil {
		return 0, err
	}
	return int64(q.Total - q.Used), nil
}

// UserResourceDetail 实现 provider.Upstream：剩余额度 + 明细条目。
//
// 明细按「窗口」逐条给出（名称 + 总量 + 剩余），不带到期时间（上游不下发
// 可消费的到期字段；伪造成「永不过期」违反不变量 18）。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	q, err := c.fetchQuota(a)
	if err != nil {
		return 0, nil, err
	}
	remain := q.Total - q.Used
	if remain < 0 {
		remain = 0
	}
	items := make([]provider.ResourceItem, 0, len(q.Windows)+1)
	for _, w := range q.Windows {
		name := w.Name
		if name == "" {
			name = "额度窗口"
		}
		items = append(items, provider.ResourceItem{
			Name:   name,
			Total:  int64(w.Total),
			Used:   int64(w.Used),
			Remain: int64(w.Total - w.Used),
			Usable: true,
		})
	}
	// 汇总条（池子为空时也能看到总量）。
	items = append(items, provider.ResourceItem{
		Name: "可用额度", Total: int64(q.Total), Used: int64(q.Used), Remain: int64(remain), Usable: true,
	})
	return int64(remain), items, nil
}

// numOf 兼容 JSON 数字与数字字符串。
func numOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		clean := strings.NewReplacer(",", "", " ", "", "¥", "", "$", "").Replace(t)
		f, err := strconv.ParseFloat(strings.TrimSpace(clean), 64)
		if err != nil {
			return 0
		}
		return f
	}
	return 0
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
