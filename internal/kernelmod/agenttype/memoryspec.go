package agenttype

// memoryspec.go —— 按 Spec 的 Memory 档位决定**本类型能用什么记忆**。
//
// ## 三档（与内置类型完全对齐，避免插件类型"看起来能用、其实记忆是残的"）
//
//	none   完全不给记忆（worker 同款）
//	light  只经主 agent 交出的 `graph_*` 槽内工具（驻留子 / rt 同款）
//	full   与主 agent 同一个图库句柄（插件作者承担"会不会污染主库"的责任）
//
// ## ★ 结构事实（2026-11-20 用户裁定）
//
// > 「主 agent 注册这些额外工具就是给各个子 agent 提供记忆能力的，
// >    各个子 agent 各自实现的部分直接删除，直接使用主 agent 注册的」
//
// 所以 `light` 档**不再自己开库**：不开只读句柄、不建 temp 实例、
// 不认识 GraphDB / LightMemory / OpenLight —— 它只用主 agent 注册进
// 扩展槽的那五个工具。句柄背后是什么，只有主 agent 知道。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

func applyMemorySpec(cfg *agent.AgentConfig, root *agent.Agent, workDir string, kind memKind) {
	if cfg == nil || root == nil {
		return
	}
	switch kind {
	case memFull:
		// ★ 什么都不注入（2026-11-20 修正）。
		//
		// 图能力一律从**主 agent 注册的槽内工具**来（graph_recall / graph_commit），
		// 由内核执行设施统一调用。往 AgentConfig 里塞图库句柄是"基座认识图库"
		// 的老形态 —— 那正是 docs §3.2 判据要消灭的（`GraphDB()` 不许出现）。
		//
		// 代价：插件类型拿不到"整理面"（蒸馏/归档/合并）。那是设计意图：
		// 整理面属于**主 agent**，插件类型只消费召回与提交。
	case memLight:
		// 轻量档：**什么都不注入**。
		//
		// 图能力从主 agent 的槽内工具来（`graph_open` / `graph_recall` /
		// `graph_commit` / `graph_close` / `graph_export`），本类型只要
		// 调 `Agent.ExtSlotCall` 就够 —— 不需要任何记忆句柄。
		//
		// 之前这里会 `OpenReadOnly` + `OpenLight`（自己开库），那是
		// "子类型各自实现记忆"的旧形态，已删除。
	}
	// memNone：什么都不给（明确的策略，不是漏接线）。
}
