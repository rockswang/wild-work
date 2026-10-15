//go:build live

// live_probe_test.go Qoder「思考档位」真实账号探针。
//
// 目的：验证参考实现（oh-my-pi-plugin-qoder / Peer-Agent）给出的结论在真实上游是否成立：
//  1. 上游模型目录里每个模型是否返回 thinking_config（含 effort ladder 与 is_default）；
//  2. is_reasoning=true/false 是否真的改变思考链长度（这是「思考到底开没开」的唯一硬证据）；
//  3. 老版 api3（COSY / agent_chat_generation）是否接受 parameters.reasoning_effort，
//     以及取值非法时是 400 还是静默忽略；
//  4. 顶层 reasoningEffort（camelCase）是否需要与 parameters.reasoning_effort 双写；
//  5. 关闭语义是否成立（none + max_thinking_tokens=0）。
//
// 本文件带 `live` build tag，**默认不参与编译**（go build ./... / go test ./... 都不会碰它），
// 也不改任何生产代码路径。测完即可删除。
//
// 运行方式（在 wild-work 仓库根目录）：
//
//	WILDWORK_AUTHDIR=./auths go test -tags live ./internal/qoder/ -run TestLiveProbe -v -timeout 1800s
//
// 可调环境变量：
//
//	WILDWORK_AUTHDIR       账号目录（默认 ./auths），读取 qoder*.json
//	WILDWORK_PROBE_MODEL   客户端模型名（默认 deepseek-v4-pro）
//	WILDWORK_PROBE_KEY     直接指定上游 model key，跳过映射（排查「模型名没映射上」时用）
//	WILDWORK_PROBE_PROMPT  探测用 prompt（默认见 defaultProbePrompt）
//	WILDWORK_PROBE_REPEAT  每组重复次数（默认 1，可 1~5，取平均以降低方差）
//	WILDWORK_PROBE_CASES   只跑指定用例 id（逗号分隔，如 "1,2,3"；默认全跑）
//	WILDWORK_PROBE_DUMP    设为目录时把每次请求的原始 SSE 落盘（probe-<id>-<n>.sse）
//
// # 关于 prompt（重要）
//
// **必须用「需要多步推理」的题**。像 "1+1=?" 这种题，模型各档位几乎都不产生思考链，
// reasoning_content 全是空的，只能验证字段是否被接受（状态码），验证不了强度差异。
//
// # 关于计时的坑（2026-09-19 修正）
//
// ChatStream 只返回 resp.Body —— 响应头一到就返回，真正的生成发生在 aggregate() 读流阶段。
// 旧版把 time.Since(start) 卡在 ChatStream 返回处，量到的是 TTFB（≈1s），于是
// 「总耗时 395s ÷ 12 次 = 33s 一次」却显示成「1.11s」。现在分别记录 TTFB 与总耗时。
//
// # 关于请求必须直发的坑（2026-09-19 修正，最关键）
//
// ChatStream 会把入参当 **OpenAI 请求**重新解析（只认 model/messages/tools/
// reasoning_effort/thinking），再重新构造 Qoder body。旧版探针传的是已经构造好的
// Qoder 原生 body，于是 `model` 解析为空、`is_reasoning` 恒为 false、
// `parameters.reasoning_effort` 与顶层 `reasoningEffort` 被整段丢弃 ——
// 6 个用例实际发出的是同一个「空 key + 关思考」请求（上游兜底到 auto），
// 思考长度当然全 0，会把「从没开启过思考」误判成「上游不下发思考」。
// 现在统一走 chatStreamRaw 直发；生产投影另有 TestLiveProbeProjectionDryRun 零成本覆盖。
//
// # 关于模型 key 的坑（2026-09-19 修正）
//
// 静态表 staticModelKeys 只有老模型（无 qwen3.8-flash 等新名），客户端名要靠
// FetchModels 拉到 display_name 才能映射成上游 key（如 qwen3.8-flash → qfmodel）。
// 旧版探针没先调 FetchModels，未命中就回退成「把客户端名原样当 key 发」，
// 上游 200 兜底但内容/思考双空 —— 会把「模型选错」误判成「档位不生效」。
// 现在先调 FetchModels 建映射，未命中会显式告警。
//
// 成本：目录探测 0 次对话；档位探测 = 用例数 × 重复次数 次对话。日志会先打印预估耗时。
package qoder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"wild-work/internal/auth"
	"wild-work/internal/reasoning"
)

// liveAuth 从 WILDWORK_AUTHDIR（默认 ./auths）取第一个 Qoder 账号。
func liveAuth(t *testing.T) *auth.Auth {
	t.Helper()
	dir := os.Getenv("WILDWORK_AUTHDIR")
	if dir == "" {
		dir = "auths"
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("解析目录失败：%v", err)
	}
	as, err := auth.LoadQoderDir(abs)
	if err != nil {
		t.Fatalf("加载 %s 下的 qoder*.json 失败：%v", abs, err)
	}
	if len(as) == 0 {
		t.Skipf("在 %s 没找到 qoder*.json —— 先用面板登录一个 Qoder 账号，或用 WILDWORK_AUTHDIR 指向账号目录", abs)
	}
	a := as[0]
	t.Logf("账号：uid=%s nickname=%s file=%s", a.UID, a.Nickname, a.FilePath)
	return a
}

