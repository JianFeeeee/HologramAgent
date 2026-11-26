package sched

// status.go —— 调度器状态面的**本地** DTO 转换。
//
// ★ 这是**驻留子自己的**状态面 DTO 转换。
//
// 主 agent 有 `mainagent/sched.StatusOf`，本包这份独立存在（docs §0.3 铁律）。
// 两份今天实现相同 —— 那是现状；将来分叉时各改各的。

import (
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// StatusOf 把调度器快照转成对外的状态 DTO（不暴露帧内容）。
func StatusOf(s *Scheduler) sdk.SchedulerStatus {
	if s == nil {
		return sdk.SchedulerStatus{}
	}
	snap := s.Snapshot()
	out := sdk.SchedulerStatus{
		ReadyQueueDepth:   len(snap.Queue),
		PendingInterrupts: len(snap.PendingInterrupts),
		SuspendStack:      len(snap.SuspendStack),
		MaxSuspendDepth:   snap.MaxInterruptFrames,
		InterruptsByLevel: snap.Stats.InterruptsByLevel,
		PreemptsByLevel:   snap.Stats.PreemptsByLevel,
		Enqueued:          snap.Stats.Enqueued,
		Executed:          snap.Stats.Executed,
		Rejected:          snap.Stats.Rejected,
		Suspended:         snap.Stats.Suspended,
		Resumed:           snap.Stats.Resumed,
		Backpressure:      snap.Stats.Backpressure,
	}
	// Preempted 是各级抢占成功次数之和，**不是** Suspended：
	// 受害者可能在让位信号生效前就自行结束（有抢占而没有挂起）。
	for lv := LevelBackground; lv <= LevelCritical; lv++ {
		out.Preempted += snap.Stats.PreemptsByLevel[lv]
	}
	if snap.Running != nil {
		out.Running = &sdk.SchedulerTask{
			ID: snap.Running.ID, Level: int(snap.Running.Level), Kind: snap.Running.Kind.String(),
		}
	}
	if snap.Immediate != nil {
		out.Immediate = &sdk.SchedulerTask{
			ID: snap.Immediate.ID, Level: int(snap.Immediate.Level), Kind: snap.Immediate.Kind.String(),
		}
	}
	for lv := LevelBackground; lv <= LevelCritical; lv++ {
		out.InterruptQueues[lv] = len(snap.InterruptQueues[lv])
	}
	for _, f := range snap.SuspendStack {
		if f == nil || f.Task == nil {
			continue
		}
		out.SuspendFrames = append(out.SuspendFrames, sdk.SchedulerFrame{
			Task: sdk.SchedulerTask{
				ID: f.Task.ID, Level: int(f.Task.Level), Kind: f.Task.Kind.String(),
			},
		})
	}
	return out
}
