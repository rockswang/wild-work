package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// 票 07.5：chat 出口把 turnState.Images 翻译成 content 里的 markdown 图片。
//
// 断言口径与票面一一对应：带图（URL 两步换址）、换址失败降级、无图逐字节一致、
// 前缀链不含图片、流式与非流式一致。

// chatImageBody 是最小的入站对话体（本渠道只取最后一条 user 文本）。
var chatImageBody = []byte(`{"model":"auto","messages":[{"role":"user","content":"画一只猫"}]}`)

// imageProductMock 建一个「对话里带一张产物图」的假上游。
func imageProductMock(t *testing.T) (*Client, *imageMock, []byte) {
	t.Helper()
	c, m := imageMockClient(t)
	product := testPNG(64, 48)
	m.mu.Lock()
	m.productFileIDs = []string{"file_chat1"}
	m.products["file_chat1"] = product
	m.convText = "画好了"
	m.mu.Unlock()
	return c, m, product
}

// chatExitStream 跑一轮流式出口，返回（正文拼接、正文增量序列）。
func chatExitStream(t *testing.T, c *Client) (string, []string) {
	t.Helper()
	rc, status, body, err := c.ChatStream(sentinelAuth(), chatImageBody)
	if err != nil || status != http.StatusOK {
		t.Fatalf("ChatStream status=%d err=%v body=%s", status, err, string(body))
	}
	rec := httptest.NewRecorder()
	if _, err := c.Stream(rec, rc, "chatgpt/auto"); err != nil {
		t.Fatalf("Stream err=%v", err)
	}
	_ = rc.Close()
	return sseContent(rec.Body.String()), sseContentChunks(rec.Body.String())
}

// chatExitAggregate 跑一轮非流式出口，返回正文。
func chatExitAggregate(t *testing.T, c *Client) string {
	t.Helper()
	rc, status, body, err := c.ChatStream(sentinelAuth(), chatImageBody)
	if err != nil || status != http.StatusOK {
		t.Fatalf("ChatStream status=%d err=%v body=%s", status, err, string(body))
	}
	resp, aerr := c.Aggregate(rc, "chatgpt/auto")
	_ = rc.Close()
	if aerr != nil {
		t.Fatalf("Aggregate err=%v", aerr)
	}
	chs, _ := resp["choices"].([]any)
	msg, _ := chs[0].(map[string]any)["message"].(map[string]any)
	content, _ := msg["content"].(string)
	return content
}

// sseContentChunks 按帧收集 delta.content（用于断言「先文本后图片」的顺序）。
func sseContentChunks(sse string) []string {
	var frames []string
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
			if s, ok := d["content"].(string); ok && s != "" {
				frames = append(frames, s)
			}
		}
	}
	return frames
}

// TestChatExitAggregateAppendsImageMarkdown 非流式：正文 + 换址后的 markdown 图，且无内部指针。
func TestChatExitAggregateAppendsImageMarkdown(t *testing.T) {
	c, m, product := imageProductMock(t)
	content := chatExitAggregate(t, c)

	wantURL := m.base() + "/estuary/file_chat1"
	if !strings.HasPrefix(content, "画好了\n\n![图](") {
		t.Fatalf("content = %q，期望正文后追加 ![图](…)", content)
	}
	if !strings.Contains(content, wantURL) {
		t.Errorf("content = %q，期望包含签名直链 %q", content, wantURL)
	}
	// 内部指针绝不外泄（票面红线：sediment:// 不出本包）。
	if strings.Contains(content, "sediment://") || strings.Contains(content, "file-service://") {
		t.Errorf("响应里出现了内部指针: %q", content)
	}
	// b64 不应在 URL 可用时进入 content（图仍在响应里只出现一次）。
	if strings.Contains(content, base64.StdEncoding.EncodeToString(product)) {
		t.Error("URL 可用时不应把 b64 也塞进 content")
	}
	// 每张图各走一遍两步下载。
	if got := m.downloads(); strings.Join(got, ",") != "file_chat1" {
		t.Errorf("下载序列 = %v，期望 [file_chat1]", got)
	}
}

