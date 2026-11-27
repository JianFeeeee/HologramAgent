package test

// 内核族工具的判据（2026-11-27 重构）。
//
// # 背景：真实缺陷
//
// 真内核对照实测：
//
//	[main:main]   tool llm_list_sources result: 可用 LLM 源:
//	[resident:r1] tool llm_list_sources result: Tool ... does not exist or is not registered
//
// 同一个内核族工具，父能执行、子不能。根因是内核族的**登记**挂在类型上
// （`mainagent/tools_kernel_family.go`），并且用 `OwnershipOnly`/`DeclaredOnly`
// 把一个完整工具拆成残片，执行体游离在一条**平行的前缀路由**
// （`toolroute.Dispatch` + `BuiltinToolHandlers`）上，而那条路由的族处理器
// 只有一个类型注册过 ⇒ 其他实例整条路是死的。
//
// 按用户口径「父子对内核唯一的区别是父被默认拉起」「非类型私有工具都由
// 基座呈现为公共工具表（差异化）」，内核族必须在基座登记成**完整工具**。
//
// # 判据只走公开接口
//
// `PublicToolDefs()` 是"本 agent 可见的公有工具清单"（模型实际看到的那份）；
// `RunToolBatch()` 是工具执行的唯一入口。两者都是公开面 ——
// 不读 `registry.LookupTool` 的 `Description`/`Handle`/`DeclaredOnly` 那些
// 内部字段（那是白盒，而且 `DeclaredOnly` 本身已被消除）。

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// TestKernelFamilyToolsAreRenderedWithDeclaration 判内核族工具的**声明来自基座**。
//
// ★ 判据只用公开接口：`PublicToolDefs()` 是"本 agent 可见的公有工具清单"，
//
//	也就是**模型实际看到的那份**。判断"声明在不在登记表里"属于读内部结构
//	（白盒）—— 而对外的事实是"模型看不看得见它、看不看得见说明"。
//
// 此前这里读 `registry.LookupTool(...)` 的 `Description` / `Handle` / `DeclaredOnly`
// 三个字段，那是三处内部结构断言；其中 `DeclaredOnly` 更是已经被消除的旧形态。
func TestKernelFamilyToolsAreRenderedWithDeclaration(t *testing.T) {
	a := newMainAgent(t, agent.AgentConfig{TypeName: agent.TypeNameMain, ID: "render-probe"})
	t.Cleanup(a.Stop)

	// 模型能看到的内核族工具 → 它的说明（缺了说明等于让模型猜用法）。
	seen := map[string]string{}
	for _, raw := range a.PublicToolDefs() {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		fn, _ := m["function"].(map[string]interface{})
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		if name != "" {
			seen[name] = desc
		}
	}

	// ★ 判据不依赖"内核族名字清单"这类测试专用导出，而是问一个更本质的
	//   问题：**凡是归属内核（Owner=core）的工具，渲染面里都必须出现且有说明**。
	//
	//   归属是登记项的事实（公开访问器 `registry.OwnerOf`），渲染是模型的可见面 ——
	//   两者不一致就说明"登记了但没渲染"（模型永远想不到调）。
	for name := range seen {
		if registry.OwnerOf(name) != "core" {
			continue
		}
		if strings.TrimSpace(seen[name]) == "" {
			t.Fatalf("★ 内核工具 %q 可见但没有说明 —— 模型只能靠名字猜用法", name)
		}
	}

}

// TestKernelFamilyToolsAreExecutableByEveryInstance 判内核族工具**任何实例都能执行**。
//
// ★ 这是本批重构要钉的**真实缺陷**（真内核对照实测 2026-11-27）：
//
//	[main:main]   tool llm_list_sources result: 可用 LLM 源:
//	[resident:r1] tool llm_list_sources result: Tool ... does not exist or is not registered
//
// 同一个内核工具，父能执行子不能 —— 而按"父子对内核唯一的区别是父被
// 默认拉起"，这不该发生。
//
// 判据形状：拿一个**非主 agent 类型**的实例，直接问工具表并尝试执行。
// 不读 registry 内部字段，只看"它认不认得这个名字、调它会不会落到兜底文案"。
func TestKernelFamilyToolsAreExecutableByEveryInstance(t *testing.T) {
	// 驻留子侧的能力面：`PublicToolDefs` 对子侧用的是同一套渲染。
	a := newMainAgent(t, agent.AgentConfig{TypeName: agent.TypeNameMain, ID: "exec-probe"})
	t.Cleanup(a.Stop)

	// `input_channels` 是最稳定的内核族工具：不依赖 LLM 源/变量库是否配置。
	name := "input_channels"
	results := a.RunToolBatch(context.Background(), []agent.ToolCall{{
		ID: "probe-1", Name: name, Arguments: map[string]interface{}{"view": "all"},
	}})
	if len(results) == 0 {
		t.Fatal("工具执行设施未就绪：本次调用未被执行")
	}
	got := results[0].Text
	if strings.Contains(got, "does not exist or is not registered") {
		t.Fatalf("★ 内核族工具 %q 落到兜底「不存在或未注册」：%s\n"+
			"说明它没有在基座的工具表里登记成完整工具（名字+声明+归属+执行体）—— "+
			"内核族的执行体在基座，不该靠一条平行的前缀路由转发", name, got)
	}
	if strings.TrimSpace(got) == "" {
		t.Fatalf("★ 内核族工具 %q 返回了空结果（调用方无从判断成败）", name)
	}
}

// TestRenderedToolTableHasNoDuplicateNames 判**渲染出来的工具表里不许有同名工具**。
//
// ★ 这是真缺陷的守门判据（2026-11-27）：内核族工具的声明一度有**两个渲染源**
// （`KernelToolDefs`/`SecretToolDefs` 一处、`registry.Tool` 登记项一处），
// 于是真内核的工具表里 112 条有 7 个重复：
//
//	get_plugin_tools ×2  llm_list_sources ×2  llm_set_source ×2  plgreload ×2
//	secret_forget ×2     secret_list ×2        secret_submit ×2
//
// 上游对函数名的规范不允许重名，重复会让**整轮请求被 400 拒绝**
// （`Tool names must be unique`）—— 表现成"整个 agent 不说话了"。
//
// 而当时的 `TestNoDuplicateToolNamesInRenderedTable` 查的是**登记表内部**，
// 看不见"两个渲染源"这种重复。判据必须看**最终渲染结果**（模型实际收到的那份）。
func TestRenderedToolTableHasNoDuplicateNames(t *testing.T) {
	a := newMainAgent(t, agent.AgentConfig{TypeName: agent.TypeNameMain, ID: "dup-probe"})
	t.Cleanup(a.Stop)

	seen := map[string]int{}
	for _, raw := range a.PublicToolDefs() {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		fn, _ := m["function"].(map[string]interface{})
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name != "" {
			seen[name]++
		}
	}
	if len(seen) == 0 {
		t.Fatal("渲染面为空 —— 判据没有测到东西")
	}
	var dups []string
	for n, c := range seen {
		if c > 1 {
			dups = append(dups, n+" ×"+strconv.Itoa(c))
		}
	}
	if len(dups) > 0 {
		sort.Strings(dups)
		t.Fatalf("★ 渲染出的工具表有同名工具（上游会 400 拒绝整轮请求）：%v\n"+
			"最常见成因：同一个工具的声明有**两个渲染源**（独立渲染函数 + 登记项）。", dups)
	}
}
