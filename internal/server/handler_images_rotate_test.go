// handler_images_rotate_test.go 生图请求内换号 + 异步任务超时 + 直传参考图（票 05）。
package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/pool"
	"wild-work/internal/provider"
)

// uidImageGen 按**账号 uid** 决定返回结果/错误，记录调用顺序（换号断言用）。
type uidImageGen struct {
	echoUpstream
	mu      sync.Mutex
	calls   []string
	lastReq provider.ImageRequest
	errBy   func(uid string) error
	results []provider.ImageResult
	inline  bool
}

func (g *uidImageGen) record(a *auth.Auth, req provider.ImageRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, a.UID)
	g.lastReq = req
	if g.errBy == nil {
		return nil
	}
	return g.errBy(a.UID)
}

func (g *uidImageGen) GenerateImage(a *auth.Auth, req provider.ImageRequest) ([]provider.ImageResult, error) {
	if err := g.record(a, req); err != nil {
		return nil, err
	}
	if len(g.results) > 0 {
		return g.results, nil
	}
	return []provider.ImageResult{{URL: "https://example.invalid/gen.png"}}, nil
}

func (g *uidImageGen) EditImage(a *auth.Auth, req provider.ImageRequest) ([]provider.ImageResult, error) {
	if err := g.record(a, req); err != nil {
		return nil, err
	}
	if len(g.results) > 0 {
		return g.results, nil
	}
	return []provider.ImageResult{{URL: "https://example.invalid/edit.png"}}, nil
}

// InlineImageRefs 声明「参考图直传」能力（inline=false 时等同默认 glm 语义）。
func (g *uidImageGen) InlineImageRefs() bool { return g.inline }

func (g *uidImageGen) calledUIDs() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func (g *uidImageGen) req() provider.ImageRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lastReq
}

// addImagePool 给 runtime 表装一个「会画图」的渠道（账号池 + 假上游）。
func addImagePool(t *testing.T, h *Handler, kind provider.Kind, up provider.Upstream, uids ...string) *pool.Pool {
	t.Helper()
	p := pool.New(t.TempDir() + "/state-" + kind.String() + ".json")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "AT-" + uid, ExpiresAt: time.Now().Add(time.Hour).Unix()})
	}
	h.cfg.Runtimes[kind] = &Runtime{Kind: kind, Pool: p, Upstream: up}
	return p
}

// TestImageRotatesAcrossAccountsUpToPoolSize 账号级可重试错误换下一个号（上限 = 池内账号数）：
// 首号软限流 → 换号成功；两号全灭 → 请求数正好等于池大小（不是 1、也不是更多）。
func TestImageRotatesAcrossAccountsUpToPoolSize(t *testing.T) {
	const model = "modelscope/Qwen/Qwen-Image"

	t.Run("首号可重试错误→换号成功", func(t *testing.T) {
		h, _, _ := newImagesHandler(t)
		g := &uidImageGen{errBy: func(uid string) error {
			if uid == "u1" {
				return &provider.Error{Kind: provider.ErrSoftRate, Msg: "429 slow down"}
			}
			return nil
		}}
		mp := addImagePool(t, h, provider.ModelScope, g, "u1", "u2")

		rec := postImages(h, "/v1/images/generations",
			[]byte(`{"model":"`+model+`","prompt":"x","response_format":"url"}`), "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200（换号后应成功）: %s", rec.Code, rec.Body.String())
		}
		calls := g.calledUIDs()
		if len(calls) != 2 || calls[0] != "u1" || calls[1] != "u2" {
			t.Fatalf("调用顺序 = %v, want [u1 u2]", calls)
		}
		// 首号按模型级冷却（不是整号冷）
		if st, _ := mp.Status("u1"); !st.ModelCooling[model].After(time.Now()) {
			t.Errorf("首号应处于模型级冷却: %+v", st.ModelCooling)
		}
		if st, _ := mp.Status("u1"); st.Cooling {
			t.Error("软限流不得整号冷却")
		}
	})

	t.Run("全池耗尽→首因透传且请求数=池大小", func(t *testing.T) {
		h, _, _ := newImagesHandler(t)
		g := &uidImageGen{errBy: func(uid string) error {
			return &provider.Error{Kind: provider.ErrSoftRate, Msg: "429 " + uid}
		}}
		addImagePool(t, h, provider.ModelScope, g, "u1", "u2")

		rec := postImages(h, "/v1/images/generations",
			[]byte(`{"model":"`+model+`","prompt":"x"}`), "application/json")
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429（首因是软限流）: %s", rec.Code, rec.Body.String())
		}
		calls := g.calledUIDs()
		if len(calls) != 2 {
			t.Fatalf("请求数 = %d (%v), want 2（= 池内账号数，每号一次）", len(calls), calls)
		}
		if !strings.Contains(rec.Body.String(), "429 u1") {
			t.Errorf("错误体应透传**首因**（u1），实际: %s", rec.Body.String())
		}
	})
}

