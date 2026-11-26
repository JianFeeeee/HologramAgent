package resident

// state.go —— 驻留子的**类型私有状态**（不占基座的格子）。
//
// ★ 形态（用户裁定 2026-11-19）：载体 `M` 自己持有它。
//   基座曾经的 `subStates` 五格 + `SubKey*` 让它成为"全类型登记处"，
//   每加一种类型就要回来加一格 —— 已整体删除。

import (
	"sync"
)

// state 是驻留子的运行状态（inputch 处理表 / contextfull 去重 / 上报通路）。
type state struct {
	// notifyParent 是"子 → 父"的主动汇报回调（L3 中断）。
	notifyParent func(text string)
	// onContextFull 是 contextfull → 父侧内核级事件（L4）。
	onContextFull func()

	// ctxFullMu / ctxFullSignaled 保证 contextfull 只上报一次（防抖动刷屏）。
	ctxFullMu       sync.Mutex
	ctxFullSignaled bool

	// tableMu 保护处理表（子持有，父 pull）。
	tableMu        sync.Mutex
	table          []InputchRecord
	pending        *InputchRecord
	currentInputch string
}
