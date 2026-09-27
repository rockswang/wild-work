package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 本测试锁定一个**真实 bug**（2026-09-27 发现）：
//
//	新增 glm 渠道时，在 main.go 创建了 glmSch、注册进 runtimes、
//	设了观察者，**但漏了 `go glmSch.Run(sctx)`** ——
//	后果是「GLM 的自动签到与保活从不运行」，而手工触发仍可用，
//	**不报错、不崩溃**，极难发现（用户是手工签到后才发现的）。
//
// 本测试用静态检查兜住这类"新增渠道漏启动"的疏漏：
// 凡是 `Xxx := scheduler.New(...)` 定义的调度器，都必须有对应的 `.Run(`。
//
// 为什么用静态检查而不是运行时测试：
// 调度器是 `go ...Run(ctx)` 长期后台循环，无法在单测里断言"它跑起来了"；
// 而"定义了却没启动"这个疏漏**在源码层面就能判定**。

func TestAllSchedulersAreStarted(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	code := string(src)

	// ① 收集所有 `xxxSch := scheduler.New(` 定义的调度器变量名
	defRe := regexp.MustCompile(`(\w+)\s*:=\s*scheduler\.New\(`)
	defs := map[string]bool{}
	for _, m := range defRe.FindAllStringSubmatch(code, -1) {
		defs[m[1]] = true
	}
	if len(defs) == 0 {
		t.Fatal("未在 main.go 中找到任何 scheduler.New 定义（正则失效？）")
	}
	t.Logf("发现 %d 个调度器定义: %v", len(defs), keys(defs))

	// ② 收集所有 `go xxxSch.Run(` 启动的调度器
	runRe := regexp.MustCompile(`go\s+(\w+)\.Run\(`)
	started := map[string]bool{}
	for _, m := range runRe.FindAllStringSubmatch(code, -1) {
		started[m[1]] = true
	}
	t.Logf("发现 %d 个已启动: %v", len(started), keys(started))

	// ③ 逐个比对：定义了就必须启动
	var missing []string
	for name := range defs {
		if !started[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("❌ 以下调度器已定义但**未启动**（自动签到/保活不会运行，且不报错）:\n  %s\n"+
			"修复：在 main.go 的「调度器后台运行」段补 `go %s.Run(sctx)`",
			strings.Join(missing, "\n  "), missing[0])
	}
}

// TestSchedulerCountMatchesRuntimes 交叉校验：
// 注册进 runtimes 的渠道，其 Scheduler 都必须已启动。
//
// 防止"注册了渠道但忘了启动其调度器"（本次 bug 的形态）。
func TestSchedulerCountMatchesRuntimes(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	code := string(src)

	// runtimes 里声明的 `Scheduler: xxxSch`
	rtRe := regexp.MustCompile(`Scheduler:\s*(\w+)`)
	declared := map[string]bool{}
	for _, m := range rtRe.FindAllStringSubmatch(code, -1) {
		declared[m[1]] = true
	}

	// 已启动的
	runRe := regexp.MustCompile(`go\s+(\w+)\.Run\(`)
	started := map[string]bool{}
	for _, m := range runRe.FindAllStringSubmatch(code, -1) {
		started[m[1]] = true
	}

	var missing []string
	for name := range declared {
		if !started[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("❌ 以下渠道在 runtimes 中注册了 Scheduler 但未启动:\n  %s",
			strings.Join(missing, "\n  "))
	}
	t.Logf("runtimes 注册 %d 个 Scheduler，已启动 %d 个", len(declared), len(started))
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
