package core

import (
	"errors"
	"fmt"
	"log"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
)

// 上下文超页（context overflow）处理。
//
// 背景（2026-10-01 记忆召回跑分 v4 实测）：HA 的 prompt 是锯齿波，峰值达
// 窗口的 7 倍，每轮都在「远超窗口 → 紧急修剪」。根因之一是超页的三条触发
// 路径没有统一出口：
//
//	① 上游 context_full 错误 —— 完全没接，整轮 outcomeFailed（用户在报错里看到它）
//	② 本地积累超限       —— 只在轮首 checkContextFull，且根 agent no-op、一次性
//	③ 子 agent contextfull —— 已在用 L4 上报，但只推信号不携带动作
//
// 本文件把三条路径收敛到 L4 唯一入口（raiseKernelInterrupt），并把「裁剪」
// 做成同步动作 —— 详见 docs/zh/context-overflow-l4-design.md。

// overflowRatio 是触发超页的本地预判水位：积累上下文超过窗口的这个比例就裁剪。
//
// 为什么是 1.25 而不是 1.0：请求体本身被 buildMessages 按 targetUsage=0.8×窗口
// 裁过（见 buildMessages），所以「请求装不下」在本地永不会发生；能观测到的
// 超限信号是**积累上下文**（a.context 里全部事件）。1.25 留了一点余量，避免
// 在水位线附近反复触发。
const overflowRatio = 1.25

// overflowRecoverBudget 是「同一 TaskFrame 内超页恢复次数」的上限。
//
// 为什么需要上限：裁剪的目标是让积累量回到窗口以内，一次通常就够；但若
// topK 与窗口脱钩（max_context_size 是**条数**不是 token，见 pruneByQuery），
// 一次裁剪可能仍不达标。没有上限就会变成「裁剪 → 复原 → 又超页 → 再裁剪」的
// 循环，每次都付一轮任务调度开销。
//
// 超限时**不做静默重试**：保存现场并明确终止，把问题暴露给用户与状态面。
//
// 声明为 var 而非 const：测试要能变异它做判据自证（变异后超预算用例必须变红），
// 将来若要按窗口大小动态给额度（窗口越大越值得多试几次），也不用改结构。
var overflowRecoverBudget = 2

// accumulatedTokens 估算**未裁剪**的积累上下文 token 数。
//
// ⚠️ 判据必须用积累量，不能用 f.Msgs：后者已被 buildMessages 按
// targetUsage = 0.8×窗口 裁过，结构上封顶 80%，对任何 >0.8 的阈值都**永远
// 不成立**（这个坑已由 resident.go:checkContextFull 的注释记录过一次，
// 这里抽成函数是为了让两处共用同一口径，避免再次漂移）。
func (a *Agent) accumulatedTokens() int {
	if a == nil || a.context == nil {
		return 0
	}
	acc := 0
	for _, e := range a.context.Recent(0) {
		acc += eventTokens(e)
	}
	return acc
}

// eventTokens 估算单条上下文事件的 token 占用。
//
// ★ ToolResults 必须计入 —— 这是实测踩过的坑：v4 与 T10 的 prompt 峰值
// （17 万~40 万 token）几乎全部来自工具回灌，而 ContextEvent 里工具输出是
// **独立字段**（ToolResults []ToolResultItem），不算它的话 accumulatedTokens
// 会返回 0，超页判据永远不触发（诊断日志 ratio=0.00/ events=1 就是这么来的）。
//
// resident.go 的 checkContextFull 有同样缺陷，但它只对驻留子生效、一直没暴露；
// 现在两处共用本函数，口径不会再漂移。
func eventTokens(e ContextEvent) int {
	n := EstimateTokens(e.Input) + EstimateTokens(e.Response)
	for _, tr := range e.ToolResults {
		n += EstimateTokens(tr.Output)
	}
	return n
}

// contextWindowTokens 取本 agent 的窗口上限（token）。
func (a *Agent) contextWindowTokens() int {
	if a != nil && a.provider != nil {
		if max := a.provider.MaxContextTokens(); max > 0 {
			return max
		}
	}
	return defaultMaxContextTokens
}

// overflowRatioNow 返回「积累量 / 窗口」。窗口拿不到时返回 0（表示不判定）。
func (a *Agent) overflowRatioNow() float64 {
	max := a.contextWindowTokens()
	if max <= 0 {
		return 0
	}
	return float64(a.accumulatedTokens()) / float64(max)
}

