// handler_images_test.go images 端点回归：/v1/images/generations + /v1/images/edits。
// mock 一台「chatglm.cn」（IDE 形态信封/SSE 生图帧）+ 一台图床，全链路离线验证。
package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/glm"
	"wild-work/internal/imgutil"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// mockGLMUp 模拟 chatglm.cn 的生图相关接口。
type mockGLMUp struct {
	mu sync.Mutex

	lastGenBody  map[string]any // 最后一次 assistant/stream 的请求体
	lastEditBody map[string]any // 最后一次 image/reference 的请求体
	lastEditRaw  string         // image/reference 收到的 image_url
	editCalls    int
	streamCalls  int
	genStatus    int // 文生图响应的 HTTP 状态（0=200）
	genFrames    []string
	editImageURL string // image/reference 返回的产物 URL
}

func (m *mockGLMUp) mux(t *testing.T, imgBytes []byte) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/chatglm/user-api/user/refresh", func(w http.ResponseWriter, r *http.Request) {
		writeGLMEnv(w, map[string]any{"access_token": "AT-new", "refresh_token": "RT-new", "user_id": "u1"})
	})
	mux.HandleFunc("/chatglm/backend-api/assistant/stream", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.lastGenBody = body
		m.streamCalls++
		st := m.genStatus
		frames := m.genFrames
		m.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			_, _ = w.Write([]byte(`{"status":40012,"message":"blocked"}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		for _, f := range frames {
			_, _ = w.Write([]byte("data: " + f + "\n\n"))
		}
		// 流末尾的裸 JSON 信封（不带 data: 前缀）——必须被解析器忽略
		_, _ = w.Write([]byte(`{"status":0,"message":"success","result":null,"rid":"x"}` + "\n"))
	})
	mux.HandleFunc("/chatglm/drawing-api/v1/image/reference", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.lastEditBody = body
		m.lastEditRaw, _ = body["image_url"].(string)
		m.editCalls++
		m.mu.Unlock()
		writeGLMEnv(w, map[string]any{"image_url": m.editImageURL, "conversation_id": "cid-edit"})
	})
	mux.HandleFunc("/img/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(imgBytes) // 产物直链（b64 下载用）
	})
	return mux
}

// writeGLMEnv 写清言统一信封。
func writeGLMEnv(w http.ResponseWriter, result map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": 0, "message": "success", "result": result})
}

// genImageFrame 构造一帧生图 SSE（单图 finish 帧）。
func genImageFrame(idx int, url string) string {
	return `{"id":"m1","conversation_id":"c1","status":"init","parts":[{"id":"m1","logic_id":"L` +
		itoaTest(idx) + `","status":"finish","content":[{"type":"image","code":"润色词` +
		itoaTest(idx) + `","image":[{"image_url":"` + url + `"}]}]}]}`
}

func itoaTest(n int) string {
	return string(rune('0' + n))
}

// tinyJPEG 生成一张 4x4 红色 JPEG（拼版/产物下载的测试素材）。
func tinyJPEG(t *testing.T) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for x := 0; x < 4; x++ {
		for y := 0; y < 4; y++ {
			im.Set(x, y, color.RGBA{R: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, im, &jpeg.Options{Quality: 80}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return buf.Bytes()
}

// newImagesHandler 装配 glm Runtime + mock 上游。
func newImagesHandler(t *testing.T) (*Handler, *mockGLMUp, *pool.Pool) {
	t.Helper()
	m := &mockGLMUp{editImageURL: "EDIT_PRODUCT_URL"}
	up := glm.NewWithBase("") // 先占位，下面指向 mock
	srv := httptest.NewServer(m.mux(t, tinyJPEG(t)))
	t.Cleanup(srv.Close)
	up = glm.NewWithBase(srv.URL)

	p := pool.New(t.TempDir() + "/state-glm.json")
	p.Add(&auth.Auth{UID: "glm-uid", AccessToken: "AT-ready", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})
	h := NewHandler(Config{
		Runtimes: map[provider.Kind]*Runtime{
			provider.GLM: {Kind: provider.GLM, Pool: p, Upstream: up, StaticModels: glm.StaticModels()},
		},
		APIKey: "", MaxRotate: 3, SoftCooldown: time.Minute, ErrThreshold: 3, ErrCooldown: time.Minute,
	})
	return h, m, p
}

// postImages 发 images 请求。
func postImages(h *Handler, path string, body []byte, ct string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestImagesGenerations 文生图：n=2 → 两张 b64 图 + revised_prompt；请求体带画图 agent 与比例。
func TestImagesGenerations(t *testing.T) {
	h, m, _ := newImagesHandler(t)
	// 3 帧：图1、图2、收尾（收尾帧顶层 finish）
	m.genFrames = []string{
		genImageFrame(1, "http://PLACE/1"),
		genImageFrame(2, "http://PLACE/2"),
		`{"id":"m1","status":"finish","parts":[{"id":"m1","status":"finish","content":[{"type":"one_to_more_finish","image":[{}]}]}]}`,
	}
	// 把 URL 换成 mock server 可达地址：genFrames 里的 URL 是死的，改用动态替换
	srvURL := h.glmRuntime().Upstream.(*glm.Client).Base
	for i, f := range m.genFrames {
		m.genFrames[i] = strings.Replace(f, "http://PLACE", srvURL+"/img", 1)
	}

	rec := postImages(h, "/v1/images/generations",
		[]byte(`{"model":"glm/image","prompt":"一只猫","n":2,"size":"1024x1024"}`), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Created int64 `json:"created"`
		Data    []struct {
			B64           string `json:"b64_json"`
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应非 JSON: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("data 项数 = %d, want 2", len(resp.Data))
	}
	for i, d := range resp.Data {
		if d.B64 == "" {
			t.Errorf("图 %d 缺 b64_json", i+1)
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(d.B64)
		if err != nil || len(raw) == 0 {
			t.Errorf("图 %d b64 非法: %v", i+1, err)
		}
		if d.RevisedPrompt == "" {
			t.Errorf("图 %d 缺 revised_prompt", i+1)
		}
	}
	// 上游请求体断言：画图 agent + 比例 + 去水印
	if m.lastGenBody["assistant_id"] != glm.DrawingAssistantID {
		t.Errorf("assistant_id = %v, want DrawingAssistantID", m.lastGenBody["assistant_id"])
	}
	meta, _ := m.lastGenBody["meta_data"].(map[string]any)
	cv, _ := meta["cogview"].(map[string]any)
	if cv["aspect_ratio"] != "1:1" {
		t.Errorf("cogview.aspect_ratio = %v, want 1:1", cv["aspect_ratio"])
	}
	if cv["rm_label_watermark"] != true {
		t.Errorf("cogview.rm_label_watermark = %v, want true", cv["rm_label_watermark"])
	}
}

// TestImagesGenerationsBadSize 非法 size 必须 400（上游会静默降级 1:1，本层拦截）。
func TestImagesGenerationsBadSize(t *testing.T) {
	h, _, _ := newImagesHandler(t)
	rec := postImages(h, "/v1/images/generations",
		[]byte(`{"prompt":"x","size":"2048x2048"}`), "application/json")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestImagesEditsSingleURL 单张 http URL：直传上游（不过图床），prompt 注入约束。
func TestImagesEditsSingleURL(t *testing.T) {
	h, m, _ := newImagesHandler(t)
	m.editImageURL = "http://127.0.0.1:1/product.jpg"

	rec := postImages(h, "/v1/images/edits",
		[]byte(`{"prompt":"把背景换成雪山","image":["http://127.0.0.1:1/ref.jpg"]}`), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if m.lastEditRaw != "http://127.0.0.1:1/ref.jpg" {
		t.Errorf("image_url = %q, want 原样直传", m.lastEditRaw)
	}
	prompt, _ := m.lastEditBody["prompt"].(string)
	if !strings.Contains(prompt, "不要复刻参考图的排版样式") || !strings.Contains(prompt, "把背景换成雪山") {
		t.Errorf("prompt 缺约束前缀或原文: %q", prompt)
	}
	if m.lastEditBody["assistant_id"] != glm.DrawingAssistantID {
		t.Errorf("assistant_id = %v, want DrawingAssistantID", m.lastEditBody["assistant_id"])
	}
}

// TestImagesEditsMultiBase64 多张 base64 图：拼版 → 图床 → image/reference。
func TestImagesEditsMultiBase64(t *testing.T) {
	h, m, _ := newImagesHandler(t)
	jpg := tinyJPEG(t)
	b64 := base64.StdEncoding.EncodeToString(jpg)
	m.editImageURL = "http://127.0.0.1:1/product.jpg"

	// mock 图床
	uploaded := false
	bedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploaded = true
		_, _ = w.Write([]byte(`{"code":200,"msg":"success","data":"https://bed.example/sheet.jpg"}`))
	}))
	defer bedSrv.Close()
	oldURL := imgutil.SangUploadURL
	imgutil.SangUploadURL = bedSrv.URL
	defer func() { imgutil.SangUploadURL = oldURL }()

	body := `{"prompt":"两个角色同框","image":["` + b64 + `","` + b64 + `"]}`
	rec := postImages(h, "/v1/images/edits", []byte(body), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if !uploaded {
		t.Fatal("参考图未上传图床（拼版路径未生效）")
	}
	if m.lastEditRaw != "https://bed.example/sheet.jpg" {
		t.Errorf("image_url = %q, want 拼版图床直链", m.lastEditRaw)
	}
	prompt, _ := m.lastEditBody["prompt"].(string)
	if !strings.Contains(prompt, "reference1") {
		t.Errorf("多图 prompt 缺 referenceN 定位说明: %q", prompt)
	}
}

// TestImagesGenerationsUpstream40012 上游 40012 → 429 + 模型级冷却（账号不整冷）。
func TestImagesGenerationsUpstream40012(t *testing.T) {
	h, m, p := newImagesHandler(t)
	m.genStatus = 400
	rec := postImages(h, "/v1/images/generations", []byte(`{"prompt":"x"}`), "application/json")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429（40012 归软限流）", rec.Code)
	}
	st, ok := p.Status("glm-uid")
	if !ok || !modelCooling(st, "glm/image") {
		t.Errorf("glm/image 应处于模型级冷却: %+v", st.ModelCooling)
	}
	if st.Cooling {
		t.Error("账号不应整冷（模型级冷却才对）")
	}
}

// TestImagesNoRuntime 未接入 glm 渠道 → 503 可操作引导。
func TestImagesNoRuntime(t *testing.T) {
	h := NewHandler(Config{APIKey: ""})
	rec := postImages(h, "/v1/images/generations", []byte(`{"prompt":"x"}`), "application/json")
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

// TestResolveReferenceURLDataURL 单张 data URI 也走托管（上游不收内联数据）。
func TestResolveReferenceURLDataURL(t *testing.T) {
	jpg := tinyJPEG(&testing.T{})
	bedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":"https://bed.example/x.jpg"}`))
	}))
	defer bedSrv.Close()
	oldURL := imgutil.SangUploadURL
	imgutil.SangUploadURL = bedSrv.URL
	defer func() { imgutil.SangUploadURL = oldURL }()

	dataURI := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpg)
	got, err := resolveReferenceURL([][]byte{[]byte(dataURI)})
	if err != nil {
		t.Fatalf("resolveReferenceURL: %v", err)
	}
	if got != "https://bed.example/x.jpg" {
		t.Errorf("got %q, want 图床直链（data URI 必须托管）", got)
	}
	_ = url.QueryEscape // 占位防未用
}

// TestImagesEditsMultipartBracketName gpt-image 系适配器（whalechan）用 image[] 字段名
// 的 multipart 形态：必须与 image 字段同等接收。
func TestImagesEditsMultipartBracketName(t *testing.T) {
	h, m, _ := newImagesHandler(t)
	m.editImageURL = "http://127.0.0.1:1/product.jpg"

	// 构造 multipart：image[] 文件字段（两张）+ prompt
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("model", "gpt-image-2"); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("prompt", "两个角色同框"); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("size", "1360x1024"); err != nil { // whalechan provider-native 会算出非五档像素
		t.Fatal(err)
	}
	jpg := tinyJPEG(t)
	for i := 0; i < 2; i++ {
		fw, err := mw.CreateFormFile("image[]", "ref.jpg")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(jpg); err != nil {
			t.Fatal(err)
		}
	}
	mw.Close()

	// mock 图床（多图走拼版+托管）
	bedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":200,"data":"https://bed.example/mp.jpg"}`))
	}))
	defer bedSrv.Close()
	oldURL := imgutil.SangUploadURL
	imgutil.SangUploadURL = bedSrv.URL
	defer func() { imgutil.SangUploadURL = oldURL }()

	rec := postImages(h, "/v1/images/edits", buf.Bytes(), mw.FormDataContentType())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if m.lastEditRaw != "https://bed.example/mp.jpg" {
		t.Errorf("image_url = %q, want 拼版图床直链", m.lastEditRaw)
	}
	prompt, _ := m.lastEditBody["prompt"].(string)
	if !strings.Contains(prompt, "两个角色同框") || !strings.Contains(prompt, "reference1") {
		t.Errorf("prompt 缺原文或 referenceN 说明: %q", prompt)
	}
}
