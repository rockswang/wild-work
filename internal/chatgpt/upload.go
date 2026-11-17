package chatgpt

// 图片上传 / 下载（同源 ww3 internal/cgpt/upload.go）。
//
// 上传四步（照抄上游，缺一步 file_id 就不可用于对话）：
//
//	① POST /backend-api/files/upload_reservations          预占上传槽位（可失败，非致命）
//	② POST /backend-api/files                              登记元数据 → file_id + upload_url
//	③ PUT  <azure blob upload_url>                         直传原始字节
//	④ POST /backend-api/files/process_upload_stream        服务端处理（响应是**裸 NDJSON**）
//
// 三个照抄就会踩的约束：
//   - ③ 的对象存储那一步**不得带 Authorization**（SAS 在 URL query 里，多带一个头就 403），
//     也不能用会话头族构造器——所以那里自建一份最小头集；
//   - ④ 的 `Content-Type: text/event-stream` 是**假象**，body 是每行一条裸 JSON（无 `data:` 前缀）；
//   - 对话消息体里的附件指针用 `sediment://<file_id>`（不是 `file-service://`）。
//
// 下载两步（产物图）：
//
//	① GET /backend-api/files/download/<file_id>?conversation_id=… → download_url（签名直链）
//	② GET <download_url> → 图片字节
//
// ⚠️ 签名有效期只有几分钟 ⇒ **现取现用、不得缓存复用**（每个产物都要各走一遍）。
// W3-07 纪律：参考图/产物字节**绝不落日志**，日志只允许张数 / 字节数 / mime。

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// 上传 / 下载端点。
const (
	PathUploadReservations = "/backend-api/files/upload_reservations"
	PathFiles              = "/backend-api/files"
	PathProcessUpload      = "/backend-api/files/process_upload_stream"
	pathFileDownload       = "/backend-api/files/download/"
)

// 上游文件端点固定取值（抓包实测值；同源于 ww3）。
const (
	uploadEntrySurface = "chat_composer"
	uploadUseCaseImage = "multimodal"
	uploadLibMode      = "opportunistic"
	// uploadMaxSizeMB 是上游 file_attachment_limits.max_size_mb。
	uploadMaxSizeMB = 512
	// downloadMaxBytes 是单张产物下载上限（防上游回一个无限流把内存吃光）。
	downloadMaxBytes = 64 << 20
)

// attachment 是已上传到上游的一张图（对话消息体里的 image_asset_pointer）。
type attachment struct {
	FileID    string
	Name      string
	MimeType  string
	SizeBytes int64
	Width     int
	Height    int
}

