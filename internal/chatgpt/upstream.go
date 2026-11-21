package chatgpt

import (
	"fmt"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 编译期断言：本包必须实现 provider.Upstream 全量方法（少一个就编译失败，
// 而不是等到 main.go 装配时才暴露）。
var _ provider.Upstream = (*Client)(nil)

// FetchModels 把上游网页目录（/backend-api/models）转成 v2 的模型清单。
//
// R49 纪律：只如实转述上游给的 slug / title / max_tokens，**不编造**上下文窗口
// 与任何能力声明 —— 上游目录里根本没有这两类字段，补全等于替上游说话。
// 因此 ContextWindow / SupportsTools / SupportsReasoning / SupportsImages 全部留零值
// （=「未声明」），面板与 `/v1/models` 据此不展示、不宣告。
func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	ms, err := c.Models(a)
	if err != nil {
		return nil, err
	}
	out := make([]provider.ModelInfo, 0, len(ms))
	for _, m := range ms {
		slug := strings.TrimSpace(m.Slug)
		if slug == "" {
			continue // 无 slug 的条目无法路由，跳过而不是编一个 ID
		}
		name := strings.TrimSpace(m.Title)
		if name == "" {
			name = slug
		}
		out = append(out, provider.ModelInfo{ID: slug, Name: name, MaxTokens: m.MaxTokens})
	}
	return out, nil
}

// staticModels 是最小静态兜底表：上游目录拉不到时至少让渠道可用。
//
// 只有一条 `auto` —— 它是上游「自动选模型」的真实取值（见 defaultChatModel，
// 对话请求体在没有指定模型时也用它）。刻意**不**在这里列具体型号 slug：
// 那些 slug 会随上游改版变化，写死等于编造（R49/R143）。
var staticModels = []provider.ModelInfo{
	{ID: defaultChatModel, Name: "自动选择"},
}

// StaticModels 静态兜底模型表（`/v1/models` 在上游目录不可用时使用）。
func StaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo(nil), staticModels...)
}

// FetchModelPricing 上游网页版没有费率端点：目录里没有单价字段，
// 逐请求响应也不带 cost（与 cline 的处境相同）⇒ 返回 (nil, nil) 而不是错误。
func (c *Client) FetchModelPricing(a *auth.Auth) ([]provider.ModelPricing, error) { return nil, nil }

// UserResource 上游没有可查询的余额端点。
//
// 网页套餐的「额度」是窗口内的用量上限，只在对话流的限流报文里体现，
// 没有任何接口能读出剩余数字；返回 error 会让面板与 pool 报错，
// 「0 + nil」才是如实事实（口径同 cline，见 internal/cline/client.go）。
func (c *Client) UserResource(a *auth.Auth) (int64, error) { return 0, nil }

// UserResourceDetail 无余额端点 ⇒ 美元明细可展示 ⇒ (0, nil, nil)。
func (c *Client) UserResourceDetail(a *auth.Auth) (int64, []provider.ResourceItem, error) {
	return 0, nil, nil
}

// DailyCheckin 本渠道没有签到活动：网页套餐不存在「领积分」动作。
//
// 调度器的签到时刻传的是显式空切片（R39），正常情况下不会被调用；
// 保留可读错误是为了防止将来有人误接一个必然失败的签到任务。
func (c *Client) DailyCheckin(a *auth.Auth) error {
	return fmt.Errorf("chatgpt 无签到活动（网页套餐无领取动作）")
}

// Classify 上游错误分类，委托包级 Classify（429 优先 → 文案指纹 → 状态码兜底）。
func (c *Client) Classify(status int, body string) provider.ErrKind {
	return Classify(status, body)
}
