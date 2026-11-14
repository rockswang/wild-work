// handler_images.go OpenAI Images 兼容端点：/v1/images/generations + /v1/images/edits。
//
// 渠道**按客户端请求的模型名选择**：`<kind>/<上游ID>` → provider.SplitModelID → Runtime
// → Upstream 的 provider.ImageGenerator 实现（可选能力）。当前实现：glm（对外模型名
// `glm/image`，也是改造前本端点唯一认识的模型名）与 modelscope/modelscopeai
// （异步任务型生图，见 internal/modelscope/image.go）。
//
// 与 chat 的差异：**换号上限 = 池内账号数**（每个号最多试一次）。生图是同步长操作
// （4~22s），但账号级错误（限流/登录态失效/账号故障）换号仍值得——不换只会把每次请求
// 都打回同一个失败；请求内容问题/上游任务失败则**不**换号（换号必复现）。
//
// 协议要点（[实证] chatglm-cli docs/生图协议与实操.md，均为 glm 渠道内部细节）：
//   - 文生图：一次 4 图、12~15s、不扣积分；产物 CDN 直链会过期 ⇒ 默认下载转 b64_json；
//   - size→ratio 映射必须严格：上游对非法比例**静默降级 1:1**，故本层 400 拒绝；
//   - 参考图改图：单图硬约束 ⇒ 多图走「客户端拼版（imgutil.BuildRefSheet）+ 图床托管
//     （imgutil.UploadJPEG）」；单张 http(s) URL 直传（不下载不上传，链路最短）；
//   - 输出比例强制跟随输入图（aspect_ratio 无效）⇒ edits 的 size 参数忽略并在响应头提示。
package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/imgutil"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// ratioBySize OpenAI size → 比例串的**校验表**（尺寸取自 2026-10-15 glm 实测全集）。
// 也接受直接传比例（"1:1" 等）。未知值必须 400——上游会静默降级 1:1，而客户端无从察觉。
// 本表只用来判「这个值能不能生图」，像素串原值（req.Size）照旧下传给渠道：
// 换算是各渠道自己的语义（glm 有自己的同名映射，见 internal/glm/image.go）。
// 不可删除：未知 size 的 400 是外部契约（TestImagesGenerationsBadSize）。
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

// legacyImageModel 本端点改造前唯一对外模型名。
// 客户端不带 model（或带了个无 `<kind>/` 前缀的裸名，如 OpenAI 的 `gpt-image-1`）时
// 回落它——改造前 model 是**忽略**的，不能让老客户端因此变红（既有用例零改动）。
const legacyImageModel = "glm/image"

// normalizeImageModel 把客户端 model 归一成「可用于路由的全串」：
// 空串与无 `<kind>/` 前缀的裸名都回落 legacyImageModel（改造前 model 被忽略）。
// 路由 key、冷却 key 与下发给渠道的 ImageRequest.Model 用**同一个**归一后的值，
// 避免「按 A 选号、告诉渠道 B」的错位。幂等，可重复调用。
func normalizeImageModel(model string) string {
	if strings.TrimSpace(model) == "" {
		return legacyImageModel
	}
	if _, _, ok := provider.SplitModelID(model); !ok {
		return legacyImageModel
	}
	return model
}

// glmRuntime 取 glm 渠道 Runtime；未接入返回 nil。
// 路由已改走 imageRuntimeFor；本访问器保留给「glm 特判」与既有测试使用。
func (h *Handler) glmRuntime() *Runtime {
	return h.cfg.Runtimes[provider.GLM]
}

// imageRuntimeFor 按客户端请求的模型名选出「会画图」的渠道。
//
// 返回 (runtime, 该渠道上游的 ImageGenerator 实现, 解析出的渠道 kind, ok)：
// ok=false 表示「这个模型名不能用于生图」——未知渠道、渠道不实现 ImageGenerator、
// 或 runtime 未装配（rt 是否为 nil 由调用方决定报错形态）。
func (h *Handler) imageRuntimeFor(model string) (rt *Runtime, gen provider.ImageGenerator, kind provider.Kind, ok bool) {
	k, _, split := provider.SplitModelID(model)
	if !split {
		return nil, nil, "", false
	}
	rt = h.cfg.Runtimes[k]
	if rt == nil {
		return nil, nil, k, false
	}
	gen, ok = rt.Upstream.(provider.ImageGenerator)
	return rt, gen, k, ok
}

