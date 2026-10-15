//go:build live

// live_probe_test.go QoderCN「思考档位」真实账号探针。
//
// 目的：QoderCN 的档位收口（thinking_config 解析 + parameters.reasoning_effort 投影）
// 是**照着 internal/qoder 移植**的，从未在 QoderCN 真实上游上验证过。本探针回答三件事：
//
//  1. 上游模型目录是否真的返回 thinking_config（含 effort ladder 与 is_default）？
//     —— 决定「面板能看到档位」是否有上游依据，也决定 ParseThinkingConfig 是否被喂到数据。
//  2. 上游是否接受 parameters.reasoning_effort（本渠道请求体走 qoder2api 形态，
//     与 qoder 渠道的 body 形状并不完全相同）？非法值 400 还是静默忽略？
//  3. 档位是否真的改变思考链长度（这是「档位生效」的唯一硬证据）；
//     以及 parameters.enable_thinking 是否必需（官方客户端与档位同时下发）。
//
// 本文件带 `live` build tag，**默认不参与编译**（go build ./... / go test ./... 都不碰它），
// 不改任何生产代码路径。测完可保留（下次渠道协议变动时重跑）。
//
// 运行方式（在 wild-work 仓库根目录）：
//
//	WILDWORK_AUTHDIR=./auths go test -tags live ./internal/qodercn/ -run TestLiveProbe -v -timeout 1800s
//
// 可调环境变量：
//
//	WILDWORK_AUTHDIR       账号目录（默认 ./auths），读取 qodercn-*.json
//	WILDWORK_PROBE_MODEL   客户端模型名（默认自动挑「有 ladder 的第一个模型」）
//	WILDWORK_PROBE_KEY     直接指定上游 model key，跳过映射
//	WILDWORK_PROBE_PROMPT  探测用 prompt（默认见 defaultProbePrompt）
//	WILDWORK_PROBE_CASES   只跑指定用例 id（逗号分隔，如 "2,3"；默认全跑）
//	WILDWORK_PROBE_DUMP    设为目录时把每次请求的原始 SSE 落盘
//
// # 三个必须踩对的坑（照抄 internal/qoder/live_probe_test.go 的血泪教训）
//
//  1. **必须用「需要多步推理」的题**。"1+1=?" 各档位都不产生思考链，只能验证状态码。
//  2. **ChatStream 只返回 resp.Body**，真正的生成发生在 aggregate() 读流阶段 ——
//     计时必须覆盖读流，否则量到的是 TTFB。
//  3. **档位能力必须先写进 reasoning.Caps 的 RealmQoderCN 面**，否则 reasoningSpecFor
//     的 Caps.Lookup 恒失败 → 只翻开关、不下发档位字段（保守守卫），
//     档位用例会静默退化成「无档位」，把「守卫生效」误判成「上游不支持档位」。
//     生产环境由 server.publishEffortCaps 在目录拉取时填充，探针里手动补上。
//
// 成本：目录探测 0 次对话；档位探测 = 用例数 次对话。日志先打印预估耗时。
package qodercn

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
	"wild-work/internal/reasoning"
)

// liveAuth 从 WILDWORK_AUTHDIR（默认 ./auths）取第一个 QoderCN 账号。
// 注意账号文件名必须是 qodercn-*.json（LoadQoderDir 的 qoder*.json glob 会吞掉它，
// 加载器内部已排除；两个渠道各归各的加载器）。
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
	as, err := auth.LoadQoderCNDir(abs)
	if err != nil {
		t.Fatalf("加载 %s 下的 qodercn*.json 失败：%v", abs, err)
	}
	if len(as) == 0 {
		t.Skipf("在 %s 没找到 qodercn-*.json —— 用面板登录一个 QoderCN 账号，"+
			"或把 qoder-*.json 复制一份改名为 qodercn-*.json（两渠道同域 qoder.com.cn，凭据通用）", abs)
	}
	a := as[0]
	t.Logf("账号：uid=%s nickname=%s domain=%s file=%s", a.UID, a.Nickname, a.Region(), a.FilePath)
	return a
}

// probeClient 建客户端并实测 userType（COSY identity 签名要用实测值，
// 缺了会 403；生产环境在登录/首次请求时填充）。
func probeClient(t *testing.T, a *auth.Auth) *Client {
	t.Helper()
	c := New()
	name, ut, err := c.FetchUserInfo(a)
	if err != nil {
		t.Fatalf("FetchUserInfo 失败（COSY identity 需要实测 userType）：%v", err)
	}
	t.Logf("userinfo：name=%s userType=%s", name, ut)
	return c
}

