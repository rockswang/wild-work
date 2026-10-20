package app

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"
)

// TestLoginCapabilitySingleSource 断言「渠道 → 登录能力」只有一张声明式表，
// 且所有登录相关的调用点都不再持有自己的渠道分支。
//
// 背景（2026-10-18 深档排障，H3/H9 的根因）：
// 改造前 `StartLoginFor` 一个函数里有 3 张 switch（生效 6-7 张表）：
//
//	A 表 app.go:634  准入判定（混了 5 种语义：真编排 / 导入型拒绝 /
//	                 匿名拒绝 / 占位 / 两步流程指路）
//	B 表 app.go:683  loginClient 工厂
//	C 表 app.go:719  Start() 调用
//	+ CancelLogin  逐渠道 Shutdown
//	+ pollLogin    逐渠道 Poll 分派 + 逐渠道 Shutdown
//
// 同一个渠道要在这 6-7 处各登记一次，且全靠注释约定「必须与前端对齐」。
// 事故已经发生两次（officeace 死按钮、7 渠道无 ⟳ 入口），且第一次的「修复」
// 就是再手工对齐一遍 —— 这正是架构问题的判据。
//
// 本测试把「结构上不许有第二份渠道清单」变成可执行断言：
//  1. 必须存在单一来源表（loginFlows）；
//  2. 登录链路的 4 个函数体内，**不得**再出现按 provider.X 分支的 switch。
func TestLoginCapabilitySingleSource(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("读 app.go 失败: %v", err)
	}

	// ① 单一来源表必须存在。
	if !containsTable(src) {
		t.Fatal("❌ 未找到渠道登录能力单一来源表（loginFlows）—— " +
			"登录编排仍散落在多张手工维护的 switch 里")
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "app.go", src, 0)
	if err != nil {
		t.Fatalf("解析 app.go 失败: %v", err)
	}

	// ② 登录链路的这 4 个函数体内不得再出现「按 provider.X 分支」的 switch/case。
	//    允许的例外：default 分支与「Fallback/不支持」之类的兜底。
	for _, fnName := range []string{"StartLoginFor", "CancelLogin", "pollLogin"} {
		fn := findFunc(file, fnName)
		if fn == nil {
			t.Fatalf("未找到函数 %s", fnName)
		}
		var offenders []string
		ast.Inspect(fn, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			for _, stmt := range sw.Body.List {
				cc, ok := stmt.(*ast.CaseClause)
				if !ok {
					continue
				}
				for _, expr := range cc.List {
					se, ok := expr.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					if id, ok := se.X.(*ast.Ident); ok && id.Name == "provider" {
						offenders = append(offenders,
							fset.Position(se.Pos()).String()+" → provider."+se.Sel.Name)
					}
				}
			}
			return true
		})
		if len(offenders) > 0 {
			t.Errorf("❌ %s 里仍有 %d 处按 provider.X 的渠道分支（应改为查单一来源表）：",
				fnName, len(offenders))
			for _, o := range offenders {
				t.Errorf("     %s", o)
			}
		}
	}

	// ③ 登录链路里也不得再出现「串行 if 链」式的渠道分派（改造前 pollLogin 里有 13 个
	//    `if a.loginKind == provider.X`），以及 CancelLogin 里 6 个 `if kind == provider.X`。
	//    这两处是「表之外的第二来源」，一旦允许存在，收表就名存实亡。
	for _, fnName := range []string{"StartLoginFor", "CancelLogin", "pollLogin"} {
		fn := findFunc(file, fnName)
		if fn == nil {
			continue
		}
		var chain []string
		ast.Inspect(fn, func(n ast.Node) bool {
			ifs, ok := n.(*ast.IfStmt)
			if !ok {
				return true
			}
			be, ok := ifs.Cond.(*ast.BinaryExpr)
			if !ok || be.Op != token.EQL {
				return true
			}
			// 左值形如 a.loginKind / kind，右值形如 provider.X
			if se, ok := be.Y.(*ast.SelectorExpr); ok {
				if id, ok := se.X.(*ast.Ident); ok && id.Name == "provider" {
					chain = append(chain,
						fset.Position(ifs.Pos()).String()+" → provider."+se.Sel.Name)
				}
			}
			return true
		})
		if len(chain) > 0 {
			t.Errorf("❌ %s 里仍有 %d 处 `if … == provider.X` 式渠道分支（应改为查 loginFlows）：",
				fnName, len(chain))
			for _, o := range chain {
				t.Errorf("     %s", o)
			}
		}
	}
}

// findFunc 在文件顶层声明里按名字找 *ast.FuncDecl。
func findFunc(file *ast.File, name string) *ast.FuncDecl {
	for _, d := range file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name && fd.Body != nil {
			return fd
		}
	}
	return nil
}

// containsTable 判断 app.go 里是否已存在单一的渠道登录能力表。
// 判据取「包级变量 loginFlows」这个符号，避免依赖具体字面量形态。
func containsTable(src []byte) bool {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "app.go", src, 0)
	if err != nil {
		return false
	}
	found := false
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range vs.Names {
				if name.Name == "loginFlows" {
					found = true
				}
			}
		}
	}
	return found
}
