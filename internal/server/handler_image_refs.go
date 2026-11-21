// handler_image_refs.go images 端点的「图片字节 IO」：入站参考图归一（拼版托管 / 内联直传）
// 与产物下载。
//
// 拆出本文件的原因：参考图流转有两套语义（glm 单图上游要一个公网 URL；modelscope 吃多张
// 内联引用），与端点编排、账号惩罚无关；混在一个文件里两边互相挤压（单一职责）。
package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wild-work/internal/imgutil"
)

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

// imageEditInput 是 /v1/images/edits 归一后的入参（multipart 与 JSON 两种形态同一形状）。
type imageEditInput struct {
	Model          string
	Prompt         string
	Size           string // 客户端像素串（glm 忽略：输出比例强制跟随输入图）
	NegativePrompt string // 反向提示词（客户端没给就为空，渠道不编造）
	OutputFormat   string // output_format（glm 的 png 走 image_process 真转换）
	Raws           [][]byte // 每张参考图的原始字节（http(s)/data URI 存原串）
}

// parseImageEditInput 解析两种入站形态并校验必填项。失败时**已写出 400**，调用方直接 return。
func parseImageEditInput(w http.ResponseWriter, r *http.Request) (*imageEditInput, bool) {
	in := &imageEditInput{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "parse multipart: "+err.Error())
			return nil, false
		}
		in.Model = r.FormValue("model")
		in.Prompt = r.FormValue("prompt")
		in.Size = r.FormValue("size")
		in.NegativePrompt = r.FormValue("negative_prompt")
		in.OutputFormat = r.FormValue("output_format")
		// OpenAI 官方 SDK 用 image 字段；gpt-image 系适配器（如 whalechan）用 image[]。
		// 两者都收，文件在前（顺序即参考图顺序）。
		for _, key := range []string{"image", "image[]"} {
			for _, fh := range r.MultipartForm.File[key] {
				f, err := fh.Open()
				if err != nil {
					writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "open image part: "+err.Error())
					return nil, false
				}
				raw, err := io.ReadAll(io.LimitReader(f, 20<<20))
				f.Close()
				if err != nil {
					writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read image part: "+err.Error())
					return nil, false
				}
				in.Raws = append(in.Raws, raw)
			}
		}
	} else {
		body, err := ReadBodyLimited(r)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
			return nil, false
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
			return nil, false
		}
		in.Model, in.Prompt, in.Size = req.Model, req.Prompt, req.Size
		in.OutputFormat, in.NegativePrompt = req.OutputFormat, req.NegativePrompt
		for _, s := range req.Image {
			in.Raws = append(in.Raws, []byte(strings.TrimSpace(s)))
		}
	}
	if strings.TrimSpace(in.Prompt) == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing required field: prompt")
		return nil, false
	}
	if len(in.Raws) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "missing required field: image")
		return nil, false
	}
	return in, true
}

// editReferences 把参考图归一成渠道可吃的形式，并给出出站提示词。
//
// 分流是**渠道能力**决定的（不做渠道类型断言）：
//   - 直传型：多张内联引用原样下发，提示词即原文（拼版约束是 glm 语义，不得注入）；
//   - 默认：归一成**一个**公网 URL（单张 http(s) 直传；多张/base64/dataURI 拼版 + 图床托管），
//     提示词补拼版约束前缀与 referenceN 定位说明。
//
// 失败时已写出 400。
func (h *Handler) editReferences(w http.ResponseWriter, imgModel string, in *imageEditInput) ([]string, string, bool) {
	if h.inlineImageRefs(imgModel) {
		urls, err := inlineReferenceURLs(in.Raws)
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return nil, "", false
		}
		return urls, in.Prompt, true
	}
	refURL, err := resolveReferenceURL(in.Raws)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return nil, "", false
	}
	outPrompt := editPromptConstraint + in.Prompt
	if len(in.Raws) > 1 {
		outPrompt = fmt.Sprintf("参考图中共有 %d 个以 reference1..%d 从左到右标注的单元，按序号对应下面提示词中的对象。", len(in.Raws), len(in.Raws)) + outPrompt
	}
	return []string{refURL}, outPrompt, true
}