// UploadImage 把图片字节上传成上游可引用的 file_id（四步）。
//
// mime 为空时按魔数嗅探；嗅不出来就**报错而不是猜**（上游按 mime 决定后续处理）。
// 失败返回 *provider.Error：4xx/5xx 走渠道 Classify（限流仍可换号），
// 形状漂移（缺 file_id / 处理未完成）归 ErrUpstream（不换号、不罚账号）。
func (c *Client) UploadImage(a *auth.Auth, data []byte, name, mime string) (*attachment, error) {
	if len(data) == 0 {
		return nil, badParams("参考图为空（0 字节）")
	}
	if len(data) > uploadMaxSizeMB<<20 {
		return nil, badParams("参考图 %q 有 %d 字节，超过上游上限 %d MB", name, len(data), uploadMaxSizeMB)
	}
	if strings.TrimSpace(mime) == "" {
		mime = sniffImageMime(data)
	}
	if mime == "" {
		return nil, badParams("无法判断图片类型（魔数不认识；支持 png/jpg/webp/gif/bmp）")
	}
	if strings.TrimSpace(name) == "" {
		name = "ref" + extByMime(mime)
	}

	// ① 预占（老账号可能没有 reservation 资格，失败不影响主流程）。
	resUploadURL := c.reserveUploadSlot(a)

	// ② 登记元数据，拿 file_id 与 upload_url。
	reg := map[string]any{
		"entry_surface":                   uploadEntrySurface,
		"file_name":                       name,
		"file_size":                       len(data),
		"client_resolved_mime_type":       mime,
		"mime_resolution_source":          "filename_extension",
		"mime_type":                       mime,
		"reset_rate_limits":               false,
		"supports_direct_azure_multipart": true,
		"timezone_offset_min":             defaultTimezoneOffsetMin,
		"use_case":                        uploadUseCaseImage,
		"store_in_library":                true,
		"library_persistence_mode":        uploadLibMode,
	}
	st, m, raw, err := c.DoJSON(a, "POST", PathFiles, reg, map[string]string{"content-type": "application/json"})
	if err != nil {
		return nil, err // 传输层错误原样返回：与账号健康无关
	}
	if st != http.StatusOK {
		return nil, StatusError("POST", PathFiles, st, raw)
	}
	fileID := str(m["file_id"])
	uploadURL := str(m["upload_url"])
	if fileID == "" {
		return nil, upstreamErr("POST /files 未返回 file_id: " + truncate(string(raw), 400))
	}
	if uploadURL == "" {
		uploadURL = resUploadURL
	}
	if uploadURL == "" {
		return nil, upstreamErr("POST /files 未返回 upload_url（且预占也没拿到）")
	}

	// ③ 直传对象存储（**不带 Authorization**，SAS 在 URL 里）。
	if err := c.putBlob(a, uploadURL, data, mime); err != nil {
		return nil, err
	}

	// ④ 服务端处理（裸 NDJSON 三帧），未完成 = file_id 不可用。
	if err := c.processUpload(a, fileID, name, mime); err != nil {
		return nil, err
	}
	w, h := imageSize(data)
	return &attachment{FileID: fileID, Name: name, MimeType: mime, SizeBytes: int64(len(data)), Width: w, Height: h}, nil
}

// reserveUploadSlot 预占一次上传槽，返回它的 upload_url（拿不到返回空串）。
// 失败**不阻塞**上传：登记那一步通常也会给 upload_url。
func (c *Client) reserveUploadSlot(a *auth.Auth) string {
	body := map[string]any{
		"intended_use_case":        "my_files",
		"entry_surface":            uploadEntrySurface,
		"requires_gizmo_id":        false,
		"store_in_library":         true,
		"library_persistence_mode": uploadLibMode,
	}
	st, m, _, err := c.DoJSON(a, "POST", PathUploadReservations, body, map[string]string{"content-type": "application/json"})
	if err != nil || st != http.StatusOK || m == nil {
		return ""
	}
	return str(m["upload_url"])
}

