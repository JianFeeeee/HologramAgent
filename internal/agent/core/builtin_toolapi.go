package core

import (
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// 本文件实现内核侧对"内置工具注册面"（方案 B）的注入。
//
// 背景与方案 B 的完整理由见 internal/sdk/tool.go 末尾的注释。
// 一句话：`memory_*` / `knowledge_*` / `doc_*` / `person_*` 这 20+ 个是
// **内核内置**的，在 executeToolCallInner 里按前缀分派，从不进 ToolAPI，
// 于是插件（seq）既查不到也调不了 —— 真机实跑实证：seq_run 报
// 「工具 knowledge_list 不存在或未注册」，而同一轮模型直接调它是成功的。
//
// 注入必须**晚绑定**且**per-agent**：内置工具的可见性由运行期状态门控
// （`if a.memory != nil` / `if a.knowledge != nil` …），而驻留子是轻量内核、
// memory 为 nil。�� ToolAPI 是全局单例，拿不到 agent，只能由 agent 自己
// 提供一份 provider。

// builtinProvider 是 *Agent 上的适配器：把 agent 的内置工具面
// 转成 sdk.BuiltinProvider。
type builtinProvider struct{ a *Agent }

// Defs 返回当前 agent 可见的内置工具声明。
//
// ⚠️ **必须复用 buildToolDefs 的同一批生成逻辑**，否则会出现"两套语义"：
// 一套决定模型看得到什么（buildToolDefs），另一套决定插件看得到什么。
// 这里直接从 buildToolDefs 里筛出**不在** StageHost/IOManager 中的那些，
// 从而保证门控条件（memory/knowledge 是否就绪）完全一致。
func (p builtinProvider) Defs() []sdk.BuiltinToolDef {
	if p.a == nil {
		return nil
	}
	// 先算出"插件/设备侧已有的名字"，剩下的才是内核内置的。
	external := map[string]bool{}
	if p.a.io != nil {
		for _, d := range p.a.io.GetAllTools() {
			external[d.Name] = true
		}
	}
	if p.a.stageHost != nil {
		for _, d := range p.a.stageHost.GetToolDefs() {
			external[d.Name] = true
		}
	}

	var out []sdk.BuiltinToolDef
	for _, raw := range p.a.buildToolDefs() {
		m, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" || external[name] {
			continue
		}
		desc, _ := fn["description"].(string)
		params, _ := fn["parameters"].(map[string]interface{})
		out = append(out, sdk.BuiltinToolDef{
			Name: name, Description: desc, Parameters: params,
		})
	}
	return out
}

// Exec 执行一个内置工具。
//
// 直接复用 executeToolCall 的完整路径（内置分支 + 授权闸 + 异常处理），
// **不复用** executeToolCallInner：后者要求经前缀 switch，而 ToolAPI 的
// 存在性判定已在 ToolDefByName/ExecuteTool 做过一次。
//
// ⚠️ 传入的 toolCall 不带 RawArguments（插件侧没有原始 JSON），
// 因此 __arg_error 的信息面在插件路径上天然缺失 —— 插件调用的是
// **已解析**的参数，不存在被截断的中间态。
func (p builtinProvider) Exec(name string, args map[string]interface{}) (string, error) {
	if p.a == nil {
		return "", errBuiltinNoAgent
	}
	tc := agentAPI.ToolCall{
		ID:        "builtin_" + name,
		Name:      name,
		Arguments: args,
	}
	return p.a.executeToolCall(tc, p.a.defaultChannelForBuiltin()), nil
}

// defaultChannelForBuiltin 给出内置工具执行时的输出通道。
//
// 内置工具本身不产出"用户可见输出"（结果回给调用方），但 executeToolCall
// 的签名需要 channel（如记忆写入会记场景）。用 agent 的调度当前通道不可靠
// （它逐任务变化），故用一个稳定的内部标记。
func (a *Agent) defaultChannelForBuiltin() string { return "builtin" }

// errBuiltinNoAgent 表示 provider 未绑定 agent（装配顺序错误）。
var errBuiltinNoAgent = &builtinErr{"内置工具执行器未绑定 agent"}

type builtinErr struct{ msg string }

func (e *builtinErr) Error() string { return e.msg }

// InstallBuiltinToolProvider 把本 agent 的内置工具面注入 ToolAPI。
//
// 供内核在**创建 agent 之后**调用（bootstrap / resident 创建处）。
// 幂等：重复调用只是覆盖为同一个 agent。
func (a *Agent) InstallBuiltinToolProvider() { a.installBuiltinProvider() }

// installBuiltinProvider 把本 agent 的内置工具面注入 ToolAPI。
//
// 由内核在**创建 agent 之后**调用（bootstrat / resident 创建处）。
// 注入是**全局**的：最后一次注入生效。⚠️ 因此多 agent 场景下，
// ToolAPI 看到的是"最近一个注入者"的内置工具面 —— 这是当前架构的
// 已知局限（ToolAPI 是单例却需要 per-agent 数据）。
// 记入设计文档 §10 待定项，不在本次解决。
func (a *Agent) installBuiltinProvider() {
	sdk.SetBuiltinProvider(builtinProvider{a: a})
}

// isBuiltinToolName 粗判某名字是否可能是内置工具（供提示词/文档用）。
// 真正的判定以 ToolAPI 查询为准（带门控）。
func isBuiltinToolName(name string) bool {
	for _, p := range []string{
		"memory_", "knowledge_", "doc_", "person_",
		"output_", "input_", "resident_", "notify_parent", "persona_set",
	} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
