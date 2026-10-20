**中文** | [English](../en/OVERVIEW.md)

# HomeAgent — Project Overview

## What Is This

HomeAgent is a continuously-running personal intelligent Agent framework.

Core architecture: a long-running kernel process (`homed`) that connects to various IO channels (QQ, Web, CLI, etc.) through a plugin system. The kernel handles LLM orchestration, memory management, and knowledge retrieval; plugins handle all external IO — sending/receiving messages, file operations, web search, etc.

### Design Highlights

**Separation of Core Domain and Application Domain** — The kernel (core domain) performs no IO operations; all IO capabilities belong to plugins (application domain). The boundary is defined through PluginSDK:
- Plugins register tools (Tool) with the kernel for LLM invocation
- Plugins hook into the processing pipeline (Stage) to intercept/rewrite message flow at various phases
- Plugins subscribe/publish events (Event) for loosely-coupled communication
- Plugins queue or interrupt input delivery through IO API

The significance lies in clear responsibility boundaries: the kernel focuses on orchestration and memory management, while plugins handle IO implementation — the two are not coupled.

**Business semantics stay in plugins** — the kernel does not define business
objects: terminals, command history, device state and media libraries all belong to
the plugin domain. Take terminals: the PTY, session lifecycle and output buffering
all live in `agentcli`; when WebUI or CLI needs a terminal panel it **subscribes to
the event bus** (`EventTerminalOutput` + `EventToolCall`) and builds its own view
instead of reading a state component inside the kernel. The kernel offers only the
event bus and the plugin registration surface — adding a new capability does not
require touching the kernel, and no second version of business truth accumulates there.

**Three-Layer Memory Architecture** — Manages information retention across long agent runtimes through a tiered storage strategy:
- **Context Layer**: In-memory pretrained word embedding scored event window (StaticEmbedder word vectors → CosineSimilarity, TF-IDF fallback), maintains recent context in real-time, low-relevance events automatically sink to the next layer
- **Document Layer**: JSON files + TF-IDF vector-indexed temporary memory, supports explicit submission and implicit archival, cold data distills to Graph
- **Graph Layer**: SQLite graph database, persists entities and relations, BFS traversal recall, distillation pipeline extracts triples from conversations

Three progressive layers — context, cold archive, long-term graph memory — form an information decay and consolidation pipeline from short-term to persistent storage.

**Media Memory (first-class memory blocks since v1.2.0)** — Images and audio are not attachments; they are first-class nodes of memory:
- **Content-addressed store (CAS)**: addressed by digest, metadata in SQLite and blobs on disk (two-level
  bucketing), identical bytes stored once. Every `Get` re-verifies the digest (silently returning corrupt
  data is worse than an error).
- **Blocks carry their own multimodal vector and fingerprint**, and are retrieved directly — there is no
  media marker in the body text any more.
- **No separate GC, no reference counting, no keep-set**: blocks follow the single-layer invariant —
  Context → Document → Graph is a **migration** of a block, not a copy, and nothing is kept alive by
  references; **deleting a block deletes its content.**
- **Reaches the plugin boundary since v1.1.1**: plugins read and write media through `InsertWithMedia` /
  `InjectInputMedia`; the model attaches media via the `media_digests` argument of `memory_commit` / `doc_commit`.
- **The old mechanism was removed entirely**: the **descriptive index** — writing the vision model's
  description into the body text as a `[<mime> <short digest>] <description>` marker and retrieving by it —
  together with `media_refs` reference counting, was deleted in v1.2.0. A description is second-hand
  information produced by a model; retrieving "someone else's rendering of the image" is worse than
  retrieving the image's own vector.

## What It Actually Does

Code is in the project root, implemented in Go.

**Kernel** (`internal/agent/core/`):
- `eventloop.go` — Message loop (`eventLoop`), queuing input from the IO layer
- `process.go` / `stages.go` — Processing pipeline: memory recall → persona injection → LLM call → tool execution → output delivery, 7 stage hooks
- `toolcall.go` — Tool scheduling and execution
- `context.go` — Context management (pretrained word embedding scoring StaticEmbedder → CosineSimilarity, TF-IDF fallback), automatic pruning of low-relevance events
- LLM calls abstracted through the Provider interface, with multi-source automatic fallback

