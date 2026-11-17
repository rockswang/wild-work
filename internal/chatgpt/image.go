// image.go 生图 / 改图：provider.ImageGenerator + provider.InlineImageRefProvider。
//
// 与 glm / modelscope 的关系（为什么这一条不需要拼版也不上图床）：
//
//	glm      ：单参考图硬约束 ⇒ 多图由 handler 拼版 + 图床托管成一条公网 URL（WAF 绕行）
//	modelscope：上游吃多张 image_url 直链 / data URI ⇒ 内联直传
//	chatgpt  ：参考图先逐张上传换 file_id，再在消息体里放 N 个 image_asset_pointer
//	           ⇒ **上游原生多图理解**，既不用拼版（拼版会改变语义）也不用图床。
//
// 出站对齐上游真实网页（同源 ww3 internal/channel/imagesgen.go）：
//  1. 预检（零配额）先跑：上传会消耗 file_upload 配额且带设备指纹，挑战未过就别碰它；
//  2. 逐张上传参考图（四步，见 upload.go）——**每轮都重挂**：上游不保留跨轮附件上下文；
//  3. 每张产物都要各走一遍「指针 → 下载接口 → 签名直链 → 字节」（两步，签名短命、不得缓存）；
//  4. 对外只出现 URL / b64 —— `sediment://` 指针**永不出本包**。
//
// 尺寸 / 比例：上游不收 size 参数，只靠提示词描述。本渠道**接受但忽略**（不 400、
// 也不在响应里暗示已生效）——「假装生效」比拒绝更糟。
package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 编译期断言：本渠道会画图，且参考图可直传（handler 据此绕开拼版/图床路径）。
var (
	_ provider.ImageGenerator         = (*Client)(nil)
	_ provider.InlineImageRefProvider = (*Client)(nil)
)

const (
	// maxImages 是单次期望张数上限：上游一次只出 1~2 张，n 靠**多轮请求**凑。
	// 超过就在渠道层直接拒（不发无效请求）。
	maxImages = 4
	// maxRefTotalBytes 是参考图总字节上限（与 handler 的内联上限同口径）。
	// 超出必须 400 明确拒绝：本渠道不拼版、不压缩，悄悄丢图或转码都是伪造。
	maxRefTotalBytes = 20 << 20
	// refFetchTimeout 是 http(s) 参考图的下载超时（参考图可能在任何站点）。
	refFetchTimeout = 30 * time.Second
)

// InlineImageRefs 实现 provider.InlineImageRefProvider：改图参考图**多张内联直传**
// （data URI 或 http(s) 直链原样下发），不拼版、不上图床。
func (c *Client) InlineImageRefs() bool { return true }

// GenerateImage 实现 provider.ImageGenerator：文生图。
func (c *Client) GenerateImage(a *auth.Auth, req provider.ImageRequest) ([]provider.ImageResult, error) {
	return c.runImages(a, req, nil)
}

// EditImage 实现 provider.ImageGenerator：参考图改图（多张参考图原生直传）。
func (c *Client) EditImage(a *auth.Auth, req provider.ImageRequest) ([]provider.ImageResult, error) {
	refs, err := normalizeRefs(req.References)
	if err != nil {
		return nil, err
	}
	if len(refs) == 0 {
		return nil, badParams("改图缺少参考图引用")
	}
	return c.runImages(a, req, refs)
}

// refInput 是归一后的参考图（已解码成字节；上传必须拿字节，上游只吃上传后的 file_id）。
type refInput struct {
	data []byte
	name string
	mime string
}

// normalizeRefs 把客户端引用归一成字节：data URI / 裸 base64 本地解码，http(s) 现下载。
//
// 单张 http(s) 直链**原样透传**给本渠道（由本渠道下载），handler 不预下载、不拼版。
func normalizeRefs(raws []string) ([]refInput, error) {
	out := make([]refInput, 0, len(raws))
	total := 0
	for i, raw := range raws {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		r, err := normalizeRef(i, s)
		if err != nil {
			return nil, err
		}
		total += len(r.data)
		if total > maxRefTotalBytes {
			return nil, badParams("参考图总字节 %d 超过上限 %d 字节（本渠道按多图直传，不做拼版压缩）", total, maxRefTotalBytes)
		}
		out = append(out, r)
	}
	return out, nil
}

