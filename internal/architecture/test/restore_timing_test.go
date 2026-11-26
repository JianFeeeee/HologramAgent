// Package test 的 restore 时序判据（2026-11-25 实测回归）。
package test

// TestRestoreWaitsForPluginsReady：子 agent 恢复必须等**插件就绪**之后。
//
// ## 事故
//
// 恢复依次踩了两次时序错误：
//
//	① 在 `mainagent.StartMainRuntime` 里恢复（= `agent` 模块 Init 期）。
//	   而插件要到 `KernelComplete` **之后**由 pluginhost 的 `loadRest` 加载。
//	   现场：6 个子实例全部恢复失败
//	   （`划入 inputch cli: inputch 未注册`），日志却报 `restored 6`。
//
//	② 改挂 `SyncBus.OnUpdate` + `KernelComplete()` —— 仍然太早。
//	   实测 `restored 0` 出现在 `[cli] unix socket listening` 之前。
//	   `KernelComplete` 是"模块装配完成"，不是"插件加载完成"。
//
// ## 正确的信号
//
// `pluginhost` 在内置插件全部加载完之后 `publishToolsReady(ks)`
// （`events.EventToolsReady`）。通道（inputch）此刻才全部就位。
//
// ## 判据形态
//
// 这是**装配时序**，只有真内核能观测完整时序；单元测试里能机械检验的是
// 结构事实：装配点**订阅的是那个正确的信号**，而不是别的就绪条件。
//
// 允许两种退化之一：
//
//	订阅 events.EventToolsReady          （生产路径）
//	无事件总线时立即恢复                  （裁剪版内核 / 判据，没有插件）
//
// 不允许：按 `KernelComplete` 恢复、或在 `Runtime.Start` 里恢复。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestRestoreWaitsForPluginsReady：恢复的挂载点订阅 `EventToolsReady`。
func TestRestoreWaitsForPluginsReady(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../../kernelmod/agent/mod.go", nil, 0)
	if err != nil {
		t.Fatalf("解析装配点: %v", err)
	}

	// 找 mountRestoreOnReady 的函数体。
	var body *ast.BlockStmt
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "mountRestoreOnReady" {
			continue
		}
		body = fn.Body
	}
	if body == nil {
		t.Fatal("★ 装配点没有 mountRestoreOnReady —— 恢复的挂载点没了，" +
			"子 agent 会在插件就绪之前被恢复（通道未注册 ⇒ 全部跳过）")
	}

	var subscribesToolsReady bool
	var usesKernelComplete bool
	ast.Inspect(body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Subscribe":
			subscribesToolsReady = true
		case "KernelComplete":
			usesKernelComplete = true
		}
		return true
	})

	if !subscribesToolsReady {
		t.Fatal("★ mountRestoreOnReady 没有订阅任何事件 —— " +
			"恢复必须等 `EventToolsReady`（插件全部注册完毕的**唯一**信号）")
	}
	if usesKernelComplete {
		t.Fatal("★ mountRestoreOnReady 用了 KernelComplete —— 那是「模块装配完成」，" +
			"不是「插件加载完成」；实测恢复仍早于 [cli] unix socket listening")
	}
}

// TestRestoreIsNotStartedFromRuntimeStart：恢复不在主 agent 的运行期启动里。
//
// 曾经的落地形态是 `StartMainRuntime` → `RestoreChildren()`，那跑在
// `agent` 模块 Init 期，早于插件加载。这条钉住"它不会再被搬回去"。
func TestRestoreIsNotStartedFromRuntimeStart(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "../../agent/mainagent/runtime_main.go", nil, 0)
	if err != nil {
		t.Fatalf("解析 runtime_main.go: %v", err)
	}
	for _, d := range file.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "StartMainRuntime" {
			continue
		}
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "RestoreChildren" {
				found = true
			}
			return true
		})
		if found {
			t.Fatal("★ StartMainRuntime 里又调了 RestoreChildren —— " +
				"那跑在 agent 模块 Init 期，插件还没加载，子实例会因为" +
				"「inputch 未注册」全部恢复失败")
		}
	}
}
