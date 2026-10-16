//go:build devdump

// devdump_on.go 生产行为实现（仅 `-tags devdump` 编译）。
//
// 为什么整包被构建标签切成两半：无 tag 时 devdump_off.go 里的 Wrap 是
// 「原样返回」的常量实现，编译器内联后 dump 相关代码彻底不存在于产物中——
// 发布版不可能因「忘了关开关」而产生任何开销或漏出上游原文。
package devdump

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
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

	// 只有「真需要看上游原文」的请求才落盘：目录/费率探测与回环请求一律跳过。
	if !shouldDump(req) || dir() == "" {
		return resp, err
	}
	// ⚠️ 不得写 t.channel：dumpTransport 挂在共享 http.Client.Transport 上，
	// 同一 transport 会被多个 goroutine 并发使用，改写字段即数据竞争。用局部值。
	channel := t.channel
	if channel == "" {
		channel = "unknown"
	}

	// ⚠️ 这里**不得** io.ReadAll(resp.Body)：那会把整个响应缓冲到底才交给调用方，
	// 对 SSE 响应等于「上游流完之后客户端才一次性收到全部内容」——实测表现为
	// 「对话时正文突然整段出现」（本机常开 WILDWORK_DUMP_DIR 时必现）。
	// 故改为 tee 边转发边落盘：响应体原样交给调用方逐帧读取并即时 flush，
	// 落盘走带缓冲的独立写入（见 respRecorder），既不攒批也不逐字节写盘。
	rec := newRespRecorder(channel, req, reqBody, start)
	// ⚠️ 错误路径必须 close()：否则已建的文件句柄泄漏、且只留一个空文件
	// （close 里会 writeHeadLocked 把请求块 + TRANSPORT ERROR 落盘）。
	if resp == nil || err != nil {
		rec.fail("### TRANSPORT ERROR\n" + errText(err) + "\n")
		_ = rec.close()
		return resp, err
	}
	rec.setResp(resp.StatusCode, resp.Header)
	if resp.Body == nil {
		rec.fail("### TRANSPORT ERROR\nno response body\n")
		_ = rec.close()
		return resp, err
	}
	resp.Body = &teeBody{rc: resp.Body, rec: rec}
	return resp, nil
}

// errText 归一化错误文本（nil → "unknown"），避免 panic 且落盘可读。
func errText(err error) string {
	if err == nil {
		return "unknown"
	}
	return err.Error()
}

