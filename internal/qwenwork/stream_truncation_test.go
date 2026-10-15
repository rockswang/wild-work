package qwenwork

import (
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// 回归（issue #42）：千问办公的流（envelope 包裹）被「可证实地」截断时，出口必须发
// error 帧且**不补** [DONE]，不能把半截流伪装成正常收尾。
//
// 修复前 streamAsOpenAI 的 sawDone 是死变量，于是连接中断/读超时被掐断与断在半个帧
// 上两种情况都会照常补 data: [DONE] → 客户端拿着半截 tool_call arguments 报
// "tool input was not fully received"，网关日志却无错误。
func TestStreamTruncationNotDisguisedAsDone(t *testing.T) {
	// envelope 写法：{body: "<inner json>"}，inner json 含 content 与一个未收尾的 tool_call。
	framef := func(inner string) string { return `data: {"body":"` + inner + `"}` + "\n\n" }
	content := `{"id":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}`
	toolPartial := `{"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash","arguments":"{\"cmd\""}}]}}]}`
	dones := "data: {\"body\":\"[DONE]\"}\n\n"
	full := framef(content) + framef(toolPartial)

	cases := []struct {
		name       string
		rd         io.Reader
		wantErr    bool
		wantDone   bool
		wantErrFrm bool
	}{
		{"传输层读错误", &qwReadErr{payload: full}, true, false, true},
		{"断在半个帧上", &qwCut{payload: full, keep: len(full) - 10}, true, false, true},
		{"正常 [DONE] 收尾", strings.NewReader(full + dones), false, true, false},
		{"EOF 但上游漏发 [DONE]", strings.NewReader(full), false, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			_, err := Stream(rec, c.rd, "m")
			body := rec.Body.String()
			if (err != nil) != c.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, c.wantErr)
			}
			if got := strings.Contains(body, "[DONE]"); got != c.wantDone {
				t.Errorf("[DONE]=%v want %v（截断不得伪装成正常收尾）\nbody=%s", got, c.wantDone, body)
			}
			if got := (strings.Contains(body, `"code":"upstream_stream_error"`) || strings.Contains(body, `"code":"upstream_timeout"`) || strings.Contains(body, `"code":"upstream_truncated"`)); got != c.wantErrFrm {
				t.Errorf("error frame=%v want %v\nbody=%s", got, c.wantErrFrm, body)
			}
		})
	}
}

type qwReadErr struct {
	payload string
	sent    bool
}

func (r *qwReadErr) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.payload), nil
	}
	return 0, fmt.Errorf("simulated: connection reset")
}

type qwCut struct {
	payload   string
	keep, off int
}

func (r *qwCut) Read(p []byte) (int, error) {
	if r.off >= r.keep {
		return 0, io.EOF
	}
	n := copy(p, r.payload[r.off:r.keep])
	r.off += n
	return n, nil
}
