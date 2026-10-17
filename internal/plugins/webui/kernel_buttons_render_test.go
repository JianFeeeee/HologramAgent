package webui

// 内核按钮必须**真的出现在渲染后的页面里**（2026-10-17）。
//
// ## 为什么这条判据必须存在
//
// dashboard.html 与 dashboard.js 都是 `//go:embed` 进二进制的，然后由
// init 组装成 dashboardHTML（把 {{DASHBOARD_JS}} 等占位符替换成实际内容）。
// 这里有两个**各自都可能坏掉**的环节：
//
//  1. 按钮写进了 dashboard.html，但占位符没被替换/替换错位
//     ⇒ 页面里看不到按钮
//  2. JS 函数写进了 dashboard.js，但它被包在 IIFE 里（不在全局）
//     ⇒ onclick="kernelRestart()" 点了报 "kernelRestart is not defined"
//
// 只查源码有没有那个字符串，这两种情况都照不出来。
// 所以判据查的是**组装后的 dashboardHTML**（就是实际发给浏览器的字节）。

import (
	"strings"
	"testing"
)

// ★★★ 判据：渲染后的页面含两个按钮的 id 与 onclick。
func TestKernelButtonsRenderedInPage(t *testing.T) {
	h := dashboardHTML
	if h == "" {
		t.Fatal("dashboardHTML 为空 —— init 没跑或 embed 失败")
	}
	for _, want := range []struct {
		what string
		s    string
	}{
		{"重启按钮 id", `id="kernel-restart-btn"`},
		{"关机按钮 id", `id="kernel-shutdown-btn"`},
		{"重启按钮 onclick", `onclick="kernelRestart()"`},
		{"关机按钮 onclick", `onclick="kernelShutdown()"`},
	} {
		if !strings.Contains(h, want.s) {
			t.Fatalf("★ 渲染页面缺少%s（%q）—— 用户在界面上看不到它", want.what, want.s)
		}
	}
}

// ★★★ 判据：JS 函数必须在**全局作用域**（onclick 才找得到）。
//
// 检查方式：函数定义前不能有包裹它的 IIFE 缩进层级 —— 用"行首即 function"
// 判定它处于顶层。jsdom 里的行为测试做不到这点（node 不强求），
// 而浏览器里 onclick 走的是全局查找。
func TestKernelFunctionsAreGlobal(t *testing.T) {
	src := string(mustReadEmbedded(t, "dashboard.js"))

	for _, fn := range []string{"kernelRestart", "kernelShutdown", "doKernelControl", "waitForKernelBack"} {
		// 顶层定义：行首（可有 async 前缀），不带缩进
		if !strings.Contains(src, "\nasync function "+fn+"(") &&
			!strings.Contains(src, "\nfunction "+fn+"(") {
			t.Fatalf("★ %s 不是顶层函数定义 —— "+
				"若它被包在 IIFE/模块里，按钮 onclick 会报 is not defined", fn)
		}
	}
}

// ★★★ 判据：两个按钮都必须在 nav-footer 里（不是被误放到某个标签页内）。
//
// 它们作用于整个服务，任何标签页都该能点到。
func TestKernelButtonsInNavFooter(t *testing.T) {
	h := dashboardHTML
	navIdx := strings.Index(h, `class="nav-footer"`)
	if navIdx < 0 {
		t.Fatal("找不到 nav-footer")
	}
	navEnd := strings.Index(h[navIdx:], "</nav>")
	if navEnd < 0 {
		t.Fatal("找不到 nav 结束标签")
	}
	footer := h[navIdx : navIdx+navEnd]

	for _, id := range []string{"kernel-restart-btn", "kernel-shutdown-btn"} {
		if !strings.Contains(footer, id) {
			t.Fatalf("★ %s 不在 nav-footer 内 —— 用户切到别的标签页就找不到它了", id)
		}
	}
}