// fetchModelsRaw 拉上游模型目录并返回**原始** entry JSON。
// 与 models.go 的 fetchModels 同构，区别是保留原始 JSON —— 因为 DynamicModel
// 没有 thinking_config 字段，只有原始 JSON 才能看到上游到底返回了什么。
func (c *Client) fetchModelsRaw(a *auth.Auth) ([]json.RawMessage, error) {
	dt := a.JWT()
	if dt == "" {
		return nil, fmt.Errorf("no dt- available")
	}
	rawURL := c.gatewayBase() + EpModels
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	sess, err := NewCosySession(a.MachineID, a.MachineToken, a.MachineType, a.Nickname, a.UID, dt, a.RefreshToken)
	if err != nil {
		return nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, "", rawURL, a.UID, false, ""); err != nil {
		return nil, fmt.Errorf("cosy headers: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var apiResp map[string]json.RawMessage
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(apiResp["chat"], &entries); err != nil {
		return nil, fmt.Errorf("chat scene parse: %w", err)
	}
	return entries, nil
}

// catalogEntry 上游目录里一个模型的探针视图。
type catalogEntry struct {
	Key         string
	DisplayName string
	ClientName  string // NormalizeModelName(DisplayName)
	Enable      bool
	IsReasoning bool
	MaxInput    int64
	HasTC       bool
	TC          json.RawMessage
	Caps        thinkCaps
}

// loadCatalog 拉目录并解析出探针需要的字段（同时打印）。
func loadCatalog(t *testing.T, c *Client, a *auth.Auth) []catalogEntry {
	t.Helper()
	raw, err := c.fetchModelsRaw(a)
	if err != nil {
		t.Fatalf("拉模型目录失败：%v", err)
	}
	out := make([]catalogEntry, 0, len(raw))
	for _, e := range raw {
		var m map[string]any
		if err := json.Unmarshal(e, &m); err != nil {
			continue
		}
		ce := catalogEntry{}
		ce.Key, _ = m["key"].(string)
		ce.DisplayName, _ = m["display_name"].(string)
		ce.ClientName = NormalizeModelName(ce.DisplayName)
		ce.Enable, _ = m["enable"].(bool)
		ce.IsReasoning, _ = m["is_reasoning"].(bool)
		if v, ok := m["max_input_tokens"].(float64); ok {
			ce.MaxInput = int64(v)
		}
		if tc, ok := m["thinking_config"]; ok && tc != nil {
			b, _ := json.Marshal(tc)
			ce.HasTC = true
			ce.TC = b
			ce.Caps = parseThinkingConfig(b)
		}
		out = append(out, ce)
	}
	return out
}

// TestLiveProbeModelCatalog 第 1 步：打印上游模型目录里的思考相关字段。
// 零对话成本，先跑这个 —— 若上游压根不返回 thinking_config，档位就只能硬编码。
func TestLiveProbeModelCatalog(t *testing.T) {
	a := liveAuth(t)
	c := New()
	entries := loadCatalog(t, c, a)
	t.Logf("目录共 %d 个模型（客户端名 = display_name 规范化，生产环境用这个名字调）", len(entries))

	withTC := 0
	for _, e := range entries {
		if !e.Enable {
			continue
		}
		if !e.HasTC {
			t.Logf("  %-16s client=%-16s is_reasoning=%-5v thinking_config=<无>", e.Key, e.ClientName, e.IsReasoning)
			continue
		}
		withTC++
		t.Logf("  %-16s client=%-16s is_reasoning=%-5v max_in=%-7d ladder=%v default=%q 可关闭=%v",
			e.Key, e.ClientName, e.IsReasoning, e.MaxInput, e.Caps.Efforts, e.Caps.DefaultEffort, e.Caps.SupportsDisable)
		t.Logf("      raw=%s", truncate(string(e.TC), 220))
	}
	t.Logf("==> %d 个模型带 thinking_config", withTC)
	if withTC == 0 {
		t.Log("==> 上游未返回 thinking_config：档位无上游依据，只能按模型硬编码或用默认档")
	} else {
		t.Log("==> ladder 就是该模型真实支持的档位集合；探针第 2 步会自动从这里挑档位来测")
	}
}

// defaultProbePrompt 默认探测题：刻意选「需要多步推理、答案短」的题。
// 这样 content 短、reasoning_content 长，档位差异最容易看出来。
const defaultProbePrompt = `9 个外观完全相同的球中有 1 个重量异常（可能偏重也可能偏轻），其余 8 个等重。` +
	`只用一个天平称 3 次，找出这个异常球并判断它是偏重还是偏轻。` +
	`请逐步推理给出完整的称量方案，并说明为什么 3 次一定够。`

// probePrompt 探测用 prompt，可用 WILDWORK_PROBE_PROMPT 覆盖。
func probePrompt() string {
	if p := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_PROMPT")); p != "" {
		return p
	}
	return defaultProbePrompt
}

// probeRepeat 每组重复次数（默认 1），可用 WILDWORK_PROBE_REPEAT 覆盖（1~5）。
// 单次结果有方差，重复 2~3 次取平均更稳（成本线性增加）。
func probeRepeat() int {
	if v := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_REPEAT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 5 {
			return n
		}
	}
	return 1
}

// ---- 思考能力解析 ----
//
// 直接复用生产实现 models.go 的 parseThinkingConfig / thinkCaps：
// 探针若另写一份解析，两边会各自漂移，反而掩盖「生产解析读错字段」这类问题。
// 探针只负责把解析结果打到日志，供人工核对上游真实形状。

// ---- 原始流抓取与诊断 ----

// captureReader 边读边留一份原始字节（诊断上游到底发了什么）。
type captureReader struct {
	r    io.Reader
	buf  bytes.Buffer
	max  int
	file *os.File
}

func newCaptureReader(r io.Reader, max int, path string) (*captureReader, error) {
	cr := &captureReader{r: r, max: max}
	if path != "" {
		f, err := os.Create(path)
		if err != nil {
			return nil, err
		}
		cr.file = f
	}
	return cr, nil
}

func (c *captureReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		if c.buf.Len() < c.max {
			room := c.max - c.buf.Len()
			if n < room {
				room = n
			}
			c.buf.Write(p[:room])
		}
		if c.file != nil {
			_, _ = c.file.Write(p[:n])
		}
	}
	return n, err
}

func (c *captureReader) Close() error {
	if c.file != nil {
		err := c.file.Close()
		c.file = nil
		return err
	}
	return nil
}

func (c *captureReader) Bytes() []byte { return c.buf.Bytes() }

// streamStats 原始流的解析统计 —— 用来区分「上游没发思考」与「我们没解析出来」。
type streamStats struct {
	Bytes      int
	DataLines  int
	Chunks     int
	BadLines   int
	DeltaKeys  map[string]int
	ContentIn  int            // 非空 delta.content 块数
	ReasonIn   int            // 非空 delta.reasoning_content 块数
	ChoiceRsn  int            // 非空 choice.reasoning_content（思考挂在 choice 层而非 delta 层）
	MsgField   int            // 出现 choice.message（非流式形态）
	Other      []string       // delta 里出现「非 content/role/reasoning_content」键的 chunk 原文
	TopKeys    map[string]int // chunk 顶层键直方图
	OtherLines map[string]int // 非 data: 行前缀直方图
	// 以下为「穷举取证」字段：思考可能挂在别处（choice 层别名键、外层信封、独立事件），
	// 只盯 delta.reasoning_content 一个名字容易漏判。
	ChoiceKeys map[string]int // choice 层键直方图（穷举 thinking/reasoning/… 等别名）
	EnvKeys    map[string]int // 外层信封键直方图（headers/body/…）
	Models     map[string]int // chunk.model 值（上游恒为 auto，可用于证伪「key 生效」）
	FinishRsn  map[string]int // finish_reason 值直方图
	Done       bool           // 流里是否出现 data: [DONE]
	Tail       string         // 流尾部 200 字节（判断是被截断还是正常收尾）
	First      []string
}

