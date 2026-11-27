package resident

// schedstatus.go —— 驻留子自己的调度器状态（**它自己的实例**，不是基座的）。

import (
	own "github.com/JianFeeeee/HologramAgent/internal/agent/resident/sched"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// SchedulerStatus 返回本驻留子的调度器快照 DTO。
func (m *M) SchedulerStatus() sdk.SchedulerStatus {
	if m == nil || m.sched == nil {
		return sdk.SchedulerStatus{}
	}
	return own.StatusOf(m.sched)
}
