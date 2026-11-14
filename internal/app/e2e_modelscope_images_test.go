// e2e_modelscope_images_test.go spec user stories 9–13（绘图端点）。
//
// 这一层要证明的是「客户端视角是同步 OpenAI 形状，而上游其实是异步任务」这件事
// 真的被网关吃干了：submit 的异步头、poll 的任务类型头、退避、产物转换，
// 以及生图路径的**请求内换号**（R29 欠账）。

package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"wild-work/internal/modelscope"
	"wild-work/internal/provider"
)

// imageReq 构造一次文生图请求体。
func imageReq(model string, extra map[string]any) map[string]any {
	req := map[string]any{"model": model, "prompt": "一只戴墨镜的柴犬"}
	for k, v := range extra {
		req[k] = v
	}
	return req
}

// TestE2E_US9_GenerateImage 用户故事 9：
// 「在同一个 /v1/images/generations 端点里用 modelscope/Qwen/Qwen-Image 出图」。
func TestE2E_US9_GenerateImage(t *testing.T) {
	t.Run("默认 b64_json（下载转换）", func(t *testing.T) {
		cn := newMSFake(t, tinyPNG(t))
		env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-img-0001"}})
		const model = "modelscope/Qwen/Qwen-Image"

		resp, raw := env.post("/v1/images/generations", imageReq(model, map[string]any{"size": "1024x1024"}))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d，want 200：%s", resp.StatusCode, truncateForLog(raw))
		}
		data, _ := jsonMap(t, raw)["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("data 长度 = %d，want 1：%s", len(data), truncateForLog(raw))
		}
		item, _ := data[0].(map[string]any)
		b64 := asString(item["b64_json"])
		if b64 == "" {
			t.Fatalf("默认应回 b64_json，实际 item=%v", item)
		}
		img, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			t.Fatalf("b64_json 解码失败: %v", err)
		}
		if string(img) != string(tinyPNG(t)) {
			t.Error("b64 内容与上游产物不一致（下载/转换链路把字节改了）")
		}

		// —— 上游契约：异步 submit + 轮询 ——
		submits := cn.reqsByPath("/v1/images/generations")
		if len(submits) != 1 {
			t.Fatalf("submit 次数 = %d，want 1", len(submits))
		}
		if got := submits[0].Header.Get("X-ModelScope-Async-Mode"); got != "true" {
			t.Errorf("X-ModelScope-Async-Mode = %q，want true（缺它上游按同步处理，生图模型上会长时间不返回）", got)
		}
		if submits[0].Token != "ms-e2e-img-0001" {
			t.Error("submit 未携带该账号凭据")
		}
		var body map[string]any
		if err := json.Unmarshal(submits[0].Body, &body); err != nil {
			t.Fatalf("submit 体不是 JSON: %v", err)
		}
		if got := asString(body["model"]); got != "Qwen/Qwen-Image" {
			t.Errorf("submit model = %q，want 剥掉渠道前缀的上游名（D7/R52）", got)
		}
		if got := asString(body["size"]); got != "1024x1024" {
			t.Errorf("submit size = %q，want 像素串原样上行（D10）", got)
		}
		if got := asFloat(body["n"]); got != 1 {
			t.Errorf("submit n = %v，want 1（未指定时取 1）", got)
		}
		if _, ok := body["negative_prompt"]; ok {
			t.Error("客户端没给 negative_prompt，上游体里不得编造该字段（R49）")
		}

		polls := len(cn.reqsByPath("/v1/tasks/task-e2e-0001"))
		if polls == 0 {
			t.Fatalf("上游从未被轮询（task 路径不对？）实际路径集合见 reqsByPath 断言")
		}
		for _, p := range cn.reqsByPath("/v1/tasks/task-e2e-0001") {
			if got := p.Header.Get("X-ModelScope-Task-Type"); got != "image_generation" {
				t.Errorf("轮询 X-ModelScope-Task-Type = %q，want image_generation", got)
			}
		}
	})

	t.Run("response_format=url 时直传 OSS 直链", func(t *testing.T) {
		cn := newMSFake(t, tinyPNG(t))
		env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-img-0002"}})

		resp, raw := env.post("/v1/images/generations",
			imageReq("modelscope/Qwen/Qwen-Image", map[string]any{"response_format": "url"}))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d：%s", resp.StatusCode, truncateForLog(raw))
		}
		data, _ := jsonMap(t, raw)["data"].([]any)
		item, _ := data[0].(map[string]any)
		want := cn.URL() + "/files/out.png"
		if got := asString(item["url"]); got != want {
			t.Errorf("url = %q，want %q（url 模式不得下载转码）", got, want)
		}
		if _, ok := item["b64_json"]; ok {
			t.Error("url 模式不得同时回 b64_json")
		}
	})

	t.Run("比例串换算成像素串（与 glm 相反）", func(t *testing.T) {
		cn := newMSFake(t, tinyPNG(t))
		env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-img-0003"}})

		resp, raw := env.post("/v1/images/generations",
			imageReq("modelscope/Qwen/Qwen-Image", map[string]any{"size": "16:9", "n": 9}))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d：%s", resp.StatusCode, truncateForLog(raw))
		}
		submits := cn.reqsByPath("/v1/images/generations")
		var body map[string]any
		_ = json.Unmarshal(submits[len(submits)-1].Body, &body)
		if got := asString(body["size"]); got != "1280x720" {
			t.Errorf("size = %q，want 1280x720（魔搭收像素串，比例串由渠道换算）", got)
		}
		if got := asFloat(body["n"]); got != 4 {
			t.Errorf("n = %v，want 夹到 4（上游一次最多 4 张）", got)
		}
	})

	t.Run("上游给几张返几张（不与 n 对齐）", func(t *testing.T) {
		cn := newMSFake(t, tinyPNG(t))
		cn.mu.Lock()
		cn.outputCount = 3 // 上游一次回 3 张，而客户端只要 1 张
		cn.mu.Unlock()
		env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-img-multi"}})

		resp, raw := env.post("/v1/images/generations",
			imageReq("modelscope/Qwen/Qwen-Image", map[string]any{"n": 1}))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d：%s", resp.StatusCode, truncateForLog(raw))
		}
		data, _ := jsonMap(t, raw)["data"].([]any)
		if len(data) != 3 {
			t.Fatalf("data 长度 = %d，want 3（上游给了几张就返几张，不按 n 截断/补齐）", len(data))
		}
		for i, it := range data {
			m, _ := it.(map[string]any)
			if asString(m["b64_json"]) == "" {
				t.Errorf("第 %d 张缺 b64_json（默认 b64 模式）：%v", i+1, m)
			}
		}

		// url 模式：逐张保序返回（上游给的顺序就是客户端看到的顺序）。
		_, rawURL := env.post("/v1/images/generations",
			imageReq("modelscope/Qwen/Qwen-Image", map[string]any{"n": 1, "response_format": "url"}))
		dataURL, _ := jsonMap(t, rawURL)["data"].([]any)
		if len(dataURL) != 3 {
			t.Fatalf("url 模式 data 长度 = %d，want 3", len(dataURL))
		}
		seen := map[string]bool{}
		for i, it := range dataURL {
			m, _ := it.(map[string]any)
			u := asString(m["url"])
			if u == "" {
				t.Fatalf("第 %d 张缺 url：%v", i+1, m)
			}
			if want := fmt.Sprintf("%s/files/out%d.png", cn.URL(), i); u != want {
				t.Errorf("第 %d 张 url = %q，want %q（顺序不得重排）", i+1, u, want)
			}
			seen[u] = true
		}
		if len(seen) != 3 {
			t.Errorf("不同 URL 数 = %d，want 3", len(seen))
		}
	})

	t.Run("非法 size 本地 400 且零出站", func(t *testing.T) {
		cn := newMSFake(t, tinyPNG(t))
		env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-img-0004"}})

		resp, raw := env.post("/v1/images/generations",
			imageReq("modelscope/Qwen/Qwen-Image", map[string]any{"size": "1000x1000"}))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d，want 400：%s", resp.StatusCode, truncateForLog(raw))
		}
		if n := len(cn.reqsByPath("/v1/images/generations")); n != 0 {
			t.Errorf("非法 size 仍打了上游 %d 次（校验必须在本层）", n)
		}
	})
}

