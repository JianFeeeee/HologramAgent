# Changelog

What actually changed in each release. The README only says what it does today; the
history lives here.

Entry points: [README](README_EN.md) · [Architecture](assets/docs/en/ARCHITECTURE.md) · [Future plan](docs/zh/future-plan.md)


**v1.4.x line** (in progress, `main`) — **declarative concurrency safety** + **sequence orchestration (seq)** + **honest tool results**.

- **Same-round parallel tool execution**: multiple `tool_call`s in one round run concurrently,
  **but safety gates it** — a tool must explicitly declare `ParallelSafe` in its own `ToolDef`
  (built-ins use `toolDefOptions`); anything undeclared runs serially, and `Serial` wins.
  **A single unsafe tool degrades the whole batch to serial.** Declarations live **in the tool
  itself**; the kernel keeps no hardcoded name table. Same-channel output stays **strictly ordered**,
  and results are separated from text (`toolOutcome.Text` / `Raw`).
- **`seq` orchestration plugin**: AST parsing + persistence, named groups, variable slots,
  **parallel within a group / serial between groups**, conditional calls (`seq_when_call`),
  cross-sequence call graph with cycle detection, error contract.
- **Tool results are reported, never silently truncated** — over budget emits statistics.
- **Structured tool error contract**: arguments pre-validated against the schema (rejected before
  dispatch), "tool not found" distinguished from "execution failed", `Success` no longer always true.
- **Configurable device command allowlist** (waiter `device_cmd_allowlist`),
  **site drift watcher**, **incremental GUI rendering** (keeps all history, reuses nodes by
  `data-msgkey`; 50/200/400 messages measured at 8.2/23.5/38.2ms).

**v1.3.x line** (v1.3.1–v1.3.13) — **resident sub-agents** + **input scheduler rework**.

- **Resident sub-agents**: the kernel can station lightweight-kernel child agents (their own
  scheduler, their own temp stable memory, sharing the channel registry). The parent dispatches
  and collects work via `resident_agents` (list/create/send/inspect/compress/reclaim/destroy).
  An inputch can be assigned to a child, so input is routed to it **before entering the kernel**.
- **Output channels addressable to a specific agent**: the `AllowedOutputs` grant set
  (consistent across all three filter points) lets parent/child deliver to each other;
  device capabilities became output channels too (one `device/<id>` per device).
- **Input scheduler**: two task classes (queued/interrupt) + four interrupt levels (L1–L4)
  - preempt/suspend/resume/interrupt-stack; same level never preempts same level, with a
  starvation guard and preemption cooldown. L4 belongs only to the kernel and kernel-level
  plugins (e.g. the WebUI stop button).
- **Lightweight kernel profile**: a child's memory surface narrows to "conventional context
  - stable memory" (narrow interface; the main graph opens as a query_only handle, writes go
  to its own temp instance).
- **Backlog timely feedback** (later in the line): when the main agent is busy for a long time,
  the kernel hands queued input to a temporary **triage assistant** — simple items are handled
  directly, items needing the main agent get an immediate "busy, please wait", so users no
  longer wait 10+ minutes in silence.
- Fixed a batch of real defects: inbound inputch (`child/<id>`) registration leak on resident
  destruction, a child's round count always showing 0 (`info()` never filled it), an empty
  child-side childIO (output channels not inherited), device heartbeat pong missing Flush
  (dropping every 60s), and Lua plugin bridge alignment with SDK 1.3.0.

**v1.2.0** — unified multimodal vector space, media promoted to first-class stable-memory nodes, and the whole data plane moved into shared memory.

- **Model-neutral unified embedding space**: the kernel no longer adapts to any specific model.
  It exposes only a public provider SPI (`pkg/embedding`: `Modality` / `Input{Data,MIME}` /
  `Info{Dimension,Fingerprint,Modalities}` + a name registry), with implementations under
  `providers/*`. Default: **Chinese-CLIP ViT-B/16** — text and image land in the **same space**
  (512-dim, fingerprint `cd2a495cf990`, Apache-2.0; measured ~1.59GB peak on load, settling to ~0.89GB steady-state);
  `qwen3vl` is kept (2048-dim, ~9.4GB) for machines with headroom or future video. Text search
  still falls back to the existing word-vector / TF-IDF path — a CLIP dual tower's pure-text
  semantics are **weaker than an MLLM-style embedder**, a cost documented rather than hidden.
- **Media are first-class nodes and edges in the graph DB**: the "index images via generated
  text descriptions" stopgap, `media_refs` and media reference counting are **removed**.
  Memory blocks follow a single-layer invariant — Context → Document → Graph is a **migration**,
  not a copy, and not kept alive by references.
- **The entire data plane goes through shared memory** (tool-call frames, Cleaners, input/output
  lanes, media blocks, document and knowledge bodies); RPC carries only offset descriptors.
  **RPC protocol is now 2**: the fd3 layout changed and there is **no rolling upgrade** —
  kernel and all plugins must be rebuilt and installed together.
- Injections can declare `InjectOptions{NoMemory, ContextPolicy}` (**defaults: still recorded,
  not pruned**); pruning must be requested explicitly and goes through the plugin's registered
  `Cleaner`. SDK 1.2.0 is **purely additive** over 1.1.0.
- **Release packages enable the ONNX space by default** and bundle the model (754MB) plus
  ONNX Runtime (24MB) in the server/full packages; `homed` drops native Windows support in
  favour of WSL2; the jieba dictionary is embedded in the binary.