// TestChatExitStreamMatchesAggregate 流式与非流式出口行为一致（且图片排在文本之后）。
func TestChatExitStreamMatchesAggregate(t *testing.T) {
	// 同一个客户端跑两条出口：换址直链带假上游的端口，分两次建 mock 比不出「逐字一致」。
	c, _, _ := imageProductMock(t)
	want := chatExitAggregate(t, c)
	got, chunks := chatExitStream(t, c)

	if got != want {
		t.Fatalf("流式正文 = %q，非流式 = %q，必须一致", got, want)
	}
	if len(chunks) < 2 {
		t.Fatalf("增量帧 = %v，期望正文帧 + 图片帧", chunks)
	}
	last := chunks[len(chunks)-1]
	if !strings.Contains(last, "![图](") {
		t.Errorf("最后一帧 = %q，期望图片 markdown（先文本后图片）", last)
	}
	for _, prev := range chunks[:len(chunks)-1] {
		if strings.Contains(prev, "![图](") {
			t.Errorf("图片出现在正文帧之前: %q", prev)
		}
	}
}

// TestChatExitImageResolveFailureDegrades 换址失败：跳过该图、响应不报错、只留纯文本。
func TestChatExitImageResolveFailureDegrades(t *testing.T) {
	c, m, _ := imageProductMock(t)
	m.mu.Lock()
	m.noDownloadURL = true
	m.mu.Unlock()

	content := chatExitAggregate(t, c)
	if content != "画好了" {
		t.Errorf("换址失败时 content = %q，期望纯文本正文", content)
	}
	if strings.Contains(content, "![图]") || strings.Contains(content, "sediment://") {
		t.Errorf("换址失败不得输出图片标记: %q", content)
	}
	if got := m.downloads(); len(got) != 1 {
		t.Errorf("下载尝试 = %v，期望 1 次（失败即跳过，不重试）", got)
	}
}

// TestChatExitNoImageKeepsContentByteIdentical 无图轮：输出与改动前逐字节一致（反向护栏）。
func TestChatExitNoImageKeepsContentByteIdentical(t *testing.T) {
	c, m := imageMockClient(t)
	const reply = "纯文本回答\n第二行  结尾有两个空格  "
	m.mu.Lock()
	m.convText = reply
	m.mu.Unlock()

	if content := chatExitAggregate(t, c); content != reply {
		t.Errorf("无图轮 content = %q，必须与上游正文逐字节一致", content)
	}
	if got := m.downloads(); len(got) != 0 {
		t.Errorf("无图轮不得触发下载: %v", got)
	}

	// 流式同样零改动（不得多出空帧/占位）。
	c2, m2 := imageMockClient(t)
	m2.mu.Lock()
	m2.convText = reply
	m2.mu.Unlock()
	content, chunks := chatExitStream(t, c2)
	if content != reply {
		t.Errorf("无图轮流式 content = %q，必须与上游正文一致", content)
	}
	if len(chunks) != 1 || chunks[0] != reply {
		t.Errorf("无图轮增量帧 = %v，期望恰好一帧原文", chunks)
	}
}

// TestChatExitImagesExcludedFromConvChain 前缀链记录的是纯文本轮：图片 markdown 不进链。
func TestChatExitImagesExcludedFromConvChain(t *testing.T) {
	c, _, _ := imageProductMock(t)
	chatExitAggregate(t, c)

	req, err := parseChatBody(chatImageBody)
	if err != nil {
		t.Fatalf("parseChatBody: %v", err)
	}
	key := func(reply string) string {
		next := append(req.throughTurn(), historyMsg{Role: "assistant", Text: reply})
		return msgFingerprint(accountKey(sentinelAuth()), next)
	}
	if _, ok := c.convChain().Lookup(key("画好了")); !ok {
		t.Error("纯文本轮必须进前缀链（recordTurnOf 现行为不变）")
	}
	withImages := "画好了" + exitImageSeparator + "![图](" + strings.Repeat("u", 8) + ")"
	if _, ok := c.convChain().Lookup(key(withImages)); ok {
		t.Error("图片 markdown 不得进前缀链")
	}
}