// enforceContextBudget 是**上下文容量保护**的唯一实现（基础设施）。
//
// ## 为什么它是基础设施，而不是某个组件的功能
//
// 上下文会在**每个注入点**增长：新消息、中断注入、任务恢复（中断任务的结果
// 已提交进 a.context，重建出的前缀更大）、以及轮内工具回灌。因此"装不下就裁"
// 必须在这些入口都成立，而不是只在某一条路径上碰巧被调用。
//
// ��史（2026-10-25 前的三个半成品入口，现已全部收敛到这里）：
//
//	task.go 发 LLM 前   maybeHandleContextOverflow（有恢复预算）
//	task.go 上游报错     handleUpstreamContextFull（**直调内部，绕过预算**）
//	resident.go 轮首     checkContextFull（只对驻留子生效，根 agent no-op）
//
// 三个入口、三套判据、三份防护差异 —— 这就是"额外造的一套机制"。
//
// ## force 的语义
//
// force 不是"另一条路径"，只是**判定已被外部短路**：
//
//	force=false  本地水位判定：积累量 > ratio × 窗口
//	force=true   外部已确认超页（如 provider 返回 ErrContextFull）
//
// 之后的一切（恢复预算、裁剪、raise L4）完全相同 —— 这正是"复用语义"的落点。
//
// 返回 handled 表示本帧需要重跑（f.Msgs 已过期，必须重建）；
// 若 f.Terminal == terminalError 则表示恢复预算耗尽，调用方应直接终止。
//
// 幂等：未超页时零开销（一次 token 估算 + 一次比较），可安全地在热路径调用。
func (a *Agent) enforceContextBudget(f *TaskFrame, query string, force bool) (handled bool, pruned int) {
	light := a != nil && a.isLightKernel()
	noCtx := a == nil || a.context == nil
	var ratio float64
	if !noCtx {
		ratio = a.overflowRatioNow()
	}
	if a != nil {
		a.overflowStat.Lock()
		checked := a.overflowStat.Checked
		a.overflowStat.Checked++
		a.overflowStat.Unlock()
		// 诊断留痕：这条判据在每个注入点都会走，而生产上很少触发。
		// 没有这行日志时，「没触发」和「没执行」无法区分（2026-10-01 T10
		// 排查花了 10 轮工具调用才卡在这里）。只记比值与判定，不记内容。
		if checked%20 == 0 {
			log.Printf("[agent] overflow check #%d: ratio=%.2f window=%d events=%d "+
				"(threshold=%.2f lightKernel=%v noContext=%v force=%v)",
				checked+1, ratio, a.contextWindowTokens(), a.contextLenSafe(),
				overflowRatio, light, noCtx, force)
		}
	}
	if noCtx || light {
		// 轻量内核（驻留子）不做按相关度裁剪，见 pruneByQuery 的同款理由。
		return false, 0
	}
	// 唯一判据点：本地水位 OR 外部明确报错。
	if !force && ratio < overflowRatio {
		return false, 0
	}
	if force {
		// 上游报错是一次**独立的上报**（与本地水位分开计数），
		// 计数放在这里而不是调用方：它是判据的一部分，不是调用方的副作用。
		a.overflowStat.Lock()
		a.overflowStat.Upstream++
		a.overflowStat.Unlock()
	}
	// 恢复预算：同一帧内裁完仍超 ⇒ 说明 topK 与窗口脱钩（见 pruneByQuery），
	// 再裁一次也是同样结果。超预算后**明确终止**，不静默重试。
	// 这条防护对所有入口一致生效 —— 包括上游报错那条（此前它绕过了这里）。
	if f != nil {
		if f.OverflowRecover >= overflowRecoverBudget {
			log.Printf("[agent] context overflow: recover budget exhausted (%d/%d), "+
				"terminating this frame instead of looping",
				f.OverflowRecover, overflowRecoverBudget)
			a.markOverflowAborted(ratio, a.context.Len())
			f.Err = fmt.Errorf("上下文超限：已裁剪 %d 次仍超出窗口，"+
				"本轮终止（请检查 core.agent.max_context_size 与窗口是否匹配）",
				f.OverflowRecover)
			f.Terminal = terminalError
			return true, 0 // 终止也是「已处理」：调用方不要再重跑
		}
		f.OverflowRecover++
	}
	return true, a.handleContextOverflow(query, ratio)
}

