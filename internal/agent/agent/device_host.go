package agent

// Agent 侧的 **device 数据面适配层**。
//
// ## 为什么适配层在 core 而不是让 device 直接操作 Agent
//
// 工具执行必须**始终**经过 Agent 的完整执行链：
//
//	参数 schema 校验 → 工具授权闸 / 设备授权闸 → 超时与取消
//	→ panic 恢复 → 明文脱敏（出向 + 入向）→ ToolResult 成败判定
//
// 这些是**执行安全**，与敏感信息安全（security 模块）是两件事，
// 都不能被对方顶替（分层规则见 docs/zh/kernel-modularization-design.md）。
// 所以 device 只拿到窄接口
// （api.ToolHost），拿不到 Agent 内部字段，也就绕过不了这条链。

import (
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// ToolDefinitions 实现 api.ToolHost：返回当前可见的工具定义。
//
// ★ 渲染归类型：基座只调类型注册的只读面（`AgentConfig.ToolDefs`）。
//
//	两处各写一份必然漂移 —— 历史上出现过"模型看得到但插件查不到"。
func (a *Agent) ToolDefinitions() []agentAPI.ToolDefinition {
	if a == nil || a.toolDefsFace() == nil {
		return nil
	}
	return a.toolDefsFace()()
}

// ExecuteTool 实现 api.ToolHost：走完整执行链执行一次工具调用。
//
// 通道用内置标记：工具执行本身不产出"用户可见输出"，
// 而记忆写入等场景需要一个稳定的来源标识（见 defaultChannelForBuiltin）。
func (a *Agent) ExecuteTool(name string, args map[string]interface{}) (string, error) {
	if a == nil {
		return "", errNoAgentForDevice
	}
	// ★ 走**同一条执行链**：与本 agent 的模型发起工具调用完全一致
	//   （路由 → 安全闸 → 并发 → 超时/取消 → 恢复 → 脱敏 → 审查）。
	//
	// 曾经这里是 `registry.DispatchTool` 直调：于是经 device/ToolAPI 的调用
	// **绕过了安全闸**（D4 实测缺口：cli 插件的 /terminal 就直接经它调工具）。
	// 现在统一入口，判据 TestCanUseAgreesWithInnerPath 钉住两条路一致。
	results := a.RunToolBatch(a.ctx, []ToolCall{{
		ID: "device_" + name, Name: name, Arguments: args,
	}})
	if len(results) == 0 {
		return "", errNoAgentForDevice
	}
	return results[0].Text, nil
}

// errNoAgentForDevice 表示 device 调用时 Agent 未装配（装配顺序错误）。
var errNoAgentForDevice = &deviceHostErr{"device: agent 未装配"}

type deviceHostErr struct{ msg string }

func (e *deviceHostErr) Error() string { return e.msg }
