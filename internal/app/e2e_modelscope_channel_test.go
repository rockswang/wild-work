// e2e_modelscope_channel_test.go spec user stories 1–8（对话渠道 / 面板 / 目录 / 路由）。
//
// 每个 Test 对应用户故事的一句话诉求（见 spec「User Stories」），断言只落在
// **用户能看到的东西**上：HTTP 响应、/api/state、/api/stats、/v1/models，
// 以及「上游到底收到了什么」（假上游的记录）——后者是「网关真的按承诺转发」的唯一证据。

package app

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// chatReq 构造一次对话请求体。
func chatReq(model string, stream bool) map[string]any {
	return map[string]any{
		"model":    model,
		"stream":   stream,
		"messages": []any{map[string]any{"role": "user", "content": "介绍下你自己"}},
	}
}

// TestE2E_US1_TokenPutsChannelOnline 用户故事 1：
// 「一枚魔搭 ms- token 就能把 ModelScope 接进来对话」。
//
// 起点是**面板粘贴成功后落盘的凭据文件**（形态与 persistModelScopeAccount 逐字一致）；
// 面板粘贴入口本身的本地拒收分支见 TestE2E_US1_PasteRejectsOffline。
func TestE2E_US1_TokenPutsChannelOnline(t *testing.T) {
	cn := newMSFake(t, tinyPNG(t))
	env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-token-aaa1"}})

	t.Run("账号进池且面板显示不适用", func(t *testing.T) {
		accts := env.accounts(providerGroupModelScope)
		if len(accts) != 1 {
			t.Fatalf("面板账号数 = %d，want 1：%v", len(accts), accts)
		}
		a := accts[0]
		if uid := asString(a["uid"]); !strings.HasPrefix(uid, "mst-") {
			t.Errorf("uid = %q，want mst- 前缀（两区 uid 必须可区分，R159）", uid)
		}
		if !strings.HasPrefix(asString(a["nickname"]), "ModelScope ") {
			t.Errorf("默认昵称 = %q，want「ModelScope + token 尾 4 位」", asString(a["nickname"]))
		}
	})

	t.Run("目录出现该渠道模型", func(t *testing.T) {
		ids := env.modelsIDs()
		mustContain(t, ids, "modelscope/Qwen/Qwen3.5-122B-A10B")
	})

	t.Run("非流式对话出字", func(t *testing.T) {
		resp, raw := env.post("/v1/chat/completions", chatReq("modelscope/Qwen/Qwen3.5-122B-A10B", false))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d，want 200：%s", resp.StatusCode, truncateForLog(raw))
		}
		m := jsonMap(t, raw)
		choices, _ := m["choices"].([]any)
		if len(choices) != 1 {
			t.Fatalf("choices 长度 = %d，want 1：%s", len(choices), truncateForLog(raw))
		}
		first, _ := choices[0].(map[string]any)
		if got := asString(dig(first, "message", "content")); got != "你好，我是魔搭。" {
			t.Errorf("正文 = %q，want 上游原文", got)
		}
		if got := asString(m["model"]); got != "modelscope/Qwen/Qwen3.5-122B-A10B" {
			t.Errorf("响应 model = %q，want 客户端请求的全串（上游回的是裸名）", got)
		}
	})

	t.Run("上游收到的是该账号的凭据与剥前缀后的模型名", func(t *testing.T) {
		calls := cn.reqsByPath("/v1/chat/completions")
		if len(calls) != 1 {
			t.Fatalf("上游对话请求数 = %d，want 1", len(calls))
		}
		if calls[0].Token != "ms-e2e-token-aaa1" {
			t.Errorf("上游收到的凭据不是该账号的 token（零 token 纪律：此处只比对，不打印）")
		}
		var body map[string]any
		if err := json.Unmarshal(calls[0].Body, &body); err != nil {
			t.Fatalf("上游收到的不是 JSON 请求体: %v", err)
		}
		if got := asString(body["model"]); got != "Qwen/Qwen3.5-122B-A10B" {
			t.Errorf("上游 model = %q，want 剥掉渠道前缀的上游名（R52）", got)
		}
		if got := asString(body["messages"].([]any)[0].(map[string]any)["content"]); got != "介绍下你自己" {
			t.Errorf("用户正文被改写：%q", got)
		}
	})

	t.Run("目录请求也带该账号凭据", func(t *testing.T) {
		calls := cn.reqsByPath("/v1/models")
		if len(calls) == 0 {
			t.Fatal("上游从未收到目录请求")
		}
		if calls[0].Token != "ms-e2e-token-aaa1" {
			t.Error("目录请求未携带该账号凭据")
		}
	})
}

