// handler_images.go OpenAI Images 兼容端点：/v1/images/generations + /v1/images/edits。
//
// 接入模式参考本项目 /v1/systemone（OpenCode Zen）：专用端点 + 统一 API key 鉴权 +
// 渠道能力直达。与 systemone 的差异：glm 是多账号渠道，选号/惩罚走 Runtime 池
// （粘性选号 + 错误分类惩罚 + SessionDead 标记），但不做请求内换号轮转——生图是
// 同步长操作（4~22s），轮转代价高；惩罚已保证下一次请求自动换号（粘性记录被清）。
//
// 用户决议（2026-10-20）：
//   - 本端点**不支持换模型**（model 字段忽略），依赖且仅依赖 glm 渠道；
//   - 生图产物/会话**不自动删除**（与保活对话的「立即删会话」相反）。
//
// 协议要点（[实证] chatglm-cli docs/生图协议与实操.md）：
//   - 文生图：一次 4 图、12~15s、不扣积分；产物 CDN 直链会过期 ⇒ 默认下载转 b64_json；
//   - size→ratio 映射必须严格：上游对非法比例**静默降级 1:1**，故本层 400 拒绝；
//   - 参考图改图：单图硬约束 ⇒ 多图走「客户端拼版（imgutil.BuildRefSheet）+ 图床托管
//     （imgutil.UploadJPEG）」；单张 http(s) URL 直传（不下载不上传，链路最短）；
//   - 输出比例强制跟随输入图（aspect_ratio 无效）⇒ edits 的 size 参数忽略并在响应头提示。
package server

