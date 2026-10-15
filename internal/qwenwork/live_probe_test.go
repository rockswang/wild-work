//go:build live

// live_probe_test.go 千问办公「思考链可见性」探针。
//
// 背景：Qoder 渠道曾出现「上游不下发思考链」的误判，根因是请求体形状没对齐
// （见 AGENTS.md R21）。千问办公是**透传**渠道 —— 只补 request_id/session_id +
// model key 映射，不做任何方言投影（ProjectReasoning 只服务 WorkBuddy 两版）——
// 所以形状问题的表现形式不同：思考链给不给，取决于上游默认行为以及客户端带了什么字段。
//
// 本探针一次性回答三个问题：
//  1. 上游在「最小 body」下是否默认下发 reasoning_content？（用例 bare）
//  2. 哪些客户端字段能打开/增强思考链？（reasoning_effort / enable_thinking / thinking_budget）
//  3. 若聚合后为空，上游原始 SSE 里究竟出现了哪些 delta 字段名？
//     sse.go 只认 delta.reasoning_content —— 字段名不同就会被静默丢掉，
//     这种情况「本地解析丢」与「上游不给」表现一样，必须看原始流才能区分。
//
// 用法：
//
//	WILDWORK_AUTHDIR=<账号目录> \
//	go test -tags live ./internal/qwenwork/ -run TestLiveProbeReasoning -v
//
// 可选环境变量：
//
//	WILDWORK_PROBE_MODEL   客户端模型名（默认 pro；可用 flash / qwen3.8-max-preview）
//	WILDWORK_PROBE_PROMPT  探测用 prompt
//	WILDWORK_PROBE_DUMP    原始 SSE 落盘目录（生成 probe-<case>.sse）
//	WILDWORK_PROBE_CASES   只跑指定用例（逗号分隔，如 "bare,enable-thinking"）
//
// 判读（跑完会打印结论表）：
//   - bare 用例思考 > 0          → 上游默认就给，客户端什么都不用做，千问「看不到思考」不成立
//   - bare = 0 但某用例 > 0      → 该用例的字段是开关；需在渠道层按需注入（参考 Qoder 的投影）
//   - 全为 0 且 deltaKeys 出现
//     reasoning/thinking 等     → 上游给了但你认错字段名 → 改 sse.go 的取值键
//   - 全为 0 且 deltaKeys 只有
//     content                   → 上游确实不给思考链（或需要更强的形状对齐，需抓官方客户端）
package qwenwork

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
)

// probeCase 单个探测用例：id + 说明 + 额外注入 body 的字段。
type probeCase struct {
	id    string
	desc  string
	extra map[string]any
}

// probeCases 用例表。顺序即执行顺序，bare 永远第一（基准）。
func probeCases() []probeCase {
	return []probeCase{
		{"bare", "最小 body（只有 model/messages/stream）", nil},
		{"effort-high", "+ reasoning_effort=high（server 归一化后的标准键）", map[string]any{
			"reasoning_effort": "high",
		}},
		{"enable-thinking", "+ enable_thinking=true（Qwen 系常见开关）", map[string]any{
			"enable_thinking": true,
		}},
		{"thinking-budget", "+ enable_thinking=true + thinking_budget=8192", map[string]any{
			"enable_thinking": true,
			"thinking_budget": 8192,
		}},
		{"both", "+ enable_thinking=true + reasoning_effort=xhigh", map[string]any{
			"enable_thinking":  true,
			"reasoning_effort": "xhigh",
		}},
	}
}

// liveProbeAuth 从 WILDWORK_AUTHDIR（默认 ./auths）取第一个千问办公账号。
func liveProbeAuth(t *testing.T) *auth.Auth {
	t.Helper()
	dir := os.Getenv("WILDWORK_AUTHDIR")
	if dir == "" {
		dir = "auths"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("解析目录失败：%v", err)
	}
	as, err := auth.LoadQwenWorkDir(abs)
	if err != nil {
		t.Fatalf("加载 %s 下的 qwenwork*.json 失败：%v", abs, err)
	}
	if len(as) == 0 {
		t.Skipf("在 %s 没找到 qwenwork*.json —— 先在面板登录一个千问办公账号，"+
			"或用 WILDWORK_AUTHDIR 指向账号目录", abs)
	}
	a := as[0]
	t.Logf("账号：uid=%s nickname=%s file=%s", a.UID, a.Nickname, a.FilePath)
	return a
}

