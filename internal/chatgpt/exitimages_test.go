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
)

// 票 07.6：chat 出口把 turnState.Images 翻译成 content 里的 markdown 图片，
// 交付形态 **b64 data URI 优先**，字节取不到才退回签名直链。
//
// 断言口径与票面一一对应：b64（可解码为合法图头）、字节失败回退 URL、两者都失败跳过、
// 无图逐字节一致、前缀链不含图片、流式与非流式一致。

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

// dataURIOf 把图片字节按 data URI 形态拼出来（断言用）。
func dataURIOf(mime string, b []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

// decodeDataURI 取出 `![图](data:image/png;base64,…)` 里的 (mime, 字节)。
func decodeDataURI(t *testing.T, content string) (string, []byte) {
	t.Helper()
	const head = "![图](data:"
	i := strings.Index(content, head)
	if i < 0 {
		t.Fatalf("content = %q，期望 `![图](data:…)` 形态", content)
	}
	rest := content[i+len(head):]
	j := strings.Index(rest, ")")
	if j < 0 {
		t.Fatalf("data URI 未闭合: %q", rest)
	}
	body := rest[:j]
	mime, b64, ok := strings.Cut(body, ";base64,")
	if !ok {
		t.Fatalf("data URI 缺少 ;base64,: %q", body)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("data URI 的 base64 解不开: %v", err)
	}
	return mime, raw
}

// TestChatExitAggregateAppendsImageDataURI 非流式：正文 + b64 data URI，且不泄漏指针/直链。
func TestChatExitAggregateAppendsImageDataURI(t *testing.T) {
	c, m, product := imageProductMock(t)
	content := chatExitAggregate(t, c)

	if !strings.HasPrefix(content, "画好了\n\n![图](data:") {
		t.Fatalf("content = %q，期望正文后追加 ![图](data:…)", content)
	}
	mime, raw := decodeDataURI(t, content)
	if mime != "image/png" {
		t.Errorf("data URI mime = %q，期望 image/png", mime)
	}
	// 解码后必须是上游那份真字节（合法 PNG 图头）。
	if string(raw) != string(product) {
		t.Errorf("解码字节与上游产物不一致（len %d vs %d）", len(raw), len(product))
	}
	if !strings.HasPrefix(string(raw), "\x89PNG\r\n\x1a\n") {
		t.Errorf("解码字节不是合法 PNG 图头: %q", raw[:8])
	}
	// 内部指针与「需要 cookie 的签名直链」都不应出现在 b64 形态里。
	if strings.Contains(content, "sediment://") || strings.Contains(content, "file-service://") {
		t.Errorf("响应里出现了内部指针: %q", content)
	}
	if strings.Contains(content, m.base()+"/estuary/file_chat1") {
		t.Errorf("b64 可用时不应把签名直链也塞进 content: %q", content)
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
	if !strings.Contains(last, "![图](data:") {
		t.Errorf("最后一帧 = %q，期望 b64 图片 markdown（先文本后图片）", last)
	}
	for _, prev := range chunks[:len(chunks)-1] {
		if strings.Contains(prev, "![图](") {
			t.Errorf("图片出现在正文帧之前: %q", prev)
		}
	}
}

// TestChatExitByteFailureFallsBackToURL 字节取不到（签名直链 500）⇒ 退回签名直链形态。
func TestChatExitByteFailureFallsBackToURL(t *testing.T) {
	c, m, _ := imageProductMock(t)
	m.mu.Lock()
	m.brokenSignedURL = true
	m.mu.Unlock()

	content := chatExitAggregate(t, c)
	wantURL := m.base() + "/estuary/file_chat1"
	if content != "画好了\n\n![图]("+wantURL+"?sig=file_chat1)" {
		t.Fatalf("content = %q，期望退回签名直链形态", content)
	}
	if strings.Contains(content, "data:") {
		t.Errorf("字节不可用时不应出现 data URI: %q", content)
	}
	if !strings.HasPrefix(wantURL, "http") || strings.Contains(content, "sediment://") {
		t.Errorf("回退地址必须是公开 http(s)，且不得是指针: %q", content)
	}
}

// TestChatExitNoURLNoBytesSkipsImage 两步都失败：跳过该图、响应不报错、只留纯文本。
func TestChatExitNoURLNoBytesSkipsImage(t *testing.T) {
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
	// 字节失败后还会单独问一次下载接口（只取直链）；两次都失败即停止，不无限重试。
	if got := m.downloads(); len(got) != 2 {
		t.Errorf("下载接口尝试 = %v，期望 2 次（DownloadImage 一次 + 直链回退一次）", got)
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
	// 用 data URI 与 URL 两种出口形态各探一次：都不能进链。
	for _, withImages := range []string{
		"画好了" + exitImageSeparator + "![图](data:image/png;base64,QUJD)",
		"画好了" + exitImageSeparator + "![图](https://x/u.png)",
	} {
		if _, ok := c.convChain().Lookup(key(withImages)); ok {
			t.Errorf("图片 markdown 不得进前缀链: %q", withImages)
		}
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
	if got := exitImageURL(exitImagePayload{}); got != "" {
		t.Errorf("无 URL 无字节 = %q，期望空（不编造）", got)
	}
	if got := exitImageURL(exitImagePayload{URL: "sediment://file_x"}); got != "" {
		t.Errorf("指针不得当 URL: %q", got)
	}
	// b64 优先于直链（票 07.6）：两者都有时只交 data URI。
	if got := exitImageURL(exitImagePayload{B64: "QUJD", Mime: "image/webp", URL: "https://x/a.png"}); got != "data:image/webp;base64,QUJD" {
		t.Errorf("b64 优先 = %q", got)
	}
	// 上游给的是 .png 字节但没声明 mime：兜 image/png。
	if got := exitImageURL(exitImagePayload{B64: "QUJD"}); got != "data:image/png;base64,QUJD" {
		t.Errorf("mime 兜底 = %q", got)
	}
	if got := exitImageURL(exitImagePayload{URL: "https://x/a.png"}); got != "https://x/a.png" {
		t.Errorf("直链回退 = %q", got)
	}
	if got := (exitImagePayload{B64: "x"}).shape(); got != exitShapeB64 {
		t.Errorf("shape(b64) = %q", got)
	}
	if got := (exitImagePayload{URL: "https://x/a"}).shape(); got != exitShapeURL {
		t.Errorf("shape(url) = %q", got)
	}
	if got := (exitImagePayload{}).shape(); got != exitShapeSkip {
		t.Errorf("shape(empty) = %q", got)
	}
}

// TestExitImageMimePrefersMagicBytes MIME 取字节魔数（与交付字节同源），其次上游声明。
func TestExitImageMimePrefersMagicBytes(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	if got := exitImageMime(jpeg, imageRef{MimeType: "image/png"}); got != "image/jpeg" {
		t.Errorf("魔数优先 = %q，期望 image/jpeg", got)
	}
	if got := exitImageMime([]byte("not-an-image"), imageRef{MimeType: "image/webp"}); got != "image/webp" {
		t.Errorf("嗅不出时用上游声明 = %q，期望 image/webp", got)
	}
	if got := exitImageMime([]byte("not-an-image"), imageRef{}); got != exitImageDefaultMime {
		t.Errorf("都缺 = %q，期望 %s", got, exitImageDefaultMime)
	}
}

// TestChatExitImageLogsDeliveryShape 票面第 5 条：日志带交付形态（b64/url/skip）与图数，零凭据。
func TestChatExitImageLogsDeliveryShape(t *testing.T) {
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
	for _, want := range []string{"uid=uid:u1", "model=chatgpt/auto", "count=1", "b64=1", "url=0", "skip=0"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志 %q 缺 %q", out, want)
		}
	}
	if strings.Contains(out, "tok") || strings.Contains(out, "Authorization") {
		t.Errorf("日志不得出现凭据: %q", out)
	}
}

// TestChatExitImageLogsURLFallbackShape 回退形态在日志里可分辨（url=1 / b64=0）。
func TestChatExitImageLogsURLFallbackShape(t *testing.T) {
	c, m, _ := imageProductMock(t)
	m.mu.Lock()
	m.brokenSignedURL = true
	m.mu.Unlock()

	var buf strings.Builder
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	_ = chatExitAggregate(t, c)
	out := buf.String()
	for _, want := range []string{"count=1", "b64=0", "url=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("日志 %q 缺 %q", out, want)
		}
	}
}

// TestExitImageBlocksPreservesOrderAndPerImageDegrade 多图：按上游顺序排列，逐张独立降级。
func TestExitImageBlocksPreservesOrderAndPerImageDegrade(t *testing.T) {
	fake := &fakeImageReader{
		Reader:  strings.NewReader(""),
		account: "uid:u1",
		got: map[string]exitImagePayload{
			"file_a": {B64: "QUJD", Mime: "image/png"}, // 字节可用 ⇒ b64
			"file_b": {URL: "https://x/b.png"},         // 字节失败 ⇒ 直链
			"file_c": {URL: "sediment://file_c"},       // 两者都不可用 ⇒ 跳过
		},
	}
	st := &turnState{Images: []imageRef{{FileID: "file_a"}, {FileID: "file_b"}, {FileID: "file_c"}}}
	blocks := exitImageBlocks(fake, st, "chatgpt/auto")
	if len(blocks) != 2 {
		t.Fatalf("图片块 = %v，期望 2（file_c 不可用被跳过）", blocks)
	}
	if blocks[0] != "![图](data:image/png;base64,QUJD)" {
		t.Errorf("第一块 = %q，期望 b64 形态", blocks[0])
	}
	if blocks[1] != "![图](https://x/b.png)" {
		t.Errorf("第二块 = %q，期望直链回退形态", blocks[1])
	}
}

// fakeImageReader 是 resolver 的桩（单一职责：只验顺序与逐张降级）。
type fakeImageReader struct {
	io.Reader
	account string
	got     map[string]exitImagePayload
	fail    map[string]bool
}

func (f *fakeImageReader) imageAccountLabel() string { return f.account }

func (f *fakeImageReader) resolveExitImage(ref imageRef, convID string) (exitImagePayload, error) {
	if f.fail[ref.FileID] {
		return exitImagePayload{}, errors.New("boom")
	}
	return f.got[ref.FileID], nil
}

// TestExitImageBlocksWithoutResolverIsEmpty 非会话读端（裸 Reader）不编造链接。
func TestExitImageBlocksWithoutResolverIsEmpty(t *testing.T) {
	st := &turnState{Images: []imageRef{{Pointer: "sediment://file_x", FileID: "file_x"}}}
	if got := exitImageBlocks(strings.NewReader(""), st, "chatgpt/auto"); len(got) != 0 {
		t.Errorf("无凭据上下文不得产出图片块: %v", got)
	}
}

// TestExitImageResolveBothFailReturnsError 两步都失败时 resolver 返回错误（由出口跳过该图）。
func TestExitImageResolveBothFailReturnsError(t *testing.T) {
	c, m, _ := imageProductMock(t)
	m.mu.Lock()
	m.noDownloadURL = true
	m.mu.Unlock()

	rc, status, body, err := c.ChatStream(sentinelAuth(), chatImageBody)
	if err != nil || status != http.StatusOK {
		t.Fatalf("ChatStream status=%d err=%v body=%s", status, err, string(body))
	}
	cap, ok := rc.(*convCapture)
	if !ok {
		t.Fatalf("读端 = %T，期望 *convCapture", rc)
	}
	if _, rerr := cap.resolveExitImage(imageRef{FileID: "file_chat1", MimeType: "image/png"}, cap.convID); rerr == nil {
		t.Error("字节与直链都取不到时必须返回错误")
	}
	_ = rc.Close()
}
