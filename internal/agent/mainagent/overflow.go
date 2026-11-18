// overflow.go —— **主 agent 的上下文超页策略**（原本住在 `standard`，那是错的）。
//
// ## 为什么它必须住 mainagent
//
// 这个文件里没有一件东西是"多个类型共用的标准实现"：
//
//	overflowRatio = 1.25          主 agent 的水位（它自己的上下文哲学）
//	overflowRecoverBudget = 2     主 agent 的恢复预算（它的重试纪律）
//	HasOversizedFrame             "frame / 水页" 是 TaskFrame 的抽象，只主 agent 有
//	SingleEventFitsBudget         同样是它的裁剪无解判定
//	OverflowLedger                记的是"我的策略做了什么"
//	handleContextOverflow         剔页 → 裁剪 → 终止 的具体顺序
//
// 驻留子是整窗不裁、撞窗口上报父；rt 子是滑动窗口、滑无可滑上报父。
// 它们**一条都用不上**这里的任何东西 —— 所以它从来就不是"standard"。
//
// ## 与基座的分工（唯一判据）
//
//	基座    5 个纯读数字（ContextLen / TokenOf / AccumulatedTokens /
//	        ContextWindowTokens / OverflowRatioNow）+ 两个上下文原语
//	        （PruneContext / EvictOversizedPages）
//	本文件  阈值、预算、判定、处置顺序、记账 —— 全部是**策略**
//
// ## 与内核的分工
//
// 内核在两处触发（把请求交给 provider 之前 / 收到 ErrContextFull 之后）；
// 主 agent 另外**自主巡检**（拼消息后、工具回灌后、恢复重建后），因为它要在
// "付代价之前"处置。一个 handler，两种来源。

package mainagent

import (
	"fmt"
	"log"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
)

// overflowRatio is the local pre-judgement water line: prune when accumulated
// context exceeds this fraction of the window.
//
// Why 1.25 rather than 1.0: the request body is already trimmed by buildMessages to
// targetUsage = 0.8×window, so "the request does not fit" never manifests locally;
// what we can observe is **accumulated context**. 1.25 leaves slack so we do not
// retrigger repeatedly right at the water line.
//
// ★ The threshold is a **policy** value, so it lives in the type library, not the
// base. The base only exposes `OverflowRatioNow()` (an observable fact).
const overflowRatio = 1.25

// overflowRecoverBudget is the cap on "overflow recoveries within one TaskFrame".
//
// Why a cap is needed: pruning usually brings the accumulated size back under the
// window in one shot; but if topK and the window are decoupled (max_context_size is
// a **count**, not tokens), one prune may still not be enough. Without a cap this
// becomes "prune → resume → overflow again → prune", paying a full scheduling round
// each time.
//
// When exhausted we **do not silently retry**: we save the scene and terminate with
// a clear message, exposing the problem to the user and the status surface.
var overflowRecoverBudget = 2

// OverBudgetRatio is the "already over the window budget" predicate.
func OverBudgetRatio(a *agent.Agent) bool { return a.OverflowRatioNow() >= overflowRatio }

// PruneByQuery prunes by relevance (strategic-pruning type).
//
// ★ It calls the base **primitive** `PruneContext` (which executes the prune),
// not the entry `PruneByQuery` (which dispatches to the hook and would recurse).
func PruneByQuery(a *agent.Agent, query string, protected int) int {
	return a.PruneContext(query, protected)
}

// ContextBudgetRelevance 是**策略裁剪型**的预算 —— 只有主 agent 用它。
//
// 与整窗型的区别：整窗型直接返回 `b.MaxContext`（只受模型硬上限约束），
// 由驻留子 / rt 各自注册。所以它不能住"共用实现库"。
func ContextBudgetRelevance(_ *agent.Agent, b kit.TokenBudget) int { return b.ContextTokens }

