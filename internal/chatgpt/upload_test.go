package chatgpt

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"wild-work/internal/provider"
)

// 上传 / 下载两步链路的离线假上游（票 07：参考图与产物指针的搬运）。
//
// 与票 03 的 chatMock 分开：这两个端点**不需要** sentinel 预检（上传/下载是低敏感调用），
// 单独一台假上游能把「四步/两步到底打了哪些请求、带了什么头」锁死。

type filesMock struct {
	mu sync.Mutex

	reservations  int
	registrations int
	blobPuts      int
	processed     []string // process_upload_stream 收到的 file_id
	downloads     []string // files/download/<file_id> 的 file_id

	// blobHeaders 记录对象存储那一步的完整头（断言不得带 Authorization）。
	blobHeaders []http.Header
	blobBodies  [][]byte

	// seq 记录请求序列（路径），用于断言步数。
	seq []string

	// 上游行为开关。
	failProcess  bool // process_upload_stream 不回 completed
	filesStatus  int  // POST /files 的状态码（0=200）
	downloadFail bool // DownloadImage 不返回 download_url

	// productBytes 是「产物直链」返回的字节。
	productBytes []byte
	srv          string
	nextFileID   string
}

func newFilesMock(t *testing.T) (*filesMock, *httptest.Server, *Client) {
	t.Helper()
	m := &filesMock{productBytes: testPNG(8, 6), nextFileID: "file_gen1"}
	mux := http.NewServeMux()
	mux.HandleFunc(PathUploadReservations, m.serveReservations)
	mux.HandleFunc(PathFiles, m.serveFiles)
	mux.HandleFunc(PathProcessUpload, m.serveProcess)
	mux.HandleFunc(pathFileDownload, m.serveDownload)
	mux.HandleFunc("/blob/", m.serveBlob)
	mux.HandleFunc("/estuary/", m.serveEstuary)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m.srv = srv.URL
	c := NewWithConfig(Config{Base: srv.URL, DataDir: t.TempDir()})
	t.Cleanup(c.Close)
	return m, srv, c
}

func (m *filesMock) serveReservations(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.reservations++
	m.seq = append(m.seq, r.URL.Path)
	m.mu.Unlock()
	writeJSONTest(w, map[string]any{"eligible": true, "reservation_id": "rsv-1",
		"upload_url": m.srv + "/blob/reserved"})
}

func (m *filesMock) serveFiles(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.registrations++
	m.seq = append(m.seq, r.URL.Path)
	st := m.filesStatus
	m.mu.Unlock()
	if st != 0 {
		w.WriteHeader(st)
		_, _ = w.Write([]byte(`{"detail":"boom"}`))
		return
	}
	writeJSONTest(w, map[string]any{"file_id": "file_up1", "upload_url": m.srv + "/blob/up1"})
}

func (m *filesMock) serveBlob(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.blobPuts++
	m.seq = append(m.seq, r.URL.Path)
	m.blobHeaders = append(m.blobHeaders, r.Header.Clone())
	m.blobBodies = append(m.blobBodies, b)
	m.mu.Unlock()
	w.WriteHeader(http.StatusCreated)
}

func (m *filesMock) serveProcess(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	m.mu.Lock()
	m.seq = append(m.seq, r.URL.Path)
	m.processed = append(m.processed, str(body["file_id"]))
	fail := m.failProcess
	m.mu.Unlock()
	w.Header().Set("content-type", "text/event-stream")
	if fail {
		// 只有「处理开始」帧：缺 completed ⇒ file_id 不可用。
		_, _ = w.Write([]byte(`{"event":"file.processing.started"}` + "\n"))
		return
	}
	// 声明是 SSE，但 body 是**每行裸 JSON**（无 data: 前缀）。
	_, _ = w.Write([]byte(`{"event":"file.processing.started"}` + "\n" +
		`{"event":"file.processing.file_ready"}` + "\n" +
		`{"event":"file.processing.completed","extra":{"metadata_object_id":"lib_1"}}` + "\n"))
}