// TestImageUpstreamFailureDoesNotRotate FAILED/CANCELED（ErrUpstream）不换号：
// 换个号也会拿到同一个已提交任务，白烧账号与时间。
func TestImageUpstreamFailureDoesNotRotate(t *testing.T) {
	h, _, _ := newImagesHandler(t)
	g := &uidImageGen{errBy: func(uid string) error {
		return &provider.Error{Kind: provider.ErrUpstream, Status: http.StatusBadGateway, Msg: "上游任务 tk-1 状态 FAILED"}
	}}
	addImagePool(t, h, provider.ModelScope, g, "u1", "u2")

	rec := postImages(h, "/v1/images/generations",
		[]byte(`{"model":"modelscope/Qwen/Qwen-Image","prompt":"x"}`), "application/json")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", rec.Code, rec.Body.String())
	}
	if calls := g.calledUIDs(); len(calls) != 1 {
		t.Fatalf("调用 = %v, want 仅 1 次（ErrUpstream 不可重试）", calls)
	}
}

// TestImagePollTimeout504WithTaskID 异步任务超时 → **504** + task_id + 「任务可能仍在执行」，
// 且不换号（换了号也拿不回已提交的任务）、不伪造结果。
func TestImagePollTimeout504WithTaskID(t *testing.T) {
	h, _, _ := newImagesHandler(t)
	g := &uidImageGen{errBy: func(string) error {
		return &provider.TaskTimeout{
			TaskID:  "tk-9f3",
			Elapsed: 2 * time.Minute,
			Err:     &provider.Error{Kind: provider.ErrUpstream, Status: http.StatusGatewayTimeout, Msg: "poll timeout"},
		}
	}}
	addImagePool(t, h, provider.ModelScope, g, "u1", "u2")

	rec := postImages(h, "/v1/images/generations",
		[]byte(`{"model":"modelscope/Qwen/Qwen-Image","prompt":"x"}`), "application/json")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "\"task_timeout\"") {
		t.Errorf("error.code = %s, want task_timeout", body)
	}
	if !strings.Contains(body, "tk-9f3") {
		t.Errorf("错误体缺 task_id（客户端无法据此续查）: %s", body)
	}
	if !strings.Contains(body, "任务可能仍在执行") {
		t.Errorf("错误体缺「任务可能仍在执行」提示: %s", body)
	}
	if calls := g.calledUIDs(); len(calls) != 1 {
		t.Errorf("超时不得换号：调用 = %v, want 1", calls)
	}
}

