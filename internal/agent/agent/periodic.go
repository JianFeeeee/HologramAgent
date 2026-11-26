package agent

// periodic.go —— **定时器设施**（基座提供刻度，类型注册任务体）。
//
// 用户裁定：基座暴露定时器回调接口，而不是在 Start 里硬编码
// distill/archive/merge/review/offload 五个循环。于是：
//
//	基座   结构 + 注册 + 刻度（`StartPeriodic` 统一拉起、带 panic 隔离）
//	类型   任务体（闭包捕获自己的载体）
//
// 基座自己**不注册任何任务** —— 它不知道有谁会用它。

import "time"

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

// PeriodicForLoop 返回**交给内核事件循环**的定时任务清单。
//
// ★ 2026-11-17：定时器不再是 agent 自己的 goroutine，而是内核事件循环的刻度
// （用户裁定「eventloop … 维持定时器」）。agent 只声明"我要什么任务、间隔多久"，
// 由事件循环统一刻度、统一 panic 隔离、统一随 owner 注销。
//
// 为什么保留 RegisterPeriodic（而非直接删）：
// 它是类型模块的**声明面**（各类型在装配点登记自己的任务）；真正的刻度者
// 换成了事件循环。两者分工与"槽位"一致：类型声明，内核推动。
func (a *Agent) PeriodicForLoop() []PeriodicTask {
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
//
// startPeriodic 已**不再启动任何 goroutine**。
//
// 保留函数是为了让 Start 的调用点保持形状；真正的刻度由内核事件循环承担
// （见 PeriodicForLoop 的注释）。留着空实现比"删掉调用点"更安全 ——
// 后者会让"谁负责刻度"这件事在代码里失去唯一落点。
func (a *Agent) StartPeriodic() {}

// ───────────────────────── 内置定时任务（由装配点注册，基座不认识它们） ─────────────────────────
//
// 下面这些**任务体**留在本包只是因为实现的搬迁还没做完（它们随后会随
// distill.go / offload.go 一起搬进 internal/agent/main）。关键点是：
// **基座不再自动启动它们** —— 是否注册、间隔多少，全部由装配点决定。
