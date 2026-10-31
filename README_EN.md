> ⚠️ **AI-Assisted Programming Notice**: Parts of this project's code, documentation, and commit history were generated or modified with AI assistance. Key changes have been human-reviewed, but please evaluate and verify before use.

# HologramAgent

> **中文**: [README.md](./README.md)

Self-hosted agent kernel. Runs on your own hardware; plugins attach as separate processes.

```bash
make build build-cli
./build/homed -data /tmp/ha
```

API keys go in the web UI at `http://localhost:8080`. Requires Linux.

The main difference from typical agent frameworks is context handling: instead of truncating
on overflow or asking the model to summarize, it scores history by relevance and sinks the
low-scoring events into cold context, then distills those into a graph. Two other things differ:
startup order is resolved at build time, and plugins launch as soon as their facilities are ready.

## Architecture

```text
                    homed (kernel)
        LLM orchestration · memory · retrieval
                       │
        ┌──────────────┼──────────────┐
        │              │              │
   stdio RPC       shared mem     event ring
     control       bulk data      notify
        │              │              │
        └──────────────┼──────────────┘
                       │
              plugins (separate processes)
   web UI · QQ · CLI · files · browser
```

The kernel process handles orchestration, memory and retrieval; HTTP, files and devices belong to
plugins (`internal/plugin`). A crashed plugin is restarted with backoff 1s / 2s / 3s; the 4th
crash within 5 minutes stops automatic restarting pending human intervention
(`cmd/homed/kmod_runtime.go`).

### Context window

`internal/agent/core/context.go`

Each event gets two vectors on entry: a dense vector sharing one space with media and cold
context, and a TF-IDF word vector as fallback. Pruning scores history against the current input
and keeps topK, with the most recent N events exempt from scoring.

N defaults to 10, bounded by budget: `min(configured, ContextTokens / avg tokens per event)`.

That bound came from a measurement. At a 50k window with 5800 tokens per event and a 40000
budget, pinning 10 events needed 58k, more than the budget itself. Every prune met its target
without freeing space; the budget was gone after two rounds and the task terminated, with recall
falling from 44% to 22%. Small windows now tighten automatically; a 1M window uses the configured
value.

Pruned events move to the cold-context layer together with the media blocks they hold. Block IDs
stay the same; only the holding layer changes. Retrieval uses `coldcontext_query`: `Peek` to
confirm, then `Remove` the source copy. There is no fetch-and-delete path, since deletion follows
confirmation.

### Three layers

`internal/agent/core/context.go` · `internal/memory/coldcontext/` · `internal/memory/graph.go`

| Layer | Holds | Reaches the model | Cooling |
|---|---|---|---|
| L1 Context | events + blocks | directly, in the system prompt | low relevance sinks |
| L2 Cold Context | events + blocks (same shape as L1) | inserted back into L1 order on recall | ≤2 accesses and 72h idle, then distilled |
| L3 Stable | triples, nodes are blocks | on recall | never leaves |

A block has exactly one holder at any layer, with no reference counting. Images and audio are
blocks in L2/L3 with their own vectors, searched in the same space as text.

### Scenes

`internal/memory/scene_emerge.go`

A scene is built from five weighted features: channel 1.0, conversation partner 1.0, tools 0.8,
topic 0.4, time of day 0.2. A scene is created on the second occurrence of the same fingerprint
(`minSceneEvidence = 2`); one-off interactions don't create one. Join threshold 0.5, recall 0.35.
Recurrence adds 1 to strength; scenes that stop recurring decay by half-life.

Two entry points run in parallel: a plugin declares the scene at an injection point (e.g.
`chan:qq/peer:group_1`), or fingerprints cluster on their own. A declared scene derives features
only from its own key and does not absorb the full fingerprint. Absorbing it would make the
declared scene match nearly every later turn of that kind at close to 1.0 similarity, and the
clustering path could never grow narrower scenes.

### Seven stages

`internal/agent/core/stages.go`

| Stage | When | Plugins can |
|---|---|---|
| `on_input` | message arrives | blocklist, rate limit, short-circuit reply |
| `pre_action` | context ready, before LLM | inject external data |
| `post_action` | LLM returns text and tool calls | filter, force redirect |
| `before_toolcall` | before one tool runs | audit, deny, rewrite args |
| `after_toolcall` | after one tool runs | redact, sort results |
| `before_output` | final text ready | format adaptation |
| `after_output` | after sending | statistics |

The inner loop is `post_action → [before_toolcall → run → after_toolcall] → post_action`. It exits
when the model stops calling tools, all calls are denied, or the limit is reached.

## Everything else

- **Channels**: web UI, CLI, QQ groups and DMs, A2A, ACP in your IDE
- **Tools**: files, shell, code, browser (real Chrome), calendar, notes, RSS, music
- **Modalities**: images, video, voice, screenshots, web pages

28 built-in plugins, 89 model-callable tools. External plugins live one per repo under [github.com/HologramAgent](https://github.com/HologramAgent).

## Downloads

[Releases](https://github.com/JianFeeeee/HologramAgent/releases) ship three variants:

| Variant | Contents | For |
| --- | --- | --- |
| **full** | homed + waiter + desktop GUI + systemd unit | Single-machine, everything |
| **server** | homed + waiter + systemd unit | Servers (no desktop environment) |
| **client** | waiter + desktop GUI | Connecting to a remote HologramAgent |

- Linux: `.deb` (amd64/arm64), `.rpm` (x86_64), `.tar.gz`
- Windows: `HologramAgent_v1.2.0_{Full,Server,Client}_win64.exe` (NSIS installer, includes the AGPL license page). Since v1.2.0 `homed` no longer supports native Windows (it relies on fd inheritance and in-segment offset dereferencing), so the installer bootstraps **WSL2** and installs the Linux packages inside the distribution the same way a Linux host would.
- Portable: `homeagent-bin-<os>_<arch>.tar.gz` (homed/waiter/initconfig)
- Verification: `SHA256SUMS`

The macOS `homed` requires a native macOS build (CGO + sqlite3), so release packages ship only `waiter`/`initconfig`.

## Build

```bash
make build build-cli    # Build daemon + CLI
make test               # go test ./...
make install            # Install to system
```

Dependencies: Go 1.25+, CGo (go-sqlite3), Linux.

> `homed` requires Linux (it relies on fd inheritance and intra-segment offset
dereferencing of the shared memory region; see `cmd/homed/platform_windows.go`).
On Windows only `waiter.exe` is built and `homed` runs under WSL2 (see Downloads).
macOS can build `waiter`/`initconfig`; `homed` must be built on native macOS.

## License

**AGPL-3.0-only**. See [LICENSE](LICENSE).

Bundled third-party components keep their own licenses; full texts ship in
`/usr/share/doc/hologramagent/licenses/`.

### Third-party components shipped with the packages

| Component | License | Location |
| --- | --- | --- |
| Chinese-CLIP ViT-B/16 (ONNX artifacts) | Apache-2.0 | `/usr/lib/hologram/models/chinese-clip-vit-b16-onnx/` |
| ONNX Runtime (`libonnxruntime.so`) | MIT | `/usr/lib/hologram/onnxruntime/` |
| jieba dictionary (embedded in the binary) | MIT | `internal/memory/jiebadict/` |
| Go dependencies (go-sqlite3, gojieba, bubbletea, …) | MIT / BSD-3 / Apache-2.0 | permissive, AGPL-3.0-compatible |

The package metadata declares this package as `AGPL-3.0-only`.
