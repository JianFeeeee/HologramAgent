package webui

import (
	"strings"
	"testing"
)

// 插件列表「启动状态」渲染的回归判据（2026-10-20 生产反馈）。
//
// ## 原始问题
//
// 面板只拿内核 status 的 loaded 一个布尔来判断，于是三种完全不同的形态
// 被显示成同一个「未加载」：
//
//	· auto_start=false 且未运行 → **按需插件正常待命**（不是故障）
//	· auto_start=true  且未运行 → 该起没起（需要关注）
//	· 运行态 plugin_stop 之后    → 用户自己停的（重启会恢复）
//
// 用户看到一屏「未加载」，以为插件全坏了 —— 实际它们只是在等 plugin_start。
//
// ## 判据思路
//
// 与 cmd_history_render_test.go 同款：不执行 JS，只守住源码结构 ——
// 渲染函数必须消费 auto_start / runtime_stopped / alive 三个字段，
// 并给出「按需待命」这一独立分支；否则回归立刻红。
func TestPluginStatusUsesRuntimeFields(t *testing.T) {
	src := dashboardJSSource(t)
	start := strings.Index(src, "function renderPlugins()")
	if start < 0 {
		t.Fatal("找不到 renderPlugins —— 函数被重命名或删除，请同步更新本判据")
	}
	end := strings.Index(src[start+1:], "\nfunction ")
	var body string
	if end < 0 {
		body = src[start:]
	} else {
		body = src[start : start+1+end]
	}

	for _, want := range []string{
		"auto_start",      // 区分自启/按需
		"runtime_stopped", // 区分运行态停止与持久禁用
		"alive",           // 区分「已加载」与「进程丢失」
		"按需待命",            // 按需插件待命是独立状态，不能混进「未运行」
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("renderPlugins 未引用 %q —— 状态判定退化，"+
				"按需插件会被误显示成故障。见本文件头部说明", want)
		}
	}
}

// 「启动」按钮必须走运行态接口 /start，不能复用持久态的 /enable ——
// 后者会清掉持久禁用标记，让「点一下启动」变成「以后都自启」。
func TestPluginStartUsesRuntimeEndpoint(t *testing.T) {
	src := dashboardJSSource(t)
	i := strings.Index(src, "async function startPlugin(")
	if i < 0 {
		t.Fatal("找不到 startPlugin")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nasync function "); j > 0 {
		body = body[:j+1]
	}
	if !strings.Contains(body, `"/plugins/" + encodeURIComponent(name) + "/start"`) {
		t.Fatal("startPlugin 必须调用 /plugins/<name>/start（运行态），" +
			"不得复用 /enable（持久态会改动开机行为）")
	}
	if strings.Contains(body, "\"/enable\"") {
		t.Fatal("startPlugin 里出现了 /enable —— 运行态启动不得动持久禁用清单")
	}
}