- Fixed three **silent install-chain failures**: `initconfig` was a no-op (`CGO_ENABLED=0` stub)
  that printed credentials without writing any, fresh installs were misdetected as "already
  configured" so default seeding was skipped entirely (0 plugins installed), and the deb
  `postinst` looked for the unit in the wrong path so `enable` never ran.
- Licensed **AGPL-3.0-only** from this version on (network clause included; statically linked
  plugins must match — see License).

> The historical entries below are kept verbatim to show the evolution; two mechanisms in them
> were **removed in v1.2.0**: text-description-based media indexing, and reference-counted media GC.
>
> **One licensing statement has also changed** (2026-09-24): "statically linked plugins must match"
> in the v1.2.0 entry below **no longer holds**. The SDK is now released under **MIT** (a permissive
> license that does not inherit the kernel's AGPL), so external plugins are **not** derivative works
> of this project: authors choose their own license (closed-source, commercial or private included)
> and are not bound by §13. The original text is kept to show the position at the time.
>
> **Two performance claims also need correcting** (measured 2026-09-20):
> "crash-to-recovery under 1s" does not hold — restarts are **linearly backed off**, i.e.
> 1s / 2s / 3s (`procRestartBackoff=1s × nth crash`). Even the *first* restart waits 1s.
> And the **4th** crash within a 5-minute window stops automatic restarts pending human
> intervention (`procMaxRestarts=3`, tested as `n > 3`).
> The backoff was already 1s when this claim was written (v1.0.0), so it never held.
> "RPC round-trip p50 24.1µs" is the right order of magnitude but does not match current
> measurements: `BenchmarkToolInvoke` on this machine gives inline/small **30.4µs**,
> frame/small 51.5µs, inline/large 767µs, frame/large 398µs.
> The original text is left unedited rather than rewritten, so the history isn't falsified.

**v1.1.1** — Multimodal reaches the **plugin boundary**. v1.1.0 gave the memory system binary
multimedia nodes, but that path was open only to the kernel itself; this release opens it to
plugins and the model. The public SDK gains media fields and three media injection methods
(paired with [SDK v1.1.0](https://github.com/JianFeeeee/hologramagentsdk/tree/v1.1.0),
shared by the whole 1.1.x line), and the kernel implements the four matching RPCs. The bridge
layer had been **silently dropping fields**: `Confidence`/types/`SentenceText` handed in by a
plugin were discarded, `Doc` kept only three fields, and `Remove` never released references
(media stayed "referenced" forever, so GC could never reclaim it). `processTextInput` and
`processMediaInput` were unified into a single `processInput`, which finally gives the media
path the dedup, `no_memory`, channel `Cleaner`, interrupt semantics and correct `EventRawInput`
it had always lacked. Three real defects fixed: **user-sent images never appeared in the WebUI
chat log** (the media path published a map while the subscriber asserted a string),
**`memory_commit`'s `sentence_text` had never been exposed to the model** (note: this tool is now named `stablemem_commit`; the original name is kept for the v1.1.1 release note) (though it is the
mandatory link in the media binding chain), and **two data races in `PluginSDK`** (11 reported
by `-race`; in production this showed up as sporadic nil-dereference crashes during plugin reload).

**v1.1.0** — Memory system supports **binary multimedia nodes**. Content-addressed media store
(CAS + SQLite metadata + on-disk blobs, `Get` always re-verifies the digest) wired through L0
(context events) / L2 (documents) / L3 (graph sentences), with reference-counted GC (referenced
items are never deleted). The description text produced by the vision model is the durable
semantic memory; the blob is only a cache that capacity GC may evict.

**v1.0.0** — External plugins moved from C ABI shared libraries to **subprocess + shared memory**. The first release that no longer loads `.so`/`.dll`, and it is incompatible with 0.9.x (existing plugins must be rebuilt into `plugin.bin` with the new toolchain — called `plugindev` back then, **now `hmapdev`** — though **business code needs zero changes**). Eliminates 6 classes of defects that had caused production incidents: hot-reload silently failing (`DF_1_NODELETE` making `dlclose` a no-op), no crash isolation (a plugin panic took down homed), stage lost updates (35.8~36.8% loss under the copy model), uncancellable cgo timeouts (linear OS-thread leaks), `output_send` reporting false success (the model was told "sent" while the message never went out), and Windows capability degradation (only 3 stage fields visible, no write-back). Three communication planes: stdio JSON-RPC (control) + shared memory segment (data) + event ring (notification); the privilege gradient is now enforced by three explicit gates. RPC round-trip p50 24.1µs; crash-to-recovery under 1s.

**v0.9.0** — C ABI v2: external plugin Stage callbacks can now write back (`invoke_stage` gained a result out-param; plugins may mutate RawMessage/LLMText/ToolResults etc. in OnInput/AfterToolcall/PostAction and have them synced to the core). ABI version now tracks core minor releases (v0.9.x → ABIVersion=2, `version_min=1` keeps old plugins loadable). Also fixes the tool-loop zen-compat placeholder that wrongly fired on first-turn system context tail. The SDK ships an enhanced sanitizer example (bad-UTF-8 / U+FFFD / ANSI-escape scrub across the whole pipeline). **This ABI retired with v1.0.0.**

**v0.8.0** — Core is functional, plugin system enhanced. 20+ built-in plugins. External plugin development via [hologramagentsdk](https://github.com/JianFeeeee/hologramagentsdk) repo. Added input channel `NoMemory`/`Cleaner`, `ChannelDef`, plugin disable/enable system (CLI + WebUI), `plugindev` toolchain C ABI `ChannelDef` support.
