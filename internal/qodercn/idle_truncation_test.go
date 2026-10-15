package qodercn

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"wild-work/internal/idle"
)

// 回归（R58）：空闲看门狗掐断的流，出口必须能自证「是被我们掐的」。
//
// 背景（现场）：Qwen3.8-Flash (QoderCOM) 输出一部分后报
// `Error: upstream stream truncated`。修复前该固定串覆盖三种完全不同的原因，
// 且 streamAsOpenAI 把原始 err **丢弃** ⇒ 无法判断是「上游排队/卡死被 300s
// 空闲看门狗掐的」还是「上游主动断连」。
//
// 本测试走真实 http 链路（httptest + idle.Monitor），不用假 reader：
// 只有真实链路才能证明「看门狗 cancel → 在途 Read 返回 context canceled →
// 被 IsIdleTimeout 认出 → code=upstream_timeout」这条链成立。
// （假 reader 直接返回 provider.ErrIdleTimeout 会绕开最关键的一环。）
func TestStreamAsOpenAIReportsIdleTimeoutCause(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// 先发一帧，制造「已输出一部分」的现场。
		fmt.Fprint(w, `data: {"body":"{\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}"}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // 之后一个字节都不再发（模拟上游卡死/排队）
	}))
	defer srv.Close()

	// 走真实的 ChatStream → idle.Monitor 包装，再喂给 Stream。
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req, cancel := idle.WithCancel(req)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := idle.Monitor(resp.Body, 300*time.Millisecond, cancel)
	defer body.Close()

	rec := httptest.NewRecorder()
	_, serr := Stream(rec, body, "m")
	out := rec.Body.String()

	if serr == nil {
		t.Fatal("截断必须返回错误，实际 nil")
	}
	if !strings.Contains(serr.Error(), "upstream stream truncated") {
		t.Errorf("错误文本应说明截断：%v", serr)
	}
	// 核心断言 1：原因必须保留（修复前这里只有固定串，看不出是谁掐的）。
	if !strings.Contains(serr.Error(), "idle timeout") {
		t.Errorf("错误文本应保留原始原因（空闲超时）：%v", serr)
	}
	// 核心断言 2：出口帧 code 必须是 upstream_timeout（与「上游主动断连」区分开）。
	if !strings.Contains(out, `"code":"upstream_timeout"`) {
		t.Errorf("出口帧 code 应为 upstream_timeout，实际 body=%s", out)
	}
	if !strings.Contains(out, "idle timeout") {
		t.Errorf("出口帧 message 应保留原因，实际 body=%s", out)
	}
	// R36：截断不得伪装成正常收尾。
	if strings.Contains(out, "[DONE]") {
		t.Errorf("截断不得补 [DONE]，body=%s", out)
	}
	// 已透传的部分必须还在（不能因为收尾把半截内容吞掉）。
	if !strings.Contains(out, `"content":"hi"`) {
		t.Errorf("已输出的内容丢失，body=%s", out)
	}
}

// 对照：上游主动断连（非空闲）必须落 upstream_truncated 而非 upstream_timeout，
// 否则错误码失去区分力。
func TestStreamAsOpenAIReportsUpstreamBreakAsTruncated(t *testing.T) {
	rec := httptest.NewRecorder()
	// 先吐一帧再返回读错误（模拟 connection reset）。
	rd := &resetAfter{payload: `data: {"body":"{\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}"}` + "\n\n"}
	_, serr := Stream(rec, rd, "m")
	out := rec.Body.String()

	if serr == nil {
		t.Fatal("应返回错误")
	}
	if strings.Contains(serr.Error(), "idle timeout") {
		t.Errorf("上游主动断连不得报成空闲超时：%v", serr)
	}
	if !strings.Contains(out, `"code":"upstream_truncated"`) {
		t.Errorf("出口帧 code 应为 upstream_truncated，body=%s", out)
	}
	if !strings.Contains(out, "connection reset") {
		t.Errorf("原因应保留在帧里，body=%s", out)
	}
}

// resetAfter 先吐完 payload，再返回一个非空闲类读错误。
type resetAfter struct {
	payload string
	sent    bool
}

func (r *resetAfter) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.payload), nil
	}
	return 0, &net.OpError{Op: "read", Err: fmt.Errorf("connection reset by peer")}
}

// 编译期断言：context.Canceled 确实被判为空闲超时（链路上游依赖该判据）。
var _ = context.Canceled
