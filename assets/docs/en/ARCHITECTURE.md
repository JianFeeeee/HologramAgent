**中文** | [English](../en/ARCHITECTURE.md)

# HomeAgent Architecture

## Architectural Principles

HomeAgent's cognitive architecture consists of three subsystems: the event loop (eventLoop), the context window (RelevanceContext), and the stage pipeline (StageHost). Together they form the orchestration framework. Within this framework, the LLM serves as a scheduled reasoning unit; cognitive continuity is maintained by the event loop, context window, and stage pipeline.

**The event loop (eventLoop)** is a three-way select: `a.io.InputChan()` receives external user input and dispatches to `processTextInput` / `processMediaInput`; `a.selfInputCh` receives internal system tasks (memory merges, distillation callbacks) routed through `processConsolidation` under the `_consolidation_` output channel; `a.ctx.Done()` accepts shutdown signals. A concurrently running `interceptLoop` goroutine independently reads `a.io.InputInterruptChan()` — on receiving a high-priority interrupt, it cancels the in-flight LLM HTTP request (`a.cancelLLM()`), then writes the event to `a.interceptCh`. This channel is drained non-blockingly by `drainInterrupts()` before each LLM call in `process()`, injecting interrupts as `[打断消息]` formatted entries into message history. The three interrupt delivery paths carry distinct semantics: `cancelLLM` terminates the current HTTP request, `interceptCh` injects text before the next LLM turn, and `InjectInput` triggers a new processing cycle when the event loop is idle.

**The stage pipeline (StageHost)** manages two registration categories: tool definitions (ToolDef) and stage handlers (StageHandler). ToolDef includes two optional memory control fields: `NoMemory bool` — when true, the tool's output is excluded from vectorization/jieba/distillation (original text preserved); and `Cleaner func(string) string` — a filter applied before the output enters the computation layer (e.g., extracting a `content` field from JSON). Neither modifies the original output; both only affect the computation layer input. `RegisterTool` rejects duplicate names, infers the owning plugin name from the tool name prefix, and maintains a `toolPlugins` mapping. `RegisterStage` appends handlers to the corresponding stage list. On stage execution (`RunStage`), **all registered handlers execute in parallel via goroutines**, sharing a single `*StageContext` protected by `sync.RWMutex`. Individual handler panics are recovered independently without affecting other handlers. Short-circuit semantics are implemented by checking `ctx.Response != nil` — any stage handler can set this value to terminate the pipeline early. `ExecuteTool` includes built-in panic recovery with stack-trace recording. `UnregisterPluginTools` removes a plugin's tool set during hot-reload.

**The context window (RelevanceContext)** maintains a chronologically ordered event list. `Append` aggregates tool outputs through `textForVector` before computing the embedding vector: `NoMemory` skips, `Cleaner` filters (per-tool cleaners registered by plugins — e.g. the QQ plugin strips its own tool-call templates), and `CleanText` finalizes with basic whitespace normalization. A three-branch strategy selects the text source (agent events use Response, user events use Input, cold_storage uses Input+Response). `Prune` triggers when the event count exceeds `topK`: it **unconditionally protects the last 10 events from eviction** (recency bias), scores remaining candidates against the current input via CosineSimilarity, keeps `topK - 10` highest-scoring entries (floor at 0), then re-sorts chronologically. Pruned events from sources other than `agentcli` and `terminal` are archived to the Document layer via `docStore.ContextToDoc`, retaining original timestamps. Persistence uses 5-second debounced writes to a JSON file.

**Tool definitions are aggregated from five sources**: IOManager-registered plugin tools; StageHost-registered SDK tools; Indexer-provided memory index tools; conditionally added built-in tools (depending on non-nil state of memory/knowledge/docStore/social/pluginReg/providerManager modules — including memory operations, knowledge retrieval, document queries, social networking, plugin reloading, child-agent spawning, per-output-channel send tools, and LLM source switching); and media processing tools added based on `pendingMedia` state. `buildToolDefs()` re-aggregates all sources on each process cycle.

**Provider invocation follows an ordered fallback strategy**: `ProviderManager.OrderedProviders()` returns the provider list in registration order. The `process()` inner loop iterates this list attempting `Chat()` on each. HTTP 401/403 responses mark the provider as permanently unavailable; other error types also mark unavailability but with higher tolerance. If all providers fail, an error is returned to the caller. If a call is interrupted by context cancellation while the agent is still running, it is retried (only on non-consolidation paths).

**The memory system adopts a three-tier storage hierarchy (Context → Document → Graph), tiering data by access locality and persistence requirements**: the Context layer is a fast-volatile working window using StaticEmbedder (pretrained word embeddings with TF-IDF fallback) for semantic relevance scoring; the Document layer **shares the same StaticEmbedder vector space with Context** (the embedder is injected into the Document Store at agent startup via `docStore.SetVectorizer(embedder)`), ensuring that relevance scores during Context pruning and semantic retrieval during Document queries operate within the same vector space — TF-IDF serves only as a fallback when the embedder is unavailable; the Graph layer uses SQLite as its persistence substrate with an entities table (nodes) and a relations table (directed edges), supporting BFS traversal recall. Data migration policies govern movement across tiers: low-scoring events sink from Context to Document (vectorized using the same embedder at archival time); cold documents, after a 72-hour no-access threshold, are distilled into triples via `docToTriples` and committed to Graph. The Indexer uses dual retrieval (entity vector similarity search + jieba keyword extraction) to construct Graph query seeds, and the `MarkRecalled` mechanism prevents entities already fetched via tool calls from being re-injected into the system prompt.