// probeModel 客户端模型名（默认 pro）。
func probeModel() string {
	if v := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_MODEL")); v != "" {
		return v
	}
	return "pro"
}

// probePromptText 探测 prompt。
func probePromptText() string {
	if v := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_PROMPT")); v != "" {
		return v
	}
	// 需要一段真实的推理过程才看得出思考链；太短的问题可能直接答。
	return "一个笼子里有鸡和兔共 35 个头、94 只脚，问鸡兔各几只？请逐步推理后给出答案。"
}

// probeSelection 解析 WILDWORK_PROBE_CASES。
func probeSelection() map[string]bool {
	sel := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_CASES"))
	if sel == "" {
		return nil
	}
	out := map[string]bool{}
	for _, s := range strings.Split(sel, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}
	return out
}

// probeResult 单用例结果。
type probeResult struct {
	id        string
	status    int
	err       error
	rawBytes  int
	chunks    int
	deltaKeys map[string]int // delta 里出现过且非空的字段名 → 次数
	reasoning string         // 聚合后 reasoning_content
	content   string         // 聚合后 content
	usage     map[string]any
	elapsed   time.Duration
	dumpPath  string
	firstLine string
}

// scanRaw 遍历原始 SSE，统计 delta 字段名分布；同时取出首行样本。
func scanRaw(raw []byte) (chunks int, deltaKeys map[string]int, firstLine string) {
	deltaKeys = map[string]int{}
	_ = parseNestedSSE(bytes.NewReader(raw), func(chunk map[string]any) error {
		chunks++
		if firstLine == "" {
			if b, err := json.Marshal(chunk); err == nil {
				firstLine = truncate(string(b), 300)
			}
		}
		chs, _ := chunk["choices"].([]any)
		for _, ci := range chs {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			// 流式：delta；非流式兜底：message
			for _, key := range []string{"delta", "message"} {
				d, _ := c[key].(map[string]any)
				if d == nil {
					continue
				}
				for k, v := range d {
					if v == nil {
						continue
					}
					if s, ok := v.(string); ok && s == "" {
						continue
					}
					deltaKeys[k]++
				}
			}
		}
		return nil
	}, nil)
	return
}

