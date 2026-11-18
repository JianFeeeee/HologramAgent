package agent

import (
	"errors"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
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

// accumulatedTokens 估算**未裁剪**的积累上下文 token 数。
//
// ⚠️ 判据必须用积累量，不能用 f.Msgs：后者已被 buildMessages 按
// targetUsage = 0.8×窗口 裁过，结构上封顶 80%，对任何 >0.8 的阈值都**永远
// 不成立**（这个坑已由 resident.go:checkContextFull 的注释记录过一次，
// 这里抽成函数是为了让两处共用同一口径，避免再次漂移）。
func (a *Agent) AccumulatedTokens() int {
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
//
// ## 必须排除 InactivePages（2026-11-02 实测修复）
//
// 段页式淘汰（P5）把某些**工具页**标成 InactivePages，渲染时整页不出现
// （process.go:597 的 buildTimeline）。但本函数此前仍按全量 ToolResults 统计 ——
// 于是「渲染的少了、统计的没少」：页剔了，水位却不动，L4 空中断反复触发，
// 最终把 overflowRecoverBudget 耗尽并报「上下文超限」。
//
// 生产实例实证（2026-11-02 19:10:45 / 20:58:24）：
//
//	air-interrupt prune: 12→5 events (44185→39714 tokens), ratio=1.21
//
// 事件少了 7 条、页也剔了，token 只降 10% —— 因为被剔页的工具结果仍在算。
// 随后下一轮 ratio 又回到 1.33，预算 2/2 用尽。
//
// ★ 判据口径：**统计视图必须等于渲染视图**。渲染不出的内容不该占预算，
// 否则裁剪永远「裁不动」，这不是保守，是自相矛盾。
func eventTokens(e ContextEvent) int {
	n := kit.EstimateTokens(e.Input) + kit.EstimateTokens(e.Response)
	if len(e.ToolResults) == 0 {
		return n
	}
	// 与 process.go 的 timelineToolLines 用**同一个**页索引函数：
	// 配对规则（CallID 优先、退化到名字、配不上记 0 永不剔除）只能有一份。
	pageOf := toolResultPageIndex(&e)
	for i, tr := range e.ToolResults {
		// 已剔除的页不渲染 ⇒ 不计入预算（渲染视图 = 统计视图）。
		if i < len(pageOf) && containsInt(e.InactivePages, pageOf[i]) {
			continue
		}
		n += kit.EstimateTokens(tr.Output)
	}
	return n
}

// contextWindowTokens 取本 agent 的窗口上限（token）。
func (a *Agent) ContextWindowTokens() int {
	if a != nil && a.provider != nil {
		if max := a.provider.MaxContextTokens(); max > 0 {
			return max
		}
	}
	return defaultMaxContextTokens
}

// overflowRatioNow 返回「积累量 / 窗口」。窗口拿不到时返回 0（表示不判定）。
func (a *Agent) OverflowRatioNow() float64 {
	max := a.ContextWindowTokens()
	if max <= 0 {
		return 0
	}
	return float64(a.AccumulatedTokens()) / float64(max)
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
func (a *Agent) EnforceContextBudget(f *TaskFrame, query string, force bool) (handled bool, pruned int) {
	// ★ 超页处置是**类型注册的钩子**：未注册 ⇒ 本类型不做（整窗型靠
	//   contextfull 上报父 agent 决策，见本文件顶部注释）。
	if a == nil || a.hooks.OverflowHandler == nil {
		return false, 0
	}
	return a.hooks.OverflowHandler(a, f, agenttypes.OverflowRequest{
		Kind: agenttypes.OverflowLocal, Query: query, Force: force,
	})
}

// maybeHandleContextOverflow 保留为发 LLM 请求前的薄封装（历史调用点）。
//
// 它现在只是 enforceContextBudget 的一个调用点，不再持有独立判据。
func (a *Agent) MaybeHandleContextOverflow(f *TaskFrame, query string) (handled bool, pruned int) {
	return a.EnforceContextBudget(f, query, false)
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
func (a *Agent) EnforceResumeContextBudget(f *TaskFrame) (handled bool, pruned int) {
	// ★ 与 EnforceContextBudget 同理：**决策由类型注册的钩子做**，基座只转发。
	//   「恢复时怎么处置」是类型策略（整窗型靠 contextfull 上报父，
	//   策略裁剪型在 standard 里有一份标准实现）。
	//
	// ★★ 这里**必须先判空 f**，且把 query 的推导也一起传下去 ——
	//   因为不同类型的策略对 "query 取什么" 的判断不同（恢复场景要用
	//   本帧最新上下文，不是 f.Input）。传 f 与 query 两者都给策略，
	//   策略自己决定用哪个（standard 用 query，整窗型根本不看）。
	if a == nil || a.hooks.OverflowHandler == nil || f == nil {
		return false, 0
	}
	return a.hooks.OverflowHandler(a, f, agenttypes.OverflowRequest{
		Kind: agenttypes.OverflowResume, Query: resumeQueryFrom(f),
	})
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

// isUpstreamContextFull 判定上游错误是否表示「上下文装不下」。
//
// 它只做**判定**，不做处理：处理一律交给 EnforceContextBudget(force=true)。
// 此前这里叫 handleUpstreamContextFull，自己又调了一遍 handleContextOverflow ——
// 那等于在中断语义之外另造一套裁剪，绕过恢复预算，且与 raise L4 抢占的
// 任务互相抢跑（2026-10-25 实测）。
func isUpstreamContextFull(llmErr error) bool {
	var pe *agentAPI.ProviderError
	return errors.As(llmErr, &pe) && pe.Kind == agentAPI.ErrContextFull
}

// ContextLen 是 nil 安全的上下文条数（基座的**纯读数字**之一）。
//
// ★ 它只报事实，不判超没超 —— 阈值与处置在类型侧。
func (a *Agent) ContextLen() int {
	if a == nil || a.context == nil {
		return 0
	}
	return a.context.Len()
}

// TokenOf 返回第 i 条 L1 事件的 token 成本（基座的**纯读数字**之一）。
//
// ★ 为什么需要它：类型侧的策略（"有没有单帧自撑爆"）要用逐条成本，
// 而它不该为此拿到整个上下文实现的句柄。O(1) 只取那一条，不复制整窗。
//
// 越界 / 无上下文 ⇒ 0（调用方按"这条不存在"处理，不是错误）。
func (a *Agent) TokenOf(i int) int {
	if a == nil || a.context == nil {
		return 0
	}
	n, ok := a.context.tokenAt(i)
	if !ok {
		return 0
	}
	return n
}

// lastUserQueryForPrune 取最后一条非中断用户输入作为裁剪的相关性基准。
//
// 空中断自己没有正文，但它被触发时"刚才在做什么"是有答案的：
// 上下文里最后一条真实用户输入。取不到返回空串（pruneByQuery 按时间保留）。
func (a *Agent) LastUserQueryForPrune() string {
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

// EvictOversizedPages 是**页级剔除原语**：把「单条事件自己就超预算」的那些帧
// 按页标注剔除，返回剔除的页数与是否有缩减。
//
// ★ 归属：这是**上下文原语**（动的是 RelevanceContext 的 InactivePages），
//
//	不是超页策略。整窗型也可能想用它（它的病同样是"单帧撑爆"），
//	而策略裁剪型会在此基础上再叠加"整帧归档"。所以它留在基座，
//	策略（何时调、调完还超怎么办）由类型注册的 EnforceOverflow 决定。
func (a *Agent) EvictOversizedPages(query string) (evicted int, reduced bool) {
	if a == nil || a.context == nil {
		return 0, false
	}
	// query 为空时自己推导（与策略无关的一条通用规则：剔除靠相关性打分，
	// 拿不到 query 就只能按顺序处理，而按顺序乱剔比如不剔更糟）。
	if query == "" {
		query = a.LastUserQueryForPrune()
	}
	if query == "" {
		return 0, false
	}
	budget := a.ComputeTokenBudget().ContextTokens
	if budget <= 0 {
		return 0, false
	}
	a.context.mu.Lock()
	defer a.context.mu.Unlock()
	for _, e := range a.context.events {
		if e == nil {
			continue
		}
		if eventTokens(*e) <= budget {
			continue
		}
		// EvictPages 内部要调 c.save()/computeVector（都需要锁），所以这里
		// 必须用已持锁的形态。为此 evictPagesLocked 自身不取锁。
		n := a.context.evictPagesLocked(e, budget, query)
		evicted += n
		if n > 0 {
			reduced = true
		}
	}
	return evicted, reduced
}

// PruneForQuery 是**裁剪原语**：按 query 的相关性裁一次（走注册的 Prune 钩子）。
//
// ★ 与 PruneByQuery 同一实现，但语义名更明确：它是"因某个 query 触发的裁剪"，
//
//	供类型的超页策略调用（例如"空中断后按最后一条用户输入裁剪"）。
func (a *Agent) PruneForQuery(query string) int { return a.PruneByQuery(query) }
