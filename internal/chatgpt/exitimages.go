// exitimages.go 把本轮产物指针翻译成调用方可用的图片（票 07.5 → 票 07.6 交付形态）。
//
// 为什么必须有这一层：上游把聊天里生成的图放在 role:"tool" + multimodal_text 的
// asset_pointer 里（sse.go 的 turnState.Images 已收下），而 OpenAI chat.completion
// 形状没有独立图字段 —— 不翻译就等于把图静默丢掉（真机：content 长度 0、finish=stop）。
//
// 交付形态（票 07.6 真机验收拍板）：**b64 data URI 优先**。
// 上游的签名直链是 `chatgpt.com/backend-api/estuary/content?id=…&sig=…`，需要 ChatGPT
// 会话 cookie 才能取（无凭据直取 403 File stream access denied）⇒ 对下游消费者等于没交付。
// 取字节失败才退回签名直链形态（ww3 网页端同款），两者都拿不到才跳过该图。
//
// 口径（票面四条）：
//  1. 每张图各走一遍既有两步换址（downloadProducts 同源：下载接口 → 签名直链 → 字节），
//     签名短命 ⇒ 不缓存、不复用；
//  2. 单张失败只跳过该图并留痕，**不**让整轮失败（降级，不是击穿）；
//  3. 顺序：先文本后图片，多张按上游出现顺序；
//  4. `sediment://` 指针永不出本包 —— 对外的只可能是 b64 `data:` 或签名直链。
//
// 反向护栏：无图轮的输出必须与改动前**逐字节一致**（不给普通文本轮加空行/占位），
// 所以本文件的每个入口在「没有图」时都直接返回空串。
package chatgpt