// maybeHandleContextOverflow 保留为发 LLM 请求前的薄封装（历史调用点）。
//
// 它现在只是 enforceContextBudget 的一个调用点，不再持有独立判据。
func (a *Agent) maybeHandleContextOverflow(f *TaskFrame, query string) (handled bool, pruned int) {
	return a.enforceContextBudget(f, query, false)
}

// enforceResumeContextBudget 是**任务恢复**入口的容量保护。
//
// 与通用 enforceContextBudget 的区别只有两点，但都很关键：
//
//  1. **query 取任务帧的最新有效上下文**，不是 f.Input。
//     被恢复的任务可能已挂起很久，它的 f.Input 与当前对话早已不同步；
//     拿旧 Input 去算相关性，会裁掉"对该任务重要、但字面上不匹配旧输入"
//     的事件——恢复出来的任务反而失去了它自己的现场。
//     这里的做法是取该帧**尾部最后一条有内容的消息**（f.Msgs 的末条），
//     那才是它此刻真正要继续做的东西。
//
//  2. **保护范围是该任务帧自己的尾部**，不是全局上下文的末尾 N 条。
//     PruneWithProtected 默认保护的 c.events 末 N 条是"最近的对话"，
//     而被挂起的任务在现场里可能攒了整批工具结果——那些必须一起保住，
//     否则恢复后 f.Msgs[PrefixLen:] 引用的内容已被归档，任务自相矛盾。
//
// 判据仍是容量（不看声明）：声明式裁剪归 pruneOnInput / stepToolAfter，
// 那是业务策略；这里是"装不下就得裁"的基础设施。
func (a *Agent) enforceResumeContextBudget(f *TaskFrame) (handled bool, pruned int) {
	if f == nil {
		return false, 0
	}
	// 水位判定（与通用入口同一套判据，只是 query/保护范围不同）。
	if !a.overBudgetLocked() {
		return false, 0
	}
	// 恢复预算：同一帧不能无限裁。
	if f.OverflowRecover >= overflowRecoverBudget {
		log.Printf("[agent] resume overflow: recover budget exhausted (%d/%d), terminating frame",
			f.OverflowRecover, overflowRecoverBudget)
		a.markOverflowAborted(a.overflowRatioNow(), a.context.Len())
		f.Err = fmt.Errorf("上下文超限：恢复时已裁剪 %d 次仍超出窗口，本轮终止", f.OverflowRecover)
		f.Terminal = terminalError
		return true, 0
	}
	f.OverflowRecover++

	query := resumeQueryFrom(f)
	// 保护该任务帧自己的现场条数（尾部消息 = 本任务攒下的工具轮产物）。
	protected := len(f.Msgs) - f.PrefixLen
	if protected < 0 {
		protected = 0
	}
	before := a.context.Len()
	a.pruneForResume(query, protected)
	after := a.context.Len()
	if after >= before {
		log.Printf("[agent] resume overflow: no reduction (%d→%d events), not raising L4", before, after)
		return false, 0
	}
	pruned = before - after
	log.Printf("[agent] resume overflow: pruned=%d (%d→%d events), protected=%d frame tail msgs",
		pruned, before, after, protected)
	a.raiseContextOverflow(pruned)
	return true, pruned
}

// overBudgetLocked 是纯判据（无副作用），供各入口复用。
//
// 命名带 Locked 是历史遗留，实际不加锁；调用方在调度器单线程上下文里使用。
func (a *Agent) overBudgetLocked() bool {
	if a == nil || a.context == nil || a.isLightKernel() {
		return false
	}
	return a.overflowRatioNow() >= overflowRatio
}

// resumeQueryFrom 取任务帧的**最新有效上下文**作为相关性查询。
//
// 从尾部往回找第一条有实质内容的消息：恢复时 f.Msgs 的最后一条
// 可能是 tool 结果、continuation 占位或空的 assistant 消息，
// 直接拿它算相关性会退化成无意义查询。
func resumeQueryFrom(f *TaskFrame) string {
	for i := len(f.Msgs) - 1; i >= 0; i-- {
		if c := strings.TrimSpace(f.Msgs[i].Content); c != "" {
			return c
		}
	}
	return f.Input
}

