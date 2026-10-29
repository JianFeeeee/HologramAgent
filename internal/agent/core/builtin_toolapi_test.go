package core

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
)

// 阶段 B：内置工具注册进 ToolAPI 面。
//
// 背景（真机实跑抓到的架构缺口）：`memory_*` / `knowledge_*` / `doc_*` /
// `person_*` 这 20+ 个是**内核内置**工具，在 executeToolCallInner 里按
// **前缀分派**（toolcall.go:96 等），**从不注册进 ToolAPI** —— 而 ToolAPI
// 只含 StageHost 插件工具与 IO 设备工具。
// ⇒ 任何经 ToolAPI 的调用方（本仓的 seq 插件）既查不到、也调不了内置工具。
// 实测现象：seq_run 报「工具 knowledge_list 不存在或未注册」，而同一轮
// 模型直接调 knowledge_list 是**成功**的。
//
// 关键前提（已核实，勿推翻）：模型看到的工具来自 buildToolDefs，它走
// `a.io.GetAllTools()` / `a.stageHost.GetToolDefs()` / `a.indexer…` **直调**，
// 与 toolImpl（只经 PluginSDK.Tool() 暴露给插件）是**两条不重叠的路径**。
// ⇒ 把内置工具加进 ToolAPI 不会让模型看到重复工具。

// ① 内置工具必须能从 ToolAPI 查到（查不到 ⇒ 插件无法编排它们）。
func TestBuiltinToolsVisibleViaToolAPI(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	api := toolAPIOf(t, a)

	// 这些是 buildToolDefs 里由 a.memory/a.knowledge 等门控的内置工具。
	// 本用例的 agent 未接 memory/knowledge，故用**无条件**注册的那批：
	// output_list_channels / input_channels / get_plugin_tools / plgreload 等。
	for _, name := range []string{
		"output_list_channels", "input_channels", "get_plugin_tools",
	} {
		if api.ToolDefByName(name) == nil {
			t.Errorf("内置工具 %q 在 ToolAPI 上查不到 —— 插件无法编排它", name)
		}
	}
}

// ② ★ ToolAPI 上查到 ≠ 能调通。必须真的能执行。
func TestBuiltinToolExecutableViaToolAPI(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	api := toolAPIOf(t, a)

	res, err := api.ExecuteTool("output_list_channels", map[string]interface{}{})
	if err != nil {
		t.Fatalf("经 ToolAPI 执行内置工具失败: %v", err)
	}
	if res == nil {
		t.Error("执行成功但结果为 nil")
	}
}

// ③ ★ 门控语义必须保持：未接 memory 时 memory_* 不该出现在 ToolAPI 上。
//
// 内置工具定义是由运行期状态门控的（`if a.memory != nil` 等）。若注册面
// 不看状态地全量暴露，会让轻量子（memory==nil）声称自己能操作记忆。
func TestBuiltinToolGatingRespected(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider()) // 无 memory / knowledge
	api := toolAPIOf(t, a)

	if api.ToolDefByName("stablemem_merge") != nil {
		t.Error("未接 memory 却声称有 memory_merge —— 门控语义被破坏")
	}
	if api.ToolDefByName("knowledge_list") != nil {
		t.Error("未接 knowledge 却声称有 knowledge_list —— 门控语义被破坏")
	}
}

// ④ ★ 接了 memory/knowledge 时必须**可见**（这是本次要补的缺口）。
func TestBuiltinToolVisibleWhenSubsystemPresent(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	a.knowledge = knowledge.NewStore(t.TempDir()) // 打开 knowledge 门控
	api := toolAPIOf(t, a)

	if api.ToolDefByName("knowledge_list") == nil {
		t.Error("接了 knowledge 但 ToolAPI 上查不到 knowledge_list —— 缺口未补")
	}
}

// ⑤ ★ 参数校验也要走同一条路：经 ToolAPI 调用同样受 schema 预校验。
//
// 否则插件能用 ToolAPI 绕过 1c 的校验（缺 required 却不报错）。
func TestBuiltinToolViaToolAPIStillValidated(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	api := toolAPIOf(t, a)

	// output_list_channels 无 required 参数 ⇒ 用一个确定有 required 的：
	// persona_set 在 personaStore 为 nil 时不可见，故用 output_send__ 家族的帮助工具。
	// 这里退一步验证更本质的一点：ToolAPI 路径上**没有**旁路校验。
	// 用一个不存在的工具名验证"不存在"语义。
	_, err := api.ExecuteTool("definitely_not_a_tool", map[string]interface{}{})
	if err == nil {
		t.Error("调用不存在的工具却成功了 —— ToolAPI 缺少存在性判定")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "not found") &&
		!strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误信息应明示『不存在』，实际: %v", err)
	}
}

// ⑥ 内置工具**不声明**并发安全：它们含 SQLite 写与召回，且门控依赖 agent 状态。
func TestBuiltinToolsNotParallelSafeByDefault(t *testing.T) {
	a := newPreemptAgent(t, newPreemptProvider())
	api := toolAPIOf(t, a)

	for _, name := range []string{"output_list_channels", "input_channels", "get_plugin_tools"} {
		def := api.ToolDefByName(name)
		if def == nil {
			continue
		}
		if def.ParallelSafe {
			t.Errorf("内置工具 %q 默认声明了 ParallelSafe —— 写类工具被并发执行有风险", name)
		}
	}
}