// putBlob 直传对象存储。
//
// 结构与流式请求不同：Blob 侧只认 SAS query，多带 Authorization 会 403。
// 因此这里**自建**一份最小头集（不走 session.header），只用同一个 TLS 客户端
// （指纹仍然有效，且代理 / cookie jar 复用）。
func (c *Client) putBlob(a *auth.Auth, absURL string, data []byte, mime string) error {
	s, err := c.session(a)
	if err != nil {
		return err
	}
	// 等响应头看门狗：自定义路径不受 net/http 的 ResponseHeaderTimeout 约束。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	timer := time.AfterFunc(s.headerTo, cancel)
	defer timer.Stop()

	req, err := fhttp.NewRequestWithContext(ctx, http.MethodPut, absURL, bytes.NewReader(data))
	if err != nil {
		return upstreamErr("构造对象存储上传请求失败: " + err.Error())
	}
	req.Header = fhttp.Header{
		"content-type":           {mime},
		"x-ms-blob-type":         {"BlockBlob"},
		"x-ms-version":           {"2020-04-08"},
		"x-ms-blob-content-type": {mime},
		"origin":                 {s.base},
		"referer":                {s.base + "/"},
		"sec-fetch-site":         {"cross-site"},
		"sec-fetch-mode":         {"cors"},
		"sec-fetch-dest":         {"empty"},
		"user-agent":             {s.ua},
		"accept":                 {"*/*"},
		"accept-language":        {"en,en-GB;q=0.9,en-US;q=0.8"},
	}
	req.Header[fhttp.HeaderOrderKey] = []string{"content-type", "x-ms-blob-type", "x-ms-version",
		"x-ms-blob-content-type", "origin", "referer", "sec-fetch-site", "sec-fetch-mode",
		"sec-fetch-dest", "user-agent", "accept", "accept-language"}
	req.Header[fhttp.PHeaderOrderKey] = []string{":method", ":authority", ":scheme", ":path"}

	resp, err := s.cli.Do(req)
	if err != nil {
		return err // 传输层错误：不换号、不计账号错误
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return upstreamErr("对象存储上传返回 HTTP " + strconv.Itoa(resp.StatusCode) + ": " + truncate(string(b), 300))
	}
	// 必须读干净再关：连接要能回到池子里复用。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// processUpload 让服务端处理文件，缺失 file.processing.completed 即失败。
//
// 响应声明 `text/event-stream` 但 body 是**每行裸 JSON**（无 `data:` 前缀）——
// 用 SSE 解析器会一行都读不到。
func (c *Client) processUpload(a *auth.Auth, fileID, name, mime string) error {
	body := map[string]any{
		"entry_surface":            uploadEntrySurface,
		"file_id":                  fileID,
		"file_name":                name,
		"index_for_retrieval":      false,
		"library_persistence_mode": uploadLibMode,
		"metadata":                 map[string]any{"store_in_library": true, "is_project_thread": false},
		"mime_type":                mime,
		"use_case":                 uploadUseCaseImage,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return upstreamErr("构造文件处理请求失败: " + err.Error())
	}
	st, b, err := c.Do(a, "POST", PathProcessUpload, raw, map[string]string{"content-type": "application/json"})
	if err != nil {
		return err
	}
	if st != http.StatusOK {
		return StatusError("POST", PathProcessUpload, st, b)
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "data: ")
		if line == "" || line == "[DONE]" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if str(ev["event"]) == "file.processing.completed" {
			return nil
		}
	}
	return upstreamErr("文件处理未完成（未收到 file.processing.completed）: " + truncate(string(b), 400))
}

// DownloadImage 把 `sediment://` 指针换成真字节（两步）。
//
// 返回 (字节, 文件名, 签名直链)。签名直链给 `response_format=url` 用；
// **有效期只有几分钟 ⇒ 不得缓存复用**，每个产物每次都要重走这两步。
func (c *Client) DownloadImage(a *auth.Auth, fileID, convID string) ([]byte, string, string, error) {
	fileID = strings.TrimSpace(fileID)
	if fileID == "" {
		return nil, "", "", badParams("下载产物需要 file_id")
	}
	p := pathFileDownload + url.PathEscape(fileID) +
		"?conversation_id=" + url.QueryEscape(convID) + "&download_intent=false&inline=true&include_library_file_state=true"
	st, m, raw, err := c.DoJSON(a, "GET", p, nil, nil)
	if err != nil {
		return nil, "", "", err
	}
	if st != http.StatusOK {
		return nil, "", "", StatusError("GET", pathFileDownload, st, raw)
	}
	dlURL := str(m["download_url"])
	if dlURL == "" {
		return nil, "", "", upstreamErr("下载接口未返回 download_url: " + truncate(string(raw), 400))
	}
	if !strings.HasPrefix(dlURL, "http") {
		dlURL = c.baseURL() + dlURL
	}
	name := str(m["file_name"])
	if name == "" {
		name = fileID
	}

	st2, body, err := c.DoAbs(a, "GET", dlURL, nil, nil)
	if err != nil {
		return nil, "", "", err
	}
	if st2 != http.StatusOK {
		return nil, "", "", upstreamErr("产物直链返回 HTTP " + strconv.Itoa(st2) + ": " + truncate(string(body), 300))
	}
	if len(body) == 0 {
		return nil, "", "", upstreamErr("产物直链返回 0 字节")
	}
	if len(body) > downloadMaxBytes {
		return nil, "", "", upstreamErr("产物超过单张下载上限（" + strconv.Itoa(downloadMaxBytes) + " 字节）")
	}
	return body, name, dlURL, nil
}

