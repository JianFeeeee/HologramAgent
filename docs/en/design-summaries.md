# Design summaries (English)

> **Status: summaries only.** Each design document exists in full only in Chinese:
> `docs/zh/*-design.md`. Those are the authority. This file gives an English reader
> enough to know *what problem each design solves* and *whether it is in effect*,
> plus a pointer for the details.
>
> **Why summaries instead of translations**: the design docs total ~7,000 lines. A
> full translation would rot faster than it could be reviewed, and a stale translation
> is worse than none. For content that changes quickly, translate the summary.

---

## kernel-modularization

**Source**: `docs/zh/kernel-modularization-design.md` (1,502 lines)
**Status**: implemented and deployed (designed 2026-10-16, landed 2026-10-24, fully
revised 2026-10-31). This document is the single authority on assembly architecture.

**Problem it solves**: the kernel had 17 serial startup steps. Any module that needed
another module's result had to be started later than it, which produced a hand-maintained
ordering that silently broke whenever a dependency was added.

**Shape**: every kernel module declares `Requires()` (facilities) and `Provides()`
(facilities). A dependency graph is computed **at build time** and embedded in the binary
as `depgraph.bin`. At startup the kernel assembles in layers — independent modules in the
same layer start concurrently. Current production shape: **6 layers / 20 modules, peak
concurrency 4**.

**Two invariants worth knowing**:

1. The dependency graph and the binary must match. The kernel compares the embedded
   graph digest against `depgraph.bin` on disk and **refuses to start** on mismatch
   (`★ 依赖图与本二进制不配套`). This is deliberate: a mismatched graph means the
   assembly order is wrong, and a wrong order produces silent `nil` reads.
2. **`depgraph.bin` lives next to the binary, not in the data directory.** Deploying it
   into the data dir produces a crash loop.

**Key concepts** for reading the code:

- *facility* — a named capability one module provides and others require
- *layer* — computed from the graph; modules in a layer have no dependencies between them
- *`kstate`* — the single mount point for shared data. Other packages mount onto it;
  it never knows about them (leaf package).

---

## secret-firewall

**Source**: `docs/zh/kernel-secret-firewall-design.md` (952 lines)
**Status**: implemented and deployed; architecture converged 2026-10-31.

**Problem it solves**: secrets (API keys, platform credentials) must not reach the model's
context, logs, or disk in plaintext — but plugins legitimately need the real values to make
API calls. So there must be a translation layer at the boundary.

**Shape**: `security` is a **firewall between the peripheral kernel and the core kernel**.
It owns its own data structures and makes its own decisions. Callers supply **facts** only
(`CheckTool(name)`, `ScanContent(text)`) — never a policy, never a "trust me" flag.

**Three properties that matter**:

1. **Aliases, not values.** The model sees `sec_xxx`; the firewall substitutes the real
   value inside the executor, after the argument has left the model's context.
2. **Direction matters.** Inbound (model → plugin) fills aliases. Outbound (plugin → model)
   scrubs any secret accidentally echoed back — e.g. `enc_shell` running `curl` whose
   response body contains the key. Both directions have separate tests because only
   inbound was implemented at first, and the outbound leak was found in production logs.
3. **`Store` delegates encryption to `Codec`, and domains must differ.** Store ciphertext
   uses domain `security/variables`; config-at-rest uses `config-at-rest`. Sharing a domain
   means one leak informs the other.

**Judgement criterion** the design insists on: `CheckPath` / `CheckNetwork` / `CheckTool`
are only real if something in the *execution chain* calls them. A policy that exists but
has no caller is indistinguishable from no policy — and this happened here twice.

---

## input-scheduler

**Source**: `docs/zh/input-scheduler-design.md` (642 lines)
**Status**: implemented and deployed (`internal/agent/core/scheduler.go`).

**Problem it solves**: a single agent receives messages from many channels (chat, CLI,
device bridge) with wildly different urgency. A naive FIFO queue either delays urgent
interrupts behind long tasks, or lets interrupts destroy work in progress.

**Shape**: four interrupt levels (L1–L4) plus a queue for non-interrupting work;
preemption with **state snapshots**, so a suspended task resumes exactly where it stopped
rather than restarting. Interrupts form a **strict LIFO stack** bounded by the number of
levels.

**Invariants** (each has a test):

- L4 does not preempt L4 (same-level interrupts queue rather than nest)
- Stack depth is bounded; exceeding it demotes rather than unbounded recursion
- A suspended task and its resumed counterpart see the same state (`SuspendedEqualsResumed`)
- Tool execution is a **critical section**: interrupts do not cancel mid-tool (side effects
  are not rollback-able). The interrupt is recorded and applied after the tool completes.