// publishCapsFromEntries 把目录解析出的档位能力写进 RealmQoderCN 面。
// 与 server.publishEffortCaps 同逻辑（同一份 toModelInfos 输出、同一套筛选），
// 仅目标面不同 —— 探针不能 import server（server 依赖本包，会成环）。
func publishCapsFromEntries(entries []ModelEntry) int {
	infos := toModelInfos(entries)
	caps := make(map[string]reasoning.Cap, len(infos))
	for _, mi := range infos {
		if len(mi.SupportedEfforts) == 0 && mi.DefaultEffort == "" && !mi.ReasoningCanDisable {
			continue
		}
		caps[mi.ID] = reasoning.Cap{
			Efforts:         mi.SupportedEfforts,
			DefaultEffort:   mi.DefaultEffort,
			SupportsDisable: mi.ReasoningCanDisable,
		}
	}
	reasoning.Caps.SetRemote(reasoning.RealmQoderCN, caps)
	return len(caps)
}

// TestLiveProbeModelCatalog 第 1 步：打印上游目录里的思考相关字段 + 解析结果。
// 零对话成本，先跑这个 —— 若上游不返回 thinking_config，档位就无上游依据，
// 面板也不会暴露档位（ListingForKind 返回空），本渠道的档位改动等于没启用。
func TestLiveProbeModelCatalog(t *testing.T) {
	a := liveAuth(t)
	c := probeClient(t, a)

	entries, err := c.fetchModels(a)
	if err != nil {
		t.Fatalf("拉模型目录失败：%v", err)
	}
	t.Logf("目录共 %d 个 enabled 模型", len(entries))

	withTC, withLadder, disableable := 0, 0, 0
	for _, e := range entries {
		client := NormalizeModelName(e.DisplayName)
		if client == "" {
			client = e.Key
		}
		if len(e.ThinkingConfig) == 0 || string(e.ThinkingConfig) == "null" {
			t.Logf("  %-18s client=%-18s is_reasoning=%-5v thinking_config=<无>", e.Key, client, e.IsReasoning)
			continue
		}
		withTC++
		// 用生产同款解析器，避免探针另写一份各自漂移。
		caps := reasoning.ParseThinkingConfig(e.ThinkingConfig)
		if len(caps.Efforts) > 0 {
			withLadder++
		}
		if caps.SupportsDisable {
			disableable++
		}
		t.Logf("  %-18s client=%-18s is_reasoning=%-5v ladder=%v default=%q 可关闭=%v",
			e.Key, client, e.IsReasoning, caps.Efforts, caps.DefaultEffort, caps.SupportsDisable)
		t.Logf("      raw=%s", truncate(string(e.ThinkingConfig), 240))
	}
	t.Logf("==> %d/%d 个模型带 thinking_config；其中 %d 个有 effort ladder，%d 个可显式关闭",
		withTC, len(entries), withLadder, disableable)
	if withLadder == 0 {
		t.Log("==> 上游没有可枚举的 ladder：档位无依据，面板不会暴露档位，" +
			"请求侧也只会翻 is_reasoning 开关（与改动前一致）。")
	} else {
		t.Log("==> ladder 就是该模型真实支持的档位；第 3 步会按它挑档位做梯度对照。")
	}

	n := publishCapsFromEntries(entries)
	t.Logf("==> 已把 %d 个模型的能力写入 RealmQoderCN 面（供后续用例的生产投影使用）", n)
}

// defaultProbePrompt 默认探测题：需要多步推理、答案短 —— content 短、思考长，档位差异最明显。
const defaultProbePrompt = `9 个外观完全相同的球中有 1 个重量异常（可能偏重也可能偏轻），其余 8 个等重。` +
	`只用一个天平称 3 次，找出这个异常球并判断它是偏重还是偏轻。` +
	`请逐步推理给出完整的称量方案，并说明为什么 3 次一定够。`

func probePrompt() string {
	if p := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_PROMPT")); p != "" {
		return p
	}
	return defaultProbePrompt
}

// msgField 从 aggregate 结果里取 message 级字段（content / reasoning_content）。
// 正文在 choices[0].message 下，不在顶层 —— 直接读顶层会恒为空，
// 把「有内容有思考」误判成 0（qoder 探针踩过：848 个 content 分片却报 0 字）。
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

// streamScan 原始流的粗粒度诊断：区分「上游没发思考」与「我们没解析出来」。
type streamScan struct {
	Bytes    int
	DataLine int
	Chunks   int
	RsnIn    int
	CtIn     int
	EnvKeys  map[string]int
	TopKeys  map[string]int
	DeltaKey map[string]int
	Done     bool
	Tail     string
}

