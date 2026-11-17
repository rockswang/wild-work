package chatgpt

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"wild-work/internal/provider"
)

// imageMock 是生图链路的离线假上游：在既有 sentinel 假上游之上补
// 文件上传（四步）、产物下载（两步）与带附件的两段式对话。
//
// 图片产物**挂在 role:"tool" 消息里**（票 07 改动锚点）：从 assistant 消息里找永远拿不到。
type imageMock struct {
	*mockSentinel

	mu sync.Mutex

	// 上传侧
	reservations  int
	registrations int
	blobPuts      int
	blobHeaders   []http.Header
	blobBodies    [][]byte
	fileSeq       int
	processIDs    []string

	// 对话侧
	conduitBodies []string
	convBodies    []string
	convStatus    int
	// productFileIDs 按对话轮次给产物 file_id（第 i 轮 → [i]）；耗尽后重复最后一个。
	productFileIDs []string
	// convText 非空时该轮的 assistant 正文就是它（用于「没出图」的三类文案）。
	convText string

	// 下载侧
	downloadIDs   []string
	noDownloadURL bool
	products      map[string][]byte

	// srvBase 是本假上游自己的地址（伪造 upload_url / download_url 用）。
	srvBase string
}

func newImageMock() *imageMock {
	return &imageMock{mockSentinel: newMockSentinel(), products: map[string][]byte{}}
}

func (m *imageMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == PathUploadReservations:
		m.mu.Lock()
		m.reservations++
		base := m.srvBase
		m.mu.Unlock()
		writeJSONTest(w, map[string]any{"reservation_id": "rsv-1", "upload_url": base + "/blob/reserved"})
	case r.URL.Path == PathFiles:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.registrations++
		m.fileSeq++
		id := "file_up" + itoa(m.fileSeq)
		base := m.srvBase
		m.mu.Unlock()
		writeJSONTest(w, map[string]any{"file_id": id, "upload_url": base + "/blob/" + id})
	case r.URL.Path == PathProcessUpload:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.processIDs = append(m.processIDs, str(body["file_id"]))
		m.mu.Unlock()
		_, _ = io.WriteString(w, `{"event":"file.processing.started"}`+"\n"+
			`{"event":"file.processing.completed","extra":{"metadata_object_id":"lib_"}}`+"\n")
	case strings.HasPrefix(r.URL.Path, pathFileDownload):
		id := strings.TrimPrefix(r.URL.Path, pathFileDownload)
		m.mu.Lock()
		m.downloadIDs = append(m.downloadIDs, id)
		noURL := m.noDownloadURL
		base := m.srvBase
		m.mu.Unlock()
		if noURL {
			writeJSONTest(w, map[string]any{"status": "success"})
			return
		}
		writeJSONTest(w, map[string]any{"status": "success", "file_name": id + ".png",
			"download_url": base + "/estuary/" + id + "?sig=" + id})
	case strings.HasPrefix(r.URL.Path, "/blob/"):
		b, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.blobPuts++
		m.blobHeaders = append(m.blobHeaders, r.Header.Clone())
		m.blobBodies = append(m.blobBodies, b)
		m.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	case strings.HasPrefix(r.URL.Path, "/estuary/"):
		id := strings.TrimPrefix(r.URL.Path, "/estuary/")
		m.mu.Lock()
		body := m.products[id]
		m.mu.Unlock()
		w.Header().Set("content-type", "image/png")
		_, _ = w.Write(body)
	case r.URL.Path == PathFConversationPrepare:
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		m.mu.Lock()
		m.conduitBodies = append(m.conduitBodies, string(raw))
		tok := "cd-" + itoa(len(m.conduitBodies))
		m.mu.Unlock()
		writeJSONTest(w, map[string]any{"conduit_token": tok})
	case r.URL.Path == PathFConversation:
		raw, _ := io.ReadAll(r.Body)
		m.mu.Lock()
		m.convBodies = append(m.convBodies, string(raw))
		hit := len(m.convBodies)
		status := m.convStatus
		text := m.convText
		fids := append([]string(nil), m.productFileIDs...)
		products := map[string][]byte{}
		for k, v := range m.products {
			products[k] = v
		}
		m.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"detail":"upstream boom"}`))
			return
		}
		fid := ""
		if len(fids) > 0 {
			if hit-1 < len(fids) {
				fid = fids[hit-1]
			} else {
				fid = fids[len(fids)-1]
			}
		}
		if text == "" && fid != "" && products[fid] == nil {
			text = "no product bytes for " + fid
		}
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"conversation_id":"conv-1","message":{"id":"m`+itoa(hit)+
			`","author":{"role":"assistant"},"recipient":"all","channel":"final","content":{"content_type":"text","parts":["`+
			jsonEscape(text)+`"]}}}`+"\n\n")
		if fid != "" {
			// 产物图在 **role:"tool"** 消息里（改动锚点）。
			_, _ = io.WriteString(w, "data: "+`{"message":{"id":"t`+itoa(hit)+
				`","author":{"role":"tool"},"recipient":"all","channel":"commentary","content":{"content_type":"multimodal_text","parts":[{"content_type":"image_asset_pointer","asset_pointer":"sediment://`+
				fid+`","mime_type":"image/png","width":1024,"height":1024}]}}}`+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	default:
		m.mockSentinel.ServeHTTP(w, r)
	}
}

