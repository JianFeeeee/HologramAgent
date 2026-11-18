// Package standard is the reusable implementation library for agentloop and
// context management (user ruling 2026-11-17).
//
// ## Positioning: a library, not a module
//
// It does not register a `kernelmod`, does not create dependency-graph edges, and
// is not constructed by any `Init`. It is a **reusable set of implementations**
// that main / resident (and, in the future, other synchronous types) compose.
//
//	type module → standard → agent (stage primitives) → agenttypes / kit
//
// ## Division of labour with kit
//
//	kit        stateless parts (error values, usage ledger, output shell, StageHost, budget math…)
//	standard   standard implementations tied to the Agent lifecycle/bind (loop, context, runner, stage)
//
// ## What is deliberately NOT here
//
// Event routing, output delivery, timer ticks, heartbeats, tool execution —
package standard

import (
	"fmt"
	"log"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
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
//	Agent.HasOversizedFrame()     is any single frame self-oversized
//	Agent.AccumulatedTokens()     how much is accumulated
//	Agent.EvictOversizedPages()   page-level eviction primitive
//	Agent.PruneContext()          pruning primitive
//	Agent.RaiseContextOverflow()  air-interrupt primitive (preempt only, no pruning)
//	Agent.NoteOverflow*()         counters
//
// And the **decision** (which water line, which remedy first, when to give up)
// lives here, registered into Hooks.EnforceOverflow by the type module.
func EnforceOverflowStandard(a *agent.Agent, f *agenttypes.Frame, req agenttypes.OverflowRequest) (bool, int) {
	if a == nil {
		return false, 0
	}
	query, force := req.Query, req.Force
	ratio := a.OverflowRatioNow()
	checked := a.NoteOverflowCheck()
	// Diagnostic trace: this predicate runs at every injection point and rarely
	// fires in production. Without this line "did not fire" and "did not run" are
	// indistinguishable (the 2026-10-01 T10 incident took 10 tool rounds to pin down).
	if checked%20 == 0 {
		log.Printf("[agent] overflow check #%d: ratio=%.2f window=%d events=%d "+
			"(threshold=%.2f force=%v)",
			checked, ratio, a.ContextWindowTokens(), a.ContextLenSafe(), overflowRatio, force)
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
				a.NoteOverflowPruned(pruned)
				return true, pruned
			}
		}
	}

	if !force && ratio < overflowRatio && !a.HasOversizedFrame() {
		return false, 0
	}
	if force {
		// An upstream error is an **independent report** (counted separately from
		// the local water line); counting here rather than in the caller because it
		// is part of the predicate, not a caller side effect.
		a.NoteOverflowUpstream()
	}

	// Recovery budget: if the frame is still over after pruning, then topK and the
	// window are decoupled (see PruneByQuery) and pruning again yields the same result.
	// After the budget is exhausted we **terminate explicitly**, never retry silently.
	if f != nil {
		if f.OverflowRecover >= overflowRecoverBudget {
			log.Printf("[agent] context overflow: recover budget exhausted (%d/%d), "+
				"terminating this frame instead of looping",
				f.OverflowRecover, overflowRecoverBudget)
			a.NoteOverflowAborted(ratio, a.ContextLenSafe())
			f.Err = fmt.Errorf("上下文超限：已裁剪 %d 次仍超出窗口，"+
				"本轮终止（请检查 core.agent.max_context_size 与窗口是否匹配）",
				f.OverflowRecover)
			f.Terminal = agenttypes.TerminalError
			return true, 0 // terminating is also "handled": the caller must not re-run
		}
		f.OverflowRecover++
	}
	return true, handleContextOverflow(a, query, ratio)
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
func handleContextOverflow(a *agent.Agent, query string, ratio float64) int {
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
	if !a.SingleEventFitsBudget() {
		avg := a.AccumulatedTokens() / max(a.ContextLenSafe(), 1)
		log.Printf("[agent] context overflow: single event (%d tok) exceeds budget (%d tok) — "+
			"pruning cannot help, terminating", avg, a.ComputeTokenBudget().ContextTokens)
		a.NoteOverflowAborted(ratio, a.ContextLenSafe())
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
	// when the air-interrupt task is scheduled (see task.go's airInterrupt branch).
	// Callers judging "did we free space" must look at the next round's water line,
	// not this return value.
	log.Printf("[agent] context overflow: ratio=%.2f events=%d — raising L4 air interrupt "+
		"(prune happens at task boundary)", ratio, a.ContextLenSafe())
	a.RaiseContextOverflow()
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