func (s streamScan) String() string {
	return fmt.Sprintf("原始%dB data行%d chunk%d reasoning块%d content块%d 信封键=[%s] 顶层键=[%s] delta键=[%s] 见[DONE]=%v 尾=%s",
		s.Bytes, s.DataLine, s.Chunks, s.RsnIn, s.CtIn,
		hist(s.EnvKeys), hist(s.TopKeys), hist(s.DeltaKey), s.Done, s.Tail)
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

func scanRaw(raw []byte) streamScan {
	st := streamScan{
		Bytes: len(raw), EnvKeys: map[string]int{}, TopKeys: map[string]int{}, DeltaKey: map[string]int{},
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimPrefix(line, "data:")
		if strings.Contains(payload, "[DONE]") {
			st.Done = true
			continue
		}
		st.DataLine++
		var env map[string]any
		if json.Unmarshal([]byte(payload), &env) == nil {
			for k := range env {
				st.EnvKeys[k]++
			}
		}
	}
	_ = parseNestedSSE(bytes.NewReader(raw), func(chunk map[string]any) error {
		st.Chunks++
		for k := range chunk {
			st.TopKeys[k]++
		}
		choices, _ := chunk["choices"].([]any)
		for _, ci := range choices {
			ch, _ := ci.(map[string]any)
			if ch == nil {
				continue
			}
			delta, _ := ch["delta"].(map[string]any)
			for k, v := range delta {
				if s, ok := v.(string); !ok || s != "" {
					st.DeltaKey[k]++
				}
			}
			if v, ok := delta["reasoning_content"].(string); ok && v != "" {
				st.RsnIn++
			}
			if v, ok := delta["content"].(string); ok && v != "" {
				st.CtIn++
			}
		}
		return nil
	}, nil)
	tail := strings.TrimSpace(string(raw))
	if len(tail) > 160 {
		tail = tail[len(tail)-160:]
	}
	st.Tail = strings.ReplaceAll(tail, "\n", "⏎")
	return st
}

// probeChatRaw 用探针自己构造的原生 body 直发上游，不经过 ChatStream 的投影。
//
// 为什么需要它：ChatStream 只认 OpenAI 请求的固定字段（model/messages/tools/
// reasoning_effort/thinking），投影规则写死在 reasoningSpecFor 里。要验证
// 「只发 reasoning_effort、不发 enable_thinking」这类**非生产组合**，必须绕开投影。
// 生产路径本身另由 TestLiveProbeProductionPath 覆盖。
func (c *Client) probeChatRaw(a *auth.Auth, body []byte, modelKey string) (io.ReadCloser, int, []byte, error) {
	encoded := qoderEncode(body)
	url := c.Gateway + EpChat
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(encoded))
	if err != nil {
		return nil, 0, nil, err
	}
	dt := a.JWT()
	sess, err := NewCosySession(a.MachineID, a.MachineToken, a.MachineType, a.Nickname, a.UID, dt, a.RefreshToken, c.userTypeOf(a))
	if err != nil {
		return nil, 0, nil, fmt.Errorf("cosy session: %w", err)
	}
	if err := sess.ApplyHeaders(req, encoded, url, a.UID, "text/event-stream", true, modelKey); err != nil {
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

// bodyFacts 抽取要发给上游的关键字段，用于在日志里确认「投影到底有没有生效」。
func bodyFacts(obj map[string]any) string {
	mc, _ := obj["model_config"].(map[string]any)
	p, _ := obj["parameters"].(map[string]any)
	keep := func(m map[string]any, keys ...string) string {
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			if v, ok := m[k]; ok {
				parts = append(parts, fmt.Sprintf("%s=%v", k, v))
			}
		}
		return strings.Join(parts, " ")
	}
	return fmt.Sprintf("model_config{%s} parameters{%s}", keep(mc, "key", "is_reasoning"), keep(p, "max_tokens", "reasoning_effort", "enable_thinking"))
}

// probeCase 一组档位注入方式。
type probeCase struct {
	id       string
	name     string
	inject   string
	enabled  bool           // model_config.is_reasoning
	params   map[string]any // 注入 parameters 的键（nil 表示不注入档位字段）
	short    bool           // 用最简 prompt（只关心状态码的用例）
	rawOnly  bool           // 只能 raw 直发（非生产组合）
	viaProd  bool           // 走生产 ChatStream（验证投影链路）
	prodBody map[string]any // viaProd 时传给 ChatStream 的 OpenAI 请求
}

type probeResult struct {
	id, name  string
	status    int
	ttfb      time.Duration
	total     time.Duration
	reasoning int
	content   int
	usage     string
	scan      streamScan
	note      string
}

// buildCases 按模型 ladder 生成用例表。
//
// 分组（2026-09-22 实测后重排）：
//   - p* 走**生产路径**（ChatStream → reasoningSpecFor 投影），这才是面板真实发出的形态；
//   - r* 走 raw 直发，用于验证生产投影**到不了**的组合（缺 enable_thinking、非法值）。
//
// 2026-09-22 实测已确认的关键事实（决定了 p0 必须存在）：
// **只发 `model_config.is_reasoning=false`（不带任何档位字段）关不掉思考** ——
// 上游照样吐 reasoning_content，而且思考量比开思考时更大（2648 块 / 898KB vs 543 块 / 385KB），
// 180s 都没结束、content 一块都没有。所以「未表达」这个默认形态必须单独盯住。
func buildCases(caps reasoning.Cap) []probeCase {
	def := caps.DefaultEffort
	if def == "" && len(caps.Efforts) > 0 {
		def = caps.Efforts[len(caps.Efforts)/2]
	}
	maxEff, lowEff := "", ""
	if len(caps.Efforts) > 0 {
		maxEff = caps.Efforts[len(caps.Efforts)-1]
		lowEff = caps.Efforts[0]
	}
	prod := func(id, name string, effort string) probeCase {
		body := map[string]any{}
		if effort != "" {
			body["reasoning_effort"] = effort
		}
		return probeCase{
			id: id, name: name, enabled: true, viaProd: true, prodBody: body,
			inject: "生产路径：OpenAI 请求 reasoning_effort=" + effort + "（空=不带该字段）",
		}
	}
	cases := []probeCase{
		// 最常见的默认形态：客户端请求里根本没有 reasoning 字段。
		prod("p0", "生产：未表达（不带 reasoning 字段）", ""),
	}
	if def != "" {
		cases = append(cases, prod("p1", fmt.Sprintf("生产：默认档 %s", def), def))
	}
	if maxEff != "" && maxEff != def {
		cases = append(cases, prod("p2", fmt.Sprintf("生产：最高档 %s", maxEff), maxEff))
	}
	if lowEff != "" && lowEff != def {
		cases = append(cases, prod("p3", fmt.Sprintf("生产：最低档 %s", lowEff), lowEff))
	}
	if caps.SupportsDisable {
		cases = append(cases, prod("p4", "生产：显式关闭（reasoning_effort=none）", "none"))
	}
	// raw 对照：验证生产投影覆盖不到的组合。
	cases = append(cases,
		probeCase{id: "r1", name: "raw：is_reasoning=false，无任何档位字段", enabled: false,
			inject: "model_config.is_reasoning=false + parameters{max_tokens}（无档位字段）"},
		// r4 是决定修复方案的关键一组：若「is_reasoning=false + enable_thinking=false」
		// 能关掉思考，则「未表达」只需补发 enable_thinking=false（最小改动）；
		// 若关不掉，说明必须连 reasoning_effort=none 一起发（见 p4）。
		probeCase{id: "r4", name: "raw：is_reasoning=false + enable_thinking=false（无档位字段）", enabled: false,
			inject: "is_reasoning=false + parameters{enable_thinking=false}（无 reasoning_effort）",
			params: map[string]any{"enable_thinking": false}},
		probeCase{id: "r2", name: "raw：只发 reasoning_effort，不带 enable_thinking", enabled: true, rawOnly: true,
			inject: "is_reasoning=true + parameters{reasoning_effort}（缺 enable_thinking）",
			params: map[string]any{"reasoning_effort": firstNonEmpty(def, "medium")}},
		probeCase{id: "r3", name: "raw：非法档位 bogus（只看状态码）", enabled: true, rawOnly: true, short: true,
			inject: "is_reasoning=true + parameters{reasoning_effort=bogus}",
			params: map[string]any{"reasoning_effort": "bogus"}},
	)
	return cases
}

// firstNonEmpty 返回第一个非空字符串（用例表构造用）。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

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

// pickModel 选探测模型：优先 WILDWORK_PROBE_MODEL，否则挑第一个有 ladder 的模型
// （没有 ladder 的模型档位用例会退化，测不出东西）。
func pickModel(t *testing.T, c *Client, entries []ModelEntry) (clientName, modelKey string, caps reasoning.Cap) {
	t.Helper()
	want := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_MODEL"))
	for _, e := range entries {
		client := NormalizeModelName(e.DisplayName)
		if client == "" {
			client = e.Key
		}
		cc := reasoning.ParseThinkingConfig(e.ThinkingConfig)
		if want != "" && client == want {
			return client, e.Key, cc
		}
		if want == "" && len(cc.Efforts) > 0 && caps.Efforts == nil {
			clientName, modelKey, caps = client, e.Key, cc
		}
	}
	if want != "" {
		t.Fatalf("WILDWORK_PROBE_MODEL=%q 不在上游目录里（目录见上一个用例的日志）", want)
	}
	if clientName == "" {
		t.Log("警告：上游目录里没有任何模型带 effort ladder —— 档位用例会退化，先看 ModelCatalog 用例的输出")
		if len(entries) > 0 {
			e := entries[0]
			clientName = NormalizeModelName(e.DisplayName)
			if clientName == "" {
				clientName = e.Key
			}
			modelKey = e.Key
			caps = reasoning.ParseThinkingConfig(e.ThinkingConfig)
		}
	}
	return clientName, modelKey, caps
}

// TestLiveProbeProductionPath 走**生产链路**（ChatStream → reasoningSpecFor →
// buildAgentBody → ApplyHeaders → aggregate），确认档位投影真的能让上游吐思考链。
//
// 这是最贴近面板实际调用的一条路径：请求体是 server 层改写后的 OpenAI 请求。
func TestLiveProbeProductionPath(t *testing.T) {
	a := liveAuth(t)
	c := probeClient(t, a)
	entries, err := c.fetchModels(a)
	if err != nil {
		t.Fatalf("拉模型目录失败：%v", err)
	}
	publishCapsFromEntries(entries)

	clientModel, key, caps := pickModel(t, c, entries)
	t.Logf("探测模型：客户端名=%s 上游 key=%s ladder=%v default=%q 可关闭=%v",
		clientModel, key, caps.Efforts, caps.DefaultEffort, caps.SupportsDisable)

	effort := caps.DefaultEffort
	if effort == "" && len(caps.Efforts) > 0 {
		effort = caps.Efforts[len(caps.Efforts)-1]
	}
	reqBody := map[string]any{
		"model":    clientModel,
		"messages": []map[string]any{{"role": "user", "content": probePrompt()}},
	}
	if effort != "" {
		reqBody["reasoning_effort"] = effort
	}
	body, _ := json.Marshal(reqBody)
	t.Logf("请求：reasoning_effort=%q（空=未表达档位）", effort)

	start := time.Now()
	rc, status, respBody, err := c.ChatStream(a, body)
	ttfb := time.Since(start)
	if err != nil {
		t.Fatalf("ChatStream 传输失败：%v", err)
	}
	if rc == nil {
		t.Fatalf("ChatStream 被拒：status=%d body=%s", status, truncate(string(respBody), 400))
	}
	defer rc.Close()

	var buf bytes.Buffer
	cr := io.TeeReader(rc, &buf)
	msg, aggErr := aggregate(cr, clientModel)
	total := time.Since(start)
	if aggErr != nil {
		t.Fatalf("aggregate 失败：%v\n原始流：%s", aggErr, truncate(buf.String(), 600))
	}
	reasoning := msgField(msg, "reasoning_content")
	content := msgField(msg, "content")
	t.Logf("status=%d TTFB=%s 总耗时=%s 思考=%d 字 回答=%d 字",
		status, ttfb.Round(time.Millisecond), total.Round(time.Millisecond),
		len([]rune(reasoning)), len([]rune(content)))
	if u, ok := msg["usage"].(map[string]any); ok && len(u) > 0 {
		b, _ := json.Marshal(u)
		t.Logf("usage=%s", b)
	}
	t.Logf("原始流：%s", scanRaw(buf.Bytes()))
	if reasoning == "" {
		t.Errorf("生产链路没拿到 reasoning_content —— 该模型 %s 在 QoderCN 上游不吐思考链，"+
			"或请求形状未对齐（先跑 ModelCatalog 确认 is_reasoning/ladder）", clientModel)
	}
}

// TestLiveProbeEffortGradient 第 2 步：横向对比开关与档位的行为差异。
//
// 判读：
//   - 用例 2 的思考长度显著大于用例 1 → 开关有效
//   - 用例 3/5 之间有梯度          → 档位生效（本渠道的核心问题）
//   - 用例 3 与 4 有差异           → parameters.enable_thinking 是必需的（官方形态）
//   - 用例 6 非 200 → 上游严格校验档位值（客户端必须按 ladder 白名单降级）
//
// 成本：用例数 次对话。先看日志里的预估耗时。
func TestLiveProbeEffortGradient(t *testing.T) {
	a := liveAuth(t)
	c := probeClient(t, a)
	entries, err := c.fetchModels(a)
	if err != nil {
		t.Fatalf("拉模型目录失败：%v", err)
	}
	publishCapsFromEntries(entries)

	clientModel, key, caps := pickModel(t, c, entries)
	cases := filterCases(buildCases(caps))
	if len(cases) == 0 {
		t.Fatalf("WILDWORK_PROBE_CASES=%q 没匹配到任何用例", os.Getenv("WILDWORK_PROBE_CASES"))
	}
	prompt := probePrompt()
	t.Logf("探测模型：客户端名=%s 上游 key=%s ladder=%v default=%q 可关闭=%v",
		clientModel, key, caps.Efforts, caps.DefaultEffort, caps.SupportsDisable)
	t.Logf("探测 prompt（%d 字）：%s", len([]rune(prompt)), truncate(strings.ReplaceAll(prompt, "\n", " "), 80))
	t.Logf("用例 %d 组 = %d 次对话；按 30s/次估算约 %.1f 分钟",
		len(cases), len(cases), float64(len(cases))*30/60)

	dumpDir := strings.TrimSpace(os.Getenv("WILDWORK_PROBE_DUMP"))
	results := make([]probeResult, 0, len(cases))
	for _, tc := range cases {
		usePrompt := prompt
		if tc.short {
			usePrompt = "1+1=?"
		}
		t.Logf("[用例 %s] %s | 注入：%s", tc.id, tc.name, tc.inject)
		res := probeResult{id: tc.id, name: tc.name}

		var rc io.ReadCloser
		var status int
		var respBody []byte
		var err error
		start := time.Now()

		if tc.viaProd {
			// 生产路径：ChatStream 内部按 reasoningSpecFor 投影。
			reqBody := map[string]any{"model": clientModel, "messages": []map[string]any{{"role": "user", "content": usePrompt}}}
			for k, v := range tc.prodBody {
				reqBody[k] = v
			}
			body, _ := json.Marshal(reqBody)
			rc, status, respBody, err = c.ChatStream(a, body)
		} else {
			// raw 路径：手工构造原生 body，直发。
			mc := c.modelEntry(key)
			raw, berr := buildAgentBody([]map[string]any{{"role": "user", "content": usePrompt}}, mc, nil,
				reasoningSpec{Enabled: tc.enabled, Effort: ""}, 0, c.userTypeOf(a), mc.ContextWindow)
			if berr != nil {
				t.Fatalf("%s: 构造 body 失败：%v", tc.name, berr)
			}
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil {
				t.Fatalf("%s: body 解析失败：%v", tc.name, err)
			}
			if len(tc.params) > 0 {
				p, _ := obj["parameters"].(map[string]any)
				if p == nil {
					p = map[string]any{}
					obj["parameters"] = p
				}
				for k, v := range tc.params {
					p[k] = v
				}
			}
			t.Logf("   实际发出：%s", bodyFacts(obj))
			body, _ := json.Marshal(obj)
			rc, status, respBody, err = c.probeChatRaw(a, body, key)
		}
		ttfb := time.Since(start)

		if err != nil {
			res.note = "传输层失败：" + err.Error()
			results = append(results, res)
			continue
		}
		res.status = status
		if rc == nil {
			res.note = truncate(string(respBody), 220)
			results = append(results, res)
			continue
		}
		var buf bytes.Buffer
		cr := io.TeeReader(rc, &buf)
		msg, aggErr := aggregate(cr, clientModel)
		total := time.Since(start)
		rc.Close()
		res.ttfb, res.total = ttfb, total
		res.scan = scanRaw(buf.Bytes())
		if dumpDir != "" {
			p := filepath.Join(dumpDir, fmt.Sprintf("probe-%s.sse", tc.id))
			_ = os.WriteFile(p, buf.Bytes(), 0o644)
			t.Logf("  [dump] %s", p)
		}
		if aggErr != nil {
			res.note = "流解析失败：" + aggErr.Error()
			results = append(results, res)
			continue
		}
		res.reasoning = len([]rune(msgField(msg, "reasoning_content")))
		res.content = len([]rune(msgField(msg, "content")))
		if u, ok := msg["usage"].(map[string]any); ok && len(u) > 0 {
			b, _ := json.Marshal(u)
			res.usage = string(b)
		}
		results = append(results, res)
	}

	byID := map[string]probeResult{}
	for _, r := range results {
		byID[r.id] = r
	}
	// 基线取「默认档」那组：档位差异相对它来看。
	base := 0
	if d, ok := byID["p1"]; ok {
		base = d.reasoning
	}
	t.Logf("%-46s %-5s %-8s %-9s %-12s %-9s %s", "用例", "状态", "TTFB", "总耗时", "思考(字)", "回答(字)", "备注")
	for _, r := range results {
		ratio := "-"
		if base > 0 && r.reasoning > 0 {
			ratio = fmt.Sprintf("%.2fx", float64(r.reasoning)/float64(base))
		}
		note := r.scan.String()
		if r.note != "" {
			note = r.note + " | " + note
		}
		reason := fmt.Sprintf("%d (%s)", r.reasoning, ratio)
		content := fmt.Sprintf("%d", r.content)
		if r.note != "" || !r.scan.Done {
			// 截断时 reasoning/content 都没走到计数（aggregate 失败即 return），
			// 直接显示 0 会被误读成「思考 0 字 / 回答 0 字」——改报流内块数。
			reason = fmt.Sprintf("截断(流内%d块)", r.scan.RsnIn)
			content = fmt.Sprintf("截断(流内%d块)", r.scan.CtIn)
		}
		t.Logf("%-46s %-5d %-8s %-9s %-14s %-14s %s", r.name, r.status,
			r.ttfb.Round(time.Millisecond), r.total.Round(time.Millisecond), reason, content, note)
	}
	for _, r := range results {
		if r.usage != "" {
			t.Logf("-- %s: usage=%s", r.name, r.usage)
		}
	}

	// 判读
	//
	// p0（未表达）是**最常见的默认形态**：客户端请求里根本没有 reasoning 字段。
	// 2026-09-22 实测：它投影成 `is_reasoning=false` 且不带档位字段，上游照样开思考，
	// 思考量甚至比默认档更大（2648 块 / 898KB，180s 未结束、content 一块没有）。
	// 所以这一组的判据是「是否显著多于默认档 / 是否被截断」，而不是「是否归零」。
	if p0, ok := byID["p0"]; ok {
		// 注意：被截断时 res.reasoning 恒为 0（aggregate 失败即 return，没走到计数），
		// 必须看流里的 reasoning 块数才能判断「到底思考了没有」。
		truncated := p0.note != "" || !p0.scan.Done
		switch {
		case p0.status != 200:
			t.Logf("==> 未表达（p0）被拒：status=%d %s", p0.status, p0.note)
		case truncated && p0.scan.RsnIn > 0:
			t.Logf("==> ⚠️ 未表达（p0）被截断（%s），但流里已有 %d 个 reasoning 块 / %d 字节、content 0 块：",
				p0.note, p0.scan.RsnIn, p0.scan.Bytes)
			t.Log("    `is_reasoning=false` 单独下发**关不掉思考**，上游反而以更高强度思考 ——")
			t.Log("    未表达形态必须改成显式关闭（对比 p4）。")
		case p0.reasoning > 0 && base > 0 && p0.reasoning > base:
			t.Logf("==> ⚠️ 未表达（p0）思考 %d 字 > 默认档 %d 字：关思考的表达无效。", p0.reasoning, base)
		case p0.reasoning == 0 && !truncated:
			t.Log("==> 未表达（p0）思考 0 字且流正常收尾：该模型默认不思考（与 p1 对比确认）。")
		default:
			t.Logf("==> 未表达（p0）思考 %d 字，默认档 %d 字。", p0.reasoning, base)
		}
	}
	if p1, ok := byID["p1"]; ok && p1.reasoning > 0 {
		t.Logf("==> 生产投影（reasoning_effort + enable_thinking 同源下发）拿到思考 %d 字：上游接受这两个字段。", p1.reasoning)
	}
	// grad 把一组的思考量渲染成可读文本：被截断时 reasoning 恒为 0，
	// 必须改报流内块数，否则「思考爆炸」会被打印成「思考 0 字」。
	grad := func(r probeResult) string {
		if r.note != "" || !r.scan.Done {
			return fmt.Sprintf("截断（流内 %d 个 reasoning 块 / %d 字节）", r.scan.RsnIn, r.scan.Bytes)
		}
		return fmt.Sprintf("%d 字", r.reasoning)
	}
	if p1, ok1 := byID["p1"]; ok1 && p1.status == 200 {
		if p2, ok2 := byID["p2"]; ok2 && p2.status == 200 {
			t.Logf("==> 档位梯度：最高 %s vs 默认 %s", grad(p2), grad(p1))
		}
		if p3, ok3 := byID["p3"]; ok3 && p3.status == 200 {
			t.Logf("==> 档位梯度：最低 %s vs 默认 %s", grad(p3), grad(p1))
		}
	}
	if p4, ok := byID["p4"]; ok {
		if p4.reasoning == 0 {
			t.Log("==> ✅ 显式关闭（reasoning_effort=none + enable_thinking=false）把思考真的关掉了（0 字）：")
			t.Log("    这才是有效的关闭表达 —— 与 p0 的 is_reasoning=false 形成对照。")
		} else {
			t.Logf("==> 显式关闭（p4）仍有思考 %d 字：该模型关不掉。", p4.reasoning)
		}
	}
	if r1, ok := byID["r1"]; ok {
		switch {
		case r1.reasoning > 0:
			t.Logf("==> raw 对照：只发 is_reasoning=false 时思考 %d 字（关不掉，与 p0 一致）。", r1.reasoning)
		case r1.scan.RsnIn > 0:
			t.Logf("==> raw 对照：只发 is_reasoning=false 时流被截断，但已有 %d 个 reasoning 块 / %d 字节（关不掉）。",
				r1.scan.RsnIn, r1.scan.Bytes)
		}
	}
	if r4, ok := byID["r4"]; ok {
		switch {
		case r4.reasoning == 0 && r4.note == "":
			t.Log("==> ✅ is_reasoning=false + enable_thinking=false 能关掉思考：")
			t.Log("    未表达形态只需补发 enable_thinking=false（最小改动，见 body.go 的 parameters 构造）。")
		case r4.note != "" || !r4.scan.Done:
			t.Logf("==> is_reasoning=false + enable_thinking=false 仍被截断（%s）：缺 reasoning_effort 不足以关闭。", r4.note)
		default:
			t.Logf("==> is_reasoning=false + enable_thinking=false 仍有思考 %d 字。", r4.reasoning)
		}
	}
	if r3, ok := byID["r3"]; ok {
		if r3.status == 200 {
			t.Log("==> 非法档位被静默忽略（200）：客户端不能盲信，必须按 ladder 降级。")
		} else {
			t.Logf("==> 非法档位被拒（%d）：上游严格校验，客户端必须白名单。", r3.status)
		}
	}
	t.Log("==> 口径：p0 未表达 / p1 默认档 / p2 最高 / p3 最低 / p4 显式关闭；r* 为 raw 对照。")
	t.Log("    同 prompt 单次采样噪声可达 ±40%，要下梯度结论需重复多次取平均。")
}

// TestLiveProbeProjectionDryRun 零成本：不联网，验证生产投影在各种控制输入下
// 落进请求体的字段是否符合预期（含「能力未知时只翻开关」的保守守卫）。
func TestLiveProbeProjectionDryRun(t *testing.T) {
	// 模拟上游目录声明的能力（真实值见 ModelCatalog 用例）。
	reasoning.Caps.SetRemote(reasoning.RealmQoderCN, map[string]reasoning.Cap{
		"qwen3.8-flash": {Efforts: []string{"low", "medium", "xhigh"}, DefaultEffort: "medium", SupportsDisable: true},
		"glm-5.3":       {Efforts: []string{"low", "high", "max"}, DefaultEffort: "max"},
	})
	show := func(label, clientModel, effort string, thinking *thinkingParam) {
		spec := reasoningSpecFor(clientModel, effort, thinking)
		t.Logf("%-34s → is_reasoning=%-5v effort=%-8q", label, spec.Enabled, spec.Effort)
	}
	show("无 reasoning 字段", "qwen3.8-flash", "", nil)
	show("effort=high（降级到 medium）", "qwen3.8-flash", "high", nil)
	show("effort=xhigh", "qwen3.8-flash", "xhigh", nil)
	show("effort=none（可关模型）", "qwen3.8-flash", "none", nil)
	show("effort=none（不可关模型→最低档）", "glm-5.3", "none", nil)
	show("thinking.type=enabled（用默认档）", "qwen3.8-flash", "", &thinkingParam{Type: "enabled"})
	show("未知模型 effort=high（守卫：只翻开关）", "no-such-model", "high", nil)

	// 再确认投影真的落进 body（三处同源）。
	spec := reasoningSpecFor("qwen3.8-flash", "xhigh", nil)
	raw, err := buildAgentBody(
		[]map[string]any{{"role": "user", "content": "hi"}},
		&ModelEntry{Key: "qfmodel", DisplayName: "Qwen3.8-Flash"}, nil, spec, 0, "personal_standard", 0)
	if err != nil {
		t.Fatalf("buildAgentBody: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	t.Logf("投影落体：%s", bodyFacts(obj))
	mc, _ := obj["model_config"].(map[string]any)
	p, _ := obj["parameters"].(map[string]any)
	if mc["is_reasoning"] != true || p["enable_thinking"] != true {
		t.Errorf("is_reasoning 与 enable_thinking 必须同源为 true：%s", bodyFacts(obj))
	}
	if p["reasoning_effort"] != "xhigh" {
		t.Errorf("reasoning_effort 未按 ladder 落进 parameters：%v", p["reasoning_effort"])
	}
}

// TestLiveProbeCaseTableDryRun 零成本：打印用例表，确认档位选择与注入字段。
func TestLiveProbeCaseTableDryRun(t *testing.T) {
	for _, c := range []struct {
		name string
		cap  reasoning.Cap
	}{
		{"qwen3.8 系（low/medium/xhigh，可关）", reasoning.Cap{Efforts: []string{"low", "medium", "xhigh"}, DefaultEffort: "medium", SupportsDisable: true}},
		{"deepseek 系（high/max）", reasoning.Cap{Efforts: []string{"high", "max"}, DefaultEffort: "max"}},
		{"无 ladder（只有开关）", reasoning.Cap{}},
	} {
		t.Logf("== %s", c.name)
		for _, tc := range buildCases(c.cap) {
			t.Logf("   [%s] %-46s 注入：%s", tc.id, tc.name, tc.inject)
		}
	}
}
