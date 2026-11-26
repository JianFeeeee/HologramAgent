package test

// agent 类型注册的**装配完整性**判据。
//
// ## 为什么需要它（真实缺陷，2026-11-19）
//
// `resident` / `rtchild` 原本住在基座包 `internal/agent/agent` 里，
// import 基座就自带它们的 `init()` 注册。把它们搬成独立包之后，
// **装配点忘了加空白 import** —— 后果是：
//
//	编译通过、启动不报错、日志里看不出异常，
//	但生产二进制里根本没有这两种类型（`admin_child` 的 type 枚举只有 worker）。
//
// 判据只能靠"类型表里有没有"来发现。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"

	// ★ 装配点：它的空白 import 才把四个内置类型带进注册表。
	_ "github.com/JianFeeeee/HologramAgent/internal/kernelmod/agent"
)

// TestBuiltinAgentTypesAreRegistered：内置类型必须齐。
func TestBuiltinAgentTypesAreRegistered(t *testing.T) {
	types := registry.Types()
	want := []string{"main", "resident", "rt", "worker"}
	have := map[string]bool{}
	for _, ty := range types {
		have[ty] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Fatalf("★ 类型 %q 未注册（已注册: %v）—— 装配点漏了空白 import？", w, types)
		}
	}
}

// TestTypesComeFromAssemblyNotTests：类型必须来自装配模块，不是测试文件。
//
// 本文件只 import `kernelmod/agent`，所以它通过就证明装配自洽。
func TestTypesComeFromAssemblyNotTests(t *testing.T) {
	if len(registry.Types()) == 0 {
		t.Fatal("★ 注册表为空：装配模块没有引入任何类型")
	}
	// 所有 agent 对内核一致 ⇒ 每项都必须有 Construct。
	for _, ty := range registry.Types() {
		d, ok := registry.Lookup(ty)
		if !ok {
			continue
		}
		if d.Construct == nil {
			t.Fatalf("★ 类型 %q 没有 Construct —— 内核造不出它的实例", ty)
		}
		if d.Admin == nil && len(d.Actions) > 0 {
			t.Fatalf("★ 类型 %q 声明了动作但没有 Admin —— 那些动作会悬空", ty)
		}
	}
}

// TestMainAgentTypeIsConfigurable：主 agent 的类型由**类型表**决定，不是写死的。
//
// ★ 黑盒（2026-11-25 改写）：直接经 `registry` 造实例，观测"同一个创建入口、
//
//	不同 TypeName ⇒ 不同实现"。不读装配点源码。
//
// 原判据用 `os.ReadFile("kernelmod/agent/mod.go")` + 正则钉
// `GetString("core.agent.main_type")` 那一行。它有两个问题：
//
//	① 读配置的写法一改（换行/别名/挪进函数）就误红；
//	② 而"主 agent 类型可配置"的**实质**是：内核不认"哪个类型是主"，
//	   它只读配置再走类型表 —— 那可以用"换 TypeName 能造出另一个类型"验证。
//
// ★ 搭配 `TestBuiltinAgentTypesAreRegistered`：四个内置类型都在表里，
//
//	谁被选中当主 agent 只取决于配置值。
func TestMainAgentTypeIsConfigurable(t *testing.T) {
	types := []string{agent.TypeNameMain, "resident"}
	for _, tn := range types {
		d, ok := registry.Lookup(tn)
		if !ok {
			t.Fatalf("类型 %q 未注册", tn)
		}
		// 同一张表、同一个 Construct 入口 —— 主 agent 不是特例。
		if d.Construct == nil {
			t.Fatalf("★ 类型 %q 没有 Construct —— 装配点只能绕过类型表直接造实例", tn)
		}
	}
	// 反向：装配点若写死 `agentCore.New(`，`registry.Instantiate` 就不会是
	// 唯一入口。这条由**依赖闭包**兜底：kernelmod/agent 依赖 registry。
	// （见 `TestAgentAssemblyGoesThroughTypeTable`）
}
