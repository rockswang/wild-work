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
	"log"
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
		APIKey: "", SoftCooldown: time.Minute, ErrThreshold: 3, ErrCooldown: time.Minute,
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
	if !ok || !st.ModelCooling["glm/image"].After(time.Now()) {
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

// fakeImageGen 假「会画图」的渠道上游：实现 provider.ImageGenerator + provider.Upstream
// （嵌入 echoUpstream 复用聊天空实现），记录入参并可按需返回错误。
type fakeImageGen struct {
	echoUpstream
	mu        sync.Mutex
	genCalls  int
	editCalls int
	lastReq   provider.ImageRequest
	err       error
}

func (f *fakeImageGen) GenerateImage(_ *auth.Auth, req provider.ImageRequest) ([]provider.ImageResult, error) {
	f.mu.Lock()
	f.genCalls++
	f.lastReq = req
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return []provider.ImageResult{{URL: "https://example.invalid/gen.png", RevisedPrompt: req.Prompt}}, nil
}

func (f *fakeImageGen) EditImage(_ *auth.Auth, req provider.ImageRequest) ([]provider.ImageResult, error) {
	f.mu.Lock()
	f.editCalls++
	f.lastReq = req
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return []provider.ImageResult{{URL: "https://example.invalid/edit.png"}}, nil
}

// addImageRuntime 给 runtime 表补一个「会画图」的渠道（账号池 + 假上游），返回账号池。
func addImageRuntime(t *testing.T, h *Handler, kind provider.Kind, up provider.Upstream, uid string) *pool.Pool {
	t.Helper()
	p := pool.New(t.TempDir() + "/state-" + kind.String() + ".json")
	p.Add(&auth.Auth{UID: uid, AccessToken: "AT-" + uid, ExpiresAt: time.Now().Add(time.Hour).Unix()})
	h.cfg.Runtimes[kind] = &Runtime{Kind: kind, Pool: p, Upstream: up}
	return p
}

// TestImagesRoutesByModelName 渠道**按客户端请求的模型名**选择：
// `glm/image` 仍走 GLM 且打到上游的请求体与改造前逐字一致（画图 agent + 比例 + 提示词），
// 带渠道前缀的全串模型名路由到对应渠道（这里用假的上游验证没有被硬编码成 glm）。
func TestImagesRoutesByModelName(t *testing.T) {
	h, m, _ := newImagesHandler(t)
	decoy := &fakeImageGen{}
	addImageRuntime(t, h, provider.ModelScope, decoy, "ms-uid")

	// 生图帧（产物 URL 指向 mock 服务器）
	m.genFrames = []string{
		genImageFrame(1, "http://PLACE/1"),
		`{"id":"m1","status":"finish","parts":[{"id":"m1","status":"finish","content":[{"type":"one_to_more_finish","image":[{}]}]}]}`,
	}
	srvURL := h.glmRuntime().Upstream.(*glm.Client).Base
	for i, f := range m.genFrames {
		m.genFrames[i] = strings.Replace(f, "http://PLACE", srvURL+"/img", 1)
	}

	// ① glm/image：仍走 GLM，别的渠道零调用
	rec := postImages(h, "/v1/images/generations",
		[]byte(`{"model":"glm/image","prompt":"一只猫","n":1,"size":"1024x1024","response_format":"url"}`), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("glm/image status = %d: %s", rec.Code, rec.Body.String())
	}
	if decoy.genCalls != 0 {
		t.Errorf("glm/image 被打到了别的渠道（genCalls=%d, want 0）", decoy.genCalls)
	}
	if m.lastGenBody["assistant_id"] != glm.DrawingAssistantID {
		t.Errorf("assistant_id = %v, want DrawingAssistantID（改造前一致）", m.lastGenBody["assistant_id"])
	}
	meta, _ := m.lastGenBody["meta_data"].(map[string]any)
	if cv, _ := meta["cogview"].(map[string]any); cv["aspect_ratio"] != "1:1" || cv["rm_label_watermark"] != true {
		t.Errorf("cogview = %v, want aspect_ratio=1:1 + rm_label_watermark=true（改造前一致）", cv)
	}
	msgs, _ := m.lastGenBody["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages 数 = %d, want 1", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	content, _ := first["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content 数 = %d, want 1", len(content))
	}
	if item, _ := content[0].(map[string]any); item["text"] != "一只猫" {
		t.Errorf("user 文本 = %v, want 原提示词", item["text"])
	}

	// ② 全串模型名：路由到该渠道，像素串原值下传
	rec2 := postImages(h, "/v1/images/generations",
		[]byte(`{"model":"modelscope/Qwen/Qwen-Image","prompt":"雪山","n":1,"size":"1280x720","response_format":"url"}`), "application/json")
	if rec2.Code != http.StatusOK {
		t.Fatalf("modelscope 全串 status = %d: %s", rec2.Code, rec2.Body.String())
	}
	if decoy.genCalls != 1 {
		t.Errorf("genCalls = %d, want 1（按模型名路由）", decoy.genCalls)
	}
	if decoy.lastReq.Model != "modelscope/Qwen/Qwen-Image" {
		t.Errorf("渠道收到的 Model = %q, want 客户端请求全串", decoy.lastReq.Model)
	}
	if decoy.lastReq.Size != "1280x720" {
		t.Errorf("渠道收到的 Size = %q, want 像素串原值", decoy.lastReq.Size)
	}
	if m.streamCalls != 1 {
		t.Errorf("GLM 上游被多打了一次（streamCalls=%d, want 1）", m.streamCalls)
	}
}

// TestImagesUnknownChannelRejectedWithoutPenalty 未知渠道 / 渠道不支持生图 → **400**，
// 且不得触达任何账号池（404 会被归成 ErrNotFound=Rotatable ⇒ 换号 + NoteError ⇒ 整条渠道冷却）。
func TestImagesUnknownChannelRejectedWithoutPenalty(t *testing.T) {
	t.Run("未知渠道前缀", func(t *testing.T) {
		h, m, p := newImagesHandler(t)
		rec := postImages(h, "/v1/images/generations",
			[]byte(`{"model":"nope/image","prompt":"x"}`), "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400（不得 404）: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "invalid_model") {
			t.Errorf("错误码 = %s, want invalid_model", rec.Body.String())
		}
		if m.streamCalls != 0 {
			t.Errorf("上游被请求 %d 次, want 0", m.streamCalls)
		}
		if st, ok := p.Status("glm-uid"); ok && (st.ErrCount != 0 || st.Cooling) {
			t.Errorf("账号被惩罚: %+v", st)
		}
	})

	t.Run("渠道不支持生图", func(t *testing.T) {
		h, _, _ := newImagesHandler(t)
		// traework 的假上游只实现对话（provider.Upstream），不是 ImageGenerator
		tp := pool.New(t.TempDir() + "/state-tw.json")
		tp.Add(&auth.Auth{UID: "tw-uid", AccessToken: "AT-tw", ExpiresAt: time.Now().Add(time.Hour).Unix()})
		h.cfg.Runtimes[provider.TraeWork] = &Runtime{Kind: provider.TraeWork, Pool: tp, Upstream: echoUpstream{}}

		rec := postImages(h, "/v1/images/generations",
			[]byte(`{"model":"traework/solo","prompt":"x"}`), "application/json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "invalid_model") {
			t.Errorf("错误码 = %s, want invalid_model", rec.Body.String())
		}
		st, ok := tp.Status("tw-uid")
		if !ok {
			t.Fatal("账号缺失")
		}
		if st.ErrCount != 0 || st.Cooling {
			t.Errorf("账号被惩罚（errCount=%d cooling=%v）, want 全零", st.ErrCount, st.Cooling)
		}
	})
}

// TestImageRuntimeErrorLogsOwnPlatform 日志与错误文案必须报**实际渠道**的显示名，
// 不得沿用改造前写死的 glm（否则第二个画图渠道出错会被误报成 glm）。
func TestImageRuntimeErrorLogsOwnPlatform(t *testing.T) {
	h, _, _ := newImagesHandler(t)
	fake := &fakeImageGen{err: &provider.Error{Kind: provider.ErrServer, Msg: "boom"}}
	addImageRuntime(t, h, provider.ModelScope, fake, "ms-uid")

	var buf bytes.Buffer
	oldOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldOut)

	rec := postImages(h, "/v1/images/generations",
		[]byte(`{"model":"modelscope/Qwen/Qwen-Image","prompt":"x"}`), "application/json")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), provider.DisplayName(provider.ModelScope)+" 生图失败") {
		t.Errorf("错误文案缺渠道显示名: %s", rec.Body.String())
	}
	logs := buf.String()
	if !strings.Contains(logs, "platform="+provider.DisplayName(provider.ModelScope)) {
		t.Errorf("日志缺渠道显示名: %q", logs)
	}
	if strings.Contains(logs, "glm") {
		t.Errorf("日志把渠道错写成 glm: %q", logs)
	}
}