// TestImageEditMultipleDataURLs 直传型渠道：多张 data URI **原样直传**（不拼版、不上图床），
// 且 prompt 不注入 glm 的拼版约束。multipart 与 JSON 两种入站形态走同一条转换。
func TestImageEditMultipleDataURLs(t *testing.T) {
	refA := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("AAA"))
	refB := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("BBB"))

	t.Run("JSON", func(t *testing.T) {
		h, _, _ := newImagesHandler(t)
		g := &uidImageGen{inline: true}
		addImagePool(t, h, provider.ModelScope, g, "u1")

		payload, _ := json.Marshal(map[string]any{
			"model": "modelscope/Qwen/Qwen-Image-Edit", "prompt": "把两只猫合成一张",
			"image": []string{refA, refB},
		})
		rec := postImages(h, "/v1/images/edits", payload, "application/json")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		got := g.req()
		if len(got.References) != 2 || got.References[0] != refA || got.References[1] != refB {
			t.Fatalf("References = %v, want 原样两张 data URI（不拼版）", got.References)
		}
		if strings.Contains(got.Prompt, editPromptConstraint) || strings.Contains(got.Prompt, "reference1") {
			t.Errorf("prompt 被注入了 glm 拼版约束: %q", got.Prompt)
		}
		if !strings.Contains(got.Prompt, "把两只猫合成一张") {
			t.Errorf("prompt 丢了原文: %q", got.Prompt)
		}
	})

	t.Run("multipart", func(t *testing.T) {
		h, _, _ := newImagesHandler(t)
		g := &uidImageGen{inline: true}
		addImagePool(t, h, provider.ModelScope, g, "u1")

		jpg := tinyJPEG(t)
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("model", "modelscope/Qwen/Qwen-Image-Edit")
		_ = mw.WriteField("prompt", "改图")
		for i := 0; i < 2; i++ {
			fw, err := mw.CreateFormFile("image", "ref.jpg")
			if err != nil {
				t.Fatal(err)
			}
			_, _ = fw.Write(jpg)
		}
		_ = mw.Close()

		rec := postImages(h, "/v1/images/edits", buf.Bytes(), mw.FormDataContentType())
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
		}
		got := g.req()
		if len(got.References) != 2 {
			t.Fatalf("References 数 = %d, want 2（多图直传）", len(got.References))
		}
		for i, ref := range got.References {
			if !strings.HasPrefix(ref, "data:image/") || !strings.Contains(ref, ";base64,") {
				t.Errorf("参考图 %d = %q, want data URI 形态", i+1, ref)
			}
		}
	})
}

// TestImageEditOversizeRejected400 参考图总字节超上限 → **400 明确拒绝**，
// 不回落拼版（拼版改变多图语义），也不触达渠道。
func TestImageEditOversizeRejected400(t *testing.T) {
	old := maxInlineRefBytes
	maxInlineRefBytes = 100
	t.Cleanup(func() { maxInlineRefBytes = old })

	h, _, _ := newImagesHandler(t)
	g := &uidImageGen{inline: true}
	addImagePool(t, h, provider.ModelScope, g, "u1")

	big := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("A"), 200))
	payload, _ := json.Marshal(map[string]any{
		"model": "modelscope/Qwen/Qwen-Image-Edit", "prompt": "改图",
		"image": []string{"data:image/png;base64," + big, "data:image/png;base64," + big},
	})
	rec := postImages(h, "/v1/images/edits", payload, "application/json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "超过上限") || !strings.Contains(rec.Body.String(), "不做拼版") {
		t.Errorf("错误文案应明确拒绝原因: %s", rec.Body.String())
	}
	if calls := g.calledUIDs(); len(calls) != 0 {
		t.Errorf("超限请求不得触达渠道：调用 = %v", calls)
	}
}

// TestImagesMultipleResultsKeepAll 上游给几张就返几张（data[] 长度不与 n 对齐）。
func TestImagesMultipleResultsKeepAll(t *testing.T) {
	h, _, _ := newImagesHandler(t)
	g := &uidImageGen{results: []provider.ImageResult{
		{URL: "https://example.invalid/a.png"},
		{URL: "https://example.invalid/b.png"},
		{URL: "https://example.invalid/c.png"},
	}}
	addImagePool(t, h, provider.ModelScope, g, "u1")

	rec := postImages(h, "/v1/images/generations",
		[]byte(`{"model":"modelscope/Qwen/Qwen-Image","prompt":"x","n":1,"response_format":"url"}`), "application/json")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应非 JSON: %v", err)
	}
	if len(resp.Data) != 3 {
		t.Fatalf("data 项数 = %d, want 3（上游给几张返几张，不按 n 截断）", len(resp.Data))
	}
}