// TestE2E_US10_ImageFailuresAreHonest 用户故事 10：
// 「画图失败时看到真实原因（任务 FAILED / 超时 504 + task_id）」。
func TestE2E_US10_ImageFailuresAreHonest(t *testing.T) {
	t.Run("任务 FAILED：502 + 任务号 + 原文", func(t *testing.T) {
		cn := newMSFake(t, tinyPNG(t))
		cn.mu.Lock()
		cn.pollScript = []string{"FAILED"}
		cn.mu.Unlock()
		env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-img-fail"}})

		resp, raw := env.post("/v1/images/generations", imageReq("modelscope/Qwen/Qwen-Image", nil))
		if resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("status = %d，want 502（任务级失败）：%s", resp.StatusCode, truncateForLog(raw))
		}
		errObj, _ := jsonMap(t, raw)["error"].(map[string]any)
		msg := asString(errObj["message"])
		if !strings.Contains(msg, "task-e2e-0001") || !strings.Contains(msg, "FAILED") {
			t.Errorf("错误文案 = %q，want 带任务号与 FAILED（用户据此判断是换参数还是重试）", msg)
		}
		if code := asString(errObj["code"]); code != "upstream_error" {
			t.Errorf("error.code = %q，want upstream_error", code)
		}
		// FAILED 不得触发换号重放（任务级失败换号必复现，且要重新计费）。
		if n := len(cn.reqsByPath("/v1/images/generations")); n != 1 {
			t.Errorf("FAILED 后 submit 次数 = %d，want 1（不重试已提交任务）", n)
		}
	})

	t.Run("submit 限流：单号池如实回 429", func(t *testing.T) {
		cn := newMSFake(t, tinyPNG(t))
		cn.mu.Lock()
		cn.submitStatus["ms-e2e-img-429"] = http.StatusTooManyRequests
		cn.submitBody["ms-e2e-img-429"] = `{"error":{"message":"too many requests, please retry later"}}`
		cn.mu.Unlock()
		env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-img-429"}})

		resp, raw := env.post("/v1/images/generations", imageReq("modelscope/Qwen/Qwen-Image", nil))
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d，want 429（无号可换时透传首因）：%s", resp.StatusCode, truncateForLog(raw))
		}
		errObj, _ := jsonMap(t, raw)["error"].(map[string]any)
		if code := asString(errObj["code"]); code != "rate_limit_exceeded" {
			t.Errorf("error.code = %q，want rate_limit_exceeded", code)
		}
	})

	// 504 超时：用**测试专用钩子**把轮询上限/间隔压低（D8 的生产口径仍是
	// max(120s, timeout_seconds)，由 internal/modelscope 的单测锁死），
	// 于是这条整机级契约在默认套件里恒常运行。
	t.Run("任务超时：504 + task_id + 可能仍在执行", func(t *testing.T) {
		cn := newMSFake(t, tinyPNG(t))
		cn.mu.Lock()
		cn.pollScript = []string{"PROCESSING"} // 永远 PROCESSING → 直到轮询上限
		cn.mu.Unlock()
		env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-img-slow"}})
		up := env.msUP(provider.ModelScope)
		up.SetPollLimitForTest(800 * time.Millisecond)
		up.SetPollSleepForTest(func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond): // 不真的按退避序列等 2s+
			}
			return nil
		})

		start := time.Now()
		resp, raw := env.post("/v1/images/generations", imageReq("modelscope/Qwen/Qwen-Image", nil))
		if resp.StatusCode != http.StatusGatewayTimeout {
			t.Fatalf("status = %d，want 504：%s", resp.StatusCode, truncateForLog(raw))
		}
		errObj, _ := jsonMap(t, raw)["error"].(map[string]any)
		msg := asString(errObj["message"])
		if !strings.Contains(msg, "task-e2e-0001") || !strings.Contains(msg, "仍在执行") {
			t.Errorf("错误文案 = %q，want 带 task_id 与「仍在执行」提示", msg)
		}
		if code := asString(errObj["code"]); code != "task_timeout" {
			t.Errorf("error.code = %q，want task_timeout", code)
		}
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Errorf("轮询 %.1fs 才放弃，want 按注入的 800ms 上限收尾（钩子没接上？）", elapsed.Seconds())
		}
		// 超时不得伪造产物：data 字段必须缺席/为空。
		if data, ok := jsonMap(t, raw)["data"]; ok && data != nil {
			t.Errorf("超时响应带了 data（不得伪造结果，R36 口径）：%v", data)
		}
	})
}

