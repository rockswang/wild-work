package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
)

// R58：截断原因必须可分辨。修复前 qoder 系四个渠道无论什么原因都只报固定串
// "upstream stream truncated"，原始 err 被丢弃 ⇒ 生产上分不清「空闲看门狗掐的」
// （连续无字节，疑似上游排队/卡死）与「上游主动断连 / 读到半个帧」。
func TestIsIdleTimeoutRecognizesIdleWatchdogFamily(t *testing.T) {
	// internal/idle 看门狗唯一的动作是 cancel 请求 context，
	// 在途 Read 因此返回 context canceled（真实链路实测，非推断）。
	if !IsIdleTimeout(context.Canceled) {
		t.Error("context.Canceled 应判为空闲超时（idle 看门狗掐断的形态）")
	}
	if !IsIdleTimeout(context.DeadlineExceeded) {
		t.Error("context.DeadlineExceeded 应判为空闲超时")
	}
	if !IsIdleTimeout(net.ErrClosed) {
		t.Error("net.ErrClosed 应判为空闲超时（连接被连带关闭）")
	}
	if !IsIdleTimeout(ErrIdleTimeout) {
		t.Error("ErrIdleTimeout 应判为空闲超时（IdleReader 形态）")
	}
	// 对照：普通网络错误不得被判成空闲超时，否则出口错误码会误导。
	if IsIdleTimeout(errors.New("connection reset by peer")) {
		t.Error("普通读错误不应判为空闲超时")
	}
	if IsIdleTimeout(nil) {
		t.Error("nil 不应判为空闲超时")
	}
}

// 原始原因必须进错误文本与出口帧，且空闲超时另给 code=upstream_timeout。
func TestTruncationErrorAndFramePreserveCause(t *testing.T) {
	cases := []struct {
		name     string
		in       error
		wantCode string
		wantMsg  string
	}{
		{"空闲看门狗（context canceled）", context.Canceled, "upstream_timeout", "upstream stream idle timeout"},
		{"IdleReader", ErrIdleTimeout, "upstream_timeout", "upstream stream idle timeout"},
		{"上游主动断连", errors.New("unexpected EOF"), "upstream_truncated", "unexpected EOF"},
		{"原因缺失", nil, "upstream_truncated", "unknown cause"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			frame := TruncationFrame(c.in)
			if !strings.Contains(frame, `"code":"`+c.wantCode+`"`) {
				t.Errorf("code 不符：want %s，frame=%s", c.wantCode, frame)
			}
			if !strings.Contains(frame, c.wantMsg) {
				t.Errorf("message 未保留原因：want 含 %q，frame=%s", c.wantMsg, frame)
			}
			err := TruncationError(c.in)
			if !strings.Contains(err.Error(), "upstream stream truncated") {
				t.Errorf("错误文本应说明截断：%v", err)
			}
			// 空闲超时经包装后仍须可判（handler 用 errors.Is 判形态决定日志/错误码）。
			if got := IsIdleTimeout(err); got != (c.wantCode == "upstream_timeout") {
				t.Errorf("IsIdleTimeout(包装后)=%v，与 code=%s 不一致", got, c.wantCode)
			}
		})
	}
}

// 出口帧必须是**合法 JSON**：原因文本可能含双引号/反斜杠（上游错误原文），
// 字符串拼接会造出坏帧，客户端解析失败就只剩「无收尾的截断流」。
func TestTruncationFrameIsValidJSONWithHostileCause(t *testing.T) {
	nasty := errors.New(`bad "quote" and \backslash and 换行` + "\n")
	frame := TruncationFrame(nasty)
	var out struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(frame), &out); err != nil {
		t.Fatalf("帧不是合法 JSON: %v\nframe=%s", err, frame)
	}
	if !strings.Contains(out.Error.Message, "quote") {
		t.Errorf("message 丢内容: %q", out.Error.Message)
	}
}

// WriteTruncationFrame 写一帧且**不补 [DONE]**（R36：截断不得伪装成正常收尾）。
func TestWriteTruncationFrameNoDone(t *testing.T) {
	var sb strings.Builder
	if err := WriteTruncationFrame(&sb, context.Canceled); err != nil {
		t.Fatal(err)
	}
	got := sb.String()
	if !strings.HasPrefix(got, "data: ") || !strings.HasSuffix(got, "\n\n") {
		t.Errorf("SSE 帧格式不符: %q", got)
	}
	if strings.Contains(got, "[DONE]") {
		t.Errorf("截断帧不得含 [DONE]: %q", got)
	}
	if !strings.Contains(got, "upstream_timeout") {
		t.Errorf("缺少 upstream_timeout: %q", got)
	}
}
