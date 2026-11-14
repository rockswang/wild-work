// image_test.go modelscope 生图/改图回归：异步 submit + 轮询、状态映射、退避与超时口径。
// 全部离线（httptest 假上游），不打真实上游（§0.1）。
package modelscope

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

// imageUp 假上游：记录 submit/poll 请求，按预设序列回状态。
type imageUp struct {
	mu sync.Mutex

	submitBody   map[string]any
	submitHeader http.Header
	pollHeader   http.Header
	submitCalls  int
	pollCalls    int

	taskID    string
	statuses  []string // 依次回的任务状态；耗尽后一直回最后一个
	images    []string
	submitSt  int // submit HTTP 状态（0=200）
	pollSt    int // poll HTTP 状态（0=200）
	lastStatus string
}

func (u *imageUp) mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/images/generations", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		u.mu.Lock()
		u.submitBody = body
		u.submitHeader = r.Header.Clone()
		u.submitCalls++
		st := u.submitSt
		id := u.taskID
		u.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"task_id":"`+id+`","request_id":"r1"}`)
	})
	mux.HandleFunc("/v1/tasks/", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.pollHeader = r.Header.Clone()
		idx := u.pollCalls
		u.pollCalls++
		status := statusAt(u.statuses, idx)
		u.lastStatus = status
		imgs := u.images
		st := u.pollSt
		u.mu.Unlock()
		if st != 0 {
			w.WriteHeader(st)
			_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
			return
		}
		outs := make([]map[string]any, 0, len(imgs))
		for _, u := range imgs {
			outs = append(outs, map[string]any{"url": u})
		}
		body := map[string]any{"task_status": status, "request_id": "r1"}
		if status == statusSucceed {
			// 官方字段形状（output_images 是 URL 列表）
			body["output_images"] = imgs
			body["outputs"] = map[string]any{"images": outs}
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	return mux
}

func statusAt(statuses []string, idx int) string {
	if len(statuses) == 0 {
		return statusSucceed
	}
	if idx >= len(statuses) {
		return statuses[len(statuses)-1]
	}
	return statuses[idx]
}

// newImageClient 装配假上游 + 注入式轮询（fake 记录间隔，不真的等 2s+）。
func newImageClient(t *testing.T, u *imageUp) (*Client, *imageUp, *[]time.Duration) {
	t.Helper()
	srv := httptest.NewServer(u.mux())
	t.Cleanup(srv.Close)
	c := New(provider.ModelScope)
	c.Base = srv.URL
	var delays []time.Duration
	c.pollSleep = func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}
	return c, u, &delays
}

func testAuth() *auth.Auth {
	return &auth.Auth{UID: "ms-uid", AccessToken: "ms-test-token", ExpiresAt: FarFutureExpiry()}
}

// TestImageSubmitPollSuccess submit 头/体正确，PROCESSING×2 → SUCCEED，
// 退避序列 = [2s, 2.5s]，返回上游全部产物 URL。
func TestImageSubmitPollSuccess(t *testing.T) {
	u := &imageUp{
		taskID:   "tk-abc",
		statuses: []string{"PROCESSING", "PROCESSING", statusSucceed},
		images:   []string{"https://oss.example/1.png", "https://oss.example/2.png"},
	}
	c, up, delays := newImageClient(t, u)

	imgs, err := c.GenerateImage(testAuth(), provider.ImageRequest{
		Model: "modelscope/Qwen/Qwen-Image", Prompt: "一座雪山", Size: "1280x720", N: 1,
	})
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if len(imgs) != 2 || imgs[0].URL != "https://oss.example/1.png" || imgs[1].URL != "https://oss.example/2.png" {
		t.Fatalf("产物 = %+v, want 上游两张原样返回", imgs)
	}

	// submit 头：Bearer + JSON + 异步开关（缺开关上游会按同步处理，生图模型上表现为不返回）
	if got := up.submitHeader.Get("Authorization"); got != "Bearer ms-test-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := up.submitHeader.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}
	if got := up.submitHeader.Get("X-ModelScope-Async-Mode"); got != "true" {
		t.Errorf("X-ModelScope-Async-Mode = %q, want true", got)
	}
	// submit 体：model 是**去掉渠道前缀**的上游名，size 是像素串原值
	if got := up.submitBody["model"]; got != "Qwen/Qwen-Image" {
		t.Errorf("submit model = %v, want Qwen/Qwen-Image（SplitModelID 第二返回值）", got)
	}
	if got := up.submitBody["prompt"]; got != "一座雪山" {
		t.Errorf("submit prompt = %v", got)
	}
	if got := up.submitBody["size"]; got != "1280x720" {
		t.Errorf("submit size = %v, want 像素串原值", got)
	}
	if _, has := up.submitBody["negative_prompt"]; has {
		t.Errorf("没给 negative_prompt 却发了: %v", up.submitBody)
	}
	// poll 头：任务类型
	if got := up.pollHeader.Get("X-ModelScope-Task-Type"); got != imageTaskTypeHdr {
		t.Errorf("X-ModelScope-Task-Type = %q, want %q", got, imageTaskTypeHdr)
	}
	if up.submitCalls != 1 || up.pollCalls != 3 {
		t.Errorf("submit=%d poll=%d, want 1/3", up.submitCalls, up.pollCalls)
	}

	want := []time.Duration{2 * time.Second, 2500 * time.Millisecond}
	if len(*delays) != len(want) {
		t.Fatalf("退避序列 = %v, want %v", *delays, want)
	}
	for i := range want {
		if (*delays)[i] != want[i] {
			t.Fatalf("退避序列 = %v, want %v", *delays, want)
		}
	}
}

// TestImagePollBackoffCapsAtFive 退避每次 +500ms、封顶 5s。
func TestImagePollBackoffCapsAtFive(t *testing.T) {
	u := &imageUp{
		taskID:   "tk-cap",
		statuses: []string{"PROCESSING", "PROCESSING", "PROCESSING", "PROCESSING", "PROCESSING", "PROCESSING", "PROCESSING", "PROCESSING", statusSucceed},
		images:   []string{"https://oss.example/x.png"},
	}
	c, _, delays := newImageClient(t, u)
	if _, err := c.GenerateImage(testAuth(), provider.ImageRequest{Model: "modelscope/Qwen/Qwen-Image", Prompt: "x"}); err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	want := []time.Duration{
		2 * time.Second, 2500 * time.Millisecond, 3 * time.Second, 3500 * time.Millisecond,
		4 * time.Second, 4500 * time.Millisecond, 5 * time.Second, 5 * time.Second,
	}
	if len(*delays) != len(want) {
		t.Fatalf("退避序列 = %v, want %v（封顶 5s）", *delays, want)
	}
	for i := range want {
		if (*delays)[i] != want[i] {
			t.Fatalf("第 %d 次退避 = %v, want %v（全序列 %v）", i+1, (*delays)[i], want[i], *delays)
		}
	}
}

// TestImagePollTimeoutCarriesTaskID 超时 = *provider.TaskTimeout（504 语义）：
// 带 task id、「任务可能仍在执行」，**不伪造**结果、不换号（Kind 不可重试）。
func TestImagePollTimeoutCarriesTaskID(t *testing.T) {
	u := &imageUp{taskID: "tk-slow", statuses: []string{"PROCESSING"}}
	srv := httptest.NewServer(u.mux())
	t.Cleanup(srv.Close)
	c := New(provider.ModelScope)
	c.Base = srv.URL
	c.pollLimit = 120 * time.Millisecond // 注入小上限，走真实计时器

	imgs, err := c.GenerateImage(testAuth(), provider.ImageRequest{Model: "modelscope/Qwen/Qwen-Image", Prompt: "x"})
	if err == nil {
		t.Fatalf("超时应报错，实际返回 %+v（不得伪造结果）", imgs)
	}
	var tt *provider.TaskTimeout
	if !errors.As(err, &tt) {
		t.Fatalf("错误类型 = %T (%v), want *provider.TaskTimeout", err, err)
	}
	if tt.TaskID != "tk-slow" {
		t.Errorf("TaskID = %q, want tk-slow", tt.TaskID)
	}
	msg := tt.Error()
	if !strings.Contains(msg, "tk-slow") || !strings.Contains(msg, "任务可能仍在执行") {
		t.Errorf("超时文案缺 task id 或「任务可能仍在执行」: %q", msg)
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrUpstream {
		t.Errorf("超时的 Kind = %v, want ErrUpstream", pe)
	}
	if pe.Kind.Rotatable() {
		t.Error("超时不得触发换号（换了号也拿不回已提交的任务）")
	}
}

// TestImageFailedAndCanceledAreUpstreamError FAILED / CANCELED → ErrUpstream（不可重试）。
func TestImageFailedAndCanceledAreUpstreamError(t *testing.T) {
	for _, status := range []string{statusFailed, statusCanceled} {
		t.Run(status, func(t *testing.T) {
			u := &imageUp{taskID: "tk-fail", statuses: []string{status}}
			c, up, _ := newImageClient(t, u)
			imgs, err := c.GenerateImage(testAuth(), provider.ImageRequest{Model: "modelscope/Qwen/Qwen-Image", Prompt: "x"})
			if err == nil {
				t.Fatalf("%s 应报错，实际返回 %+v", status, imgs)
			}
			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("错误类型 = %T (%v), want *provider.Error", err, err)
			}
			if pe.Kind != provider.ErrUpstream {
				t.Errorf("Kind = %v, want ErrUpstream", pe.Kind)
			}
			if pe.Kind.Rotatable() {
				t.Errorf("%s 不得触发换号（换号必然复现同一上游任务失败）", status)
			}
			if !strings.Contains(pe.Msg, "tk-fail") {
				t.Errorf("失败文案缺 task id: %q", pe.Msg)
			}
			if up.pollCalls != 1 {
				t.Errorf("poll 次数 = %d, want 1（终态立即结束，不继续轮询）", up.pollCalls)
			}
		})
	}
}

// TestImageSucceedWithoutOutputsIsUpstreamError 状态成功但没产物 URL = 上游故障，不伪造图。
func TestImageSucceedWithoutOutputsIsUpstreamError(t *testing.T) {
	u := &imageUp{taskID: "tk-empty", statuses: []string{statusSucceed}, images: nil}
	c, _, _ := newImageClient(t, u)
	if _, err := c.GenerateImage(testAuth(), provider.ImageRequest{Model: "modelscope/Qwen/Qwen-Image", Prompt: "x"}); err == nil {
		t.Fatal("无产物应报错（不得回空 data 伪装成功）")
	}
}

// TestImageUnknownStatusKeepsPolling 未知状态（上游新增）继续轮询，不当成失败。
func TestImageUnknownStatusKeepsPolling(t *testing.T) {
	u := &imageUp{taskID: "tk-new", statuses: []string{"RUNNING", "RUNNING", statusSucceed}, images: []string{"https://oss.example/a.png"}}
	c, up, delays := newImageClient(t, u)
	imgs, err := c.GenerateImage(testAuth(), provider.ImageRequest{Model: "modelscope/Qwen/Qwen-Image", Prompt: "x"})
	if err != nil {
		t.Fatalf("未知状态不应失败: %v", err)
	}
	if len(imgs) != 1 {
		t.Fatalf("产物数 = %d, want 1", len(imgs))
	}
	if up.pollCalls != 3 || len(*delays) != 2 {
		t.Errorf("poll=%d delays=%d, want 3/2", up.pollCalls, len(*delays))
	}
}

// TestImageSizePassthroughFiveBuckets 五档像素串**原样上行**（不是 glm 的比例串），
// 比例串按同构档位换算成像素串。
func TestImageSizePassthroughFiveBuckets(t *testing.T) {
	cases := []struct{ in, want string }{
		{"1024x1024", "1024x1024"},
		{"864x1152", "864x1152"},
		{"1152x864", "1152x864"},
		{"720x1280", "720x1280"},
		{"1280x720", "1280x720"},
		{"1:1", "1024x1024"},
		{"3:4", "864x1152"},
		{"4:3", "1152x864"},
		{"9:16", "720x1280"},
		{"16:9", "1280x720"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			u := &imageUp{taskID: "tk-size", statuses: []string{statusSucceed}, images: []string{"https://oss.example/s.png"}}
			c, up, _ := newImageClient(t, u)
			if _, err := c.GenerateImage(testAuth(), provider.ImageRequest{Model: "modelscope/Qwen/Qwen-Image", Prompt: "x", Size: tc.in}); err != nil {
				t.Fatalf("GenerateImage: %v", err)
			}
			if got := up.submitBody["size"]; got != tc.want {
				t.Errorf("size = %v, want %q（上游大小写敏感的像素串）", got, tc.want)
			}
		})
	}
}

// TestImageNSingleDefaultAndCapFour n<=0 → 1；>4 → 4。
func TestImageNSingleDefaultAndCapFour(t *testing.T) {
	cases := []struct {
		in   int
		want float64
	}{
		{0, 1}, {-3, 1}, {1, 1}, {2, 2}, {4, 4}, {9, 4},
	}
	for _, tc := range cases {
		u := &imageUp{taskID: "tk-n", statuses: []string{statusSucceed}, images: []string{"https://oss.example/n.png"}}
		c, up, _ := newImageClient(t, u)
		if _, err := c.GenerateImage(testAuth(), provider.ImageRequest{Model: "modelscope/Qwen/Qwen-Image", Prompt: "x", N: tc.in}); err != nil {
			t.Fatalf("n=%d: %v", tc.in, err)
		}
		if got := up.submitBody["n"]; got != tc.want {
			t.Errorf("n=%d → submit n = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestImageNegativePromptOnlyWhenGiven 客户端给了才发，不给就不编造。
func TestImageNegativePromptOnlyWhenGiven(t *testing.T) {
	u := &imageUp{taskID: "tk-neg", statuses: []string{statusSucceed}, images: []string{"https://oss.example/n.png"}}
	c, up, _ := newImageClient(t, u)
	if _, err := c.GenerateImage(testAuth(), provider.ImageRequest{
		Model: "modelscope/Qwen/Qwen-Image", Prompt: "x", NegativePrompt: "模糊",
	}); err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if got := up.submitBody["negative_prompt"]; got != "模糊" {
		t.Errorf("negative_prompt = %v, want 模糊", got)
	}
}

// TestImageEditPassesDataURLs 改图：多张参考图走 image_url[] **直传**（不拼版、不图床），
// 同一端点 + 上游模型名去前缀；prompt 不加拼版约束（那是 glm 单图上游的语义）。
func TestImageEditPassesDataURLs(t *testing.T) {
	u := &imageUp{taskID: "tk-edit", statuses: []string{statusSucceed}, images: []string{"https://oss.example/e.png"}}
	c, up, _ := newImageClient(t, u)
	refs := []string{
		"data:image/png;base64,AAAA",
		"data:image/jpeg;base64,BBBB",
	}
	imgs, err := c.EditImage(testAuth(), provider.ImageRequest{
		Model: "modelscope/Qwen/Qwen-Image-Edit", Prompt: "把两个人合成一张", Size: "1024x1024", N: 1, References: refs,
	})
	if err != nil {
		t.Fatalf("EditImage: %v", err)
	}
	if len(imgs) != 1 || imgs[0].URL != "https://oss.example/e.png" {
		t.Fatalf("产物 = %+v", imgs)
	}
	raw, _ := json.Marshal(up.submitBody["image_url"])
	var got []string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("image_url 形状 = %v (%v), want 字符串数组", up.submitBody["image_url"], err)
	}
	if len(got) != 2 || got[0] != refs[0] || got[1] != refs[1] {
		t.Errorf("image_url = %v, want 原样两张 data URI", got)
	}
	if up.submitBody["prompt"] != "把两个人合成一张" {
		t.Errorf("prompt = %v, want 原样（不得注入 glm 拼版约束）", up.submitBody["prompt"])
	}
	if up.submitBody["model"] != "Qwen/Qwen-Image-Edit" {
		t.Errorf("model = %v, want 去渠道前缀", up.submitBody["model"])
	}
}

// TestImageEditRequiresReference 无参考图 → ErrBadParams（请求级，不打上游）。
func TestImageEditRequiresReference(t *testing.T) {
	u := &imageUp{taskID: "tk-noref"}
	c, up, _ := newImageClient(t, u)
	_, err := c.EditImage(testAuth(), provider.ImageRequest{Model: "modelscope/Qwen/Qwen-Image-Edit", Prompt: "x"})
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Kind != provider.ErrBadParams {
		t.Fatalf("err = %v, want ErrBadParams", err)
	}
	if up.submitCalls != 0 {
		t.Errorf("无参考图仍打了上游 %d 次", up.submitCalls)
	}
}

// TestImagePollLimitWidensWithTimeout 轮询总上限 = max(120s, upstream.timeout_seconds)：
// 只放宽不压低（R110）。600s 配置必须生效，不能被常量 120s 吃掉。
func TestImagePollLimitWidensWithTimeout(t *testing.T) {
	c := New(provider.ModelScope)
	if got := c.imagePollLimit(); got != imageMinWait {
		t.Errorf("默认上限 = %v, want %v", got, imageMinWait)
	}
	c.HTTP.Timeout = 600 * time.Second
	if got := c.imagePollLimit(); got != 600*time.Second {
		t.Errorf("upstream.timeout_seconds=600s 时上限 = %v, want 600s（只放宽不压低）", got)
	}
	c.HTTP.Timeout = 30 * time.Second
	if got := c.imagePollLimit(); got != imageMinWait {
		t.Errorf("短超时不得压低轮询上限：%v, want %v", got, imageMinWait)
	}
}

// TestImageSubmitHTTPErrorClassified submit 的 4xx/5xx 走 Classify（401 ⇒ SessionDead 等），
// 不得被当成裸 error（那会被 handler 记成传输层错误、永不换号）。
func TestImageSubmitHTTPErrorClassified(t *testing.T) {
	u := &imageUp{taskID: "tk-401", submitSt: http.StatusUnauthorized}
	c, _, _ := newImageClient(t, u)
	_, err := c.GenerateImage(testAuth(), provider.ImageRequest{Model: "modelscope/Qwen/Qwen-Image", Prompt: "x"})
	var pe *provider.Error
	if !errors.As(err, &pe) {
		t.Fatalf("错误类型 = %T (%v), want *provider.Error", err, err)
	}
	if pe.Kind != provider.ErrSessionDead || !pe.Kind.Rotatable() {
		t.Errorf("Kind = %v, want ErrSessionDead（可换号）", pe.Kind)
	}
}

// TestImageTokenNeverLogged 生图路径的日志不得出现 token 明文（不变量 2）。
func TestImageTokenNeverLogged(t *testing.T) {
	const secret = "ms-0123456789abcdef0123456789abcdef"
	logs := captureLog(t)
	u := &imageUp{taskID: "tk-log", statuses: []string{"PROCESSING", statusFailed}}
	c, _, _ := newImageClient(t, u)
	_, _ = c.GenerateImage(&auth.Auth{AccessToken: secret}, provider.ImageRequest{Model: "modelscope/Qwen/Qwen-Image", Prompt: "x"})
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("生图日志泄露 token：\n%s", logs.String())
	}
}