// TestE2E_US11_EditsMultiReference 用户故事 11：
// 「参考图可以直接多张传入（edits），不必先拼版上传」。
func TestE2E_US11_EditsMultiReference(t *testing.T) {
	cn := newMSFake(t, tinyPNG(t))
	env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-edit-0001"}})

	refs := []string{
		"data:image/png;base64," + tinyPNGBase64,
		"data:image/png;base64," + tinyPNGBase64,
	}
	body := map[string]any{
		"model":  "modelscope/Qwen/Qwen-Image-Edit",
		"prompt": "把这两张图的人物合到一张合影里",
		"image":  refs,
	}
	resp, raw := env.post("/v1/images/edits", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d，want 200：%s", resp.StatusCode, truncateForLog(raw))
	}
	data, _ := jsonMap(t, raw)["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("data 长度 = %d，want 1", len(data))
	}
	item, _ := data[0].(map[string]any)
	if asString(item["b64_json"]) == "" {
		t.Errorf("改图默认也应回 b64_json，实际 %v", item)
	}

	submits := cn.reqsByPath("/v1/images/generations")
	if len(submits) != 1 {
		t.Fatalf("submit 次数 = %d，want 1", len(submits))
	}
	var up map[string]any
	if err := json.Unmarshal(submits[0].Body, &up); err != nil {
		t.Fatalf("submit 体不是 JSON: %v", err)
	}
	got, _ := up["image_url"].([]any)
	if len(got) != 2 {
		t.Fatalf("image_url 张数 = %d，want 2（多图直传，不拼版不上图床）", len(got))
	}
	for i := range refs {
		if asString(got[i]) != refs[i] {
			t.Errorf("第 %d 张参考图被改写（应为原样 data URI 直传，拼版会改变语义）：%.40s…",
				i+1, asString(got[i]))
		}
	}
	if got := asString(up["prompt"]); got != "把这两张图的人物合到一张合影里" {
		t.Errorf("prompt 被注入额外约束（拼版前缀是 glm 语义，不得用于直传渠道）：%q", got)
	}
}

