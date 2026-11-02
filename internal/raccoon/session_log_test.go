package raccoon

// R148：会话 ID 与「是否新建」必须写进 app.log。
//
// 动机：429 的归因（会话稳定性）只能靠「同一账号+模型是否复用同一 sess-」验证，而该值
// 此前只在出站头里、日志看不到，等于关键决策点无法事后核对。
//
// 这组测试断言的是**日志内容**，不是 sessionFor 的返回值——后者已有 session_test.go 覆盖。
// 只测返回值测不出「日志有没有接上线」（本仓踩过的假绿形态）。

import (
	"bytes"
	"log"
	"regexp"
	"strings"
	"testing"
)

// captureLog 把标准 log 输出重定向到缓冲区，测完还原。
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	oldOut := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
	})
	return &buf
}

// TestSessionLogsIDAndNewFlag 首次请求必须打出 sid 且 new=true，第二次同账号同模型 new=false。
//
// new 标志是本决策的可观测点：`new=true` 说明三个条件齐（key 未命中 + 首现/过 24h）才新建。
func TestSessionLogsIDAndNewFlag(t *testing.T) {
	resetSessions(t)
	cap := &sessionCapture{}
	c := newSessionTestServer(t, cap)
	buf := captureLog(t)

	const model = "raccoon-8c4485"

	chatOnce(t, c, model) // 第 1 次：新建
	firstLog := buf.String()
	buf.Reset()
	chatOnce(t, c, model) // 第 2 次：复用
	secondLog := buf.String()

	// 形状：raccoon session: uid=<uid> model="<model>" sid=sess-<n>-<hex> new=<bool>
	re := regexp.MustCompile(`raccoon session: uid=(\S+) model="([^"]+)" sid=(sess-\d+-[0-9a-f]{8}) new=(true|false)`)

	m1 := re.FindStringSubmatch(firstLog)
	if m1 == nil {
		t.Fatalf("首次请求未打出会话日志，实际输出：\n%s", firstLog)
	}
	if m1[1] != "Lily" {
		t.Errorf("日志 uid = %q，want Lily（应带账号维度）", m1[1])
	}
	if m1[2] != model {
		t.Errorf("日志 model = %q，want %q", m1[2], model)
	}
	if m1[4] != "true" {
		t.Errorf("首次请求 new = %q，want true（应当新建）", m1[4])
	}

	m2 := re.FindStringSubmatch(secondLog)
	if m2 == nil {
		t.Fatalf("第二次请求未打出会话日志，实际输出：\n%s", secondLog)
	}
	if m2[4] != "false" {
		t.Errorf("第二次同账号同模型 new = %q，want false（应复用原会话）", m2[4])
	}
	// 两次的 sid 必须相同（复用语义）。
	if m1[3] != m2[3] {
		t.Errorf("两次日志的 sid 不同：%q vs %q，应复用", m1[3], m2[3])
	}
}

// TestSessionLogNeverLeaksToken 会话日志不得出现 access token。
//
// 与 既有红线/既有红线/既有红线 的「零 token 落盘」同一条红线：uid/模型/会话 ID 可打，token 不可。
func TestSessionLogNeverLeaksToken(t *testing.T) {
	resetSessions(t)
	cap := &sessionCapture{}
	c := newSessionTestServer(t, cap)
	buf := captureLog(t)

	const secret = "sekrit-access-token-value-1234567890"
	a := testAuthWithToken("Lily", secret)
	chatOnceAs(t, c, a, "raccoon-8c4485")

	got := buf.String()
	if strings.Contains(got, secret) {
		t.Fatalf("会话日志泄露了 access token：\n%s", got)
	}
	if !strings.Contains(got, "raccoon session:") {
		t.Fatalf("会话日志没打出来，实际输出：\n%s", got)
	}
}
