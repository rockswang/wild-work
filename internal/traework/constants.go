// Package traework 封装 Trae SOLO 免费通道上游协议。
package traework

const (
	AgentHost      = "https://trae-api-cn.mchost.guru"
	UgHost         = "https://api.trae.cn"
	OAuthHost      = "https://api.trae.com.cn"
	ConsoleHost    = "https://www.trae.cn"
	ClientID       = "en1oxy7wnw8j9n"
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IdeVersion     = "0.1.52" // 对齐 connectedGraph/traework2api 上游，对应更新更全的模型配置表（含 glm-5.3）
	IdeVersionCode = "20260811"
	DeviceBrand    = "20Y5A002XX"   // 设备机型示例（指纹头用，可替换为真实机型，无需精确匹配）
	OSVersion      = "Windows 10 Pro" // 真实客户端系统版本
	PluginVersion  = "2.3.73734"     // 真实客户端插件版本（登录 URL 用）
	Function       = "solo_work_lite"

	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/web_user_ent_usage" // 网页版积分接口（带 require_usage 拿实际用量）
)

const DefaultConfigName = "glm-5.2"
