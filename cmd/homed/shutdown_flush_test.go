package main

// 停机**两条路径对称性**的判据（2026-10-17）。
//
// ## 缺陷（实测发现）
//
// `main()` 里有 `defer ks.CloseAll()`，但 `waitForShutdown` 在重启路径上
// 调用 `os.Exit(exitRestartRequested)` —— **os.Exit 不跑 defer**。
// ⇒ 两条退出路径的资源清理**不对称**：
//
//	路径 A（SIGTERM）: return 到 main → defer CloseAll ✓
//	路径 B（exit 42）: os.Exit 跳过 defer            ✗
//
// 而路径 B 是**每次热更新都走**的（guard 靠 exit 42 判定"主动重启"）。
// 后果是这些写者从不 flush：
//   · docStore.Stop()  → flush()   文档索引可能陈旧
//   · distiller.Stop() → flush()   待蒸馏规则丢失
//   · textMem.Stop()   → Close()   文本记忆未落盘
//
// ## 判据怎么咬
//
// 不能靠"源码里有 os.Exit 吗"（写意不写实）。这里用 **AST** 检查
// `waitForShutdown` 函数体里**在 os.Exit 之前**确实调用了传入的
// stopKernelModules 形参 —— 即"退出前显式关闭"，而不是靠 defer。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// ★★★ 判据：waitForShutdown 在 os.Exit 之前显式调用 stopKernelModules。
func TestWaitForShutdownClosesModulesBeforeExit(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "bootstrap.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "waitForShutdown" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("找不到 waitForShutdown")
	}

	// 找 os.Exit 与 stopKernelModules() 调用的相对位置（按字节偏移）
	exitPos := token.NoPos
	closePos := token.NoPos
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "os" && sel.Sel.Name == "Exit" {
				if !exitPos.IsValid() {
					exitPos = call.Pos()
				}
			}
		}
		if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "stopKernelModules" {
			if !closePos.IsValid() {
				closePos = call.Pos()
			}
		}
		return true
	})

	if !exitPos.IsValid() {
		t.Fatal("waitForShutdown 里没有 os.Exit（重启路径怎么退出的？）")
	}
	if !closePos.IsValid() {
		t.Fatal("★ waitForShutdown 从未调用 stopKernelModules ⇒ " +
			"热更新路径（os.Exit 跳过 defer）下模块关闭/落盘全部被跳过")
	}
	if closePos > exitPos {
		t.Fatal("★ stopKernelModules 在 os.Exit **之后** ⇒ 永远不会执行" +
			"（os.Exit 立即终止进程）")
	}
}

// ★★★ 判据：main 必须把真实的 ks.CloseAll 传给 waitForShutdown（不是空函数）。
func TestMainWiresRealCloseAllIntoShutdown(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "waitForShutdown" {
			return true
		}
		// 最后一个实参必须是含有 CloseAll 调用的函数字面量
		for _, a := range call.Args {
			lit, ok := a.(*ast.FuncLit)
			if !ok {
				continue
			}
			ast.Inspect(lit, func(m ast.Node) bool {
				if c, ok := m.(*ast.CallExpr); ok {
					if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "CloseAll" {
						found = true
					}
				}
				return true
			})
		}
		return true
	})
	if !found {
		t.Fatal("★ main 传给 waitForShutdown 的 stopKernelModules 里没有 CloseAll ⇒ " +
			"热更新路径下模块不会被关闭（传了个空函数？）")
	}
}