// TestE2E_US11_EditsRejectsOversize 参考图总量超 20MB 必须 400 明确拒绝（不回落拼版）。
func TestE2E_US11_EditsRejectsOversize(t *testing.T) {
	if testing.Short() {
		t.Skip("构造 20MB+ 样本较慢，-short 下跳过")
	}
	cn := newMSFake(t, tinyPNG(t))
	env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-edit-big"}})

	// 两张 11MiB 的同构大图（内容不重要，只看字节预算）：合计 22MiB > 20MiB。
	big := base64.StdEncoding.EncodeToString(make([]byte, 11<<20))
	body := map[string]any{
		"model":  "modelscope/Qwen/Qwen-Image-Edit",
		"prompt": "超大参考图",
		"image":  []string{"data:image/png;base64," + big, "data:image/png;base64," + big},
	}
	resp, raw := env.post("/v1/images/edits", body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d，want 400：%s", resp.StatusCode, truncateForLog(raw))
	}
	errObj, _ := jsonMap(t, raw)["error"].(map[string]any)
	if msg := asString(errObj["message"]); !strings.Contains(msg, "上限") {
		t.Errorf("错误文案未说明字节上限：%q", msg)
	}
	if n := len(cn.reqsByPath("/v1/images/generations")); n != 0 {
		t.Errorf("超限请求仍打了上游 %d 次（应在本地拒绝，不回落拼版）", n)
	}
}

