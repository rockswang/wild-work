package modelscope

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wild-work/internal/auth"
	"wild-work/internal/provider"
)

func modelIDs(ms []provider.ModelInfo) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

func hasModel(ms []provider.ModelInfo, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

// TestModelsUnionStaticAndRemote 目录 = 静态表 ∪ 上游 `/v1/models`（R42）：
// 远程独有条目进目录、同 ID 去重、静态表刻意不声明能力时由远程上游声明补全，
// 且**上游斜杠原样保留**（对外前缀只由 provider.PublicModelID 拼一次，渠道内不得自己拼）。
func TestModelsUnionStaticAndRemote(t *testing.T) {
	remote := `{"object":"list","data":[` +
		`{"id":"Qwen/Qwen3.5-122B-A10B","context_length":131072,"max_tokens":8192,"supports_tools":true},` +
		`{"id":"deepseek-ai/DeepSeek-V3.1","context_length":65536,"reasoning_efforts":["low","high"],"default_reasoning_effort":"high"},` +
		`{"id":"MiniMax/MiniMax-M3"},` +
		`{"id":"black-forest-labs/FLUX.1-dev"},` +
		`{"id":"Qwen/Qwen2.5-VL-72B-Instruct","input_modalities":["text","image"]}` +
		`]}`
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, remote)
	}))
	defer srv.Close()

	c := New(provider.ModelScope)
	c.Base = srv.URL
	models, err := c.FetchModels(&auth.Auth{AccessToken: "ms-test"})
	if err != nil {
		t.Fatalf("FetchModels 不应因远程成功而报错：%v", err)
	}
	if gotPath != "/v1/models" {
		t.Errorf("目录路径 = %q，期望 /v1/models", gotPath)
	}
	if gotAuth != "Bearer ms-test" {
		t.Errorf("目录鉴权 = %q，期望 Bearer ms-test", gotAuth)
	}

	// ① 并集：静态独有（生图模型）+ 远程独有都在。
	for _, want := range []string{
		"Qwen/Qwen3.5-122B-A10B",       // 静态 ∩ 远程
		"Qwen/Qwen-Image-Edit",         // 静态独有（生图模型）
		"MiniMax/MiniMax-M3",           // 远程独有（对话）
		"deepseek-ai/DeepSeek-V3.1",    // 远程独有
		"black-forest-labs/FLUX.1-dev", // 远程独有（生图）
		"Qwen/Qwen2.5-VL-72B-Instruct", // 远程独有（视觉理解）
	} {
		if !hasModel(models, want) {
			t.Errorf("目录缺模型 %q，实际 = %v", want, modelIDs(models))
		}
	}
	// ② 去重：同 ID 只出现一次（静态 ∩ 远程的重叠条目）。
	seen := map[string]bool{}
	for _, id := range modelIDs(models) {
		if seen[id] {
			t.Errorf("目录出现重复条目 %q", id)
		}
		seen[id] = true
	}
	// ③ 静态表不声明能力 ⇒ 远程声明必须被采纳（否则客户端看不到上下文与 tools）。
	var staticEntryInfo *provider.ModelInfo
	for i := range models {
		if models[i].ID == "Qwen/Qwen3.5-122B-A10B" {
			staticEntryInfo = &models[i]
		}
	}
	if staticEntryInfo == nil {
		t.Fatal("静态条目在并集中丢失")
	}
	if staticEntryInfo.ContextWindow != 131072 || !staticEntryInfo.ContextFromAPI {
		t.Errorf("远程声明的上下文未补进静态条目：%+v", *staticEntryInfo)
	}
	if !staticEntryInfo.SupportsTools {
		t.Error("远程声明的 tools 能力未补进静态条目")
	}
	// ④ 远程独有条目的能力同样只按上游声明。
	for _, m := range models {
		if m.ID != "Qwen/Qwen2.5-VL-72B-Instruct" {
			continue
		}
		if !m.SupportsImages {
			t.Error("input_modalities 含 image 的条目应声明 SupportsImages（能看图）")
		}
		if IsImageModel(m.ID) {
			t.Error("视觉理解模型被判成生图模型（能看图 ≠ 能出图）")
		}
	}
	// ⑤ 双斜杠护栏：上游 ID 保留斜杠，对外 ID 只由 PublicModelID 拼一次前缀。
	for _, m := range models {
		if strings.HasPrefix(m.ID, "modelscope/") {
			t.Errorf("渠道内不得自己拼前缀：%q", m.ID)
		}
	}
	if got, want := provider.PublicModelID(provider.ModelScope, "Qwen/Qwen-Image"), "modelscope/Qwen/Qwen-Image"; got != want {
		t.Errorf("PublicModelID = %q，期望 %q", got, want)
	}
	if k, up, ok := provider.SplitModelID("modelscope/Qwen/Qwen-Image"); !ok || k != provider.ModelScope || up != "Qwen/Qwen-Image" {
		t.Errorf("SplitModelID = (%q,%q,%v)，期望 (modelscope, Qwen/Qwen-Image, true)", k, up, ok)
	}
}