// TestImagesBareModelFallsBackToLegacyGLM 无 `<kind>/` 前缀的裸模型名（OpenAI 的
// gpt-image-1/2）视同未指定：改造前 model 是忽略的，老客户端不得因此变红。
func TestImagesBareModelFallsBackToLegacyGLM(t *testing.T) {
	h, m, _ := newImagesHandler(t)
	m.genFrames = []string{
		`{"id":"m1","status":"finish","parts":[{"id":"m1","status":"finish","content":[{"type":"one_to_more_finish","image":[{}]}]}]}`,
	}
	rec := postImages(h, "/v1/images/generations",
		[]byte(`{"model":"gpt-image-2","prompt":"x","response_format":"url"}`), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（裸模型名回落 glm/image）: %s", rec.Code, rec.Body.String())
	}
	if m.streamCalls != 1 {
		t.Errorf("GLM streamCalls = %d, want 1", m.streamCalls)
	}
}

// TestImagesModelCooldownKeyedByRequestedModel 软限流的模型级冷却 key 必须是**客户端
// 请求的 model 全串**（不得退回写死的 glm/image）：否则第二个画图渠道限流会冻错 key，
// 既救不了自己、又把 glm/image 无辜冻住。
func TestImagesModelCooldownKeyedByRequestedModel(t *testing.T) {
	h, _, _ := newImagesHandler(t)
	fake := &fakeImageGen{err: &provider.Error{Kind: provider.ErrSoftRate, Msg: "429"}}
	mp := addImageRuntime(t, h, provider.ModelScope, fake, "ms-uid")

	const model = "modelscope/Qwen/Qwen-Image"
	rec := postImages(h, "/v1/images/generations",
		[]byte(`{"model":"`+model+`","prompt":"x"}`), "application/json")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", rec.Code, rec.Body.String())
	}
	st, ok := mp.Status("ms-uid")
	if !ok {
		t.Fatal("账号缺失")
	}
	if !st.ModelCooling[model].After(time.Now()) {
		t.Errorf("冷却 key 应为请求全串 %q: %+v", model, st.ModelCooling)
	}
	if _, poisoned := st.ModelCooling[legacyImageModel]; poisoned {
		t.Errorf("不得把冷却挂在写死的 %q 上: %+v", legacyImageModel, st.ModelCooling)
	}
	if st.Cooling {
		t.Error("账号不应整冷（模型级冷却才对）")
	}
}