// TestE2E_US12_GlmImageUnchanged 用户故事 12：
// 「glm 生图行为一字不变」——抽 provider.ImageGenerator 之后，
// 不带渠道前缀的模型名（老客户端的 gpt-image-1 等）仍回落 glm/image，
// 且 modelscope 的接入不会把它截走。
func TestE2E_US12_GlmImageUnchanged(t *testing.T) {
	imgSrv := newMSFake(t, tinyPNG(t)) // 复用它当产物图服务器（只取 /files/out.png）
	glm := &glmStub{image: imgSrv.URL() + "/files/out.png"}
	cn := newMSFake(t, tinyPNG(t))
	env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-glm-0001"}, GLM: glm})

	for _, model := range []string{"", "glm/image", "gpt-image-1"} {
		req := map[string]any{"prompt": "一只戴墨镜的柴犬", "size": "1024x1024", "n": 9}
		if model != "" {
			req["model"] = model
		}
		resp, raw := env.post("/v1/images/generations", req)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("model=%q status = %d，want 200：%s", model, resp.StatusCode, truncateForLog(raw))
		}
		data, _ := jsonMap(t, raw)["data"].([]any)
		if len(data) != 1 {
			t.Fatalf("model=%q data 长度 = %d，want 1", model, len(data))
		}
		item, _ := data[0].(map[string]any)
		if asString(item["b64_json"]) == "" {
			t.Errorf("model=%q 默认应回 b64_json", model)
		}
	}

	gens := glm.gens()
	if len(gens) != 3 {
		t.Fatalf("glm 渠道收到 %d 次请求，want 3（空/glm/image/裸名全部回落 glm）", len(gens))
	}
	for i, req := range gens {
		if req.Model != "glm/image" {
			t.Errorf("第 %d 次 glm 请求 Model = %q，want glm/image（路由 key 与冷却 key 同一来源）", i+1, req.Model)
		}
		if req.Size != "1024x1024" {
			t.Errorf("第 %d 次 glm 请求 Size = %q，want 原始像素串（换算由渠道自己做）", i+1, req.Size)
		}
		if req.N != 4 {
			t.Errorf("第 %d 次 glm 请求 N = %d，want 夹到 4", i+1, req.N)
		}
	}
	if n := len(cn.reqsByPath("/v1/images/generations")); n != 0 {
		t.Errorf("glm 请求被 modelscope 截走 %d 次", n)
	}
	if n := len(cn.reqsByPath("/v1/chat/completions")); n != 0 {
		t.Errorf("生图请求误入对话路径 %d 次", n)
	}

	// 改图语义未被直传渠道串改：glm 收单张 http(s) 直链 + 拼版约束前缀，
	// 而 modelscope 收多张原样 data URI（见 US11）。这里只喂单张公网 URL，
	// 避免触发图床上传（那需要外网，属本层不可离线覆盖的部分）。
	refURL := imgSrv.URL() + "/files/ref.png"
	resp, raw := env.post("/v1/images/edits", map[string]any{
		"model": "glm/image", "prompt": "换成白天", "image": []string{refURL},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("glm 改图 status = %d：%s", resp.StatusCode, truncateForLog(raw))
	}
	if len(glm.editReqs) != 1 {
		t.Fatalf("glm 改图请求数 = %d，want 1", len(glm.editReqs))
	}
	edit := glm.editReqs[0]
	if len(edit.References) != 1 || edit.References[0] != refURL {
		t.Errorf("glm 改图 References = %v，want 原样单张直链（不下载不上传）", edit.References)
	}
	if !strings.Contains(edit.Prompt, "不要复刻参考图的排版样式") {
		t.Errorf("glm 改图缺少拼版约束前缀（默认语义被直传路径串改？）prompt=%q", edit.Prompt)
	}

	// D6/D12：生图模型要进 `/v1/models` 且带「能出图」标记——客户端靠它区分画图与对话模型
	// （旧行为：`glm/image` 只能从文档里知道，目录里根本没有它）。
	t.Run("glm 生图模型进 /v1/models 且带能出图标记", func(t *testing.T) {
		resp, raw := env.get("/v1/models")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("/v1/models = %d：%s", resp.StatusCode, truncateForLog(raw))
		}
		var list struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("目录响应不是合法 JSON: %v", err)
		}
		byID := make(map[string]map[string]any, len(list.Data))
		for _, e := range list.Data {
			byID[asString(e["id"])] = e
		}
		img := byID["glm/image"]
		if img == nil {
			t.Fatalf("目录缺 glm/image（客户端无法发现本渠道能画图）：%v", byID)
		}
		if v, _ := img["supports_image_generation"].(bool); !v {
			t.Errorf("glm/image 的 supports_image_generation = %v，want true", img["supports_image_generation"])
		}
		chat := byID["glm/chatglm:moe_53f"]
		if chat == nil {
			t.Fatalf("目录缺 glm 对话模型（静态表投放有问题）：%v", byID)
		}
		if _, has := chat["supports_image_generation"]; has {
			t.Error("对话模型带了 supports_image_generation（能看图/能出图与对话能力正交）")
		}
	})
}