// TestE2E_US1_PasteRejectsOffline 用户故事 1 + 4 的「粘贴入口」侧：
// 形状不对的 token 必须在**上游请求之前**被拒，并给出可行动文案。
//
// ⚠️ 这是本层唯一能覆盖粘贴入口的部分：合法形状的 token 会真的去打正式上游
// （ValidateToken 没有注入点，见 e2e-findings.md）。因此这里只喂「本地就会拒」的输入。
func TestE2E_US1_PasteRejectsOffline(t *testing.T) {
	cn := newMSFake(t, tinyPNG(t))
	ai := newMSFake(t, tinyPNG(t))
	env := newE2EEnv(t, e2eOptions{CN: cn, AI: ai})

	cases := []struct {
		name    string
		path    string
		token   string
		wantMsg string
	}{
		{"空 token", "/api/login/modelscope_token", "", "不能为空"},
		{"不是魔搭 token", "/api/login/modelscope_token", "sk-openai-looking-key", "ms-"},
		{"含空白字符", "/api/login/modelscope_token", "ms-aaa bbb", "空白"},
		{"国际站端点同样拒收", "/api/login/modelscopeai_token", "sk-openai-looking-key", "ms-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp, raw := env.post(c.path, map[string]any{"token": c.token})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d，want 400：%s", resp.StatusCode, truncateForLog(raw))
			}
			msg := asString(dig(jsonMap(t, raw), "error"))
			if !strings.Contains(msg, c.wantMsg) {
				t.Errorf("错误文案 %q 未包含 %q（用户要据此自查）", msg, c.wantMsg)
			}
			if strings.Contains(string(raw), c.token) && c.token != "" {
				t.Errorf("响应回显了 token 原文（零 token 落盘纪律）")
			}
		})
	}

	// 本地拒收 = 一次上游请求都不该发生（证明拒收发生在网络之前）。
	if n := cn.total() + ai.total(); n != 0 {
		t.Fatalf("本地拒收却打了上游 %d 次", n)
	}
	if got := len(env.accounts("")); got != 0 {
		t.Fatalf("拒收后不应留下任何账号，实际 %d 个", got)
	}
}

// TestE2E_US2_TwoTokensOnePoolRotatesWithinRequest 用户故事 2：
// 「一枚 token = 一个账号、多枚 token 组成池，按粘性路由与冷却策略轮换」。
//
// 断言的是 R29 的核心承诺：**账号级错误不留给客户端重试**——同一次请求内换号成功。
func TestE2E_US2_TwoTokensOnePoolRotatesWithinRequest(t *testing.T) {
	cn := newMSFake(t, tinyPNG(t))
	const tokA, tokB = "ms-e2e-pool-0001", "ms-e2e-pool-0002"
	cn.setChatStatus(tokA, http.StatusTooManyRequests, `{"error":{"message":"rate limited, retry later"}}`)
	env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{tokA, tokB}})

	if got := len(env.accounts(providerGroupModelScope)); got != 2 {
		t.Fatalf("池内账号数 = %d，want 2（一枚 token = 一个账号）", got)
	}

	resp, raw := env.post("/v1/chat/completions", chatReq("modelscope/Qwen/Qwen3.5-122B-A10B", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d，want 200（同请求内换号后应成功）：%s", resp.StatusCode, truncateForLog(raw))
	}

	calls := cn.reqsByPath("/v1/chat/completions")
	if len(calls) != 2 {
		t.Fatalf("上游对话请求数 = %d，want 2（首号 429 → 换号成功，且不重复打同一个号）", len(calls))
	}
	seen := map[string]bool{}
	for _, c := range calls {
		seen[c.Token] = true
	}
	if !seen[tokA] || !seen[tokB] {
		t.Fatalf("两个账号都该被尝试过；实际 tokens=%v（脱敏：只看个数=%d）", len(seen), len(seen))
	}

	// 429 的号必须落在**模型级冷却**（不是整号冷却）：同号其他模型照常可用。
	accts := env.accounts(providerGroupModelScope)
	var cooled map[string]any
	for _, a := range accts {
		if mc, ok := a["model_cooling"].(map[string]any); ok && len(mc) > 0 {
			cooled = a
			if _, ok := mc["Qwen/Qwen3.5-122B-A10B"]; !ok {
				t.Errorf("模型级冷却的 key = %v，want 上游模型名 Qwen/Qwen3.5-122B-A10B", mc)
			}
		}
	}
	if cooled == nil {
		t.Error("首号 429 后应留下模型级冷却记录（面板可见）")
	} else if b, _ := cooled["cooling"].(bool); b {
		t.Error("429 不得把整号打进冷却（否则该号其他模型一起不可用）")
	}
}

