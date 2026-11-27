package sched

// status.go —— 调度器状态面的**本地** DTO 转换。
//
// ★ 只有这一个函数是本包需要的（`resident` 有自己的那份）。
//   原先 standard/api.go 里的 13 个类型别名（Frame/Outcome/Step/…) 是
//   "公共转发层" —— 已删除（docs §0.3 铁律：不允许共享）。

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