// Detail 输出「思考到底藏在哪」的穷举证据：外层信封键、choice 层键、上游 model、
// finish_reason、是否见到 [DONE]、流尾部字节。回答有字但思考为 0 时必须看这些 ——
// 只检查 delta.reasoning_content 一个字段，漏判概率不低。
func (s streamStats) Detail() string {
	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n     · 外层信封键=[%s]", hist(s.EnvKeys)))
	b.WriteString(fmt.Sprintf("\n     · choice 层键=[%s] finish_reason=[%s]", hist(s.ChoiceKeys), hist(s.FinishRsn)))
	b.WriteString(fmt.Sprintf("\n     · 上游返回 model=[%s] 见到[DONE]=%v", hist(s.Models), s.Done))
	if s.Tail != "" {
		b.WriteString(fmt.Sprintf("\n     · 流尾部 200B：%s", s.Tail))
	}
	return b.String()
}

// scanStream 用生产同款解析器扫一遍原始字节，统计各字段出现情况。
// 若 Chunks==0 但 DataLines>0 → 上游 SSE 信封不是 data:{"body":"..."} 这个形状。
func scanStream(raw []byte) streamStats {
	st := streamStats{
		DeltaKeys: map[string]int{}, TopKeys: map[string]int{}, OtherLines: map[string]int{},
		ChoiceKeys: map[string]int{}, EnvKeys: map[string]int{}, Models: map[string]int{},
		FinishRsn: map[string]int{}, Bytes: len(raw),
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimPrefix(line, "data:")
			if strings.Contains(payload, "[DONE]") {
				st.Done = true
				continue
			}
			st.DataLines++
			if len(st.First) < 3 {
				st.First = append(st.First, truncate(line, 160))
			}
			// 外层信封键：正常是 {headers, body}。多出别的键（如 reasoning/thinking）
			// 就说明思考挂在信封层而不是内层 chunk 里。
			var env map[string]any
			if json.Unmarshal([]byte(payload), &env) == nil {
				for k := range env {
					st.EnvKeys[k]++
				}
			}
		} else if line != "" {
			prefix := line
			if i := strings.Index(line, ":"); i >= 0 {
				prefix = line[:i]
			}
			st.OtherLines[prefix]++
		}
	}
	_ = parseNestedSSE(bytes.NewReader(raw), func(chunk map[string]any) error {
		st.Chunks++
		for k := range chunk {
			st.TopKeys[k]++
		}
		if v, ok := chunk["model"].(string); ok && v != "" {
			st.Models[v]++
		}
		if choices, ok := chunk["choices"].([]any); ok {
			for _, ci := range choices {
				ch, _ := ci.(map[string]any)
				if ch == nil {
					continue
				}
				for k := range ch {
					st.ChoiceKeys[k]++
				}
				if v, ok := ch["finish_reason"].(string); ok && v != "" {
					st.FinishRsn[v]++
				}
				if _, ok := ch["message"]; ok {
					st.MsgField++
				}
				if v, ok := ch["reasoning_content"].(string); ok && v != "" {
					st.ChoiceRsn++
				}
				delta, _ := ch["delta"].(map[string]any)
				if delta == nil {
					continue
				}
				hasOther := false
				for k, v := range delta {
					if s, ok := v.(string); !ok || s != "" {
						st.DeltaKeys[k]++
						if k != "content" && k != "role" && k != "reasoning_content" {
							hasOther = true
						}
					}
				}
				if hasOther {
					b, _ := json.Marshal(delta)
					st.Other = append(st.Other, truncate(string(b), 200))
				}
				if v, ok := delta["content"].(string); ok && v != "" {
					st.ContentIn++
				}
				if v, ok := delta["reasoning_content"].(string); ok && v != "" {
					st.ReasonIn++
				}
			}
		}
		return nil
	}, nil)
	if st.DataLines > st.Chunks {
		st.BadLines = st.DataLines - st.Chunks
	}
	// 流尾部：被截断（unexpected EOF）时能看出是「生成中途断」还是「正常收尾」。
	tail := strings.TrimSpace(string(raw))
	if len(tail) > 200 {
		tail = tail[len(tail)-200:]
	}
	st.Tail = strings.ReplaceAll(tail, "\n", "⏎")
	return st
}

func (s streamStats) String() string {
	keys := make([]string, 0, len(s.DeltaKeys))
	for k, n := range s.DeltaKeys {
		keys = append(keys, fmt.Sprintf("%s×%d", k, n))
	}
	sort.Strings(keys)
	return fmt.Sprintf("原始%dB data行%d chunk%d 未解析%d content块%d reasoning块%d choice 层思考%d 键=[%s]",
		s.Bytes, s.DataLines, s.Chunks, s.BadLines, s.ContentIn, s.ReasonIn, s.ChoiceRsn, strings.Join(keys, " "))
}

func hist(m map[string]int) string {
	if len(m) == 0 {
		return "<空>"
	}
	kvs := make([]string, 0, len(m))
	for k, n := range m {
		kvs = append(kvs, fmt.Sprintf("%s×%d", k, n))
	}
	sort.Strings(kvs)
	return strings.Join(kvs, ", ")
}

// ---- 用例 ----

// probeCase 一组档位注入方式。
type probeCase struct {
	id     string
	name   string
	inject string // 实际注入的字段（打印出来对照）
	// shortPrompt 为 true 时用最简 prompt：只关心状态码的用例（如非法值），
	// 不必花长思考的钱。
	shortPrompt bool
	// reasoning 传给 buildAgentBody 的 is_reasoning 值。
	reasoning bool
	mutate    func(obj map[string]any)
}

// probeResult 单组结果（重复多次时长度与耗时取平均）。
type probeResult struct {
	id        string
	name      string
	status    int
	ttfb      time.Duration // 响应头到达耗时
	total     time.Duration // 含读完整条流的耗时
	reasoning int           // 平均思考长度（rune 数）
	content   int           // 平均回答长度（rune 数）
	usage     string        // 流末尾 usage 块（含 reasoning_tokens 时能判断「想了但不下发」）
	stats     streamStats
	note      string
}

