package main

import (
	"os"
	"strings"
	"testing"
)

// 本测试锁定一个**真实 bug**（2026-10-15 发现）：
//
//	启动时 `appInst.StartServer()` 失败（最常见：端口已被另一个实例占用）
//	原实现只 `log.Printf` 一行就继续往下跑 —— 调度器与托盘照常起来，
//	表现为「面板看着一切正常、其实是别的实例在服务本端口」，
//	而两个进程共享同一数据目录（config.json / auths / data），
//	会重复签到并互相覆盖刷新出的 token。
//
// 为什么用静态检查而不是运行时测试：
// 触发这条分支必须真实占用端口、再观测**进程退出**，单测里做不到；
// 而「失败分支有没有终止进程」在源码层面就能判定。
func TestListenFailureIsFatal(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 main.go 失败: %v", err)
	}
	code := string(src)

	const marker = "appInst.StartServer(); err != nil {"
	i := strings.Index(code, marker)
	if i < 0 {
		t.Fatalf("未在 main.go 中找到 %q（代码结构已变？）", marker)
	}

	block := braceBlock(t, code[i+len(marker)-1:])
	if !strings.Contains(block, "fatal(") {
		t.Errorf("❌ StartServer 失败分支未调用 fatal()，进程会带着「监听失败」继续跑：\n"+
			"   调度器与托盘照常启动，面板却由别的实例服务；两进程共享数据目录，\n"+
			"   会重复签到并互相覆盖刷新出的 token。\n"+
			"   失败分支源码：\n%s", block)
	}
}

// braceBlock 从 `{` 起按花括号配平截取一个完整代码块（含首尾花括号）。
func braceBlock(t *testing.T, s string) string {
	t.Helper()
	depth := 0
	for i, r := range s {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1]
			}
		}
	}
	t.Fatalf("花括号不配平，无法截取代码块")
	return ""
}
