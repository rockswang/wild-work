package chatgpt

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wild-work/internal/pool"
)

// 多轮续接（spec §9 的历史前缀哈希链）的接线测试：真的走 5 步链路，
// 断言「第二轮命中前缀指纹 → 带上游续接点」「未命中安静新建」「首句相同不串话」等验收条款。

// setResp 设置下一次对话端点返回的上游流（chatMock 自带 60 秒 conduit 假实现）。
func (m *chatMock) setResp(resp string) {
	m.mu.Lock()
	m.convResp = resp
	m.mu.Unlock()
}

// framesFor 造一段真实形状的上游流：首帧完整消息（带 conversation_id + 消息 id）。
func framesFor(convID, msgID, text string) string {
	m, _ := json.Marshal(map[string]any{
		"conversation_id": convID,
		"message": map[string]any{
			"id":      msgID,
			"author":  map[string]any{"role": "assistant"},
			"content": map[string]any{"content_type": "text", "parts": []string{text}},
		},
	})
	return "data: " + string(m) + "\n\n"
}

// turnResult 是一轮的结果：出站对话体 + 下发给客户端的正文。
type turnResult struct {
	body      map[string]any
	delivered string
}

// runStreamTurn 跑一轮（流式路径）并返回第 5 步的出站 body 与下发的正文。
func runStreamTurn(t *testing.T, c *Client, m *chatMock, frames, msgs string) turnResult {
	t.Helper()
	m.setResp(frames)
	body := []byte(`{"model":"auto","stream":true,"messages":` + msgs + `}`)
	rc, status, respBody, err := c.ChatStream(sentinelAuth(), body)
	if err != nil {
		t.Fatalf("ChatStream err=%v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, string(respBody))
	}
	defer rc.Close()
	rec := httptest.NewRecorder()
	if _, serr := c.Stream(rec, rc, "auto"); serr != nil {
		t.Fatalf("Stream err=%v", serr)
	}
	out, _ := m.lastConv()
	return turnResult{body: out, delivered: sseContent(rec.Body.String())}
}

// runAggregateTurn 跑一轮（非流式路径）：两条路径必须都落链（否则非流式客户端永远失忆）。
func runAggregateTurn(t *testing.T, c *Client, m *chatMock, frames, msgs string) turnResult {
	t.Helper()
	m.setResp(frames)
	body := []byte(`{"model":"auto","messages":` + msgs + `}`)
	rc, status, respBody, err := c.ChatStream(sentinelAuth(), body)
	if err != nil {
		t.Fatalf("ChatStream err=%v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, string(respBody))
	}
	defer rc.Close()
	resp, aerr := c.Aggregate(rc, "auto")
	if aerr != nil {
		t.Fatalf("Aggregate err=%v", aerr)
	}
	out, _ := m.lastConv()
	delivered := ""
	if chs, ok := resp["choices"].([]any); ok && len(chs) > 0 {
		if cm, ok := chs[0].(map[string]any); ok {
			if msg, ok := cm["message"].(map[string]any); ok {
				delivered, _ = msg["content"].(string)
			}
		}
	}
	return turnResult{body: out, delivered: delivered}
}

// sseContent 拼出 OpenAI SSE 帧里的 delta.content（= 客户端会重放的历史）。
func sseContent(sse string) string {
	var out strings.Builder
	for _, line := range strings.Split(sse, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if line == "" || line == "[DONE]" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		chs, _ := ev["choices"].([]any)
		for _, ch := range chs {
			cm, _ := ch.(map[string]any)
			d, _ := cm["delta"].(map[string]any)
			if s, ok := d["content"].(string); ok {
				out.WriteString(s)
			}
		}
	}
	return out.String()
}

// chainField 读一个字符串字段（缺字段 = 空串）。
func chainField(body map[string]any, key string) string {
	s, _ := body[key].(string)
	return s
}

// convBodyAt 取第 i 次对话请求的出站体（用于同一条链上的多次断言）。
func convBodyAt(t *testing.T, m *chatMock, i int) map[string]any {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= len(m.convBodies) {
		t.Fatalf("只有 %d 次对话请求，取不到第 %d 次", len(m.convBodies), i+1)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte(m.convBodies[i]), &body)
	return body
}

// TestConvChainResumesSecondTurn 验收①：第二轮命中前缀指纹 → 带上游续接点续聊。
//
// 同时锁死「键就是客户端下一轮会重放的那段历史」：本轮下发的正文必须原样被客户端
// 重放回来才能命中，所以先断言流式路径下发的正文，再用它拼第二轮请求。
func TestConvChainResumesSecondTurn(t *testing.T) {
	c, m := chatMockClient(t)

	t1 := runStreamTurn(t, c, m, framesFor("c-1", "m-1", "第一轮答复"),
		`[{"role":"user","content":"第一问"}]`)
	if t1.delivered != "第一轮答复" {
		t.Fatalf("第一轮下发正文 = %q", t1.delivered)
	}
	if got := chainField(t1.body, "conversation_id"); got != "" {
		t.Errorf("首轮不该带 conversation_id，实际 %q", got)
	}
	if got := chainField(t1.body, "parent_message_id"); got != parentMessageRoot {
		t.Errorf("首轮 parent = %q，期望 %q", got, parentMessageRoot)
	}

	t2 := runStreamTurn(t, c, m, framesFor("c-1", "m-2", "第二轮答复"),
		`[{"role":"user","content":"第一问"},{"role":"assistant","content":"第一轮答复"},{"role":"user","content":"第二问"}]`)
	if got := chainField(t2.body, "conversation_id"); got != "c-1" {
		t.Errorf("第二轮 conversation_id = %q，期望 c-1（续接上一轮）", got)
	}
	if got := chainField(t2.body, "parent_message_id"); got != "m-1" {
		t.Errorf("第二轮 parent_message_id = %q，期望 m-1（上一轮 assistant 消息）", got)
	}
	// conduit 预取（第 4 步）指向别处上游不认 ⇒ 必须与第 5 步同源。
	m.mu.Lock()
	prep := m.prepBodies[len(m.prepBodies)-1]
	m.mu.Unlock()
	var prepBody map[string]any
	if err := json.Unmarshal([]byte(prep), &prepBody); err != nil {
		t.Fatalf("conduit body 不是 JSON: %v", err)
	}
	if chainField(prepBody, "conversation_id") != "c-1" || chainField(prepBody, "parent_message_id") != "m-1" {
		t.Errorf("conduit 预取续接点 = %v / %v，期望 c-1 / m-1",
			prepBody["conversation_id"], prepBody["parent_message_id"])
	}
	// 正文仍只发最后一句（续接靠上游历史，不是靠本地重放）。
	msgs, _ := t2.body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("上游只吃一条 user 消息，实际 %d 条", len(msgs))
	}

	// 第三轮：续接点换成第二轮解析出的 (c-1, m-2)，证明链是**逐轮推进**的。
	t3 := runAggregateTurn(t, c, m, framesFor("c-1", "m-3", "第三轮答复"),
		`[{"role":"user","content":"第一问"},{"role":"assistant","content":"第一轮答复"},{"role":"user","content":"第二问"},{"role":"assistant","content":"第二轮答复"},{"role":"user","content":"第三问"}]`)
	if chainField(t3.body, "conversation_id") != "c-1" || chainField(t3.body, "parent_message_id") != "m-2" {
		t.Errorf("第三轮续接点 = %v / %v，期望 c-1 / m-2",
			t3.body["conversation_id"], t3.body["parent_message_id"])
	}
}

// TestConvChainMissStartsNewConversation 验收②：未命中一律新建会话，不报错、不影响可用性。
func TestConvChainMissStartsNewConversation(t *testing.T) {
	c, m := chatMockClient(t)

	// 没有前一轮记录，却带着完整历史（客户端重启/裁剪历史/换账号）⇒ 安静新建。
	res := runStreamTurn(t, c, m, framesFor("c-9", "m-9", "答复"),
		`[{"role":"user","content":"上一轮的问题"},{"role":"assistant","content":"上一轮的答复"},{"role":"user","content":"新问题"}]`)
	if got := chainField(res.body, "conversation_id"); got != "" {
		t.Errorf("未命中不该带 conversation_id，实际 %q", got)
	}
	if got := chainField(res.body, "parent_message_id"); got != parentMessageRoot {
		t.Errorf("未命中 parent = %q，期望 root（新建会话）", got)
	}

	// 未命中不能污染链路：这一轮自己成为下一轮的锚点。
	res2 := runStreamTurn(t, c, m, framesFor("c-9", "m-10", "答复2"),
		`[{"role":"user","content":"上一轮的问题"},{"role":"assistant","content":"上一轮的答复"},{"role":"user","content":"新问题"},{"role":"assistant","content":"答复"},{"role":"user","content":"再问"}]`)
	if chainField(res2.body, "conversation_id") != "c-9" || chainField(res2.body, "parent_message_id") != "m-9" {
		t.Errorf("未命中轮之后应能续上，实际 %v / %v",
			res2.body["conversation_id"], res2.body["parent_message_id"])
	}
}

// TestConvChainSameFirstQuestionDoesNotCrossTalk 验收③：两个对话首句相同也不串话。
//
// 判据就是「回复正文参与键」：同首句的两个对话，只有 us 下发的答复不同，
// 下一轮的历史前缀因此不同 ⇒ 各回各的上游会话。
func TestConvChainSameFirstQuestionDoesNotCrossTalk(t *testing.T) {
	c, m := chatMockClient(t)

	a1 := runStreamTurn(t, c, m, framesFor("c-A", "m-A", "答复甲"), `[{"role":"user","content":"同一个问题"}]`)
	b1 := runStreamTurn(t, c, m, framesFor("c-B", "m-B", "答复乙"), `[{"role":"user","content":"同一个问题"}]`)
	if a1.delivered == b1.delivered {
		t.Fatalf("两个对话的答复应当不同，否则本用例证明不了「不串话」")
	}

	// 对话 A 的第二轮：必须回 A 的会话。
	a2 := runStreamTurn(t, c, m, framesFor("c-A", "m-A2", "A 的答复2"),
		`[{"role":"user","content":"同一个问题"},{"role":"assistant","content":"答复甲"},{"role":"user","content":"A 的第二问"}]`)
	if chainField(a2.body, "conversation_id") != "c-A" || chainField(a2.body, "parent_message_id") != "m-A" {
		t.Errorf("对话 A 串到了 %v / %v，期望 c-A / m-A",
			a2.body["conversation_id"], a2.body["parent_message_id"])
	}

	// 对话 B 的第二轮：必须回 B 的会话（哪怕首句一模一样）。
	b2 := runStreamTurn(t, c, m, framesFor("c-B", "m-B2", "B 的答复2"),
		`[{"role":"user","content":"同一个问题"},{"role":"assistant","content":"答复乙"},{"role":"user","content":"B 的第二问"}]`)
	if chainField(b2.body, "conversation_id") != "c-B" || chainField(b2.body, "parent_message_id") != "m-B" {
		t.Errorf("对话 B 串到了 %v / %v，期望 c-B / m-B",
			b2.body["conversation_id"], b2.body["parent_message_id"])
	}
}

// TestConvChainLastMessageNotUser 验收④：最后一条不是 user 时向前找最近一条 user，不报错。
func TestConvChainLastMessageNotUser(t *testing.T) {
	c, m := chatMockClient(t)

	// 先建锚点（第一轮）。
	runStreamTurn(t, c, m, framesFor("c-1", "m-1", "第一轮答复"), `[{"role":"user","content":"第一问"}]`)

	// 第二轮的最后一条是 tool 回调：本轮真正的 user 是它前面那条，必须能续上。
	res := runStreamTurn(t, c, m, framesFor("c-1", "m-2", "第二轮答复"),
		`[{"role":"user","content":"第一问"},{"role":"assistant","content":"第一轮答复"},`+
			`{"role":"user","content":"第二问"},{"role":"tool","content":"工具结果不该被当成提问"}]`)
	if chainField(res.body, "conversation_id") != "c-1" || chainField(res.body, "parent_message_id") != "m-1" {
		t.Errorf("末条非 user 时未续上，实际 %v / %v",
			res.body["conversation_id"], res.body["parent_message_id"])
	}
	msgs, _ := res.body["messages"].([]any)
	msg, _ := msgs[0].(map[string]any)
	content, _ := msg["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	text, _ := parts[0].(string)
	if text != "第二问" {
		t.Errorf("正文 = %q，期望最近一条 user（第二问）", text)
	}
	if strings.Contains(text, "工具结果") {
		t.Errorf("正文 = %q，末条 tool 内容不得被当成提问", text)
	}
}

// TestConvChainRestoresAfterRestart 验收⑥：重启后从落盘恢复（新客户端读同一路径）。
func TestConvChainRestoresAfterRestart(t *testing.T) {
	dir := t.TempDir()
	m := newChatMock()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)

	first := NewWithConfig(Config{Base: srv.URL, DataDir: dir})
	runStreamTurn(t, first, m, framesFor("c-1", "m-1", "第一轮答复"), `[{"role":"user","content":"第一问"}]`)
	if _, err := os.Stat(filepath.Join(dir, convChainFileName)); err != nil {
		t.Fatalf("前缀链没落盘: %v", err)
	}
	first.Close()

	second := NewWithConfig(Config{Base: srv.URL, DataDir: dir})
	t.Cleanup(second.Close)
	res := runAggregateTurn(t, second, m, framesFor("c-1", "m-2", "第二轮答复"),
		`[{"role":"user","content":"第一问"},{"role":"assistant","content":"第一轮答复"},{"role":"user","content":"第二问"}]`)
	if chainField(res.body, "conversation_id") != "c-1" || chainField(res.body, "parent_message_id") != "m-1" {
		t.Errorf("重启后未恢复续接点，实际 %v / %v",
			res.body["conversation_id"], res.body["parent_message_id"])
	}
}

// TestConvChainTruncatedTurnNotRecorded 截断的一轮不得成为下一轮的锚点
// （客户端只收到半句，重放回来的是残句 ⇒ 记了也命不中，只会白白污染 LRU）。
func TestConvChainTruncatedTurnNotRecorded(t *testing.T) {
	c, m := chatMockClient(t)
	m.mu.Lock()
	m.noDone = true // 上游没发 [DONE] 就断连
	m.mu.Unlock()

	m.setResp(framesFor("c-1", "m-1", "半句"))
	body := []byte(`{"model":"auto","stream":true,"messages":[{"role":"user","content":"第一问"}]}`)
	rc, status, _, err := c.ChatStream(sentinelAuth(), body)
	if err != nil || status != http.StatusOK {
		t.Fatalf("status=%d err=%v", status, err)
	}
	rec := httptest.NewRecorder()
	if _, serr := c.Stream(rec, rc, "auto"); serr == nil {
		t.Fatal("截断的流必须报错（不补 [DONE]）")
	}
	_ = rc.Close()

	c.mu.Lock()
	chain := c.conv
	c.mu.Unlock()
	if chain == nil {
		t.Fatal("前缀链应当是懒创建的")
	}
	if _, ok := chain.Lookup(msgFingerprint(accountKey(sentinelAuth()),
		[]historyMsg{{Role: "user", Text: "第一问"}, {Role: "assistant", Text: "半句"}})); ok {
		t.Error("截断的一轮不得入链")
	}
}

// TestConvChainDoesNotTouchPoolStateFile 验收⑦护栏：状态落独立文件，绝不进账号池状态文件。
//
// R143 血案：池状态由 pool.saveLocked **整文件覆写**，塞进去的记录会被下一次
// 积分刷新/冷却记账抹掉。故这里显式让池真写一次盘，再核对两边内容。
func TestConvChainDoesNotTouchPoolStateFile(t *testing.T) {
	dir := t.TempDir()
	c, m := chatMockClientAt(t, dir)
	runStreamTurn(t, c, m, framesFor("c-1", "m-1", "第一轮答复"), `[{"role":"user","content":"第一问"}]`)

	chainPath := filepath.Join(dir, convChainFileName)
	poolPath := filepath.Join(dir, "state-chatgpt.json")
	if chainPath == poolPath {
		t.Fatal("前缀链与池状态文件必须是两个文件")
	}

	chainRaw, err := os.ReadFile(chainPath)
	if err != nil {
		t.Fatalf("前缀链文件不存在: %v", err)
	}
	if !strings.Contains(string(chainRaw), "c-1") || !strings.Contains(string(chainRaw), "m-1") {
		t.Errorf("前缀链文件没记续接点: %s", chainRaw)
	}

	// 让池真写一次盘（SetCreditDetail 无论账号在不在都会落盘）。
	p := pool.New(poolPath)
	p.SetCreditDetail("u1", 100, 0, 0, time.Time{})
	poolRaw, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatalf("池状态文件不存在: %v", err)
	}
	for _, banned := range []string{"c-1", "m-1", "conv_id", "parent_id", "chatgpt-convs"} {
		if strings.Contains(string(poolRaw), banned) {
			t.Errorf("池状态文件里出现了续接链内容 %q：%s", banned, poolRaw)
		}
	}
	if !strings.Contains(string(chainRaw), "entries") {
		t.Errorf("前缀链文件形状不对: %s", chainRaw)
	}
}

// newChatMockClientAt 建客户端并把前缀链/缓存落在指定目录（重启恢复与护栏用例需要）。
func chatMockClientAt(t *testing.T, dir string) (*Client, *chatMock) {
	t.Helper()
	m := newChatMock()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	c := NewWithConfig(Config{Base: srv.URL, DataDir: dir})
	t.Cleanup(c.Close)
	return c, m
}
