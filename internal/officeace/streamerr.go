package officeace

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"wild-work/internal/provider"
)

// 上游（华为网关）在部分限流/故障时用 **HTTP 200 + 流内 error 帧** 返回
// （如 `data:{"error":{"code":"81111",...}}`）。通用 streamCore 只把该帧原样透传给你
// 的客户端，**不返回 error** ⇒ handler 拿不到分类、无法冷却账号（R40）。
// 本渠道据此自己嗅探 error 帧并返回实现 StreamErrorClassifier 的错误，
// 供 handler 按 Kind() 冷却账号（换号可解限流）。

// StreamError 是「流内业务错误」，实现 provider.StreamErrorClassifier。
type StreamError struct {
	Status int
	Body   string
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("upstream stream error (http %d): %s", e.Status, truncate([]byte(e.Body), 300))
}

// Kind 把流内错误分类（复用与 HTTP 路径同一套 classifyUpstream）。
func (e *StreamError) Kind() provider.ErrKind { return classifyUpstream(e.Status, e.Body) }

var _ provider.StreamErrorClassifier = (*StreamError)(nil)

// errorSniffer 是包在 SSE 读取流下方的透传嗅探器：不改字节，只在完整 data 行里
// 记录最后一个带 "error" 键的 payload（末帧权威）。
type errorSniffer struct {
	src     io.Reader
	line    []byte
	payload string
}

func (s *errorSniffer) Read(p []byte) (int, error) {
	n, err := s.src.Read(p)
	for i := 0; i < n; i++ {
		c := p[i]
		if c == '\n' {
			s.procLine()
			s.line = s.line[:0]
		} else {
			s.line = append(s.line, c)
		}
	}
	return n, err
}

func (s *errorSniffer) procLine() {
	line := strings.TrimSpace(strings.TrimRight(string(s.line), "\r"))
	if !strings.HasPrefix(line, "data:") {
		// 容忍裸 `{...}`（部分上游不写 data: 前缀）
		if !strings.HasPrefix(line, "{") {
			return
		}
	} else {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	}
	if line == "[DONE]" || !strings.HasPrefix(line, "{") {
		return
	}
	// 必须能解析成 JSON 且**顶层存在 `error` 键**才算错误帧。
	// 不能用 strings.Contains(line, "error")：正常回答正文里出现 error 字样
	// （模型在写代码 / 解释报错）会被误判成错误帧 → 产出 StreamError →
	// statusOfPayload 默认 502 → classifyUpstream 判 ErrServer（Rotatable）→
	// handler 累计 errCount 后**冷却健康账号**（实证误伤）。
	var obj map[string]any
	if json.Unmarshal([]byte(line), &obj) != nil {
		return
	}
	if _, ok := obj["error"]; ok {
		s.payload = line
	}
}