**The separation of core domain and application domain** constrains the kernel's responsibilities to LLM orchestration, memory management, and knowledge retrieval — no direct IO operations; all external interaction is mediated through the plugin domain. This separation limits the kernel's complexity to a verifiable scope while granting the plugin domain independent evolution: plugins can be independently developed, independently released, hot-loaded, and do not directly affect the stability of the core domain.

## Message Processing Flow

### Full Pipeline

```
External input (via plugin InjectInput)
        │
        ▼
eventLoop() → processTextInput()
        │
        ├── on_input stage          Plugins can intercept/rewrite/short-circuit
        ├── Context.Append          Record to context window
        ├── Context.Prune           Low-relevance events archived to Document
        ├── buildMemoryContext()    Indexer recall → GraphDB BFS traversal
        │
        ├── pre_action stage        Plugins can inject system messages
        │
        ├── [Tool Loop] process()
        │   ├── buildSystemPrompt   Persona + Memory + Knowledge + Context
        │   ├── buildToolDefs       Built-in tools + Plugin tools
        │   ├── provider.Chat()     LLM call
        │   ├── post_action stage   Plugins see LLM output + tool list
        │   ├── Has tools?
        │   │   ├── before_toolcall Plugins can reject/modify params
        │   │   ├── executeToolCall Route to plugin/built-in
        │   │   ├── after_toolcall  Plugins can modify results
        │   │   └── → back to post_action
        │   └── No tools → exit loop
        │
        ├── Context.Append(response)
        ├── before_output stage     Plugins can modify final text
        ├── emitResponse()          Send via output_send
        └── after_output stage      Read-only, cleanup
```

Code: `internal/agent/core/process.go` — `process()` is the main tool loop

### 7 Stage Hooks

| Stage | Trigger | Plugin Capabilities |
|-------|---------|---------------------|
| `on_input` | Message arrives at Agent, zero processing | Blacklist/rate-limit/short-circuit reply |
| `pre_action` | Context ready, before LLM call | Inject external data into context |
| `post_action` | LLM returns text + tool list | Sensitive word filter/forced redirect |
| `before_toolcall` | Before single tool execution | Audit/reject/modify params |
| `after_toolcall` | After single tool execution | Desensitize/sort results |
| `before_output` | Final text ready, before sending | Format adaptation |
| `after_output` | Already sent | Statistics/logging |

Code: `internal/agent/core/stages.go` — `StageHost` orchestration

### Loop Rules

`post_action → [before_toolcall → execute → after_toolcall] → post_action` forms the inner loop.
Exit conditions: LLM has no tool calls / all rejected / exceeded limit.

### Short-Circuit Rules

Setting `ctx.Response` at any stage jumps to `after_output`.


## Three-Layer Memory

### Memory Flow

```
① Context (Working Window)
   RelevanceContext — In-memory events[] + JSON persistence
   Append: Each input, CleanTemplateText → three-branch vector(textForVector)
           agent→Response, user→Input, cold_storage→Input+Response
           Vector layers: unified multimodal space (primary, with fingerprint) → StaticEmbedder word embedding → TF-IDF (fallback)
   Prune:  DenseCosine (compared only within the same fingerprint) → StaticEmbedder CosineSimilarity fallback; keep topK + last 10
       ├── Keep → timeline → chronologically sorted → system prompt
       └── Low score → Document layer archive (original timestamp)
   Save: 5s debounce write to disk

         ↓ Prune archive                       ↑ LLM active recall

② Document (File Memory)
   DocStore — JSON files + dense vectors (unified multimodal space; dense_fp must match the current space fingerprint or the doc is recomputed; fallback: StaticEmbedder / TF-IDF InvertedIndex)
   Write: Prune archive / doc_commit / Graph snapshot (syncGraphToDocs)
   Read:
       ├── Auto-inject: Query(input, top3) → similarity summary under same vector space → [Related Memory Docs] → system prompt (read-only)
       └── LLM active:  doc_query → Consume(read and delete)
                         → context.Append{Timestamp: d.CreatedAt, Source: "cold_storage"} per doc
                         → Docs written to context timeline with original timestamps, deleted from docStore
   Cold: FindColdDocs(72h, ≤2 accesses) → docToTriples → Graph

         ↓ Cold doc distillation               ↑ Auto recall

③ Graph (Graph Database)
   SQLite — entities + relations tables
   Write: memory_commit / cold doc distillation / Pipeline rule distillation / memory_merge
   Read:
       ├── Auto recall: Indexer.BuildContext(input)
       │     → CleanTemplateText → vector entity search + jieba keywords → SQLite LIKE + BFS depth=2
       │     → [Memory Index] → system prompt
       └── LLM active: memory_recall / memory_merge / memory_purge / memory_edit / memory_delete_entity
   Social: person_query / set_trait / relate (wraps GraphDB)

④ Four Independent Heartbeat Loops (separate tickers and config intervals)
   distillLoop  (distillInterval,  default 30m):  Context pruning — Context.Prune → Document
   archiveLoop (archiveInterval, default 60m):  Cold doc archival — docToTriples → GraphDB
   mergeLoop   (mergeInterval,   default 120m): Entity merge detection — similarity → LLM decision
   reviewLoop  (reviewInterval,  default 120m): Relation review — SentenceRef recall → LLM fix

⑤ Pipeline Rule Distiller (every 10min heartbeat)
   distillOnce → regex match personal info:
     我叫X / 我住在X / 我喜欢X / 我X岁 / 我的工作是X
     → triples → GraphDB.Commit
```

### Vectorization: Unified Multimodal Space (primary) → Word Embedding → TF-IDF (fallback)

