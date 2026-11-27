package mainagent

// tools_private.go —— **本类型私有工具族**的归属声明（用户裁定 2026-11-19）。
//
// ## 判据
//
// 「这个工具读的是不是本类型的私有状态？」
//
//	stablemem_*     → 主 agent 的图记忆（*GraphDB）
//	social_*        → 主 agent 的社交库
//	coldcontext_*   → 主 agent 的冷文档（*coldcontext.Store）
//	doc_*           → 同上（旧前缀，工具已改名，兼容存量调用）
//	person_*        → 同 social（实体/关系工具）
//	persona_set     → 主 agent 的人格库（PersonaStore）
//	toolresult_read → 主 agent 的上下文（超长结果落盘句柄）
//
// 是 ⇒ 声明成本类型拥有，agentloop 自己执行（`ownsTool` 按 Owner 分岔）。
//
// ## 为什么必须声明
//
// 不声明 ⇒ `ownsTool` 返回 false ⇒ 这些工具被丢给内核执行设施 ⇒
// 内核不认识图记忆 ⇒ 工具全废。这正是"归属靠名字前缀猜"的老毛病：
// `stablemem_` 改名时前缀表漏了一处，整族静默失效。
//
// ★ 声明的是**归属**，不是工具定义：schema 在 `tooldefs.go`（发给模型），
//   执行体是 `(*Module, ToolCall) string`（读本类型状态）。注册表里的
//   `ToolCtx.Host` 拿不到那个形状，所以不能塞进 `toolTable`。

import (
	"fmt"

	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// privateToolPrefixes 是私有族的**前缀**（运行时展开成具体工具名声明）。
//
// ★ 前缀只用于"批量声明归属"，不用于执行分岔：分岔只查 `registry.OwnerOf(name)`。
//
//	所以前缀写错不会导致工具静默失效 —— 工具表里没有那个名字，模型看不到它。
var privateToolPrefixes = []string{
	"stablemem_",
	"social_",
	"coldcontext_",
	"doc_",
	"person_",
}

// privateToolNames 是私有族里**名字不遵循前缀**的几个。
var privateToolNames = []string{
	"persona_set",
	"toolresult_read",
}

func init() {
	// 精确名直接声明。
	for _, n := range privateToolNames {
		declarePrivate(n)
	}
}

// DeclarePrivateTools 把**实际渲染进工具表的**名字声明成本类型私有。
//
// ★ 为什么不在 init 里按前缀展开：前缀展开需要知道"这一轮到底有哪些名字"，
//
//	而那取决于运行期状态（图记忆是否就绪、人格库是否接上）。所以由
//	`BuildToolDefs` 的调用者（每轮渲染完）把**真实名字**交回来声明一次 ——
//	幂等，且永远不会声明一个不存在的名字。
func (m *Module) DeclarePrivateTools(names []string) {
	for _, n := range names {
		declarePrivate(n)
	}
}

// declarePrivate 声明一个工具归本类型所有（幂等）。
func declarePrivate(name string) {
	if name == "" {
		return
	}
	if err := registry.DeclareOwnership(name, TypeName); err != nil {
		// 与类型注册同一纪律：两个类型声称同一工具是计划错误。
		panic(fmt.Sprintf("agent/main: 私有工具归属声明失败: %v", err))
	}
}

// IsPrivateToolName 报告某名字是否属于本类型私有族（前缀判定，仅用于渲染侧过滤）。
func IsPrivateToolName(name string) bool {
	for _, p := range privateToolPrefixes {
		if len(name) >= len(p) && name[:len(p)] == p {
			return true
		}
	}
	for _, n := range privateToolNames {
		if name == n {
			return true
		}
	}
	return false
}
