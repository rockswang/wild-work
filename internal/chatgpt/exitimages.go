// exitimages.go 把本轮产物指针翻译成调用方可用的图片链接（票 07.5）。
//
// 为什么必须有这一层：上游把聊天里生成的图放在 role:"tool" + multimodal_text 的
// asset_pointer 里（sse.go 的 turnState.Images 已收下），而 OpenAI chat.completion
// 形状没有独立图字段 —— 不翻译就等于把图静默丢掉（真机：content 长度 0、finish=stop）。
//
// 口径（票面四条）：
//  1. 每张图各走一遍既有两步换址（downloadProducts 同源：下载接口 → 签名直链 → 字节），
//     签名短命 ⇒ 不缓存、不复用；
//  2. 单张失败只跳过该图并留痕，**不**让整轮失败（降级，不是击穿）；
//  3. 顺序：先文本后图片，多张按上游出现顺序；
//  4. `sediment://` 指针永不出本包 —— 对外的只可能是签名直链或 `data:` b64。
//
// 反向护栏：无图轮的输出必须与改动前**逐字节一致**（不给普通文本轮加空行/占位），
// 所以本文件的每个入口在「没有图」时都直接返回空串。
package chatgpt

import (
	"encoding/base64"
	"io"
	"log"
	"strings"

	"wild-work/internal/provider"
)

const (
	// exitImageLabel 是 markdown 图片的 alt 文本（固定，便于客户端/用户识别）。
	exitImageLabel = "图"
	// exitImageSeparator 是正文与图片块之间的分隔（流式与非流式共用，保证拼接一致）。
	exitImageSeparator = "\n\n"
	// exitImageErrLogMax 限制错误日志长度（上游错误体可能很长，且含签名直链）。
	exitImageErrLogMax = 160
)

// exitImageResolver 由「带凭据的会话读端」实现（convCapture）。
//
// 用接口断言而不是把凭据参数一路传下去：`provider.Upstream` 的 Stream/Aggregate
// 签名是全仓 20+ 渠道共享的契约，为一条渠道的图片翻译去改契约会波及所有渠道
// （与 recordTurnOf 同一取舍）。
type exitImageResolver interface {
	// imageAccountLabel 是日志里的账号标识（UID 优先，绝不输出 token）。
	imageAccountLabel() string
	// resolveExitImage 把单张产物换成真地址 + 字节（两步下载，每张各走一遍）。
	resolveExitImage(ref imageRef, convID string) (provider.ImageResult, error)
}

// exitImageBlocks 解析本轮产物并生成 markdown 图片块（空 = 无图或全部换址失败）。
//
// 无图 / 读端不是带凭据的会话（例如单测直接喂 strings.Reader）⇒ 返回空，
// **不编造**任何链接。
func exitImageBlocks(r io.Reader, st *turnState, model string) []string {
	if st == nil || len(st.Images) == 0 {
		return nil
	}
	res, ok := r.(exitImageResolver)
	if !ok {
		return nil
	}
	blocks := make([]string, 0, len(st.Images))
	for _, ref := range st.Images {
		got, err := res.resolveExitImage(ref, st.ConvID)
		if err != nil {
			log.Printf("chatgpt exit image skipped: uid=%s model=%s file=%s err=%s",
				res.imageAccountLabel(), model, ref.FileID, truncate(err.Error(), exitImageErrLogMax))
			continue
		}
		link := exitImageURL(got, ref)
		if link == "" {
			log.Printf("chatgpt exit image skipped: uid=%s model=%s file=%s err=no usable url or bytes",
				res.imageAccountLabel(), model, ref.FileID)
			continue
		}
		blocks = append(blocks, "!["+exitImageLabel+"]("+link+")")
	}
	if len(blocks) == 0 {
		return nil
	}
	// 观测点（真机核对用）：一行即可，零凭据。
	log.Printf("chatgpt exit images: uid=%s model=%s conv=%s count=%d",
		res.imageAccountLabel(), model, st.ConvID, len(blocks))
	return blocks
}

// exitImageURL 选出对外可用的图片地址：签名直链优先，其次 b64 data URI。
//
// 指针形态（sediment:// / file-service://）**绝不**外泄 —— 它只在包内存在。
func exitImageURL(got provider.ImageResult, ref imageRef) string {
	if u := strings.TrimSpace(got.URL); u != "" && isPublicImageURL(u) {
		return u
	}
	if got.B64JSON != "" {
		mime := strings.TrimSpace(ref.MimeType)
		if mime == "" {
			mime = "image/png"
		}
		return "data:" + mime + ";base64," + got.B64JSON
	}
	return ""
}

// isPublicImageURL 判定地址是否可对调用方公开（指针不是地址）。
func isPublicImageURL(u string) bool {
	low := strings.ToLower(u)
	if strings.HasPrefix(low, "sediment://") || strings.HasPrefix(low, "file-service://") {
		return false
	}
	return strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "data:")
}

// exitImageBlock 把图片块拼成一段 markdown（多张按上游顺序换行排列）。
func exitImageBlock(blocks []string) string { return strings.Join(blocks, "\n") }

// exitImageDelta 是流式路径的图片增量：分隔符必须与非流式拼接的形态一致
// （同一轮同一份正文，流式拼起来 == 非流式 content）。
func exitImageDelta(text, block string) string {
	if block == "" {
		return ""
	}
	if text == "" {
		return block
	}
	return exitImageSeparator + block
}

// exitImageContent 是非流式路径的等价拼接（无块时**原样返回**，零字节改动）。
func exitImageContent(text, block string) string {
	if block == "" {
		return text
	}
	return text + exitImageDelta(text, block)
}

// encodeImageB64 是给 convCapture 的共用编码（与 downloadProducts 同一口径）。
func encodeImageB64(body []byte) string { return base64.StdEncoding.EncodeToString(body) }

// ── convCapture 的 exitImageResolver 实现 ─────────────────────────────────

// imageAccountLabel 实现 exitImageResolver（日志标识：UID 优先，绝不出 token）。
func (cc *convCapture) imageAccountLabel() string {
	if cc == nil {
		return ""
	}
	return cc.account
}

// resolveExitImage 实现 exitImageResolver：复用既有两步换址（指针 → 下载接口 → 签名直链 → 字节）。
//
// 与 downloadProducts（生图契约）完全同源，**不缓存**签名直链（短命）。
func (cc *convCapture) resolveExitImage(ref imageRef, convID string) (provider.ImageResult, error) {
	if cc == nil || cc.cli == nil || cc.auth == nil {
		return provider.ImageResult{}, upstreamErr("缺少换址所需的账号上下文")
	}
	if convID == "" {
		convID = cc.convID
	}
	body, _, signed, err := cc.cli.DownloadImage(cc.auth, ref.FileID, convID)
	if err != nil {
		return provider.ImageResult{}, err
	}
	return provider.ImageResult{URL: signed, B64JSON: encodeImageB64(body)}, nil
}