Vectorization degrades through three layers by availability; **each missing layer reports an explicit
error and never pretends to succeed**:

**① Unified multimodal space (primary path, since v1.2.0)**
Text and images share **one model, one dimension, one fingerprint** (default `chineseclip`: 512d,
Apache-2.0, Chinese-native; `qwen3vl` or an external `http` provider are alternatives).
Providers register through the public `pkg/embedding` SPI — **the kernel hardcodes no model**.
Vectors persist together with their fingerprint (`dense_fp` / `vec_model`); any mismatch with the
current fingerprint triggers recomputation, and only blocks with the **same fingerprint and the same
dimension** participate in fusion (mixing coordinate systems yields a direction resembling neither).

**② Word embedding (text fallback)** — `StaticEmbedder` (`internal/memory/static_embedder.go`):

**Model sources** (aligned 300d)
- Model sources: ConceptNet Numberbatch (77-language aligned) / fastText Chinese / fastText English
- Configured via `core.agent.embedding_model_path` (comma-separated multi-model)
- Path containing `numberbatch` → auto-download ConceptNet; `cc.zh.` → fastText Chinese; `cc.en.` → fastText English
- Falls back to ConceptNet by default if no match
- **Pre-processing**: plugins register per-tool `Cleaner` functions; `textForVector` applies them before `CleanText` final normalization
- **Three-branch vector source**: agent→Response, user→Input, cold_storage→Input+Response
- **TF-IDF fallback**: auto-fallback to bag-of-words TF-IDF if model download fails or not configured

| Location | File | Purpose | Algorithm |
|----------|------|---------|-----------|
> The table below pins **function names**, not line numbers: lines drift on every edit,
> function names are the stable anchor. (The old version cited `context.go:155` /
> `document.go:206` / `indexer.go:96+111`; after the memory reorg `context.go` moved to
> `agent/core/`, so those line numbers had long been stale.)

| Location | File (function) | Purpose | Algorithm |
|----------|------|---------|-----------|
| Context Prune | `internal/agent/core/context.go` — `RelevanceContext.Prune` / `PruneWithProtected` | Trim low-relevance context events | VectorizeClean → CosineSimilarity(queryVec, evt.Vector) |
| DocStore Query | `internal/memory/document/document.go` — `Store.Query` / `QueryScored` | Recall from document memory | StaticEmbedder.Vectorize (primary) / TF-IDF (fallback) → vec.Search |
| Indexer Entity Search | `internal/memory/indexer.go` — `Indexer.vectorSearchEntities` | Recall from Graph | vector entity search + jieba keywords → SQLite LIKE + BFS |
| Entity Similarity Detection | `internal/agent/core/distill.go` | Detect similar entities in Graph | Weighted Jaccard (`sim > 0.75` → consolidation) |

### Context Layer

`internal/agent/core/context.go` — `RelevanceContext`
- Maintains recent event list, writes JSON on each Append/Prune to prevent data loss
- Pre-vectorization pipeline: per-tool `Cleaner` functions strip template noise, then `CleanText` for basic whitespace normalization
- Three-branch `textForVector`: agent events → Response, user events → Input, cold_storage → Input+Response
- Pretrained word embedding `StaticEmbedder` → CosineSimilarity, auto-fallback to TF-IDF if unavailable
- Protects last 10 events from eviction; excess candidates are sorted by relevance and archived to document memory
- Archived events retain original timestamps; on `doc_query` recall they re-insert into the context timeline at their original position

### Document Layer

`internal/memory/document/document.go` — `Store`
- Consume-on-read mode: deleted after `doc_query` retrieval
- Dual recall: shared StaticEmbedder semantic vector search + jieba keyword extraction (falls back to char-bigram TF-IDF when model is not loaded)

### Graph Layer

`internal/memory/graph.go` — `GraphDB`
- SQLite WAL mode, two tables (driver: mattn/go-sqlite3, CGo)
- `Commit(triples)` — UPSERT entities + INSERT relations
- `Recall(keywords, depth)` — Keyword LIKE search + BFS traversal

### Memory Tools (LLM-callable)

| Tool | Purpose |
|------|---------|
| `memory_recall` | Recall from Graph |
| `memory_commit` | Write triples to Graph |
| `memory_merge` | Merge two entity nodes |
| `memory_purge` | Delete entity node |
| `memory_edit` | Edit existing entity/relation |
| `memory_delete_entity` | Delete entity and all its relations |
| `memory_introspect` | View memory statistics |
| `doc_query` | Search from Document |
| `doc_commit` | Write to Document |

Since v1.1.1 `memory_commit` and `doc_commit` accept `media_digests`, and the kernel appends the
`[<mime> <short digest>] <description>` marker into the sentence/body — **the kernel builds the
marker, the model only supplies the digest**. Requiring the caller to know the format would mean a
single typo silently breaks reference binding with no error anywhere in the chain. `memory_commit`
also gained `sentence_text`: media references hang off a sentence, so with no sentence there is
nowhere to attach them.

### Media Memory (block union: media *is* a block)

Media is neither attached content nor a separate storage layer — it is **one of the values a block
can take**. `MemoryBlock` is a union: a text block carries its payload in `Text`, a media block in
`Data` (plus MIME/width/height/size). The bytes live and die with the block — same row, same block
value, with no independent addressing layer.

