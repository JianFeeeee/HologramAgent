package test

// internal/agent/core 的**工具定义对外投影**判据 —— 面向接口的黑盒。
//
// 入口：core.NewStageHost() 注册工具 → core.New(AgentConfig{StageHost}) →
// Agent.ToolDefinitions()（api.ToolDefinition 列表）。
//
// ## 原判据的缺口
//
// `stages_tooldef_test.go` 只测 **StageHost 内部的两个查询**与 ToolDef 等价
// （`ConcurrencySafeOf` / `NoMemoryOf` 是否等于 `ToolDef().ParallelSafe`），
// 而且它自己就把期望值从 `def.ParallelSafe && !def.Serial` 抄了一遍 ——
// 那是把实现搬进判据。
//
// 真正决定行为的是 `Agent.ToolDefinitions()`：模型看到的那份清单，
// 其 ParallelSafe 位决定整批并发还是串行。这条**对外投影**零覆盖。
//
// ## 为什么 `Serial` 优先是必须的契约
//
// `Serial:true` + `ParallelSafe:true` 同时写时，必须先尊重 Serial：
// 工具作者写 Serial 是想保护自己（有副作用、要求外界没并发进来）。
// 若被 ParallelSafe 覆盖，那个字段就是个**没被读取的死字段** ——
// 作者以为自己在保护，实际毫无作用，且不会有任何报错。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/core"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// toolDefHarness 造一个带工具集的 agent。
func toolDefHarness(t *testing.T, defs ...sdk.ToolDef) *core.Agent {
	t.Helper()
	sh := core.NewStageHost()
	noop := func(map[string]interface{}) (interface{}, error) { return nil, nil }
	for _, d := range defs {
		if err := sh.RegisterTool(d.Name, d, noop); err != nil {
			t.Fatalf("RegisterTool(%s): %v", d.Name, err)
		}
	}
	a := core.New(core.AgentConfig{ID: "tooldef", StageHost: sh})
	t.Cleanup(a.Stop)
	return a
}

// parallelSafeOf 从对外清单里读某工具的并发位。
func parallelSafeOf(t *testing.T, a *core.Agent, name string) (safe, found bool) {
	t.Helper()
	for _, d := range a.ToolDefinitions() {
		if d.Name == name {
			return d.ParallelSafe, true
		}
	}
	return false, false
}

// ---- 判据 1：Serial 优先于 ParallelSafe（对外投影上）----

// TestSerialOverridesParallelSafeInToolDefinitions：两个位同时声明时，
// 对外清单必须报 ParallelSafe=false。
//
// 这条在源码注释里被点名（`argvalidate.go`：「判据 TestSerialOverridesParallelSafe」
// 钉住它），但**那个判据不存在** —— 而它守的是"显式声明必须串行"这条
// 工具作者的自我保护手段。
//
// 变异：把 ConcurrencySafeOf 的实现改成只读 ParallelSafe（不看 Serial）
// ⇒ 本判据红。
func TestSerialOverridesParallelSafeInToolDefinitions(t *testing.T) {
	a := toolDefHarness(t,
		sdk.ToolDef{Name: "par", ParallelSafe: true},
		sdk.ToolDef{Name: "ser", Serial: true},
		sdk.ToolDef{Name: "both", ParallelSafe: true, Serial: true},
		sdk.ToolDef{Name: "plain"},
	)

	cases := []struct {
		name string
		want bool
	}{
		{"par", true},    // 只声明 ParallelSafe ⇒ 可并发
		{"ser", false},   // 只声明 Serial ⇒ 不可并发
		{"both", false},  // 两个都声明 ⇒ **Serial 赢**
		{"plain", false}, // 什么都不声明 ⇒ 保守串行
	}
	for _, c := range cases {
		got, found := parallelSafeOf(t, a, c.name)
		if !found {
			t.Fatalf("★ 工具 %q 没出现在对外清单里", c.name)
		}
		if got != c.want {
			t.Fatalf("★ 工具 %q 的 ParallelSafe=%v，期望 %v（Serial 必须优先于 ParallelSafe）",
				c.name, got, c.want)
		}
	}
}

// ---- 判据 2：未声明的工具按"串行"走（保守）----