// newRespRecorder 建目录并创建落盘文件。任何失败返回「空转」recorder（active=false），
// 调试工具不得干扰主流程。文件此时即为空文件，正文由 tee 边收边写。
func newRespRecorder(channel string, req *http.Request, reqBody []byte, start time.Time) *respRecorder {
	r := &respRecorder{
		start: start, channel: channel, req: req, reqBody: reqBody,
		tail: make([]byte, 0, 2*tailKeep),
	}
	sub := filepath.Join(dir(), channel)
	if os.MkdirAll(sub, 0o755) != nil {
		return r
	}
	f, ferr := os.OpenFile(filepath.Join(sub, dumpName(req, int(seq.Add(1)))), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if ferr != nil {
		return r
	}
	r.f = f
	r.w = bufio.NewWriterSize(f, dumpBufSize)
	r.active = true
	return r
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

// dumpBufSize 落盘写缓冲：上游每来一小帧只写进内存，攒满此值才落一次盘
// （延迟写，**不是**逐字节写盘）。放在 4 MiB 的 maxBody 下限之下，
// 典型几百 KB 的对话响应只需几次系统调用。
const dumpBufSize = 64 << 10

// bodyCap 写入文件正文的字节上限。与旧 truncateBody 的「前 maxBody-tailKeep + 后 tailKeep」
// 等效：正文流式写入到 bodyCap，收尾再补最近 tailKeep 字节，总量不超过 maxBody。
const bodyCap = maxBody - tailKeep

// respRecorder 以 tee 方式把响应体**边转发边落盘**：不缓冲整个响应（否则 SSE 会被攒批成
// 「上游流完之后一次性到达」，见 RoundTrip 注释），只在内存保留最近 tailKeep 字节供收尾
// 计算尾部与 usage 速览（usage 只在末帧）。
type respRecorder struct {
	mu sync.Mutex

	f      *os.File
	w      *bufio.Writer
	active bool // 文件是否可用（建目录/建文件失败时为 false，全部操作空转）

	start   time.Time
	channel string
	req     *http.Request
	reqBody []byte
	status  int
	header  http.Header
	hasResp bool

	headerWritten bool
	closed        bool

	tail    []byte // 最近 tailKeep 字节
	written int64  // 已写入文件的响应体字节数（≤ bodyCap）
	total   int64  // 已转发的响应体字节总数
	capped  bool   // 达 bodyCap 后正文不再写文件
	readErr error  // 读响应体错误（EOF 除外）
}

// newRespRecorder 见上；此处仅保留构造函数体在 newRespRecorder 中。

// setResp 记录响应状态与头（写文件头时需要）。
func (r *respRecorder) setResp(status int, h http.Header) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status, r.header, r.hasResp = status, h, true
}

// fail 记录传输错误（resp 为 nil 或 RoundTrip 报错时）。
func (r *respRecorder) fail(msg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.readErr = errors.New(strings.TrimRight(msg, "\n"))
}

// Write 由转发路径调用：写文件并维护尾部窗口，恒返回 (len(p), nil)。
func (r *respRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.active {
		return len(p), nil
	}
	r.writeHeadLocked()
	r.total += int64(len(p))

	// 尾部窗口：一次裁到 tailKeep（每写满一个窗口才复制一次，避免逐帧复制的 O(n²)）。
	r.tail = append(r.tail, p...)
	if len(r.tail) > 2*tailKeep {
		nt := make([]byte, tailKeep)
		copy(nt, r.tail[len(r.tail)-tailKeep:])
		r.tail = nt
	}

	// 正文：只写到 bodyCap，超出部分仅保留在尾部窗口里（收尾补写）。
	if !r.capped {
		room := bodyCap - int(r.written)
		if room < 0 {
			room = 0
		}
		n := len(p)
		if n > room {
			n = room
		}
		if n > 0 {
			_, _ = r.w.Write(p[:n])
			r.written += int64(n)
		}
		if r.written >= bodyCap {
			r.capped = true
		}
	}
	return len(p), nil
}

// writeHeadLocked 首字节（或收尾兜底）时写文件头：摘要行 + 请求块 + 响应头。
func (r *respRecorder) writeHeadLocked() {
	if r.headerWritten {
		return
	}
	r.headerWritten = true
	fmt.Fprintf(r.w, "# channel=%s status=%d ttfb=%s\n", r.channel, r.status, time.Since(r.start).Round(time.Millisecond))
	fmt.Fprintf(r.w, "### REQUEST %s %s\n", r.req.Method, r.req.URL.String())
	writeHeaderBlock(r.w, r.req.Header)
	_, _ = r.w.WriteString("\n")
	_, _ = r.w.Write(truncateBody(r.reqBody))
	_, _ = r.w.WriteString("\n\n")
	if !r.hasResp {
		_, _ = r.w.WriteString("### TRANSPORT ERROR\n")
		return
	}
	fmt.Fprintf(r.w, "### RESPONSE %d\n", r.status)
	writeHeaderBlock(r.w, r.header)
	_, _ = r.w.WriteString("\n")
}

// close 收尾：补尾部窗口 + 写汇总行（duration/usage），随后刷盘。
// 幂等；任何失败静默（调试工具不得干扰主流程），恒返回 nil 以免影响转发路径。
func (r *respRecorder) close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if !r.active {
		return nil
	}
	r.writeHeadLocked() // 未读到任何字节时也要留下请求块
	if r.readErr != nil {
		fmt.Fprintf(r.w, "%s\n", r.readErr)
	}
	if r.total > bodyCap {
		if r.total > maxBody {
			_, _ = r.w.WriteString("\n<<...omitted...>>\n")
		}
		_, _ = r.w.Write(r.tail)
	}
	_, _ = r.w.WriteString("\n")
	fmt.Fprintf(r.w, "# duration=%s usage=%s\n", time.Since(r.start).Round(time.Millisecond), usageHint(r.tail))
	_ = r.w.Flush()
	_ = r.f.Close()
	return nil
}

// teeBody 包装上游响应体：读取原样转发（调用方逐帧拿到并即时 flush，不受落盘影响），
// 同时把字节喂给 recorder。EOF/错误时收尾落盘。
type teeBody struct {
	rc  io.ReadCloser
	rec *respRecorder
}

func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		_, _ = t.rec.Write(p[:n])
	}
	if err != nil {
		if err != io.EOF {
			t.rec.mu.Lock()
			t.rec.readErr = err
			t.rec.mu.Unlock()
		}
		_ = t.rec.close()
	}
	return n, err
}

func (t *teeBody) Close() error {
	_ = t.rec.close()
	return t.rc.Close()
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

// writeHeaderBlock 写 HTTP 头（keys 排序，便于跨请求 diff）。w 可为 *bytes.Buffer 或 *bufio.Writer。
func writeHeaderBlock(w io.Writer, h http.Header) {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range h[k] {
			fmt.Fprintf(w, "%s: %s\n", k, v)
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