import (
	"encoding/base64"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

const (
	// exitImageLabel 是 markdown 图片的 alt 文本（固定，便于客户端/用户识别）。
	exitImageLabel = "图"
	// exitImageSeparator 是正文与图片块之间的分隔（流式与非流式共用，保证拼接一致）。
	exitImageSeparator = "\n\n"
	// exitImageErrLogMax 限制错误日志长度（上游错误体可能很长，且含签名直链）。
	exitImageErrLogMax = 160
	// exitImageDefaultMime 是既嗅不出、上游也没声明时的兜底 MIME。
	exitImageDefaultMime = "image/png"
)

// 出口交付形态（票 07.6 第 5 条：日志里要能看出这一轮是怎么交付的）。
const (
	exitShapeB64  = "b64"  // 字节可用 ⇒ data URI
	exitShapeURL  = "url"  // 字节不可用、直链可用 ⇒ 退回签名直链
	exitShapeSkip = "skip" // 两者都拿不到 ⇒ 跳过该图
)

// exitImagePayload 是一张产物交到出口时需要的全部信息。
type exitImagePayload struct {
	// B64 是已取回的字节（base64）；空 = 取字节失败，走直链回退。
	B64 string
	// Mime 是字节的 MIME（魔数优先，其次上游声明）。
	Mime string
	// URL 是签名直链（带字节时也一并保留，便于观测与回退）。
	URL string
}

// shape 返回这张图的交付形态（观测日志用）。
func (p exitImagePayload) shape() string {
	switch {
	case p.B64 != "":
		return exitShapeB64
	case strings.TrimSpace(p.URL) != "":
		return exitShapeURL
	default:
		return exitShapeSkip
	}
}

// exitImageResolver 由「带凭据的会话读端」实现（convCapture）。
//
// 用接口断言而不是把凭据参数一路传下去：`provider.Upstream` 的 Stream/Aggregate
// 签名是全仓 20+ 渠道共享的契约，为一条渠道的图片翻译去改契约会波及所有渠道
// （与 recordTurnOf 同一取舍）。
type exitImageResolver interface {
	// imageAccountLabel 是日志里的账号标识（UID 优先，绝不输出 token）。
	imageAccountLabel() string
	// resolveExitImage 把单张产物换成字节 + 直链（两步下载，每张各走一遍）。
	resolveExitImage(ref imageRef, convID string) (exitImagePayload, error)
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
	var nB64, nURL, nSkip int
	blocks := make([]string, 0, len(st.Images))
	for _, ref := range st.Images {
		got, err := res.resolveExitImage(ref, st.ConvID)
		link := ""
		if err == nil {
			link = exitImageURL(got)
		}
		if link == "" {
			nSkip++
			if err != nil {
				log.Printf("chatgpt exit image skipped: uid=%s model=%s file=%s err=%s",
					res.imageAccountLabel(), model, ref.FileID, truncate(err.Error(), exitImageErrLogMax))
			} else {
				log.Printf("chatgpt exit image skipped: uid=%s model=%s file=%s err=no usable url or bytes",
					res.imageAccountLabel(), model, ref.FileID)
			}
			continue
		}
		if got.shape() == exitShapeB64 {
			nB64++
		} else {
			nURL++
		}
		blocks = append(blocks, "!["+exitImageLabel+"]("+link+")")
	}
	if len(blocks) == 0 {
		return nil
	}
	// 观测点（真机核对用）：一行即可，零凭据；带交付形态以便分辨 b64 / 直链回退。
	log.Printf("chatgpt exit images: uid=%s model=%s conv=%s count=%d %s=%d %s=%d %s=%d",
		res.imageAccountLabel(), model, st.ConvID, len(blocks),
		exitShapeB64, nB64, exitShapeURL, nURL, exitShapeSkip, nSkip)
	return blocks
}

// exitImageURL 选出对外可用的图片地址：**b64 data URI 优先**（票 07.6），其次签名直链。
//
// 指针形态（sediment:// / file-service://）**绝不**外泄 —— 它只在包内存在。
func exitImageURL(p exitImagePayload) string {
	if p.B64 != "" {
		mime := strings.TrimSpace(p.Mime)
		if mime == "" {
			mime = exitImageDefaultMime
		}
		return "data:" + mime + ";base64," + p.B64
	}
	if u := strings.TrimSpace(p.URL); u != "" && isPublicImageURL(u) {
		return u
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

// exitImageSuffix 返回要追加到正文末尾的片段，**流式与非流式共用同一条规则**：
// 流式把它作为一条 delta 下发，非流式直接拼到 content ⇒ 两条出口拼出的正文逐字一致。
//
// 无块时返回空串（零字节改动：无图轮不得多出空行/占位）。
func exitImageSuffix(text, block string) string {
	if block == "" {
		return ""
	}
	if text == "" {
		return block
	}
	return exitImageSeparator + block
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

// exitImageMime 选出 data URI 的 MIME：字节魔数优先（与交付字节同源、唯一真源），
// 其次上游在 asset_pointer 里声明的 mime_type，最后兜 image/png。
func exitImageMime(body []byte, ref imageRef) string {
	if m := sniffImageMime(body); m != "" {
		return m
	}
	if m := strings.TrimSpace(ref.MimeType); m != "" {
		return m
	}
	return exitImageDefaultMime
}

// resolveExitImage 实现 exitImageResolver：复用既有两步换址（指针 → 下载接口 → 签名直链 → 字节）。
//
// 与 downloadProducts（生图契约）完全同源，**不缓存**签名直链（短命）。
// 字节取不到时退回直链形态（票 07.6 第 2 条），两者都拿不到才返回错误（调用方跳过该图）。
func (cc *convCapture) resolveExitImage(ref imageRef, convID string) (exitImagePayload, error) {
	if cc == nil || cc.cli == nil || cc.auth == nil {
		return exitImagePayload{}, upstreamErr("缺少换址所需的账号上下文")
	}
	if convID == "" {
		convID = cc.convID
	}
	body, _, signed, err := cc.cli.DownloadImage(cc.auth, ref.FileID, convID)
	if err == nil {
		return exitImagePayload{URL: signed, B64: encodeImageB64(body), Mime: exitImageMime(body, ref)}, nil
	}
	// 字节取不到：DownloadImage 的错误路径会把已经拿到的签名直链一并丢弃，故单独问一次
	// 下载接口（只取直链、不取字节），把「可用的 URL 形态」交出去。
	u, uerr := cc.signedImageURL(ref.FileID, convID)
	if uerr != nil {
		return exitImagePayload{}, err
	}
	return exitImagePayload{URL: u}, nil
}

// signedImageURL 只走两步换址的第一步：向下载接口要签名直链，不取字节（票 07.6 的 URL 回退用）。
//
// 为什么这段路径拼装与 upload.go 的 DownloadImage 重复：DownloadImage 在第二步失败时
// 把已拿到的直链连同错误一起丢弃，而票面范围红线禁止改它；故出口层是否保留这一份
// 最小拼装是刻意的取舍（路径的单一真源仍是 upload.go 的 pathFileDownload 常量）。
func (cc *convCapture) signedImageURL(fileID, convID string) (string, error) {
	p := pathFileDownload + url.PathEscape(fileID) +
		"?conversation_id=" + url.QueryEscape(convID) + "&download_intent=false&inline=true&include_library_file_state=true"
	st, m, raw, err := cc.cli.DoJSON(cc.auth, "GET", p, nil, nil)
	if err != nil {
		return "", err
	}
	if st != http.StatusOK {
		return "", StatusError("GET", pathFileDownload, st, raw)
	}
	u := strings.TrimSpace(str(m["download_url"]))
	if u == "" {
		return "", upstreamErr("下载接口未返回 download_url: " + truncate(string(raw), 400))
	}
	if !strings.HasPrefix(u, "http") {
		u = cc.cli.baseURL() + u
	}
	if !isPublicImageURL(u) {
		return "", upstreamErr("下载接口返回的地址不可公开: " + truncate(u, exitImageErrLogMax))
	}
	return u, nil
}
