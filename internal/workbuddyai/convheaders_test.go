// convheaders_test.go 会话头族取值的单测：国际版（www.workbuddy.ai）。
//
// 三层语义的护栏都在这里：锚点提取（首条 user = 会话、末条 user = 轮）、
// 会话 id 稳定且按账号/会话隔离、轮 id 一轮一个、调用 id 每次新生成。
package workbuddyai

import (
	"regexp"
	"strings"
	"testing"
)

var (
	uuidV4Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	hex32Re  = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// TestConversationAnchorsConvAndTurn 三层语义的锚点来源：conv=首条 user、turn=末条 user。
func TestConversationAnchorsConvAndTurn(t *testing.T) {
	base := `{"model":"m","messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":"第一问"},
		{"role":"assistant","content":"回答"},
		{"role":"user","content":"第二问"}]}`
	conv, turn := conversationAnchors([]byte(base))
	if conv == "" || turn == "" {
		t.Fatalf("anchors empty: conv=%q turn=%q", conv, turn)
	}
	if conv == turn {
		t.Fatalf("conv 与 turn 应区分（首条 vs 末条 user）：%q", conv)
	}

	// 工具回调继续追加消息：末条 user 未变 ⇒ turn 不变（上游按它计一次 send）。
	withTool := `{"model":"m","messages":[
		{"role":"system","content":"sys"},
		{"role":"user","content":"第一问"},
		{"role":"assistant","content":"回答"},
		{"role":"user","content":"第二问"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"结果"}]}`
	conv2, turn2 := conversationAnchors([]byte(withTool))
	if conv2 != conv || turn2 != turn {
		t.Fatalf("工具回调后锚点漂移：conv %q→%q turn %q→%q", conv, conv2, turn, turn2)
	}

	// 新一轮 user send ⇒ turn 变，conv 不变（同一会话）。
	nextTurn := strings.Replace(base, `{"role":"user","content":"第二问"}`, `{"role":"user","content":"第三问"}`, 1)
	conv3, turn3 := conversationAnchors([]byte(nextTurn))
	if conv3 != conv {
		t.Fatalf("同一会话换轮后 conv 漂移：%q → %q", conv, conv3)
	}
	if turn3 == turn {
		t.Fatalf("换轮后 turn 应变化")
	}
}

// TestConversationAnchorsContentShapes content 的两种形态（裸串 / 分片数组）都要能抽。
func TestConversationAnchorsContentShapes(t *testing.T) {
	arr := `{"messages":[{"role":"user","content":[{"type":"text","text":"分片文本"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	conv, turn := conversationAnchors([]byte(arr))
	if conv == "" || turn == "" {
		t.Fatalf("数组形态抽不到锚点")
	}
	plain := `{"messages":[{"role":"user","content":"分片文本"}]}`
	conv2, _ := conversationAnchors([]byte(plain))
	if conv2 != conv {
		t.Fatalf("同一文本的数组/裸串形态锚点应一致：%q vs %q", conv, conv2)
	}

	// 纯图片（无文本）⇒ 抽不到文本锚点，退回空串（调用方退回随机）。
	imgOnly := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	if conv, turn := conversationAnchors([]byte(imgOnly)); conv != "" || turn != "" {
		t.Fatalf("纯图片不应产出锚点：conv=%q turn=%q", conv, turn)
	}
}

// TestConversationAnchorsFallbacks 无 user / 非法 JSON / 超限 body 的处理。
func TestConversationAnchorsFallbacks(t *testing.T) {
	if conv, turn := conversationAnchors(nil); conv != "" || turn != "" {
		t.Fatalf("空 body 不该有锚点")
	}
	if conv, turn := conversationAnchors([]byte(`not json`)); conv != "" || turn != "" {
		t.Fatalf("非法 JSON 不该有锚点")
	}
	// 无 user：退化为首条消息（官方客户端也有只有 system 的探针请求）。
	conv, turn := conversationAnchors([]byte(`{"messages":[{"role":"system","content":"sys only"}]}`))
	if conv == "" || conv != turn {
		t.Fatalf("无 user 时应退化为首条消息：conv=%q turn=%q", conv, turn)
	}
	// 超限 body：直接放弃（多为内联图片）。
	big := `{"messages":[{"role":"user","content":"` + strings.Repeat("x", maxAnchorBody) + `"}]}`
	if conv, turn := conversationAnchors([]byte(big)); conv != "" || turn != "" {
		t.Fatalf("超限 body 不该有锚点")
	}
}

// TestConversationIDStableAndScoped 会话 id：同 (uid,锚点) 恒定；换 uid / 换锚点必不同。
func TestConversationIDStableAndScoped(t *testing.T) {
	a := conversationID("uid-1", "anchor-a")
	if !uuidV4Re.MatchString(a) {
		t.Fatalf("会话 id 不是 UUIDv4 形态：%q", a)
	}
	if again := conversationID("uid-1", "anchor-a"); again != a {
		t.Fatalf("同 uid+锚点应稳定：%q vs %q", a, again)
	}
	// 掺 uid：同一段对话被另一个账号服务时不得共用同一个会话 id（那才是关联键）。
	if other := conversationID("uid-2", "anchor-a"); other == a {
		t.Fatalf("不同账号不得共用会话 id：%q", other)
	}
	if other := conversationID("uid-1", "anchor-b"); other == a {
		t.Fatalf("不同会话不得共用会话 id：%q", other)
	}
	// 无锚点 ⇒ 随机（旧行为），但仍须合法形态且互不相同。
	x, y := conversationID("uid-1", ""), conversationID("uid-1", "")
	if !uuidV4Re.MatchString(x) || !uuidV4Re.MatchString(y) {
		t.Fatalf("随机会话 id 形态不对：%q %q", x, y)
	}
	if x == y {
		t.Fatalf("无锚点时应每请求随机：%q", x)
	}
}

// TestTurnIDStableAndScoped 轮 id：32 hex；同轮恒定；换轮/换号必不同。
func TestTurnIDStableAndScoped(t *testing.T) {
	conv := conversationID("uid-1", "conv-anchor")
	turn := turnIDOf("uid-1", conv, "turn-anchor")
	if !hex32Re.MatchString(turn) {
		t.Fatalf("轮 id 不是 32 位 hex：%q", turn)
	}
	if again := turnIDOf("uid-1", conv, "turn-anchor"); again != turn {
		t.Fatalf("同轮应稳定：%q vs %q", turn, again)
	}
	if other := turnIDOf("uid-1", conv, "turn-anchor-2"); other == turn {
		t.Fatalf("换轮后轮 id 应变化")
	}
	if other := turnIDOf("uid-2", conv, "turn-anchor"); other == turn {
		t.Fatalf("不同账号不得共用轮 id")
	}
	if !hex32Re.MatchString(turnIDOf("uid-1", conv, "")) {
		t.Fatalf("无锚点时轮 id 应为 32 hex")
	}
}

// TestNewMessageIDUniqueAndShape 调用级 id：32 hex，每次新生成。
func TestNewMessageIDUniqueAndShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id := newMessageID()
		if !hex32Re.MatchString(id) {
			t.Fatalf("消息 id 不是 32 hex：%q", id)
		}
		seen[id] = true
	}
	if len(seen) != 64 {
		t.Fatalf("消息 id 不唯一：%d/64", len(seen))
	}
}