// handleContextOverflow 执行「裁剪 + L4 打断」。
//
// 关键取舍：裁剪在**这里同步做完**，L4 只负责打断与告知。理由：
//   - Prune 全路径无 IO、无 panic（打分→排序→重排→可选写 docStore），
//     把它放进 L4 任务里做只会多付一次完整任务调度开销；
//   - 这样「L4 里 Prune 出错 → 再触发 L4」在结构上就不可能发生。
//
// 出错处理（用户口径：保存退出，不重试）：裁剪 0 条说明「本来装得下」
// 却报超页 —— 这是判定逻辑的 bug，不是可恢复状态。此时保存现场并明确终止，
// 绝不再次触发中断。
func (a *Agent) handleContextOverflow(query string, ratio float64) int {
	// 单条事件本身就超预算 ⇒ 裁剪在任何保护数下都无解（实测 T10c：
	// 事件 40026 token vs 预算 10667）。此时直接终止并报真实原因，
	// 而不是裁剪两轮后 Budget 耗尽 —— 后者会丢失已经裁掉的记忆。
	if !a.singleEventFitsBudget() {
		avg := a.accumulatedTokens() / a.context.Len()
		log.Printf("[agent] context overflow: single event (%d tok) exceeds budget (%d tok) — "+
			"pruning cannot help, terminating", avg, a.computeTokenBudget().ContextTokens)
		a.markOverflowAborted(ratio, a.context.Len())
		return 0
	}

	// ★ 只发空中断，**不在这里裁剪**（2026-10-25 按原始设计修正）。
	//
	// 原设计：L4 空中断把当前任务抢占下来 ⇒ 任务边界处做语义相关性裁剪。
	// 此前这里先 pruneByQuery 再 raise，把中断变成了"裁剪已完成"的回执，
	// 顺序反了（见 raiseContextOverflow 里记录的偏移经过）。
	//
	// 这里返回 0 是如实的：本轮**还没有**裁剪，裁剪会在空中断任务被
	// 调度执行时发生（pruneForAirInterrupt）。调用方若要判断"有没有腾出
	// 空间"，必须看下一轮的水位，而不是这个返回值。
	before := a.context.Len()
	log.Printf("[agent] context overflow: ratio=%.2f events=%d — raising L4 air interrupt (prune happens at task boundary)",
		ratio, before)
	a.raiseContextOverflow(0)
	return 0
}

// markOverflowAborted 记录一次「超页但裁不出东西」的终止，供状态面观测。
func (a *Agent) markOverflowAborted(ratio float64, events int) {
	a.overflowStat.Lock()
	a.overflowStat.Aborted++
	a.overflowStat.LastRatio = ratio
	a.overflowStat.LastEvents = events
	a.overflowStat.Unlock()
}

// raiseContextOverflow 通过 **L4 唯一入口**发起一次上下文超页中断。
//
// 与 panic / child_contextfull 并列，是 L4 的第三个来源。
//
// ★ **它是一次空中断：不带任何消息**（2026-10-25 按原始设计修正）。
//
// 用途是把当前任务抢占下来，好在任务边界做一次**语义相关性裁剪**——
// 中断是触发源，裁剪是它引发的动作，两者不是同一步。
//
// ## 此前为什么被写成了"带文本的通知"（记录一下这个偏移，避免再犯）
//
// 当时的推理是：L4 任务按调度器设计拿不到被打断者的上下文（suspend 的
// D1=B），所以"已裁剪了什么、查不到不等于不存在"必须显式写进 payload，
// 否则模型会把「没检索到」推断成「不存在」（v4 实测过这类回答）。
//
// 那个**观察**是真的，但**结论**错了：它把"补偿 L4 看不到上下文"当成了
// 中断该带正文的理由，于是裁剪被挪到了 raise 之前（先裁完再发一条回执），
// 中断本身退化成一个通知任务——既晚了一步，又给模型塞了它不需要的说明。
//
// 正确的分工是：中断只负责抢占（空），裁剪由任务真正执行时做
// （见 pruneForAirInterrupt）。模型不需要知道"有内容被移走"这件事本身，
// 它需要的是**保留下来的内容与当前问题相关**——那由裁剪质量保证。
func (a *Agent) raiseContextOverflow(pruned int) {
	a.overflowStat.Lock()
	a.overflowStat.Triggered++
	a.overflowStat.LastPruned = pruned
	a.overflowStat.Unlock()

	// 空中断：vectorArgs 不带 Pruned —— 中断不需要知道裁了多少，
	// 那是裁剪侧自己的统计（overflowStat），不是中断的输入。
	a.raiseVector(VectorContextOverflow, vectorArgs{})
}