// HasOversizedFrame 报告是否存在「单条事件自己就超过上下文预算」的帧（主 agent 的策略概念）。
//
// SingleEventFitsBudget 用的是**平均值**（全局视角：平均每条是否装得下），
// HasOversizedFrame 看的是**任意一条**是否越过预算。前者判断"裁剪是否无解"，
// 后者是"有没有该走页级剔除的帧"。
//
// ★ 两者住这里而不是基座："frame"、"水页"是主 agent 的**处理抽象**（TaskFrame
// 是 agentloop 的概念），不是基座事实。基座只给 ContextLen 与 TokenOf。
func HasOversizedFrame(a *agent.Agent) bool {
	if a == nil {
		return false
	}
	n := a.ContextLen()
	if n == 0 {
		return false
	}
	budget := a.ComputeTokenBudget().ContextTokens
	if budget <= 0 {
		return false
	}
	for i := 0; i < n; i++ {
		if a.TokenOf(i) > budget {
			return true
		}
	}
	return false
}

// SingleEventFitsBudget 报告「单条事件是否就装不进预算」。
//
// 为什么单独判：保护条数收紧到 1 条仍可能装不下 —— 当单条事件本身就大于
// ContextTokens 预算时（实测 T10c：工具回灌型事件可达 40026 token 而预算
// 只有 10667），裁剪在**任何**保护数下都无解。
//
// 此时正确做法不是继续收紧（收紧也没用），而是让调用方知道「裁剪帮不上忙」，
// 由超页处理走终止路径并报出真实原因，而不是反复裁剪直到预算耗尽。
func SingleEventFitsBudget(a *agent.Agent) bool {
	if a == nil {
		return true
	}
	n := a.ContextLen()
	if n == 0 {
		return true
	}
	avg := a.AccumulatedTokens() / n
	if avg <= 0 {
		avg = 2000 // 与基座默认事件平均 token 同值（无积累样本时的保守估计）
	}
	return avg <= a.ComputeTokenBudget().ContextTokens
}

// EnforceOverflowStandard is the **standard overflow strategy** for the
// strategic-pruning type (main agent; resident children compose it too).
//
// ## Why this lives in `standard` rather than in the base (user 2026-11-18)
//
// > "Overflow shouldn't contain concrete logic — it should be a predefined hook,
// >  and each sub-agent registers that hook to define its own overflow behaviour."
//
// So the base now only exposes **facts and primitives**:
//
//	Agent.OverflowRatioNow()      how full it is
//	Agent.AccumulatedTokens()     how much is accumulated
//	Agent.ContextLen()            how many events
//	Agent.ContextWindowTokens()   the window size
//	Agent.TokenOf(i)              per-event cost
//	Agent.EvictOversizedPages()   page-level eviction primitive
//	Agent.PruneContext()          pruning primitive
//
// And **everything else** — the water line, the recovery budget, "is any single
// frame self-oversized", "does the average event fit", the counters, and issuing
// the preemption — lives in this package, because it is the type's policy.
// HasOversizedFrame / SingleEventFitsBudget / OverflowStat all moved here.
func EnforceOverflowStandard(a *agent.Agent, f *agenttypes.Frame, req agenttypes.OverflowRequest) (bool, int) {
	// 无账本 ⇒ 用进程内默认账本（主 agent 在 Initialize 里注入自己的那份）。
	return EnforceOverflowWith(a, f, req, defaultLedger)
}

