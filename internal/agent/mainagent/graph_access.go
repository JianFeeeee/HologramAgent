package mainagent

// graph_access.go —— 主 agent 的图记忆访问器。
//
// ## 为什么这两个在类型这边而不是基座
//
// 图库是**主 agent 的私有实现**（`mainagent/memory.GraphDB`，docs §11.2：
// `kernelmod/memory` → `mainagent/memory/`）。基座不认识它，也不该认识 ——
// `GraphMem()` / `GraphDB()` 曾是基座的方法（暴露"这个 agent 有没有图记忆"），
// 而那是**类型语义**（docs §3.2「不许出现的方法」）。
//
// ★ 子 agent 拿到图能力的方式是**调槽内工具**（`graph_recall` / `graph_commit`，
//   由主 agent 注册、内核执行），不是问自己的载体要一个图库句柄。
//   所以这两个访问器只在本包里是 `m.xxx`，没有对应的基座方法。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// graphDB 返回**完整**图记忆面（整理面：蒸馏/归档/合并/场景衰减…）。
//
// nil ⇒ 本实例没有图库（独立工具 / 未装配记忆），所有调用点已有的
// `if g == nil` 关卡继续生效。
func (m *Module) graphDB() kernelapi.GraphMemory {
	if m == nil || m.graph == nil {
		return nil
	}
	return m.graph
}

// graphMem 返回图记忆的**共同面**（三方法：Recall / RecallSorted / Commit）。
//
// 与 graphDB 的差别只在窄/宽：跨模态召回与工具路径只需要三方法，
// 而整理面要完整的库。今天两者是同一个对象，但方法集刻意分开 ——
// 将来给子类型换实现（只读视图）时，窄面仍然成立。
func (m *Module) graphMem() agent.GraphMemory {
	if g := m.graphDB(); g != nil {
		return g
	}
	return nil
}
