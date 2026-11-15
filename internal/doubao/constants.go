// Package doubao 实现豆包（ByteDance Doubao）渠道。
//
// # 协议来源与选型（2026-11-15，本仓实测）
//
// 豆包 Web 有两套客户端身份，走错那套会**全量 429**：
//
//   - 网页端身份（aid=497858）：对话接口要 a_bogus 签名 + fp + msToken + device_id/web_id
//     一整套，缺一个就被判异常客户端。Agent2API 的 doubaowork 渠道死在这里。
//   - **桌面客户端身份（aid=582478）**：本包走这条。不需要 a_bogus、不需要 msToken。
//
// # 对话端点：/chat/completion（工作模式 agent_mode=1 + model_item_key）
//
// 2026-11-15 本仓在合格账号上实测打通：能选真模型（deepseek/kimi/glm 共 9 个）。
// 请求体形状逐项照搬 yyydsz/doubao2api（MIT）。曾走过 /samantha/chat/completion
// （只有思考档位、选不了模型），已废弃；初版在此用 /chat/completion 报 710020202 是
// **请求体形状不对**（缺 content_block / conversation_init_ext / model_config /
// general_task_param 一整套），非账号问题。
//
// # 已知限制（如实记录，勿夸大）
//
//  1. **身份参数会漂移**。pc_version / runtime_version / chromium_version 是快照。
//  2. **风控码 710022002/710022004** 分别对应「请求频率过高」与「触发人机验证」。
//  3. **Cookie 无自动续期**：上游没有 refresh 端点，过期后重新扫码（见 qr.go）。
//  4. **多模态未实现**：识图/读文件/文生图等是第二阶段，本包只做文本对话。
//  5. **每日签到仅「活动赠送额度」轨道账号可领**：新注册号被发「体验套餐」而非活动轨道，
//     领奖回「暂不符合领取条件」（官方客户端亦如此，实测）——见 R157。
package doubao

const (
	// BaseURL 豆包 Web 主站。
	BaseURL = "https://www.doubao.com"
	// ChatPath 对话端点（工作模式，带 model_item_key 选真模型）。
	ChatPath = "/chat/completion"
	// CheckinPath 每日签到（工作模式活动）。
	CheckinPath = "/alice/ug/campaign/landing/checkin"
	// QuotaPath 额度汇总。
	QuotaPath = "/alice/commerce/sale/subscription/quota/summary/"

	// UserAgent 桌面客户端形态（SamanthaDoubao 段是判据）。
	UserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Chrome/147.0.0.0 Safari/537.36 SamanthaDoubao/2.30.1"

	// CheckinScene 工作模式「每日签到」活动号。
	CheckinScene = "pc_incentive_task_mvp_v3"

	// WorkBotID 工作模式 bot id。
	WorkBotID = "7338286299411103781"
	// WorkModeID 工作模式 mode id。
	WorkModeID = "3"
	// WorkReasoningEffort 默认思考强度。
	WorkReasoningEffort = "4"
)

// 风控码（流内/响应体里的业务码）。
const (
	// RiskRateLimited 请求频率过高。
	RiskRateLimited = 710022002
	// RiskCaptcha 触发人机验证（场景 doubao_message_web）。
	RiskCaptcha = 710022004
	// CheckinNoCampaign 没有可用签到活动。
	CheckinNoCampaign = 710010702
)

// chatQuery 对话（/chat/completion）用的桌面身份参数。
//
// 带 PC 形态四参数（channel=win / web_platform=desktop / doubao_device_platform=desktop /
// doubao_pc_version）——与签到的 checkinQuery 同构，版本号取 yyydsz/doubao2api 的实测值
// （2026-11-15 本仓在合格账号上打通）。⚠️ samantha 端点拒这四个参数，但 /chat/completion 要。
var chatQuery = map[string]string{
	"aid":                    "582478",
	"real_aid":               "582478",
	"channel":                "win",
	"client_platform":        "pc_client",
	"device_platform":        "web",
	"doubao_device_platform": "desktop",
	"doubao_pc_version":      "2.30.1",
	"pc_version":             "2.30.1",
	"pkg_type":               "release_version",
	"language":               "zh",
	"region":                 "CN",
	"sys_region":             "CN",
	"runtime":                "web",
	"runtime_version":        "3.37.4",
	"samantha_web":           "1",
	"use-olympus-account":    "1",
	"version_code":           "20800",
	"web_platform":           "desktop",
	"chromium_version":       "147.0.7727.149",
}

