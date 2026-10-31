> ⚠️ **AI-Assisted Programming Notice**: Parts of this project's code, documentation, and commit history were generated or modified with AI assistance. Key changes have been human-reviewed, but please evaluate and verify before use.

# HologramAgent

> **中文**: [README.md](./README.md)

Self-hosted agent kernel. Connects to QQ, a web UI, your shell, files, and a browser. Runs for months without a restart and keeps what it learned.

```bash
make build build-cli
./build/homed -data /tmp/ha
./build/waiter
```

API keys go in the web UI at `http://localhost:8080`. Requires Linux.

## What it does

- **Channels**: web UI, CLI, QQ groups and DMs, A2A, ACP in your IDE
- **Tools**: files, shell, code, browser (real Chrome), calendar, notes, RSS, music
- **Modalities**: images, video, voice, screenshots, web pages
- **Memory**: three layers (context → document → graph) plus a knowledge base, recalled by scene
- **Scheduling**: parallel sub-agents, interrupt preemption, resumable state

28 built-in plugins, 89 model-callable tools. External plugins live one per repo under [github.com/HologramAgent](https://github.com/HologramAgent).

## Architecture

```go
homed  (kernel: LLM orchestration, memory, retrieval)
   ↑ PluginSDK
plugins  (all IO: web UI, QQ, CLI, files, network)
```

The kernel makes no IO calls. Plugins run as separate processes and talk to it over three channels: JSON-RPC on stdio for control, shared memory for bulk data, an event ring for notifications. A plugin panic doesn't take the kernel down.

Memory sinks over time. Recent turns stay in context, older ones become documents, oldest get distilled into graph triples.

Memory is partitioned by scene rather than by session. A scene is made of channel, conversation partner, tools used, and time of day, so returning to a topic recalls what was concluded last time.

## Documentation

- [Architecture](assets/docs/en/ARCHITECTURE.md) ([中文](assets/docs/zh/ARCHITECTURE.md))
- [Plugin development](assets/docs/en/PLUGIN_DEV.md)
- [SDK docs](https://sdk.homeagent.jianfgit.xyz/) · [llms.txt](https://sdk.homeagent.jianfgit.xyz/llms.txt)
- [Changelog](CHANGELOG_EN.md) · [Design docs](docs/zh/README.md) · [Future plan](docs/zh/future-plan.md)

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
