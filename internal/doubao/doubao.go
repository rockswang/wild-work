// doubao.go 渠道包的 Upstream 拼装 + 工作模式参数。
//
// 本文件把 client / chat / sse / checkin / credits 五块拼成 provider.Upstream，
// 并定义工作模式（agent_mode=1）的请求参数来源。
package doubao

import (
	"net/http"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// workOptions 工作模式参数（当前全为渠道级常量，后续可改成按账号配置）。
type workOptions struct {
	MemoryProfile   string
	TotalWindow     string
	ReasoningEffort string
	DeviceName      string
	LocalAppID      string
	LocalDeviceID    string
	Workspace       string
}

func defaultWorkOptions() workOptions {
	return workOptions{
		MemoryProfile:   "300k_230",
		TotalWindow:     "300000",
		ReasoningEffort: WorkReasoningEffort,
		DeviceName:      "WildWork",
		LocalAppID:      "582478",
	}
}

// parseChatRequest 解出客户端 OpenAI 请求里本渠道用得到的字段。
func parseChatRequest(body []byte) (*chatRequest, error) {
	var req chatRequest
	if err := jsonUnmarshal(body, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

// outboundBody 由客户端 OpenAI 请求体构造上游 /chat/completion 请求体。
//
// model 从**请求体里**的 model 字段取（形如 `doubao/deepseek-v4-flash`，R52 单一来源），
// 剥掉渠道前缀后查静态表拿 model_item_key。未知模型**本地拒绝**（ErrBadParams，请求级
// 错误，不罚账号不轮转）——上游对未知模型的行为未知，不能拿账号去试。
//
// ⚠️ dev（device_id）必须透传：/chat/completion 的 agent_task_param.local_device_id 要用。
//
// ⚠️ 历史上在这里栽过一个大坑（2026-11-15）：ChatStream 一度**没有调用本函数**，把客户端
// 的 OpenAI 请求体原样转发，上游报 `invalid character 'h' ...`，而表面的 710020202
// 完全指向错误方向。
func outboundBody(body []byte, dev string) ([]byte, error) {
	req, err := parseChatRequest(body)
	if err != nil {
		return nil, &provider.Error{Kind: provider.ErrBadParams, Msg: "doubao：请求体解析失败: " + err.Error()}
	}
	name := req.Model
	if i := strings.Index(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	if name == "" {
		name = DefaultModel
	}
	spec, ok := staticModels[name]
	if !ok {
		return nil, &provider.Error{
			Kind: provider.ErrBadParams,
			Msg:  "doubao：未知模型 " + name + "（可用模型见 /v1/models）",
		}
	}
	// 思考强度：工作模式固定深思考（reasoning_effort=4）。客户端的 low/medium/high 不是
	// 豆包档位、暂不映射（R63 出站注入留待后续，不编造档位）。
	return buildChatBody(req, spec.ItemKey, "", dev, spec.MemoryProfile, spec.TotalWindow)
}

// ---------- 编译期断言：Client 实现 provider.Upstream ----------

var _ provider.Upstream = (*Client)(nil)
var _ provider.CheckinReporter = (*Client)(nil)

// authWithCookie 由裸 Cookie 构造一个最小凭据（供包内校验用）。
func authWithCookie(cookie, deviceID, webID, fp string) *auth.Auth {
	return &auth.Auth{
		Kind:        "doubao",
		AccessToken: cookie,
		Extra: map[string]string{
			ExtraCookie:   cookie,
			ExtraDeviceID: deviceID,
			ExtraWebID:    webID,
			ExtraFP:       fp,
		},
	}
}

// 供 outboundBody 使用的小封装（避免在本文件重复 import encoding/json）。
func jsonUnmarshal(b []byte, v any) error { return jsonUnmarshalImpl(b, v) }

var _ = http.MethodPost