> ★ 2026-10-11 CAS removal: media bytes used to live in a separate **content-addressed store**
> (`internal/memory/media/`, with `media.db` + `blobs/`, a `ref_count` ledger, a second vector set
> and a second retrieval path `QueryMediaScored`), with blocks holding only a digest pointing at it.
> Two stores inevitably demand patches (`crossmodal.go` needed `heldMediaDigests()` to stop
> retrieving "media with nowhere to belong"), and the dual digest/block-ID identity made
> cross-layer references easy to misalign. That layer was removed wholesale: media travels with
> the block, and the block ID *is* the media's identity.

| Concern | Approach | Why |
|---|---|---|
| Payload ownership | Bytes in `memory_blocks.data` (BLOB), living and dying with the block | A block carries either text or bytes; there is no byte store outside the block |
| Block identity | **Content-derived** `blk_media_<sha256(trim(mime)+\0+data)[:12]>` | Same content ⇒ same ID ⇒ `ON CONFLICT` dedupes naturally (the benefit CAS once provided, now supplied directly by the block ID) |
| Retrieval | Blocks carry **their own multimodal vector and fingerprint**, participating directly in `RecallBlocks` vector search | Pure vector retrieval never touches payloads; a payload is fetched on demand via `BlockData` only after a block is selected |
| Fetching bytes | `BlockData(blockID)` loads one block on demand; plugins call `Memory().MediaBytes(ref)` with a block reference | `blockColumns` deliberately excludes `data`: including payloads would read tens of MB on every full-table recall |
| Lifecycle | **No separate GC, no refcounts, no keep-set**; deleting the block deletes the content | Media is a memory block, not a cache that needs keeping alive |
| Knowledge media | Bytes travel as data carried by the entry, stored in the entry directory (beside `content.md` and `.media.json`) | `Remove`'s `RemoveAll` cleans it up naturally, leaving no orphans |

**Description-based indexing is gone**: the old implementation embedded a
`[<mime> <short digest>] <description>` marker in the body and treated the description as the
semantic memory (retrieval used it). That path was removed wholesale in v1.2.0: a description is
second-hand model output, and retrieving "someone else's paraphrase of an image" is strictly worse
than retrieving the image's own vector. Images are now retrieved only by their own vector in the
unified space, and no media marker is written into the body.

**Cross-space vector migration**: blocks store their vector together with a `fingerprint` (the space
fingerprint). At startup `reembedStaleMedia()` recomputes and **writes back** every block whose
`vector` is empty/`'null'` or whose fingerprint differs from the current space (model/dimension
switched), fetching bytes on demand via `BlockData`. Modalities outside the space return
`ErrModalityUnsupported` — the kernel **never substitutes another model's vector**.

### Other Memory Layers

- **Social** (`internal/memory/social/social.go`) — Persona traits and relationship network, wraps GraphDB entity types
- **Text Memory** (`internal/memory/text/text.go`) — Raw conversation JSONL logs, rotation strategy
- **Memory Indexer** (`internal/memory/indexer.go`) — Entity vectorization + jieba keyword extraction, auto-inject into system prompt

### Distillation Pipeline

`internal/memory/pipeline/pipeline.go`
- 10-minute tick, 7-day retention
- Rule-based triple extraction (name / location / likes / age / job patterns)
- Writes to GraphDB

### Context Pruning

```
Four independent heartbeat loops (each with configurable interval):
  ├── distillLoop  (distillInterval,  default 30m)
  │   └── distillContext() — Context.Prune → Document
  ├── archiveLoop (archiveInterval, default 60m)
  │   └── archiveColdDocs() — Cold docs → docToTriples → GraphDB
  ├── mergeLoop   (mergeInterval,   default 120m)
  │   └── detectEntityMerge() — Entity similarity detection → LLM decision
  └── reviewLoop  (reviewInterval,  default 120m)
      └── reviewRelations() — Relation review → SentenceRef recall → LLM fix
```

Entity conflict detection heuristic (bigram Jaccard > 0.75), routed through `selfInputCh` internal channel, LLM makes the final merge decision.


## Knowledge Base

`internal/knowledge/knowledge.go`
- File directory `knowledge/<name>/content.md`
- Independent TF-IDF index, separate from memory system
- `knowledge_search` / `knowledge_create` / `knowledge_list`


## Provider & Lua Adapter Layer

```
Agent
  │
  ▼
Provider Interface (Name / Chat / ChatStream)
  │
  └── LuaAdaptedProvider (the only implementation)
      ├── Serialize CompletionRequest → JSON
      ├── adapter.transform_request() → API format
      ├── HTTP request + adapter.headers
      ├── adapter.transform_response() → unified format
      └── Deserialize
```

Code: `internal/agent/api/provider.go`

ProviderManager manages multiple sources, fallback in registration order. Lua adapters at `internal/lua/adapters/`, each `.lua` script defines `transform_request` / `transform_response` / `transform_stream_chunk`.

VM built-ins: `json.encode` / `json.decode` / `log` / `http_get` / `http_post`.


## Plugin System

### Four Loading Methods

| Method | Registration Mechanism | Compilation | Usage |
|--------|----------------------|-------------|-------|
| Built-in | `init()` → `RegisterFactory` | `internal/plugins/` compiled into kernel | webui/cli/agentcli etc. (13 total; see the criteria section below) |
| External subprocess plugin | Handshake + stdio JSON-RPC reverse registration | `hmapdev build` → `plugin.bin` (ordinary Go binary) | qq/browser/files etc. |
| Lua script plugin | Execute `main.lua` to register tools | No compilation, takes effect after restart/reload | luademo etc. |
| SKILL plugin | Parse `SKILL.md` | Markdown definition | Loaded via clawhubadapter |

Built-in plugin registration: `internal/plugins/all.go` blank imports → each plugin `init()` → `Registry.Load()` scans directory to match factory.