// TestE2E_US13_ImageRotationWithinOneRequest 用户故事 13：
// 「生图遇到限流时同请求内换号，而不是把客户端打回 429」。
func TestE2E_US13_ImageRotationWithinOneRequest(t *testing.T) {
	const model = "modelscope/Qwen/Qwen-Image"

	t.Run("首号限流 → 换号成功（客户端只见 200）", func(t *testing.T) {
		cn := newMSFake(t, tinyPNG(t))
		const tokA, tokB = "ms-e2e-igen-0001", "ms-e2e-igen-0002"
		cn.mu.Lock()
		cn.submitStatus[tokA] = http.StatusTooManyRequests
		cn.submitBody[tokA] = `{"error":{"message":"rate limited"}}`
		cn.mu.Unlock()
		env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{tokA, tokB}})

		resp, raw := env.post("/v1/images/generations", imageReq(model, nil))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d，want 200（换号后应成功）：%s", resp.StatusCode, truncateForLog(raw))
		}
		submits := cn.reqsByPath("/v1/images/generations")
		if len(submits) != 2 {
			t.Fatalf("submit 次数 = %d，want 2（首号 429 → 换号，且不重复打同一个号）", len(submits))
		}
		seen := map[string]bool{}
		for _, s := range submits {
			seen[s.Token] = true
		}
		if !seen[tokA] || !seen[tokB] {
			t.Fatalf("两个账号都该被尝试；实际尝试了 %d 个不同账号", len(seen))
		}
		// 冷却 key = 客户端请求的全串（D7），且是模型级而非整号。
		found := false
		for _, a := range env.accounts(providerGroupModelScope) {
			if mc, ok := a["model_cooling"].(map[string]any); ok {
				if _, ok := mc[model]; ok {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("被限流的号应留下模型级冷却（key = %q），面板要能看到「该模型暂时不可用」", model)
		}
	})

	t.Run("全池限流 → 请求数正好等于池大小（不透支重试）", func(t *testing.T) {
		cn := newMSFake(t, tinyPNG(t))
		toks := []string{"ms-e2e-igen-a", "ms-e2e-igen-b"}
		cn.mu.Lock()
		for _, tk := range toks {
			cn.submitStatus[tk] = http.StatusTooManyRequests
			cn.submitBody[tk] = `{"error":{"message":"rate limited"}}`
		}
		cn.mu.Unlock()
		env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: toks})

		resp, raw := env.post("/v1/images/generations", imageReq(model, nil))
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("status = %d，want 429（全池耗尽透传首因）：%s", resp.StatusCode, truncateForLog(raw))
		}
		if n := len(cn.reqsByPath("/v1/images/generations")); n != len(toks) {
			t.Errorf("submit 次数 = %d，want %d（每号最多一次，没有魔法上限）", n, len(toks))
		}
	})
}

// TestE2E_US13_ImageModelNotRoutable 生图模型名写错时的报错形态：
// 必须 400 invalid_model（而不是 404 —— 404 归 ErrNotFound 会换号并给健康号记错，
// 详见 R139 的自伤事故）。这条与用户故事 10「看到真实原因」同族。
func TestE2E_US13_ImageModelNotRoutable(t *testing.T) {
	cn := newMSFake(t, tinyPNG(t))
	env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-igen-x"}})

	resp, raw := env.post("/v1/images/generations", imageReq("modelscope/No/Such-Image", nil))
	// 模型名能解析到渠道、渠道也能画图 ⇒ 会真的提交给上游；这里只断言「不是 404 路由错」。
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("写错模型名回 404：会被归 ErrNotFound 并惩罚健康账号（R139）")
	}

	// 未知渠道前缀：本地 400 且零出站。
	before := len(cn.reqsByPath("/v1/images/generations"))
	resp, raw = env.post("/v1/images/generations", imageReq("nosuchkind/Qwen/Qwen-Image", nil))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知渠道 status = %d，want 400：%s", resp.StatusCode, truncateForLog(raw))
	}
	errObj, _ := jsonMap(t, raw)["error"].(map[string]any)
	if code := asString(errObj["code"]); code != "invalid_model" {
		t.Errorf("error.code = %q，want invalid_model", code)
	}
	if after := len(cn.reqsByPath("/v1/images/generations")); after != before {
		t.Errorf("未知渠道仍打了上游 %d 次", after-before)
	}
}

// 编译期守卫：本文件断言的"会画图 / 吃多张内联参考图"能力若被删，先在这里红。
var (
	_ provider.ImageGenerator         = (*glmStub)(nil)
	_ provider.ImageGenerator         = (*modelscope.Client)(nil)
	_ provider.InlineImageRefProvider = (*modelscope.Client)(nil)
)
