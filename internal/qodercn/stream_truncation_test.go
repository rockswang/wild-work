package qodercn

import (
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// 回归（issue #42）：流被「可证实地」截断时，出口必须发 error 帧且**不补** [DONE]。
//
// 修复前 streamAsOpenAI 的 sawDone 是死变量（声明后从不赋值），于是：
//   - 上游连接中断 / 读超时被掐断 → 出口照样补 data: [DONE]，客户端以为流正常结束，
//     实际 tool_call arguments 是半截 JSON，报 "tool input was not fully received"，
//     而网关日志里没有任何错误；
//   - 断在半个帧上（帧未以换行收尾）同样被伪装成成功。
//
// 修复后：传输层错误与半帧 → 发 error 帧（错误码按成因细分）且不发 [DONE]；
// 正常收尾（含上游漏发 [DONE] 的兜底）→ 行为不变，仍恰好一个 [DONE]。
func TestStreamTruncationNotDisguisedAsDone(t *testing.T) {
	chunk := func(s string) string { return `data: {"body":"` + s + `"}` + "\n\n" }
	full := chunk(`{\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}`) +
		chunk(`{\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"Bash\",\"arguments\":\"{\\\"cmd\\\"`)

	cases := []struct {
		name       string
		rd         io.Reader
		wantErr    bool
		wantDone   bool
		wantErrFrm bool
	}{
		{
			name:     "传输层读错误（连接中断/超时）",
			rd:       &readErrAfter{payload: full},
			wantErr:  true,
			wantDone: false, wantErrFrm: true,
		},
		{
			name:     "断在半个帧上（末行无换行）",
			rd:       &cutReader{payload: full, keep: len(full) - 12},
			wantErr:  true,
			wantDone: false, wantErrFrm: true,
		},
		{
			name:     "正常 [DONE] 收尾",
			rd:       strings.NewReader(full + chunk("[DONE]")),
			wantErr:  false,
			wantDone: true, wantErrFrm: false,
		},
		{
			name:     "EOF 但末帧完整、上游漏发 [DONE]",
			rd:       strings.NewReader(full),
			wantErr:  false,
			wantDone: true, wantErrFrm: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			_, err := StreamCapture(rec, c.rd, "m", nil)
			body := rec.Body.String()
			if got := err != nil; got != c.wantErr {
				t.Errorf("err=%v, wantErr=%v", err, c.wantErr)
			}
			if !c.wantErr {
				return
			}
			if got := strings.Contains(body, "[DONE]"); got != c.wantDone {
				t.Errorf("[DONE] present=%v, want %v（截断不得伪装成正常收尾）\nbody=%s", got, c.wantDone, body)
			}
			if got := (strings.Contains(body, `"code":"upstream_stream_error"`) || strings.Contains(body, `"code":"upstream_timeout"`) || strings.Contains(body, `"code":"upstream_truncated"`)); got != c.wantErrFrm {
				t.Errorf("error frame present=%v, want %v\nbody=%s", got, c.wantErrFrm, body)
			}
			if !errors.Is(err, errStreamTruncatedSentinel()) && !strings.Contains(err.Error(), "truncated") {
				t.Errorf("unexpected err: %v", err)
			}
		})
	}
}

// errStreamTruncatedSentinel 仅为让上面断言写法统一（实现返回的是 errors.New 字面量）。
func errStreamTruncatedSentinel() error { return errors.New("upstream stream truncated") }

// readErrAfter 先吐完 payload，再返回一个读错误（模拟上游连接被掐断 / 读超时）。
type readErrAfter struct {
	payload string
	sent    bool
}

func (r *readErrAfter) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		n := copy(p, r.payload)
		return n, nil
	}
	return 0, fmt.Errorf("simulated: connection reset by peer")
}

// cutReader 只吐 payload 的前 keep 字节后干净 EOF（模拟断在半个帧上）。
type cutReader struct {
	payload string
	keep    int
	off     int
}

func (r *cutReader) Read(p []byte) (int, error) {
	if r.off >= r.keep {
		return 0, io.EOF
	}
	n := copy(p, r.payload[r.off:r.keep])
	r.off += n
	return n, nil
}
