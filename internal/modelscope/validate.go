package modelscope

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 粘贴登录入口（票 04）：面板把一枚 `ms-` Access Token 贴进来 → 校验 → 落盘。
//
// 为什么校验只调 `GET /v1/models`：它是本渠道唯一的**零费用**读端点（不产生 token 消耗、
// 不产生额度消耗），且 401/403 是上游对「token 无效 / 区域不匹配 / 未实名」的统一拒绝形态。
// **已知限制（未实证）**：该目录端点对未鉴权请求同样回 200，若上游对「无效 token」也回 200，
// 粘贴阶段就无法证明 token 可用 —— 那种情况要等首次对话才暴露（表现为 SessionDead 徽标 +
// 卡片上的 ⟳ 重贴入口），面板文案因此必须把「区域须与 token 一致」这类自查项写清楚。

// tokenPrefix 官方 `ms-` Access Token 前缀（区分大小写不敏感）。
const tokenPrefix = "ms-"

// NormalizeToken 规整面板粘贴的 token：去空白/引号/Bearer 前缀。
//
// 用户常把 `Authorization: Bearer ms-xxx` 整行贴进来（从 curl 或文档里复制），
// 规整不掉就是「token 无效」——这类失败用户完全无从自查。
func NormalizeToken(s string) string {
	s = strings.TrimSpace(s)
	// 整段请求头形态：`Authorization: Bearer ms-…`
	if i := strings.Index(s, ":"); i > 0 && strings.EqualFold(strings.TrimSpace(s[:i]), "authorization") {
		s = strings.TrimSpace(s[i+1:])
	}
	// `Bearer ms-…`（大小写不敏感）
	if len(s) >= 7 && strings.EqualFold(s[:7], "bearer ") {
		s = strings.TrimSpace(s[7:])
	}
	return strings.Trim(s, `"'`+"`")
}

// UIDFor 由 token 派生账号 uid：`mst-`（国内站）/ `msai-`（国际站）+ token 的 sha256 前 16 位。
//
// 为什么掺渠道前缀：两区是**独立服务**，同一枚 token 在两边各自成账号，uid 不得撞
// （撞了会让面板按 uid 定位账号时把两个渠道的操作串到一起，见 R141 的 comate/lingxi 教训）。
// 为什么不直接用 token 尾号：尾号可重复（同一批 token 常常是同一段随机串的不同前缀），
// 而 sha256 前缀在同一 token 池里稳定且零碰撞风险；同时 uid 不携带 token 明文（零 token 落盘）。
func UIDFor(kind provider.Kind, token string) string {
	sum := sha256.Sum256([]byte(token))
	prefix := "mst-"
	if kind == provider.ModelScopeAI {
		prefix = "msai-"
	}
	return prefix + hex.EncodeToString(sum[:])[:16]
}

// DefaultNickname 默认显示名：渠道显示名 + token 尾 4 位（用户可在面板上改名）。
//
// 只取尾 4 位：足够区分同一批 token，又不至于把凭据当昵称铺在面板上。
func DefaultNickname(kind provider.Kind, token string) string {
	tail := token
	if len(tail) > 4 {
		tail = tail[len(tail)-4:]
	}
	if kind == provider.ModelScopeAI {
		return "ModelScopeAI " + tail
	}
	return "ModelScope " + tail
}

// regionLabel 渠道区域（错误文案用）：两区 token 不通用，报错必须点明是哪一区。
func regionLabel(kind provider.Kind) string {
	if kind == provider.ModelScopeAI {
		return "国际站 " + DefaultBaseAI
	}
	return "国内站 " + DefaultBaseCN
}

// ValidateToken 校验面板粘贴的 Access Token 并返回账号 uid（app.SubmitModelScopeToken 的入口）。
func ValidateToken(kind provider.Kind, token string) (string, error) {
	return New(kind).ValidateToken(token)
}

