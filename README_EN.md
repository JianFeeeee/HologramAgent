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

The kernel makes no HTTP calls and reads no files. Everything touching the outside world is a
plugin. A plugin crash leaves the kernel running; restarts back off 1s / 2s / 3s, and the 4th
crash within 5 minutes stops automatic restarting for human intervention.

### Context is not "truncate and summarize"

The usual approach drops old turns or asks the model to summarize them. Both share a flaw: what
gets lost is decided by position. Here each historical event is scored against the current input,
and low scorers sink into the cold-context layer.

Scoring uses dense cosine similarity in the same vector space as media and cold context, falling
back to TF-IDF when the model isn't loaded. The most recent N events are always kept, but N is
itself bounded by budget:

```go
// protected count cannot exceed what the budget can hold
budgetCap = ContextTokens / avgEventTokens
pCount = min(configured, budgetCap)
```

That bound came from a measurement: with a 50k window, one event averaged 5800 tokens (tool
results), while the budget was 40000, so "pin 10 events" didn't fit. Every prune hit the
protection floor, the budget was exhausted after two rounds, and the task terminated. Recall
dropped from 44% to 22%. Smaller windows now tighten automatically, and a 1M window can use the
full configured value.

Pruned events aren't discarded, they migrate: the event and the media blocks it holds move into
the cold-context layer with block IDs unchanged, only the holding layer changes. Retrieval uses
`coldcontext_query` in two phases (Peek to confirm, then Remove) rather than delete-then-refill,
which would lose data permanently if the refill failed.

### Three layers migrate, they don't copy

```text
L1  Context        relevance pruning, last N always protected
      ↓   low scorers sink (with media blocks)
L2  Cold Context   dense retrieval; ≤2 accesses and 72h stale means sink further
      ↓   distilled into triples
L3  Stable         entities, relations, scene index; recall by scene
```

A block has exactly one holder at any layer, with no reference counting to keep it alive. Media
are not attachments: images and audio are first-class graph nodes with their own vectors,
searchable in the same space as text.

### Memory is partitioned by scene, not by session

Typical agents keep one history per session, so switching topics loses context or crams
everything into one window. Here memory is partitioned by observable scene fingerprint: channel,
conversation partner, tools, topic, time of day, weighted 1.0/1.0/0.8/0.4/0.2.

A scene comes into being only on the second matching fingerprint (`minSceneEvidence = 2`).
Join threshold 0.5, recall 0.35 — recall is looser because failing to recall loses information
while an extra scene costs a few lines of context. Each recurrence adds to scene strength;
scenes that stop recurring decay by half-life.

Two paths run in parallel: **declared** (a plugin states at the injection point which scene this
is) and **emergent** (fingerprints cluster on their own). Declared scenes deliberately don't
absorb the full fingerprint, otherwise they'd score near 1.0 against every later turn and the
emergent path could never grow finer-grained scenes.

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

`post_action → [before_toolcall → run → after_toolcall] → post_action` is the inner loop. It exits
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
