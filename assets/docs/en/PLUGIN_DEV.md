**中文** | [English](../en/PLUGIN_DEV.md)

# HomeAgent Plugin Development Guide

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## Overview

All external interaction capabilities of HomeAgent comes from plugins. Plugins interact with the kernel through `PluginSDK` (Go API).

**SDK Repository**: Plugin development tools, template code, and example plugins are hosted in the [homeagent-sdk](https://github.com/JianFeeeee/homeagentsdk) repository.

```bash
git clone https://github.com/JianFeeeee/homeagentsdk.git
cd homeagent-sdk
```

Each plugin implements a three-method interface:

```go
type Plugin interface {
    Name() string
    Start(sdk *PluginSDK) error
    Stop() error
}
```

### Three Development Methods

| Method | Use Case | Complexity |
|--------|----------|------------|
| **Subprocess plugin (recommended)** | Independently distributed third-party plugins | Medium, generated using `hmapdev` toolchain |
| **Built-in plugin** | Released with HomeAgent | Simple, requires merging into main repo |
| **Lua script plugin** | Lightweight rapid prototyping | Simple, generated using `hmapdev init --lua` |

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 1. Quick Start: Using the hmapdev Toolchain

`hmapdev` is the unified plugin development toolchain provided in the SDK repository, supporting both Go and Lua
plugin types, and producing `.hmap` plugin bundles (the tool is named after that package format).

> Rename note: as of 1.2.0 the toolchain was renamed from `plugindev` to `hmapdev`; the SDK store moved from
> `~/.homeagent/plugindev/sdk` to `~/.homeagent/hmapdev/sdk` (the old directory keeps working automatically).

### Installation

```bash
cd homeagent-sdk/tools/hmapdev
go build -o hmapdev
# Add hmapdev to PATH or use directly
# Prebuilt binaries also ship as release assets (hmapdev_linux_amd64, ...)
```

### SDK Version Management

`hmapdev sdk` manages local SDK versions:

```bash
hmapdev sdk list       # list installed SDK versions
hmapdev sdk current    # show current SDK version
hmapdev sdk latest     # show latest available version
hmapdev sdk install v1.2.0  # install a specific version
hmapdev sdk use v1.2.0      # switch to a version
hmapdev sdk path       # show current SDK path
```

SDK is stored at `~/.homeagent/hmapdev/sdk/<version>/`; `hmapdev init` reads the current SDK version for `go.mod`.

### Source Debugging

`hmapdev debug` interprets plugin source and prints a call trace, no compilation environment needed:

```bash
hmapdev debug [dir]   # dir defaults to the current directory
```

### Creating a Go Plugin

```bash
hmapdev init myplugin
cd myplugin
# Edit plugin code
vim plugin.go
# Build and package (default is a multi-platform bundle, see below)
hmapdev build
# Output: dist/myplugin_bundle.hmap
# Single-platform build:
hmapdev build --no-bundle
# Output: dist/myplugin_linux_amd64.hmap (or windows_amd64)
```

### Creating a Lua Plugin

```bash
hmapdev init myluaplugin --lua
cd myluaplugin
# Edit plugin code
vim main.lua
# Local test
lua main.lua
# Build and package
hmapdev build
# Output: dist/myluaplugin_lua.hmap
```

### Template Project Structure

**Go plugin**:

```
myplugin/
├── plg.json       — Plugin metadata (name, version, entry, target platforms)
├── plugin.go      — Plugin implementation (Plugin interface + NewPlugin export)
├── go.mod         — Go module definition
├── README.md      — Documentation
└── thirdpart/     — Optional external source code directory
```

Subprocess runtime files (`z_proc_gen.go` and friends) are auto-generated at build time.

**Lua plugin**:

```
myluaplugin/
├── plg.json       — Plugin metadata (entry: "main.lua", targets: "lua")
├── main.lua       — Plugin implementation (Lua version of Plugin interface)
├── sdk.lua        — SDK mock layer (supports `lua main.lua` standalone testing)
└── README.md      — Documentation
```

### Build & Package

`hmapdev build` automatically handles compilation and packaging:

```bash
cd myplugin
hmapdev build                      # default bundle mode (multi-platform)
hmapdev build --no-bundle          # single-target build (per plg.json targets)
hmapdev build --target linux/amd64 # append a target on top of plg.json targets
hmapdev build --outdir dist        # output directory (default: dist)
hmapdev build --sdk-path <path>    # SDK path override (go.mod replace)
hmapdev build --replace <mod@path> # append a go.mod replace directive (repeatable)
```

Execution process:
1. Reads `plg.json` `targets`/`bundle` fields to determine build targets (bundle takes priority, see below)
2. Auto-generates subprocess runtime code (`z_proc_gen.go` + `z_proc_shm_unix.go` + `z_proc_shm_windows.go`)
3. **Go plugin**: Runs `go build` (a plain executable, `CGO_ENABLED=0`)
4. **Lua plugin**: Packages source code directly, no compilation needed (contents: `plugin.json` + `main.lua`, plus optional `README.md`, `LICENSE`, `thirdpart/*.lua`)
5. Generates `plugin.json` output manifest
6. Packages as `.hmap` distribution (zip format, containing `plugin.json` + binary)

### plg.json (project config) vs plugin.json (output manifest)

| File | Purpose | Key fields |
|------|---------|------------|
| `plg.json` | Project metadata, maintained by developer | `targets` — single-target build list (e.g. `"linux/amd64,windows/amd64"`); `bundle` — multi-platform bundle switch (default `true`) |
| `plugin.json` | Build artifact manifest, auto-generated | `entry` — entry filename; `platforms` — declared platforms |

Each target produces a separate `.hmap`. Subprocess plugins are plain executables with
**no platform-specific extension**:

| Platform | Binary |
|----------|--------|
| Linux / macOS / Windows | `plugin.bin` |

Inside a bundle package the per-platform entries are named `plugin.bin.<goos>.<goarch>`;
the kernel picks the one matching the current platform and renames it to `plugin.bin`.

> ⚠️ **v1.0.0 breaking change**: external plugins moved from C ABI shared libraries to
> **subprocess + shared memory**.
>
> - `plugin.so` / `plugin.dylib` / `plugin.dll` are **no longer loaded**. The new kernel
>   skips legacy artifacts with an actionable error instead of crashing.
> - **Business code needs no changes** — the public SDK interface is unchanged; just
>   rebuild with the new `hmapdev` (formerly `plugindev`).
> - The `entry` field in `plg.json` is **meaningless for Go plugins** now (leaving
>   `plugin.so` there is harmless); it only distinguishes Lua plugins.
> - Artifacts no longer need cgo, so cross-compiling requires no target C toolchain.
> - Windows went from "only 3 stage fields delivered, no writeback" to all 16 fields
>   visible plus writeback, sharing the same RPC implementation as Unix.

### Build Targets & Multi-platform Bundle

**`hmapdev build` defaults to bundle mode** (unless `plg.json` explicitly sets `"bundle": false`): it builds linux/amd64 + darwin/amd64 + windows/amd64 in one pass, producing a single `.hmap` with all platform binaries. The output manifest includes a `platforms` field. The kernel auto-selects the correct binary during installation.

```bash
hmapdev build              # default bundle, outputs dist/myplugin_bundle.hmap
hmapdev build --bundle     # explicitly enable bundle (same as above)
hmapdev build --no-bundle  # disable bundle, build per plg.json targets
```

Notes:
- In bundle mode the `plg.json` `targets` field is ignored; the three platforms above are always built
- Cross-compilation needs the corresponding toolchains (e.g. building darwin on Linux requires clang/macOS SDK); if a toolchain is missing the build fails — use `--no-bundle` to build only the current platform
- Single-target output naming: `{name}_{os}_{arch}.hmap`, e.g. `myplugin_linux_amd64.hmap`

Output in `dist/` directory:
```
dist/
├── myplugin_bundle.hmap           # default bundle: multi-platform
├── myplugin_linux_amd64.hmap      # after --no-bundle: Linux
├── myplugin_windows_amd64.hmap    # after --no-bundle: Windows
├── myplugin_darwin_amd64.hmap     # after --no-bundle: macOS
└── myplugin_lua.hmap              # Lua plugin
```

### Deployment

Install via PluginMgr HTTP API (three methods):

```bash
# 1. Install from URL (http/https only, streamed, no local temp file)
curl -X POST http://127.0.0.1:9876/plugins \
  -H "Content-Type: application/json" \
  -d '{"url": "https://example.com/myplugin.hmap"}'

# 2. Install from local path (reads the given file, source file untouched)
curl -X POST http://127.0.0.1:9876/plugins \
  -H "Content-Type: application/json" \
  -d '{"path": "/path/to/myplugin.hmap"}'

# 3. Upload binary directly
curl -X POST http://127.0.0.1:9876/plugins \
  --data-binary @dist/myplugin.hmap
```

`9876` is the pluginmgr local port (defaults to listening on 127.0.0.1 only, no auth).

Reload plugins via `/api/v1/plugins/reload` or restart the kernel to activate.

Or upload via the WebUI plugin management page, or through the WebUI HTTP API (default port 8080, requires the `api_key` bearer token; it proxies to pluginmgr):

```bash
curl -X POST http://127.0.0.1:8080/api/v1/plugins \
  -H "Authorization: Bearer <api_key>" \
  -H "Content-Type: application/json" \
  -d '{"path": "/path/to/myplugin.hmap"}'
```

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 2. Go Plugin Development in Detail

### Plugin Interface

```go
package main

import "github.com/JianFeeeee/homeagentsdk/sdk"

type Plugin struct {
    name string
    sdk  *sdk.PluginSDK
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
    p.sdk = s
    // Register config items, tools, stage hooks, etc.
    return nil
}

func (p *Plugin) Stop() error {
    // Clean up resources
    return nil
}

// NewPluginFactory creates plugin instance (called by main.go or Windows bridge)
func NewPluginFactory(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return &Plugin{name: name}, nil
}
```

### Entry Point

`hmapdev init` generates `plugin.go` with the `NewPlugin` export function directly,
which is the entry point when the kernel loads the plugin:

```go
func NewPlugin(name string, config map[string]interface{}) (sdk.Plugin, error) {
    return &Plugin{name: name}, nil
}
```

At build time, `hmapdev build` auto-generates subprocess runtime code
(`z_proc_gen.go` for the platform-independent part, plus `z_proc_shm_unix.go` /
`z_proc_shm_windows.go`). All three platforms share the same entry point and the same
RPC logic; only the cross-process resource-passing mechanism differs (inherited fds on
Unix, named kernel objects on Windows). No manual bridge code needed.

### PluginSDK Core API

#### Tool Registration — Make your capabilities callable by LLM

```go
s.RegisterTool("weather_query", sdk.ToolDef{
    Name:        "weather_query",
    Description: "Query weather for a specified city",
    NoMemory:    false,                            // false=output participates in memory, true=skip
    // Cleaner:  func(output string) string {     // Optional: clean output before vector/jieba/distill
    //     return extractJSON(output, "content")
    // },
    Parameters: map[string]interface{}{
        "type": "object",
        "properties": map[string]interface{}{
            "city": map[string]interface{}{
                "type":        "string",
                "description": "City name, e.g. Beijing",
            },
        },
        "required": []string{"city"},
    },
}, func(args map[string]interface{}) (interface{}, error) {
    city, _ := args["city"].(string)
    return map[string]interface{}{
        "city":    city,
        "temp":    25,
        "weather": "Sunny",
    }, nil
})
```

##### NoMemory and Cleaner

`NoMemory` and `Cleaner` are optional fields on `ToolDef` that control how tool output participates in the **memory computation layer** (vectorization, jieba tokenization, distillation):

- **`NoMemory`** (default `false`): When `true`, the tool's output is excluded from all memory computation (vector, tokenization, distillation), but the original text is preserved in Context and Document. LLM attention is unaffected. Use cases: `cmd_run` (unpredictable noise in command output), pure operation tools like file upload/delete.

- **`Cleaner`** (optional): A function `func(output string) string`. When set, the tool output is filtered through this function before participating in vectorization/jieba/distillation. Typical use: stripping SQL prefixes, extracting a `content` field from JSON. The original output is never modified — Cleaner only affects the computation layer input.

Decision matrix:

```
Tool output → valuable for LLM attention?
  ├── No  → NoMemory=true (output preserved, skipped in computation)
  └── Yes → Contains cleanable noise?
       ├── Yes → Cleaner filters before computation
       └── No  → Normal memory, no extra handling
```

> **Note**: `Cleaner` is a Go `func` type (`json:"-"`), cannot be serialized across process boundaries, so it is unavailable for C/C++/Rust remote plugins. **Lua plugins are not affected**: pass a Lua function in the def table (`cleaner = function(text) return text end`) — the Go bridge calls it back per invocation during memory computation.

#### Stage Hooks — Intervene in message processing flow

7 stages:

| Stage | Timing | Purpose |
|-------|--------|---------|
| `on_input` | Message just arrived at Agent | Blacklist, rate-limit, short-circuit |
| `pre_action` | About to call LLM | Inject context |
| `post_action` | LLM returned results | Modify output/tool list |
| `before_toolcall` | Before tool execution | Audit, reject, modify params |
| `after_toolcall` | After tool execution | Desensitize, rewrite results |
| `before_output` | Before output | Format adaptation, leak cleanup |
| `after_output` | After output | Statistics/logging |

```go
// Global: receive all stage events
s.RegisterStage(sdk.StagePreAction, func(ctx *sdk.StageContext) error {
	ctx.Lock()
	ctx.ContextMsgs = append(ctx.ContextMsgs, map[string]interface{}{
		"role":    "system",
		"content": "Injected context content",
	})
	ctx.Unlock()
	return nil
})

// Own tools only: only before_toolcall/after_toolcall for this plugin's tools
s.RegisterStage(sdk.StageBeforeToolcall, myHandler, sdk.StageScopeOwnTools)
```

#### Configuration Management

```go
// Register config definition
s.Settings().RegisterDef(sdk.ConfigDef{
    Key:         "plugin.myplugin.api_key",
    Default:     "",
    Type:        "string",
    DisplayName: "API Key",
    Description: "API key",
    Category:    "myplugin",
})

// Read/write config
val, err := s.Settings().Get("api_key")
s.Settings().Set("api_key", "new-value")

// Read core config
s.Settings().GetCore("llm.model")

// Read other plugin's config
s.Settings().GetPlugin("other_plugin", "some_key")
```

#### Input Delivery

```go
// Normal delivery (processed in order)
s.InjectText(source, channel, text string)

// Interrupt delivery (can interrupt current LLM processing)
s.InjectInterruptText(source, channel, text string)

// No memory recording
s.InjectTextNoMemory(source, channel, text string)
```

#### Input Channel Registration — Declare External Message Sources

```go
s.RegisterInputChannel("qq", sdk.ChannelDef{
    NoMemory: true,
    Cleaner: func(text string) string {
        return strings.TrimSpace(text)
    },
})
```

`ChannelDef` controls channel behavior in the memory computation layer:

| Field | Default | Description |
|-------|---------|-------------|
| `NoMemory` | `false` | Channel input/output skips vectorization/keyword/distillation; original text preserved in context |
| `Cleaner` | `nil` | `func(string) string` computation filter (does not modify original text) |

Noisy sources (QQ group messages, RSS feeds, etc.) should set `NoMemory: true`.

#### Output Channel Registration — Declare Output Destinations

```go
s.RegisterOutputChannel("email", 1, "Send Email", sdk.ChannelDef{
    NoMemory: true,
}, func(args map[string]interface{}) (interface{}, error) {
    to, _ := args["to"].(string)
    subject, _ := args["subject"].(string)
    body, _ := args["body"].(string)
    return map[string]interface{}{"status": "sent"}, nil
})
```

Parameters: `name` (route key), `caps` (1=text/2=rich/4=file/8=image), `desc`, `def` (ChannelDef), `handler` (callback).

#### Event Subscription

```go
import "github.com/JianFeeeee/homeagentsdk/sdk"

unsub := s.Events().Subscribe(sdk.EventToolCall, func(evt *sdk.Event) {
    log.Printf("Tool was called: %v", evt.Payload)
})
defer unsub()
```

#### Capability Access

```go
// Graph Memory (entity-relation store)
entities, relations, err := s.Memory().Recall([]string{"keyword"}, 2)

// Document Memory (vector store)
docs := s.DocMemory().Query("query text", 3)

// Knowledge
results, err := s.Knowledge().Search("query", 5)

// LLM source management
s.LLM().ListSources() // returns []string
s.LLM().SetSource("deepseek")
```

#### Event Subscription (built-in plugins)

```go
// Subscribe to system events, returns unsubscribe function
unsub := s.Subscribe("tool_call", func(evt *events.Event) {
    log.Printf("Tool was called: %v", evt.Payload)
})
defer unsub()

// Publish event
s.Publish(&events.Event{
    Type:    "custom_event",
    Payload: map[string]interface{}{"key": "value"},
})
```

#### IO Channel Management (built-in plugins)

```go
// Register a channel (bind device driver), dev must implement the agentIO.Device interface:
//   Name() string
//   Type() DeviceType
//   Description() string
//   Tools() []ToolDef
//   Execute(tool string, args map[string]interface{}) (interface{}, error)
//   Start() error
//   Stop() error
//   OutputCapabilities() OutputCapability
s.RegisterChannel("mydevice", deviceImpl)

// Unregister a channel
s.UnregisterChannel("mydevice")

// List all channels
channels := s.ListChannels()
```

#### Input Delivery (built-in plugins)

```go
// Queued delivery (processed in order)
s.InjectInput(source, channel, eventType string, payload map[string]interface{})

// Synchronous delivery (waits for response)
resp := s.InjectInputSync(source, channel, eventType string, payload map[string]interface{})

// Interrupt delivery (can preempt current LLM processing)
s.InjectInterrupt(source, channel, eventType string, payload map[string]interface{})

// Synchronous text shortcuts
resp := s.InjectTextSync(source, channel, text string)
resp := s.InjectTextSyncNoMemory(source, channel, text string)

// Get output channel
outputCh := s.OutputChan()
```

> **Note**: `Subscribe`, `Publish`, `RegisterChannel`, `UnregisterChannel`, `ListChannels`, `InjectInput`, `InjectInputSync`, `InjectInterrupt`, `InjectTextSync`, `InjectTextSyncNoMemory`, `OutputChan` are only available in built-in plugins (`internal/sdk` package). External dynamic plugins should use the public APIs: `InjectText`, `InjectInterruptText`, `InjectTextNoMemory`.

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 3. Lua Plugin Development in Detail

Lua plugins are suitable for lightweight rapid prototyping, requiring no Go compilation environment. Changes take effect after kernel restart.

### Execution Model

Lua plugins run inside the kernel process on a gopher-lua interpreter (single Lua state guarded by a mutex). This differs fundamentally from Go plugins:

- **Passive callback model**: `main.lua` executes only once at load time. Afterward, tools, stage hooks, output/input channels, and registered APIs are all invoked by the kernel via callbacks into Lua functions. Plugins cannot start background tasks on their own.
- **No concurrency / no long-running services**: Lua has no goroutines, coroutine scheduling, `os`/`io` libraries, or socket listening. The only outbound capability is `sdk.http.get/post` (synchronous). Any blocking loop will stall every call of that plugin while holding the lock.
- **For long-running services (listening on a port, background polling, timers) use a Go plugin** (`plugin.bin` built with the toolchain, which may spawn goroutines — see the webui/cli plugins). The Lua equivalent is event-driven: register tools/stage hooks/channels to be called back by the kernel, or interact with external processes via `sdk.http`.

### Plugin Structure

```lua
-- main.lua
local plugin = {
  name = "myluaplugin"
}

function plugin.start(sdk)
  sdk.log("info", "myluaplugin starting...")

  sdk.register_tool("myluaplugin_hello", {
    description = "A hello world tool",
    parameters = {
      type = "object",
      properties = {}
    }
  }, function(args)
    return { content = "Hello from myluaplugin plugin!" }
  end)

  sdk.log("info", "myluaplugin started")
end

function plugin.stop()
  sdk.log("info", "myluaplugin stopped")
end

return plugin
```

### SDK Mock Layer

`sdk.lua` provides a pure Lua SDK mock implementation, supporting `lua main.lua` standalone testing:

```bash
lua main.lua
# Output:
# [lua-plugin] info: myluaplugin starting...
# [lua-plugin] register_tool: myluaplugin_hello
# [lua-plugin] info: myluaplugin started
```

When running inside the kernel, `sdk.*` global variables are injected by the Go layer, and all functions marked with `-- !impl` are replaced with real implementations.

### Lua SDK API

The `sdk.*` API of Lua plugins is aligned with external plugins (toolchain-built `plugin.bin` subprocesses) up to **SDK 1.3.0** (requires kernel **1.4.0+**, also backfilled by the Lua-alignment patch `v1.3.11`): registration functions raise a Lua error on failure; data functions uniformly return `(result, err)` with `err == nil` on success. Subsystems not wired by the core (e.g. SocialAPI) return empty values instead of errors.

> Historical note: the 1.1–1.3 media / inject-flags / priority capabilities were long available only on the Go side and were silently missing on the Lua side. They are now fully aligned, guarded by the contract test in `internal/plugin/lua_surface_test.go` (every function promised by the mock has a runtime binding).

**Registration**

| Function | Description |
|----------|-------------|
| `sdk.log(level, msg)` | Log output |
| `sdk.register_tool(name, def, handler)` | Register tool; `def` supports `description`, `parameters`, `no_memory`, `context_policy` (`"none"`/`"prune"`), `cleaner` |
| `sdk.register_stage(stage, handler, scope)` | Register stage hook; `scope` is `nil`/`"global"` (default) or `"own_tools"` (fires only for `before_toolcall`/`after_toolcall` when the tool belongs to this plugin) |
| `sdk.register_api(name)` | Register API |
| `sdk.register_output_channel(name, caps, desc, def, handler)` | Register output channel; `def` supports `no_memory`, `context_policy`, `cleaner` |
| `sdk.register_input_channel(name, def)` | Register input channel; `def` as above |
| `sdk.unregister_output_channel(name)` | Unregister an output channel (for resource-bound channels, e.g. remote devices); returns `(nil, err)` |
| `sdk.set_auto_restart(enabled)` | Auto-restart the plugin after a crash |

**Stage hook context**

Stage handlers receive the full context (same as external plugins): `raw_message`, `user_id`, `group_id`, `phase`, `llm_text`, `reasoning_content`, `final_text`, `no_memory`, `context_msgs`, `token_usage`, `memory`, `extra`, `errors`, `response` (when responded), `tool_calls`, `tool_results`.

**Stage writeback**: the `ctx` table passed to the handler is a reference — mutating writable fields inside the handler syncs back to the core `StageContext` (aligned with subprocess external-plugin capability):

```lua
sdk.register_stage("on_input", function(ctx)
  ctx.raw_message = "[clean]" .. ctx.raw_message   -- modify input, adopted by core
end)

sdk.register_stage("post_action", function(ctx)
  ctx.llm_text = ctx.llm_text .. "[tail]"            -- modify LLM output
  ctx.tool_results = { { call_id = "x", result = "rewritten" } }
end)
```

Writable fields: `raw_message`, `llm_text`, `final_text`, `user_id`, `group_id`, `no_memory`, `response`, `tool_calls`, `tool_results`. Other fields are read-only.

**IO and config**

| Function | Description |
|----------|-------------|
| `sdk.get_setting(key)` / `sdk.set_setting(key, value)` | Own plugin config read/write |
| `sdk.settings.get_core/set_core/list_core(key)` | Core config read/write |
| `sdk.settings.get_plugin/set_plugin/list_plugin(plugin, key)` | Other plugin config read/write |
| `sdk.settings.list/defs/dump/plugins(prefix)` | Config queries |
| `sdk.settings.register_def(def)` | Register config definition (WebUI display) |
| `sdk.inject_text(source, channel, text)` | Deliver text message |
| `sdk.inject_interrupt(source, channel, text)` | Interrupt delivery |
| `sdk.inject_text_no_memory(source, channel, text)` | Deliver without memory computation |
| `sdk.inject_text_opts` / `sdk.inject_interrupt_opts(source, channel, text, opts)` | Delivery with flags; `opts = { no_memory=bool, context_policy="none"|"prune", cleaner_name=string, priority="L1".."L3" }` |
| `sdk.inject_input_sync(source, channel, text)` | ⚠️ **Unavailable in Lua**: always returns `(nil, err)`. It waits for this turn's reply while a Lua callback holds the plugin lock, so it would self-deadlock. Use a Go plugin for synchronous waits, or the async injectors below |
| `sdk.inject_input_sync_opts(source, channel, text, opts)` | Same (unavailable) |
| `sdk.inject_input_media(source, channel, text, blocks)` | Inject text + multimodal content blocks |
| `sdk.inject_input_media_opts(source, channel, text, blocks, opts)` | Same, with flags |
| `sdk.inject_input_media_sync` / `..._sync_opts(...)` | ⚠️ **Unavailable in Lua** (same as `inject_input_sync`) |
| `sdk.inject_interrupt_media(source, channel, text, blocks)` | Interrupt delivery with media |
| `sdk.inject_interrupt_media_opts(source, channel, text, blocks, opts)` | Same, with flags |
| `sdk.set_tool_blocks(blocks)` | Set multimodal blocks carried by the next tool message (lets the model see images / hear audio) |

Each `blocks` item: `{ type="text", text="..." }`, `{ type="image_url", image_url={ url="...", detail="high" } }`, or `{ type="audio_url", audio_url={ url="..." } }`. An absent `opts` is the zero value (recorded in memory + no pruning), equivalent to the three-argument form.

**Data APIs (aligned with subprocess external plugins, all return `(result, err)`)**

| Sub-table | Functions |
|-----------|-----------|
| `sdk.memory.*` | `recall(query, depth)`, `commit({triples})` (triple supports `subject/relation/object/confidence/subject_type/object_type/sentence_text/media_digests`), `introspect()`, `merge(source, target)`, `purge(criteria, hard)` |
| `sdk.doc.*` | `query(text, top_k)`, `insert({id,title,content})`, `insert_with_media(doc, attachments)`, `remove(id)`, `stats()` |
| `sdk.knowledge.*` | `search(query, limit)`, `add(tag, content)`, `list()` |
| `sdk.text_memory.*` | `append({role,content,timestamp,channel,attachments})` |
| `sdk.llm.*` | `list_sources()`, `set_source(name)`, `current_source()` |
| `sdk.social.*` (read-only) | `get_person(name)`, `get_network(name, depth)`, `get_trait(name, trait)`, `get_relations(name)`, `list_persons()` |
| `sdk.events.*` | `subscribe(event_type, handler)` → returns an unsubscribe function; handler receives `{type,source,timestamp,payload}` |
| `sdk.plugin_mgr.*` | `reload_one(name)`, `list_loaded()`, `is_disabled(name)` |
| `sdk.json.*` | `encode(val)`, `decode(str)` |
| `sdk.http.*` | `get(url)`, `post(url, body, content_type)` |

Each `attachments` item: `{ digest=, mime=, name=, data=<base64> }`; with `data` it is new content (stored in the content-addressed store), with only `digest` it references existing content.

> The `sdk.events.subscribe` callback runs on the kernel's event-publishing goroutine, and Lua is single-state + mutex-guarded — **do only lightweight forwarding inside the callback; never block**, or every call of this plugin will stall.

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 4. Built-in Plugins

Built-in plugins use `init()` self-registration, compiled into the kernel, no separate deployment needed.

### Directory Structure

```
internal/plugins/yourplugin/
    plugin.go       — Plugin main file
```

### Minimal Plugin Example

```go
package yourplugin

import (
    "gitcode.com/JianFeeeee/HomeAgent/internal/plugin"
    sdk "gitcode.com/JianFeeeee/HomeAgent/internal/sdk"
)

func init() {
    plugin.RegisterFactory("yourplugin", func(name string, config map[string]interface{}) (sdk.Plugin, error) {
        return New(name), nil
    })
}

type Plugin struct {
    name string
}

func New(name string) *Plugin {
    return &Plugin{name: name}
}

func (p *Plugin) Name() string { return p.name }

func (p *Plugin) Start(s *sdk.PluginSDK) error {
    // Initialize plugin here: start goroutines, register tools, subscribe events, etc.
    return nil
}

func (p *Plugin) Stop() error {
    // Clean up resources
    return nil
}
```

### Register with Kernel

Add blank import in `internal/plugins/all.go`:

```go
package plugins

import (
    _ "gitcode.com/JianFeeeee/HomeAgent/internal/plugins/yourplugin"
    // ... other plugins
)
```

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 5. Best Practices

1. `Start()` is non-blocking — start long tasks in goroutines, don't block Start
2. `Stop()` cleans up resources — close connections, stop goroutines, cancel subscriptions
3. Unique tool names — use plugin name prefix to avoid conflicts
4. When handler returns `error`, LLM will receive it and may retry
5. Use `InjectInterruptText` for interrupts, `InjectText` for normal delivery
6. Use `Settings().Get/Set` for config, don't hardcode
7. External Go plugins compile independently, not tied to kernel version; only built-in plugins need recompilation with kernel

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 6. Plugin Management

### CLI Commands

```bash
/plugin list                  # List all plugins with status (loaded/disabled)
/plugin disable <name>        # Disable plugin (immediate, no longer receives input)
/plugin enable <name>         # Enable plugin (restored after restart)
/plugin reload                # Reload all plugins
```

### WebUI

Dashboard plugin list provides "Disable/Enable" buttons in the actions column. Disabling WebUI itself shows a confirmation dialog to prevent misoperation.

### Built-in Plugin API

```go
pmgr := s.PluginMgr()
pmgr.DisablePlugin("qq", "admin")       // Disable
pmgr.EnablePlugin("qq")                  // Enable
list := pmgr.ListDisabledPlugins()       // List disabled plugins
loaded := pmgr.ListLoadedPlugins()       // List loaded plugins
pmgr.IsPluginDisabled("qq")              // Check if disabled
pmgr.ReloadPlugins()                     // Reload all plugins
```

Internal: records are stored in SQLite `disabled_plugins` table (`name`, `disabled_at`, `disabled_by`). Disabling takes effect immediately (plugin stops receiving input); full removal requires a restart.

> **Note**: `PluginMgr()` returns an interface with **only 3 methods** (`ReloadOne` /
> `ListLoadedPlugins` / `IsPluginDisabled`). Enable/disable/remove/reload-all
> (`EnablePlugin` / `DisablePlugin` / `RemovePlugin` / `ReloadPlugins`) and the
> built-in check (`IsBuiltinPlugin`) exist only on the kernel-internal
> `internal/sdk.PluginManager` — external plugins cannot reach them. To reload from
> an external plugin, use `ReloadOne`.
> The two interfaces have similar names but are **not the same**:
> `sdk.PluginMgrAPI` (public, 3 methods) vs `internal/sdk.PluginManager` (internal, 9).

---

<img src="../../branding/mascot-xiaozhai-avatar.webp" width="20" style="border-radius:50%;vertical-align:middle"> :

## 7. Example Plugin Reference

### SDK Repository Examples (`homeagent-sdk/example/`)

| Example | Type | Features |
|---------|------|----------|
| [weather](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/weather) | Go | Weather queries (wttr.in); demonstrates NoMemory/Cleaner/stage hooks/channels/text memory |
| [luademo](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/luademo) | Lua | Full-featured Lua example covering the whole v0.8.0 Lua SDK surface |
| [qq](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/qq) | Go | NapCat OneBot integration, 20 tools, full input/output channel wiring |
| [memo](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/memo) | Go | Memo management, PreAction injection + timed interrupt dual reminder |
| [files](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/files) | Go | File system operations, 4 write modes, sandbox isolation |
| [browser](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/browser) | Go | Web search + HTTP fetch (SSRF) + Chromium render (merged from web/webfetch) |
| [bili](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/bili) | Go | Bilibili video download (yt-dlp) |
| [editdoc](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/editdoc) | Go | Office document editing and format conversion |
| [a2a](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/a2a) | Go | Agent-to-Agent protocol |
| [ocr](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/ocr) | Go | Offline text recognition (Tesseract) |
| [sanitizer](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/sanitizer) | Go | Output sanitizer filter |
| [calendar](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/calendar) | Go | Calendar management |
| [rss](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/rss) | Go | RSS subscriptions |
| [ai_image](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/ai_image) | Go | AI image generation |
| [music](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/music) | Go | Music playback |
| [vikunja](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/vikunja) | Go | Vikunja task management (projects/tasks/labels CRUD) |
| [vanblog](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/vanblog) | Go | VanBlog publishing and management |
| [deepsearch](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/deepsearch) | Go | Multi-round deep search (progressive focus + cited summary) |
| [acp](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/acp) | Go | Agent Client Protocol (external editors/IDEs drive this agent) |
| [recoverydiag](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/recoverydiag) | Go | Five-part fault diagnosis (triage / sqlite check / log signatures / diff / ranked conclusions); core plugin of failback mode |
| [plugindev](https://github.com/JianFeeeee/homeagentsdk/tree/main/example/plugindev) | Go | Plugin scaffolding: generate, build, install — lets the agent develop plugins itself |

### Built-in Plugins

| Plugin | Location | Features |
|--------|----------|----------|
| Timer | `internal/plugins/timer/` | Simplest complete example, registers one tool + interrupt feedback |
| CLI | `internal/plugins/cli/` | Unix socket listener + synchronous request-response |
| WebUI | `internal/plugins/webui/` | HTTP service + dependency injection |

> **timer has been moved out of the built-in plugins** and is now provided as an external
> hmap plugin (all three tools `timer_set` / `timer_list` / `timer_cancel` and the `timer`
> input channel are registered by the external plugin).
>
> ★ **Upgrade order**: install the timer plugin *before* upgrading the kernel. Doing it the
> other way round leaves a window where none of the three timer tools exist — and it
> **fails silently**: no error, the tools simply vanish (the LLM cannot see them, which
> surfaces as "reminders are gone" with nothing in the logs to explain it).
>
> The external version is also a strict superset: the built-in one only had `timer_set`, and
> in-flight countdowns were lost on restart; the external one adds `timer_list` / `timer_cancel`
> and persists pending timers across restarts.

---

*Want to understand the project goals? See [OVERVIEW.md](OVERVIEW.md).*
*Want to understand the architecture? See [ARCHITECTURE.md](ARCHITECTURE.md).*
*SDK repository and development tools? See [homeagent-sdk](https://github.com/JianFeeeee/homeagentsdk).*
