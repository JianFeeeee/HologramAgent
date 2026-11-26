package mainagent

// toolroute.go —— 工具执行的**分岔点**（用户裁定 2026-11-19）。
//
// ## 用户原话
//
// > 「本身不就应该是具体类型的 agentloop 过滤自己定义的工具后，把剩余的工具
// >    丢给基座，由基座给内核嘛，然后基座再把执行结果返给 agentloop，
// >    agentloop 按照工具结果与对应的标记进行处理，比如 nomemory 等标记」
//
// ## 分岔标准
//
// 只有一条：**这个工具读的是不是本类型的私有状态？**
//
//	是  ⇒ 本类型执行（它持有图记忆 / 冷文档 / 社交库 / 人格库 / 实例管理器）
//	否  ⇒ 基座 → 内核执行设施（插件工具 / 设备工具 / 内核设施类工具）
//
// ★ 判据是**登记的 Owner**，不是名字前缀。前缀表是本仓反复踩过的形态：
//   `stablemem_` 改名成 `memory_` 时两处不同步，整族工具静默失效。
//   现在"谁的工具"由登记时的 `registry.Tool.Owner` 说了算。
//
// ## 为什么返回值形状要一致
//
// 后处理（裁剪 / 召回 / 记忆 / 消息落盘）在 `stepToolAfter` 里，它不该知道
// 工具是从哪条路执行的。所以两条路都返回 `toolOutcome`，差别只在内核那条
// 额外带回**工具声明**（`Tool` 字段），供标记消费。

import (
	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// executeOrDelegateTool 执行一个工具：本类型私有的自己跑，其余交给内核。
func (m *Module) executeOrDelegateTool(tc agentAPI.ToolCall, channel string, turnScenes []string) toolOutcome {
	if m.ownsTool(tc.Name) {
		return m.ExecuteToolCallOutcome(tc, channel, turnScenes...)
	}
	return m.delegateToKernel(tc, channel, turnScenes)
}

// delegateToKernel 把工具交给内核执行设施（经基座 RunToolBatch）。
//
// 单条提交（不是批）：本函数是 agentloop 状态机里的**一个工具一步**，
// 批语义在 `runToolBatch` 那一层（并发判据看的是整批）。
func (m *Module) delegateToKernel(tc agentAPI.ToolCall, channel string, turnScenes []string) toolOutcome {
	sceneCopy := append([]string(nil), turnScenes...)
	results := m.a.RunToolBatch([]agentAPI.ToolCall{{
		ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
	}}, agentPkg.BatchOpts{
		Ctx:     m.a.Ctx(),
		Channel: channel,
		Scenes:  sceneCopy,
	})
	if len(results) == 0 {
		return toolOutcome{
			Text: "内核工具执行设施未就绪（装配错误）：本次工具调用没有被执行",
		}
	}
	r := results[0]
	return toolOutcome{Text: r.Text, Raw: r.Raw, Denied: r.Denied, Tool: r.Tool}
}

// ownsTool 报告该工具是否由**本类型**执行。
//
// 判据是登记表里该工具的 Owner：与本类型名一致 ⇒ 本类型的私有工具。
// 未登记（插件工具 / 设备工具 / 内核设施类工具）⇒ 不是本类型的。
func (m *Module) ownsTool(name string) bool {
	if m == nil || m.a == nil {
		return false
	}
	// ★ 槽内工具**不是**本类型的私有工具（docs §6.2 第 2 条：逻辑在注册时
	//   已交出，内核一视同仁执行）。它们与插件工具走同一条执行链 ——
	//   所以这里必须先排除，否则会落进私有族分岔（那里没有它们的分支）。
	if t, ok := registry.LookupTool(name); ok && t.Slot {
		return false
	}
	owner := m.toolOwnerOf(name)
	return owner != "" && owner == m.ownTypeName()
}

// ownTypeName 返回本类型在登记表里的名字（主 agent = "main"）。
func (m *Module) ownTypeName() string { return TypeName }

// toolOwnerOf 返回工具登记时的归属。
//
// 三处事实来源按序问（都是**登记**，不是名字猜测）：
//
//	① registry.Tool.Owner    内核侧登记（内核设施类工具 / 各类型私有工具）
//	② stageHost.ToolPlugin   插件注册的工具（归属插件名）
//	③ 空                     io 设备工具（它们不声明 Owner）
func (m *Module) toolOwnerOf(name string) string {
	if o := registry.OwnerOf(name); o != "" {
		return o
	}
	if m.st != nil && m.stageHostNow() != nil {
		return m.toolRegNow().ToolPlugin(name)
	}
	return ""
}
