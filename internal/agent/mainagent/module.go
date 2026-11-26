package mainagent

// module.go —— 主 agent 类型的**实现载体**（组合基座句柄 + 本类型状态）。
//
// 组合，不内嵌；需要基座什么就直接调 `m.A.Xxx()`。没有 `Host` 接口、
// 没有映射层。主 agent 是**根**（不是谁的子），所以它的载体由本包自己持有。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/memory"
	"github.com/JianFeeeee/HologramAgent/internal/artifacts"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	mainctx "github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/ctx"
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/sched"
	"github.com/JianFeeeee/HologramAgent/internal/agent/mainagent/services"
)

// Module 是主 agent 类型的实现载体。
type Module struct {
	a *agent.Agent
	// sched 是**本类型自己的**输入调度器实例（类型内部实现，不占基座字段）。
	//
	// ★ 驻留子当前调度行为相同，但那是**当前**的事实 —— 各自持有一份，
	//   任一方改自己的抢占/挂起语义时不用回基座改、也不影响另一方。
	sched *sched.Scheduler

	// st 是本类型自己的私有装配（记忆整理面 / 媒体 / 插件 / 人格 / 预算记账）。
	st *state

	// ctx 是**本类型的上下文实现**（`mainagent/ctx.RelevanceContext`）。
	// 基座不持有它、也读不到它 —— 它只服务本类型的 agentloop 渲染。
	ctx *mainctx.RelevanceContext

	// sv 是本类型的**私有服务集合**（图记忆 / 冷文档 / 文本记忆 / 蒸馏 / 追踪）。
	//
	// ★ 用户裁定：「textmem / coldcontext 本身就是主 agent 的东西」。
	//   它们由本类型自己装配、自己关闭，内核一格都不持有。
	sv *services.Services

	// graph 是本类型的**图记忆句柄**（`*memory.GraphDB`）。
	//
	// ★ 它住这里而不是基座（2026-11-20）：图记忆是主 agent 的私有实现，
	//   基座不认识它（docs §3.2 删掉 GraphDB()/GraphMem()/MainGraph()）。
	//   两条来源在 installState 里归一：显式注入的 cfg.Memory，或自己装的 sv.Memory。
	graph *memory.GraphDB

	// extractor / norm / toolDeclaresNoMemory 是**记忆整理**的纯函数能力，
	// 由装配期注入（主 agent 私有；驻留子/rt 没有整理面）。
	extractor artifacts.Extractor
	// interruptInput 是本轮输入是否为中断（帧事实，本类型自己的记账）。
	interruptInput bool
	norm           artifacts.Norm
}

// Agent 返回组合的基座句柄。
func (m *Module) Agent() *agent.Agent {
	if m == nil {
		return nil
	}
	return m.a
}

// carriers 是**本类型自己**的载体表（按 agent 指针索引）。
//
// ★ 不占基座的格子：基座曾是"全类型登记处"（`subStates` + `SubKeyMain`），
//
//	每加一种类型就要回来加一格 —— 那些格子已删。类型自己持有自己的东西。
var carriers sync.Map // map[*agent.Agent]*Module

// Wrap 取回（不存在则创建）主 agent 的载体。
func Wrap(a *agent.Agent) *Module {
	if a == nil {
		return nil
	}
	if v, ok := carriers.Load(a); ok {
		return v.(*Module)
	}
	// 调度器是**本类型的内部实现**：载体第一次成形时就位，
	// 后续 `InstallScheduler` 只在需要覆盖容量时才重建。
	m := &Module{a: a, st: &state{}}
	m.sched = sched.New(256)
	v, _ := carriers.LoadOrStore(a, m)
	return v.(*Module)
}