// checkinQuery 签到接口用的身份参数（与对话同构，版本号略新）。
var checkinQuery = map[string]string{
	"aid":                    "582478",
	"real_aid":               "582478",
	"channel":                "win",
	"client_platform":        "pc_client",
	"device_platform":        "web",
	"doubao_device_platform": "desktop",
	"doubao_pc_version":      "2.32.6",
	"pc_version":             "2.32.6",
	"pkg_type":               "release_version",
	"language":               "zh",
	"region":                 "CN",
	"sys_region":             "CN",
	"runtime":                "web",
	"runtime_version":        "3.42.0",
	"samantha_web":           "1",
	"use-olympus-account":    "1",
	"version_code":           "20800",
	"web_platform":           "desktop",
	"chromium_version":       "148.0.7816.0",
}

// identityQuery 额度（quota/summary）用的身份参数（无 PC 形态四参数）。
var identityQuery = map[string]string{
	"aid":                 "582478",
	"real_aid":            "582478",
	"client_platform":     "pc_client",
	"device_platform":     "web",
	"language":            "zh",
	"region":              "CN",
	"sys_region":          "CN",
	"pkg_type":            "release_version",
	"version_code":        "20800",
	"pc_version":          "2.32.6",
	"runtime":             "web",
	"runtime_version":     "3.42.0",
	"samantha_web":        "1",
	"use-olympus-account": "1",
	"chromium_version":    "148.0.7816.0",
}

// modelSpec 一个模型的上游参数。
type modelSpec struct {
	Display string
	// ItemKey model_item_key（/chat/completion 选模型用）。
	ItemKey string
	// ContextWindow 上下文窗口（0 = 未知，面板显示「—」不编造，R49）。
	ContextWindow int
	// MemoryProfile / TotalWindow 上游 memory_profile 与 total_window_size。
	MemoryProfile string
	TotalWindow   string
}

// staticModels 模型表：公开 ID → 上游参数。
//
// 来源：yyydsz/doubao2api config.example.json（2026-11-15），与豆包桌面客户端模型列表逐一致。
// 上下文/倍率是该实现记载的值；上游未在本仓逐模型实测 capacity，故 ContextWindow 仅作展示参考。
var staticModels = map[string]modelSpec{
	"doubao-2.1-lite":    {Display: "豆包 2.1 Lite", ItemKey: "seed-lite-7b", ContextWindow: 256000, MemoryProfile: "256k", TotalWindow: "256000"},
	"doubao-2.1-pro":     {Display: "豆包 2.1 Pro", ItemKey: "5", ContextWindow: 256000, MemoryProfile: "256k", TotalWindow: "256000"},
	"deepseek-v4-flash":  {Display: "DeepSeek V4.1 Flash", ItemKey: "person-deepseek-v4-1-flash-261017", ContextWindow: 300000, MemoryProfile: "300k_230", TotalWindow: "300000"},
	"deepseek-v4-pro":    {Display: "DeepSeek V4 Pro", ItemKey: "person-deepseek-v4-pro-ga-260813", ContextWindow: 300000, MemoryProfile: "300k_230", TotalWindow: "300000"},
	"kimi-k3":            {Display: "Kimi K3", ItemKey: "person-kimi-k3-261111", ContextWindow: 300000, MemoryProfile: "300k_230", TotalWindow: "300000"},
	"kimi-k3.1-pro":      {Display: "Kimi K3.1 Pro", ItemKey: "person-kimi-k3-1-pro-261111", ContextWindow: 300000, MemoryProfile: "300k_230", TotalWindow: "300000"},
	"kimi-k3.1-pro-fast": {Display: "Kimi K3.1 Pro 高速", ItemKey: "person-kimi-k3-1-pro-highspeed-261111", ContextWindow: 300000, MemoryProfile: "300k_230", TotalWindow: "300000"},
	"glm-5.3":            {Display: "GLM 5.3", ItemKey: "person-glm-5-3-261111", ContextWindow: 300000, MemoryProfile: "300k_230", TotalWindow: "300000"},
	"glm-5.3-flash":      {Display: "GLM 5.3 Flash", ItemKey: "person-glm-5-3-flash-261111", ContextWindow: 300000, MemoryProfile: "300k_230", TotalWindow: "300000"},
}

// DefaultModel 未指定模型时的兜底。
const DefaultModel = "doubao-2.1-lite"
