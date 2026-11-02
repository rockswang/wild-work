// 流式成功出口必须把 completion token 摊进滑动窗口；非流式不得产生速率样本。
// 动机：旧口径用「总耗时 − TTFB」作分母，把排队/prefill 整段抹掉，
// 实测小浣熊单请求夸大 3.56 倍（docs/adr/accepted/r132-*.md）。
package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/stats"
)

// tputUpstream 流式假上游：写 3 帧 content + 末帧 usage（completion=90）。
// 帧走真实 Write 路径（statWriter 记账），故 sw.chunks 会采到 1 个秒桶。
type tputUpstream struct {
	acctUpstream
}

func (u *tputUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return io.NopCloser(strings.NewReader("x")), http.StatusOK, nil, nil
}

func (u *tputUpstream) Stream(w http.ResponseWriter, _ io.Reader, _ string) (map[string]any, error) {
	for i := 0; i < 3; i++ {
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`+"\n\n")
	}
	_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":90}}`+"\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	return map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(90)}, nil
}

// 流式成功 ⇒ 该模型出现窗口速率（90 token 落在 1 个秒桶 → 90/60 = 1.5 tok/s）；
// 请求日志单行 tok/s 必须是端到端口径（token ÷ 总耗时），不是生成期比值。
func TestStreamTputBookedIntoWindow(t *testing.T) {
	st, err := stats.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h, _, _ := newFbHandler(t, &tputUpstream{}, &tputUpstream{}, nil)
	h.stats = st

	callModelStream(h, "oczen/big-pickle")

	var got float64
	var win int
	for _, m := range st.Snapshot(time.Now()).Models {
		if m.Model == "oczen/big-pickle" {
			got, win = m.TokPerSec, m.TputWindowSec
		}
	}
	if win != 60 {
		t.Fatalf("窗口长度应回传 60 秒，实际 %d", win)
	}
	// 90 token 落在 1 个秒桶 → 90/60 = 1.5 tok/s（窗口是 60 秒，不是该请求的时长）
	if got != 1.5 {
		t.Fatalf("窗口速率 = %v, want 1.5（90 token ÷ 60 秒窗口）", got)
	}

	rows := st.Logs(5)
	if len(rows) != 1 {
		t.Fatalf("应有 1 条请求日志，实际 %d", len(rows))
	}
	r := rows[0]
	if r.Tok != 90 {
		t.Fatalf("请求日志 tok = %d, want 90", r.Tok)
	}
	if r.TotalSec <= 0 {
		t.Fatalf("请求日志 total_sec 应 > 0，实际 %v", r.TotalSec)
	}
	if want := 90 / r.TotalSec; r.TokPerSec < want*0.9 || r.TokPerSec > want*1.1 {
		t.Fatalf("请求日志 tok/s = %v，应≈端到端 %v（不是生成期比值）", r.TokPerSec, want)
	}
}

// 非流式不产生吞吐样本（一次性写回没有分帧意义）。
func TestNonStreamNoTputSample(t *testing.T) {
	st, err := stats.New(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	h, _, _ := newFbHandler(t, &acctUpstream{}, &acctUpstream{}, nil)
	h.stats = st

	callModel(h, "oczen/big-pickle")

	ms := st.Snapshot(time.Now()).Models
	if len(ms) == 0 {
		t.Fatal("非流式仍应记消耗（只是无速率样本）")
	}
	for _, m := range ms {
		if m.Model == "oczen/big-pickle" && m.TokPerSec != 0 {
			t.Fatalf("非流式不应有窗口速率: %v", m.TokPerSec)
		}
	}
}
