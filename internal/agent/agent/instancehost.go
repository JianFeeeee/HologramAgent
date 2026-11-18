package agent

// instancehost.go —— 根 agent 实现 `InstanceHost`（§9 C+D ③）。
//
// ## 为什么单独一个文件
//
// 这六个方法是 `*Agent` 对**实例管理器**的出生面。放在这里是为了让
// "实例管理器需要父的什么"在一处可数：
//
//	OwnerID / DataDir / Children / AgentsRegistry / Logf
//	DestroyResident / DestroyRTChild
//
// 其余一切（provider、context、scheduler、mainState…）**都不在面里** ——
// 那正是把 `*Agent` 收成接口的意义：以前它顺手就能读，现在读不到。

import (
	"strings"

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

// DisposeResident 实现 InstanceHost（按类型语义销毁一个驻留子）。
//
// ★ 名字不与既有的 `DestroyResident(id)` 冲突：那个是"父随时可做的销毁"，
//
//	本方法多一个**残余处置决定**（实例管理器的 `DestroyChild` 携带它）。
func (a *Agent) DisposeResident(id, policy string) (string, error) {
	if a == nil {
		return "", nil
	}
	pol := ResidualKeep
	if strings.EqualFold(policy, "drop") {
		pol = ResidualDrop
	}
	_, msg, err := a.FinishResidentWithTarget(id, pol, ChannelOrphan, "", nil)
	return msg, err
}

// DestroyRTChild 实现 InstanceHost。
func (a *Agent) DestroyRTChild(id string) error {
	if a == nil {
		return nil
	}
	return a.StopRTChild(id)
}