// TestE2E_US3_RegionsAreSeparatePools 用户故事 3：
// 「国内/国际两个区域分别用两个渠道，区域与 token 不会配错」。
func TestE2E_US3_RegionsAreSeparatePools(t *testing.T) {
	const cnTok, aiTok = "ms-e2e-cn-0001", "ms-e2e-ai-0001"

	t.Run("同一枚 token 在两区各自成账号且各走各的基址", func(t *testing.T) {
		cn := newMSFake(t, tinyPNG(t))
		ai := newMSFake(t, tinyPNG(t))
		const shared = "ms-e2e-shared-9999"
		env := newE2EEnv(t, e2eOptions{
			CN: cn, AI: ai,
			CNTokens: []string{shared}, AITokens: []string{shared},
		})

		cnAccts := env.accounts(providerGroupModelScope)
		aiAccts := env.accounts(providerGroupModelScopeAI)
		if len(cnAccts) != 1 || len(aiAccts) != 1 {
			t.Fatalf("两区账号数 = %d/%d，want 1/1（独立池）", len(cnAccts), len(aiAccts))
		}
		cnUID, aiUID := asString(cnAccts[0]["uid"]), asString(aiAccts[0]["uid"])
		if cnUID == aiUID {
			t.Fatalf("两区 uid 相同（%q）：面板按 uid 定位账号会把操作串到另一区（R141 教训）", cnUID)
		}
		if !strings.HasPrefix(cnUID, "mst-") || !strings.HasPrefix(aiUID, "msai-") {
			t.Fatalf("uid 前缀 = %q / %q，want mst- / msai-", cnUID, aiUID)
		}

		// 国内站模型打国内站基址；国际站模型打国际站基址（绝不互相兜底）。
		if resp, raw := env.post("/v1/chat/completions", chatReq("modelscope/Qwen/Qwen3.5-122B-A10B", false)); resp.StatusCode != http.StatusOK {
			t.Fatalf("国内站对话 = %d：%s", resp.StatusCode, truncateForLog(raw))
		}
		if resp, raw := env.post("/v1/chat/completions", chatReq("modelscopeai/Qwen/Qwen3.5-122B-A10B", false)); resp.StatusCode != http.StatusOK {
			t.Fatalf("国际站对话 = %d：%s", resp.StatusCode, truncateForLog(raw))
		}
		if n := len(cn.reqsByPath("/v1/chat/completions")); n != 1 {
			t.Errorf("国内站上游对话请求数 = %d，want 1", n)
		}
		if n := len(ai.reqsByPath("/v1/chat/completions")); n != 1 {
			t.Errorf("国际站上游对话请求数 = %d，want 1", n)
		}
	})

	t.Run("区域写错时如实报 401 且不回落到另一区", func(t *testing.T) {
		cn := newMSFake(t, tinyPNG(t))
		ai := newMSFake(t, tinyPNG(t))
		// 国际站只认国际站 token：国内站 token 打过去必然 401。
		ai.acceptOnly("ms-e2e-ai-real-0001")
		env := newE2EEnv(t, e2eOptions{
			CN: cn, AI: ai,
			CNTokens: []string{cnTok}, AITokens: []string{aiTok},
		})

		resp, raw := env.post("/v1/chat/completions", chatReq("modelscopeai/Qwen/Qwen3.5-122B-A10B", false))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d，want 401（token 与区域不匹配必须如实透出）：%s",
				resp.StatusCode, truncateForLog(raw))
		}
		if n := len(cn.reqsByPath("/v1/chat/completions")); n != 0 {
			t.Fatalf("国际站失败后回落打了国内站 %d 次（两区互不兜底）", n)
		}
		// 登录态失效走可自愈标记（R76）：面板徽标 + 重贴入口，而不是静默 Disable。
		accts := env.accounts(providerGroupModelScopeAI)
		if len(accts) != 1 {
			t.Fatalf("国际站账号数 = %d，want 1", len(accts))
		}
		if dead, _ := accts[0]["session_dead"].(bool); !dead {
			t.Errorf("凭据被上游拒绝后应标 session_dead（面板显示「需重新登录」），实际 %v", accts[0])
		}
		if disabled, _ := accts[0]["disabled"].(bool); disabled {
			t.Error("session_dead 不得退化成 disabled（不可自愈，只能人工恢复）")
		}
	})
}