// TestLiveProbeReasoning 千问办公思考链可见性探针（见文件头注释）。
func TestLiveProbeReasoning(t *testing.T) {
	a := liveProbeAuth(t)
	c := New()
	clientModel := probeModel()
	dumpDir := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_DUMP"))
	if dumpDir != "" {
		if err := os.MkdirAll(dumpDir, 0o755); err != nil {
			t.Fatalf("创建 dump 目录失败：%v", err)
		}
	}
	sel := probeSelection()

	// 前置诊断：先拉模型目录。这一步同时验证账号可用性与 COSY 签名是否有效 ——
	// 若这里就失败，问题在账号/鉴权，与「思考链形状」无关；
	// 上游对推理请求回的 503 "Model catalog unavailable" 即属于此类。
	if infos, err := c.FetchModels(a); err != nil {
		t.Logf("⚠ FetchModels 失败（先排查账号/鉴权，与思考链无关）：%v", err)
	} else {
		ids := make([]string, 0, len(infos))
		for _, m := range infos {
			ids = append(ids, m.ID)
		}
		t.Logf("模型目录拉取成功（%d 个）：%v", len(ids), ids)
	}

	t.Logf("模型=%s  上游 key=%s  dump=%q", clientModel, ModelKey(clientModel), dumpDir)
	t.Log("说明：本渠道透传客户端 body，仅补 request_id/session_id + model key + business.product。")
	t.Log("")

	var results []*probeResult
	for _, pc := range probeCases() {
		if sel != nil && !sel[pc.id] {
			continue
		}
		r := &probeResult{id: pc.id}
		body := map[string]any{
			"model":    clientModel,
			"messages": []map[string]any{{"role": "user", "content": probePromptText()}},
		}
		for k, v := range pc.extra {
			body[k] = v
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}

		start := time.Now()
		rc, status, respBody, err := c.ChatStream(a, raw)
		r.elapsed = time.Since(start)
		r.status = status

		switch {
		case err != nil:
			r.err = err
		case rc == nil:
			r.err = fmt.Errorf("被上游拒绝：status=%d body=%s", status, truncate(string(respBody), 300))
		default:
			buf, readErr := io.ReadAll(rc)
			rc.Close()
			if readErr != nil {
				r.err = fmt.Errorf("读流失败：%w", readErr)
			} else {
				r.rawBytes = len(buf)
				if dumpDir != "" {
					p := filepath.Join(dumpDir, "probe-"+pc.id+".sse")
					if werr := os.WriteFile(p, buf, 0o644); werr == nil {
						r.dumpPath = p
					}
				}
				r.chunks, r.deltaKeys, r.firstLine = scanRaw(buf)
				if msg, aerr := aggregate(bytes.NewReader(buf), clientModel); aerr == nil {
					r.reasoning = msgField(msg, "reasoning_content")
					r.content = msgField(msg, "content")
					if u, ok := msg["usage"].(map[string]any); ok {
						r.usage = u
					}
				} else {
					r.err = fmt.Errorf("aggregate 失败：%w", aerr)
				}
			}
		}
		results = append(results, r)
		t.Logf("── 用例 %-16s %s", pc.id, pc.desc)
		reportCase(t, r)
	}

	// ── 结论 ────────────────────────────────────────────────────────────
	t.Log("")
	t.Log("================ 结论 ================")
	if len(results) == 0 {
		t.Fatalf("没有执行任何用例（WILDWORK_PROBE_CASES=%q 没匹配上）", os.Getenv("WILDWORK_PROBE_CASES"))
	}

	var bare, anyOK *probeResult
	var allKeys = map[string]int{}
	for _, r := range results {
		if r.reasoning != "" && anyOK == nil {
			anyOK = r
		}
		if r.id == "bare" {
			bare = r
		}
		for k, n := range r.deltaKeys {
			allKeys[k] += n
		}
	}

	// (1) 上游默认行为
	if bare != nil {
		if bare.err == nil && len([]rune(bare.reasoning)) > 0 {
			t.Logf("✓ bare 用例思考 %d 字 —— 上游默认就下发思考链，客户端无需任何额外字段。",
				len([]rune(bare.reasoning)))
			t.Log("  注意：这依赖 prepareChatBody 已注入 business.product —— 该字段缺省时")
			t.Log("        上游恒回 503「Model catalog unavailable」，与思考链无关，是请求形状问题。")
		} else if bare.err == nil {
			t.Log("✗ bare 用例思考 0 字 —— 上游不默认下发，需要客户端/渠道注入开关字段。")
		} else {
			t.Logf("! bare 用例异常：%v", bare.err)
		}
	}

	// (2) 哪个字段是开关
	if bare != nil && bare.err == nil && len([]rune(bare.reasoning)) == 0 {
		if anyOK != nil {
			t.Logf("→ 开关字段：用例 %q 能拿到 %d 字思考。",
				anyOK.id, len([]rune(anyOK.reasoning)))
			t.Log("  建议：在 qwenwork.prepareChatBody 里按该字段注入（参考 Qoder 的档位投影），")
			t.Log("        并让 reasoning.SupportsEffortKind 收录 qwenwork，档位才走统一漏斗。")
		} else {
			t.Log("→ 所有用例都是 0 字：" +
				"要么上游真的要更强的形状对齐（抓官方千问办公客户端比对），" +
				"要么字段名认错了（看下面的 delta 字段名分布）。")
		}
	}

	// (3) 字段名分布 —— 区分「上游不给」与「本地认错键」
	keys := make([]string, 0, len(allKeys))
	for k := range allKeys {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return allKeys[keys[i]] > allKeys[keys[j]] })
	t.Logf("全部用例的 delta 字段名分布：%v", keys)

	suspicious := []string{"reasoning", "reasoning_content", "thinking", "reasoning_details",
		"analysis", "thought", "reasoning_text"}
	for _, k := range suspicious {
		if allKeys[k] > 0 {
			mark := "（sse.go 已识别）"
			if k != "reasoning_content" {
				mark = "（sse.go 未识别 → 会被静默丢弃，需补取值键）"
			}
			t.Logf("  思考候选字段 %-18s 出现 %d 次 %s", k, allKeys[k], mark)
		}
	}

	// (4) reasoning_tokens —— 上游自报的思考量，可交叉验证
	for _, r := range results {
		if r.usage == nil {
			continue
		}
		if rt, ok := r.usage["reasoning_tokens"]; ok {
			t.Logf("  用例 %-16s usage.reasoning_tokens=%v", r.id, rt)
		}
	}
}

