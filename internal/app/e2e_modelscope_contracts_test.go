// e2e_modelscope_contracts_test.go spec user stories 14–15（维护者视角）。
//
// 这两条不是「跑一次对话」，而是「同一份事实被写在 6 个地方」的一致性：
// Go 的 DisplayNames / ChannelDisplayOrder、JS 的 CH_LABEL / CH_CLASS、HTML 的按钮、
// CSS 的 --ch-*、配色规范文档、决议项与渠道备忘。
// 任何一处漏改都**不会报错**——只会让面板少一个按钮、图上两个渠道同色、
// 或者后来者照着旧文档把硬编码加回来。故这里逐项对齐，让漏改立刻变红。
//
// 注：scripts/ui-static-check.mjs 有更细的同源断言（它解析 JS/AST）；本文件是
// **不依赖 Node** 的那一层，保证 `go test ./...` 一套命令就能兜住。

package app

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"wild-work/internal/provider"
)

// repoFile 读仓库内文件（测试的工作目录是包目录，故先向上找到 go.mod）。
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			raw, err := os.ReadFile(filepath.Join(dir, rel))
			if err != nil {
				t.Fatalf("读 %s 失败: %v", rel, err)
			}
			return string(raw)
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("找不到仓库根（go.mod）")
	return ""
}

