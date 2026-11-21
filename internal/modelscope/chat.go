package modelscope

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/idle"
	"wild-work/internal/upstream"
)

// 超时口径（R35/R58）：两段独立兜底，互不代表。
const (
	// defaultIdleTimeout 拿到响应头之后的流中空闲上限：防「上游连上了、
	// 吐了几字节后彻底卡死」。刻意不跟着 upstream.timeout_seconds 放宽——
	// 那种情况下多等只是让用户多等（与 R58/R110 同口径）。
	defaultIdleTimeout = 90 * time.Second
	// maxErrBody 上游错误体读取上限（够放下 OpenAI 风格错误体与可读提示）。
	maxErrBody = 1 << 20
)

// chatPath OpenAI 兼容对话端点（魔搭 API-Inference 的对话面就是 OpenAI 形状）。
const chatPath = "/v1/chat/completions"

// IdleTimeout 返回实际生效的流中空闲上限（零值客户端退回默认，供测试与日志断言）。
func (c *Client) IdleTimeout() time.Duration {
	if c.idle > 0 {
		return c.idle
	}
	return defaultIdleTimeout
}

// chatPeek 从客户端请求体里取路由需要的最小信息。
//
// 为什么在渠道内再解一次 body：`model` 已被 handler 用 rewriteModel 改成**上游模型名**
// （如 `Qwen/Qwen-Image`），渠道是唯一能据此判断「这是生图模型、不该走对话」的地方。
type chatPeek struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func peekChatBody(body []byte) chatPeek {
	var p chatPeek
	_ = json.Unmarshal(body, &p) // 解析失败不阻断：body 由 handler 保证是合法 JSON
	return p
}

// ChatStream 对话请求：POST {base}/v1/chat/completions，`Authorization: Bearer <token>`，
// **请求体原样透传**（magic 全在 handler 的 PrepareBody 里，渠道不得二次改造）。
//
// 流式走 StreamHTTP（无总超时，R35），非流式走 HTTP（upstream.timeout_seconds 兜底）。
// 传输层错误与超时**原样返回 error**（不计账号错误、不换号——同渠道所有账号打的是
// 同一个 host，网络问题与账号健康无关）。
func (c *Client) ChatStream(a *auth.Auth, body []byte) (io.ReadCloser, int, []byte, error) {
	peek := peekChatBody(body)
	// 生图模型不参与对话路由（D12）：本地拒绝，省掉一次必然失败的出站。
	// 用 400 + image_model_not_chat，Classify 归 ErrBadParams（请求级：不罚号、不轮转）。
	if peek.Model != "" && IsImageModel(peek.Model) {
		return nil, http.StatusBadRequest, imageModelRejectBody(peek.Model), nil
	}
	req, err := c.newRequest(context.Background(), http.MethodPost, chatPath, a, body)
	if err != nil {
		return nil, 0, nil, err
	}
	req, cancel := idle.WithCancel(req)
	hc := c.HTTP
	if peek.Stream && c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		cancel()
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		cancel()
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		_ = resp.Body.Close()
		// 上游 ≥400 原样透传（不变量 8）：由 handler 的 Classify 决定处置。
		return nil, resp.StatusCode, respBody, nil
	}
	return idle.Monitor(resp.Body, c.IdleTimeout(), cancel), http.StatusOK, nil, nil
}

// Stream SSE 原样转发：逐帧透传上游 OpenAI 形状的 chunk，末帧 usage 交给 handler 记流水；
// 同时把每帧的 `model` 回填为**客户端请求的全串**（provider.go:531-540 契约——上游通常
// 回裸名，客户端看到 `Qwen/...` 会以为换了模型）。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	err := upstream.StreamCapture(w, rewriteModelReader(r, model), func(u map[string]any) { usage = u })
	return usage, err
}

// Aggregate 非流式：上游若回普通 JSON 就解成 map（并把 model 回填为客户端全串）；
// 若回的是 SSE（客户端发 stream=false 但上游仍流式）就聚合 SSE 帧。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	br := bufio.NewReader(r)
	for {
		b, err := br.Peek(1)
		if err != nil {
			return nil, err
		}
		if b[0] == ' ' || b[0] == '\t' || b[0] == '\r' || b[0] == '\n' {
			_, _ = br.ReadByte()
			continue
		}
		if b[0] == '{' {
			var resp map[string]any
			if err := json.NewDecoder(br).Decode(&resp); err != nil {
				return nil, err
			}
			if model != "" {
				resp["model"] = model
			}
			return resp, nil
		}
		break
	}
	resp, err := upstream.Aggregate(br)
	if err != nil {
		return nil, err
	}
	if model != "" {
		resp["model"] = model
	}
	return resp, nil
}

// imageModelRejectBody 本地拒绝生图模型时的错误体（OpenAI 风格，客户端能据此自助纠正）。
func imageModelRejectBody(model string) []byte {
	msg := fmt.Sprintf("%s 是生图模型，不支持对话；请改用 POST /v1/images/generations", model)
	b, err := json.Marshal(map[string]any{"error": map[string]any{
		"message": msg,
		"type":    "invalid_request_error",
		"code":    "image_model_not_chat",
	}})
	if err != nil { // 不可能失败：纯字面量 map
		return []byte(`{"error":{"message":"image model not supported for chat","code":"image_model_not_chat"}}`)
	}
	return b
}

// ---------------------------------------------------------------------------
// SSE 模型名回填（OpenAI 形状的 chunk 里 `"model": "…"`）
// ---------------------------------------------------------------------------

type modelRewriter struct {
	src     *bufio.Reader
	pending []byte
	model   string
}

func rewriteModelReader(r io.Reader, model string) io.Reader {
	if model == "" {
		return r
	}
	return &modelRewriter{src: bufio.NewReaderSize(r, 64*1024), model: model}
}

func (m *modelRewriter) Read(p []byte) (int, error) {
	for len(m.pending) == 0 {
		line, err := m.src.ReadBytes('\n')
		if len(line) > 0 {
			m.pending = rewriteModelLine(line, m.model)
		}
		if err != nil {
			if len(m.pending) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, m.pending)
	m.pending = m.pending[n:]
	return n, nil
}

// rewriteModelLine 把一行内所有 `"model" : "…"` 的值替换为 model。
//
// 容忍冒号两侧空白（上游未必是 Go 的紧凑 JSON 编码）；转义引号（tool arguments 里的
// `\"model\":\"…\"`）不会被误改——那里的引号前面有反斜杠，匹配不到 `"model"` 后紧跟 `"`。
func rewriteModelLine(line []byte, model string) []byte {
	needle := []byte(`"model"`)
	out := make([]byte, 0, len(line)+len(model))
	rest := line
	for {
		i := bytes.Index(rest, needle)
		if i < 0 {
			return append(out, rest...)
		}
		j := i + len(needle)
		k := j
		for k < len(rest) && (rest[k] == ' ' || rest[k] == '\t') {
			k++
		}
		if k >= len(rest) || rest[k] != ':' {
			out = append(out, rest[:j]...)
			rest = rest[j:]
			continue
		}
		k++
		for k < len(rest) && (rest[k] == ' ' || rest[k] == '\t') {
			k++
		}
		if k >= len(rest) || rest[k] != '"' {
			out = append(out, rest[:j]...)
			rest = rest[j:]
			continue
		}
		end := bytes.IndexByte(rest[k+1:], '"')
		if end < 0 {
			out = append(out, rest[:j]...)
			rest = rest[j:]
			continue
		}
		end += k + 1
		out = append(out, rest[:k+1]...)
		out = append(out, model...)
		out = append(out, '"')
		rest = rest[end+1:]
	}
}
