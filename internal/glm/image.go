// image.go glm 生图（文生图 + 参考图改图）。
//
// 协议依据 chatglm-cli docs/生图协议与实操.md（全部 [实证]，2026-10-15 线上复核）：
//   - **没有独立的「创建生图任务」接口**：文生图就是普通对话通道换 AI画图 agent
//     （DrawingAssistantID）+ meta_data.cogview 参数，产物在 SSE 里逐张推回；
//   - 一次固定回 4 图（12.5~14.7s），**不扣积分**（left_score 前后不变）；
//   - 图帧的 part.status=finish（不是 init），润色后 prompt 在 image 内容项的 code 字段；
//   - 流末尾有一行不带 data: 前缀的裸 JSON 信封 ⇒ parseSSE 的 default 分支天然忽略；
//   - aspect_ratio 只认 1:1/3:4/4:3/9:16/16:9，非法值**静默降级 1:1** ⇒ 调用方必须校验；
//   - rm_label_watermark:true 实证去水印（2026-10-16 A/B）。
//
// 参考图改图走独立的同步端点 image/reference（17~22s），只收**上游可达的公网 URL**；
// 多参考图由 server 层先调 imgutil 拼版 + 图床托管（上游单图硬约束）。
package glm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// imageGenTimeout 生图流的整体兜底超时（实测 12.5~14.7s；流式 client 无 Client.Timeout，
// 靠这里兜「上游卡死」。参考 idle.Monitor 的思路但对生图用更紧的墙钟）。
const imageGenTimeout = 3 * time.Minute

// GeneratedImage 一张生成图。
type GeneratedImage struct {
	// URL 产物 CDN 直链（无签名但会过期 [未知具体时长]，调用方按需立即下载）。
	URL string
	// RevisedPrompt 上游润色后的实际生图 prompt（文生图路径有；改图路径无此字段）。
	RevisedPrompt string
}

// generateImageBody 构造文生图请求体：普通对话体换 agent + cogview.aspect_ratio。
func generateImageBody(prompt, ratio string) glmChatRequest {
	return glmChatRequest{
		AssistantID:    DrawingAssistantID,
		ConversationID: "",
		ProjectID:      "",
		ChatType:       "user_chat",
		Messages: []glmMessage{{
			Role:    "user",
			Content: []glmContentItem{{Type: "text", Text: prompt}},
		}},
		MetaData: glmChatMeta{
			Channel:          "",
			DraftID:          "",
			Cogview:          glmCogview{RmLabelWatermark: true, AspectRatio: ratio},
			IfPlusModel:      false,
			InputQuestionTyp: "xxxx",
			IsNetworking:     false, // 生图不需要联网检索
			IsTest:           false,
			Platform:         "pc",
			QuoteLogID:       "",
			SelectedModel:    chatModel,
		},
	}
}

// GenerateImage 文生图：发一次画图 agent 的对话流，收集全部产物图。
// n≤0 表示收全部（上游一次 4 图）。返回的图按上游产出顺序排列。
//
// ratio 必须是 1:1/3:4/4:3/9:16/16:9 之一（上游对非法值静默降级 1:1，
// 归一化与校验是 server 层的职责，这里只透传）。
func (c *Client) GenerateImage(a *auth.Auth, prompt, ratio string, n int) ([]GeneratedImage, error) {
	payload, err := json.Marshal(generateImageBody(prompt, ratio))
	if err != nil {
		return nil, err
	}
	rc, status, respBody, err := c.postStream(a, payload)
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		if rc != nil {
			rc.Close()
		}
		return nil, &provider.Error{Kind: Classify(status, string(respBody)), Status: status,
			Msg: truncate(string(respBody), 200)}
	}
	defer rc.Close()

	// 逐帧收集 image 内容项（part.status=finish 是该图全文；同图可能在多帧重复，去重保序）。
	var imgs []GeneratedImage
	seen := map[string]bool{}
	done := false
	werr := parseSSE(rc, func(frame []byte) error {
		var f streamFrame
		if json.Unmarshal(frame, &f) != nil {
			return nil // 非 JSON 行（裸信封已被 parseSSE 过滤，这里防御）吞掉继续
		}
		for _, p := range f.Parts {
			for _, ct := range p.Content {
				if ct.Type != "image" {
					continue
				}
				for _, im := range ct.Image {
					if im.ImageURL == "" || seen[im.ImageURL] {
						continue
					}
					seen[im.ImageURL] = true
					imgs = append(imgs, GeneratedImage{URL: im.ImageURL, RevisedPrompt: ct.Code})
				}
			}
		}
		// 收尾帧：顶层 status=finish（one_to_more_finish）
		if f.Status == "finish" {
			done = true
		}
		return nil
	})
	if werr != nil {
		return nil, fmt.Errorf("glm: 生图流读取失败: %w", werr)
	}
	// 流被截断不得伪装正常收尾（R36 同族）：既无 finish 帧也无图，视为上游异常。
	if !done && len(imgs) == 0 {
		return nil, &provider.Error{Kind: provider.ErrServer, Status: 200,
			Msg: "生图流被截断（未见 finish 帧与产物）"}
	}
	if n > 0 && len(imgs) > n {
		imgs = imgs[:n]
	}
	return imgs, nil
}

// EditImage 参考图改图（智能编辑）：同步长请求（实测 17~22s，HTTP client 10min 超时内）。
//
// imageURL 必须是上游服务器能拉到的公网 URL（data URI/境外直连返回 status=500 或 10031）；
// 输出比例强制跟随输入图（aspect_ratio 传值无效 [实证]，恒传 1:1 占位）。
// 返回产物 CDN 直链。会话不自动删除（用户决议：生图不自动删除）。
func (c *Client) EditImage(a *auth.Auth, imageURL, prompt string) (string, error) {
	token, err := c.acquireToken(context.Background(), a)
	if err != nil {
		return "", err
	}
	title := "【智能编辑】 " + truncate(prompt, 40)
	body, _ := json.Marshal(map[string]any{
		"image_url":          imageURL, // 唯一必需输入：公网 URL
		"file_id":            "",
		"type":               "Intelligent Editing",
		"category":           1,
		"conversation_id":    "",
		"history_id":         "",
		"assistant_id":       DrawingAssistantID,
		"prompt":             prompt,
		"if_plus_model":      false,
		"title":              title,
		"aspect_ratio":       "1:1", // 无效参数 [实证]，照抄前端约定占位
		"rm_label_watermark": true,
	})
	raw, err := c.doJSON(context.Background(), http.MethodPost, EpImageReference, token, json.RawMessage(body))
	if err != nil {
		return "", err
	}
	var r struct {
		ImageURL string `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("glm: 解析 image/reference 响应失败: %w", err)
	}
	if r.ImageURL == "" {
		return "", &provider.Error{Kind: provider.ErrServer, Status: 200,
			Msg: "上游未返回产物图 URL"}
	}
	return r.ImageURL, nil
}
