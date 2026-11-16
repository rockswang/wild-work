package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// blockAfter 从 marker 起截取第一个**花括号配平**的块（含首尾大括号）。
// 用于对 main.go 的装配片段做源码级契约断言——装配漏项不会报错、也不会崩溃，
// 只表现为「渠道在面板里看着正常但某条链路从不生效」，只能静态检查兜住。
func blockAfter(t *testing.T, src, marker string) string {
	t.Helper()
	i := strings.Index(src, marker)
	if i < 0 {
		t.Fatalf("main.go 找不到锚点 %q", marker)
	}
	start := strings.IndexByte(src[i:], '{')
	if start < 0 {
		t.Fatalf("锚点 %q 之后没有开括号", marker)
	}
	start += i
	depth := 0
	for j := start; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : j+1]
			}
		}
	}
	t.Fatalf("锚点 %q 之后的花括号未配平", marker)
	return ""
}

// TestModelscopeWiringSchedulers 两个渠道的调度器必须「起得来」且**没有任何定时任务**：
//
//   - `go msSch.Run(sctx)` / `go msaiSch.Run(sctx)` 漏了不报错（R34），只表现为
//     「将来给它加任务时静默不跑」；
//   - CheckinMinutes/KeepaliveHours 必须是**显式空切片**（R39）：传 nil 会被
//     scheduler.New 补成默认 9:00/21:00 签到与 22:00 保活，每天空跑必然失败的调用
//     并把面板染成「签到失败」（R150）；
//   - **不得**声明 FollowGlobalCheckin：那会让面板改一次签到时间就把任务塞回来。
func TestModelscopeWiringSchedulers(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)

	for _, tc := range []struct{ name, sch, kind string }{
		{"国内站", "msSch", "modelscope"},
		{"国际站", "msaiSch", "modelscopeai"},
	} {
		// 锚定行首（允许缩进）而不是裸 substring：`// go msSch.Run(sctx)` 这类
		// 被注掉的启动行同样包含子串，裸 Contains 会把它误判成已启动（实测变异逃逸）。
		runRe := regexp.MustCompile(`(?m)^\s*go\s+` + tc.sch + `\.Run\(sctx\)`)
		if !runRe.MatchString(code) {
			t.Errorf("%s 调度器缺少 `go %s.Run(sctx)`（R34：漏了静默不跑）", tc.name, tc.sch)
		}
		blk := blockAfter(t, code, tc.sch+" := scheduler.New(scheduler.Config{")
		if !strings.Contains(blk, `Name: "`+tc.kind+`"`) {
			t.Errorf("%s 调度器 Name 应为 %q，实际块：%s", tc.name, tc.kind, blk)
		}
		if !strings.Contains(blk, "CheckinMinutes: []int{}") {
			t.Errorf("%s 调度器 CheckinMinutes 必须是显式空切片（R39，传 nil 会被补成默认签到）", tc.name)
		}
		if !strings.Contains(blk, "KeepaliveHours: []int{}") {
			t.Errorf("%s 调度器 KeepaliveHours 必须是显式空切片（R39，传 nil 会被补成默认保活）", tc.name)
		}
		if strings.Contains(blk, "FollowGlobalCheckin") {
			t.Errorf("%s 不得声明 FollowGlobalCheckin（无签到渠道，改面板签到时间会塞回任务）", tc.name)
		}
	}
}

// TestModelscopeWiringRuntimeTables runtimes 与 appRuntimes 是**两套表**：
// 漏一套表现为「面板能加号但聊天 503」或「签到/刷新认得它但 handler 不认」。
func TestModelscopeWiringRuntimeTables(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)

	tables := []struct {
		name   string
		marker string
	}{
		{"runtimes", "runtimes := map[provider.Kind]*server.Runtime{"},
		{"appRuntimes", "appRuntimes := map[provider.Kind]*app.Runtime{"},
	}
	for _, tb := range tables {
		blk := blockAfter(t, code, tb.marker)
		for _, k := range []string{"provider.ModelScope:", "provider.ModelScopeAI:"} {
			if n := strings.Count(blk, k); n != 1 {
				t.Errorf("%s 表中 %s 出现 %d 次，期望恰好 1 次", tb.name, k, n)
			}
		}
		// 两区是真多 token 池：不得置 SingleAccount（那会跳过全部账号级惩罚与轮换）。
		if strings.Contains(blk, "provider.ModelScope: {Kind: provider.ModelScope, Pool: msPool, Upstream: msUp, SingleAccount") ||
			strings.Contains(blk, "provider.ModelScopeAI: {Kind: provider.ModelScopeAI, Pool: msaiPool, Upstream: msaiUp, SingleAccount") {
			t.Error("ModelScope 是真多 token 池，不得置 SingleAccount")
		}
	}
}

// TestModelscopeWiringAuthAndPools 两区各自的 auth 加载器与账号池必须都在装配里：
// 两区 token 不通用（独立服务），共用一个池会让「随机 401」变成常态。
func TestModelscopeWiringAuthAndPools(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	code := string(src)

	for _, want := range []string{
		"auth.LoadModelscopeDir(cfg.AuthDir)",
		"auth.LoadModelscopeAIDir(cfg.AuthDir)",
		`filepath.Join(stateDir, "state-modelscope.json")`,
		`filepath.Join(stateDir, "state-modelscopeai.json")`,
	} {
		if !strings.Contains(code, want) {
			t.Errorf("main.go 缺少 %q（装配漏项：账号加载不进去 / 两区共用 state 文件）", want)
		}
	}
}

// TestModelscopeInCreditRefresh 两区的魔粒余额端点在主站（见 internal/modelscope/credits.go）：
// **必须**进积分自动刷新 kinds（10 分钟），否则对话消耗后面板数字永远停在首次校验时的旧值。
// （2026-11-16 票 01 探针推翻了「上游无余额端点」的旧结论，此前的反向断言已反转。）
func TestModelscopeInCreditRefresh(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	blk := blockAfter(t, string(src), "appInst.StartCreditAutoRefresh(sctx, []provider.Kind{")
	for _, k := range []string{"provider.ModelScope,", "provider.ModelScopeAI,"} {
		if !strings.Contains(blk, k) {
			t.Errorf("main.go 积分刷新白名单缺 %s（纳入后每 10 分钟刷新真实魔粒余额）", k)
		}
	}
}