import (
	"encoding/base64"
	"errors"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/glm"
	"wild-work/internal/imgutil"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// ratioBySize OpenAI size → 清言 aspect_ratio 映射（尺寸取自 2026-10-15 实测全集）。
// 也接受直接传比例（"1:1" 等）。未知值必须 400，不得透传（上游会静默降级 1:1）。
var ratioBySize = map[string]string{
	"1024x1024": "1:1",
	"864x1152":  "3:4",
	"1152x864":  "4:3",
	"720x1280":  "9:16",
	"1280x720":  "16:9",
	"auto":      "1:1",
	"1:1":       "1:1",
	"3:4":       "3:4",
	"4:3":       "4:3",
	"9:16":      "9:16",
	"16:9":      "16:9",
}

// editPromptConstraint 拼版参考图的出站提示词约束（chatglm-cli 同款 [实证]）：
// 拼版排版是工具侧产物，模型不得复刻。
const editPromptConstraint = "根据参考图中的人或物，按照下面提示词绘制结果图，不要复刻参考图的排版样式。"

// glmRuntime 取 glm 渠道 Runtime；未接入返回 nil。
func (h *Handler) glmRuntime() *Runtime {
	return h.cfg.Runtimes[provider.GLM]
}

// imagesGenerations POST /v1/images/generations —— 文生图（走 glm 画图 agent）。
func (h *Handler) imagesGenerations(w http.ResponseWriter, r *http.Request) {
	body, err := ReadBodyLimited(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req struct {
		Model          string `json:"model"`
		Prompt         string `json:"prompt"`
		N              int    `json:"n"`
		Size           string `json:"size"`
		ResponseFormat string `json:"response_format"`
		OutputFormat   string `json:"output_format"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing required field: prompt")
		return
	}
	// size 归一化：空 = 1:1；未知值 400（上游对非法值静默降级 1:1，不报错，必须本层拦截）。
	ratio := req.Size
	if ratio == "" {
		ratio = "1:1"
	}
	mapped, ok := ratioBySize[ratio]
	if !ok {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("unsupported size %q: use 1024x1024/864x1152/1152x864/720x1280/1280x720（或 1:1/3:4/4:3/9:16/16:9）", req.Size))
		return
	}
	n := req.N
	if n <= 0 {
		n = 1
	}
	if n > 4 {
		n = 4 // 上游一次固定回 4 图
	}
	wantB64 := req.ResponseFormat != "url" // gpt-image-1 语义：默认 b64_json

	// 模型分流：如果以 gemini/ 开头或带有 imagen，路由至 Gemini 上游
	if strings.HasPrefix(strings.ToLower(req.Model), "gemini/") || strings.Contains(strings.ToLower(req.Model), "imagen") {
		imgsAny, err := h.runImageGenerator(w, r, provider.Gemini, "gemini/image", func(up provider.Upstream, acct *auth.Auth) (any, error) {
			gen, ok := up.(ImageGeneratorProvider)
			if !ok {
				return nil, fmt.Errorf("gemini 上游未实现 ImageGeneratorProvider: %T", up)
			}
			b64s, revised, err := gen.GenerateImage(acct, req.Prompt, mapped, n)
			if err != nil {
				return nil, err
			}
			return imageGenResult{B64List: b64s, Revised: revised}, nil
		})
		if err != nil {
			return
		}
		genResult := imgsAny.(imageGenResult)
		data := make([]map[string]any, 0, len(genResult.B64List))
		for i, b64 := range genResult.B64List {
			revised := req.Prompt
			if i < len(genResult.Revised) && genResult.Revised[i] != "" {
				revised = genResult.Revised[i]
			}
			item := map[string]any{
				"revised_prompt": revised,
			}
			if wantB64 {
				item["b64_json"] = b64
			} else {
				item["url"] = "data:image/png;base64," + b64
			}
			data = append(data, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
		return
	}

	imgsAny, err := h.runGLM(w, r, "glm/image", func(up provider.Upstream, acct *auth.Auth) (any, error) {
		client, ok := up.(*glm.Client)
		if !ok {
			return nil, fmt.Errorf("glm 上游实现类型异常: %T", up)
		}
		return client.GenerateImage(acct, req.Prompt, mapped, n)
	})
	if err != nil {
		return // 响应已写出
	}
	imgs := imgsAny.([]glm.GeneratedImage)
	data := make([]map[string]any, 0, len(imgs))
	for _, im := range imgs {
		item := map[string]any{"revised_prompt": im.RevisedPrompt}
		if wantB64 {
			b64, err := fetchImageB64(imageCDNURL(im.URL, req.OutputFormat))
			if err != nil {
				// 单图下载失败不拖垮整批：降级为 url 让客户端自行拉取
				log.Printf("images: 产物下载失败（降级为 url）url=%s err=%v", im.URL, err)
				item["url"] = im.URL
			} else {
				item["b64_json"] = b64
			}
		} else {
			item["url"] = im.URL
		}
		data = append(data, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

// imagesEdits POST /v1/images/edits —— 参考图改图（多参考图自动拼版 + 图床托管）。
//
// 双形态入参：
//   - multipart（OpenAI 官方 SDK 形状）：image 字段可重复（文件或 URL 文本）；
//   - JSON：{"prompt": "...", "image": ["https://... | data:image/...;base64,..." ...]}。
func (h *Handler) imagesEdits(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	var prompt string
	var raws [][]byte  // 每张参考图的原始字节（URL 形态在 resolve 时特判直传）
	var outFmt string  // output_format（png 时产物转真 PNG）

	if strings.HasPrefix(ct, "multipart/") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse multipart: "+err.Error())
			return
		}
		prompt = r.FormValue("prompt")
		// OpenAI 官方 SDK 用 image 字段；gpt-image 系适配器（如 whalechan）用 image[]。
		// 两者都收，文件在前（顺序即参考图顺序）。
		for _, key := range []string{"image", "image[]"} {
			for _, fh := range r.MultipartForm.File[key] {
				f, err := fh.Open()
				if err != nil {
					writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "open image part: "+err.Error())
					return
				}
				raw, err := io.ReadAll(io.LimitReader(f, 20<<20))
				f.Close()
				if err != nil {
					writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read image part: "+err.Error())
					return
				}
				raws = append(raws, raw)
			}
		}
		// output_format：OpenAI images API 参数（png/jpeg/webp）；whalechan 恒传 png。
		// 上游 CDN 原生产物是 jpg，png 用 image_process 真转换 [实证 生图协议 §2.2]。
		outFmt = r.FormValue("output_format")
	} else {
		body, err := ReadBodyLimited(r)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
			return
		}
		var req struct {
			Prompt       string   `json:"prompt"`
			Image        []string `json:"image"`
			OutputFormat string   `json:"output_format"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
			return
		}
		prompt = req.Prompt
		outFmt = req.OutputFormat
		for _, s := range req.Image {
			raws = append(raws, []byte(strings.TrimSpace(s)))
		}
	}
	if strings.TrimSpace(prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing required field: prompt")
		return
	}
	if len(raws) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing required field: image")
		return
	}

	// 分流：单张 http(s) URL 直传（上游只认公网 URL，链路最短、不过图床）；
	// 其余（多图 / base64 / dataURI）拼版 + 托管。
	refURL, err := resolveReferenceURL(raws)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 出站提示词：约束前缀（拼版排版不得复刻）+ 多图时给 referenceN 定位说明。
	outPrompt := editPromptConstraint + prompt
	if len(raws) > 1 {
		outPrompt = fmt.Sprintf("参考图中共有 %d 个以 reference1..%d 从左到右标注的单元，按序号对应下面提示词中的对象。", len(raws), len(raws)) + outPrompt
	}

	imgURL, err := h.runGLM(w, r, "glm/image", func(up provider.Upstream, acct *auth.Auth) (any, error) {
		client, ok := up.(*glm.Client)
		if !ok {
			return nil, fmt.Errorf("glm 上游实现类型异常: %T", up)
		}
		return client.EditImage(acct, refURL, outPrompt)
	})
	if err != nil {
		return // 响应已写出
	}
	url := imgURL.(string)
	item := map[string]any{"url": url}
	if b64, err := fetchImageB64(imageCDNURL(url, outFmt)); err == nil {
		item = map[string]any{"b64_json": b64}
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": []map[string]any{item}})
}

// resolveReferenceURL 把参考图输入归一成「上游可达的公网 URL」。
// 单张 http(s) URL：原样返回（不下载不上传）。多张/base64/dataURI：拼版 → 图床。
func resolveReferenceURL(raws [][]byte) (string, error) {
	if len(raws) == 1 {
		s := string(raws[0])
		if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
			return s, nil // 单 URL 直传（实测上游直接拉取）
		}
	}
	images := make([][]byte, 0, len(raws))
	for i, raw := range raws {
		s := string(raw)
		switch {
		case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
			b, err := fetchImageBytes(s)
			if err != nil {
				return "", fmt.Errorf("参考图 %d 下载失败: %w", i+1, err)
			}
			images = append(images, b)
		case strings.HasPrefix(s, "data:"): // data:image/...;base64,xxx
			if _, b64, ok := strings.Cut(s, ","); ok {
				b, err := base64.StdEncoding.DecodeString(b64)
				if err != nil {
					return "", fmt.Errorf("参考图 %d data URI 解码失败: %w", i+1, err)
				}
				images = append(images, b)
			}
		default: // 裸 base64 或二进制
			if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) > 0 {
				images = append(images, b)
			} else {
				images = append(images, raw) // 当作原始图片字节
			}
		}
	}