// imagesGenerations POST /v1/images/generations —— 文生图（按 model 选渠道的画图能力）。
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
		// NegativePrompt 可选；**没给就不发**（渠道不得编造，实测口径见 image.go）。
		NegativePrompt string `json:"negative_prompt"`
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
	if _, ok := ratioBySize[ratio]; !ok {
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

	imgModel := normalizeImageModel(req.Model)
	imgs, err := h.runImage(w, r, imgModel, func(gen provider.ImageGenerator, acct *auth.Auth) ([]provider.ImageResult, error) {
		// Size 传**像素串原值**（不是本层换算出的比例）：各渠道自行取用，
		// glm 在自己的包里换算，T2 的魔搭直通上游。
		return gen.GenerateImage(acct, provider.ImageRequest{
			Model: imgModel, Prompt: req.Prompt, Size: req.Size, N: n,
			NegativePrompt: req.NegativePrompt,
		})
	})
	if err != nil {
		return // 响应已写出
	}
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

// imagesEdits POST /v1/images/edits —— 参考图改图。
//
// 参考图形态按**渠道能力**分两路（`provider.InlineImageRefProvider`）：
//   - 直传型（modelscope）：多张 data URI/URL 原样下发，**不拼版、不上图床**（向上游硬上限 20MB）；
//   - 默认（glm）：单图上游 ⇒ 拼版 + 图床托管（既有语义零改动）。
//
// 双形态入参：
//   - multipart（OpenAI 官方 SDK 形状）：image 字段可重复（文件或 URL 文本）；
//   - JSON：{"prompt": "...", "image": ["https://... | data:image/...;base64,..." ...]}。
func (h *Handler) imagesEdits(w http.ResponseWriter, r *http.Request) {
	ct := r.Header.Get("Content-Type")
	var model string
	var prompt string
	var sizeStr string // 客户端像素串（glm 忽略：输出比例强制跟随输入图）
	var negative string // 反向提示词（客户端没给就为空，渠道不编造）
	var raws [][]byte  // 每张参考图的原始字节（URL 形态在 resolve 时特判直传）
	var outFmt string  // output_format（png 时产物转真 PNG）

	if strings.HasPrefix(ct, "multipart/") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse multipart: "+err.Error())
			return
		}
		model = r.FormValue("model")
		prompt = r.FormValue("prompt")
		sizeStr = r.FormValue("size")
		negative = r.FormValue("negative_prompt")
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
			Model          string   `json:"model"`
			Prompt         string   `json:"prompt"`
			Size           string   `json:"size"`
			Image          []string `json:"image"`
			OutputFormat   string   `json:"output_format"`
			NegativePrompt string   `json:"negative_prompt"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
			return
		}
		model = req.Model
		prompt = req.Prompt
		sizeStr = req.Size
		outFmt = req.OutputFormat
		negative = req.NegativePrompt
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

	imgModel := normalizeImageModel(model)
	// 分流：直传型渠道吃内联引用（多图原样、不拼版）；其余（glm 默认语义）把参考图归一成
	// 一个公网 URL——单张 http(s) 直传，多张/base64/dataURI 拼版 + 图床托管。
	var refs []string
	outPrompt := prompt
	if h.inlineImageRefs(imgModel) {
		urls, err := inlineReferenceURLs(raws)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		refs = urls
	} else {
		refURL, err := resolveReferenceURL(raws)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		refs = []string{refURL}
		// 出站提示词：约束前缀（拼版排版不得复刻）+ 多图时给 referenceN 定位说明。
		outPrompt = editPromptConstraint + prompt
		if len(raws) > 1 {
			outPrompt = fmt.Sprintf("参考图中共有 %d 个以 reference1..%d 从左到右标注的单元，按序号对应下面提示词中的对象。", len(raws), len(raws)) + outPrompt
		}
	}

	results, err := h.runImage(w, r, imgModel, func(gen provider.ImageGenerator, acct *auth.Auth) ([]provider.ImageResult, error) {
		return gen.EditImage(acct, provider.ImageRequest{
			Model: imgModel, Prompt: outPrompt, Size: sizeStr, N: 1, References: refs,
			NegativePrompt: negative,
		})
	})
	if err != nil {
		return // 响应已写出
	}
	// 「上游给几张就返几张」（data[] 长度不与 n 强行对齐；拿不到 URL 的条目跳过）。
	data := make([]map[string]any, 0, len(results))
	for _, res := range results {
		url := strings.TrimSpace(res.URL)
		if url == "" {
			continue
		}
		item := map[string]any{"url": url}
		if b64, err := fetchImageB64(imageCDNURL(url, outFmt)); err == nil {
			item = map[string]any{"b64_json": b64}
		} else {
			log.Printf("images: 改图产物下载失败（降级为 url）url=%s err=%v", url, err)
		}
		data = append(data, item)
	}
	if len(data) == 0 {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "上游未返回产物图")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

// inlineImageRefs 报告该模型对应的渠道是否吃**内联多参考图**（能力声明，不做渠道类型断言）。
// 未接入/未知渠道返回 false，由 runImage 统一报 invalid_model。
func (h *Handler) inlineImageRefs(model string) bool {
	_, gen, _, ok := h.imageRuntimeFor(model)
	if !ok {
		return false
	}
	p, ok := gen.(provider.InlineImageRefProvider)
	return ok && p.InlineImageRefs()
}

// maxInlineRefBytes 内联参考图总字节上限（上游按多图直传时的硬上限）。
// 超出必须 **400 明确拒绝**，不得回落拼版：拼版会改变多图语义（给 glm 单图上游用的）。
// var 而非 const：测试把它调小以免造 20MB+ 样本。
var maxInlineRefBytes = 20 << 20

// inlineReferenceURLs 把入站参考图转成渠道可直传的内联引用：
// http(s) 直链原样、data URI 原样、裸 base64/二进制 → data URI（mime 嗅探）。
func inlineReferenceURLs(raws [][]byte) ([]string, error) {
	out := make([]string, 0, len(raws))
	total := 0
	for i, raw := range raws {
		s := strings.TrimSpace(string(raw))
		switch {
		case strings.HasPrefix(s, "http://"), strings.HasPrefix(s, "https://"):
			out = append(out, s) // 上游自己拉取，不占本地字节预算
			continue
		case strings.HasPrefix(s, "data:"):
			// base64 长度 → 解码后字节数（4 字符 = 3 字节，含 padding 也只会高估）。
			_, payload, ok := strings.Cut(s, ",")
			if !ok {
				return nil, fmt.Errorf("参考图 %d data URI 非法（缺少逗号分隔）", i+1)
			}
			total += len(payload) * 3 / 4
			out = append(out, s)
		default:
			b := decodeReferenceBytes(s, raw)
			if len(b) == 0 {
				return nil, fmt.Errorf("参考图 %d 为空", i+1)
			}
			total += len(b)
			out = append(out, "data:"+http.DetectContentType(b)+";base64,"+base64.StdEncoding.EncodeToString(b))
		}
		if total > maxInlineRefBytes {
			return nil, fmt.Errorf("参考图总字节 %d 超过上限 %d 字节（本渠道按多图直传，不做拼版压缩）", total, maxInlineRefBytes)
		}
	}
	return out, nil
}

// decodeReferenceBytes 裸 base64 → 原始字节；解不出来就当作已经是二进制图片。
func decodeReferenceBytes(s string, raw []byte) []byte {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) > 0 {
		return b
	}
	return raw
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

// runImage 按模型名选渠道 + 选号执行生图调用，统一错误分类、惩罚与**请求内换号**。
//
// 换号（D16）：生图是同步长操作（4~22s），但「账号级错误」换号的价值仍然成立——
// 限流/登录态失效/账号故障的号留着只会把每次请求都打回同一个失败。故：
//   - 可重试错误（`provider.ErrKind.Rotatable()`）换下一个账号，**上限 = 池内账号数**
//     （每个号最多试一次；没有硬编码常量上限）；
//   - `ErrSoftRate` 先对该号做**模型级**冷却（key = 请求全串）再换；
//   - 不可重试（内容拦截/参数错/上游任务失败/传输层）立即结束；
//   - 全池耗尽 → 用**首因**回 429/502（不把最后一个号的错误当前因）。
//
// model 是**客户端请求的全串**：粘性路由 key 与模型级冷却 key 都落在它上面——
// `glm/image` 因此与改造前逐字一致，`modelscope/Qwen/Qwen-Image` 各自独立冷却。
func (h *Handler) runImage(w http.ResponseWriter, r *http.Request, model string,
	call func(provider.ImageGenerator, *auth.Auth) ([]provider.ImageResult, error)) ([]provider.ImageResult, error) {
	model = normalizeImageModel(model)
	rt, gen, kind, ok := h.imageRuntimeFor(model)
	if !ok {
		if kind == provider.GLM && rt == nil {
			// 旧语义（改造前唯一路径）：`glm/image` 未接入 → 503 可操作引导。
			writeOpenAIError(w, http.StatusServiceUnavailable, "channel_unavailable", "glm 渠道未接入（未绑定账号）")
			return nil, errors.New("no glm runtime")
		}
		reason := fmt.Sprintf("渠道 %s 不支持生图", provider.DisplayName(kind))
		if rt == nil {
			reason = fmt.Sprintf("渠道 %s 未接入（未知渠道或未装配）", kind)
		}
		// 必须 400，不得 404：404 归 ErrNotFound（Rotatable）⇒ handler 换号 + NoteError，
		// 换到的号照样被本地拒，几次之后整条渠道进冷却（R139 同款事故）。
		log.Printf("images invalid model=%s kind=%s reason=%s", model, kind, reason)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model",
			fmt.Sprintf("模型 %q 不能用于生图：%s", model, reason))
		return nil, fmt.Errorf("invalid image model: %s", model)
	}
	platform := provider.DisplayName(rt.Kind)
	acct := h.pickWithSticky(rt, model)
	if acct == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account",
			platform+" 渠道无可用账号（全部冷却/登录态失效），请在面板检查账号状态")
		return nil, errors.New("no healthy image account")
	}
	// 轮转上限 = 池内账号数（每个号最多试一次）。
	maxAttempts := len(rt.Pool.List())
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	tried := make(map[string]bool, maxAttempts)
	var firstErr error
	for attempt := 0; acct != nil && attempt < maxAttempts; attempt++ {
		res, err := call(gen, acct)
		if err == nil {
			rt.Pool.NoteSuccess(acct.UID)
			if attempt > 0 {
				// 换号成功：粘性改钉新号，后续请求直接复用它（会话/缓存连续性不丢）
				h.stickyPin(rt, acct.UID)
			}
			h.stickySuccess(rt)
			return res, nil
		}
		tried[acct.UID] = true
		if firstErr == nil {
			firstErr = err
		}
		h.stickyClear(rt)
		if !h.penalizeImage(rt, platform, model, acct, err) {
			h.writeImageError(w, platform, err)
			return nil, err
		}
		next := rt.Pool.PickExcludingForModel(tried, model)
		if next == nil {
			break
		}
		log.Printf("images rotate platform=%s model=%s from=%s to=%s reason=%v",
			platform, model, acct.UID, next.UID, err)
		acct = next
	}
	// 全池耗尽：透传**首因**（首号是限流就 429，是 5xx/任务失败就 502）。
	if acct != nil && firstErr != nil {
		log.Printf("images rotate exhausted platform=%s model=%s tried=%d err=%v",
			platform, model, len(tried), firstErr)
	}
	h.writeImageError(w, platform, firstErr)
	return nil, firstErr
}

// penalizeImage 按错误类别施加账号惩罚，返回**是否值得换号继续本请求**。
//
// 惩罚表与改造前逐项一致（每个分支的冷却类型/日志字段不交），只是把「写响应」
// 从里面抽出去——因为现在可能不结束请求而是换号。
func (h *Handler) penalizeImage(rt *Runtime, platform, model string, acct *auth.Auth, err error) bool {
	var pe *provider.Error
	if !errors.As(err, &pe) {
		// 传输层错误：与账号健康无关（同一 host），计错但不换号（R109 同口径）
		rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
		log.Printf("images transport error platform=%s uid=%s err=%v", platform, acct.UID, err)
		return false
	}
	switch pe.Kind {
	case provider.ErrSessionDead:
		// 登录态失效：标记（挑号跳过 + 面板徽标），后面的号可继续
		rt.Pool.MarkSessionDead(acct.UID, pe.Msg)
		log.Printf("images session dead platform=%s uid=%s err=%s", platform, acct.UID, pe.Msg)
	case provider.ErrSoftRate:
		// 限流/风控瞬时拒绝：**模型级**冷却（key = 客户端请求的 model），不冻结整个账号
		rt.Pool.CooldownModel(acct.UID, model, h.cfg.SoftCooldown, "生图限流: "+pe.Msg)
		log.Printf("images rate limited platform=%s uid=%s model=%s err=%s", platform, acct.UID, model, pe.Msg)
	case provider.ErrWafBlock, provider.ErrAccountFault:
		rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, pe.Kind.String())
		log.Printf("images waf/fault platform=%s uid=%s kind=%s err=%s", platform, acct.UID, pe.Kind, pe.Msg)
	case provider.ErrContentBlocked, provider.ErrBadParams, provider.ErrPassthrough:
		// 请求内容问题：不罚号，也不换号（换个号必然复现）
		log.Printf("images request rejected platform=%s uid=%s kind=%s err=%s", platform, acct.UID, pe.Kind, pe.Msg)
	default:
		log.Printf("images upstream error platform=%s uid=%s kind=%s err=%s", platform, acct.UID, pe.Kind, pe.Msg)
	}
	return pe.Kind.Rotatable()
}

// writeImageError 把生图错误写成对外响应（状态码/code 映射是外部契约，不得改码）。
// err 为 nil 时不写任何东西（防御：调用方保证非 nil）。
func (h *Handler) writeImageError(w http.ResponseWriter, platform string, err error) {
	if err == nil {
		err = errors.New("unknown image error")
	}
	// 异步任务超时（modelscope 生图）：上游已受理、可能仍在执行 ⇒ 504 + task id
	var tt *provider.TaskTimeout
	if errors.As(err, &tt) {
		log.Printf("images task timeout platform=%s task=%s err=%s", platform, tt.TaskID, tt.Error())
		writeOpenAIError(w, http.StatusGatewayTimeout, "task_timeout", platform+" 生图任务超时："+tt.Error())
		return
	}
	var pe *provider.Error
	if !errors.As(err, &pe) {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", platform+" 生图网络错误："+err.Error())
		return
	}
	switch pe.Kind {
	case provider.ErrSessionDead:
		writeOpenAIError(w, http.StatusUnauthorized, "session_dead", platform+" 账号登录态失效，请重新登录："+pe.Msg)
	case provider.ErrSoftRate:
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_exceeded", platform+" 生图被限流，请稍后重试："+pe.Msg)
	case provider.ErrWafBlock, provider.ErrAccountFault:
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", platform+" 上游风控拦截："+pe.Msg)
	case provider.ErrContentBlocked, provider.ErrBadParams, provider.ErrPassthrough:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", pe.Msg)
	default:
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", platform+" 生图失败："+pe.Msg)
	}
}

// stickyPin 把粘性路由改钉到指定账号（换号成功后保持后续请求的会话/缓存连续）。
func (h *Handler) stickyPin(rt *Runtime, uid string) {
	h.stickyMu.Lock()
	h.sticky[h.stickyKey(rt.Kind)] = &stickyEntry{uid: uid, maxReqs: h.cfg.StickyMaxRequests}
	h.stickyMu.Unlock()
	log.Printf("new sticky route platform=%s uid=%s maxReqs=%d reason=image_rotate",
		rt.Kind, uid, h.cfg.StickyMaxRequests)
}

// imageCDNURL 按 output_format 调整产物 CDN 直链：sfile.chatglm.cn 产物支持
// ?image_process=format,png 真转换 [实证 生图协议 §2.2]；其他格式/非上游 CDN 原样返回。
//
// modelscope 的产物是 OSS 直链（非 chatglm CDN）⇒ `output_format=png` **不转换**，
// 原样 b64（内容可能仍是 jpg）。这是已知限制（R158），不是 bug：上游没有转换参数。
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