// setNestedModelConfig 把键写进 chat_context.extra.modelConfig（协议里 is_reasoning
// 与 key 都在这层重复出现，档位很可能也在这里）。
func setNestedModelConfig(o map[string]any, k string, v any) {
	cc, _ := o["chat_context"].(map[string]any)
	if cc == nil {
		return
	}
	ex, _ := cc["extra"].(map[string]any)
	if ex == nil {
		return
	}
	mc, _ := ex["modelConfig"].(map[string]any)
	if mc == nil {
		return
	}
	mc[k] = v
}

// setParams 往 parameters 对象里合并多个键。
func setParams(o map[string]any, kv map[string]any) {
	p, _ := o["parameters"].(map[string]any)
	if p == nil {
		p = map[string]any{}
		o["parameters"] = p
	}
	for k, v := range kv {
		p[k] = v
	}
}

// buildProbeCases 按模型能力生成用例表。
//
// 关键：**必须包含 is_reasoning=false / true 的对照**。旧版所有用例都硬编码
// is_reasoning=true，只比了 parameters 层，一旦上游根本不吐思考链就完全看不出来。
func buildProbeCases(caps thinkCaps) []probeCase {
	def := "high"
	maxEff := "high"
	if len(caps.Efforts) > 0 {
		low := caps.Efforts[0]
		_ = low
		def = caps.Efforts[len(caps.Efforts)/2]
		maxEff = caps.Efforts[len(caps.Efforts)-1]
	}
	setParam := func(o map[string]any, k string, v any) func(map[string]any) {
		return func(o map[string]any) {
			p, _ := o["parameters"].(map[string]any)
			if p == nil {
				p = map[string]any{}
				o["parameters"] = p
			}
			p[k] = v
		}
	}
	cases := []probeCase{
		{id: "1", name: "is_reasoning=false（关闭基线）", inject: "model_config.is_reasoning=false", reasoning: false},
		{id: "2", name: "is_reasoning=true（当前生产行为）", inject: "model_config.is_reasoning=true", reasoning: true},
		{id: "3", name: fmt.Sprintf("parameters.reasoning_effort=%s（默认档）", def),
			inject: "is_reasoning=true + parameters.reasoning_effort=" + def, reasoning: true,
			mutate: setParam(nil, "reasoning_effort", def)},
		{id: "5", name: fmt.Sprintf("顶层 reasoningEffort=%s（单写）", def),
			inject: "is_reasoning=true + 顶层 reasoningEffort=" + def, reasoning: true,
			mutate: func(o map[string]any) { o["reasoningEffort"] = def }},
		{id: "6", name: fmt.Sprintf("顶层 + parameters 双写 %s", def),
			inject: "is_reasoning=true + 顶层 reasoningEffort=" + def + " + parameters.reasoning_effort=" + def, reasoning: true,
			mutate: func(o map[string]any) {
				o["reasoningEffort"] = def
				p, _ := o["parameters"].(map[string]any)
				if p == nil {
					p = map[string]any{}
					o["parameters"] = p
				}
				p["reasoning_effort"] = def
			}},
		{id: "7", name: "none + max_thinking_tokens=0（关闭语义）",
			inject: "is_reasoning=true + parameters.reasoning_effort=none + max_thinking_tokens=0", reasoning: true,
			mutate: func(o map[string]any) {
				p, _ := o["parameters"].(map[string]any)
				if p == nil {
					p = map[string]any{}
					o["parameters"] = p
				}
				p["reasoning_effort"] = "none"
				p["max_thinking_tokens"] = 0
			}},
		{id: "8", name: "非法值 bogus（只看状态码）", inject: "parameters.reasoning_effort=bogus",
			shortPrompt: true, reasoning: true,
			mutate: setParam(nil, "reasoning_effort", "bogus")},
		// 用例 9/10 针对「CLI 的 --reasoning-effort 到底写在哪」这个未解问题。
		// 依据：wild-work 自己就把 is_reasoning 同时写在 model_config 与
		// chat_context.extra.modelConfig 两处（body.go:61/67），档位很可能同样双写；
		// 而 chat_context 内部是 camelCase 容器 + snake_case 字段名（chatPrompt/modelConfig/is_reasoning）。
		{id: "9", name: fmt.Sprintf("chat_context.extra.modelConfig.reasoning_effort=%s", def),
			inject: "is_reasoning=true + chat_context.extra.modelConfig.reasoning_effort=" + def, reasoning: true,
			mutate: func(o map[string]any) { setNestedModelConfig(o, "reasoning_effort", def) }},
		{id: "10", name: fmt.Sprintf("两处 modelConfig 双写 %s（最接近 CLI 形态）", def),
			inject:    "is_reasoning=true + model_config.reasoning_effort + chat_context.extra.modelConfig.reasoning_effort=" + def,
			reasoning: true,
			mutate: func(o map[string]any) {
				if mc, ok := o["model_config"].(map[string]any); ok {
					mc["reasoning_effort"] = def
				}
				setNestedModelConfig(o, "reasoning_effort", def)
			}},
		// 用例 11/12 直接照抄 Qoder CN 桌面版（Electron，SDK v1.1.53）构造 legacy
		// agent_chat_generation body 的官方写法（qoder-worker-runtime.obf.mjs 的 bve()）：
		//
		//	parameters.reasoning_effort = <档位>
		//	parameters.enable_thinking  = true           // ← 关键：随档位一起写
		//	parameters.max_tokens       = <默认输出上限>
		//	effort == "none" → enable_thinking=false，并删掉 reasoning_budget_tokens
		//	effort == "none" / enable_thinking==false → model_config.is_reasoning = false
		//
		// 之前只注入 reasoning_effort、不注入 enable_thinking —— 与官方请求不一致，
		// 很可能是「思考 0 字」的真正原因。
		{id: "11", name: fmt.Sprintf("桌面版官方形态 %s（reasoning_effort+enable_thinking）", def),
			inject:    "is_reasoning=true + parameters{reasoning_effort=" + def + ", enable_thinking=true, max_tokens=32768}",
			reasoning: true,
			mutate: func(o map[string]any) {
				setParams(o, map[string]any{"reasoning_effort": def, "enable_thinking": true, "max_tokens": 32768})
			}},
		{id: "12", name: "桌面版关闭形态（none + enable_thinking=false）",
			inject:    "is_reasoning=true + parameters{reasoning_effort=none, enable_thinking=false, max_tokens=32768}",
			reasoning: true,
			mutate: func(o map[string]any) {
				setParams(o, map[string]any{"reasoning_effort": "none", "enable_thinking": false, "max_tokens": 32768})
			}},
	}
	if maxEff != def {
		cases = append(cases[:3], append([]probeCase{{
			id: "4", name: fmt.Sprintf("parameters.reasoning_effort=%s（最高档）", maxEff),
			inject: "is_reasoning=true + parameters.reasoning_effort=" + maxEff, reasoning: true,
			mutate: setParam(nil, "reasoning_effort", maxEff),
		}}, cases[3:]...)...)
	}
	return cases
}

