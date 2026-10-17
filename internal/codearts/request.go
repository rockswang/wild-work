// request.go 出站请求的签名装配与执行（控制面 / 对话共用）。
//
// 关键纪律（协议 §2.2）：**签名后追加的头绝不参与签名**——`Agent-Type` / `X-Language` /
// `Chat-Id` / `Session-Id` / `lang` 都在 signAndApply 之后设置，若混进 canonical 会得到
// `401 APIG.0301 verify ak sk signature fail`。
package codearts

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"
)

// signAndApply 计算 SDK-HMAC-SHA256 签名并落到请求上。
//
// body 必须与真正写入请求体的字节**完全一致**（签名里含 x-sdk-content-sha256），
// 故调用方先把它 marshal 好再传进来。
func signAndApply(req *http.Request, ac *Account, body []byte, contentType string,
	signedExtra map[string]string, now time.Time) error {
	headers, err := SignRequest(ac.AccessKeyID, ac.SecretAccessKey, ac.SecurityToken,
		req.Method, req.URL.String(), body, contentType, signedExtra, now)
	if err != nil {
		return err
	}
	ApplySignedHeaders(req, headers)
	return nil
}

// doSigned 执行一次已签名请求并读回响应体（供控制面接口使用）。
func (c *Client) doSigned(client *http.Client, ac *Account, method, rawURL, contentType string,
	body []byte, signedExtra, unsigned map[string]string) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, rawURL, rd)
	if err != nil {
		return 0, nil, err
	}
	if err := signAndApply(req, ac, body, contentType, signedExtra, time.Now()); err != nil {
		return 0, nil, err
	}
	applyUnsigned(req, unsigned)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

// applyUnsigned 追加**不参与签名**的头。
func applyUnsigned(req *http.Request, unsigned map[string]string) {
	for k, v := range unsigned {
		if strings.TrimSpace(v) == "" {
			continue
		}
		req.Header.Set(k, v)
	}
}

// controlHeaders 目录 / 积分 / 活动端点的未签名头（协议 §2.2 表）：
// `Agent-Type: PromptCenter` + `X-Language: zh-cn`。
func controlHeaders() map[string]string {
	return map[string]string{HeaderAgentType: AgentTypePromptCenter, HeaderXLanguage: LangZH}
}

// queueHeaders queue/status 端点的未签名头：`Agent-Type: INFERHUB_AGENT` + `X-Language: en`
// + `x-snap-traceid`（uuid）。
func queueHeaders() map[string]string {
	return map[string]string{
		HeaderAgentType:   AgentTypeInferHub,
		HeaderXLanguage:   LangEN,
		HeaderSnapTraceID: UUID4(),
	}
}