// badParams 是「请求内容/参数问题」的本地拒绝（不换号、不罚账号）。
func badParams(format string, args ...any) *provider.Error {
	return &provider.Error{Kind: provider.ErrBadParams, Status: http.StatusBadRequest, Msg: fmt.Sprintf(format, args...)}
}

// ── 图片类型 / 尺寸（纯标准库，不解码整图） ────────────────────────────────

// sniffImageMime 由魔数判 MIME（**不认识就返回空串，不猜**）。
func sniffImageMime(b []byte) string {
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif"
	case len(b) >= 2 && b[0] == 'B' && b[1] == 'M':
		return "image/bmp"
	}
	return ""
}

// extByMime 是 mime → 文件名后缀（上传时上游按后缀判断 mime_resolution_source）。
func extByMime(mime string) string {
	switch strings.ToLower(mime) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/bmp":
		return ".bmp"
	}
	return ".img"
}

// imageSize 从文件头读宽高；读不出返回 0,0（上游接受缺省尺寸，不臆造）。
func imageSize(b []byte) (w, h int) {
	if len(b) < 24 {
		return 0, 0
	}
	// PNG: IHDR 在 16..24
	if b[0] == 0x89 && b[1] == 'P' && b[2] == 'N' && b[3] == 'G' && len(b) >= 24 {
		return int(binary.BigEndian.Uint32(b[16:20])), int(binary.BigEndian.Uint32(b[20:24]))
	}
	// GIF: 6..10 little endian
	if string(b[0:3]) == "GIF" {
		return int(binary.LittleEndian.Uint16(b[6:8])), int(binary.LittleEndian.Uint16(b[8:10]))
	}
	// JPEG: 扫 SOFn 段
	if b[0] == 0xFF && b[1] == 0xD8 {
		for i := 2; i+9 < len(b); {
			if b[i] != 0xFF {
				i++
				continue
			}
			marker := b[i+1]
			if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
				i += 2
				continue
			}
			segLen := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
			// SOF0..SOF15（排除 0xC4 DHT / 0xC8 JPG / 0xCC DAC）
			if marker >= 0xC0 && marker <= 0xCF && marker != 0xC4 && marker != 0xC8 && marker != 0xCC {
				if i+9 <= len(b) {
					h = int(binary.BigEndian.Uint16(b[i+5 : i+7]))
					w = int(binary.BigEndian.Uint16(b[i+7 : i+9]))
					return w, h
				}
			}
			i += 2 + segLen
		}
	}
	// WEBP: VP8X / VP8 / VP8L
	if len(b) > 30 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		switch string(b[12:16]) {
		case "VP8X":
			w = int(uint32(b[24])|uint32(b[25])<<8|uint32(b[26])<<16) + 1
			h = int(uint32(b[27])|uint32(b[28])<<8|uint32(b[29])<<16) + 1
			return w, h
		case "VP8 ":
			return int(binary.LittleEndian.Uint16(b[26:28]) & 0x3FFF), int(binary.LittleEndian.Uint16(b[28:30]) & 0x3FFF)
		case "VP8L":
			bits := binary.LittleEndian.Uint32(b[21:25])
			w = int(bits&0x3FFF) + 1
			h = int((bits>>14)&0x3FFF) + 1
			return w, h
		}
	}
	return 0, 0
}