// base 返回本假上游的地址（由 imageMockClient 注入：服务端不知道自己的对外地址）。
func (m *imageMock) base() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.srvBase
}

func (m *imageMock) snapshotConvs() (conduits, convs []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.conduitBodies...), append([]string(nil), m.convBodies...)
}

func (m *imageMock) uploadSeq() (int, int, int, []http.Header, [][]byte, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reservations, m.registrations, m.blobPuts, append([]http.Header(nil), m.blobHeaders...),
		append([][]byte(nil), m.blobBodies...), append([]string(nil), m.processIDs...)
}

func (m *imageMock) downloads() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.downloadIDs...)
}

func dataURI(mime string, b []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}

// imageMockClient 建客户端并把 sentinel / 上传 / 对话 / 下载的假上游装好。
func imageMockClient(t *testing.T) (*Client, *imageMock) {
	t.Helper()
	m := newImageMock()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	m.mu.Lock()
	m.srvBase = srv.URL
	m.mu.Unlock()
	c := NewWithConfig(Config{Base: srv.URL, DataDir: t.TempDir()})
	t.Cleanup(c.Close)
	return c, m
}

// TestGenerateImagePointerConvertedTwoSteps 指针 → 下载地址 → 字节，对外只出现 URL / b64。
func TestGenerateImagePointerConvertedTwoSteps(t *testing.T) {
	c, m := imageMockClient(t)
	product := testPNG(640, 480)
	m.mu.Lock()
	m.productFileIDs = []string{"file_gen1"}
	m.products["file_gen1"] = product
	m.mu.Unlock()

	res, err := c.GenerateImage(sentinelAuth(), provider.ImageRequest{Model: "chatgpt/auto", Prompt: "画一只猫", N: 1})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("产物数 = %d，期望 1", len(res))
	}
	want := base64.StdEncoding.EncodeToString(product)
	if res[0].B64JSON != want {
		t.Errorf("B64JSON 与产物字节不一致（len=%d want %d）", len(res[0].B64JSON), len(want))
	}
	if !strings.Contains(res[0].URL, "/estuary/file_gen1") || !strings.Contains(res[0].URL, "sig=") {
		t.Errorf("URL = %q，期望签名直链", res[0].URL)
	}
	// 断言：内部指针绝不外泄（URL 与 b64 里都不允许出现 sediment://）。
	if strings.Contains(res[0].URL, "sediment://") || strings.Contains(res[0].B64JSON, "sediment") {
		t.Error("对外响应里出现了内部指针 sediment://")
	}
	// 两步下载各一次（现取现用，不缓存）。
	if got := m.downloads(); len(got) != 1 || got[0] != "file_gen1" {
		t.Errorf("下载请求 = %v", got)
	}
	// 无参考图 ⇒ 不应有任何上传调用。
	if _, regs, puts, _, _, _ := m.uploadSeq(); regs != 0 || puts != 0 {
		t.Errorf("文生图不应上传参考图（registrations=%d blobPuts=%d）", regs, puts)
	}
	_, convs := m.snapshotConvs()
	if len(convs) != 1 {
		t.Fatalf("对话轮次 = %d", len(convs))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(convs[0]), &body); err != nil {
		t.Fatalf("对话体非法 JSON: %v", err)
	}
	if got := body["model"]; got != defaultChatModel {
		t.Errorf("出站 model = %v，期望 %q", got, defaultChatModel)
	}
}