// TestE2E_US14_SingleSourceOfTruth 用户故事 14：
// 「渠道标识、显示名、配色、按钮顺序都有单一真源与静态断言」。
func TestE2E_US14_SingleSourceOfTruth(t *testing.T) {
	t.Run("Go 侧真源", func(t *testing.T) {
		if got := provider.DisplayName(provider.ModelScope); got != "ModelScope" {
			t.Errorf("DisplayName(modelscope) = %q，want ModelScope", got)
		}
		if got := provider.DisplayName(provider.ModelScopeAI); got != "ModelScopeAI" {
			t.Errorf("DisplayName(modelscopeai) = %q，want ModelScopeAI", got)
		}
		builtin := provider.BuiltinKinds()
		for _, k := range []provider.Kind{provider.ModelScope, provider.ModelScopeAI} {
			if !builtin[k.String()] {
				t.Errorf("%s 不在 BuiltinKinds（自定义渠道可与内建同名 ⇒ 路由分裂）", k)
			}
		}
		// 展示序由显示名字节序派生（R142）：两区必须相邻且顺序固定。
		order := provider.ChannelDisplayOrder
		idx := map[provider.Kind]int{}
		for i, k := range order {
			idx[k] = i
		}
		cn, ok1 := idx[provider.ModelScope]
		ai, ok2 := idx[provider.ModelScopeAI]
		if !ok1 || !ok2 {
			t.Fatalf("ChannelDisplayOrder 缺 modelscope/modelscopeai：%v", order)
		}
		if ai != cn+1 {
			t.Errorf("展示序 = %d/%d，want 相邻且 ModelScope 在前（MiniMax < ModelScope < ModelScopeAI < MonkeyCode）", cn, ai)
		}
		// 规则本身：整张表必须按显示名升序。
		names := make([]string, len(order))
		for i, k := range order {
			names[i] = provider.DisplayName(k)
		}
		if !sort.SliceIsSorted(names, func(a, b int) bool { return names[a] < names[b] }) {
			t.Errorf("ChannelDisplayOrder 不是显示名字节序：%v", names)
		}
	})

	t.Run("前端 CH_LABEL / CH_CLASS 与 Go 显示名逐项一致", func(t *testing.T) {
		js := repoFile(t, "cmd/wild-work/web/app.js")
		for _, kv := range [][2]string{
			{provider.ModelScope.String(), provider.DisplayName(provider.ModelScope)},
			{provider.ModelScopeAI.String(), provider.DisplayName(provider.ModelScopeAI)},
		} {
			key, label := kv[0], kv[1]
			// CH_LABEL 里必须出现 `key: "label"`（顺序无关，只求存在且值一致）。
			want := key + `: "` + label + `"`
			if !strings.Contains(js, want) {
				t.Errorf("app.js CH_LABEL 缺 %s（期望子串 %s）——前端会回退成裸 kind", key, want)
			}
			// CH_CLASS 缺键 ⇒ chColor() 回退默认色 ⇒ 图上与别的渠道撞色（R99 事故）。
			if !strings.Contains(js, key+`: "`+key+`"`) {
				t.Errorf("app.js CH_CLASS 缺 %s（图上会与别的渠道撞色）", key)
			}
		}
	})

	t.Run("魔粒口径与「无签到」文案同源", func(t *testing.T) {
		js := repoFile(t, "cmd/wild-work/web/app.js")
		// 2026-11-16 反转：两区已有真实魔粒余额端点 ⇒ **不得**再进 NO_CREDITS_BUILTIN
		// （进了面板会显示「不适用」、也没有「刷新积分」入口）；虚拟账号集合同样不含它们
		// （无积分 ≠ 不可删，token 池必须可删可停，R130/R131）。
		noCredits := jsSet(t, js, "NO_CREDITS_BUILTIN")
		virtual := jsSet(t, js, "VIRTUAL_ACCOUNTS")
		for _, k := range []string{"modelscope", "modelscopeai"} {
			if noCredits[k] {
				t.Errorf("NO_CREDITS_BUILTIN 含 %s（会让面板显示「不适用」，而魔粒余额端点实测可用）", k)
			}
			if virtual[k] {
				t.Errorf("VIRTUAL_ACCOUNTS 含 %s（无积分 ≠ 不可删：token 池必须可删可停，R130/R131）", k)
			}
		}
		// 渠道级显示术语「魔粒」必须同源：后端 creditsTerm 定义、前端 credits_term 消费。
		if app := repoFile(t, "internal/app/app.go"); !strings.Contains(app, `return "魔粒"`) {
			t.Error("app.go creditsTerm 未定义「魔粒」术语（面板文案无处可来）")
		}
		if !strings.Contains(js, "credits_term") {
			t.Error("app.js 未消费 credits_term（面板不会显示「可用魔粒 / 魔粒明细」）")
		}
		if !strings.Contains(js, `modelscope: "无签到"`) || !strings.Contains(js, `modelscopeai: "无签到"`) {
			t.Error("NO_CHECKIN_TAG 缺两 kinds 的「无签到」文案（灰标会退回兜底文案，R150）")
		}
	})

	t.Run("能出图标记四层同源（字段 / 渠道侧 / glm 静态表 / /v1/models 输出）", func(t *testing.T) {
		prov := repoFile(t, "internal/provider/provider.go")
		if !strings.Contains(prov, "SupportsImageGeneration bool") {
			t.Error("provider.ModelInfo 缺 SupportsImageGeneration（能出图是独立维度，不得复用 SupportsImages）")
		}
		ms := repoFile(t, "internal/modelscope/models.go")
		if !strings.Contains(ms, "mi.SupportsImageGeneration = IsImageModel(id)") {
			t.Error("modelscope 目录未把命名启发式接到 SupportsImageGeneration（D12 的动态标记未落地）")
		}
		if !strings.Contains(ms, "SupportsImageGeneration: e.Image") {
			t.Error("modelscope 静态表未把 image:true 接到 SupportsImageGeneration（D12 的静态标记未落地）")
		}
		glmSrc := repoFile(t, "internal/glm/constants.go")
		if !strings.Contains(glmSrc, "info.SupportsImageGeneration = true") {
			t.Error("glm 静态表未标记生图模型（glm/image 会以对话模型身份进目录）")
		}
		srv := repoFile(t, "internal/server/handler.go")
		if !strings.Contains(srv, `entry["supports_image_generation"] = true`) {
			t.Error("/v1/models 未输出 supports_image_generation（客户端仍无法区分生图与文本模型）")
		}
		// 面板与 /v1/models 同源：/api/fees 的能力字段要能带出同一标记。
		appSrc := repoFile(t, "internal/app/app.go")
		if !strings.Contains(appSrc, "SupportsImageGeneration: mi.SupportsImageGeneration") {
			t.Error("/api/fees 的模型行未透传 SupportsImageGeneration（面板 🎨 会与 /v1/models 漂移）")
		}
		js := repoFile(t, "cmd/wild-work/web/app.js")
		if !strings.Contains(js, "supports_image_generation") {
			t.Error("前端未消费 supports_image_generation（面板区分不了画图/对话模型）")
		}
	})

	t.Run("按钮存在且顺序与展示序一致", func(t *testing.T) {
		html := repoFile(t, "cmd/wild-work/web/index.html")
		cn := strings.Index(html, `id="btnAddModelScope"`)
		ai := strings.Index(html, `id="btnAddModelScopeAI"`)
		if cn < 0 || ai < 0 {
			t.Fatalf("index.html 缺按钮：btnAddModelScope=%d btnAddModelScopeAI=%d", cn, ai)
		}
		if cn > ai {
			t.Error("按钮 DOM 序与 ChannelDisplayOrder 相反（ModelScope 应在 ModelScopeAI 之前）")
		}
	})

	t.Run("配色三处走变量、值取自同一份文档", func(t *testing.T) {
		css := repoFile(t, "cmd/wild-work/web/style.css")
		spec := repoFile(t, "docs/渠道配色规范.md")

		for _, c := range []struct{ kind, hex string }{
			{"modelscope", "#0369a1"},
			{"modelscopeai", "#854d0e"},
		} {
			rootVar := "--ch-" + c.kind + ": " + c.hex
			if !strings.Contains(css, rootVar) {
				t.Errorf("style.css :root 缺 %q（渠道色单一真源）", rootVar)
			}
			// 徽章与按钮只能引用变量，不得写死 hex（写死就会与折线图 series 漂移）。
			if strings.Contains(css, ".badge."+c.kind+" { background: "+c.hex) {
				t.Errorf(".badge.%s 写死了 hex，必须走 var(--ch-%s)", c.kind, c.kind)
			}
			if !strings.Contains(css, ".badge."+c.kind+" { background: var(--ch-"+c.kind+")") {
				t.Errorf(".badge.%s 未使用 var(--ch-%s)", c.kind, c.kind)
			}
			if !strings.Contains(spec, "`"+c.hex+"`") {
				t.Errorf("docs/渠道配色规范.md 未登记 %s 的色值 %s（改色必须同步文档）", c.kind, c.hex)
			}
		}
	})
}