// TestE2E_US5_CreditsNotApplicable 用户故事 5：
// 「积分列显示『不适用』而不是 0」——两个 kind 都必须如此，且不得编造计量单位。
func TestE2E_US5_CreditsNotApplicable(t *testing.T) {
	cn := newMSFake(t, tinyPNG(t))
	ai := newMSFake(t, tinyPNG(t))
	env := newE2EEnv(t, e2eOptions{
		CN: cn, AI: ai,
		CNTokens: []string{"ms-e2e-na-0001"}, AITokens: []string{"ms-e2e-na-0002"},
	})

	for _, group := range []string{providerGroupModelScope, providerGroupModelScopeAI} {
		accts := env.accounts(group)
		if len(accts) != 1 {
			t.Fatalf("%s 账号数 = %d，want 1", group, len(accts))
		}
		a := accts[0]
		if na, _ := a["credits_na"].(bool); !na {
			t.Errorf("%s 的 credits_na = false（应为『不适用』）", group)
		}
		if got := asFloat(a["credits"]); got != 0 {
			t.Errorf("%s 的 credits = %v，want 0（不编造数字）", group, got)
		}
		// 「不适用」不等于「用 Token 计数」：credits_unit 不得被写成 token/次数。
		if unit := asString(a["credits_unit"]); unit != "" {
			t.Errorf("%s 的 credits_unit = %q，want 空（本渠道没有额度单位）", group, unit)
		}
	}

	// 平台表整体：不适用渠道不该被算进「积分合计」。
	if snap := env.statsSnapshot(); asFloat(snap["total_credits"]) != 0 {
		t.Errorf("total_credits = %v，want 0（无余额端点渠道不参与积分合计）", snap["total_credits"])
	}
}

// TestE2E_US6_StatsCountTokensAndRequests 用户故事 6：
// 「魔搭的 token 与请求数照常进平台概览表」——R130 的教训是漏白名单会静默全丢。
func TestE2E_US6_StatsCountTokensAndRequests(t *testing.T) {
	cn := newMSFake(t, tinyPNG(t))
	env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-stats-0001"}})

	const model = "modelscope/Qwen/Qwen3.5-122B-A10B"
	if resp, raw := env.post("/v1/chat/completions", chatReq(model, false)); resp.StatusCode != http.StatusOK {
		t.Fatalf("对话失败 = %d：%s", resp.StatusCode, truncateForLog(raw))
	}

	// 上游 usage = 11 + 7 = 18 token。
	eventually(t, "运行统计出现 modelscope 平台行", func() bool {
		row := env.platformRow("modelscope")
		return row != nil && asFloat(row["tokens"]) >= 18
	})
	row := env.platformRow("modelscope")
	if got := asFloat(row["tokens"]); got != 18 {
		t.Errorf("平台行 tokens = %v，want 18（上游 usage 原样记账）", got)
	}
	if got := asFloat(row["accounts"]); got != 1 {
		t.Errorf("平台行 accounts = %v，want 1", got)
	}

	// 流水（ledger）也要归属到该渠道：R17 双流水口径下 token 走 ledger，
	// 漏了它面板「用量」tab 与平台表会对不上。
	_, usageRaw := env.get("/api/usage?days=1")
	usage := jsonMap(t, usageRaw)
	tokStats, _ := usage["token"].(map[string]any)
	if got := asFloat(tokStats["total"]); got != 18 {
		t.Errorf("ledger token.total = %v，want 18", got)
	}
	byDay, _ := tokStats["by_day"].([]any)
	attributed := false
	for _, it := range byDay {
		d, _ := it.(map[string]any)
		byCh, _ := d["by_channel"].(map[string]any)
		if asFloat(byCh["modelscope"]) == 18 {
			attributed = true
		}
	}
	if !attributed {
		t.Errorf("ledger 的 token 未归属到渠道 modelscope：%v", byDay)
	}

	// 模型榜按**规范对外 ID** 归行（带渠道前缀），不得裂成裸名一行。
	snap := env.statsSnapshot()
	if got := asFloat(snap["reqs"]); got < 1 {
		t.Errorf("全池请求数 = %v，want ≥1", got)
	}
	found := false
	modelsList, _ := snap["models"].([]any)
	for _, it := range modelsList {
		m, _ := it.(map[string]any)
		if asString(m["model"]) == model {
			found = true
			if got := asFloat(m["tokens"]); got != 18 {
				t.Errorf("模型行 tokens = %v，want 18", got)
			}
		}
	}
	if !found {
		t.Errorf("模型榜缺 %q 行：%v", model, modelsList)
	}
}