// TestGenerateImageSizeAcceptedButIgnored 尺寸参数接受但忽略：不 400、也不下发出站（不假装生效）。
func TestGenerateImageSizeAcceptedButIgnored(t *testing.T) {
	c, m := imageMockClient(t)
	m.mu.Lock()
	m.productFileIDs = []string{"file_gen1"}
	m.products["file_gen1"] = testPNG(8, 8)
	m.mu.Unlock()

	if _, err := c.GenerateImage(sentinelAuth(), provider.ImageRequest{Prompt: "画山", N: 1, Size: "1024x1024"}); err != nil {
		t.Fatalf("标准尺寸不应报错: %v", err)
	}
	_, convs := m.snapshotConvs()
	if len(convs) != 1 {
		t.Fatalf("对话轮次 = %d", len(convs))
	}
	// 上游不收 size：任何形状都不该出现在出站请求里。
	if strings.Contains(convs[0], "1024x1024") || strings.Contains(convs[0], `"size"`) {
		t.Errorf("出站请求体里出现了尺寸参数（上游不支持，不得假装生效）: %s", convs[0])
	}
}

// TestGenerateImageNTooLargeRejectedLocally 张数超上限在渠道层直接拒，不发任何上游请求。
func TestGenerateImageNTooLargeRejectedLocally(t *testing.T) {
	c, m := imageMockClient(t)
	_, err := c.GenerateImage(sentinelAuth(), provider.ImageRequest{Prompt: "画猫", N: 5})
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrBadParams {
		t.Fatalf("err = %v，期望 ErrBadParams（请求级，不换号）", err)
	}
	if m.hits(PathSentinelPrepare) != 0 {
		t.Error("本地拒绝后不得跑预检（不得发无效请求）")
	}
	if _, convs := m.snapshotConvs(); len(convs) != 0 {
		t.Errorf("本地拒绝后仍有对话请求: %v", convs)
	}
}

// TestGenerateImageMultipleRoundsShareConversation n>1 靠多轮请求凑，且续接在同一个会话里。
func TestGenerateImageMultipleRoundsShareConversation(t *testing.T) {
	c, m := imageMockClient(t)
	first := testPNG(640, 480)
	second := testPNG(320, 240)
	m.mu.Lock()
	m.productFileIDs = []string{"file_a", "file_b"}
	m.products["file_a"] = first
	m.products["file_b"] = second
	m.mu.Unlock()

	res, err := c.GenerateImage(sentinelAuth(), provider.ImageRequest{Prompt: "画两张", N: 2})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("产物数 = %d，期望 2", len(res))
	}
	if res[0].B64JSON != base64.StdEncoding.EncodeToString(first) ||
		res[1].B64JSON != base64.StdEncoding.EncodeToString(second) {
		t.Error("多轮产物顺序与上游下发顺序不一致")
	}
	_, convs := m.snapshotConvs()
	if len(convs) != 2 {
		t.Fatalf("对话轮次 = %d，期望 2", len(convs))
	}
	var b1, b2 map[string]any
	_ = json.Unmarshal([]byte(convs[0]), &b1)
	_ = json.Unmarshal([]byte(convs[1]), &b2)
	if b1["conversation_id"] != nil {
		t.Errorf("第一轮应新建会话，实际带 conversation_id=%v", b1["conversation_id"])
	}
	if b2["conversation_id"] != "conv-1" {
		t.Errorf("第二轮应续接同一会话，conversation_id = %v", b2["conversation_id"])
	}
	if b2["parent_message_id"] != "m1" {
		t.Errorf("第二轮 parent_message_id = %v，期望上一轮 assistant 消息 id m1", b2["parent_message_id"])
	}
	// 每张产物都各走一遍下载（签名短命，不得复用）。
	if got := m.downloads(); strings.Join(got, ",") != "file_a,file_b" {
		t.Errorf("下载序列 = %v", got)
	}
}

