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
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/auth"
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
// 参考图形态按**渠道能力**分两路（`provider.InlineImageRefProvider`，见 handler_image_refs.go）：
//   - 直传型（modelscope）：多张 data URI/URL 原样下发，**不拼版、不上图床**（向上游硬上限 20MB）；
//   - 默认（glm）：单图上游 ⇒ 拼版 + 图床托管（既有语义零改动）。
//
// 双形态入参：
//   - multipart（OpenAI 官方 SDK 形状）：image 字段可重复（文件或 URL 文本）；
//   - JSON：{"prompt": "...", "image": ["https://... | data:image/...;base64,..." ...]}。
func (h *Handler) imagesEdits(w http.ResponseWriter, r *http.Request) {
	in, ok := parseImageEditInput(w, r)
	if !ok {
		return // 响应已写出
	}
	imgModel := normalizeImageModel(in.Model)
	refs, outPrompt, ok := h.editReferences(w, imgModel, in)
	if !ok {
		return // 响应已写出
	}
	results, err := h.runImage(w, r, imgModel, func(gen provider.ImageGenerator, acct *auth.Auth) ([]provider.ImageResult, error) {
		return gen.EditImage(acct, provider.ImageRequest{
			Model: imgModel, Prompt: outPrompt, Size: in.Size, N: 1, References: refs,
			NegativePrompt: in.NegativePrompt,
		})
	})
	if err != nil {
		return // 响应已写出
	}
	data := editDataItems(results, in.OutputFormat)
	if len(data) == 0 {
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", "上游未返回产物图")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

// editDataItems 把改图产物转成响应 data[]：「上游给几张就返几张」（不与 n 对齐），
// 下载失败的那张**降级为 url**（日志留痕，不拖垮整批）。
func editDataItems(results []provider.ImageResult, outputFormat string) []map[string]any {
	data := make([]map[string]any, 0, len(results))
	for _, res := range results {
		url := strings.TrimSpace(res.URL)
		if url == "" {
			continue
		}
		item := map[string]any{"url": url}
		if b64, err := fetchImageB64(imageCDNURL(url, outputFormat)); err == nil {
			item = map[string]any{"b64_json": b64}
		} else {
			log.Printf("images: 改图产物下载失败（降级为 url）url=%s err=%v", url, err)
		}
		data = append(data, item)
	}
	return data
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


