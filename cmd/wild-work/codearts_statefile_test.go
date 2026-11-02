package main

import (
	"os"
	"strings"
	"testing"
)

// TestCodeArtsUnusableFileIsDedicated 装配层护栏：模型摘除记录**必须**落独立文件。
//
// 为什么需要这条静态断言：客户端单测（TestUnusableNotClobberedByPoolSave）自己选文件路径，
// 只能证明「用独立文件时不被 pool 抹掉」，无法证明**装配层真的选了独立文件**。
// 变异验证实测：把 main.go 的路径改回共用的 state-codearts.json，客户端测试仍然全绿
// （测试缓存命中，毫无察觉）——这条断言正是补那个缺口。
//
// 背景（线上事故 2026-10-26）：pool.saveLocked 用 json.MarshalIndent 整文件覆写
// state-codearts.json，它不认识 unusable_models 键 ⇒ 一次积分刷新/签到就把摘除记录
// 抹掉，被摘除的模型重启后重新出现在 /v1/models。
func TestCodeArtsUnusableFileIsDedicated(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	const want = `caUp.UnusableFile = filepath.Join(stateDir, "models-codearts.json")`
	if !strings.Contains(string(src), want) {
		t.Fatalf("装配层必须用独立文件: 找不到 %q\n"+
			"若改回 state-codearts.json，pool 整文件覆写会抹掉摘除记录", want)
	}
}
