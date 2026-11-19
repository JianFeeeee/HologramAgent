package mainagent

// maintenance.go —— 主 agent 的**后台维护任务注册**（用户裁定，§0 第 12 条）。
//
// ## 为什么不再是 `RegisterMaintenanceTasks` 一个批量入口
//
// 「凡是"基座里写死一批 N 个条目、每个条目代表某种能力/任务"的形态，
//   都要改成**类型注册、基座按需取**」。
//
// 原先 `agent.RegisterMaintenanceTasks` 在**基座**里列了 5 个任务
// （distill / archive / merge / review / offload）。那意味着：
//
//	· 基座认识"蒸馏""归档""合并"这些**业务概念**
//	· 驻留子/rt 只要不调它，就"恰好"没有这些任务 —— 靠调用与否表达类型差异
//	· 想加第 6 个任务必须改基座
//
// 现在：每个任务**独立注册**（`RegisterPeriodic`），注册权属于类型。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
)

// registerMaintenance 逐个注册主 agent的后台任务（每个任务一条 statement）。
//
// ★ 为什么逐个写而不是循环：循环需要一个"任务清单"，而清单就是
//
//	"基座写死一批"的形态本身。逐个注册让"这个类型声明了哪些任务"
//	在代码里可数、可 grep、可逐条删。
func registerMaintenance(a *agent.Agent) {
	var names []string
	reg := func(t agent.PeriodicTask) {
		if t.Name != "" && a.RegisterPeriodic(t) {
			names = append(names, t.Name)
		}
	}
	// ① 上下文裁剪（L1→L2）+ 插件自动重载
	reg(a.DistillTask())
	// ② 冷文档归档（L2→L3）
	reg(a.ArchiveTask())
	// ③ 实体合并检测 + 场景去重
	reg(a.MergeTask())
	// ④ 关系复审
	reg(a.ReviewTask())
	// ⑤ 积压任务转投（未启用时返回零值，注册被拒）
	reg(offloadTask(a))
	a.Logf("registered %d periodic task(s): %v", len(names), names)
}
