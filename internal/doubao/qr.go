// qr.go 豆包扫码登录的协议常量与凭据校验。
//
// 流程（照搬参考实现，纯 HTTP、**不需要浏览器也不需要桌面客户端**）：
//
//	① GET accounts.doubao.com/passport/web/get_qrcode/  → token + 二维码图（base64）
//	② 轮询 GET accounts.doubao.com/passport/web/check_qrconnect/?token=…
//	   状态 new → scanning → confirmed / expired
//	③ confirmed 时该响应的 Set-Cookie 就是完整登录态（sessionid 等）
//
// 与参考实现的差异（本仓补强）：拿到 Cookie 后**必须调一次额度接口校验**才算登录完成
// （R128 同款纪律：捕获 Cookie ≠ 登录完成，判据只能是校验通过）。
package doubao

// QR 端点（passport 域，与主站同属 .doubao.com）。
//
// 做成 var 而非 const：测试要把它们指到 httptest，避免扫码流程打到真上游
// （§0.1：绝不能用真上游做实验，哪怕是登录探测）。
var (
	// QRGetURL 申请二维码。
	QRGetURL = "https://accounts.doubao.com/passport/web/get_qrcode/"
	// QRCheckURL 轮询扫码状态。
	QRCheckURL = "https://accounts.doubao.com/passport/web/check_qrconnect/"
)

// qrQuery 扫码接口的身份参数（与参考实现的 qr.query 一致）。
//
// 注意 aid 这里用 582478（桌面客户端身份），与对话接口同一套。
// QRQuery 扫码接口的身份参数（导出供登录包使用）。
func QRQuery() map[string]string {
	return map[string]string{
		"aid":                    "582478",
		"language":               "zh",
		"next":                   BaseURL + "/",
		"is_from_ttaccountsdk":   "1",
		"passport_jssdk_version": "4.4.6",
		"passport_jssdk_type":    "normal",
		"sdk_version":            "2.2.11-doubao.0",
		"pkg_type":               "release_version",
		"version_code":           "20800",
		"device_platform":        "desktop",
	}
}

// VerifyCookie 校验一份 Cookie 是否真的是可用登录态。
//
// 判据用**额度接口**：它需要登录态、无副作用（不发对话、不触发发放），
// 比「Cookie 里有没有 sessionid」强——后者在部分流程下会拿到预登录凭据。
func VerifyCookie(cookie string) error {
	if SessionIDOf(cookie) == "" {
		return &CookieError{Msg: "Cookie 里没有 sessionid"}
	}
	c := New()
	dev, webID, fp := DeviceParamsOf(cookie, "", "", "")
	if _, err := c.UserResource(authWithCookie(cookie, dev, webID, fp)); err != nil {
		return &CookieError{Msg: "Cookie 校验未通过（额度接口）: " + err.Error()}
	}
	return nil
}

// CookieError Cookie 不可用（面板可直接展示给用户）。
type CookieError struct{ Msg string }

func (e *CookieError) Error() string { return e.Msg }
