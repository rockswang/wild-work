package login_loomy

// accountBase 讯飞账号服务基址。
//
// ⚠️ 它是**变量**而不是常量，唯一原因：回归测试要把它指向本机 httptest 假上游
// （AGENTS.md §0.1 严禁对真实上游做批量/穷举探测）。生产代码不得修改它，
// 除测试外没有任何赋值点。
var accountBase = "https://account.xfinfr.com"

// 讯飞 CAccount（账号服务）协议常量。
//
// 全部取自参考实现 `dsh-our-free-model/vendor/channel-pack/src/loomy-product.ts:118-129`
// 与 `loomy-oauth.ts:10-21`；它们在 ofm 里又被注明「取自 Loomy 客户端资源里的
// `.env.prod`（VITE_XFYUN_ACCESS_KEY_ID / _SECRET / _APP_ID）」，即官方随客户端分发的公开值。
//
// ⚠️ AccessKey 的定位（ofm loomy-product.ts:19-23 原话）：它**只**用于
// 讯飞账号端点（account.xfinfr.com）的 HMAC-SHA1 签名（短信验证码登录）。
// 业务与推理端点用的是用户登录后的 session，与 AccessKey 无关；
// 故它不是本工具新增的秘密，也没有新增泄露面。

const (
	// LoginURL 「浏览器登录」的引导入口。
	//
	// 它不是上游授权页（Loomy 没有 OAuth 授权页，见包注释），而是**本工具的**登录页：
	// 页面上完成「手机号 → 短信验证码 → 换 session」这三步，验证码由本工具用签名协议
	// 真实下发到用户手机。真正的上游登录页 https://loomy.xunfei.cn 只是网页版工作台，
	// 它的登录态不会交给本工具，故不能作为引导入口。
	//
	// 实际地址是 Start 时生成的本机回环地址；本常量只用于「逻辑入口」文案与测试锚点，
	// Start 会返回真实地址（前端拿的是返回值）。
	LoginURL = "https://loomy.xunfei.cn/"

	// apiDomain / apiHost 写进 auth 文件，供 internal/loomy 组装出站请求。
	// 与 internal/loomy/constants.go 的 GatewayBase + APIPrefix 同值（此处重复是为
	// 避免让登录包反向依赖渠道包，从而在 import_local.go 之外多一条依赖边）。
	apiHost   = "https://loomyad.xunfei.cn"
	apiDomain = "/api/v1"

	// 讯飞 appId（账号请求体 base.appid）。
	accessKeyAppID = "GM3LOOMY"
	// 讯飞 AccessKeyId / Secret（HMAC-SHA1 签名的 `account {ak}:{sig}`）。
	accessKeyID     = "2thryby66wxi53sk"
	accessKeySecret = "zsak6eadrbawz683wf5r3m2snrwj868r"

	// 账号端点路径（相对 accountBase）。
	epSendCode  = "/login/phone/sendMsgCode"
	epCheckCode = "/login/phone/checkCode"

	// smsCodeTTLSeconds 短信验证码有效期（秒）。
	smsCodeTTLSeconds = 300
	// sessionTTLSeconds 登录会话有效期（秒）= 14 天。
	//
	// 依据 ofm loomy-oauth.ts:32-39：`session` 是向服务端**声明** expire 得来的
	// （客户端 account-service.js:391 的 `expire: 14 * 24 * 3600`），
	// 响应里**不带**到期时间戳，故凭据的 expiresAt 只能由本地按此推算。
	sessionTTLSeconds = 1_209_600

	// accountModelID / accountVersion / accountDeviceID 账号请求体 base 段。
	accountModelID  = "Web"
	accountVersion  = "1.0.0"
	accountDeviceID = "web"
	// accountUA **硬编码 macOS**：客户端在 Windows 上发的也是这个值，照抄不要改
	// （ofm loomy-oauth.ts:65-66 原话）。
	accountUA = "Loomy|Desktop|Electron|macOS"

	// 引导页路由（本机 loopback）。
	guidePath       = "/"
	apiSendCodePath = "/api/loomy/send_code"
	apiLoginPath    = "/api/loomy/login"
	apiStatusPath   = "/api/loomy/status"
)