// TestUndeclaredToolsAreSerial：既没声明 ParallelSafe 也没声明 Serial 的工具
// （含**根本不存在的工具**）必须判不可并发。
//
// 保守在这里是刻意的：「判断不出来就按串行走」让存量插件不会因为新语义
// 而意外并发 —— 意外并发会让原本靠"批内只有一个工具在跑"的工具出错，
// 而慢一点没人会踩雷。
func TestUndeclaredToolsAreSerial(t *testing.T) {
	a := toolDefHarness(t, sdk.ToolDef{Name: "cautious"})

	if safe, _ := parallelSafeOf(t, a, "cautious"); safe {
		t.Fatal("★ 未声明 ParallelSafe 的工具被判可并发 —— 存量插件会意外并发")
	}
	// 不存在的工具同样必须 false（不能因为"查不到"就默认放行）
	if safe, found := parallelSafeOf(t, a, "no-such-tool"); found || safe {
		t.Fatalf("★ 不存在的工具出现在清单里或报可并发：found=%v safe=%v", found, safe)
	}
}

// ---- 判据 3：插件摘除后工具从对外清单消失 ----

// TestUnregisteredPluginToolsLeaveNoTrace：插件卸载后，它注册的工具必须
// 从对外清单里消失。
//
// 这是"热重载静默失效"那一类缺陷的对外观测点：工具没了模型才不会调，
// 而残留的工具会让模型调到一个不存在的 handler（表现为执行时报错，
// 但清单看起来一切正常）。
func TestUnregisteredPluginToolsLeaveNoTrace(t *testing.T) {
	sh := core.NewStageHost()
	noop := func(map[string]interface{}) (interface{}, error) { return nil, nil }
	for _, name := range []string{"alpha_tool", "beta_tool"} {
		if err := sh.RegisterTool(name, sdk.ToolDef{Name: name, Plugin: "acme"}, noop); err != nil {
			t.Fatalf("RegisterTool: %v", err)
		}
	}
	if err := sh.RegisterTool("keep_tool", sdk.ToolDef{Name: "keep_tool", Plugin: "other"}, noop); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	a := core.New(core.AgentConfig{ID: "tooldef", StageHost: sh})
	t.Cleanup(a.Stop)

	sh.UnregisterPluginTools("acme")

	names := map[string]bool{}
	for _, d := range a.ToolDefinitions() {
		names[d.Name] = true
	}
	for _, gone := range []string{"alpha_tool", "beta_tool"} {
		if names[gone] {
			t.Fatalf("★ 插件 acme 卸载后工具 %q 仍在对外清单里 —— 模型会调一个不存在的 handler", gone)
		}
	}
	if !names["keep_tool"] {
		t.Fatal("★ 摘除 acme 时把别的插件的工具也删了")
	}
}

// ---- 判据 4：NoMemory 声明必须到达对外查询 ----

// TestNoMemoryDeclarationIsVisible：工具声明的 NoMemory 必须能从
// StageHost 的对外查询读回。
//
// 它与并行位是两件不同的事，但都属于"工具定义写下的声明真的被读到了吗"
// 这个共同契约：写得下、读得到，中间没有白名单把字段吃掉。
func TestNoMemoryDeclarationIsVisible(t *testing.T) {
	sh := core.NewStageHost()
	noop := func(map[string]interface{}) (interface{}, error) { return nil, nil }
	if err := sh.RegisterTool("quiet", sdk.ToolDef{Name: "quiet", NoMemory: true}, noop); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}
	if err := sh.RegisterTool("loud", sdk.ToolDef{Name: "loud"}, noop); err != nil {
		t.Fatalf("RegisterTool: %v", err)
	}

	if v, ok := sh.NoMemoryOf("quiet"); !ok || !v {
		t.Fatalf("★ 声明 NoMemory=true 的工具读回 %v（ok=%v）", v, ok)
	}
	if v, ok := sh.NoMemoryOf("loud"); !ok || v {
		t.Fatalf("★ 未声明 NoMemory 的工具读回 %v（ok=%v）", v, ok)
	}
	names := sh.NoMemoryToolNames()
	if !names["quiet"] {
		t.Fatal("★ NoMemoryToolNames 里没有 quiet —— 记忆计算层拿不到这份清单")
	}
	if names["loud"] {
		t.Fatal("★ NoMemoryToolNames 里混进了未声明的 loud")
	}
}
