package agent

// overflow.go —— **基座只给数字与原语**（策略全部在类型侧）。
//
// ## 这个文件里有什么
//
//	ContextLen                条数
//	TokenOf(i)                第 i 条的 token 成本
//	AccumulatedTokens         积累量（未裁剪）
//	ContextWindowTokens       窗口上限
//	OverflowRatioNow          积累量 / 窗口
//	EvictOversizedPages       页级剔除原语
//	PruneForQuery             裁剪原语（按 query 走注册的 Prune）
//
// ## 这里**没有**什么（全部搬走了，别再搬回来）
//
//	阈值 1.25 / 恢复预算 2         → mainagent（主 agent 的上下文哲学）
//	HasOversizedFrame             → mainagent（"frame/水页"是 TaskFrame 的抽象）
//	SingleEventFitsBudget         → mainagent
//	OverflowStat / NoteOverflow*  → mainagent.OverflowLedger（记的是"我的策略做了什么"）
//	RaiseContextOverflow / 空中断 → mainagent（类型自己投 self input，基座只透传标签）
//	"超页入口的薄封装"那一族      → 已删（task.go 三处调用点直调 OverflowHandler）
//	"上游错误是不是超页"的判定    → api.IsContextFull（识别归上游调用的发出者）
//
import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
)

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
