// image.go 魔搭生图/改图：**异步任务**协议（submit 拿 task_id → 轮询），
// 对客户端仍是同步 OpenAI 形状（请求内轮询，handler 只看到一次「出图」）。
//
// 协议依据 ModelScope API-Inference 文档（官方：生图异步三步）：
//
//	POST {base}/v1/images/generations  + `X-ModelScope-Async-Mode: true` → {"task_id":"…"}
//	GET  {base}/v1/tasks/{task_id}     + `X-ModelScope-Task-Type: image_generation`
//	     → {"task_status":"…","output_images":["https://…"],…}
//
// 与 glm 生图（同步、一次 4 图、单参考图硬约束）完全不同：这里上游**没有同步面**，
// 也没有「拼版」概念——多张参考图按 `image_url[]` 直传（见 provider.InlineImageRefProvider）。
// 本渠道**不下载**产物：返回上游 URL 列表，b64 转换由 handler 负责（T2 复用同一段逻辑）。
package modelscope

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 编译期断言：modelscope 是「会画图」的渠道（/v1/images/* 按模型名路由到本渠道）。
var _ provider.ImageGenerator = (*Client)(nil)

// 编译期断言：参考图直传（不拼版、不图床）。handler 据此绕开 glm 的拼版路径。
var _ provider.InlineImageRefProvider = (*Client)(nil)

// 上游端点与状态名。
const (
	imageGenPath     = "/v1/images/generations"
	imageTaskPath    = "/v1/tasks/"
	imageTaskTypeHdr = "image_generation"

	statusSucceed  = "SUCCEED"
	statusFailed   = "FAILED"
	statusCanceled = "CANCELED"
)

// 轮询节奏与上限（spec D9）。
const (
	// imagePollInitial 首次轮询间隔；之后每次 +imagePollStep，封顶 imagePollMax。
	imagePollInitial = 2 * time.Second
	imagePollStep    = 500 * time.Millisecond
	imagePollMax     = 5 * time.Second
	// imageMinWait 轮询总时长的**硬下限**：只放宽、不压低（R110 同口径）。
	// upstream.timeout_seconds 为本工具整条请求设的超时，拿它去压轮询会把长任务
	// 打成必然失败；而它比 120s 更大时必须跟随放宽（本地 600s 配置即生效）。
	imageMinWait = 120 * time.Second

	// maxImageN 上游一次最多接受的张数（超出夹到 4，与 glm 口径一致）。
	maxImageN = 4
)

// pixelBySize 把客户端的像素串/比例串归一成上游**像素串**。
//
// 与 glm 的 ratioBySize 刻意不同：glm 收比例串（1:1 等），魔搭收像素串
// （`1024x1024`）。五档来自本渠道实测支持集；比例串按同构档位换算。
// 未知值不会走到这里（handler 已 400 拦截），返回 ok=false 时调用方**不发**该字段。
var pixelBySize = map[string]string{
	"1024x1024": "1024x1024",
	"864x1152":  "864x1152",
	"1152x864":  "1152x864",
	"720x1280":  "720x1280",
	"1280x720":  "1280x720",
	"auto":      "1024x1024",
	"1:1":       "1024x1024",
	"3:4":       "864x1152",
	"4:3":       "1152x864",
	"9:16":      "720x1280",
	"16:9":      "1280x720",
}

// pixelSize 归一 size；空串/未知值返回 ok=false（调用方省略该字段，交给上游默认）。
func pixelSize(size string) (string, bool) {
	px, ok := pixelBySize[strings.TrimSpace(size)]
	return px, ok
}

// InlineImageRefs 实现 provider.InlineImageRefProvider：改图的多张参考图直传
// （`image_url: ["data:image/…;base64,…", …]`），**不拼版、不上图床**。
func (c *Client) InlineImageRefs() bool { return true }

// upstreamImageModel 取客户端全串里的**上游模型名**：`modelscope/Qwen/Qwen-Image`
// → `Qwen/Qwen-Image`（SplitModelID 是拼/拆名的唯一来源，R52）。
func upstreamImageModel(model string) string {
	if _, id, ok := provider.SplitModelID(model); ok && strings.TrimSpace(id) != "" {
		return strings.TrimSpace(id)
	}
	return strings.TrimSpace(model)
}

// clampImageN 归一期望张数：<=0 → 1，>4 → 4。
func clampImageN(n int) int {
	if n <= 0 {
		return 1
	}
	if n > maxImageN {
		return maxImageN
	}
	return n
}

// imageSubmitBody submit 请求体。可选字段一律 omitempty：
// **客户端没给的东西不得编造**（尤其 negative_prompt）。
type imageSubmitBody struct {
	Model          string   `json:"model"`
	Prompt         string   `json:"prompt"`
	Size           string   `json:"size,omitempty"`
	N              int      `json:"n,omitempty"`
	NegativePrompt string   `json:"negative_prompt,omitempty"`
	ImageURL       []string `json:"image_url,omitempty"`
}

// GenerateImage 实现 provider.ImageGenerator：文生图（submit + 轮询）。
func (c *Client) GenerateImage(a *auth.Auth, req provider.ImageRequest) ([]provider.ImageResult, error) {
	return c.runImageTask(a, req, false)
}

// EditImage 实现 provider.ImageGenerator：参考图改图（同一端点，参考图走 `image_url[]`）。
func (c *Client) EditImage(a *auth.Auth, req provider.ImageRequest) ([]provider.ImageResult, error) {
	return c.runImageTask(a, req, true)
}

// runImageTask 走完 submit + poll，返回**上游给的全部产物**（可能多于 n，按原样返回）。
func (c *Client) runImageTask(a *auth.Auth, req provider.ImageRequest, edit bool) ([]provider.ImageResult, error) {
	refs := make([]string, 0, len(req.References))
	for _, r := range req.References {
		if s := strings.TrimSpace(r); s != "" {
			refs = append(refs, s)
		}
	}
	if edit && len(refs) == 0 {
		return nil, &provider.Error{Kind: provider.ErrBadParams, Msg: "改图缺少参考图引用"}
	}
	payload := imageSubmitBody{
		Model:  upstreamImageModel(req.Model),
		Prompt: req.Prompt,
		N:      clampImageN(req.N),
	}
	if px, ok := pixelSize(req.Size); ok {
		payload.Size = px
	}
	if s := strings.TrimSpace(req.NegativePrompt); s != "" {
		payload.NegativePrompt = s
	}
	if edit {
		payload.ImageURL = refs
	}
	body, err := json.Marshal(payload)
	if err != nil { // 不可能失败：纯字面量结构体
		return nil, fmt.Errorf("modelscope: 构造生图请求失败: %w", err)
	}
	taskID, err := c.submitImageTask(a, body)
	if err != nil {
		return nil, err
	}
	log.Printf("modelscope image submit: model=%s task=%s edit=%v n=%d refs=%d",
		payload.Model, taskID, edit, payload.N, len(refs))
	return c.pollImageTask(a, taskID)
}

// submitImageTask 提交异步任务，返回上游 task_id。
func (c *Client) submitImageTask(a *auth.Auth, body []byte) (string, error) {
	req, err := c.newRequest(context.Background(), http.MethodPost, imageGenPath, a, body)
	if err != nil {
		return "", err
	}
	// 异步模式开关：缺这个头上游会按同步处理（生图模型上表现为长时间不返回）。
	req.Header.Set("X-ModelScope-Async-Mode", "true")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err // 传输层错误原样返回（handler 判为非账号错误，不换号）
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	if resp.StatusCode >= 400 {
		return "", &provider.Error{Kind: c.Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: clip(string(raw), 200)}
	}
	id := extractTaskID(raw)
	if id == "" {
		// 200 但没有 task_id：协议已变或上游换形态 —— 不猜、当上游故障上报。
		return "", &provider.Error{Kind: provider.ErrServer, Status: resp.StatusCode,
			Msg: "submit 响应缺少 task_id: " + clip(string(raw), 200)}
	}
	return id, nil
}

// extractTaskID 取 task_id（顶层，或 data 段里；两种形态都见过）。
func extractTaskID(raw []byte) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	if s, ok := objString(m, "task_id", "taskId"); ok {
		return s
	}
	if d, ok := m["data"].(map[string]any); ok {
		if s, ok := objString(d, "task_id", "taskId"); ok {
			return s
		}
	}
	return ""
}

// imageTaskResult 轮询响应（`output_images` 是官方字段；`outputs.images[].url`
// 是同一份数据的另一种形状，两者都收，前者优先）。
type imageTaskResult struct {
	TaskStatus   string   `json:"task_status"`
	OutputImages []string `json:"output_images"`
	Outputs      struct {
		Images []struct {
			URL string `json:"url"`
		} `json:"images"`
	} `json:"outputs"`
}

// imageURLs 汇总产物 URL（保序、去空）。
func (t imageTaskResult) imageURLs() []string {
	out := make([]string, 0, len(t.OutputImages))
	for _, u := range t.OutputImages {
		if s := strings.TrimSpace(u); s != "" {
			out = append(out, s)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, im := range t.Outputs.Images {
		if s := strings.TrimSpace(im.URL); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// pollImageTask 轮询到出结果、失败或超时。
//
// 状态映射：SUCCEED → 产物 URL；FAILED/CANCELED → ErrUpstream（不可重试、不换号）；
// 其余（含 PROCESSING/未知状态）继续轮询直到总时限。
func (c *Client) pollImageTask(a *auth.Auth, taskID string) ([]provider.ImageResult, error) {
	limit := c.imagePollLimit()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	start := time.Now()
	delay := imagePollInitial
	polls := 0
	for {
		polls++
		task, err := c.fetchImageTask(ctx, a, taskID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, c.taskTimeout(taskID, time.Since(start))
			}
			return nil, err
		}
		switch task.TaskStatus {
		case statusSucceed:
			urls := task.imageURLs()
			if len(urls) == 0 {
				return nil, &provider.Error{Kind: provider.ErrUpstream, Status: http.StatusOK,
					Msg: "上游任务 " + taskID + " 状态 SUCCEED 但没有产物图"}
			}
			log.Printf("modelscope image done: task=%s polls=%d images=%d elapsed=%s",
				taskID, polls, len(urls), time.Since(start).Round(time.Millisecond))
			out := make([]provider.ImageResult, 0, len(urls))
			for _, u := range urls {
				out = append(out, provider.ImageResult{URL: u})
			}
			return out, nil
		case statusFailed, statusCanceled:
			log.Printf("modelscope image %s: task=%s polls=%d elapsed=%s",
				task.TaskStatus, taskID, polls, time.Since(start).Round(time.Millisecond))
			return nil, &provider.Error{Kind: provider.ErrUpstream, Status: http.StatusBadGateway,
				Msg: "上游任务 " + taskID + " 状态 " + task.TaskStatus + "（本工具不重试已提交任务）"}
		}
		// PROCESSING / PENDING / 未知状态：继续等。未知状态不得当成失败——
		// 上游加新状态（如 RUNNING）时提前放弃只会把可成功的长任务打成失败。
		if err := c.sleep(ctx, delay); err != nil {
			return nil, c.taskTimeout(taskID, time.Since(start))
		}
		delay += imagePollStep
		if delay > imagePollMax {
			delay = imagePollMax
		}
	}
}

// fetchImageTask 查一次任务状态。
func (c *Client) fetchImageTask(ctx context.Context, a *auth.Auth, taskID string) (imageTaskResult, error) {
	var out imageTaskResult
	req, err := c.newRequest(ctx, http.MethodGet, imageTaskPath+url.PathEscape(taskID), a, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("X-ModelScope-Task-Type", imageTaskTypeHdr)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	if resp.StatusCode >= 400 {
		return out, &provider.Error{Kind: c.Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: clip(string(raw), 200)}
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, &provider.Error{Kind: provider.ErrServer, Status: resp.StatusCode,
			Msg: "轮询响应不是合法 JSON: " + clip(string(raw), 200)}
	}
	return out, nil
}

// imagePollLimit 轮询总时限 = max(120s, upstream.timeout_seconds)（只放宽、不压低）。
func (c *Client) imagePollLimit() time.Duration {
	if c.pollLimit > 0 { // 测试注入点
		return c.pollLimit
	}
	if t := c.HTTP.Timeout; t > imageMinWait {
		return t
	}
	return imageMinWait
}

// taskTimeout 构造超时错误：带 task id + 「任务可能仍在执行」，**不伪造**结果（R36 同族）。
func (c *Client) taskTimeout(taskID string, elapsed time.Duration) error {
	return &provider.TaskTimeout{
		TaskID:  taskID,
		Elapsed: elapsed,
		Err:     &provider.Error{Kind: provider.ErrUpstream, Status: http.StatusGatewayTimeout, Msg: "poll timeout"},
	}
}

// sleep 轮询间隔（pollSleep 为测试注入点，nil = 真实计时器）。
func (c *Client) sleep(ctx context.Context, d time.Duration) error {
	if c.pollSleep != nil {
		return c.pollSleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// clip 截断上游响应体（只用于错误文案，不用于解析）。
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}