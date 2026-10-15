//go:build devdump

// devdump_on.go 生产行为实现（仅 `-tags devdump` 编译）。
//
// 为什么整包被构建标签切成两半：无 tag 时 devdump_off.go 里的 Wrap 是
// 「原样返回」的常量实现，编译器内联后 dump 相关代码彻底不存在于产物中——
// 发布版不可能因「忘了关开关」而产生任何开销或漏出上游原文。
package devdump

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// envVar 运行期开关的环境变量名。设置该变量为目录路径即开启落盘；
// 清空后重新启动（或调 Disable）即关闭。
const envVar = "WILDWORK_DUMP_DIR"

// state 运行期开关："" = 关闭；非空 = 落盘根目录。
// 用 atomic.Pointer[string] 而非 atomic.Value：后者装载需类型断言、零值不可用；
// 这里 Store 后热路径只做一次指针读，零锁零分配。
var state atomic.Pointer[string]

func init() { Refresh() }

// Refresh 重读环境变量（启动期由 main 调一次）。
func Refresh() { set(os.Getenv(envVar)) }

// Enable 以 dir 为落盘根目录开启（空串等价 Disable）。
func Enable(dir string) { set(dir) }

// Disable 关闭落盘。
func Disable() { set("") }

// set 切换开关；目录建不出来即视为关闭（不把错误带到热路径）。
func set(dir string) {
	d := ""
	if dir != "" && os.MkdirAll(dir, 0o755) == nil {
		if abs, err := filepath.Abs(dir); err == nil {
			d = abs
		} else {
			d = dir
		}
	}
	state.Store(&d)
}

// Enabled 报告当前是否开启落盘。
func Enabled() bool {
	p := state.Load()
	return p != nil && *p != ""
}

// dir 当前落盘根目录（未开启返回空串）。
func dir() string {
	if p := state.Load(); p != nil {
		return *p
	}
	return ""
}

// Wrap 给 client 的 Transport 套上 dump 层并返回同一个 client。
// 未开启时原样返回入参（不新建 Transport、不改变行为）。
// client.Transport 为 nil 时用 http.DefaultTransport 兜底（生产各渠道都非 nil）。
func Wrap(client *http.Client, channel string) *http.Client {
	if client == nil || !Enabled() {
		return client
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = &dumpTransport{base: base, channel: channel}
	return client
}

// maxBody 单侧 body 落盘上限。SSE 长回答可达数 MB，全量落盘会拖慢开发期
// 请求且无必要——本工具的目标是「看 usage 字段」，取头部即可。
// 但 usage 常出现在**末帧**，故截断时保留「前 (maxBody-tailKeep) + 后 tailKeep」。
const maxBody = 256 << 10

// tailKeep 截断时保留的尾部字节数（usage 帧多在末尾）。
const tailKeep = 96 << 10

// seq 全局序号，保证同一毫秒内并发请求的文件名不冲突。
var seq atomic.Uint64

// dumpTransport 包装底层 RoundTripper：转发请求的同时把请求/响应快照落盘。
// 请求体先完整读出（开发期体积可控），再用新 Reader 重建，不影响上游。
type dumpTransport struct {
	base    http.RoundTripper
	channel string
}

// pathSanitize 把 URL 路径压成文件名安全片段（保留字母数字与 -_）。
var pathSanitize = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

func (t *dumpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// 请求体：读全量后重建，保证下游照常发送（ContentLength 已定，GetBody 兜底）。
	reqBody := []byte(nil)
	if req.Body != nil {
		reqBody, _ = io.ReadAll(req.Body)
		_ = req.Body.Close()
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}

	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	dur := time.Since(start)

	// 只有「真需要看上游原文」的请求才落盘：目录/费率探测与回环请求一律跳过。
	if !shouldDump(req) {
		return resp, err
	}

	var (
		status     int
		respHeader http.Header
		respBody   []byte
	)
	if err != nil {
		respBody = []byte("<<transport error: " + err.Error() + ">>")
	} else if resp != nil {
		status, respHeader = resp.StatusCode, resp.Header
		respBody, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}
	writeDump(t.channel, req, reqBody, status, respHeader, respBody, err, dur)
	return resp, err
}

// dumpSubstr 命中即落盘的路径片段（各渠道对话/推理入口，实测路径全列）。
var dumpSubstr = []string{
	"chat/completions",      // workbuddy / workbuddyai|oczen / loomy / raccoon / cline（含代理寻址的变体）
	"assistant/stream",      // glm 智谱清言
	"agent_chat_generation", // qodercn / qodercom / qwenwork（SSE 对话）
	"llm_utils_chat",        // traework / traecode（SOLO）
	"/messages",             // monkeycode（Anthropic 形状）
	"/responses",            // monkeycode（Responses 形状）
}

// noDumpSubstr 命中即跳过的路径片段（目录/费率/凭据/状态探测，噪音高）。
// 只列**明确非对话**的路径：宁可多 dump 一个，也不要漏掉某个渠道的对话帧。
var noDumpSubstr = []string{
	"/models", "/api-keys", "/pricing", "/user/refresh", "/user/info",
	"member_info", "/balance", "/quota", "checkin", "ent_usage", "campaigns",
	"/v1/messages/count_tokens", // Anthropic 兼容层的 token 计数（非推理）
}

// shouldDump 判定该请求是否值得落盘（只 dump 对话类 POST）。
func shouldDump(req *http.Request) bool {
	if req == nil || req.URL == nil || req.Method != http.MethodPost {
		return false
	}
	switch req.URL.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return false
	}
	p := strings.ToLower(req.URL.Path)
	for _, s := range noDumpSubstr {
		if strings.Contains(p, s) {
			return false
		}
	}
	for _, s := range dumpSubstr {
		if strings.Contains(p, s) {
			return true
		}
	}
	return false
}