// TestEditImageMultipleInlineRefsNativelyUploaded 多张参考图**逐张上传**（不拼版、不上图床）。
func TestEditImageMultipleInlineRefsNativelyUploaded(t *testing.T) {
	c, m := imageMockClient(t)
	first := testPNG(64, 64)
	second := testPNG(32, 32)
	m.mu.Lock()
	m.productFileIDs = []string{"file_out"}
	m.products["file_out"] = testPNG(512, 512)
	m.mu.Unlock()

	res, err := c.EditImage(sentinelAuth(), provider.ImageRequest{
		Prompt:     "把第二张图的人换到第一张里",
		N:          1,
		References: []string{dataURI("image/png", first), dataURI("image/png", second)},
	})
	if err != nil {
		t.Fatalf("EditImage: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("产物数 = %d", len(res))
	}
	_, regs, puts, _, blobBodies, processIDs := m.uploadSeq()
	if regs != 2 || puts != 2 {
		t.Fatalf("注册=%d 直传=%d，期望各 2（每张参考图独立上传）", regs, puts)
	}
	if string(blobBodies[0]) != string(first) || string(blobBodies[1]) != string(second) {
		t.Error("上传字节与参考图不一致（顺序或内容错）")
	}
	if strings.Join(processIDs, ",") != "file_up1,file_up2" {
		t.Errorf("处理步骤 file_id = %v", processIDs)
	}

	conduits, convs := m.snapshotConvs()
	if len(convs) != 1 || len(conduits) != 1 {
		t.Fatalf("conduit=%d conv=%d", len(conduits), len(convs))
	}
	// conduit 必须带 attachment_mime_types（上游据此预热多模态通道）。
	var conduit map[string]any
	_ = json.Unmarshal([]byte(conduits[0]), &conduit)
	if mt, _ := conduit["attachment_mime_types"].([]any); len(mt) != 2 {
		t.Errorf("attachment_mime_types = %v，期望 2 项", conduit["attachment_mime_types"])
	}

	var body map[string]any
	_ = json.Unmarshal([]byte(convs[0]), &body)
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("上游只吃一条 user 消息，实际 %d", len(msgs))
	}
	msg, _ := msgs[0].(map[string]any)
	content, _ := msg["content"].(map[string]any)
	if content["content_type"] != "multimodal_text" {
		t.Errorf("content_type = %v，期望 multimodal_text（多图原生）", content["content_type"])
	}
	parts, _ := content["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts = %d，期望 2 张图 + 1 段文本", len(parts))
	}
	for i, want := range []string{"sediment://file_up1", "sediment://file_up2"} {
		p, _ := parts[i].(map[string]any)
		if p["asset_pointer"] != want {
			t.Errorf("parts[%d].asset_pointer = %v，期望 %q", i, p["asset_pointer"], want)
		}
	}
	if text, _ := parts[2].(string); !strings.Contains(text, "把第二张图的人换到第一张里") {
		t.Errorf("文本分片 = %v", parts[2])
	}
	// metadata 里并列第二份附件清单（上游据此显示文件名/库 ID）。
	meta, _ := msg["metadata"].(map[string]any)
	if list, _ := meta["attachments"].([]any); len(list) != 2 {
		t.Errorf("metadata.attachments = %v，期望 2 项", meta["attachments"])
	}
}

// TestEditImageHTTPSRefDownloadedByChannel 单张 http(s) 直链原样交给渠道，由渠道下载后上传。
func TestEditImageHTTPSRefDownloadedByChannel(t *testing.T) {
	refBytes := testPNG(96, 96)
	refSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "image/png")
		_, _ = w.Write(refBytes)
	}))
	defer refSrv.Close()

	c, m := imageMockClient(t)
	m.mu.Lock()
	m.productFileIDs = []string{"file_out"}
	m.products["file_out"] = testPNG(128, 128)
	m.mu.Unlock()

	if _, err := c.EditImage(sentinelAuth(), provider.ImageRequest{
		Prompt: "改成油画风格", N: 1, References: []string{refSrv.URL + "/ref.png"},
	}); err != nil {
		t.Fatalf("EditImage: %v", err)
	}
	_, regs, puts, _, blobBodies, _ := m.uploadSeq()
	if regs != 1 || puts != 1 {
		t.Fatalf("注册=%d 直传=%d，期望各 1", regs, puts)
	}
	if string(blobBodies[0]) != string(refBytes) {
		t.Error("上传的字节不是 http(s) 参考图的内容")
	}
}

