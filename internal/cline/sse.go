package cline

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"wild-work/internal/upstream"
)

// localErrMarkers 是 streamCore **本地合成**的兜底帧标识。
// 它们代表网关自身观察到的故障（空流/截断），不是上游下发的业务 error 帧，
// 不得当作「上游错误帧」二次归类 —— 那种形态 streamCore 已经返回了 error。
var localErrMarkers = []string{
	"empty upstream stream",
	`"code":"upstream_truncated"`,
	`"code":"upstream_parse"`,
}

func isLocalErrFrame(payload string) bool {
	for _, m := range localErrMarkers {
		if strings.Contains(payload, m) {
			return true
		}
	}
	return false
}

// parseErrFrame 把一帧 error 载荷转成 *StreamError；非 error 帧返回 nil。
//
// 实测形态（2026-10-10 抓包，thinkingmachines/inkling-small:free 等 6 个模型）：
//
//	data: {"error":{"code":"stream_initialization_failed",
//	      "message":"Failed to create stream: … request failed with status 429:
//	                 {\"error\":{…\"Daily limit reached for …\"}}"}}
//	data: [DONE]
//
// 即 **HTTP 200 + 一帧 error + 上游自己发完 [DONE]**。照旧透传会让客户端看到
// 「模型回了个空答案」，而 handler 拿不到 error ⇒ 账号不惩罚、粘性路由不清（R40）。
func parseErrFrame(payload string) *StreamError {
	var obj struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &obj) != nil || obj.Error.Message == "" {
		return nil
	}
	se := &StreamError{
		Code:   obj.Error.Code,
		Msg:    obj.Error.Message,
		capped: IsCapBody(payload),
	}
	// 数字 code（实测另见 "502"）＝ 上游 provider 侧故障，同样不罚账号。
	if se.Code == "" {
		se.Code = "unknown"
	}
	return se
}

// Stream 实现 provider.Upstream：逐帧透传上游 SSE，同时
// ① 捕获流内 error 帧并转成可分类的 error（R40）；② 捕获末帧 usage 供 ledger 记账。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	var frame string

	serr := upstream.StreamWith(w, r,
		func(payload string) string {
			// hintFn 只在 error 帧上被调用；这里只取样、不附加 hint（返回空串 ⇒ 原文透传）。
			if frame == "" && !isLocalErrFrame(payload) {
				frame = payload
			}
			return ""
		},
		func(u map[string]any) { usage = u })

	if frame != "" {
		se := parseErrFrame(frame)
		if se != nil {
			se.Model = model
			if se.capped {
				// 共享出口上限：只标记模型（/v1/models 期间摘除），**不动账号**。
				c.markCapped(model, capCooldown)
			}
			log.Printf("cline: 流内 error 帧 uid=- model=%s code=%s capped=%v msg=%.180s",
				model, se.Code, se.capped, se.Msg)
			if serr == nil {
				return usage, se
			}
		}
	}
	return usage, serr
}

// Aggregate 实现 provider.Upstream（客户端 stream=false 时由 handler 调用，
// 见 internal/server/handler.go 的 Aggregate 分支）。
//
// 上游的 `{data,success}` 非流式信封在这里**不会被看到** —— 本渠道永远请求 stream=true
// （不变量 1），所以聚合走的是流式帧，信封问题与本渠道无关。
// 但 error 帧同样要识别：否则非流式客户端拿到空 choices 且不报错（R40 的第二个入口）。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil {
		return nil, err
	}
	se := scanAggregateErrFrame(raw)
	res, aerr := upstream.Aggregate(bytes.NewReader(raw))
	if res != nil {
		// 回填调用方请求的原始模型名（上游常回 "auto" 或裸名，R14 同款显式传参）。
		res["model"] = model
	}
	if res != nil {
		// 路由归属记一行：运维判断"是 Cline 网关抖动还是 OpenRouter 日限"的唯一线索。
		if id, _ := res["id"].(string); id != "" {
			log.Printf("cline: 聚合完成 model=%s route=%s", model, RouteOf(id))
		}
	}
	if se != nil {
		se.Model = model
		if se.capped {
			c.markCapped(model, capCooldown)
		}
		log.Printf("cline: 聚合流内 error 帧 model=%s code=%s capped=%v msg=%.180s",
			model, se.Code, se.capped, se.Msg)
		if aerr == nil {
			return res, se
		}
	}
	return res, aerr
}

// scanAggregateErrFrame 在已读尽的 SSE 文本里找第一帧**上游** error。
func scanAggregateErrFrame(raw []byte) *StreamError {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if p == "" || p == "[DONE]" || isLocalErrFrame(p) {
			if p == "[DONE]" {
				break
			}
			continue
		}
		if !strings.Contains(p, `"error"`) {
			continue
		}
		if se := parseErrFrame(p); se != nil {
			return se
		}
	}
	return nil
}