func normalizeRef(idx int, s string) (refInput, error) {
	switch {
	case strings.HasPrefix(s, "data:"):
		_, payload, ok := strings.Cut(s, ",")
		if !ok {
			return refInput{}, badParams("参考图 %d data URI 非法（缺少逗号分隔）", idx+1)
		}
		mime := ""
		if head, _, ok := strings.Cut(strings.TrimPrefix(s, "data:"), ";"); ok {
			mime = strings.TrimSpace(head)
		}
		b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
		if err != nil || len(b) == 0 {
			return refInput{}, badParams("参考图 %d data URI 解码失败", idx+1)
		}
		return refInput{data: b, name: "ref" + strconv.Itoa(idx+1) + extByMime(mime), mime: mime}, nil
	case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
		b, name, err := fetchRefURL(s)
		if err != nil {
			return refInput{}, badParams("参考图 %d 下载失败: %v", idx+1, err)
		}
		return refInput{data: b, name: name}, nil
	}
	// 裸 base64
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) > 0 {
		return refInput{data: b, name: "ref" + strconv.Itoa(idx+1) + extByMime(sniffImageMime(b))}, nil
	}
	return refInput{}, badParams("参考图 %d 既不是 data URI / http(s) 直链，也不是合法 base64", idx+1)
}

// fetchRefURL 下载 http(s) 参考图（裸 HTTP：参考图可能在任何站点，与上游指纹无关）。
func fetchRefURL(u string) ([]byte, string, error) {
	cli := &http.Client{Timeout: refFetchTimeout}
	resp, err := cli.Get(u)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxRefTotalBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(b) > maxRefTotalBytes {
		return nil, "", fmt.Errorf("超过单张参考图上限 %d 字节", maxRefTotalBytes)
	}
	name := path.Base(u)
	if pu, perr := url.Parse(u); perr == nil && path.Base(pu.Path) != "" && path.Base(pu.Path) != "." && path.Base(pu.Path) != "/" {
		name = path.Base(pu.Path)
	}
	if name == "" || name == "/" {
		name = "ref" + extByMime(sniffImageMime(b))
	}
	return b, name, nil
}

// capturedImage 是一张产物的来源信息（产物所属会话下载接口要用）。
type capturedImage struct {
	ref    imageRef
	convID string
}

// runImages 是生成/改图的共用编排：预检 → 上传参考图 → n 轮对话 → 两步换真地址。
func (c *Client) runImages(a *auth.Auth, req provider.ImageRequest, refs []refInput) ([]provider.ImageResult, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return nil, badParams("prompt 不能为空")
	}
	n := req.N
	if n <= 0 {
		n = 1
	}
	if n > maxImages {
		// 张数超上游上限：渠道层直接拒，不发无效请求（handler 对外仍是 400）。
		return nil, badParams("n=%d 超过本渠道单次上限 %d", n, maxImages)
	}
	// 尺寸/比例：接受但忽略（上游不收 size）——不报错、也不假装生效。
	_ = req.Size

	// 预检先跑：上传是带设备指纹的高敏感调用（且消耗 file_upload 配额），
	// 挑战未过就先别碰它；拿到的 token 直接给第一轮用（一次性，别浪费）。
	sen, err := c.EnsureFresh(a)
	if err != nil {
		return nil, err
	}
	atts, err := c.uploadRefs(a, refs)
	if err != nil {
		return nil, err
	}

	convID, parentID := "", ""
	var got []capturedImage
	for i := 0; i < n; i++ {
		if i > 0 {
			// sentinel token 一次性：每轮都要一张新的（缓存里没有就跑完整链路）。
			if sen, err = c.EnsureFresh(a); err != nil {
				return nil, err
			}
		}
		st, err := c.imageTurn(a, prompt, atts, convID, parentID, sen)
		if err != nil {
			return nil, err
		}
		if len(st.Images) == 0 {
			return nil, noImageError(st.Text)
		}
		turnConv := convID
		if st.ConvID != "" {
			turnConv = st.ConvID
		}
		for _, im := range st.Images {
			got = append(got, capturedImage{ref: im, convID: turnConv})
		}
		// 续接：下一轮接在本轮的 assistant 消息后面（附件每轮重挂，会话连续）。
		if st.ConvID != "" && st.MsgID != "" {
			convID, parentID = st.ConvID, st.MsgID
		} else {
			// 拿不到 assistant 消息 id 就没法续接：下一轮从新会话开始（不编 parent）。
			convID, parentID = "", ""
		}
	}

	out := make([]provider.ImageResult, 0, len(got))
	for _, g := range got {
		body, _, signed, err := c.DownloadImage(a, g.ref.FileID, g.convID)
		if err != nil {
			return nil, err
		}
		// 对外只出现签名直链 / b64：`sediment://` 指针永不出本包
		// （上游没给签名直链就只给 b64，绝不用指针顶替 URL）。
		out = append(out, provider.ImageResult{
			URL:     signed,
			B64JSON: base64.StdEncoding.EncodeToString(body),
		})
	}
	return out, nil
}

