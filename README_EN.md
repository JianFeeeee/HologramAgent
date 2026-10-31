> ⚠️ **AI-Assisted Programming Notice**: Parts of this project's code, documentation, and commit history were generated or modified with AI assistance. Key changes have been human-reviewed, but please evaluate and verify before use.

# HologramAgent

> **中文**: [README.md](./README.md)

HologramAgent is an agent kernel that runs on your own hardware. Connect it to QQ, a web UI,
your shell, your files, a browser, and it works in all of them.

Memory sinks: the last few turns stay in context, older ones become documents, oldest get
distilled into triples in a graph database. That is why it can run for months without
stuffing the entire history into every prompt.

```bash
./build/homed -data /tmp/ha      # start the kernel
./build/waiter                    # talk to it
```

You need a Linux box. API keys go in through the web UI and live in local SQLite.
Development started July 2026; currently 1.4.0.

---

## Design decisions

Plugins run in separate processes. One panics and the kernel stays up; it restarts on its
own. Drop in a new `plugin.bin` and it's live. The earlier version used C shared libraries,
where `dlclose` silently no-ops under `DF_1_NODELETE`, cgo timeouts leave threads alive, and
one plugin panic dragged `homed` down with it. That is why it was rewritten as subprocesses
plus shared memory.

The kernel itself makes no HTTP calls and reads no files. Everything that touches the outside
world lives in plugins: the web UI, QQ, command execution, web search, the browser. Not for
tidiness. It means you can swap the entire interaction layer without touching the kernel.

Inside one QQ plugin, a group owner and a random member can hold different permissions.
Instructions arriving from another agent don't get written into your memory, though they can
trigger recall. The kernel's security module makes those calls; plugins only declare.

Memory is partitioned by scene, not by session. Move from work talk to personal talk and it
splits context by channel, conversation partner, tools used, and time of day. When a scene
recurs, last time's conclusions come back, including the ones it got wrong.

## What it can do

Places it talks: web UI, shell, QQ groups and DMs, other agents over A2A, and an ACP server
inside your IDE. It can look at images, video, voice, and web pages, and it can drive a real
Chrome.

For getting things done it reads and writes files, runs commands, writes code, and handles
calendars, notes, RSS, and music. QQ files and voice messages go both ways.

Behind that sit three memory layers plus a graph database plus a knowledge base, recalled by
scene. When new work arrives mid-task it enters a scheduler; if the user pushes back it can
jump the queue, and interrupted work saves its state and resumes. Long jobs can be handed to
sub-agents so they don't block the line you're talking on.