External plugin loading (since v1.0.0): `internal/plugin/dynamic_proc.go` → `exec.Command(plugin.bin)`
→ inherit shared-segment fds → handshake (protocol version check) → `plugin.init` → `plugin.start`
(the plugin reverse-registers tools/stages/channels during this window).
**The C ABI channel (`-buildmode=c-shared` + bridge) was removed entirely in v1.0.0**—
the old `plugin.Open` path-cache workarounds (SHA256 temp-path copies) retired with it.

Lua script plugin loading: `internal/plugin/` → the gopher-lua interpreter executes `main.lua` (at load time `sdk.register_*` only buffers handlers), then `Start()` swaps in the real SDK implementation and registers them in batch. The script is read only once at load time; runtime execution happens via callbacks.

### Which Plugins Stay Built-in (criteria, finalized 2026-10-19)

**A plugin stays in the compiled-in list only if it requires internal-SDK-only
capabilities — API surfaces the public SDK does not have.**

The internal SDK (`internal/sdk`) exposes several capability surfaces that are
**never granted to external plugins**: `TrackerAPI` /
`StatusAPI` / `SupervisorAPI` / `AdapterAPI` / `SelftestAPI` / `ToolAPI`, and
**`Publish` (event publishing)**. External plugins run over the proc bridge's
narrowed capability set where those interfaces simply do not exist at the type
level (authoritative list: `withheldCapabilities` in
`internal/plugin/proc/capability.go`).

This yields a clean binary rule:

- **Plugin needs a withheld capability ⇒ it must stay built-in.** Externalizing
  it makes the feature **fail silently** — no error, no crash, just a data link
  broken between plugin and kernel. The hardest class of bug to trace.
- **Plugin only uses public SDK ⇒ it should be externalized** (.hmap package).
  Independent build and release; a one-line fix no longer means recompiling the
  87 MB kernel.

Positive examples (kept built-in, each for a reason it cannot do without):

| Plugin | Internal capability it needs |
|---|---|
| cli / webui | TrackerAPI, StatusAPI, SupervisorAPI, AdapterAPI |
| agentcli | **EventPublish**: reports terminal output and lifecycle via `Publish(EventTerminalOutput)`; the public SDK has no Publish |
| healthcheck | SelftestAPI (can build a virtual kernel instance for self-checks) |
| pluginmgr | required by the failback recovery chain (`core.agent.failback_plugins` defaults to webui/pluginmgr/recoverydiag) |
| seq | ToolAPI (direct tool-table access; in-group parallel steps invoke tools by name) |
| remotedevice | RegisterChannel (kernel IO channel registration) |
| clawhubadapter | InjectInputSync (OC-plugin proactive injection) |

Counter-examples (externalized, because public capabilities suffice):

| Plugin | Actual dependencies |
|---|---|
| timer | RegisterTool + Settings + InjectText (public) |
| ai_image / files / cmd / localuse / mcp | RegisterTool + Settings (+ public RegisterStage) |

> ★ Reversal on final review (2026-10-19, method-by-method comparison against
> the public SDK brought two back): **kbtree** needs
> `KnowledgeAPI.Subtree/CategoryCounts/SearchIn/Categories` — the public
> `KnowledgeAPI` only has `Search/Add/List`, and the public SDK lacks the
> `KnowledgeTreeView`/`KnowledgeCategoryCount` types. **multimodal** needs
> `LLMAPI.Chat` + `LLMMessage`/`LLMContentBlock` — the public `LLMAPI` only
> has `ListSources/SetSource/CurrentSource`. Both were initially misjudged as
> "public-only" because `s.Knowledge()`/`s.LLM()` exist on both sides **with
> the same names but different surfaces**. Lesson: externalization decisions
> require method-by-method comparison against the public interface, not name
> matching.

> ⚠️ Both copies existing at once is forbidden: `loadOne` checks the built-in
> factory table first and never looks at the external `plugin.bin` if it hits
> ⇒ the built-in version always wins and the external one **installs but never
> takes effect**. When externalizing a plugin, remove it from
> `internal/plugins/all.go` (the single authoritative list) in the same change.

### Terminal ownership: agentcli owns it, the kernel does not (2026-10-20)

Terminal logic belongs solely to `agentcli` (PTY, session lifecycle, output
buffering, history). The kernel **no longer** models terminal semantics — the
former `internal/agent/core/terminal_registry.go` is gone, because encoding
"what a terminal is and how to merge sessions out of tool calls" inside the
kernel was a layering mistake.

Plugins that need a terminal panel (WebUI / CLI) **subscribe to the event bus and
build their own view**:

| Event | Publisher | Purpose |
|---|---|---|
| `EventTerminalOutput` | agentcli | session output, create/close/timeout (`running` flips) |
| `EventToolCall` | kernel | `cmd_run` history, `terminal_create/close` merge |

WebUI and CLI share the `internal/pluginview` implementation so both parse
identically (historically two independent derivations each hit the same
"tool result is Go map text" bug). The view is not authoritative: the truth is in
agentcli, so a plugin starting after a session will miss its early events.

agentcli therefore needs `Publish` (not available to external plugins) and stays
built-in under the rule above; externalizing it would first require a controlled
plugin-to-kernel event channel, which is deliberately not opened today.

### Built-in vs External Plugins