// TestE2E_US7_ModelsUnion 用户故事 7：
// 「/v1/models 里能看到魔搭全部可用模型（静态 ∪ 上游目录）」，且远程失败要降级不报错。
func TestE2E_US7_ModelsUnion(t *testing.T) {
	cn := newMSFake(t, tinyPNG(t))
	env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-catalog-0001"}})

	ids := env.modelsIDs()
	// 静态表（远程目录里没有的也要在：Qwen-Image-Edit 只存在于静态表）
	mustContain(t, ids, "modelscope/Qwen/Qwen-Image-Edit")
	// 远程目录独有（证明并集真的并了）
	mustContain(t, ids, "modelscope/Qwen/Qwen2.5-7B-Instruct")
	// 两边都有：必须只出现一次（不重复条目）
	count := 0
	for _, id := range ids {
		if id == "modelscope/Qwen/Qwen3.5-122B-A10B" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("modelscope/Qwen/Qwen3.5-122B-A10B 出现 %d 次，want 1（静态与远程同 ID 要合并）", count)
	}
	// 生图模型也在目录里（客户端要能复制正确 ID 去画图）
	mustContain(t, ids, "modelscope/Qwen/Qwen-Image")

	// 能力字段只透传上游声明：fake 给 Qwen3.5 声明了 context_length 与 supports_tools。
	resp, raw := env.get("/v1/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/models = %d", resp.StatusCode)
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatalf("目录响应不是合法 JSON: %v", err)
	}
	for _, entry := range list.Data {
		if asString(entry["id"]) != "modelscope/Qwen/Qwen3.5-122B-A10B" {
			continue
		}
		if got := asFloat(entry["context_length"]); got != 131072 {
			t.Errorf("context_length = %v，want 131072（上游声明要透传）", got)
		}
		if v, _ := entry["supports_tools"].(bool); !v {
			t.Errorf("supports_tools = %v，want true（上游声明要透传）", entry["supports_tools"])
		}
	}
}

// TestE2E_US7_CatalogFailureFallsBackToStatic 目录端点挂掉时用户仍能拿到静态目录
// （不报错、不给空列表——R133「一个坏号/一次抖动不得让整渠道模型面归零」）。
func TestE2E_US7_CatalogFailureFallsBackToStatic(t *testing.T) {
	cn := newMSFake(t, tinyPNG(t))
	cn.mu.Lock()
	cn.modelsStatus = http.StatusInternalServerError
	cn.mu.Unlock()
	env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-catalog-0002"}})

	ids := env.modelsIDs()
	mustContain(t, ids, "modelscope/Qwen/Qwen3.5-122B-A10B")
	mustContain(t, ids, "modelscope/Qwen/Qwen-Image-Edit")
	mustNotContain(t, ids, "modelscope/Qwen/Qwen2.5-7B-Instruct") // 远程独有条目自然缺席
}

