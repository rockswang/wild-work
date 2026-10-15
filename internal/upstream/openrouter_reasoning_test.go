// openrouter_reasoning_test.go OpenRouter 系上游（Cline 免费池 / OpenCodeZen）
// 思考字段归一化回归测试。
//
// 背景（2026-10-13 实抓）：Cline 免费池的 muse-spark-1.3-contributor 经
// Cline → OpenRouter → Meta 链路，思考增量下发的是 OpenRouter 归一化字段
// `delta.reasoning` 与结构化块 `delta.reasoning_details`，而**不是**本仓库
// 历史唯一形态 `delta.reasoning_content`。旧白名单只认后者 ⇒ 思考文本被
// 静默丢弃（面板/客户端表现为「模型思考了但看不到思考」，usage 里
// reasoning_tokens 却非零）；同一原因使 loopguard 在该渠道完全失效
// （判据只喂 reasoning_content，永远喂不进）。
//
// 实抓原始帧（同一响应，前后两帧）：
//
//	data: {"id":"gen-1791861799-3rQhgaaMZHd7M91A94x1",...,"choices":[{"index":0,
//	      "delta":{"content":"","role":"assistant",
//	               "reasoning":"Comparing decimal values by aligning places to determine 9.",
//	               "reasoning_details":[{"type":"reasoning.summary",
//	                     "summary":"Comparing decimal values by aligning places to determine 9.",
//	                     "format":"meta-responses-v1","index":0}]}}]}
//	data: {...,"delta":{"content":"","role":"assistant","reasoning":null,
//	               "reasoning_details":[{"type":"reasoning.encrypted","data":"Q-PaDgGE…"}]}}
//
// 语义（实测比对，非推断）：`reasoning` 与同帧 `reasoning_details.summary`
// 的 summary 文本逐字相同（拼接后长度 179 == 179 且全等），均为增量 delta；
// `reasoning.encrypted` 是回放用密文、无可读文本。
package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// openRouterReasoningFixture 模拟 OpenRouter 系上游的真实帧序：
// 前两帧是思考（summary 形态 + encrypted 形态），随后正文，末帧 usage + [DONE]。
// 与实抓帧相比只精简了 id/created 等无关字段与超长密文。
const openRouterReasoningFixture = "" +
	`data: {"id":"gen-1","object":"chat.completion.chunk","created":1,"model":"meta/muse-spark-1.3-contributor","choices":[{"index":0,"delta":{"content":"","role":"assistant","reasoning":"Comparing decimal values to determine which is larger.","reasoning_details":[{"type":"reasoning.summary","summary":"Comparing decimal values to determine which is larger.","format":"meta-responses-v1","index":0}]}}]}` + "\n\n" +
	`data: {"id":"gen-1","object":"chat.completion.chunk","created":1,"model":"meta/muse-spark-1.3-contributor","choices":[{"index":0,"delta":{"content":"","role":"assistant","reasoning":null,"reasoning_details":[{"type":"reasoning.encrypted","data":"Q-PaDgGEbihG6WhH9F4A-crxjDMPw48khZF8bVvMfim0epGCEQvfHvgt99ZDM2LVxEShAQZ5IeZ-SArr4CScl5fnu6c_kbnTiieKro8uWekxDzQsjyUWF1Fv9bQX2h5yyFOtsI"}]}}]}` + "\n\n" +
	`data: {"id":"gen-1","object":"chat.completion.chunk","created":1,"model":"meta/muse-spark-1.3-contributor","choices":[{"index":0,"delta":{"content":"9.8 更大。"},"finish_reason":null,"native_finish_reason":null}]}` + "\n\n" +
	`data: {"id":"gen-1","object":"chat.completion.chunk","created":1,"model":"meta/muse-spark-1.3-contributor","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":30,"total_tokens":40}}` + "\n\n" +
	"data: [DONE]\n\n"

// reasoningFramesOf 解出转发帧里的 delta.reasoning_content 增量序列。
func reasoningFramesOf(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data: ") || strings.Contains(ln, "[DONE]") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &obj); err != nil {
			t.Fatalf("bad frame %q: %v", ln, err)
		}
		chs, _ := obj["choices"].([]any)
		for _, ci := range chs {
			c, _ := ci.(map[string]any)
			d, _ := c["delta"].(map[string]any)
			if rc, ok := d["reasoning_content"].(string); ok && rc != "" {
				out = append(out, rc)
			}
		}
	}
	return out
}