var sheet []byte
	var err error
	if len(images) == 1 {
		// 单图（base64/dataURI/webp 等形态）：统一转 JPEG 后托管——
		// 图床上传名是 .jpg（Content-Type image/jpeg），且上游参考图对 webp 支持未实证，
		// 统一转码避免名实不符。
		sheet, err = imgutil.EnsureJPEG(images[0])
		if err != nil {
			return "", fmt.Errorf("参考图转码失败: %w", err)
		}
	} else {
		if sheet, _, _, err = imgutil.BuildRefSheet(images); err != nil {
			return "", fmt.Errorf("参考图拼版失败: %w", err)
		}
	}
	url, _, err := imgutil.UploadJPEG(sheet)
	if err != nil {
		return "", fmt.Errorf("参考图托管失败（图床不可用）: %w", err)
	}
	return url, nil
}

// runGLM 选 glm 账号执行生图调用，统一错误分类与惩罚。
//
// 与 chat 的 serveRotation 不同：**不做请求内换号轮转**——生图是同步长操作
// （4~22s），轮转代价高；惩罚已保证下一次请求自动换号（粘性记录被清）。
// 无健康账号/上游错误时写出响应并返回 error（调用方直接收尾）。
//
// model 固定 "glm/image"：粘性路由 key 与模型级冷却都落在这个虚拟模型名上，
// 与对话模型（chatglm:moe_53f 等）互不干扰。
func (h *Handler) runGLM(w http.ResponseWriter, r *http.Request, model string, call func(provider.Upstream, *auth.Auth) (any, error)) (any, error) {
	rt := h.glmRuntime()
	if rt == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "channel_unavailable", "glm 渠道未接入（未绑定账号）")
		return nil, errors.New("no glm runtime")
	}
	acct := h.pickWithSticky(rt, model)
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			"glm 渠道无可用账号（全部冷却/登录态失效），请在面板检查账号状态")
		return nil, errors.New("no healthy glm account")
	}
	res, err := call(rt.Upstream, acct)
	if err == nil {
		rt.Pool.NoteSuccess(acct.UID)
		h.stickySuccess(rt)
		return res, nil
	}
	h.stickyClear(rt)
	var pe *provider.Error
	if errors.As(err, &pe) {
		switch pe.Kind {
		case provider.ErrSessionDead:
			// 登录态失效：标记（挑号跳过 + 面板徽标），401 自愈由下次请求的 refresh 链完成
			rt.Pool.MarkSessionDead(acct.UID, pe.Msg)
			log.Printf("images session dead platform=glm uid=%s err=%s", acct.UID, pe.Msg)
			writeOpenAIError(w, http.StatusUnauthorized, "session_dead", "glm 账号登录态失效，请重新登录："+pe.Msg)
		case provider.ErrSoftRate:
			// 限流/风控瞬时拒绝：模型级冷却（glm/image 名下），不冻结整个账号
			rt.Pool.CooldownModel(acct.UID, model, h.cfg.SoftCooldown, "生图限流: "+pe.Msg)
			log.Printf("images rate limited platform=glm uid=%s err=%s", acct.UID, pe.Msg)
			writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "glm 生图被限流，请稍后重试："+pe.Msg)
		case provider.ErrWafBlock, provider.ErrAccountFault:
			rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, pe.Kind.String())
			log.Printf("images waf/fault platform=glm uid=%s kind=%s err=%s", acct.UID, pe.Kind, pe.Msg)
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "glm 上游风控拦截："+pe.Msg)
		case provider.ErrContentBlocked, provider.ErrBadParams, provider.ErrPassthrough:
			// 请求内容问题：不罚号，透传原因
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", pe.Msg)
		default:
			log.Printf("images upstream error platform=glm uid=%s kind=%s err=%s", acct.UID, pe.Kind, pe.Msg)
			writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "glm 生图失败："+pe.Msg)
		}
		return nil, err
	}
	// 非类型化错误（网络层）：计错 + 502
	rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
	log.Printf("images transport error platform=glm uid=%s err=%v", acct.UID, err)
	writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "glm 生图网络错误："+err.Error())
	return nil, err
}