// jsSet 解析 `const NAME = new Set(["a", "b"])` 形态的集合字面量。
func jsSet(t *testing.T, js, name string) map[string]bool {
	t.Helper()
	re := regexp.MustCompile(`const ` + name + ` = new Set\(\[([^\]]*)\]\)`)
	m := re.FindStringSubmatch(js)
	if m == nil {
		t.Fatalf("app.js 里找不到 %s 集合定义（前端口径真源变了？）", name)
	}
	out := map[string]bool{}
	for _, part := range strings.Split(m[1], ",") {
		if s := strings.Trim(strings.TrimSpace(part), `"`); s != "" {
			out[s] = true
		}
	}
	return out
}

// TestE2E_US15_DocsSynced 用户故事 15：
// 「决议与文档同步（R158/R159、渠道接入备忘、接口参考、降级链、配色规范）」。
func TestE2E_US15_DocsSynced(t *testing.T) {
	agents := repoFile(t, "AGENTS.md")
	res := repoFile(t, "docs/决议项.md")
	notes := repoFile(t, "docs/渠道接入备忘.md")
	api := repoFile(t, "docs/接口参考.md")

	t.Run("决议项表体与索引表都有 R158/R159", func(t *testing.T) {
		for _, r := range []string{"| R158 |", "| R159 |"} {
			if !strings.Contains(res, r) {
				t.Errorf("docs/决议项.md 缺 %s 行", r)
			}
			if !strings.Contains(agents, r) {
				t.Errorf("AGENTS.md 决议索引缺 %s 行（索引与全文必须双份）", r)
			}
		}
	})

	t.Run("范围标记三处同步（R123 纪律）", func(t *testing.T) {
		marks := []struct{ name, body string }{
			{"AGENTS.md §1 标题", agents},
			{"AGENTS.md §6 文档表", agents},
			{"docs/决议项.md 标题", res},
		}
		for _, m := range marks {
			if !strings.Contains(m.body, "R1–R160") {
				t.Errorf("%s 缺范围标记 R1–R160", m.name)
			}
		}
	})

	t.Run("渠道备忘与接口参考覆盖魔搭", func(t *testing.T) {
		if !strings.Contains(notes, "modelscope") {
			t.Error("docs/渠道接入备忘.md 未记录 modelscope（后来者会照旧代码再写一遍硬编码）")
		}
		// 异步生图协议的两个关键头必须写进备忘（否则无从复现协议）。
		for _, hdr := range []string{"X-ModelScope-Async-Mode", "X-ModelScope-Task-Type"} {
			if !strings.Contains(notes, hdr) {
				t.Errorf("docs/渠道接入备忘.md 未记录 %s（生图协议的三步缺一不可）", hdr)
			}
			if !strings.Contains(res, hdr) {
				t.Errorf("docs/决议项.md 的 R158 未记录 %s", hdr)
			}
		}
		if !strings.Contains(api, "modelscope") {
			t.Error("docs/接口参考.md 未提到 modelscope 的粘贴端点 / 生图模型")
		}
	})

	t.Run("降级链文档承认含斜杠模型 ID 的写法", func(t *testing.T) {
		fallback := repoFile(t, "docs/模型降级链定义.md")
		if !strings.Contains(fallback, "ModelScope") && !strings.Contains(fallback, "modelscope") {
			t.Error("docs/模型降级链定义.md 未提及含斜杠的 modelscope 模型 ID（from/to 必须写全串）")
		}
	})
}