| Dimension | Built-in Plugin | External Plugin |
|-----------|----------------|-----------------|
| Registration | `init()` calls `plugin.RegisterFactory(name, factory)` | Implements `NewPluginFactory(name, config) (sdk.Plugin, error)` entry function |
| Compilation | Compiled into `homed` binary, no separate build | Compiled via `hmapdev build` to `plugin.bin` (ordinary Go binary, zero cgo); the kernel spawns it as a subprocess |
| Distribution | Bundled with kernel, not independently installable | `.hmap` package (ZIP archive), installed via WebUI or pluginmgr API |
| Metadata | `plugin.RegisterPluginMeta()` for display name | `plugin.json` manifest file (name, version, entry, platforms, capabilities, etc.) |
| Plugin directory | No separate directory, compiled into binary | `plugins/<name>/` independent directory with `plugin.json` + `plugin.bin` |
| SDK permissions | Full PluginSDK (SocialAPI read/write, Publish events) | Narrowed `procCore` surface + manifest capabilities declaration + RPC boundary rejection |
| Lifecycle | Starts/stops with kernel, no individual hot-reload | Independent process; true hot-reload by swapping `plugin.bin` (ReloadOne) plus enable/disable |
| Crash recovery | No independent recovery | Process-level isolation: a crash cannot take down the kernel; the kernel detaches its registrations then restarts it with backoff (`SetAutoRestart(false)` opts out) |

Common ground:
- Built-in `RegisterFactory` and external `NewPluginFactory` share the same `NativeFactory` type signature
- `Registry.Load()` handles both uniformly: checks the factory table first (built-in), otherwise dispatches by the manifest `entry` to the proc / lua / skill channel
- Both use the same `Plugin` interface and public SDK API; tool registration, stage hooks, and output channel APIs are identical
- Both share the same tool registry (`StageHost`); LLM invocations treat them identically

### The Three Communication Planes of Subprocess Plugins (v1.0.0)

| Plane | Mechanism | Why this choice |
|---|---|---|
| Control | stdio JSON-RPC (NDJSON frames), currently 82 methods (authoritative list: `internal/plugin/proc/protocol.go`) | The process boundary *is* the ABI boundary—no need to maintain three platform-specific dynamic-library loaders |
| Data | Shared memory segment, **one segment shared by all subprocesses** | One segment per plugin would degrade "kernel ctx → segment → plugin mutates → read back" into the copy model under concurrency, reproducing lost updates exactly |
| Notification | Event ring + platform notify (Linux eventfd / macOS pipe / Windows Event) | The kernel must never block on a consumer: streaming output publishes per token, so any wait shows up as stutter |

**Subprocess lifecycle management**:
- One dedicated `waitLoop` per subprocess (the sole `cmd.Wait()` call site)—it does not rely on
  stdout EOF, because grandchild processes forked by a plugin (browser spawning chromium,
  editdoc spawning python) inherit the same stdout, so EOF never arrives after the plugin itself dies
- Central ledger `proc.Supervisor`: registered on successful handshake, unregistered on exit;
  `Host.Close()` runs StopAll before tearing down the segment (reversing that order leaves plugins
  holding a mapping that has been unmapped—SIGBUS on their next access)
- Crash self-healing: detach registrations (tools + stage handlers + IO channels) → remove from
  the registry → restart with backoff
- Linux `Pdeathsig` is the last-resort guard so subprocesses do not linger as orphans when homed is SIGKILLed

### PluginSDK Four Channels

```
Plugin ──→ Kernel

RegisterTool(name, fn)   ──→  buildToolDefs() / executeToolCall()
RegisterStage(stage, fn, scope...)  ──→  runStage() called at corresponding phase (scope: global / own-tools-only)
Subscribe(event, fn)      ──→  Publish() notify all subscribers
RegisterOutputChannel(name, caps, desc, handler) ──→ output_send__{name} tool generation
```

`internal/sdk/` bridges external SDK interface to kernel, defines complete PluginSDK:

```go
sdk.RegisterTool(name, def, handler)
sdk.RegisterStage(stage, handler, scope...)
sdk.Publish(event)
sdk.InjectInput(source, channel, payload)
sdk.InjectInterrupt(source, channel, payload)
sdk.Memory().Recall/Commit
sdk.Knowledge().Search/Create
sdk.Settings().Get/Set/List
sdk.RegisterOutputChannel("qq", sdk.CapText|sdk.CapAudio|sdk.CapImage, "QQ channel, see output_send__qq_help for details", handler)

// v1.1.1 media APIs (all additive, no signature changes)
sdk.DocMemory().InsertWithMedia(doc, attachments)   // attachments with Data land in CAS; Digest-only ones reference existing content
sdk.InjectInputMedia(source, channel, text, blocks) // media reaches the model in *this* turn
sdk.InjectInputMediaSync(...)                       // same, and waits for the reply
sdk.InjectInterruptMedia(...)                       // media-bearing interrupt, can preempt current processing
```

How media injection differs from `SetToolBlocks`: the latter is only callable inside a tool handler
and its media reaches the model with the **next** tool message; these three let a plugin
**initiate a turn that carries media** — it goes out with this turn's message and is automatically
stored in CAS with a memory reference attached. `Triple` and `Doc` gained `MediaDigests` /
`Attachments` correspondingly.

`internal/sdk/` is the bridge implementation for this layer and is not subject to the public
interface freeze (see `docs/git-branching.md` §6).