// EnforceOverflowWith 是带**策略账本**的版本（类型模块注入自己的 `OverflowLedger`）。
//
// ★ 账本属于策略：`Checked/Triggered/Aborted/Upstream` 每一项都在回答
// "我的策略做了什么"，基座连水位阈值都不知道，无从记这些。
func EnforceOverflowWith(a *agent.Agent, f *agenttypes.Frame, req agenttypes.OverflowRequest, led *OverflowLedger) (bool, int) {
	if a == nil {
		return false, 0
	}
	query, force := req.Query, req.Force

	// ★ 空中断被打断后**在任务边界**做的那次裁剪。
	//
	// 它不走水位判定：它本身就是"上一次判定已经决定要裁"的落实。
	// 相关性基准在这里推导（最后一条真实用户输入）—— 这是主 agent 的
	// 策略知识，基座不替它猜（投空中断时 Query 留空）。
	if req.Kind == agenttypes.OverflowAir {
		if query == "" {
			query = a.LastUserQueryForPrune()
		}
		if pruned := a.PruneContext(query, a.ContextTopK()); pruned > 0 {
			log.Printf("[agent] air interrupt: pruned %d event(s) at task boundary", pruned)
			led.Pruned(pruned)
			return true, pruned
		}
		return false, 0
	}

	ratio := a.OverflowRatioNow()
	checked := led.Check()
	// Diagnostic trace: this predicate runs at every injection point and rarely
	// fires in production. Without this line "did not fire" and "did not run" are
	// indistinguishable (the 2026-10-01 T10 incident took 10 tool rounds to pin down).
	if checked%20 == 0 {
		log.Printf("[agent] overflow check #%d: ratio=%.2f window=%d events=%d "+
			"(threshold=%.2f force=%v)",
			checked, ratio, a.ContextWindowTokens(), a.ContextLen(), overflowRatio, force)
	}

	// Decision point: local water line OR upstream explicitly reported OR
	// **a single frame itself busts the budget**.
	//
	// ★ The third condition was added 2026-11-16. The original predicate was only
	// "global water line >= 1.25×window", yet page-level design targets exactly the
	// "single frame busts the window" disease — it can happen while the global water
	// line is far from the limit (all other events are small, this one is a giant
	// 468-tool-call frame). That predicate kept the page-eviction tier from ever
	// running, dragging the病灶 to when the global size also overflowed — at which
	// point the only remedy left is whole-frame archiving (irreversible).
	//
	// Single-frame bust is "this frame has a water page"; global overflow is
	// "what should have been forgotten was not". Two diseases; if either holds we
	// must enter this function — page eviction only treats the former, the latter
	// still goes through Prune/L4. They do not interfere.
	//
	// ★ 恢复入口（OverflowResume）**先裁一次**、且保护本帧自己的现场：
	//   f.Msgs[PrefixLen:] 是"这个任务刚做完的事"（工具轮产物），按全局末尾
	//   N 条保护会把它们归档掉 ⇒ 恢复出一个自相矛盾的任务。
	//
	//   这与常规入口的差别正是**同一个钩子在不同时机下的不同处置**，
	//   所以它必须在钩子里（策略），而不是基座里写两条分支。
	if req.Kind == agenttypes.OverflowResume {
		if protected := frameTailLen(f); protected > 0 && ratio >= overflowRatio {
			if pruned := a.PruneContext(query, protected); pruned > 0 {
				log.Printf("[agent] resume overflow: pruned=%d, protected=%d frame tail msgs",
					pruned, protected)
				led.Pruned(pruned)
				return true, pruned
			}
		}
	}

	if !force && ratio < overflowRatio && !HasOversizedFrame(a) {
		return false, 0
	}
	if force {
		// An upstream error is an **independent report** (counted separately from
		// the local water line); counting here rather than in the caller because it
		// is part of the predicate, not a caller side effect.
		led.Upstream()
	}

	// Recovery budget: if the frame is still over after pruning, then topK and the
	// window are decoupled (see PruneByQuery) and pruning again yields the same result.
	// After the budget is exhausted we **terminate explicitly**, never retry silently.
	if f != nil {
		if f.OverflowRecover >= overflowRecoverBudget {
			log.Printf("[agent] context overflow: recover budget exhausted (%d/%d), "+
				"terminating this frame instead of looping",
				f.OverflowRecover, overflowRecoverBudget)
			led.Aborted(ratio, a.ContextLen())
			f.Err = fmt.Errorf("上下文超限：已裁剪 %d 次仍超出窗口，"+
				"本轮终止（请检查 core.agent.max_context_size 与窗口是否匹配）",
				f.OverflowRecover)
			f.Terminal = agenttypes.TerminalError
			return true, 0 // terminating is also "handled": the caller must not re-run
		}
		f.OverflowRecover++
	}
	return true, handleContextOverflow(a, query, ratio, led)
}