- The notify hook fires on push and pop, **while holding the lock** (a hook that takes the
  lock itself would self-deadlock)

---

## resident-subagent

**Source**: `docs/zh/resident-subagent-design.md` (712 lines)
**Status**: implemented and deployed.

**Problem it solves**: some work spans many messages over hours (a project conversation, a
phone call). Stateless sub-agents lose everything between turns; giving every sub-agent a
full agent loop is too heavy.

**Shape**: a resident child has its own input queue, its own two-level memory, and a
parent-child interrupt relationship. Residual tasks left at teardown have four states
(keep / drop / park / return) rather than being silently lost.

**Why it is separate from `rtchild`**: the rt (realtime) child has the *opposite* lifetime
semantics — no task queue, no residuals, a fresh context window per call. They share the
registry skeleton but not the semantics.

**Known trap** (this is why `child.Registry` exists): the singular stop path used to
unregister the inbound channel while the plural path forgot it, so residue accumulated
monotonically with each create/destroy until restart. Both paths now call the same
`Cleanup`.

---

## toolcall-contract

**Source**: `docs/zh/toolcall-contract-and-sequence-design.md` (839 lines)
**Status**: implemented and deployed.

**Problem it solves**: with parallel tool execution, the wire format has hard constraints
that are *not* optional — an `assistant` message declaring `tool_calls` must be followed
**immediately** by one `tool` message per `tool_call_id`. Anything inserted in between
(multimodal attachment, memory recall, a system note) makes providers reject the whole
request with HTTP 400.

**Shape**: the batch executor collects all tool results first, then writes messages
sequentially, then flushes deferred attachments (`flushBatchAttachments`). Nothing may be
inserted inside the batch.

**Measured failure modes** (all three happened):

1. Parallel path appended `ToolResults` but never `ToolCalls` ⇒ results outnumbered calls
2. Budget truncation in L1 rendering cut between `assistant(tool_calls)` and its `tool`
   messages ⇒ dangling calls
3. A rejected/skipped tool recorded a call but not a result ⇒ same mismatch, persisted into
   `context.json` and replayed every turn

The third is the instructive one: **"skipped" ≠ "no result"**. The rejection reason *is* the
result, and the model already saw it. The context copy must match what the model saw.

---

## context-overflow-l4

**Source**: `docs/zh/context-overflow-l4-design.md` (163 lines)
**Status**: implementation record; **terminology in the document is outdated** (it still
says `doc_query` / `doc_commit`, renamed to `coldcontext_*`).

**Problem it solves**: when the context window is exhausted, the two conventional options
are both bad — truncate (lose information) or ask the model to summarize (costly, lossy,
and the model may summarize wrongly).

**Shape**: instead of discarding, low-relevance events are **sunk** into a cold-context
layer, and from there distilled into a graph. Nothing is deleted; it changes storage tier.
Overflow handling recurses to L4 when trimming alone cannot help.

**Why it matters for reading the code**: `InactivePages` in `ContextEvent` is the "this page
is hidden for *this* render" marker. It is deliberately `json:"-"` — hidden pages are a
per-render decision, not persisted state. The constraint is that a *whole* tool call and its
results are hidden or shown together; hiding one half produces the 400 in
`toolcall-contract` above.

---

## Other design docs (Chinese only, not summarized here)

| Document | Scope |
| --- | --- |
| `commondev-design.md` | The `commondev` plugin (multi-repo supervisor) |
| `commondev-agent-runtime-design.md` | Runtime wiring for that plugin |
| `triple-distill-llm-design.md` | Triple extraction/distillation pipeline |
| `triple-extract-attention.md` | Attention weighting for extraction |
| `recall-as-association.md` | Recall as spreading activation rather than lookup |
| `recall-capability-tiers.md` | What each recall tier can and cannot do |
| `multimodal-space.md` | Unified multimodal embedding space |
| `vector-provider-decision.md` | Choice of embedding provider and why |
| `abstention-criterion.md` | When the agent should decline to answer |
| `legacy-issues.md` | Known legacy problems |
| `legacy-table-retirement.md` | Old-table retirement plan (cited by production code) |
| `kernel-performance.md` | Performance measurements and budgets |
| `production-recall-validation.md` | Recall validation on production data |

These are not translated because they are either plugin-specific or data-report shaped —
their value is in the numbers and the local context, both of which the Chinese original
carries adequately.
