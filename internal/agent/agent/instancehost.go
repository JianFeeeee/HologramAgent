package agent

// instancehost.go —— 根 agent 实现 `InstanceHost`（§9 C+D ③）。
//
// ## 为什么单独一个文件
//
// 这六个方法是 `*Agent` 对**实例管理器**的出生面。放在这里是为了让
// "实例管理器需要父的什么"在一处可数：
//
//	OwnerID / DataDir / Children / AgentsRegistry / Logf
//
// ★ 这里**没有** `DisposeResident` / `DestroyRTChild`（2026-11-19 删）：
//   那两格是"基座认识两个具体类型的销毁动作"，正是要消灭的形态。
//   现在销毁一律走 `Descriptor.Admin(action="destroy")` —— 类型自己决定
//   残余怎么处置、通道怎么交代，内核只按 type 查表调用。
//
// 其余一切（provider、context、scheduler、mainState…）**都不在面里** ——
// 那正是把 `*Agent` 收成接口的意义：以前它顺手就能读，现在读不到。

import (
	childpkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent/child"
)

// 编译期确认：根 agent 是实例管理器的宿主。
var _ InstanceHost = (*Agent)(nil)

// OwnerID 实现 InstanceHost。
func (a *Agent) OwnerID() string {
	if a == nil {
		return ""
	}
	return string(a.id)
}

// DataDir 实现 InstanceHost。
func (a *Agent) DataDir() string {
	if a == nil {
		return ""
	}
	return a.dataDir
}

// Children 实现 InstanceHost。
func (a *Agent) Children() *childpkg.Registry {
	if a == nil {
		return nil
	}
	return &a.children
}