// writeDump 落盘一份快照；任何失败静默（调试工具不得干扰主流程）。
func writeDump(channel string, req *http.Request, reqBody []byte, status int,
	respHeader http.Header, respBody []byte, err error, dur time.Duration) {
	root := dir()
	if root == "" {
		return
	}
	if channel == "" {
		channel = "unknown"
	}
	sub := filepath.Join(root, channel)
	if os.MkdirAll(sub, 0o755) != nil {
		return
	}
	f, ferr := os.OpenFile(filepath.Join(sub, dumpName(req, int(seq.Add(1)))), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	defer func() { _ = f.Close() }()

	var b bytes.Buffer
	// 响应侧 usage 速览放文件头：分析时一眼可见，无需翻正文（本包的存在理由）。
	fmt.Fprintf(&b, "# channel=%s status=%d duration=%s\n", channel, status, dur.Round(time.Millisecond))
	fmt.Fprintf(&b, "# usage=%s\n", usageHint(respBody))
	fmt.Fprintf(&b, "### REQUEST %s %s\n", req.Method, req.URL.String())
	writeHeaderBlock(&b, req.Header)
	b.WriteString("\n")
	b.Write(truncateBody(reqBody))
	b.WriteString("\n\n")
	if err != nil {
		fmt.Fprintf(&b, "### TRANSPORT ERROR\n%s\n", err.Error())
	} else {
		fmt.Fprintf(&b, "### RESPONSE %d\n", status)
		writeHeaderBlock(&b, respHeader)
		b.WriteString("\n")
		b.Write(truncateBody(respBody))
		b.WriteString("\n")
	}
	_, _ = f.Write(b.Bytes())
}

// usageHint 从响应体里抓出 usage 片段压缩成一行速览（**本包存在的唯一目的**：
// 一眼看清上游到底发了哪些 token/缓存字段）。SSE 里取**最后**一个 usage（末帧才是全量）。
func usageHint(body []byte) string {
	if len(body) == 0 {
		return "(empty)"
	}
	s := string(body)
	i := strings.LastIndex(strings.ToLower(s), "usage")
	if i < 0 {
		return "(none)"
	}
	seg := s[i:]
	if len(seg) > 320 {
		seg = seg[:320]
	}
	seg = strings.NewReplacer("\n", " ", "\r", " ", "\"", "").Replace(seg)
	return strings.Join(strings.Fields(seg), " ")
}

// writeHeaderBlock 写 HTTP 头（keys 排序，便于跨请求 diff）。
func writeHeaderBlock(b *bytes.Buffer, h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			fmt.Fprintf(b, "%s: %s\n", k, v)
		}
	}
}

// truncateBody 超长时保留头尾（usage 常在末帧）。
func truncateBody(raw []byte) []byte {
	if len(raw) <= maxBody {
		return raw
	}
	var b bytes.Buffer
	fmt.Fprintf(&b, "<<truncated: kept first %d + last %d of %d bytes>>\n", maxBody-tailKeep, tailKeep, len(raw))
	b.Write(raw[:maxBody-tailKeep])
	b.WriteString("\n<<...omitted...>>\n")
	b.Write(raw[len(raw)-tailKeep:])
	return b.Bytes()
}

// dumpName 生成文件名：<时间戳>-<序号>-<方法>-<路径摘要>-<URL哈希>.http。
// 哈希段保证同路径不同 query 的不同请求不互相覆盖。
func dumpName(req *http.Request, n int) string {
	slug := strings.Trim(pathSanitize.ReplaceAllString(req.URL.Path, "_"), "_")
	if len(slug) > 48 {
		slug = slug[:48]
	}
	sum := sha1.Sum([]byte(req.URL.String()))
	return fmt.Sprintf("%s-%04d-%s-%s-%s.http",
		time.Now().Format("20060102-150405.000"), n, strings.ToLower(req.Method), slug, hex.EncodeToString(sum[:4]))
}