// handleContextOverflow executes "remedy + L4 interrupt" for the strategic-pruning type.
//
// Key trade-off: page eviction is done **here, synchronously**; L4 only preempts and
// notifies. Reasons:
//   - EvictOversizedPages runs entirely inside the context object (scoring → sorting →
//     reordering), no IO, no panic; putting it in an L4 task would only add a full task
//     scheduling cost;
//   - Thus "Evict throws inside L4 → triggers L4 again" is structurally impossible.
//
// Error handling (user principle: save and exit, do not retry): pruning 0 events means
// "it actually fit" yet overflow was reported — that is a bug in the predicate, not a
// recoverable state. In that case save the scene and terminate explicitly, never
// trigger an interrupt again.
func handleContextOverflow(a *agent.Agent, query string, ratio float64, led *OverflowLedger) int {
	// ★ Page-based: try the **lightest tier** first — page-level eviction (P5, 2026-10-29).
	//
	// Eviction is a mark (InactivePages), not a delete and not a cross-layer migration:
	//   · the data stays in place and can be restored at any time (clearing InactivePages
	//     restores rendering)
	//   · the frame does not move, the layer does not move; only a few tool round-trips
	//     are no longer rendered
	//
	// Why it goes before "whole-frame archiving" (L4 air interrupt): archiving is an
	// irreversible inter-layer migration whose cost far exceeds "render one page less".
	// When eviction suffices we must not touch layers — this is exactly the gain of
	// "paged" over "whole-segment elimination".
	if evicted, reduced := a.EvictOversizedPages(query); evicted > 0 && reduced {
		log.Printf("[agent] context overflow: evicted %d page(s) across frames — "+
			"trimmed (not archived), no layer migration", evicted)
		return evicted
	}

	// Still unsolvable after eviction: a single event is itself over budget
	// (measured T10c: 40026 tokens vs 10667 budget). Pruning is then unsolvable under
	// any protected count, so terminate with the real reason instead of pruning twice
	// and burning the budget — the latter loses memory that was already pruned.
	if !SingleEventFitsBudget(a) {
		avg := a.AccumulatedTokens() / max(a.ContextLen(), 1)
		log.Printf("[agent] context overflow: single event (%d tok) exceeds budget (%d tok) — "+
			"pruning cannot help, terminating", avg, a.ComputeTokenBudget().ContextTokens)
		led.Aborted(ratio, a.ContextLen())
		return 0
	}

	// ★ Raise the air interrupt only; **do not prune here** (2026-10-25, per the
	// original design).
	//
	// The original design: an L4 air interrupt preempts the current task so that
	// semantic relevance pruning can happen at the task boundary. Previously this
	// pruned by query first and then raised, turning the interrupt into a receipt for
	// "pruning already done" — the order was inverted.
	//
	// Returning 0 here is truthful: this round **has not** pruned yet; pruning happens
	// when the air-interrupt task is scheduled (见基座 prepareInputTask 的 self_kind=="air").
	// Callers judging "did we free space" must look at the next round's water line,
	// not this return value.
	log.Printf("[agent] context overflow: ratio=%.2f events=%d — enabling air interrupt "+
		"(prune happens at task boundary)", ratio, a.ContextLen())
	led.PruneRequested()
	InjectSelfAirInterrupt(a)
	return 0
}

// frameTailLen 返回本帧**自己的现场**条数（f.Msgs[PrefixLen:]）。
//
// 0 表示"这不是恢复场景"或没有尾部消息 —— 此时保护一个不存在的尾部没有意义。
func frameTailLen(f *agenttypes.Frame) int {
	if f == nil || f.PrefixLen <= 0 || f.PrefixLen > len(f.Msgs) {
		return 0
	}
	return len(f.Msgs) - f.PrefixLen
}

// max is a tiny helper (Go 1.21+ has builtin max for ordered types; this keeps
// the call site readable without importing anything).
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
