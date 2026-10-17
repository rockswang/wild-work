// bodytrace.go 请求体构成摘要（诊断用，默认关闭）。
//
// 这是本包唯一的「例外」：usage/credit 流水只记 uid/模型名/token 数（§6-2 红线，
// 绝不落 token 凭证）；本文件为定位「上下文为何膨胀」而临时开启，会写入消息内容
// 的前 200 字预览，因此：
//   - 独立成 body-trace-<会话>.jsonl，与正式流水完全分离；
//   - 只有 config.body_log=true 时才写（默认关，见 SetBodyTrace）；
//   - 只记摘要不记全文（条数/角色分布/体量 Top10），单分片超 8MB 滚动到 .1/.2…，
//     分片按文件修改时间保留 7 天。
//
// 会话切片：OpenAI 请求体不带会话 ID，故用「首条 user 消息文本」的 SHA-1 前 10 位
// 当会话指纹——同一会话内开场问题稳定不变，不同会话（不同任务）互不相同。
// 一个会话一个文件（过大则滚动分片），文件名形如 body-trace-a1b2c3d4e5.jsonl。
//
// 用途：回答「那 10 万 token 到底是什么撑大的」——按消息体量排序取 Top10，
// 每条带开头 200 字，一眼看出是哪个文件/哪段历史把上下文撑起来的。
package ledger

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// bodyTraceOn 诊断开关（config.body_log），进程内全局生效。
var bodyTraceOn atomic.Bool

// btClean 上次清理过期分片的时间戳（AutoFlush 节流：每小时扫一次）。
var btClean atomic.Int64

// SetBodyTrace 开关请求体摘要记录（App 启动与面板改配置时调用）。
func SetBodyTrace(on bool) { bodyTraceOn.Store(on) }

// BodyTraceEnabled 当前是否开启。
func BodyTraceEnabled() bool { return bodyTraceOn.Load() }

const (
	// previewChars 单个内容块的预览字符数上限。
	previewChars = 200
	// topBlocks 摘要里保留的最大内容块条数。
	topBlocks = 10
	// bodyTraceMaxBytes 单分片上限，超出滚动到 .1/.2…（避免单文件无限膨胀）。
	bodyTraceMaxBytes = 8 << 20
	// bodyTraceKeepDays 分片保留天数（按文件修改时间清理）。
	bodyTraceKeepDays = 7
	// bodyTraceNoSess 无会话指纹时的兜底 key（固定值，避免每请求各生成一个文件）。
	bodyTraceNoSess = "0000000000"
)

// TraceBlock 单个内容块的体量与预览。
type TraceBlock struct {
	Idx     int    `json:"i"`       // 消息在 messages 中的下标
	Role    string `json:"role"`    // 角色（user/assistant/system/tool...）
	Chars   int    `json:"chars"`   // 文本字符数（rune 计）
	Preview string `json:"preview"` // 开头 200 字（换行折叠为空格）
}

// BodyTraceEntry 单次请求的上下文构成摘要。
type BodyTraceEntry struct {
	Ts       int64          `json:"ts"`
	Head     string         `json:"head"`      // 会话开场（首条 user 消息前 200 字），用于辨认文件
	Ch       string         `json:"ch"`        // 渠道 kind
	Model    string         `json:"model"`     // 客户端请求的原始模型名
	Bytes    int            `json:"bytes"`     // 请求体字节数
	Msgs     int            `json:"msgs"`      // messages 条数
	Roles    map[string]int `json:"roles"`     // 角色 → 条数
	TextChar int            `json:"text_char"` // 全部消息文本字符数合计
	SysChar  int            `json:"sys_char"`  // system/developer 提示字符数合计
	ToolsLen int            `json:"tools_len"` // tools 定义字符数（0 = 未携带）
	Top      []TraceBlock   `json:"top"`       // 体量最大的若干内容块（降序）
}