func (m *filesMock) serveDownload(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, pathFileDownload)
	m.mu.Lock()
	m.seq = append(m.seq, r.URL.Path)
	m.downloads = append(m.downloads, id)
	fail := m.downloadFail
	m.mu.Unlock()
	if fail {
		writeJSONTest(w, map[string]any{"status": "success"})
		return
	}
	// 每次都给一个**新的**签名（有效期短 ⇒ 不得缓存复用）。
	writeJSONTest(w, map[string]any{"status": "success", "file_name": id + ".png",
		"download_url": m.srv + "/estuary/" + id + "?sig=" + id + "-sig"})
}

func (m *filesMock) serveEstuary(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.seq = append(m.seq, r.URL.Path)
	m.mu.Unlock()
	w.Header().Set("content-type", "image/png")
	_, _ = w.Write(m.productBytes)
}

func (m *filesMock) snapshot() (seq []string, blobHdrs []http.Header, blobBodies [][]byte, processed, downloads []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seq...), append([]http.Header(nil), m.blobHeaders...),
		append([][]byte(nil), m.blobBodies...), append([]string(nil), m.processed...),
		append([]string(nil), m.downloads...)
}

// testPNG 造一个「只看头」的 PNG：imageSize 只读 IHDR 的宽高，不解码整图。
func testPNG(w, h int) []byte {
	b := []byte("\x89PNG\r\n\x1a\n")
	b = append(b, 0, 0, 0, 13) // IHDR 长度
	b = append(b, 'I', 'H', 'D', 'R')
	wd := make([]byte, 4)
	binary.BigEndian.PutUint32(wd, uint32(w))
	b = append(b, wd...)
	hd := make([]byte, 4)
	binary.BigEndian.PutUint32(hd, uint32(h))
	b = append(b, hd...)
	return append(b, 8, 6, 0, 0, 0) // bit depth / color type / 填充（凑够 24+ 字节）
}

// TestUploadImageWalksFourSteps 预占 → 登记 → 直传对象存储（**不带 Authorization**）→ 处理。
func TestUploadImageWalksFourSteps(t *testing.T) {
	m, _, c := newFilesMock(t)
	png := testPNG(1024, 768)

	att, err := c.UploadImage(sentinelAuth(), png, "", "")
	if err != nil {
		t.Fatalf("UploadImage: %v", err)
	}
	if att.FileID != "file_up1" || att.MimeType != "image/png" || att.SizeBytes != int64(len(png)) {
		t.Errorf("attachment = %+v", att)
	}
	if att.Width != 1024 || att.Height != 768 {
		t.Errorf("宽高 = %d×%d，期望 1024×768", att.Width, att.Height)
	}
	seq, hdrs, bodies, processed, _ := m.snapshot()
	want := []string{PathUploadReservations, PathFiles, "/blob/up1", PathProcessUpload}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("请求序列 = %v，期望 %v", seq, want)
	}
	if len(hdrs) != 1 {
		t.Fatalf("对象存储 PUT 次数 = %d", len(hdrs))
	}
	// 改动锚点：对象存储那一步多带一个 Authorization 会被拒。
	if got := hdrs[0].Get("Authorization"); got != "" {
		t.Errorf("对象存储 PUT 带了 Authorization（%q）⇒ 会被上游拒", got)
	}
	if got := hdrs[0].Get("x-ms-blob-type"); got != "BlockBlob" {
		t.Errorf("x-ms-blob-type = %q", got)
	}
	if len(bodies) != 1 || string(bodies[0]) != string(png) {
		t.Error("对象存储收到的字节与参考图不一致")
	}
	if len(processed) != 1 || processed[0] != "file_up1" {
		t.Errorf("处理步骤收到 file_id = %v", processed)
	}
}

