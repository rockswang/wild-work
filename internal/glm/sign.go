// sign.go 智谱清言 IDE(Electron) 客户端指纹与签名。
//
// 2026-10-20 从 WEB 指纹迁移到 IDE 指纹（协议依据 chatglm-cli/internal/glm，全部 [实证]）：
//   - WEB 通道受阿里云 WAF「头组合指纹」对抗，实测成功率低；
//   - IDE 主对话链路实测 100% 通过，**不需要 refer__991、不做退避重试**；
//   - WAF 校验的是「头组合指纹」，必须逐头照抄 IDE 客户端，禁止按直觉增删：
//     X-App-Platform=win / X-App-Version=2.0.6 / X-App-fr=default / X-Exp-Groups 必带 /
//     Electron UA（品牌段 `????` 是客户端自己写坏的，抓包原样保留，勿"修正"）/
//     无 Referer / sec-ch-ua 是 Chromium 150。
//
// 签名算法（[实证] PROTOCOL §2，6/6 复算命中）：
//
//	X-Sign = md5(X-Timestamp + "-" + X-Nonce + "-" + SIGN_SECRET)   小写 hex
//	X-Timestamp = 13 位毫秒原值（旧实现的「倒数第二位校验位」算法对签名结果无影响，已删）
//	X-Nonce = 32 位小写 hex
//	X-Device-Id 与 access_token JWT payload 的 device_id 一致（IDE 客户端行为）。
package glm

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Sign 一次签名所需的三个值。
type Sign struct {
	Timestamp string
	Nonce     string
	Sign      string
}

// generateSign 按上游算法生成签名。now 由调用方传入以便测试。
func generateSign(now time.Time) Sign {
	ts := fmt.Sprintf("%d", now.UnixMilli())
	nonce := randomHex(32)
	return Sign{
		Timestamp: ts,
		Nonce:     nonce,
		Sign:      md5Hex(ts + "-" + nonce + "-" + SIGN_SECRET),
	}
}

// md5Hex 返回小写 hex 的 MD5。
func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// randomHex 返回 n 位小写 hex 随机串（n 为偶数时恰好 n/2 字节）。
// 随机源失败时退回时间派生值，保证调用方永远拿得到非空串。
func randomHex(n int) string {
	buf := make([]byte, (n+1)/2)
	if _, err := rand.Read(buf); err != nil {
		fallback := md5.Sum([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		return hex.EncodeToString(fallback[:])[:n]
	}
	return hex.EncodeToString(buf)[:n]
}

// ── IDE 指纹常量（逐头照抄 chatglm-cli/internal/glm/constants.go，勿改）────

const (
	idePlatform = "win"   // IDE 是 win（WEB 是 pc）
	ideVersion  = "2.0.6" // IDE 客户端版本
	ideFR       = "default"
	// ideUA 的 Electron UA：中文品牌段被客户端自己写坏成 ????（抓包原样保留）。
	// {device_id} 占位符在发请求时替换；device_id 为空时整段 DeviceId/ 移除。
	ideUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) ????/2.0.6 Chrome/150.0.7871.250 Electron/43.6.0 " +
		"Safari/537.36 Platform/ChatGLM_Win; AppVersion/2.0.6;DeviceId/{device_id};RefreshId/1790641614338"
	ideSecChUA = `"Not;A=Brand";v="8", "Chromium";v="150"`
	// ideExpGroups 服务端不校验取值但必须存在（WAF 指纹）[实证 PROTOCOL §2]。
	ideExpGroups = "na_android_config:exp:NA,na_4o_config:exp:4o_A,tts_config:exp:tts_config_a," +
		"na_glm4plus_config:exp:open,mainchat_server_app:exp:A,mobile_history_daycheck:exp:a," +
		"desktop_toolbar:exp:A,chat_drawing_server:exp:A,drawing_server_cogview:exp:cogview4," +
		"app_welcome_v2:exp:A,chat_drawing_streamv2:exp:A,mainchat_rm_fc:exp:add," +
		"mainchat_dr:exp:open,chat_auto_entrance:exp:A,drawing_server_hi_dream:control:A," +
		"homepage_square:exp:close,assistant_recommend_prompt:exp:3,app_home_regular_user:exp:A," +
		"mainchat_moe:exp:300,assistant_greet_user:exp:greet_user," +
		"app_welcome_personalize:exp:A,assistant_model_exp_group:exp:glm4.5,ai_wallet:exp:ai_wallet_enable"
)

// ideHeaders 组装一次请求的完整头（**不含 Cookie**：Cookie 一律交给 cookie jar，
// 显式 Cookie 头会覆盖 jar，使服务端下发的 WAF cookie（acw_tc/cdn_sec_tc）无法带回
// → 持续 40012 [实证 chatglm-cli PROTOCOL §1.4]）。
// deviceID 为空时 UA 的 DeviceId/ 段与 X-Device-Id 头都不发（仍可用，但建议总是传）。
func ideHeaders(token, accept, deviceID string) map[string]string {
	s := generateSign(time.Now())
	ua := strings.Replace(ideUA, "{device_id}", deviceID, 1)
	if deviceID == "" {
		ua = strings.Replace(ua, "DeviceId/{device_id}", "DeviceId/", 1)
	}
	h := map[string]string{
		"Accept":          accept,
		"Accept-Encoding": "gzip, deflate", // Go stdlib 只能解 gzip/deflate；上游不压缩 SSE [实证]
		"Accept-Language": "zh-CN",
		"App-Name":        "chatglm",
		"Origin":          "https://chatglm.cn",
		"User-Agent":      ua,
		// 业务伪装头
		"X-App-Platform": idePlatform,
		"X-App-Version":  ideVersion,
		"X-App-fr":       ideFR,
		"X-Lang":         "zh",
		"X-Device-Model": "",
		"X-Device-Brand": "",
		"X-Exp-Groups":   ideExpGroups,
		// 浏览器客户端提示（IDE 是 Electron/Chromium 150）
		"sec-ch-ua":          ideSecChUA,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "same-origin",
		// 签名三件套
		"X-Timestamp": s.Timestamp,
		"X-Nonce":     s.Nonce,
		"X-Sign":      s.Sign,
	}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	if deviceID != "" {
		h["X-Device-Id"] = deviceID // 与 JWT.device_id 一致 [实证 PROTOCOL §1.2]
	}
	h["X-Request-Id"] = randomHex(32)
	return h
}

// chatModelOf 对话体的 selected_model 取值。
//
// 实测（2026-10-20 probe + chatglm-cli 全部线上请求）：selected_model 恒定
// "glm-5.3-flash" 即可，服务端不接受客户端选版本（三种 chat_mode 上报同一 moe_53f，
// R33）；模型名的 `:moe_53f` 后缀只是展示用途，不进 selected_model。
const chatModel = "glm-5.3-flash"

func chatModelOf(_ string) string { return chatModel }

// eventDate 返回清言签到接口要求的 event_date（UTC+8 当天，YYYY-MM-DD）。
// 对应官网 JS：
//
//	new Date(Date.now() - 6e4*new Date().getTimezoneOffset()).toJSON().slice(0,10)
//
// 该表达式把本地时间平移到 UTC，再取日期——对 UTC+8 机器即本地当天。
// 本工具全渠道统一用 UTC+8 墙钟（见 provider.expireLoc），故直接取 UTC+8 日期。
func eventDate(now time.Time) string {
	return now.In(utc8).Format("2006-01-02")
}