// reportCase 打印单用例诊断。
func reportCase(t *testing.T, r *probeResult) {
	t.Helper()
	if r.err != nil {
		t.Logf("   ✗ %v", r.err)
		if r.firstLine != "" {
			t.Logf("   首行样本：%s", r.firstLine)
		}
		return
	}
	t.Logf("   status=%d 耗时=%s 原始=%d bytes chunk=%d",
		r.status, r.elapsed.Round(time.Millisecond), r.rawBytes, r.chunks)
	t.Logf("   思考=%d 字  回答=%d 字  识别到的 delta 字段=%v",
		len([]rune(r.reasoning)), len([]rune(r.content)), sortedKeys(r.deltaKeys))
	if r.dumpPath != "" {
		t.Logf("   原始 SSE 已落盘：%s", r.dumpPath)
	}
	t.Logf("   首行样本：%s", r.firstLine)
}

// sortedKeys 排序后的 map 键（打印稳定）。
func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// msgField 从聚合后的 message 取字符串字段。
func msgField(msg map[string]any, key string) string {
	chs, _ := msg["choices"].([]any)
	for _, ci := range chs {
		c, _ := ci.(map[string]any)
		if c == nil {
			continue
		}
		m, _ := c["message"].(map[string]any)
		if m == nil {
			continue
		}
		if s, ok := m[key].(string); ok {
			return s
		}
	}
	return ""
}