// filterCases 按 WILDWORK_PROBE_CASES（逗号分隔 id）过滤。
func filterCases(all []probeCase) []probeCase {
	sel := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_CASES"))
	if sel == "" {
		return all
	}
	want := map[string]bool{}
	for _, s := range strings.Split(sel, ",") {
		want[strings.TrimSpace(s)] = true
	}
	out := make([]probeCase, 0, len(all))
	for _, c := range all {
		if want[c.id] {
			out = append(out, c)
		}
	}
	return out
}

// msgField 从聚合结果里取 message 级字段（content / reasoning_content）。
//
// 注意：aggregate 返回的是**完整 chat.completion**，正文在 choices[0].message 下，
// 不在顶层。早期探针直接读 msg["content"] / msg["reasoning_content"] → 恒为空，
// 把「有内容、有思考」一律误判成 0（真实流里 848 个 content 分片却报「回答 0 字」）。
// 这里按 message → choice → 顶层 三级兜底取。
func msgField(msg map[string]any, key string) string {
	if v, ok := msg[key].(string); ok && v != "" {
		return v
	}
	choices, _ := msg["choices"].([]any)
	if len(choices) == 0 {
		return ""
	}
	ch, _ := choices[0].(map[string]any)
	if ch == nil {
		return ""
	}
	if m, ok := ch["message"].(map[string]any); ok {
		if v, ok := m[key].(string); ok {
			return v
		}
	}
	if v, ok := ch[key].(string); ok {
		return v
	}
	return ""
}

// TestLiveProbeAggregateExtract 零成本回归：确认 msgField 能从 aggregate 结果里取到
// 正文与思考（字段在 choices[0].message 下）。不联网。
func TestLiveProbeAggregateExtract(t *testing.T) {
	mk := func(chunk map[string]any) string {
		body, _ := json.Marshal(chunk)
		env, _ := json.Marshal(map[string]any{"headers": map[string]any{"Content-Type": []string{"application/json"}}, "body": string(body)})
		return "data:" + string(env)
	}
	var b strings.Builder
	b.WriteString(mk(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"reasoning_content": "先称 123 对 456", "role": "assistant"}, "index": 0}},
		"id":      "chatcmpl-t",
	}) + "\n")
	b.WriteString(mk(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{"content": "把 1、2、3 放左盘"}, "index": 0}},
		"id":      "chatcmpl-t",
	}) + "\n")
	b.WriteString(`data:{"headers":{"Content-Type":["application/json"]},"body":"[DONE]"}` + "\n")

	msg, err := aggregate(strings.NewReader(b.String()), "m")
	if err != nil {
		t.Fatalf("aggregate: %v", err)
	}
	gotContent := msgField(msg, "content")
	gotReason := msgField(msg, "reasoning_content")
	if gotContent != "把 1、2、3 放左盘" {
		t.Errorf("msgField(content) = %q，期望 %q（字段在 choices[0].message 下）", gotContent, "把 1、2、3 放左盘")
	}
	if gotReason != "先称 123 对 456" {
		t.Errorf("msgField(reasoning_content) = %q，期望 %q", gotReason, "先称 123 对 456")
	}
	t.Logf("提取正确：content=%q reasoning=%q", gotContent, gotReason)
}

// TestLiveProbeCaseTableDryRun 零成本：验证用例表生成逻辑（ladder 解析、档位选择、注入字段）。
// 不联网，仅确认 probeCase 构造正确。
func TestLiveProbeCaseTableDryRun(t *testing.T) {
	testLadder := func(name string, ladder []string) {
		caps := thinkCaps{Efforts: ladder, DefaultEffort: "high", SupportsDisable: true}
		cases := buildProbeCases(caps)
		t.Logf("== %s（%v）", name, ladder)
		t.Logf("   ladder=%v default=%q 可关闭=%v", caps.Efforts, caps.DefaultEffort, caps.SupportsDisable)
		for _, c := range cases {
			t.Logf("   [%s] %-40s 注入：%s", c.id, c.name, c.inject)
		}
	}
	testLadder("deepseek/glm 系", []string{"high", "max"})
	testLadder("qwen3.8 系", []string{"low", "medium", "xhigh"})
	testLadder("无 ladder（只有开关）", []string{})

	// 再打一个用例 3 的请求体关键字段，确认 parameters 确实被注入了。
	cases := buildProbeCases(thinkCaps{Efforts: []string{"low", "medium", "xhigh"}})
	for _, c := range cases {
		if c.id == "3" {
			var obj map[string]any
			raw, _ := buildAgentBody([]map[string]any{{"role": "user", "content": "test"}}, "qfmodel", nil, reasoningSpec{Enabled: true, Effort: "medium"}, 0)
			json.Unmarshal(raw, &obj)
			c.mutate(obj)
			jb, _ := json.Marshal(obj)
			t.Logf("== 用例 3 的请求体关键字段：%s", truncate(string(jb), 180))
			break
		}
	}
}

