package agent

// pluginvirtual.go —— 基座对**虚拟插件管理器**的取用面。
//
// ## 架构位置（用户裁定 2026-11-27）
//
//	真实 plghost（唯一物理事实）= kernelapi.FacPluginRegistry（stage-host 提供）
//	         ↓ 按 agentID 派生
//	虚拟插件管理器（每实例一个，接口与 PluginHost 一致）
//	         ↓
//	基座 a.PluginHost() 返回**本实例**的那一个
//
// ## 为什么按 agentID 从设施取，而不是从父派生
//
// 插件系统是**内核设施**（`FacPluginRegistry` 在 kstate 里），不是父的私有
// 财产。任何实例（含未来新类型）问同一处、带上自己的 ID 即可 —— 零接线。
//
// 「从父派生」要每个建子点各写一遍，新增类型必漏（本仓已有同类教训：
// 提示词段漏接导致子 agent 一条 skill 都看不到）。而**基座一处**就够了。
//
// ## 为什么字段 `pluginReg` 消失了
//
// 它曾是装配期注入的裸句柄 ⇒ 每实例一份指向**同一个全局对象**，等于
// "所有实例看见同一个世界"。装配期只能给根实例注入，子实例拿到 nil
// （四处 `MountSurface(..., nil, ...)`）—— 那些 nil 就是这个设计缺陷的表征。
//
// 现在改为**按 ID 现取**：不存在"谁注入了、谁忘了注入"的问题。
//
// 依赖方向：基座 → kstate → 设施实现（plugin.Registry）。
// 基座不 import `internal/plugin` 的具体类型，只认 `kernelapi.PluginHost`
// 与一个"按 ID 派生"的窄接口 —— 后者由 plugin 包实现。

import (
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// virtualPluginDeriver 是"按 agentID 派生虚拟插件管理器"的窄契约。
//
// ★ 为什么用局部接口而不是 import `internal/plugin`：基座只依赖内核契约。
//
//	实现方（plugin.Registry）天然满足它 —— 结构化类型，无需显式声明。
type virtualPluginDeriver interface {
	VirtualPlugins(agentID string, writable bool) kernelapi.PluginHost
}

// PluginHost 返回**本实例视角**的虚拟插件管理器（未装配 ⇒ nil）。
//
// 可见范围由 plghost 里该 agentID 的授权声明决定（插件/工具三档）。
// 插件加载、卸载、授权变更通过 `EventPluginsChanged` 通知，订阅方重取一次
// 即可拿到新视图 —— 本方法每次现算，不做快照。
func (a *Agent) PluginHost() kernelapi.PluginHost {
	if a == nil {
		return nil
	}
	// 根实例可写（它是唯一调 admin_child 的实例），其余只读。
	return a.pluginViewFor(string(a.id), !a.IsChildAgent())
}

// pluginViewFor 按给定 agentID 取虚拟管理器（基座内部用）。
//
// 空 ID（未初始化的裸实例）⇒ nil：没有身份就没有"谁能看见什么"这个问题。
func (a *Agent) pluginViewFor(agentID string, writable bool) kernelapi.PluginHost {
	if agentID == "" {
		return nil
	}
	ks := kstate.Get()
	if ks == nil {
		return nil
	}
	impl, ok := ks.Require(kernelapi.FacPluginRegistry)
	if !ok || impl == nil {
		return nil
	}
	if d, ok := impl.(virtualPluginDeriver); ok {
		return d.VirtualPlugins(agentID, writable)
	}
	// 设施存在但没实现派生口（测试桩/旧实现）：退回裸读面。
	//
	// ★ 这不是"优雅降级"，是把"一个实例看见全部插件"的旧行为保留给**未装配
	//   授权子系统的环境**（单测）。生产装配下 FacPluginRegistry 一定是
	//   plugin.Registry，走上面的分支。
	if h, ok := impl.(kernelapi.PluginHost); ok {
		return h
	}
	return nil
}