28 built-in plugins, 89 model-callable tools. External plugins live one-per-repo at
[github.com/HologramAgent](https://github.com/HologramAgent).

## How it works

```go
homed  (kernel: LLM orchestration + memory + retrieval)
   ↑ PluginSDK
plugins  (web UI / QQ / CLI / files / network / all IO)
```

Three channels between kernel and plugins: JSON-RPC over stdio for control, shared memory
for bulk data, an event ring for notifications. Large objects aren't serialized, so there's
nothing to copy twice.

Incoming messages hit a scheduler first, not the model. This exists for the case where work is
already in flight and a new instruction arrives: queued work and interruptions are separate
classes, interruptions carry four urgency levels, a higher level preempts a lower one and the
preempted work saves its state to resume later. Same level waits.

Below that sits a seven-stage pipeline where the model can call tools in a loop. Multiple tool
calls in one turn are serial by default unless a tool declares itself concurrency-safe. One
undeclared tool in a batch drops the whole batch back to serial.

## Quick Start

```bash
make build build-cli
./build/homed -data /tmp/ha
```

```bash
# Interactive mode
./build/waiter

# Or a single message
echo "Hi, remember I like coffee" | ./build/waiter
```

### Daemon mode

`waiter` can also run resident, holding a persistent connection to `homed` and waiting for a
TUI instance to attach. Useful when the agent needs to reach you proactively or keep a device
bridge alive.

```bash
# Resident (connects to ~/.homeagent/cli.sock by default)
./build/waiter --daemon

# Custom socket
./build/waiter --socket /path/to/cli.sock --daemon

# Any TUI or one-shot instance then attaches to the running daemon instead of homed directly
./build/waiter
```

The daemon listens on `~/.homeagent/waiter.sock`, replays the last 256 lines of conversation to
new clients, survives disconnects, reconnects to `homed` on its own, and keeps the device bridge
alive when `device_gateway` / `device_token` are configured.

Set API keys in the web UI at `http://localhost:8080`; they persist in SQLite.

## Code Structure

```
cmd/homed/          Daemon entry, assembles all subsystems
cmd/waiter/         CLI client (Unix socket)
internal/
├── agent/core/     Agent core: input scheduler (2 classes + 4 levels), event loop, LLM tool loop, 7-stage pipeline, residents
├── agent/api/      LLM Provider (Lua adapter layer: provider.go drives the vm)
├── memory/         Three-layer memory: Graph(SQLite) / Document(JSON+TF-IDF) / Text(JSONL) + StaticEmbedder(pretrained word embedding/TF-IDF fallback) + CleanTemplateText(de-template)
├── knowledge/      Knowledge base (filesystem + TF-IDF)
├── plugin/         Plugin registry + subprocess loader (stdio RPC + shared memory segment + event ring)
├── plugins/        built-in plugins (webui / cli / files / cmd / mcp / kbtree / seq / skillmgr …)
├── sdk/            PluginSDK (Tool/Stage/Event three channels)
├── config/         SQLite config center
├── events/         Event bus
└── internal/lua/adapters/   10 LLM protocol adapter scripts
External plugin development: see the [hologramagentsdk](https://github.com/JianFeeeee/hologramagentsdk)
repo with the `hmapdev` toolchain. Each plugin has its own repo under
[github.com/HologramAgent](https://github.com/HologramAgent).
```

## Documentation

**Online docs**:

- Introduction site (project overview): <https://introduce.homeagent.jianfgit.xyz/>
- Plugin SDK docs (getting started / API reference / guides / examples): <https://sdk.homeagent.jianfgit.xyz/>
  plain-text entry for agents: [`llms.txt`](https://sdk.homeagent.jianfgit.xyz/llms.txt) /
  [`llms-full.txt`](https://sdk.homeagent.jianfgit.xyz/llms-full.txt)

**In-repo docs** (versioned with the code):

- [Project Overview](assets/docs/en/OVERVIEW.md) | [中文](assets/docs/zh/OVERVIEW.md)
- [Technical Architecture](assets/docs/en/ARCHITECTURE.md) | [中文](assets/docs/zh/ARCHITECTURE.md)
- [Plugin Development Guide](assets/docs/en/PLUGIN_DEV.md) | [中文](assets/docs/zh/PLUGIN_DEV.md)
- [Lua Adapter](assets/docs/en/ADAPTER.md) | [中文](assets/docs/zh/ADAPTER.md)
- [Knowledge Base Demo](assets/knowledge/homeagent_architecture/content.md)
- [Design doc index](docs/zh/README.md) — status classification and trust rules for 32 design docs
- [Changelog](CHANGELOG_EN.md) — what actually changed in each release
- [Future plan](docs/zh/future-plan.md) — everything not yet built

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

This project is released under the **GNU Affero General Public License, version 3
(AGPL-3.0-only)** — see [LICENSE](LICENSE).

This is the strongest copyleft in the GPL family: besides shipping the complete corresponding
source when you distribute the software, **you must also offer the source to users who interact
with it over a network** (§13, Remote Network Interaction). Anyone running a modified HologramAgent
as a network service therefore has to make the modified source available to that service's users.

Plugins are **statically linked** against this project through the public
[hologramagentsdk](https://github.com/JianFeeeee/hologramagentsdk) (the SDK source ends up inside the
plugin binary), but that repository is released under **MIT**. That is a permissive license, so code
received under it does **not** inherit this project's AGPL. External plugins are therefore **not
derivative works of this project**: authors pick their own license (closed-source, commercial or
private included), with no same-license obligation and no §13 network clause. The safety and
vitality of the third-party plugin ecosystem rest on this.

The boundary is clean: **AGPL covers the kernel and the bundled plugins** (`homed`, `internal/`,
the 13 built-in plugins under `internal/plugins/`); **MIT covers the public SDK** (`sdk/`, whose
`go.mod` has zero external dependencies and imports only the Go standard library, never
any kernel code). Process isolation is beside the point here. What decides the
license is the linked SDK code itself, and that code is MIT.

### Third-party components shipped with the packages

| Component | License | Location |
| --- | --- | --- |
| Chinese-CLIP ViT-B/16 (ONNX artifacts) | Apache-2.0 | `/usr/lib/hologram/models/chinese-clip-vit-b16-onnx/` |
| ONNX Runtime (`libonnxruntime.so`) | MIT | `/usr/lib/hologram/onnxruntime/` |
| jieba dictionary (embedded in the binary) | MIT | `internal/memory/jiebadict/` |
| Go dependencies (go-sqlite3, gojieba, bubbletea, …) | MIT / BSD-3 / Apache-2.0 | permissive, AGPL-3.0-compatible |

These components keep their own licenses and are not relicensed by this project. Full texts are
shipped in `/usr/share/doc/hologramagent/licenses/`, and the package metadata declares this package
as `AGPL-3.0-only`.