// TestUploadImageProcessIncompleteIsUpstreamError 缺 completed 帧 ⇒ 不换号的「上游错误」。
func TestUploadImageProcessIncompleteIsUpstreamError(t *testing.T) {
	m, _, c := newFilesMock(t)
	m.failProcess = true
	_, err := c.UploadImage(sentinelAuth(), testPNG(4, 4), "a.png", "image/png")
	var pe *provider.Error
	if err == nil || !asProviderErrorIs(err, &pe) || pe.Kind != provider.ErrUpstream {
		t.Fatalf("err = %v，期望 ErrUpstream（不换号、不罚账号）", err)
	}
}

// TestUploadImageUnknownMimeRejectedLocally 魔数不认识就在本地拒（不打上游）。
func TestUploadImageUnknownMimeRejectedLocally(t *testing.T) {
	m, _, c := newFilesMock(t)
	_, err := c.UploadImage(sentinelAuth(), []byte("not an image"), "", "")
	var pe *provider.Error
	if err == nil || !asProviderErrorIs(err, &pe) || pe.Kind != provider.ErrBadParams {
		t.Fatalf("err = %v，期望 ErrBadParams", err)
	}
	if seq, _, _, _, _ := m.snapshot(); len(seq) != 0 {
		t.Errorf("本地拒绝后不得打上游，实际 %v", seq)
	}
}

// TestDownloadImageTwoStepsAndNoCache 指针 → 签名直链 → 字节；两次都要各走一遍（不缓存）。
func TestDownloadImageTwoStepsAndNoCache(t *testing.T) {
	m, _, c := newFilesMock(t)
	body, name, signed, err := c.DownloadImage(sentinelAuth(), "file_gen1", "c-1")
	if err != nil {
		t.Fatalf("DownloadImage: %v", err)
	}
	if string(body) != string(m.productBytes) {
		t.Errorf("字节长度 = %d，期望 %d", len(body), len(m.productBytes))
	}
	if name != "file_gen1.png" {
		t.Errorf("name = %q", name)
	}
	if !strings.Contains(signed, "/estuary/file_gen1") || !strings.Contains(signed, "sig=") {
		t.Errorf("签名直链 = %q", signed)
	}
	seq, _, _, _, downloads := m.snapshot()
	want := []string{pathFileDownload + "file_gen1", "/estuary/file_gen1"}
	if strings.Join(seq, ",") != strings.Join(want, ",") {
		t.Fatalf("请求序列 = %v，期望 %v", seq, want)
	}

	// 第二次：签名有效期短 ⇒ 必须重新两步，不得复用上次直链。
	if _, _, _, err := c.DownloadImage(sentinelAuth(), "file_gen1", "c-1"); err != nil {
		t.Fatalf("第二次 DownloadImage: %v", err)
	}
	if _, _, _, _, dl := m.snapshot(); len(dl) != 2 || downloads[0] != dl[1] {
		t.Errorf("两次下载的 file_id = %v（%v），每次都必须重走下载接口", downloads, dl)
	}
}

// TestDownloadImageMissingURLIsUpstreamError 上游回 200 但没有 download_url ⇒ 上游错误。
func TestDownloadImageMissingURLIsUpstreamError(t *testing.T) {
	m, _, c := newFilesMock(t)
	m.downloadFail = true
	_, _, _, err := c.DownloadImage(sentinelAuth(), "file_x", "c-1")
	var pe *provider.Error
	if err == nil || !asProviderErrorIs(err, &pe) || pe.Kind != provider.ErrUpstream {
		t.Fatalf("err = %v，期望 ErrUpstream", err)
	}
}

// asProviderErrorIs 是 errors.As 的测试内联写法（避免每个用例重复导入 errors）。
func asProviderErrorIs(err error, target **provider.Error) bool {
	for err != nil {
		if pe, ok := err.(*provider.Error); ok {
			*target = pe
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