// TestModelsRemoteFailureFallsBackToStatic 远程目录失败必须**降级不报错**：
// 回静态表、error 恒 nil（否则 server 层会把渠道打进 5min 负缓存并从面板抹掉，
// R133：一个坏 token 不该让整渠道目录归零），且绝不给「上游没有」与「这次失败」
// 留下同一个空洞的 `0 个模型 + nil error`。
func TestModelsRemoteFailureFallsBackToStatic(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"500", http.StatusInternalServerError, `{"error":{"message":"boom"}}`},
		{"404 端点不存在", http.StatusNotFound, `{"error":{"message":"not found"}}`},
		{"401 token 失效", http.StatusUnauthorized, `{"error":{"message":"invalid token"}}`},
		{"非 JSON（网关兜底页）", http.StatusOK, `<html><body>captive portal</body></html>`},
	}
	static := modelIDs(StaticModels())
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits++
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := New(provider.ModelScope)
			c.Base = srv.URL
			models, err := c.FetchModels(&auth.Auth{AccessToken: "ms-test"})
			if err != nil {
				t.Fatalf("远程失败不得上抛（会写负缓存）：%v", err)
			}
			if hits != 1 {
				t.Errorf("远程只应尝试一次（无自动重试），实际 %d 次", hits)
			}
			if got := modelIDs(models); strings.Join(got, ",") != strings.Join(static, ",") {
				t.Errorf("降级目录 = %v，期望静态表 %v", got, static)
			}
			if len(models) == 0 {
				t.Fatal("降级后目录为空（R133 事故形态）")
			}
		})
	}
}

// TestImageFlagHeuristicAndFalsePositiveGuard 生图判定（D12）：
//   - 静态 `image:true` 标记生效；
//   - 命名启发式能抓住上游目录里的生图模型（FLUX / diffusion 系）；
//   - **视觉理解模型不得被标成生图模型**（`Qwen2.5-VL` / `VL` 系是「能看图」，不是「能出图」），
//     这是本条最重要的一条护栏：误判会让视觉理解模型在对话路径被本地拒掉。
func TestImageFlagHeuristicAndFalsePositiveGuard(t *testing.T) {
	imageModels := []string{
		"Qwen/Qwen-Image",
		"Qwen/Qwen-Image-Edit",
		"black-forest-labs/FLUX.1-dev",
		"black-forest-labs/FLUX.1-schnell",
		"AI-ModelScope/stable-diffusion-xl",
		"Kwai-Kolors/Kolors",
	}
	for _, id := range imageModels {
		if !IsImageModel(id) {
			t.Errorf("%q 应被判为生图模型", id)
		}
	}
	chatModels := []string{
		"Qwen/Qwen2.5-VL-72B-Instruct", // 能看图，不能出图
		"Qwen/Qwen3-VL-30B-A3B-Instruct",
		"Qwen/Qwen3-235B-A22B-Instruct-2507",
		"deepseek-ai/DeepSeek-V3.1",
		"Qwen/Qwen2.5-Coder-32B-Instruct",
		"", // 空串不得被判成生图（否则空 model 的请求会被本地拒）
	}
	for _, id := range chatModels {
		if IsImageModel(id) {
			t.Errorf("%q 被误判为生图模型（能看图 ≠ 能出图；误判会让对话路径本地拒掉它）", id)
		}
	}
	// 整 token 匹配：子串偶合不得触发（imagegen-xxx 不是 image）。
	if IsImageModel("vendor/imagegen-pro") {
		t.Error("imagegen 是子串偶合，不得按生图模型处理")
	}
	// 静态表里 image:true 的每一条都必须被判定为生图模型（否则对话路径会放行它们）。
	for _, e := range staticCatalog {
		if e.Image && !IsImageModel(e.ID) {
			t.Errorf("静态生图模型 %q 未被判定为生图模型", e.ID)
		}
	}
}

// TestModelsDoNotInventCapabilities R49/R119：上游没写的字段一律不声明。
// 这条是「探测不到就一律不声明」的反向护栏——若解析器开始按名字猜能力，它立刻变红。
func TestModelsDoNotInventCapabilities(t *testing.T) {
	raw := `{"data":[{"id":"Qwen/Qwen-Image"},{"id":"vendor/mystery-model"}]}`
	models, err := parseModelList([]byte(raw))
	if err != nil {
		t.Fatalf("parseModelList: %v", err)
	}
	for _, m := range models {
		if m.SupportsTools || m.SupportsReasoning || m.SupportsImages || m.ContextWindow != 0 || m.MaxTokens != 0 {
			t.Errorf("%q 声明了上游未提供的能力（R49 禁止编造）：%+v", m.ID, m)
		}
		if len(m.ReasoningEfforts) != 0 || m.DefaultReasoningEffort != "" {
			t.Errorf("%q 编造了思考档位：%+v", m.ID, m)
		}
	}
}

// TestMergeModelsRemoteDuplicateIDs 上游目录出现重复 ID 时必须去重且**不得越界**：
// 早期实现用同一个 map 既记「静态位置」又给远程独有条目写 -1 哨兵，第二条重名命中
// 哨兵会 out[-1] panic（目录一变就把进程刷崩）。
func TestMergeModelsRemoteDuplicateIDs(t *testing.T) {
	static := []provider.ModelInfo{{ID: "a/static"}}
	remote := []provider.ModelInfo{
		{ID: "b/only-remote"},
		{ID: "b/only-remote", ContextWindow: 4096, ContextFromAPI: true},
		{ID: "a/static", SupportsTools: true},
		{ID: "a/static", SupportsImages: true},
	}
	got := mergeModels(static, remote)
	ids := modelIDs(got)
	if strings.Join(ids, ",") != "a/static,b/only-remote" {
		t.Fatalf("合并结果 = %v，期望 [a/static b/only-remote]", ids)
	}
	if !got[0].SupportsTools || !got[0].SupportsImages {
		t.Errorf("重名远程条目应逐条补全静态条目，实际 %+v", got[0])
	}
	if got[1].ContextWindow != 4096 || !got[1].ContextFromAPI {
		t.Errorf("重名远程条目应只入目录一次并带上游声明，实际 %+v", got[1])
	}
}
