> ⚠️ **AI-Assisted Programming Notice**: Parts of this project's code, documentation, and commit history were generated or modified with AI assistance. Key changes have been human-reviewed, but please evaluate and verify before use.

# HologramAgent

> **中文**: [README.md](./README.md)

Self-hosted agent kernel. Runs on your own hardware; plugins attach as separate processes.

```bash
make build build-cli
./build/homed -data /tmp/ha
```

API keys go in the web UI at `http://localhost:8080`. Requires Linux.

## Design notes

**Startup order is resolved at build time.** Twenty kernel modules declare `Requires`/`Provides`.
The build computes a 6-layer graph into `depgraph.bin` and injects its digest into the binary via
`-ldflags`. At runtime the kernel reads the graph rather than recomputing it. A mismatch between
graph and binary refuses startup.

**Plugins launch as soon as their facilities are ready.** A plugin declaring
`requires_facilities: [config]` starts when layer 1 completes instead of waiting for the agent
layer at layer 4. That saves a measured 35 seconds, which the agent layer spends on dense
backfill of 750 events and reindexing.

**Two input classes, four interrupt levels.** Queued input has no level, so any interrupt can
preempt it. Interrupts carry L1–L4: higher preempts lower and pushes the preempted state onto a
stack for later resume, same level never preempts. L4 is kernel-only; plugins declaring it get
clamped to L3. Preemption count can raise a level by at most two steps
(`preemptPromotionCap = 2`), so low-priority work can't starve indefinitely.

**Memory is partitioned by scene.** A fingerprint is built from channel, partner, tools, topic,
and time of day, weighted 1.0/1.0/0.8/0.4/0.2. A scene exists only after the second matching
fingerprint (`minSceneEvidence = 2`). Join threshold 0.5, recall 0.35 — recall is looser because
failing to recall loses information while an extra scene costs a few lines of context.

## Architecture

```
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

The kernel makes no HTTP calls and reads no files. Everything that touches the outside world is a
plugin. A plugin crash leaves the kernel running; restart backs off 1s / 2s / 3s, and the 4th
crash within 5 minutes stops automatic restarts for human intervention.

**Seven stages after a message arrives** (`internal/agent/core/stages.go`):

| Stage | When | Plugins can |
|---|---|---|
| `on_input` | message arrives | blocklist, rate limit, short-circuit reply |
| `pre_action` | context ready, before LLM | inject external data |
| `post_action` | LLM returns text and tool calls | filter, force redirect |
| `before_toolcall` | before one tool runs | audit, deny, rewrite args |
| `after_toolcall` | after one tool runs | redact, sort results |
| `before_output` | final text ready | format adaptation |
| `after_output` | already sent | statistics |

`post_action → [before_toolcall → run → after_toolcall] → post_action` is the inner loop. It exits
when the model stops calling tools, all calls are denied, or the limit is reached.

**Three memory layers**: context (last 10 always protected) → documents (cold data sinks) → graph
database (distilled into triples). Media are first-class graph nodes, not attachments.

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