// ValidateToken 是可注入 Base 的版本（测试用 httptest 假上游替换 Base）。
func (c *Client) ValidateToken(token string) (string, error) {
	tok := NormalizeToken(token)
	if tok == "" {
		return "", errors.New("Access Token 不能为空")
	}
	if strings.ContainsAny(tok, " \t\r\n") {
		return "", fmt.Errorf("Access Token 里含空白字符（%d 字符），"+
			"请只粘贴 token 本身（形如 ms-xxxxxxxx）；从表格/文档里复制常会带上多余列", len(tok))
	}
	if !strings.HasPrefix(strings.ToLower(tok), tokenPrefix) {
		return "", fmt.Errorf("这看起来不是魔搭 Access Token（应以 %s 开头）："+
			"请到 https://modelscope.cn/my/myaccesstoken 生成 Access Token 后重新粘贴", tokenPrefix)
	}

	ctx, cancel := context.WithTimeout(context.Background(), modelsTimeout)
	defer cancel()
	req, err := c.newRequest(ctx, http.MethodGet, modelsPath, &auth.Auth{AccessToken: tok}, nil)
	if err != nil {
		return "", fmt.Errorf("构造校验请求失败：%w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("无法连接%s（网络/代理问题）：%w", regionLabel(c.Kind), err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, modelsBodyCap))

	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return "", tokenDeniedError(c.Kind, resp.StatusCode, raw)
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("校验 Access Token 失败（HTTP %d，%s）：%s",
			resp.StatusCode, regionLabel(c.Kind), truncateBody(raw, 200))
	}
	// 上游偶发用 HTTP 200 + 错误信封表达鉴权失败（各家网关写法不一），故再验一层。
	if msg, denied := authDeniedInBody(raw); denied {
		return "", tokenDeniedError(c.Kind, 200, []byte(msg))
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("上游返回的不是 JSON（%s）：可能被代理/网关拦截，请检查网络后重试", regionLabel(c.Kind))
	}
	return UIDFor(c.Kind, tok), nil
}

// tokenDeniedError 401/403 的统一文案：必须**可行动** —— 三条自查项各自独立
// （区域不一致 / 未绑阿里云实名 / token 已失效），并附上游原文供排障。
func tokenDeniedError(kind provider.Kind, status int, raw []byte) error {
	return fmt.Errorf("Access Token 无效或与渠道区域不匹配（HTTP %d，本渠道 = %s）："+
		"① 两区 token **不通用**，国内站的 token 不能用于 ModelScopeAI，反之亦然；"+
		"② token 必须属于**已绑定阿里云账号并完成实名认证**的魔搭账号；"+
		"③ 请到 https://modelscope.cn/my/myaccesstoken 重新生成后粘贴。"+
		"上游原文：%s", status, regionLabel(kind), truncateBody(raw, 200))
}

// authEnvelope 只取「可能是鉴权拒绝」的两个位置：顶层 code 与 errors.code。
type authEnvelope struct {
	Code   json.RawMessage `json:"code"`
	Errors struct {
		Code json.RawMessage `json:"code"`
	} `json:"errors"`
}

// authDeniedInBody 报告 200 响应体是否是「鉴权拒绝」信封，返回人话摘要。
//
// 判据收窄到数值 401/403：`/v1/models` 的合法响应体里没有 code 字段，
// 过宽的判据（例如把所有带 code 的 200 都当失败）会把正常目录判成失败。
func authDeniedInBody(raw []byte) (string, bool) {
	var env authEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", false
	}
	for _, c := range []json.RawMessage{env.Code, env.Errors.Code} {
		s := strings.Trim(strings.TrimSpace(string(c)), `"`)
		if s == "401" || s == "403" {
			return truncateBody(raw, 200), true
		}
	}
	return "", false
}

// truncateBody 截断上游原文（错误文案里带原文，但不得放进整段响应体）。
//
// 按 **rune** 截断而不是按字节：上游错误体常带中文，按字节切会把最后一个字符
// 拦腰截断成乱码（错误文案是给用户看的，乱码比截断更糟）。
func truncateBody(raw []byte, n int) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "(空)"
	}
	if rs := []rune(s); len(rs) > n {
		return string(rs[:n]) + "…"
	}
	return s
}
