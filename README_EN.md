> ⚠️ **AI-Assisted Programming Notice**: Parts of this project's code, documentation, and commit history were generated or modified with AI assistance. Key changes have been human-reviewed, but please evaluate and verify before use.

# HologramAgent

> **中文**: [README.md](./README.md)

An agent kernel that runs for months and still remembers.

In plain terms: connect it to QQ, a web UI, your shell, your files, a browser, and it gets
work done in all of them. What you discussed this morning, it still knows tonight. A
preference you mentioned three weeks ago doesn't vanish when the context window scrolls.
A plugin crashes and it keeps running. Swap the model and nothing else changes.

```bash
./build/homed -data /tmp/ha      # start the kernel
./build/waiter                    # talk to it
```

All you need is a Linux box. API keys go in through the web UI and live in local SQLite.

---

## What makes it different

**Plugins are separate processes, not shared libraries.** A plugin panic can't take the
kernel down, and it restarts on its own. Drop in a new `plugin.bin` and it's live. No
rebuild, no restart. This isn't architectural purity for its own sake: the `.so` approach
burned us badly enough to justify the rewrite (`dlclose` silently no-ops under
`DF_1_NODELETE`, cgo timeouts leave threads alive, one plugin panic dragged the whole
`homed` process down with it).

**The kernel touches no IO.** No HTTP calls, no file reads, no reach into the world outside
its database. Web UI, QQ, command execution, web search are all plugins. The payoff isn't
tidiness; it's that you can replace the entire interaction layer without touching the kernel.

**Memory sinks as it ages; it doesn't pile up in the context window.** Recent turns stay in
the window, older ones settle into a document layer, oldest get distilled into a graph.
That's why it can run for months without stuffing the whole history into every prompt.

**It remembers scenes, not sessions.** Move from work talk to personal talk and it splits
the context by channel, conversation partner, tools used, and time of day. When a scene
recurs, last time's conclusions come back, including the ones it got wrong.

**Security is a wall, not a line each tool remembers to check.** Everything leaving the
kernel and everything flowing back from plugins passes through it. Secrets never enter the
prompt (they're substituted at execution time), tool allow/deny lists, sensitive-word
alerts, per-principal authorization (the same QQ plugin can grant an owner and a random
group member different powers).

## What it can do

No API listings. What you can point it at:

- **Where it talks**: web UI, CLI/TUI, QQ groups and DMs, other agents (A2A), ACP in your IDE
- **What it sees**: images, video, voice, screenshots, web pages
- **What it does**: read and write files, run commands, write code, drive a real Chrome, calendar, notes, RSS, music
- **What it remembers**: three-layer memory plus graph memory plus a knowledge base, recallable by scene
- **How it schedules**: chat with a human while long jobs run in parallel sub-agents; jump the queue when the user pushes back; resume interrupted work where it stopped

28 built-in plugins, 89 model-callable tools. External plugins live one-per-repo at
[github.com/HologramAgent](https://github.com/HologramAgent).

## How it works

```go
homed  (kernel: LLM orchestration + memory + retrieval)
   ↑ PluginSDK
plugins  (web UI / QQ / CLI / files / network / all IO)
```

Three channels between kernel and plugins: stdio JSON-RPC for control, shared memory for
bulk data, an event ring for notifications. Large objects aren't serialized and copied twice.

Incoming messages hit a **scheduler** first, not the model. Queued work and interruptions
are separate classes; interruptions come in four levels ("how badly can this wait"), and a
higher level preempts a lower one while saving its state. Same level waits its turn.

Then seven pipeline stages, and the model can call tools in a loop. Multiple tool calls in
one turn are **serial by default**; to run them in parallel a tool must declare itself safe.
One undeclared tool in a batch drops the whole batch back to serial.

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
