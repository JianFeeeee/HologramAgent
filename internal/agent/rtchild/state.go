package rtchild

// state.go —— rt 的**类型私有状态**（不占基座格子）。

import ()

// rtState 是 rt 的运行状态（单槽 + 单轮工具上限）。
type rtState struct {
	slot     *rtSlot
	maxTools int
	// notifyParent 是"rt → 父"的主动汇报回调（askparent 的形态）。
	notifyParent func(text string)
}