// TestE2E_US8_SlashModelIDsRouteAndRewrite 用户故事 8：
// 「模型 ID 带斜杠时对外 ID 是 modelscope/Qwen/... 且能正常路由到上游」。
func TestE2E_US8_SlashModelIDsRouteAndRewrite(t *testing.T) {
	cn := newMSFake(t, tinyPNG(t))
	env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-slash-0001"}})

	t.Run("非流式：上行剥前缀、下行还原全串", func(t *testing.T) {
		const model = "modelscope/Qwen/Qwen3.5-122B-A10B"
		resp, raw := env.post("/v1/chat/completions", chatReq(model, false))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d：%s", resp.StatusCode, truncateForLog(raw))
		}
		if got := asString(jsonMap(t, raw)["model"]); got != model {
			t.Errorf("响应 model = %q，want %q", got, model)
		}
		calls := cn.reqsByPath("/v1/chat/completions")
		var body map[string]any
		_ = json.Unmarshal(calls[len(calls)-1].Body, &body)
		if got := asString(body["model"]); got != "Qwen/Qwen3.5-122B-A10B" {
			t.Errorf("上游 model = %q，want 斜杠保留、仅剥渠道前缀", got)
		}
	})

	t.Run("流式：逐帧正文与 model 回填", func(t *testing.T) {
		const model = "modelscope/Qwen/Qwen2.5-7B-Instruct"
		resp, raw := env.post("/v1/chat/completions", chatReq(model, true))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d：%s", resp.StatusCode, truncateForLog(raw))
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
			t.Fatalf("Content-Type = %q，want SSE", ct)
		}
		text, models := sseDeltas(raw)
		if text != "你好，我是魔搭。" {
			t.Errorf("流式正文 = %q，want 上游原文", text)
		}
		if !models[model] {
			t.Errorf("流内 model 未被回填为客户端全串；见到 %v", models)
		}
	})

	t.Run("生图模型不许走对话", func(t *testing.T) {
		before := len(cn.reqsByPath("/v1/chat/completions"))
		resp, raw := env.post("/v1/chat/completions", chatReq("modelscope/Qwen/Qwen-Image", false))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status = %d，want 400（生图模型应本地拒绝并指路）：%s",
				resp.StatusCode, truncateForLog(raw))
		}
		errObj, _ := jsonMap(t, raw)["error"].(map[string]any)
		if code := asString(errObj["code"]); code != "image_model_not_chat" {
			t.Errorf("error.code = %q，want image_model_not_chat", code)
		}
		if msg := asString(errObj["message"]); !strings.Contains(msg, "/v1/images/generations") {
			t.Errorf("错误文案未指路到生图端点：%q", msg)
		}
		if after := len(cn.reqsByPath("/v1/chat/completions")); after != before {
			t.Errorf("本地拒绝后仍打了上游 %d 次（应零出站）", after-before)
		}
	})
}

// TestE2E_US8_StickyRoutingKeepsOneAccount 用户故事 2/8 的交集：粘性路由
// （同一渠道连续请求复用同一账号，避免把一段对话拆到多个账号）。
func TestE2E_US8_StickyRoutingKeepsOneAccount(t *testing.T) {
	cn := newMSFake(t, tinyPNG(t))
	env := newE2EEnv(t, e2eOptions{CN: cn, CNTokens: []string{"ms-e2e-sticky-01", "ms-e2e-sticky-02"}, StickyMax: 50})

	for i := 0; i < 3; i++ {
		if resp, raw := env.post("/v1/chat/completions", chatReq("modelscope/Qwen/Qwen3.5-122B-A10B", false)); resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 次对话失败 = %d：%s", i+1, resp.StatusCode, truncateForLog(raw))
		}
	}
	calls := cn.reqsByPath("/v1/chat/completions")
	if len(calls) != 3 {
		t.Fatalf("上游请求数 = %d，want 3", len(calls))
	}
	for i := 1; i < len(calls); i++ {
		if calls[i].Token != calls[0].Token {
			t.Fatalf("第 %d 次请求换了账号（粘性路由失效）：同一段对话被拆到多个账号", i+1)
		}
	}
	// 粘性只在内存（重启即重建），不需要额外清理；这里只确认上游被打了 3 次。
}

// providerGroupModelScope / providerGroupModelScopeAI 是 /api/state 的 group 值
// （= provider.Kind 字符串，R159）。集中一处避免各用例写裸串漂移。
const (
	providerGroupModelScope   = "modelscope"
	providerGroupModelScopeAI = "modelscopeai"
)
