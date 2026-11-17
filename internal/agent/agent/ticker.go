package agent

// ticker.go —— 基座的**定时任务回调接口**（用户裁定 2026-11-17）。
//
// ## 用户原话
//
// > 「agent 基类应该暴露一个定时器回调接口用于触发定时任务，而不是你现在这样硬编码」
//
// ## 之前错在哪
//
// 基座的 `Start()` 里硬编码了五个循环：
//
//	go a.distillLoop()    // 上下文裁剪 L1→L2
//	go a.archiveLoop()    // 冷文档归档 L2→L3
//	go a.mergeLoop()      // 实体合并检测
//	go a.reviewLoop()     // 关系复审
//	go a.offloadLoop()    // 积压任务转投
//
// 于是**基座知道了"这个 agent 要做蒸馏/归档/合并/复审/转投"** —— 那是主 agent
// 的职责，而驻留子与 rtchild 跑起来也会白白起五个 goroutine（它们各自的
// 循环体第一行就是 `if a.graphDB() == nil { return }`，也就是"起来了但立刻退出"）。
// 这正是"层层嵌合"在时间维度上的形态：新增一种 agent 类型就要回来给这些
// 启动点加条件。
//
// ## 现在的形态
//
// 基座只提供**能力**，不知道有谁会用它：
//
//	RegisterPeriodic(PeriodicTask{Name, Interval, Run})   // 装配期注册
//	（Start 时统一拉起、统一带 panic 隔离与重启）
//
// 每个类型（含主 agent）在**自己的装配点**注册自己的定时任务。基座里
// 零业务定时逻辑。
//
// ## 与七公共面的关系
//
// 它是"agent loop"（槽位 6）在**时间维度**上的补充：loop 驱动"一轮任务"，
// 定时器驱动"后台维护"。两者都是类型注册的，基座都不认识具体内容。

import (
	"runtime/debug"
	"time"
)

// PeriodicTask 是一个**由类型注册**的定时任务。
type PeriodicTask struct {
	// Name 是任务名（日志与诊断用；也用于去重）。
	Name string
	// Interval 是触发间隔（<=0 视为无效，注册时拒绝）。
	Interval time.Duration
	// Run 是任务体。它会在**自己的 goroutine** 里按 Interval 反复执行，
	// 基座负责 panic 隔离与重启（见 startPeriodic）。
	//
	// ★ 参数是 *Agent：任务体通常需要读本 agent 的上下文/记忆/调度器。
	// 这就是"基座提供基础设施、类型提供行为"的分工。
	Run func(a *Agent)
}

// RegisterPeriodic 注册一个定时任务（**装配期**调用）。
//
// 重复同名会被拒：两个任务同名意味着装配点写错了，而"哪个生效"取决于
// 注册顺序 —— 那是不可判定的故障。
func (a *Agent) RegisterPeriodic(t PeriodicTask) bool {
	if a == nil || t.Name == "" || t.Interval <= 0 || t.Run == nil {
		return false
	}
	for _, existing := range a.periodic {
		if existing.Name == t.Name {
			a.Logf("periodic task %q already registered — ignoring duplicate", t.Name)
			return false
		}
	}
	a.periodic = append(a.periodic, t)
	return true
}

// PeriodicTasks 返回已注册的定时任务快照（诊断/判据用）。
func (a *Agent) PeriodicTasks() []PeriodicTask {
	if a == nil {
		return nil
	}
	return append([]PeriodicTask(nil), a.periodic...)
}

// startPeriodic 拉起全部已注册的定时任务（Start 内部调用一次）。
//
// 每个任务一个 goroutine、自己的 ticker，互相独立：
//
//	· 一个任务 panic 不影响其他任务（各自 recover + 重启）
//	· a.ctx 结束即全部退出（不发散）
//	· 间隔取注册值，基座不做任何"业务上该多久跑一次"的判断
func (a *Agent) startPeriodic() {
	for _, t := range a.periodic {
		task := t
		go a.runPeriodic(task)
	}
}

// runPeriodic 是单个定时任务的主循环（含 panic 隔离与重启）。
func (a *Agent) runPeriodic(t PeriodicTask) {
	defer func() {
		if r := recover(); r != nil {
			// ★ 与原先五个 loop 同一纪律：panic 不终止后台维护，
			// 隔一秒重启（否则一次瞬态故障会让该任务永久停摆且无人知晓）。
			a.Logf("periodic task %q panicked: %v\n%s", t.Name, r, debug.Stack())
			select {
			case <-a.ctx.Done():
				return
			case <-time.After(time.Second):
			}
			go a.runPeriodic(t)
		}
	}()

	ticker := time.NewTicker(t.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.Logf("periodic tick: %s", t.Name)
			t.Run(a)
		case <-a.ctx.Done():
			return
		}
	}
}

// ───────────────────────── 内置定时任务（由装配点注册，基座不认识它们） ─────────────────────────
//
// 下面这些**任务体**留在本包只是因为实现的搬迁还没做完（它们随后会随
// distill.go / offload.go 一起搬进 internal/agent/main）。关键点是：
// **基座不再自动启动它们** —— 是否注册、间隔多少，全部由装配点决定。

// DistillTask 返回"上下文裁剪（L1→L2）+ 插件自动重载"这个定时任务的描述。
func (a *Agent) DistillTask() PeriodicTask {
	return PeriodicTask{Name: "distill", Interval: a.distillInterval, Run: func(x *Agent) {
		x.distillContext()
		x.autoReloadPlugins()
	}}
}

// ArchiveTask 返回"冷文档归档（L2→L3）"。
func (a *Agent) ArchiveTask() PeriodicTask {
	return PeriodicTask{Name: "archive", Interval: a.archiveInterval, Run: func(x *Agent) { x.archiveColdDocs() }}
}

// MergeTask 返回"实体合并检测 + 场景去重"。
func (a *Agent) MergeTask() PeriodicTask {
	return PeriodicTask{Name: "merge", Interval: a.mergeInterval, Run: func(x *Agent) {
		x.detectEntityMerge()
		x.dedupeScenes()
	}}
}

// ReviewTask 返回"关系复审"。
func (a *Agent) ReviewTask() PeriodicTask {
	return PeriodicTask{Name: "review", Interval: a.reviewInterval, Run: func(x *Agent) { x.reviewRelations() }}
}

// OffloadTask 返回"积压任务转投"（未启用或本类型不转投时返回零值，注册被拒）。
func (a *Agent) OffloadTask() PeriodicTask {
	if !a.offload.Enabled || !a.hooks.MayOffload {
		return PeriodicTask{}
	}
	interval := a.offload.BusyAfter / 5
	if interval < time.Second {
		interval = time.Second
	}
	return PeriodicTask{Name: "offload", Interval: interval, Run: func(x *Agent) { x.offloadPendingTasks(x.offload) }}
}

// RegisterMaintenanceTasks 注册**主 agent 的后台维护任务全集**。
//
// ★ 这是装配点的入口（`kernelmod/agent` 与将来的 `internal/agent/main` 调用），
// 基座自己**不**调用它。驻留子/rtchild 不调用 ⇒ 它们一个后台 goroutine 都不多。
func (a *Agent) RegisterMaintenanceTasks() []string {
	var registered []string
	for _, t := range []PeriodicTask{a.DistillTask(), a.ArchiveTask(), a.MergeTask(), a.ReviewTask(), a.OffloadTask()} {
		if t.Name == "" {
			continue
		}
		if a.RegisterPeriodic(t) {
			registered = append(registered, t.Name)
		}
	}
	a.Logf("registered %d periodic task(s): %v", len(registered), registered)
	return registered
}
