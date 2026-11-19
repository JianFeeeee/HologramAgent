package worker

// tools.go —— 工作式子**自己注册**的两个工具（§7.2「基座只按表渲染」）。
//
// ## 为什么注册在这里
//
// `spawn_child` / `child_result` 是**工作式子这个类型的能力**：只有它知道
// "一个异步子任务怎么创建、结果怎么取"。基座只按 `registry` 表渲染 schema、
// 按表分派 —— 与它处理插件工具完全一样。
//
// ★ 这样加一种新类型时不需要动基座：新包自己 `RegisterTool` 即可。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

func init() {
	register("spawn_child",
		"启动一个异步子 Agent 执行独立任务，不阻塞当前对话。完成后系统自动通知你。"+
			"多个互不依赖的子任务可在同一轮里一次 spawn 多个。",
		map[string]interface{}{
			"task": map[string]interface{}{
				"type": "string", "description": "要子 Agent 完成的任务描述（要完整、含必要背景）",
			},
			"max_turns": map[string]interface{}{
				"type": "integer", "description": "子 Agent 最大工具轮数（默认 5，范围 1-30）",
			},
		}, []string{"task"},
		func(a *M, tc agentAPI.ToolCall, ch string) string { return a.SpawnChild(tc, ch) })

	register("child_result",
		"查询异步子 Agent 的执行结果（收到完成通知后调用，只需查询一次）。",
		map[string]interface{}{
			"task_id": map[string]interface{}{
				"type": "string", "description": "spawn_child 返回的任务 ID，如 child_1",
			},
		}, []string{"task_id"},
		func(a *M, tc agentAPI.ToolCall, _ string) string { return a.ChildResult(tc) })
}

// register 把工具登记到统一的工具表（失败即装配错误）。
func register(name, desc string, props map[string]interface{}, required []string,
	h func(*M, agentAPI.ToolCall, string) string) {
	if err := registry.RegisterTool(registry.Tool{
		Name: name, Description: desc, Side: registry.SideParent, Owner: "core",
		Properties: props, Required: required,
		Handle: func(c registry.ToolCtx, tc agentAPI.ToolCall) string {
			a, _ := c.Host.(*agent.Agent)
			if a == nil {
				return "内核装配错误：工具上下文里没有 agent"
			}
			return h(ensure(a), tc, c.Channel)
		},
	}); err != nil {
		panic("agent/worker: " + err.Error())
	}
}