### Plugin Interface

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
```


## Output Channel System

Each output channel generates two tools:

| Tool | Type | Purpose |
|------|------|---------|
| `output_send__{name}` | function | Accepts `payload` (content), `meta` (JSON routing metadata), `type` (enum) — routed to plugin handler |
| `output_send__{name}_help` | function | Returns the channel's meta format and type enum documentation |

Capability flags:

| Flag | Value | Meaning |
|------|-------|---------|
| CapText | 1 | Plain text |
| CapFile | 2 | File |
| CapImage | 4 | Image |
| CapAudio | 8 | Audio |
| CapStructured | 16 | Structured data |

System prompt injection: output gate rules, multi-call support, long message splitting.
Child agent permission: `output_send__` prefix tools are allowed.


## EventAgentLLMChain Event

- Event type `agent_llm_chain` emitted after each LLM turn
- Contains the full LLM response (text + tool calls + reasoning)
- WebUI subscribes to this event via SSE for real-time display
- Plugins can subscribe via EventSubscriber (read-only for external plugins)


## Restricted External Plugin API

Layered architecture: internal plugins get full PluginSDK, external plugins get restricted SDK.

| API | Internal Plugin | External Plugin |
|-----|-----------------|-----------------|
| SocialAPI | Full read/write | Read-only (GetPerson / GetTrait / GetRelations / GetNetwork / ListPersons) |
| EventSubscriber | Subscribe + Publish | Subscribe-only (no Publish capability) |

Extended fields:
- Triple extensions: Confidence, SubjectType, ObjectType
- Relation extension: Confidence


## Input Scheduler & Interrupt Mechanism

Inputs do not go straight to the LLM — they first enter the **input scheduler**
(`internal/agent/core/scheduler.go`). Full design:
[`docs/zh/input-scheduler-design.md`](../../../docs/zh/input-scheduler-design.md).

### Two task classes

| Class | Level | Meaning |
|-------|-------|---------|
| `TaskQueued` | none (always 0) | Pending work. Any interrupt (≥ L1) preempts it |
| `TaskInterrupt` | L1–L4 | "How urgent is this", declared by the source via `InjectOptions.Priority` |

### Four interrupt levels

| Level | Meaning | Typical source |
|-------|---------|----------------|
| L1 Background | Fully deferrable | QQ/WeChat messages, bulk notifications |
| L2 Message | General notice | Plugin hints that should be seen soon but aren't urgent |
| L3 Interactive | Needs timely handling | Timer expiry, terminal output, resident-agent reports |
| L4 Critical | **Kernel-exclusive** | panic, kernel events, kernel-level plugin stop button |

When no level is declared it defaults to **L1** — "explicit is a privilege", so a new
plugin never gets preemption rights by accident. L4 declared by an external plugin is
**clamped to L3** (`clampPluginLevel`).

### Preemption and suspension

- **Same level never preempts same level** (`canPreempt` requires strictly greater) —
  this is why messages normally wait for the running task to finish.
- A preempted task is pushed onto the **interrupt stack** (LIFO) with its frame saved,
  and resumed later; the stack is never re-sorted by priority.
- **Starvation guard**: preemption count raises the effective level
  (`effectiveLevel = Level + min(PreemptCount, 2)`, capped at L4).
- **Preemption cooldown**: a just-preempted task cannot be preempted again for
  `preemptCooldown` (2s), so a high-priority stream cannot interrupt the same task forever.
- The interrupt stack depth is structurally bounded (chain = queued ← L1 ← L2 ← L3 ← L4).

### Stop (user presses stop / `/stop`)

Stop is not an empty interrupt. It does two things: ① cancel the current LLM inference;
② short-circuit the x messages **already queued at the moment of stop** during their
pre-action phase (`cancelBudget` snapshot), instead of running them as new input.
Inputs arriving **after** the stop are unaffected.

`PendingInputs()` must include the segment still sitting in `io.inputCh` (not yet moved
into the queue by `pumpInbox`) — during a stop the scheduler is usually busy running a
task, and counting only `sched.queue` yields 0.

### Resident sub-agents and timely feedback

Design: [`docs/zh/resident-subagent-design.md`](../../../docs/zh/resident-subagent-design.md).

- A **resident** is an independent lightweight-kernel agent: its own scheduler, its own
  temp graph memory, sharing the channel registry.
- Parent→child control plane: `resident_agents`
  (list / create / send / inspect / compress / reclaim / destroy).
- **Backlog feedback**: when the main agent is busy for a long time (default > 5m,
  configurable), the kernel hands queued inputs to a temporary **triage assistant**
  (`offload_*` config): simple ones are handled directly, ones needing the main agent
  get an immediate "busy, please wait". Users no longer wait 10+ minutes in silence.
- The triage assistant gets **no inputch** (it receives no plugin user input) and
  **all output channels** (results must reach the original channel).
- On reclaim/destroy, its **residual tasks are decided explicitly by the parent**:
  `residual=keep` (returned to the parent queue, default) or `drop` (explicitly
  discarded with a per-item log entry).

### Legacy three-path view (still present, now a layer beneath the scheduler)

```
interceptLoop (goroutine)
  ├── InputInterruptChan() ← Timer/message notifications
  ├── (a) cancelLLM() → Cancel Provider HTTP request
  ├── (b) interceptCh → process() pre-loop read [interrupt message]
  └── (c) InjectInput() → Trigger new processing when idle
```

Code: `internal/agent/core/scheduler.go` (scheduler), `eventloop.go` (intercept loop).

## Context Budget

`internal/agent/core/tokenbudget.go` — `ComputeTokenBudget`:

```
maxCtx        = provider.MaxContextTokens()      // declared window (per-source context_window wins)
targetUsage   = min(maxCtx × 0.8, 600000)        // working band, capped at 600K
  ├── memory recall budget = (targetUsage - fixed) / 3
  └── context events budget = remaining 2/3