// uploadRefs 逐张上传参考图（每张独立 file_id ⇒ 上游原生多图）。
func (c *Client) uploadRefs(a *auth.Auth, refs []refInput) ([]attachment, error) {
	atts := make([]attachment, 0, len(refs))
	for i, r := range refs {
		up, err := c.UploadImage(a, r.data, r.name, r.mime)
		if err != nil {
			return nil, err
		}
		if up == nil {
			return nil, upstreamErr("参考图 " + strconv.Itoa(i+1) + " 上传未返回 file_id")
		}
		atts = append(atts, *up)
	}
	return atts, nil
}

// imageTurn 发一轮「带参考图」的对话，返回解析出的本轮状态（含产物指针）。
//
// 两段式（conduit + conversation）与 chat 完全同形，只多附件；不写多轮前缀链——
// 生图的续接是**请求内**的（n 轮共用一个会话），不该污染对话的多轮记忆。
func (c *Client) imageTurn(a *auth.Auth, prompt string, atts []attachment, convID, parentID string, sen *Sentinel) (*turnState, error) {
	req := &chatRequest{Prompt: prompt, Model: defaultChatModel}
	traceID := newUUID()

	extra := map[string]string{
		"content-type":                            "application/json",
		"x-oai-turn-trace-id":                     traceID,
		"openai-sentinel-chat-requirements-token": sen.ReqToken,
	}
	status, m, raw, err := c.DoJSON(a, "POST", PathFConversationPrepare, conduitBodyFor(req, convID, parentID, atts), extra)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, StatusError("POST", PathFConversationPrepare, status, raw)
	}
	conduit := str(m["conduit_token"])
	if conduit == "" {
		return nil, precheckErr("conversation/prepare 未下发 conduit_token: %s", truncate(string(raw), 400))
	}

	payload, err := json.Marshal(conversationBodyFor(req, newUUID(), convID, parentID, atts))
	if err != nil {
		return nil, upstreamErr("生图对话请求体序列化失败: " + err.Error())
	}
	resp, err := c.OpenStream(a, PathFConversation, payload, conversationHeaders(traceID, conduit, sen))
	if err != nil {
		return nil, err // 传输层错误：不换号、不计账号错误
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return nil, StatusError("POST", PathFConversation, resp.StatusCode, b)
	}
	st, _, err := consumeTurn(resp.Body, func(string, string) error { return nil })
	if err != nil {
		return nil, asProviderError(err)
	}
	return st, nil
}

// noImageError 把「上游没给图」的三种情形分开（否则调用方无法判断该重试、
// 该换号还是该换提示词）：
//
//	① 额度耗尽（plan limit / quota）  → ErrSoftRate（账号级，换号 + 冷却有意义）
//	② 内容策略拦截（content policies）→ ErrContentBlocked（请求级，换号必复现）
//	③ 模型自主决定不画               → ErrUpstream（上游任务级失败，不换号）
func noImageError(reply string) *provider.Error {
	txt := truncate(strings.TrimSpace(reply), 200)
	low := strings.ToLower(reply)
	switch {
	case strings.Contains(low, "plan limit"), strings.Contains(low, "reached your limit"),
		strings.Contains(low, "quota"), strings.Contains(low, "usage limit"):
		return &provider.Error{Kind: provider.ErrSoftRate, Status: http.StatusTooManyRequests,
			Msg: "上游生图额度已耗尽（等重置或换账号）：" + txt}
	case strings.Contains(low, "content policies"), strings.Contains(low, "violate"):
		return &provider.Error{Kind: provider.ErrContentBlocked, Status: http.StatusBadRequest,
			Msg: "上游内容策略拦截（换提示词重试）：" + txt}
	}
	return &provider.Error{Kind: provider.ErrUpstream, Status: http.StatusBadGateway,
		Msg: "上游未生成图片（模型回复：" + txt + "）"}
}

// imageContent 把附件 + 文本拼成 multimodal_text 内容块。
//
// 形状固定（同源于上游抓包）：N 个 image_asset_pointer 在前，文本串在最后；
// asset_pointer 用 `sediment://`（不是 file-service://，后者已不被接受）。
func imageContent(atts []attachment, prompt string) map[string]any {
	parts := make([]any, 0, len(atts)+1)
	for _, a := range atts {
		parts = append(parts, map[string]any{
			"content_type":  "image_asset_pointer",
			"asset_pointer": "sediment://" + a.FileID,
			"width":         a.Width,
			"height":        a.Height,
			"size_bytes":    a.SizeBytes,
		})
	}
	parts = append(parts, prompt)
	return map[string]any{"content_type": "multimodal_text", "parts": parts}
}
