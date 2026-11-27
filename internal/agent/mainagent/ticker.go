package mainagent

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
//	go m.distillLoop()    // 上下文裁剪 L1→L2
//	go m.archiveLoop()    // 冷文档归档 L2→L3
//	go m.mergeLoop()      // 实体合并检测
//	go m.reviewLoop()     // 关系复审
//	go m.offloadLoop()    // 积压任务转投
//
// 于是**基座知道了"这个 agent 要做蒸馏/归档/合并/复审/转投"** —— 那是主 agent
// 的职责，而驻留子与 rtchild 跑起来也会白白起五个 goroutine（它们各自的
// 循环体第一行就是 `if m.graphDB() == nil { return }`，也就是"起来了但立刻退出"）。
// 这正是"层层嵌合"在时间维度上的形态：新增一种 agent 类型就要回来给这些
// 启动点加条件。
//
// ## 现在的形态
//
// 基座只提供**能力**，不知道有谁会用它：
//
//	RegisterPeriodic(agent.PeriodicTask{Name, Interval, Run})   // 装配期注册
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
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"time"
)

// DistillTask 返回"上下文裁剪（L1→L2）+ 插件自动重载"这个定时任务的描述。
func (m *Module) DistillTask() agent.PeriodicTask {
	return agent.PeriodicTask{Name: "distill", Interval: m.a.CfgDuration(agent.CfgKeyDistillInterval, 30*time.Minute), Run: func(x *agent.Agent) {
		Wrap(x).distillContext()
		Wrap(x).autoReloadPlugins()
	}}
}

// ArchiveTask 返回"冷文档归档（L2→L3）"。
func (m *Module) ArchiveTask() agent.PeriodicTask {
	return agent.PeriodicTask{Name: "archive", Interval: m.a.CfgDuration(agent.CfgKeyArchiveInterval, 60*time.Minute), Run: func(x *agent.Agent) { Wrap(x).archiveColdDocs() }}
}

// MergeTask 返回"实体合并检测 + 场景去重"。
func (m *Module) MergeTask() agent.PeriodicTask {
	return agent.PeriodicTask{Name: "merge", Interval: m.a.CfgDuration(agent.CfgKeyMergeInterval, 120*time.Minute), Run: func(x *agent.Agent) {
		Wrap(x).detectEntityMerge()
		Wrap(x).dedupeScenes()
	}}
}

// ReviewTask 返回"关系复审"。
func (m *Module) ReviewTask() agent.PeriodicTask {
	return agent.PeriodicTask{Name: "review", Interval: m.a.CfgDuration(agent.CfgKeyReviewInterval, 120*time.Minute), Run: func(x *agent.Agent) { Wrap(x).reviewRelations() }}
}