```

★ **Window ≠ working band**: a source's real window may reach 1M, but near-full windows
lose attention and cost/latency rise linearly, so `maxTargetTokens=600000` caps the
working band separately. If the model name (e.g. `AUTO`) yields no window,
`ModelContextWindow` **logs a warning** and falls back conservatively; operators should
declare `core.llm.sources.<name>.context_window` explicitly.

**Budgets are ceilings, not fill targets**: memory is recall-ranked (it stops when nothing
is relevant) and the timeline is taken newest-first within budget. Measured: with a 400K
budget, actual injection was still a few hundred characters.


## Prompt layering (established 2026-10-20)

The system prompt is **not** the place for tool usage instructions. Decide where a
piece of text belongs by its lifetime and visibility:

| Content | Belongs in | Why |
|---|---|---|
| Identity, how to address the user, tone, appearance | Persona (`personal/personal.md` or `core.agent.personal_prompt`) | Lives and dies with the persona; stable across plugins and channels |
| One tool: when to use, parameter meaning, side effects, pitfalls | The tool `description` | **Travels with the tool**: if the plugin is absent or not started, the description is not in the list, so the model cannot misuse it |
| Multi-step procedures, strategies, domain knowledge | Skills (`skill/<name>/SKILL.md`) | Progressive disclosure: the index is always present, the body is fetched on demand via `skill_info` |
| Output-delivery rules, same-round parallelism, timeline semantics | Kernel runtime blocks (`buildSystemPrompt` / timeline header) | Generated from **actual** execution semantics; static copies drift from the implementation |

**Anti-pattern (measured 2026-10-20)**: the default system prompt carried
"cmd_run runs through a full bash…", "prefer files_write for writing files…",
"spawn_child in parallel…". After those plugins were externalized and made
on-demand, `cmd_run` was no longer in the tool list while the prompt still told
the model to use it — the first turn called a nonexistent tool and only then
learned "tool cmd_run not registered".

**Rule of thumb**: a concrete tool name plus usage instructions in the prompt is
almost always misplaced. The only exceptions are (1) tool names **injected at
runtime** (e.g. `output_send__{channel}`, whose channel set changes per turn) and
(2) statements about **kernel behaviour** rather than tool behaviour (e.g.
"multiple calls in one round run in parallel by default").

Likewise, a plugin's skill must ship **in the same package**: an
`auto_start=false` plugin without a skill has its tools absent from the initial
list, so the model never knows it exists and never starts it — equivalent to not
being installed. hmapdev enforces this at packaging time
(`autostart_skill_guard_test.go`).

## Configuration System

`internal/config/registry.go` — ConfigRegistry

- SQLite storage, `config` table + `config_<plugin>` independent tables
- Namespaces: `core.*` / `plugin.<name>.*`
- `RegisterDefault` inserts ~80 default keys (including 1 LLM source seed; further sources are added at deploy time)
- WebUI settings page `/api/v1/settings` for read/write


## Code Structure

```
cmd/homed/main.go          — Entry: assembles all subsystems
cmd/waiter/main.go         — CLI client (Unix socket)
internal/
├── agent/
│   ├── core/              — Agent core (eventLoop/process/stages/context)
│   │   └── plugin_health.go — Plugin health monitoring and auto-restart
│   ├── api/               — Provider interface + LuaAdaptedProvider
│   ├── io/                — IOManager (queue/interrupt/output)
│   └── personal.go        — Persona loading
├── plugin/
│   ├── registry.go        — Registry + lifecycle
│   ├── dynamic.go         — Load-channel dispatch (proc / lua / skill); .so only remains to error loudly
│   └── manifest.go        — plugin.json metadata
├── plugins/               — Built-in plugin implementations (13; see "Which Plugins Stay Built-in")
│   ├── all.go             — Blank imports (**the single authoritative list**; edit only this)
│   ├── webui/             — HTTP server + embedded SPA
│   ├── cli/               — Unix socket CLI
│   ├── clawhubadapter/   — ClawHub adapter (OC plugin/SKILL/JS/Python sidecar)
│   ├── agentcli/          — PTY terminal
│   ├── healthcheck/       — Health check
│   ├── pluginmgr/         — Plugin manager
│   ├── skillmgr/          — Skill plugin management
│   ├── cfgmgr/            — Config manager
│   ├── remotedevice/      — Remote device bridge
│   ├── seemedia/          — Direct media view
│   ├── seq/               — Sequence orchestration
│   ├── data/ plugin_data/ — Plugin static assets (non-Go, shipped with the kernel)
│   ├── cmd/ localuse/ mcp/ ai_image/ files/
│   │                      — Source remains but **not listed in all.go**: externalized
│   │                        on 2026-10-19 as .hmap packages (projects in the SDK repo's
│   │                        example/); directories to be removed once external projects stabilize
├── sdk/                   — PluginSDK definitions
│   ├── plugin.go          — Plugin interface + PluginSDK
│   ├── memory.go          — MemoryAPI
│   ├── knowledge.go       — KnowledgeAPI
│   ├── settings.go        — SettingsAPI
│   └── llm.go             — LLMAPI
├── memory/
│   ├── graph.go           — SQLite graph database
│   ├── indexer.go         — Graph → vector index
│   ├── vector/store.go    — TF-IDF vector engine
│   ├── document/document.go — Document memory
│   ├── text/text.go       — Text logs
│   └── pipeline/          — Distiller
├── knowledge/knowledge.go — Knowledge base
├── lua/
│   ├── vm.go              — Lua VM (json/log/http)
│   └── adapters/          — 10 LLM protocol adapter scripts
├── config/registry.go     — SQLite config center
├── events/bus.go          — Event bus
├── tracker/               — OverlayFS change tracking
├── supervisor/            — Daemon management
└── meta/                  — Meta information
    └── meta.go            — Agent metadata
```