// chatStreamRaw 用**探针自己构造的 Qoder 原生 body** 直发上游，不经过 ChatStream 的
// OpenAI→Qoder 投影。
//
// # 为什么必须直发（2026-09-19 血泪教训）
//
// ChatStream 会把入参当 **OpenAI 请求**重新解析（只认 model/messages/tools/
// reasoning_effort/thinking），再重新调 buildAgentBody 构造 Qoder body。探针传的是
// 已经构造好的 Qoder 原生 body（model_config / parameters / 顶层 reasoningEffort），于是：
//   - `model` 解析为空 → model_config.key=""（上游兜底到 auto）
//   - `is_reasoning` 由 reasoningEnabled("", nil) 算出 → 恒为 false
//   - 注入的 parameters.reasoning_effort / 顶层 reasoningEffort 被整段丢弃
//
// 结果：所有用例实际发出的是同一个「空 key + 关思考」请求，思考长度当然全 0 ——
// 会把「上游不下发思考」误判成结论。
//
// 用这个函数才能把任意字段真实送达上游。
func (c *Client) chatStreamRaw(a *auth.Auth, body []byte, modelKey string) (io.ReadCloser, int, []byte, error) {
	encoded := qoderEncode(body)
	url := c.gatewayBase() + EpChat
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(encoded))
	if err != nil {
		return nil, 0, nil, err
	}
	dt := a.JWT()
	sess, err := NewCosySession(a.MachineID, a.MachineToken, a.MachineType, a.Nickname, a.UID, dt, a.RefreshToken)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, encoded, url, a.UID, true, modelKey); err != nil {
		return nil, 0, nil, fmt.Errorf("cosy headers: %w", err)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// bodyFacts 抽取要发给上游的关键字段，用于在日志里确认「注入到底有没有生效」。
func bodyFacts(obj map[string]any) string {
	mc, _ := obj["model_config"].(map[string]any)
	p, _ := obj["parameters"].(map[string]any)
	return fmt.Sprintf("model_config=%v parameters=%v 顶层 reasoningEffort=%v",
		mc, p, obj["reasoningEffort"])
}

// TestLiveProbeProjectionDryRun 零成本：验证生产路径（ChatStream）从 OpenAI 风格请求
// 投影出的 Qoder body 是否符合官方 bve() 的写法 —— 开关 + parameters 两处同源。
// 不联网。这个用例能挡住「探针误用」与「投影逻辑失效」两类问题。
//
// 用假的模型能力表覆盖 Qoder 面，模拟上游目录声明（真实 ladder 见 ModelCatalog 用例）。
func TestLiveProbeProjectionDryRun(t *testing.T) {
	reasoning.Caps.SetRemote(reasoning.RealmQoder, map[string]reasoning.Cap{
		"qwen3.8-flash": {Efforts: []string{"low", "medium", "xhigh"}, DefaultEffort: "medium", SupportsDisable: true},
		"glm-5.3":       {Efforts: []string{"low", "high", "max"}, DefaultEffort: "max"},
	})
	project := func(name string, openaiBody string) {
		var req struct {
			Model           string           `json:"model"`
			Messages        []map[string]any `json:"messages"`
			Tools           []any            `json:"tools"`
			ReasoningEffort string           `json:"reasoning_effort"`
			Thinking        *thinkingParam   `json:"thinking"`
		}
		if err := json.Unmarshal([]byte(openaiBody), &req); err != nil {
			t.Fatalf("%s: 解析失败: %v", name, err)
		}
		spec := reasoningSpecFor(req.Model, req.ReasoningEffort, req.Thinking)
		raw, err := buildAgentBody(req.Messages, "qfmodel", req.Tools, spec, 0)
		if err != nil {
			t.Fatalf("%s: build 失败: %v", name, err)
		}
		var obj map[string]any
		json.Unmarshal(raw, &obj)
		t.Logf("%-30s → is_reasoning=%-5v effort=%-7q | %s", name, spec.Enabled, spec.Effort, bodyFacts(obj))
	}
	project("无 reasoning 字段", `{"model":"qwen3.8-flash","messages":[{"role":"user","content":"hi"}]}`)
	project("reasoning_effort=high（降级）", `{"model":"qwen3.8-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	project("reasoning_effort=xhigh", `{"model":"qwen3.8-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"xhigh"}`)
	project("reasoning_effort=none（可关模型）", `{"model":"qwen3.8-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)
	project("reasoning_effort=none（不可关模型）", `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`)
	project("thinking.type=enabled", `{"model":"qwen3.8-flash","messages":[{"role":"user","content":"hi"}],"thinking":{"type":"enabled"}}`)
	t.Log("== 生产路径按官方 A6e() 投影：model_config.is_reasoning 与 parameters.enable_thinking 同源，")
	t.Log("   parameters.reasoning_effort 按该模型 ladder 就近降级；parameters 恒下发（至少带 max_tokens）。")
}

// TestLiveProbeProductionPath 走**生产链路**（ChatStream → buildAgentBodyMeta →
// ApplyHeaders → sse.go aggregate），确认思考链真的能透到 OpenAI 兼容层。
//
// 与 TestLiveProbeEffort 的区别：那个用 chatStreamRaw 手搓 body 直发，只验证
// 「上游在什么条件下吐思考」；这个验证「wild-work 自己发出去的请求就能拿到思考」，
// 顺带覆盖动态模型元数据（model_config 的 display_name/is_vl/max_input_tokens…）
// 是否真的被带上。2026-09-20 之前这里恒为 0 字（body/header 未对齐桌面版）。
func TestLiveProbeProductionPath(t *testing.T) {
	a := liveAuth(t)
	c := New()
	if _, err := c.FetchModels(a); err != nil {
		t.Fatalf("FetchModels 失败（生产路径依赖动态元数据）：%v", err)
	}

	clientModel := os.Getenv("WILDWORK_PROBE_MODEL")
	if clientModel == "" {
		clientModel = "qwen3.8-flash"
	}
	key := c.modelKey(clientModel)
	meta := c.modelMetaFor(key)
	t.Logf("客户端名=%s 上游 key=%s 元数据=%+v", clientModel, key, meta)
	// 动态元数据必须真的拉到：display_name 来自目录、默认窗口来自 context_config。
	// 注意 max_output_tokens 目录里没有（实测 2026-09-20），恒由 defaultMaxOutputTokens 兜底，
	// 所以不能把它当「元数据拉到了」的判据。
	if meta.DisplayName == "" || meta.DefaultContextWindow == 0 {
		t.Fatalf("动态元数据没拉到（%+v）—— 生产路径会退化成默认值，用例失去意义", meta)
	}

	body, err := json.Marshal(map[string]any{
		"model":            clientModel,
		"messages":         []map[string]any{{"role": "user", "content": probePrompt()}},
		"reasoning_effort": "xhigh",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	start := time.Now()
	rc, status, respBody, err := c.ChatStream(a, body)
	if err != nil {
		t.Fatalf("ChatStream 传输失败：%v", err)
	}
	if rc == nil {
		t.Fatalf("ChatStream 被拒：status=%d body=%s", status, truncate(string(respBody), 300))
	}
	defer rc.Close()

	msg, err := aggregate(rc, clientModel)
	if err != nil {
		t.Fatalf("aggregate 解析失败：%v", err)
	}
	reasoning := msgField(msg, "reasoning_content")
	content := msgField(msg, "content")
	t.Logf("status=%d 耗时=%s 思考=%d 字 回答=%d 字", status, time.Since(start).Round(time.Millisecond),
		len([]rune(reasoning)), len([]rune(content)))
	if u, ok := msg["usage"].(map[string]any); ok {
		if b, err := json.Marshal(u); err == nil {
			t.Logf("usage=%s", b)
		}
	}
	if reasoning == "" {
		t.Errorf("生产链路没拿到 reasoning_content —— body/header 与桌面版又不一致了（见 _spy/qoder-wire-body-spec.md）")
	}
}

// TestLiveProbeEffort 第 2 步：同一 prompt 下横向对比不同字段注入的行为。
//
// 判读：
//   - 用例 2（is_reasoning=true）的思考长度显著大于用例 1（false）→ 开关有效
//   - 用例 3/4 之间出现长度梯度           → 档位字段生效
//   - 用例 7 归零或显著变小               → 关闭语义成立
//   - 用例 8 非 200 → 上游严格校验（客户端须白名单）；200 → 静默忽略（不能盲信）
//   - ⚠️ 用例 1 是**生产默认形态**（客户端不带任何 reasoning 字段 → `reasoningSpec{}`）：
//     它若「思考块极多 + 流被截断」而用例 12（带 `enable_thinking=false`）正常收尾，
//     就说明 body 漏了下发 `enable_thinking`。2026-09-22 实测正是如此
//     （用例 1：1793 个 reasoning 块 / 608KB / 3m0s 截断；用例 12：0 块 / 48.2s）。
//
// 若所有用例的「回答」列都是 0，说明流根本没解析出来 —— 先看每行下方的
// 「原始流」诊断与首行样本，多半是模型 key 没映射上或 SSE 信封变了。
func TestLiveProbeEffort(t *testing.T) {
	a := liveAuth(t)
	c := New()

	clientModel := os.Getenv("WILDWORK_PROBE_MODEL")
	if clientModel == "" {
		clientModel = "deepseek-v4-pro"
	}

	// 关键：先建动态映射表，否则新模型名（如 qwen3.8-flash）映射不到上游 key。
	entries := loadCatalog(t, c, a)
	if _, err := c.FetchModels(a); err != nil {
		t.Logf("警告：FetchModels 建映射失败（%v），将退回静态表", err)
	}

	modelKey := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_KEY"))
	if modelKey == "" {
		modelKey = c.modelKey(clientModel)
	}
	if modelKey == "" {
		modelKey = clientModel
		t.Logf("警告：客户端名 %q 没映射到任何上游 key（静态表与动态目录都没有）。", clientModel)
		t.Logf("     会把 %q 原样当作 key 发给上游 —— 上游通常 200 兜底但内容/思考双空，结论不可用。", modelKey)
		t.Logf("     可用 WILDWORK_PROBE_MODEL 指定目录里的 client 名，或用 WILDWORK_PROBE_KEY 直接指定 key。")
	}

	var caps thinkCaps
	for _, e := range entries {
		if e.Key == modelKey {
			caps = e.Caps
			break
		}
	}
	if len(caps.Efforts) == 0 {
		t.Logf("警告：上游目录里 %s 没有 effort ladder，档位用例会退化为 high 单档", modelKey)
	}

	prompt := probePrompt()
	repeat := probeRepeat()
	t.Logf("探测模型：客户端名=%s 上游 key=%s；每组重复 %d 次", clientModel, modelKey, repeat)
	t.Logf("模型 ladder：%v（默认档=%q，支持关闭=%v）", caps.Efforts, caps.DefaultEffort, caps.SupportsDisable)
	t.Logf("探测 prompt（%d 字）：%s", len([]rune(prompt)), truncate(strings.ReplaceAll(prompt, "\n", " "), 90))

	cases := filterCases(buildProbeCases(caps))
	if len(cases) == 0 {
		t.Fatalf("WILDWORK_PROBE_CASES=%q 没匹配到任何用例", os.Getenv("WILDWORK_PROBE_CASES"))
	}
	dumpDir := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_DUMP"))
	t.Logf("用例 %d 组 × 重复 %d 次 = %d 次对话；上次实测约 33s/次 → 预计 %.1f 分钟",
		len(cases), repeat, len(cases)*repeat, float64(len(cases)*repeat)*33/60)

	results := make([]probeResult, 0, len(cases))
	for _, tc := range cases {
		usePrompt := prompt
		if tc.shortPrompt {
			usePrompt = "1+1=?"
		}
		t.Logf("[用例 %s] %s | 注入：%s", tc.id, tc.name, tc.inject)
		res := probeResult{id: tc.id, name: tc.name}
		var sumReason, sumContent int
		var sumTTFB, sumTotal time.Duration
		okCount := 0
		for i := 0; i < repeat; i++ {
			messages := []map[string]any{{"role": "user", "content": usePrompt}}
			raw, err := buildAgentBody(messages, modelKey, nil, reasoningSpec{Enabled: tc.reasoning}, 0)
			if err != nil {
				t.Fatalf("%s: 构造 body 失败：%v", tc.name, err)
			}
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatalf("%s: body 解析失败：%v", tc.name, err)
			}
			if tc.mutate != nil {
				tc.mutate(obj)
			}
			body, _ := json.Marshal(obj)
			// 关键：直发原生 body，否则注入的 model key / is_reasoning / parameters 会被
			// ChatStream 的 OpenAI→Qoder 投影整段丢弃（见 chatStreamRaw 注释）。
			if i == 0 {
				t.Logf("   实际发出：%s", bodyFacts(obj))
			}

			start := time.Now()
			rc, status, respBody, err := c.chatStreamRaw(a, body, modelKey)
			ttfb := time.Since(start)
			if err != nil {
				res.note = "传输层失败：" + err.Error()
				break
			}
			res.status = status
			if rc == nil {
				// 非 2xx：上游拒绝，响应体是判读依据
				res.note = truncate(string(respBody), 200)
				break
			}

			dumpPath := ""
			if dumpDir != "" {
				dumpPath = filepath.Join(dumpDir, fmt.Sprintf("probe-%s-%d.sse", tc.id, i))
			}
			cr, cerr := newCaptureReader(rc, 8<<20, dumpPath)
			if cerr != nil {
				t.Logf("%s: 落盘失败（忽略）：%v", tc.name, cerr)
				cr, _ = newCaptureReader(rc, 8<<20, "")
			}
			// 关键修正：计时必须覆盖读完整条流，ChatStream 返回的只是响应头。
			msg, aggErr := aggregate(cr, clientModel)
			total := time.Since(start)
			_ = cr.Close()
			_ = rc.Close()
			res.total, res.ttfb = total, ttfb
			res.stats = scanStream(cr.Bytes())
			if dumpPath != "" {
				t.Logf("  [dump] %s → %s", tc.name, dumpPath)
			}
			if aggErr != nil {
				res.note = "流解析失败：" + aggErr.Error()
				break
			}
			if v := msgField(msg, "reasoning_content"); v != "" {
				sumReason += len([]rune(v))
			}
			if v := msgField(msg, "content"); v != "" {
				sumContent += len([]rune(v))
			}
			// 抓 usage 块（含 reasoning_tokens）——用于判断「模型想了但不下发」vs「根本没想」。
			if u, ok := msg["usage"].(map[string]any); ok && len(u) > 0 {
				b, _ := json.Marshal(u)
				res.usage = string(b)
			}
			sumTTFB += ttfb
			sumTotal += total
			okCount++
		}
		if okCount > 0 {
			res.reasoning = sumReason / okCount
			res.content = sumContent / okCount
			res.ttfb = sumTTFB / time.Duration(okCount)
			res.total = sumTotal / time.Duration(okCount)
		}
		results = append(results, res)
	}

	// 判读按用例 id 找，避免用 WILDWORK_PROBE_CASES 过滤后下标错位。
	byID := map[string]probeResult{}
	for _, r := range results {
		byID[r.id] = r
	}
	base := results[0].reasoning
	if on, ok := byID["2"]; ok {
		base = on.reasoning // 倍数基线取「思考已开、未注入档位」的那组
	}
	t.Logf("%-34s %-5s %-8s %-9s %-10s %-10s %s", "用例", "状态", "TTFB", "总耗时", "思考 (字)", "回答 (字)", "备注")
	for _, r := range results {
		ratio := "-"
		if base > 0 && r.reasoning > 0 {
			ratio = fmt.Sprintf("%.2fx", float64(r.reasoning)/float64(base))
		}
		note := r.stats.String()
		switch {
		case r.note != "" && r.stats.Bytes > 0:
			note = r.note + " | " + note
		case r.note != "":
			note = r.note
		}
		t.Logf("%-34s %-5d %-8s %-9s %-10s %-10s %s", r.name, r.status,
			r.ttfb.Round(time.Millisecond), r.total.Round(time.Millisecond),
			fmt.Sprintf("%d (%s)", r.reasoning, ratio), strconv.Itoa(r.content), note)
	}

	// 失败定位：全部为空时把原始流首行打出来，避免又一轮盲猜。
	for _, r := range results {
		if r.status == 200 && r.content == 0 && r.reasoning == 0 && r.note == "" {
			t.Logf("!! %s：状态 200 但内容/思考双空 —— 原始流诊断：%s", r.name, r.stats)
			for i, l := range r.stats.First {
				t.Logf("     line%d: %s", i+1, l)
			}
			break
		}
	}
	// 非标准 delta 键（如 extends）原文 —— 思考有可能藏在里面。
	for _, r := range results {
		for i, o := range r.stats.Other {
			t.Logf(">> %s 的非标准 delta chunk%d：%s", r.name, i+1, o)
		}
	}
	// 穷举取证：只要「回答有字但思考 0」或「流异常」，就把所有可能藏思考的位置打全
	// —— 顶层键 / 非 data 行 / 外层信封键 / choice 层键 / 上游 model / finish_reason /
	// [DONE] / 流尾部。只盯 delta.reasoning_content 一个名字，漏判概率不低。
	for _, r := range results {
		if r.stats.Bytes == 0 {
			continue
		}
		if r.reasoning > 0 && r.note == "" {
			continue // 已经拿到思考，不需要取证
		}
		t.Logf("-- %s：回答 %d 字、思考 %d 字 → chunk 顶层键=[%s]，非 data 行=[%s]%s",
			r.name, r.content, r.reasoning, hist(r.stats.TopKeys), hist(r.stats.OtherLines), r.stats.Detail())
	}

	// 判读按用例 id 找。
	offCase, hasOff := byID["1"]
	onCase, hasOn := byID["2"]
	if hasOff && hasOn {
		switch {
		case offCase.reasoning == 0 && onCase.reasoning > 0:
			t.Logf("==> 开关有效：is_reasoning=false 思考 0 字，=true 思考 %d 字（对照成立）。", onCase.reasoning)
		case offCase.reasoning == 0 && onCase.reasoning == 0:
			t.Log("==> 用例 1/2 都是 0：is_reasoning 开关不产生思考链。**但 1/2 都没注入 parameters.reasoning_effort**，")
			t.Log("    档位路径还没测 —— 继续跑用例 3/4（WILDWORK_PROBE_CASES=3,4）。")
		case offCase.reasoning > 0:
			t.Logf("==> 关闭态竟然也有思考（%d 字）：该模型可能默认开思考，is_reasoning 关不掉。", offCase.reasoning)
		}
	} else if hasOn && onCase.reasoning == 0 {
		t.Log("==> 用例 2（is_reasoning=true）思考 0 字：上游没吐思考链，先看上面的原始流诊断。")
	}
	t.Log("==> 看「思考 (字)」列：用例 2 明显大于 1 → 开关有效；3/4 之间有梯度 → 档位生效；7 归零 → 关闭语义成立。")
	t.Log("==> 用例 8 非 200 → 上游严格校验（客户端必须白名单）；200 → 静默忽略，不能盲信参考实现。")

	// 档位的另一种可能效果：不产生思考链，但改变生成量/耗时。把回答字数与耗时横向比一下。
	if len(results) > 1 {
		t.Log("==> 档位是否改变了生成量（思考链之外的可能效果）：")
		for _, r := range results {
			if r.status == 200 && r.note == "" {
				t.Logf("     %-40s 回答 %d 字 / %s", r.name, r.content, r.total.Round(time.Second))
			}
		}
		t.Log("     同 prompt 下单次采样噪声可达 ±40%，要下结论必须 WILDWORK_PROBE_REPEAT=3 取平均。")
	}
	t.Log("==> 若所有用例都是「回答有字、思考 0 字」，且上面穷举的位置（顶层键/非 data 行/")
	t.Log("    外层信封键/choice 层键）都没有思考痕迹 → 该模型在 api3（agent_chat_generation）下不下发思考链。")
	t.Log("    换模型复测时优先 qwen3.8-max：官方 Qoder CLI 只对 Max 系暴露 --reasoning-effort 别名")
	t.Log("    （avaritiachaos/qoder-proxy 的模型表：qwen3.8-max-effort-{low,medium,high,max}），")
	t.Log("    说明档位在这两个模型上才是官方支持的能力。")
	// 抓 usage 块（含 reasoning_tokens）——用于判断「模型想了但不下发」vs「根本没想」。
	for _, r := range results {
		if r.usage != "" {
			t.Logf("-- %s: usage=%s", r.name, r.usage)
		}
	}
}
