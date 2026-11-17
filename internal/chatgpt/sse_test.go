package chatgpt

import (
	"encoding/json"
	"strings"
	"testing"
)

// 上游流的两种 patch 形态都必须接（票 03 验收：只认 `o:"patch"` 会静默丢帧）。

func applyFrame(t *testing.T, st *turnState, raw string) {
	t.Helper()
	var ev map[string]any
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("测试帧不是合法 JSON: %v (%s)", err, raw)
	}
	st.apply(ev)
}

// TestPatchTwoShapesBothApplied 带包裹与裸的 patch 都要生效。
func TestPatchTwoShapesBothApplied(t *testing.T) {
	st := &turnState{}
	applyFrame(t, st, `{"conversation_id":"conv-1","message":{"id":"m1","author":{"role":"assistant"},`+
		`"content":{"content_type":"text","parts":["Hello. What can"]},"recipient":"all","channel":"final"}}`)
	if st.Text != "Hello. What can" {
		t.Fatalf("完整消息未生效: %q", st.Text)
	}
	// 形态一：{"o":"patch","v":[…]}
	applyFrame(t, st, `{"o":"patch","v":[{"o":"append","p":"/message/content/parts/0","v":" I help"}]}`)
	if st.Text != "Hello. What can I help" {
		t.Fatalf("包裹形态未生效: %q", st.Text)
	}
	// 形态二：裸 {"v":[…]}（只认前者会在这里静默丢帧）
	applyFrame(t, st, `{"v":[{"o":"append","p":"/message/content/parts/0","v":" with?"}]}`)
	if st.Text != "Hello. What can I help with?" {
		t.Fatalf("裸形态未生效: %q", st.Text)
	}
}

// TestPatchBareShapeOnly 单独验证裸形态是一条独立代码路径（不靠包裹形态兜住）。
func TestPatchBareShapeOnly(t *testing.T) {
	st := &turnState{}
	applyFrame(t, st, `{"message":{"id":"m1","author":{"role":"assistant"},`+
		`"content":{"content_type":"text","parts":["A"]},"recipient":"all","channel":"final"}}`)
	applyFrame(t, st, `{"v":[{"o":"append","p":"/message/content/parts/0","v":"B"}]}`)
	if st.Text != "AB" {
		t.Fatalf("裸 patch 未生效: %q", st.Text)
	}
}

// TestOnlyVisibleAssistantEntersBody 隐藏消息 / 非 final 通道 / tool 通道都不进正文。
func TestOnlyVisibleAssistantEntersBody(t *testing.T) {
	st := &turnState{}
	applyFrame(t, st, `{"message":{"id":"hidden","author":{"role":"assistant"},`+
		`"is_visually_hidden_from_conversation":true,`+
		`"content":{"content_type":"text","parts":["SECRET"]},"recipient":"all","channel":"final"}}`)
	applyFrame(t, st, `{"message":{"id":"tool","author":{"role":"assistant"},`+
		`"content":{"content_type":"text","parts":["TOOLTEXT"]},"recipient":"all","channel":"commentary"}}`)
	applyFrame(t, st, `{"message":{"id":"web","author":{"role":"assistant"},`+
		`"content":{"content_type":"text","parts":["WEBRUN"]},"recipient":"web.run","channel":"final"}}`)
	applyFrame(t, st, `{"message":{"id":"user","author":{"role":"user"},`+
		`"content":{"content_type":"text","parts":["USER"]},"recipient":"all","channel":"final"}}`)
	if st.Text != "" {
		t.Fatalf("可见性过滤漏了：%q", st.Text)
	}
	applyFrame(t, st, `{"message":{"id":"m2","author":{"role":"assistant"},`+
		`"content":{"content_type":"text","parts":["OK"]},"recipient":"all","channel":"final"}}`)
	if st.Text != "OK" {
		t.Fatalf("可见正文被过滤掉了: %q", st.Text)
	}
}

// TestThoughtsAccumulateSeparately 思考链走 content_type="thoughts"，不污染正文。
func TestThoughtsAccumulateSeparately(t *testing.T) {
	st := &turnState{}
	applyFrame(t, st, `{"message":{"id":"t1","author":{"role":"assistant"},`+
		`"content":{"content_type":"thoughts","thoughts":[{"content":"先想一步"}]}}}`)
	if st.Text != "" {
		t.Fatalf("思考漏进正文: %q", st.Text)
	}
	applyFrame(t, st, `{"v":[{"o":"append","p":"/message/content/thoughts","v":"再想一步"}]}`)
	if st.Reason != "先想一步\n再想一步" {
		t.Fatalf("思考链 = %q", st.Reason)
	}
	applyFrame(t, st, `{"message":{"id":"m1","author":{"role":"assistant"},`+
		`"content":{"content_type":"text","parts":["答案"]},"recipient":"all","channel":"final"}}`)
	if st.Text != "答案" || st.Reason != "先想一步\n再想一步" {
		t.Fatalf("正文/思考串台: text=%q reason=%q", st.Text, st.Reason)
	}
}

// TestTurnStateAnchors 会话 id 首帧非空者胜；AssistantID 取最后一个可见 assistant 消息。
func TestTurnStateAnchors(t *testing.T) {
	st := &turnState{}
	applyFrame(t, st, `{"conversation_id":"conv-1","message":{"id":"m1","author":{"role":"assistant"},`+
		`"content":{"content_type":"text","parts":["A"]},"recipient":"all","channel":"final"}}`)
	applyFrame(t, st, `{"conversation_id":"conv-1","v":[{"p":"/message/content/parts/0","o":"append","v":"B"}]}`)
	applyFrame(t, st, `{"v":[{"p":"/message/id","o":"replace","v":"m2"}]}`)
	if st.ConvID != "conv-1" {
		t.Fatalf("ConvID = %q", st.ConvID)
	}
	if st.Text != "AB" {
		t.Fatalf("Text = %q", st.Text)
	}
}

// TestApplyDelta 增量差分：前缀扩展取增量；替换补公共前缀之后的尾巴；变小不发。
func TestApplyDelta(t *testing.T) {
	cases := []struct {
		name       string
		prev, next string
		want       string
		wantOK     bool
	}{
		{"前缀扩展", "abc", "abcdef", "def", true},
		{"无变化", "abc", "abc", "", false},
		{"替换变长", "abc", "abXYcdef", "XYcdef", true},
		{"替换变短", "abcdef", "abc", "", false},
		{"首帧", "", "hi", "hi", true},
	}
	for _, c := range cases {
		got, ok := applyDelta(c.prev, c.next)
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: applyDelta(%q,%q) = (%q,%v)，期望 (%q,%v)",
				c.name, c.prev, c.next, got, ok, c.want, c.wantOK)
		}
	}
}

// TestScanSSEBothDataPrefixes 入站帧解析必须同时认 `data: ` 与 `data:`（LobsterAI 事故同款）。
func TestScanSSEBothDataPrefixes(t *testing.T) {
	var got []string
	err := scanSSE(strings.NewReader("data: {\"a\":1}\n\ndata:{\"b\":2}\n\n"), func(p string) error {
		got = append(got, p)
		return nil
	})
	if err != nil {
		t.Fatalf("scanSSE: %v", err)
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"b":2}` {
		t.Fatalf("载荷 = %v", got)
	}
}