// imageCDNURL 按 output_format 调整产物 CDN 直链：sfile.chatglm.cn 产物支持
// ?image_process=format,png 真转换 [实证 生图协议 §2.2]；其他格式/非上游 CDN 原样返回。
func imageCDNURL(url, format string) string {
	if format == "png" && strings.Contains(url, "sfile.chatglm.cn/") && !strings.Contains(url, "?") {
		return url + "?image_process=format,png"
	}
	return url
}

// fetchImageBytes 下载图片（10s 超时，上限 20MB）。
func fetchImageBytes(url string) ([]byte, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 20<<20))
}

// fetchImageB64 下载图片并转 base64（产物 CDN 直链会过期，默认落 b64_json）。
func fetchImageB64(url string) (string, error) {
	b, err := fetchImageBytes(url)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

type ImageGeneratorProvider interface {
	GenerateImage(a *auth.Auth, prompt, size string, n int) (b64List []string, revisedPrompts []string, err error)
}

type imageGenResult struct {
	B64List []string
	Revised []string
}

func (h *Handler) runImageGenerator(w http.ResponseWriter, r *http.Request, kind provider.Kind, model string, call func(provider.Upstream, *auth.Auth) (any, error)) (any, error) {
	rt := h.getRuntime(kind)
	if rt == nil || rt.Pool == nil || rt.Upstream == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "channel_unavailable", fmt.Sprintf("%s 渠道未接入", kind))
		return nil, errors.New("channel unavailable")
	}
	acct := rt.Pool.PickForModel(model)
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", fmt.Sprintf("%s 无可用健康账号", kind))
		return nil, errors.New("no healthy account")
	}
	res, err := call(rt.Upstream, acct)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return nil, err
	}
	return res, nil
}
