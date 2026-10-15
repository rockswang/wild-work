// dump.go 提供 env 门控的请求体落盘（诊断上游拒答类故障用）。
// 默认关闭：仅当 WILDWORK_DUMP_DIR 非空时生效，零开销差异只在开启时存在。
// 两个落盘点：
//   - 客户端入站 body（dumpChatBody，stage="client"）：prepareChatBody 之前的
//     客户端原始请求，可直接重放回 /v1/chat/completions；
//   - 上游失败对（dumpUpstreamFailure）：全链路改写后真正发给上游的出站 body
//     + 上游错误响应原文，仅在上游返回 4xx/5xx 时落盘，与入站 dump 共用序号。
package server

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	dumpOnce sync.Once
	dumpDir  string
	dumpMu   sync.Mutex // 保护 dumpSeq：并发请求下序号必须唯一，否则失败对会互相覆盖
	dumpSeq  int
)

// dumpEnabled 首次调用读取环境变量；之后恒定。
func dumpEnabled() (string, bool) {
	dumpOnce.Do(func() { dumpDir = strings.TrimSpace(os.Getenv("WILDWORK_DUMP_DIR")) })
	return dumpDir, dumpDir != ""
}

// dumpChatBody 把**客户端原始**请求体（prepareChatBody 之前，可直接重放回
// /v1/chat/completions）写入 dump 目录，并在同目录 index.log 追加一行元数据
// （时间/序号/模型/字节数/stage），返回本次序号（未启用返回 0）。
// 放在改写前是刻意的：改写后的 body 已丢掉客户端原始字段（messageId/reasoning/
// traceId 等），拿它重放复现不了「客户端真实请求被上游拒绝」这类故障。
// 失败只打日志，绝不影响主链路。
func dumpChatBody(body []byte, model, stage string) int {
	dir, ok := dumpEnabled()
	if !ok {
		return 0
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("dump mkdir %s: %v", dir, err)
		return 0
	}
	dumpMu.Lock()
	dumpSeq++
	seq := dumpSeq
	dumpMu.Unlock()
	name := dumpName(dir, seq, model, stage)
	if err := os.WriteFile(name, body, 0o600); err != nil {
		log.Printf("dump write %s: %v", name, err)
		return seq
	}
	dumpIndex(dir, "%s seq=%04d model=%s bytes=%d stage=%s\n",
		time.Now().Format("2006-01-02 15:04:05.000"), seq, model, len(body), stage)
	return seq
}

// dumpUpstreamFailure 在上游返回 4xx/5xx 时把「出站请求体 + 上游错误响应」成对
// 落盘（stage 分别为 outbound / resp<status>）。出站 body 是经 prepareChatBody
// 与渠道层改写后真正发给上游的形态——上游错误文本（如 Kimi 的「message at
// position N」）按它计数校验，入站 dump 不足以对照定位。seq 取同一请求
// dumpChatBody 的返回值，用于把入站/出站/响应三份文件关联起来；入站未落盘时
// 现取序号，保证失败对内部仍可配对。
func dumpUpstreamFailure(seq int, outBody, respBody []byte, model string, status int, platform, uid string) {
	dir, ok := dumpEnabled()
	if !ok {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Printf("dump mkdir %s: %v", dir, err)
		return
	}
	dumpMu.Lock()
	if seq == 0 {
		dumpSeq++
		seq = dumpSeq
	}
	dumpMu.Unlock()
	outName := dumpName(dir, seq, model, "outbound")
	if err := os.WriteFile(outName, outBody, 0o600); err != nil {
		log.Printf("dump write %s: %v", outName, err)
	}
	respName := dumpName(dir, seq, model, fmt.Sprintf("resp%d", status))
	if err := os.WriteFile(respName, respBody, 0o600); err != nil {
		log.Printf("dump write %s: %v", respName, err)
	}
	dumpIndex(dir, "%s seq=%04d model=%s status=%d platform=%s uid=%s out_bytes=%d resp_bytes=%d\n",
		time.Now().Format("2006-01-02 15:04:05.000"), seq, model, status, platform, uid, len(outBody), len(respBody))
}

// dumpName 生成 dump 文件名：<序号>_<时刻>_<模型>_<stage>.json（模型名做了
// 文件名字符白名单过滤）。
func dumpName(dir string, seq int, model, stage string) string {
	safe := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, model)
	return filepath.Join(dir, fmt.Sprintf("%04d_%s_%s_%s.json", seq, time.Now().Format("150405.000"), safe, stage))
}

// dumpIndex 向 dump 目录的 index.log 追加一行元数据；失败只打日志。
func dumpIndex(dir, format string, args ...any) {
	f, err := os.OpenFile(filepath.Join(dir, "index.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		log.Printf("dump index open: %v", err)
		return
	}
	fmt.Fprintf(f, format, args...)
	f.Close()
}