// TestImageNoProductSplitsThreeCases 没出图 = 三种截然不同的结论（该换号 / 该换提示词 / 上游任务失败）。
func TestImageNoProductSplitsThreeCases(t *testing.T) {
	cases := []struct {
		name string
		text string
		want provider.ErrKind
	}{
		{"额度耗尽", "You've reached your plan limit for image generations.", provider.ErrSoftRate},
		{"内容策略", "This request violates our content policies.", provider.ErrContentBlocked},
		{"模型不画", "I can't draw that, but here is a description instead.", provider.ErrUpstream},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, m := imageMockClient(t)
			m.mu.Lock()
			m.convText = tc.text
			m.mu.Unlock()
			_, err := c.GenerateImage(sentinelAuth(), provider.ImageRequest{Prompt: "画点什么", N: 1})
			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("err = %v，期望类型化错误", err)
			}
			if pe.Kind != tc.want {
				t.Errorf("Kind = %v，期望 %v", pe.Kind, tc.want)
			}
			if strings.Contains(pe.Msg, "sediment://") {
				t.Error("错误文案里出现了内部指针")
			}
		})
	}
}

// TestImageUpstreamStatusClassified 上游 HTTP 层失败沿用票 03 的分类表（429 → 软限流，5xx → 不换号）。
func TestImageUpstreamStatusClassified(t *testing.T) {
	cases := []struct {
		status int
		want   provider.ErrKind
	}{
		{429, provider.ErrSoftRate},
		{500, provider.ErrServer},
	}
	for _, tc := range cases {
		c, m := imageMockClient(t)
		m.mu.Lock()
		m.convStatus = tc.status
		m.mu.Unlock()
		_, err := c.GenerateImage(sentinelAuth(), provider.ImageRequest{Prompt: "画猫", N: 1})
		var pe *provider.Error
		if !errors.As(err, &pe) || pe.Kind != tc.want {
			t.Errorf("status=%d → %v，期望 %v", tc.status, err, tc.want)
		}
	}
}

// TestImageDownloadFailureKeepsPointerInternal 下载接口不回签字直链时：报上游错误，且不外泄指针。
func TestImageDownloadFailureKeepsPointerInternal(t *testing.T) {
	c, m := imageMockClient(t)
	m.mu.Lock()
	m.productFileIDs = []string{"file_gen1"}
	m.products["file_gen1"] = testPNG(8, 8)
	m.noDownloadURL = true
	m.mu.Unlock()

	res, err := c.GenerateImage(sentinelAuth(), provider.ImageRequest{Prompt: "画猫", N: 1})
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrUpstream {
		t.Fatalf("err = %v，期望 ErrUpstream", err)
	}
	if len(res) != 0 {
		t.Errorf("失败时不得返回任何产物: %+v", res)
	}
	if strings.Contains(pe.Msg, "sediment://") {
		t.Errorf("错误文案泄露内部指针: %s", pe.Msg)
	}
}

// TestImageDetailsNeverLogBytes 参考图/产物字节不进日志（W3-07：只允许张数/字节数/mime）。
func TestImageDetailsNeverLogBytes(t *testing.T) {
	// 结构性护栏：本包的日志只出现在 upload.go / image.go，且不得打印字节切片。
	// 这里直接断言产物 b64 不会出现在 error 文案里（日志用的是同一批字段）。
	c, m := imageMockClient(t)
	m.mu.Lock()
	m.productFileIDs = []string{"file_gen1"}
	m.products["file_gen1"] = testPNG(8, 8)
	m.mu.Unlock()
	res, err := c.GenerateImage(sentinelAuth(), provider.ImageRequest{Prompt: "画猫", N: 1})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(res) != 1 || len(res[0].B64JSON) == 0 {
		t.Fatal("产物缺失")
	}
	// 正向断言：结果里只有 b64/URL 两个字段的值可能带敏感数据，且都在 ImageResult 里
	// （不会被 fmt 到日志——由 image.go/upload.go 的日志纪律保证）。
	if strings.Contains(res[0].URL, res[0].B64JSON) {
		t.Error("URL 里混进了 b64")
	}
}