// TestLiveProbeRawRequest 裸请求诊断：定位 503 "Model catalog unavailable"。
//
// 打印三样东西，用于区分「账号无权限/无额度」与「请求形状不对」：
//  1. 账号侧信息（昵称 / 资源额度）—— 若为 0 或报错，问题在账号；
//  2. 实际发出的请求头（脱敏）+ 请求体 —— 与备忘 §2.4 的「极简头」逐项比对；
//  3. 完整响应头 + 响应体 —— `x-model-name` 等路由头能证明请求是否被正确路由。
//
// 用法：WILDWORK_AUTHDIR=<账号目录> go test -tags live ./internal/qwenwork/ \
//
//	-run TestLiveProbeRawRequest -v
func TestLiveProbeRawRequest(t *testing.T) {
	a := liveProbeAuth(t)
	c := New()

	// ── 1. 账号侧 ──────────────────────────────────────────────
	if n, err := c.FetchNickname(a); err == nil {
		t.Logf("账号昵称=%s", n)
	} else {
		t.Logf("⚠ FetchNickname 失败：%v", err)
	}
	if v, err := c.UserResource(a); err == nil {
		t.Logf("账号资源（积分/额度）=%d", v)
	} else {
		t.Logf("⚠ UserResource 失败：%v", err)
	}
	if v, items, err := c.UserResourceDetail(a); err == nil {
		t.Logf("资源明细 total=%d", v)
		for _, it := range items {
			t.Logf("   · %+v", it)
		}
	} else {
		t.Logf("⚠ UserResourceDetail 失败：%v", err)
	}

	// ── 2. 构造与生产路径同形的推理请求 ────────────────────────
	clientModel := probeModel()
	uid, name, email, token := identity(a)
	sess, err := NewCosySession(uid, name, email, token)
	if err != nil {
		t.Fatalf("cosy session: %v", err)
	}
	body, _ := json.Marshal(map[string]any{
		"model":      clientModel,
		"messages":   []map[string]any{{"role": "user", "content": probePromptText()}},
		"stream":     true,
		"request_id": uuid4(),
		"session_id": uuid4(),
	})
	rawURL := c.gateway() + EpChat
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	headers := map[string]string{}
	if err := sess.ApplyHeadersWithUID(headers, string(body), rawURL, uid); err != nil {
		t.Fatalf("cosy headers: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("x-model-key", ModelKey(clientModel))

	t.Logf("URL=%s", rawURL)
	t.Logf("--- 请求头（脱敏）---")
	keys := make([]string, 0, len(req.Header))
	for k := range req.Header {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("  %-16s %s", k, maskHeader(k, req.Header.Get(k)))
	}
	t.Logf("--- 请求体（%d 字节）---\n%s", len(body), truncate(string(body), 500))

	// ── 3. 响应全貌 ────────────────────────────────────────────
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	t.Logf("--- 响应 status=%d ---", resp.StatusCode)
	rkeys := make([]string, 0, len(resp.Header))
	for k := range resp.Header {
		rkeys = append(rkeys, k)
	}
	sort.Strings(rkeys)
	for _, k := range rkeys {
		t.Logf("  %-24s %s", k, maskHeader(k, resp.Header.Get(k)))
	}
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
	t.Logf("--- 响应体（%d 字节）---\n%s", len(buf), truncate(string(buf), 1200))
}

// maskHeader 脱敏：授权类头只留前缀，避免进日志。
func maskHeader(k, v string) string {
	switch strings.ToLower(k) {
	case "authorization", "cosy-key", "cosy-user", "cookie", "set-cookie":
		if len(v) > 20 {
			return v[:20] + fmt.Sprintf("…[截断，共 %d 字节]", len(v))
		}
	}
	return v
}

// buddyStyleBody 复刻参考实现 Buddy2api（providers/qwenwork/chat.py）的重构造 body。
//
// 与 wild-work 当前「纯透传」的差别就在这些字段：**`model_config.source = "system"`
// 与 `x-model-source: system` 是「用系统模型目录」的声明** —— 缺了它上游回
// `503 Model catalog unavailable`。`chat_context`/`chat_task`/`agent_id`/`source`/`version`
// 等则是官方桌面版 0.1.8 请求的固定骨架。
func buddyStyleBody(model, prompt, reqID, sessID string) map[string]any {
	return map[string]any{
		"request_id":     reqID,
		"request_set_id": reqID,
		"chat_record_id": reqID,
		"session_id":     sessID,
		"stream":         true,
		"chat_task":      "FREE_INPUT",
		"chat_context": map[string]any{
			"text":     prompt,
			"features": []any{},
			"extra": map[string]any{
				"context":         []any{},
				"modelConfig":     map[string]any{"key": model, "is_reasoning": true},
				"originalContent": prompt,
			},
			"chatPrompt": "",
			"imageUrls":  nil,
		},
		"is_reply":         true,
		"is_retry":         false,
		"source":           1,
		"version":          "3",
		"agent_id":         "agent_common",
		"task_id":          "common",
		"session_type":     "qoder_work",
		"aliyun_user_type": "",
		"model_config": map[string]any{
			"key":              model,
			"display_name":     model,
			"model":            "",
			"format":           "openai",
			"is_vl":            model == "pro" || model == "qwork-advanced",
			"is_reasoning":     true,
			"api_key":          "",
			"url":              "",
			"source":           "system",
			"max_input_tokens": 180000,
		},
		"system":     "",
		"messages":   []map[string]any{{"role": "user", "content": prompt}},
		"tools":      []any{},
		"parameters": map[string]any{"max_tokens": 32000},
	}
}

// buddyStyleHeaders 复刻 Buddy2api static_headers 的完整头集。
// wild-work 当前只发「极简头」（COSY 四件套 + x-model-key），缺 `x-model-source` 与
// 一整组 `X-QwenWork-*` / `Cosy-Business-*` —— 这正是要验证的变量。
func buddyStyleHeaders(reqID, model string) map[string]string {
	return map[string]string{
		"Accept":                     "text/event-stream",
		"Content-Type":               "application/json",
		"User-Agent":                 "qoderwork/0.1.8",
		"X-Request-Id":               reqID,
		"X-QwenWork-Version":         "0.1.8",
		"X-QwenWork-Release-Version": "0.1.8-26081406",
		"X-QwenWork-Build":           "26081406",
		"X-QwenWork-Platform":        "win32",
		"X-QwenWork-Arch":            "x64",
		"X-QwenWork-Channel":         "stable",
		"Cosy-Version":               "1.1.18",
		"Cosy-ClientType":            "6",
		"Cosy-Business-Product":      "qoder_work",
		"Cosy-Business-Type":         "agent",
		"Cosy-Scene":                 "qwork",
		"Cosy-MachineOS":             "x86_64_win32",
		"Login-Version":              "v2",
		"x-model-key":                model,
		"x-model-source":             "system",
		"Cache-Control":              "no-cache",
		"Connection":                 "keep-alive",
		"Accept-Encoding":            "identity",
	}
}

// TestLiveProbeBuddyBody 用参考实现的「完整头 + 重构造 body」打一次，验证 503 的归因。
//
// 目的：确认 `503 Model catalog unavailable` 是缺 `model_config` / `x-model-source`
// 造成的，而不是账号或上游故障。三种组合逐层加码：
//
//	A 极简头 + 透传 body（当前实现，作对照，预期 503）
//	B 完整头 + 透传 body（只换头，验证头是不是唯一变量）
//	C 完整头 + 重构造 body（参考实现形状，预期 200）
//
// 用法：WILDWORK_AUTHDIR=<账号目录> go test -tags live ./internal/qwenwork/ \
//
//	-run TestLiveProbeBuddyBody -v
func TestLiveProbeBuddyBody(t *testing.T) {
	a := liveProbeAuth(t)
	c := New()
	clientModel := ModelKey(probeModel())
	uid, name, email, token := identity(a)
	prompt := probePromptText()

	type variant struct {
		id      string
		desc    string
		headers func(reqID string) map[string]string
		body    func(reqID, sessID string) any
	}
	variants := []variant{
		{
			id:   "A-baseline",
			desc: "极简头 + 透传 body（当前实现）",
			headers: func(reqID string) map[string]string {
				return map[string]string{"x-model-key": clientModel}
			},
			body: func(reqID, sessID string) any {
				return map[string]any{
					"model": clientModel, "stream": true,
					"request_id": reqID, "session_id": sessID,
					"messages": []map[string]any{{"role": "user", "content": prompt}},
				}
			},
		},
		{
			id:   "B-full-headers",
			desc: "完整头 + 透传 body（验证头是否为唯一变量）",
			headers: func(reqID string) map[string]string {
				return buddyStyleHeaders(reqID, clientModel)
			},
			body: func(reqID, sessID string) any {
				return map[string]any{
					"model": clientModel, "stream": true,
					"request_id": reqID, "session_id": sessID,
					"messages": []map[string]any{{"role": "user", "content": prompt}},
				}
			},
		},
		{
			id:   "C-full-both",
			desc: "完整头 + 重构造 body（参考实现形状）",
			headers: func(reqID string) map[string]string {
				return buddyStyleHeaders(reqID, clientModel)
			},
			body: func(reqID, sessID string) any {
				return buddyStyleBody(clientModel, prompt, reqID, sessID)
			},
		},
	}

	for _, v := range variants {
		reqID := uuid4()
		sessID := uuid4()
		raw, err := json.Marshal(v.body(reqID, sessID))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		sess, err := NewCosySession(uid, name, email, token)
		if err != nil {
			t.Fatalf("cosy: %v", err)
		}
		rawURL := c.gateway() + EpChat
		req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		cosy := map[string]string{}
		if err := sess.ApplyHeadersWithUID(cosy, string(raw), rawURL, uid); err != nil {
			t.Fatalf("cosy headers: %v", err)
		}
		for k, val := range cosy {
			req.Header.Set(k, val)
		}
		for k, val := range v.headers(reqID) {
			req.Header.Set(k, val)
		}
		// 签名串里含 body 与 path，头改动不影响签名；但 x-model-key 由 header 决定路由。

		t.Logf("── 变体 %s：%s（body %d 字节）", v.id, v.desc, len(raw))
		resp, err := c.HTTP.Do(req)
		if err != nil {
			t.Logf("   ✗ 传输失败：%v", err)
			continue
		}
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		msg, aerr := aggregate(bytes.NewReader(buf), clientModel)
		if aerr != nil {
			t.Logf("   ✗ HTTP %d，流内错误：%v", resp.StatusCode, aerr)
			t.Logf("   原始前 400 字节：%s", truncate(string(buf), 400))
			continue
		}
		reasoning := msgField(msg, "reasoning_content")
		content := msgField(msg, "content")
		t.Logf("   ✓ HTTP %d，思考=%d 字，回答=%d 字，流 %d 字节",
			resp.StatusCode, len([]rune(reasoning)), len([]rune(content)), len(buf))
		if reasoning != "" {
			t.Logf("   思考片段：%s", truncate(reasoning, 200))
		}
	}
}

// TestLiveProbeRefreshThenChat 先刷新 token 再请求 —— 验证 503 的最终归因。
//
// 线索：auth 文件里的 `accessToken` 是登录拿到的**网页 JWT**（`eyJhbG…`，iss=qwenwork.cn，
// aud=oauth_app），而 `RefreshToken` 会把 `accessToken` 换成 `device_token`
// （client.go：`dt := out.DeviceToken; if dt == "" { dt = out.Token }`）。
// 探针此前直接用网页 JWT 打推理 → `503 Model catalog unavailable`；
// 生产路径（app 层加载账号后会刷新）不会踩到 —— 这正是「只有探针失败」的解释。
//
// 用法：WILDWORK_AUTHDIR=<账号目录> go test -tags live ./internal/qwenwork/ \
//
//	-run TestLiveProbeRefreshThenChat -v
//
// 注意：本用例会**写回 auth 文件**（refresh token 轮换，不落盘则下次失效）。
func TestLiveProbeRefreshThenChat(t *testing.T) {
	a := liveProbeAuth(t)
	c := New()

	before := a.AccessTokenValue()
	t.Logf("刷新前 accessToken：长度=%d 前缀=%.10s", len(before), before)

	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("RefreshToken 失败：%v", err)
	}
	after := a.AccessTokenValue()
	t.Logf("刷新后 accessToken：长度=%d 前缀=%.10s", len(after), after)
	t.Logf("刷新后 refreshToken：长度=%d 前缀=%.8s", len(a.RefreshTokenValue()), a.RefreshTokenValue())
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("SaveAtomic：%v", err)
	}
	t.Log("已写回 auth 文件")

	// 用参考实现形状请求
	clientModel := ModelKey(probeModel())
	uid, name, email, token := identity(a)
	prompt := probePromptText()
	reqID, sessID := uuid4(), uuid4()
	raw, err := json.Marshal(buddyStyleBody(clientModel, prompt, reqID, sessID))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sess, err := NewCosySession(uid, name, email, token)
	if err != nil {
		t.Fatalf("cosy: %v", err)
	}
	rawURL := c.gateway() + EpChat
	req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	cosy := map[string]string{}
	if err := sess.ApplyHeadersWithUID(cosy, string(raw), rawURL, uid); err != nil {
		t.Fatalf("cosy headers: %v", err)
	}
	for k, v := range cosy {
		req.Header.Set(k, v)
	}
	for k, v := range buddyStyleHeaders(reqID, clientModel) {
		req.Header.Set(k, v)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	msg, aerr := aggregate(bytes.NewReader(buf), clientModel)
	if aerr != nil {
		t.Errorf("刷新后仍失败：HTTP %d，流内错误 %v\n原始：%s",
			resp.StatusCode, aerr, truncate(string(buf), 400))
		return
	}
	reasoning := msgField(msg, "reasoning_content")
	content := msgField(msg, "content")
	t.Logf("✓ HTTP %d，思考=%d 字，回答=%d 字，流 %d 字节",
		resp.StatusCode, len([]rune(reasoning)), len([]rune(content)), len(buf))
	if reasoning != "" {
		t.Logf("思考片段：%s", truncate(reasoning, 300))
	}
	if u, ok := msg["usage"].(map[string]any); ok {
		if b, err := json.Marshal(u); err == nil {
			t.Logf("usage=%s", b)
		}
	}
}

// TestLiveProbeAccountContext 拉账号上下文（套餐 / 配额 / 计划），判断 503 是否为权限问题。
//
// 推理恒 503「Model catalog unavailable」而目录接口正常时，需要区分
// 「账号没有推理权限」与「上游故障」—— 套餐信息是最直接的证据。
//
// 用法：WILDWORK_AUTHDIR=<账号目录> go test -tags live ./internal/qwenwork/ \
//
//	-run TestLiveProbeAccountContext -v
func TestLiveProbeAccountContext(t *testing.T) {
	a := liveProbeAuth(t)
	c := New()
	raw, err := c.doBearer(c.web()+EpAcctCtx, a)
	if err != nil {
		t.Fatalf("account-context 失败：%v", err)
	}
	t.Logf("账号上下文（前 2000 字节）：\n%s", truncate(string(raw), 2000))
}

// TestLiveProbeMachineId 验证「Cosy-MachineId = 桌面端 loginDeviceId」是否为 503 的开关。
//
// 线索：官方桌面客户端 `auth-v2.dat` 里的 `token`（555 字符 JWT）与 wild-work auth 的
// `accessToken` **完全相同**、`user.id` 也与 uid 相同 —— 唯一差别是多一个
// `loginDeviceId`（36 字符；wild-work 的 auth 中 deviceId/machineId/machineToken 全空）。
// 参考实现 Buddy2api 把它作为 `Cosy-MachineId` 头下发（`_headers_for`）。
//
// 用法：
//
//	WILDWORK_AUTHDIR=<账号目录> WILDWORK_PROBE_DEVICEID=<36字符 deviceId> \
//	go test -tags live ./internal/qwenwork/ -run TestLiveProbeMachineId -v
func TestLiveProbeMachineId(t *testing.T) {
	devID := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_DEVICEID"))
	if devID == "" {
		t.Skip("需要 WILDWORK_PROBE_DEVICEID（桌面端 auth-v2.dat 的 loginDeviceId）")
	}
	t.Logf("deviceId：长度=%d 前缀=%.6s", len(devID), devID)

	a := liveProbeAuth(t)
	c := New()
	clientModel := ModelKey(probeModel())
	uid, name, email, token := identity(a)
	prompt := probePromptText()

	for _, cs := range []struct {
		id      string
		machine string
	}{
		{"no-machineid", ""},
		{"with-machineid", devID},
	} {
		reqID, sessID := uuid4(), uuid4()
		raw, err := json.Marshal(buddyStyleBody(clientModel, prompt, reqID, sessID))
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		sess, err := NewCosySession(uid, name, email, token)
		if err != nil {
			t.Fatalf("cosy: %v", err)
		}
		rawURL := c.gateway() + EpChat
		req, err := http.NewRequest(http.MethodPost, rawURL, bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		cosy := map[string]string{}
		if err := sess.ApplyHeadersWithUID(cosy, string(raw), rawURL, uid); err != nil {
			t.Fatalf("cosy headers: %v", err)
		}
		for k, v := range cosy {
			req.Header.Set(k, v)
		}
		for k, v := range buddyStyleHeaders(reqID, clientModel) {
			req.Header.Set(k, v)
		}
		if cs.machine != "" {
			req.Header.Set("Cosy-MachineId", cs.machine)
		}

		t.Logf("── 变体 %s", cs.id)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			t.Logf("   ✗ 传输失败：%v", err)
			continue
		}
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		msg, aerr := aggregate(bytes.NewReader(buf), clientModel)
		if aerr != nil {
			t.Logf("   ✗ HTTP %d 流内错误：%v", resp.StatusCode, aerr)
			continue
		}
		reasoning := msgField(msg, "reasoning_content")
		content := msgField(msg, "content")
		t.Logf("   ✓ HTTP %d，思考=%d 字，回答=%d 字，流 %d 字节",
			resp.StatusCode, len([]rune(reasoning)), len([]rune(content)), len(buf))
		if reasoning != "" {
			t.Logf("   思考片段：%s", truncate(reasoning, 250))
		}
	}
}