// TestStreamNormalizesOpenRouterReasoning 流式：OpenRouter 的 `reasoning`
// 与 `reasoning_details.summary` 都应归一化为 `reasoning_content` 透出，
// 且不重复（两者同内容时只发一次）。
func TestStreamNormalizesOpenRouterReasoning(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(openRouterReasoningFixture)); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	body := rec.Body.String()

	got := reasoningFramesOf(t, body)
	if len(got) != 1 {
		t.Fatalf("期望 1 条思考增量（同帧 reasoning 与 summary 同内容去重），实得 %d: %q", len(got), got)
	}
	if got[0] != "Comparing decimal values to determine which is larger." {
		t.Fatalf("思考文本被改动: %q", got[0])
	}

	// encrypted 密文不得作为可读思考下发
	if strings.Contains(body, "Q-PaDgGEbihG6WhH9F4A") {
		t.Errorf("encrypted 密文不应透出到 reasoning_content:\n%s", body)
	}
	// OpenRouter 原始字段名不得残留（下游统一按 reasoning_content 读）
	if strings.Contains(body, `"reasoning_details"`) {
		t.Errorf("reasoning_details 不应原样透出:\n%s", body)
	}
	// 正文不受影响
	if !strings.Contains(body, "9.8 更大。") {
		t.Errorf("正文丢失:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("缺少 [DONE]:\n%s", body)
	}
}

// TestStreamReasoningOnlyFallbackToSummary 仅 `reasoning_details.summary`
// 形态（无同帧 reasoning 字段）也应归一化透出。
func TestStreamReasoningOnlyFallbackToSummary(t *testing.T) {
	fixture := `data: {"id":"g","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_details":[{"type":"reasoning.summary","summary":"第一步：读题。","format":"meta-responses-v1","index":0}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(fixture)); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := reasoningFramesOf(t, rec.Body.String())
	if len(got) != 1 || got[0] != "第一步：读题。" {
		t.Fatalf("summary 回退失败: %q", got)
	}
}

// TestStreamReasoningTextBeatsSummary `reasoning.text`（原始全文）与
// `reasoning.summary`（摘要）并存时取 text，不重复下发两种粒度。
func TestStreamReasoningTextBeatsSummary(t *testing.T) {
	fixture := `data: {"id":"g","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_details":[{"type":"reasoning.text","text":"原始全文推理","format":"anthropic-claude-v1","index":0},{"type":"reasoning.summary","summary":"摘要","format":"anthropic-claude-v1","index":1}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(fixture)); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := reasoningFramesOf(t, rec.Body.String())
	if len(got) != 1 || got[0] != "原始全文推理" {
		t.Fatalf("text/summary 优先级错误: %q", got)
	}
}

// TestStreamReasoningContentWinsOverOpenRouter 三种字段并存时
// `reasoning_content` 优先（避免同内容重复拼接）。
func TestStreamReasoningContentWinsOverOpenRouter(t *testing.T) {
	fixture := `data: {"id":"g","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"首选","reasoning":"备选","reasoning_details":[{"type":"reasoning.summary","summary":"备选"}]}}]}` + "\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(fixture)); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	got := reasoningFramesOf(t, rec.Body.String())
	if len(got) != 1 || got[0] != "首选" {
		t.Fatalf("优先级错误: %q", got)
	}
}

// TestAggregateNormalizesOpenRouterReasoning 非流式聚合：思考应并入
// message.reasoning_content，密文不得混入。
func TestAggregateNormalizesOpenRouterReasoning(t *testing.T) {
	resp, err := Aggregate(strings.NewReader(openRouterReasoningFixture))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	chs, _ := resp["choices"].([]any)
	if len(chs) == 0 {
		t.Fatal("no choices")
	}
	msg, _ := chs[0].(map[string]any)["message"].(map[string]any)
	rc, _ := msg["reasoning_content"].(string)
	if rc != "Comparing decimal values to determine which is larger." {
		t.Fatalf("聚合思考 = %q", rc)
	}
	if strings.Contains(rc, "Q-PaDgGEbihG6WhH9F4A") {
		t.Fatalf("密文混入聚合思考: %q", rc)
	}
	content, _ := msg["content"].(string)
	if content != "9.8 更大。" {
		t.Fatalf("聚合正文 = %q", content)
	}
}