**Memory System** (`internal/memory/`):
- **GraphDB** (`graph.go`) — SQLite, entities + relations tables, BFS traversal
- **Document Store** (`document/document.go`) — Temporary memory, JSON files + TF-IDF vector index, consume-on-read
- **Text Memory** (`text/text.go`) — Raw conversation logs, JSONL file rotation
- **Social Store** (`social/social.go`) — Persona traits + relationship network, wraps GraphDB
- **Memory Indexer** (`indexer.go`) — Auto-vectorizes GraphDB entities, recalls and injects into system prompt on user input

**Knowledge Base** (`internal/knowledge/knowledge.go`):
- File system directory `knowledge/<name>/content.md`
- TF-IDF vector search, independent index instance from the memory system
- LLM operates via three tools: `knowledge_search` / `knowledge_create` / `knowledge_list`

**Plugin System** (`internal/plugin/`):
- Built-in plugins: Go `init()` self-registration, compiled into kernel
- External plugins (since v1.0.0): compiled to an ordinary Go binary `plugin.bin`, spawned by the
  kernel as an **independent subprocess**, communicating over stdio JSON-RPC (control plane) +
  a shared memory segment (data plane) + an event ring (notification plane); Lua script plugins are
  also supported (the C ABI shared-library channel, `-buildmode=c-shared`, was removed entirely in v1.0.0)
- PluginSDK (`internal/sdk/`) defines four channels: RegisterTool / RegisterStage / Subscribe / RegisterOutputChannel
- 7 stage hooks: on_input → pre_action → post_action → before_toolcall → after_toolcall → before_output → after_output

**LLM Provider** (`internal/agent/api/provider.go`):
- Provider interface: Name / Chat / ChatStream / MaxContextTokens
- Single implementation, `LuaAdaptedProvider`: every source goes through a Lua adapter script that
  transforms requests/responses; adapter scripts live in `internal/lua/adapters/`, one `.lua` per protocol
- 10 built-in adapters: deepseek / openai / anthropic / gemini / mistral / groq / github / kimicode / ollama / server
- Sources (`core.llm.sources.<name>.*`) are configured at deploy time — the count is deployment-specific
  and is not tied to the number of built-in adapters

**WebUI** (`internal/plugins/webui/`):
- Embedded SPA dashboard (`dashboard.html` packaged via `//go:embed`)
- REST API: status query, configuration management, memory operations, knowledge management, plugin management
- OpenAI API-compatible `/v1/chat/completions` endpoint
- SSE event stream `/api/v1/chat/events`

**ClawHub Adapter** (`internal/plugins/clawhubadapter/`):
- Unified loader for OC plugins (Node.js), Python sidecar, JS sidecar, and SKILL plugins
- RegistryDispatcher pattern: routes registration notifications to Tool/Provider/Channel/Stage registries
- ClawHub marketplace search and install: `clawhubadapter_search` / `clawhubadapter_npm_install`
- 9 provider types mapped to LLM-accessible tools (image generation, web search, speech, etc.)
- OC channels auto-registered as IO devices with text/file/image/audio capability flags


## Project Status

Core functionality is operational. Plugin system and SDK are ready for independent external plugin development.

- Built-in plugins (13): webui / cli / agentcli / cfgmgr / healthcheck / kbtree / multimodal / pluginmgr / clawhubadapter / remotedevice / skillmgr / seemedia / seq
- Criterion: **a plugin stays built-in only if it requires internal-SDK-only capabilities**; everything else ships as an .hmap external plugin (see ARCHITECTURE)
- External plugin examples ([homeagent-sdk](https://github.com/JianFeeeee/homeagentsdk) repo `example/`, both Go and Lua types): qq / files / a2a / ai_image / bili / browser / calendar / editdoc / memo / music / ocr / rss / sanitizer / weather / luademo
- Distribution: `.hmap` plugin package format, installable via WebUI
