// upload.go 参考图托管：把拼版 JPEG 传到大陆可达的匿名图床，换公网直链。
//
// glm 的参考图改图端点只收「上游服务器可达的公网 URL」（data URI/境外直连 500），
// 官方上传接口 chat_upload 被 WAF 拦死（缺 refer__991 时 100% 40012 [实证]），
// 故用第三方图床中转。
//
// 图床选型依据 ref/大陆可达图床评估.md（2026-10-16 实测）：
//   - 主选 file.sang.pub meitu 通道（落地 meitudata.com 大陆 CDN）；
//   - 失败回退 icve 通道（落地 file.icve.com.cn，任意格式、上限 100MB）。
// ⚠️ 必须 raw binary body（--data-binary 语义），multipart 会被原样存成信封。
// ⚠️ 参考图会上传到第三方：只用于本工具的生图链路，用户应知悉该行为。
package imgutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// SangUploadURL 图床上传端点（公开变量以便 httptest 注入）。
var SangUploadURL = "https://file.sang.pub/api/upload"

// uploadChannels 通道回退顺序（meitu 限图片格式但快，icve 任意格式较慢）。
var uploadChannels = []string{"meitu", "icve"}

var uploadClient = &http.Client{Timeout: 120 * time.Second}

// UploadJPEG 把 JPEG 字节传到图床，返回公网直链。
// 逐通道尝试：HTTP 传输错误或 code!=200 都视为该通道失败，换下一个。
func UploadJPEG(jpeg []byte) (url, channel string, err error) {
	name := fmt.Sprintf("wildwork-ref-%d.jpg", time.Now().UnixMilli())
	var lastErr error
	for _, ch := range uploadChannels {
		req, e := http.NewRequest(http.MethodPost, SangUploadURL, bytes.NewReader(jpeg))
		if e != nil {
			return "", "", e
		}
		req.Header.Set("X-Upload-Type", ch)
		req.Header.Set("X-File-Name", name)
		req.Header.Set("X-File-Size", fmt.Sprint(len(jpeg)))
		req.Header.Set("Content-Type", "image/jpeg")

		resp, e := uploadClient.Do(req)
		if e != nil {
			lastErr = fmt.Errorf("通道 %s 传输失败: %w", ch, e)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("通道 %s HTTP %d: %s", ch, resp.StatusCode, truncStr(string(body), 120))
			continue
		}
		// 响应 {"code":200,"msg":"success","data":"https://.../<hash>.jpg"}
		code, direct, e := parseSangResp(body)
		if e != nil {
			lastErr = fmt.Errorf("通道 %s 响应异常: %w", ch, e)
			continue
		}
		if code != 200 || direct == "" {
			lastErr = fmt.Errorf("通道 %s 上传被拒 code=%d", ch, code)
			continue
		}
		if !strings.HasPrefix(direct, "http") {
			lastErr = fmt.Errorf("通道 %s 返回异常直链: %q", ch, truncStr(direct, 120))
			continue
		}
		return direct, ch, nil
	}
	return "", "", fmt.Errorf("全部图床通道失败: %w", lastErr)
}

func parseSangResp(body []byte) (code int, data string, err error) {
	var m struct {
		Code int    `json:"code"`
		Data string `json:"data"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return 0, "", err
	}
	return m.Code, strings.TrimSpace(m.Data), nil
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