// isUpstreamContextFull 判定上游错误是否表示「上下文装不下」。
//
// 它只做**判定**，不做处理：处理一律交给 enforceContextBudget(force=true)。
// 此前这里叫 handleUpstreamContextFull，自己又调了一遍 handleContextOverflow ——
// 那等于在中断语义之外另造一套裁剪，绕过恢复预算，且与 raise L4 抢占的
// 任务互相抢跑（2026-10-25 实测）。
func isUpstreamContextFull(llmErr error) bool {
	var pe *agentAPI.ProviderError
	return errors.As(llmErr, &pe) && pe.Kind == agentAPI.ErrContextFull
}

// contextLenSafe 是 nil 安全的上下文条数（诊断日志用）。
func (a *Agent) contextLenSafe() int {
	if a == nil || a.context == nil {
		return 0
	}
	return a.context.Len()
}

// overflowStat 字段定义在 Agent 上（见 agent.go），此处只做访问辅助。

// pruneForAirInterrupt 执行一次因空中断触发的语义相关性裁剪。
//
// ## 为什么裁剪放在这里（而不是发中断之前）
//
// contextoverflow 的原始设计是：L4 空中断把当前任务抢占下来 ⇒ 任务边界处
// 做语义相关性裁剪。中断是**触发源**，裁剪是它引发的**动作**。
//
// 此前实现顺序反了（先 pruneByQuery 再 raise 带文本的中断），于是那个 L4
// 退化成一个"通知裁剪已发生"的回执任务——既晚了一步（裁剪早已完成），
// 又给模型塞了一条它不需要的说明。
//
// ## query 取什么
//
// 空中断本身没有正文，所以拿不到"当前问题"。这里用上下文里**最后一条真实
// 用户输入的文本**作为相关性基准：裁剪要保留的正是与"刚才在做的事"相关的
// 内容。取不到就退化为按时间保留（pruneByQuery 内部对空 query 的处理）。
func (a *Agent) pruneForAirInterrupt() {
	if a == nil || a.context == nil || a.isLightKernel() {
		return
	}
	before := a.context.Len()
	beforeTokens := a.accumulatedTokens()

	a.pruneByQuery(a.lastUserQueryForPrune())

	after := a.context.Len()
	afterTokens := a.accumulatedTokens()
	if after >= before && afterTokens >= beforeTokens {
		// 裁不动：记录下来即可。空中断不承担"必须裁成功"的职责——
		// 真正的超限判定仍在 enforceContextBudget，那里有终止语义。
		a.overflowStat.Lock()
		a.overflowStat.Aborted++
		a.overflowStat.LastRatio = a.overflowRatioNow()
		a.overflowStat.LastEvents = before
		a.overflowStat.Unlock()
		log.Printf("[agent] air-interrupt prune: no reduction (%d→%d events, %d→%d tokens)",
			before, after, beforeTokens, afterTokens)
		return
	}
	a.overflowStat.Lock()
	a.overflowStat.Triggered++
	a.overflowStat.LastPruned = before - after
	a.overflowStat.Unlock()
	log.Printf("[agent] air-interrupt prune: %d→%d events (%d→%d tokens), ratio=%.2f",
		before, after, beforeTokens, afterTokens, a.overflowRatioNow())
}

// lastUserQueryForPrune 取最后一条非中断用户输入作为裁剪的相关性基准。
//
// 空中断自己没有正文，但它被触发时"刚才在做什么"是有答案的：
// 上下文里最后一条真实用户输入。取不到返回空串（pruneByQuery 按时间保留）。
func (a *Agent) lastUserQueryForPrune() string {
	if a == nil || a.context == nil {
		return ""
	}
	evts := a.context.Recent(0) // 值为 ContextEvent（非指针），Recent 已做深拷贝
	for i := len(evts) - 1; i >= 0; i-- {
		if evts[i].Input != "" {
			return evts[i].Input
		}
	}
	return ""
}