// TraceBody 记录一次请求的上下文构成摘要；未开启时立即返回（近乎零开销）。
// ch/model 由调用方传入（此时尚未选账号，故不含 uid）。
func (l *Ledger) TraceBody(ch, model string, raw []byte) {
	if !bodyTraceOn.Load() {
		return
	}
	e, key := summarizeBody(ch, model, raw)
	line, err := json.Marshal(e)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.currentBodyTraceFile(key), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// currentBodyTraceFile 该会话当前应写入的分片：取已存在的最大序号分片，未超上限则续写，
// 超限则新建下一个序号（调用方须持有 l.mu）。
func (l *Ledger) currentBodyTraceFile(key string) string {
	base := filepath.Join(l.dir, bodyTraceName(key, 0))
	st, err := os.Stat(base)
	if err != nil || st.Size() <= bodyTraceMaxBytes {
		return base // 不存在或未超限 → 写 base
	}
	max := 0
	if ents, err := os.ReadDir(l.dir); err == nil {
		prefix := "body-trace-" + key + "."
		for _, ent := range ents {
			name := ent.Name()
			if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			mid := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".jsonl")
			if n, err := strconv.Atoi(mid); err == nil && n > max {
				max = n
			}
		}
	}
	if max > 0 {
		cur := filepath.Join(l.dir, bodyTraceName(key, max))
		if st, err := os.Stat(cur); err == nil && st.Size() <= bodyTraceMaxBytes {
			return cur
		}
	}
	return filepath.Join(l.dir, bodyTraceName(key, max+1))
}

// bodyTraceName 会话分片文件名：seq<=0 → body-trace-<key>.jsonl，否则 body-trace-<key>.<seq>.jsonl。
func bodyTraceName(key string, seq int) string {
	if seq <= 0 {
		return "body-trace-" + key + ".jsonl"
	}
	return "body-trace-" + key + "." + strconv.Itoa(seq) + ".jsonl"
}

// cleanupBodyTrace 删除超过保留期的会话分片（按文件修改时间）。启动与定时调用。
func (l *Ledger) cleanupBodyTrace() {
	cut := time.Now().AddDate(0, 0, -bodyTraceKeepDays)
	ents, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	for _, ent := range ents {
		name := ent.Name()
		if !strings.HasPrefix(name, "body-trace-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cut) {
			_ = os.Remove(filepath.Join(l.dir, name))
		}
	}
}

// summarizeBody 解析 OpenAI 风格请求体，产出构成摘要与会话指纹 key。
// 解析失败时只保留字节数——诊断工具不能因畸形请求报错或阻塞主链路。
func summarizeBody(ch, model string, raw []byte) (BodyTraceEntry, string) {
	e := BodyTraceEntry{
		Ts:    time.Now().Unix(),
		Ch:    ch,
		Model: model,
		Bytes: len(raw),
		Roles: map[string]int{},
	}
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return e, bodyTraceNoSess
	}
	e.Msgs = len(req.Messages)
	blocks := make([]TraceBlock, 0, len(req.Messages))
	seed := ""
	for i, m := range req.Messages {
		e.Roles[m.Role]++
		text := extractText(m.Content)
		n := len([]rune(text))
		e.TextChar += n
		if m.Role == "system" || m.Role == "developer" {
			e.SysChar += n
		}
		if seed == "" && m.Role == "user" {
			seed, e.Head = text, preview(text)
		}
		blocks = append(blocks, TraceBlock{Idx: i, Role: m.Role, Chars: n, Preview: preview(text)})
	}
	if seed == "" && len(req.Messages) > 0 {
		// 无 user 消息（少见）：退化为首条消息当会话指纹
		seed = extractText(req.Messages[0].Content)
		e.Head = preview(seed)
	}
	if len(req.Tools) > 0 && string(req.Tools) != "null" {
		e.ToolsLen = len([]rune(string(req.Tools)))
	}
	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].Chars > blocks[j].Chars })
	if len(blocks) > topBlocks {
		blocks = blocks[:topBlocks]
	}
	e.Top = blocks
	return e, sessionKey(seed)
}

// sessionKey 会话指纹：首条 user 消息文本的 SHA-1 前 10 位十六进制。
func sessionKey(seed string) string {
	if seed == "" {
		return bodyTraceNoSess
	}
	sum := sha1.Sum([]byte(seed))
	return hex.EncodeToString(sum[:])[:10]
}

// extractText 提取消息文本：content 可能是字符串，也可能是分片数组（多模态/工具结果）。
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return string(raw)
}

// preview 取前 previewChars 个字符做预览，换行折叠为空格便于单行展示。
func preview(s string) string {
	r := []rune(strings.NewReplacer("\n", " ", "\r", " ", "\t", " ").Replace(s))
	if len(r) > previewChars {
		return string(r[:previewChars]) + "…"
	}
	return string(r)
}