// TestExitImageSuffixNoBlockIsVerbatim 纯函数护栏：无块时零改动、有块时纯文本在前。
func TestExitImageSuffixNoBlockIsVerbatim(t *testing.T) {
	if got := exitImageSuffix("abc\n", ""); got != "" {
		t.Errorf("无块后缀 = %q，必须为空（不得改动正文）", got)
	}
	if got := "abc" + exitImageSuffix("abc", "![图](u)"); got != "abc\n\n![图](u)" {
		t.Errorf("有块拼接 = %q", got)
	}
	if got := "" + exitImageSuffix("", "![图](u)"); got != "![图](u)" {
		t.Errorf("空正文拼接 = %q", got)
	}
	if got := exitImageSuffix("abc", "![图](u)"); got != "\n\n![图](u)" {
		t.Errorf("流式增量 = %q", got)
	}
	if got := exitImageURL(provider.ImageResult{}, imageRef{}); got != "" {
		t.Errorf("无 URL 无字节 = %q，期望空（不编造）", got)
	}
	if got := exitImageURL(provider.ImageResult{URL: "sediment://file_x"}, imageRef{}); got != "" {
		t.Errorf("指针不得当 URL: %q", got)
	}
	if got := exitImageURL(provider.ImageResult{B64JSON: "QUJD"}, imageRef{MimeType: "image/webp"}); got != "data:image/webp;base64,QUJD" {
		t.Errorf("b64 兜底 = %q", got)
	}
}

// TestChatExitImageLogsOneLine 票面第 5 条：出口带图打一行日志（uid/model/图数），零凭据。
func TestChatExitImageLogsOneLine(t *testing.T) {
	c, _, _ := imageProductMock(t)
	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)

	chatExitAggregate(t, c)
	out := buf.String()
	if !strings.Contains(out, "chatgpt exit images:") {
		t.Fatalf("缺少带图日志: %q", out)
	}
	for _, want := range []string{"uid=uid:u1", "model=chatgpt/auto", "count=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志 %q 缺 %q", out, want)
		}
	}
	if strings.Contains(out, "tok") || strings.Contains(out, "Authorization") {
		t.Errorf("日志不得出现凭据: %q", out)
	}
}

// TestExitImageBlocksPreservesOrderAndSkipsFailures 多图：按上游顺序排列，单张失败只跳过该张。
func TestExitImageBlocksPreservesOrderAndSkipsFailures(t *testing.T) {
	fake := &fakeImageReader{
		Reader:  strings.NewReader(""),
		account: "uid:u1",
		urls:    map[string]string{"file_a": "https://x/a.png", "file_b": "https://x/b.png", "file_c": "https://x/c.png"},
		fail:    map[string]bool{"file_b": true},
	}
	st := &turnState{Images: []imageRef{{FileID: "file_a"}, {FileID: "file_b"}, {FileID: "file_c"}}}
	blocks := exitImageBlocks(fake, st, "chatgpt/auto")
	if len(blocks) != 2 {
		t.Fatalf("图片块 = %v，期望 2（file_b 换址失败被跳过）", blocks)
	}
	if !strings.Contains(blocks[0], "https://x/a.png") || !strings.Contains(blocks[1], "https://x/c.png") {
		t.Errorf("顺序 = %v，期望按上游出现顺序 a → c", blocks)
	}
}

// fakeImageReader 是 imageResolver 的桩（单一职责：只验顺序与逐张降级）。
type fakeImageReader struct {
	io.Reader
	account string
	urls    map[string]string
	fail    map[string]bool
}

func (f *fakeImageReader) imageAccountLabel() string { return f.account }

func (f *fakeImageReader) resolveExitImage(ref imageRef, convID string) (provider.ImageResult, error) {
	if f.fail[ref.FileID] {
		return provider.ImageResult{}, errors.New("boom")
	}
	return provider.ImageResult{URL: f.urls[ref.FileID]}, nil
}

// TestExitImageBlocksWithoutResolverIsEmpty 非会话读端（裸 Reader）不编造链接。
func TestExitImageBlocksWithoutResolverIsEmpty(t *testing.T) {
	st := &turnState{Images: []imageRef{{Pointer: "sediment://file_x", FileID: "file_x"}}}
	if got := exitImageBlocks(strings.NewReader(""), st, "chatgpt/auto"); len(got) != 0 {
		t.Errorf("无凭据上下文不得产出图片块: %v", got)
	}
}
