// Package meta 收集 HologramAgent SDK 的全部元数据。
// 版本号应与核心 meta.Version 保持一致。
package meta

var (
	// Version 是 HologramAgent SDK 版本号。
	// 通过 `-ldflags="-X github.com/JianFeeeee/homeagentsdk/meta.Version=vX.Y.Z"` 注入。
	//
	// 版本号语义：**SDK 版本跟随核心的中版本，patch 位恒为 .0**。
	// 整条核心 1.1.x 线（1.1.0、1.1.1、1.1.7…）共用 SDK 1.1.0；
	// 只有核心进入 1.2.0 这种中版本跃迁时 SDK 才升到 1.2.0。
	// 这样插件开发者只需关心「我在为哪个中版本写插件」，
	// 不必跟着核心的每个 bugfix 换 SDK 依赖（见 核心仓 docs/git-branching.md §七）。
	//
	// 1.0.0：插件运行模型从 C ABI 动态库改为子进程 + 共享内存。
	//        公开 SDK 接口零改动，但产物形态变了（plugin.so → plugin.bin）。
	// 1.1.0：多模态贯通插件边界。**全部是新增，无签名变更**：
	//          - Triple.SentenceText / Triple.MediaDigests
	//          - Doc.MediaDigests / Doc.Attachments、MediaAttachment
	//          - TextEvent.Attachments
	//          - DocMemoryAPI.InsertWithMedia
	//          - IOInjector 的 InjectInputMedia / InjectInputMediaSync /
	//            InjectInterruptMedia；PluginSDK 补上缺失的 SetToolBlocks 包装
	//        同版修掉两处并发竞态（sdk/stress_test.go 的 -race 实证，不是理论风险）：
	//        PluginSDK 的 API 字段与 autoRestart 标志此前无锁，而写方
	//        （内核注入 API、插件 SetAutoRestart）与读方（插件后台 goroutine
	//        注入、内核 registry 读 AutoRestart）天然跨 goroutine。
	//        存量插件不需要改一行也不需要重编：新增方法由**插件调用、内核实现**，
	//        不调就不受影响。想用新字段的插件重编即可。
	//
	// 1.2.0：注入行为的记忆/裁剪标志位。**全部是新增，无签名变更**：
	//          - InjectOptions{NoMemory, ContextPolicy}
	//          - IOInjector 的六个 *Opts 变体（排队/中断/同步/带媒体各一对）
	//          - ChannelDef.ContextPolicy（顺带给 ChannelDef 补上 JSON tag：
	//            它要跨进程传给内核，而 Cleaner 是函数必须忽略；无 tag 时只能
	//            手写字段白名单，新增字段会被静默丢掉）
	//        语义：零值 InjectOptions 与旧的三参数方法完全等价（记入记忆 +
	//        不裁剪），因此存量插件不需要改一行也不需要重编。
	//        裁剪（ContextPolicy=prune）必须显式声明——它会归档丢弃低相关事件。
	//
	// ❗main 分支上此值是**下一个未发布中版本**；已发布的值看对应的
	// release/vX.Y.x 分支与 tag（见 核心仓 docs/git-branching.md §2.1 与 §七.1）。
	//
	// 现为 1.4.0：1.3.0 已随核心的正式 tag `v1.3.0` 定版并发版（本仓 tag v1.3.0、
	// release/v1.3.x 承载它），该号从此归发布线所有，main 遂推进到下一个未发布中版本。
	//
	// ❗本仓**不发 patch tag**（§七.1）：一个中版本只发一次 `vX.Y.0`，核心的 1.3.x
	// 后续 patch **不伴随 SDK 发版** —— patch 位恒为 `.0`，带非零 patch 的 SDK tag
	// 都是错的。（2026-09-13 曾误发 `v1.3.1`，已撤回；`v1.2.1` 是同一类历史遗留。）
	//
	Version = "1.4.0"

	// Commit 是构建时的 Git commit hash。
	Commit = "unknown"

	// BuildTime 是构建时间。
	BuildTime = "unknown"

	// SDKName 是 SDK 名称。
	SDKName = "HologramAgent SDK"

	// SDKModule 是本 SDK 仓**当前**的 Go module path。
	//
	// ★ 演进史（三次）：`gitcode.com/JianFeeeee/homeagent-sdk`
	//   → `github.com/JianFeeeee/homeagentsdk`（迁到 GitHub）
	//   → `github.com/JianFeeeee/hologramagentsdk`（与仓名统一，2026-10）
	//
	// ★★ 本常量**必须与 go.mod 的 module 行逐字一致**（2026-10-29 实测缺陷）：
	//
	//   它此前停在中间名 `github.com/JianFeeeee/homeagentsdk`，而 go.mod 已
	//   推进到 `...hologramagentsdk`。后果不是"显示错"，而是**模板工程编不出**：
	//
	//     · hmapdev 用本常量生成插件 go.mod 的 require/replace
	//     · 而 SDK 内部（如 proc_main 运行时模板）引用的是 go.mod 的真名
	//     ⇒ 两个模块名并存，`go build` 报
	//       "no required module provides package .../sdk"
	//     ⇒ 用户照提示 `go get` 只会**再拉一份** SDK，问题依旧。
	//
	//   因此凡改 module 名，必须同时改 go.mod 与本常量。判据见
	//   `meta/meta_module_test.go`（钉住"与 go.mod 一致"，仅认改名后的新名）。
	//
	// ❗存量插件的 go.mod 里仍可能是更早的名字，所以「识别 SDK 模块」的判定
	//   必须**所有历史名都接受**（见 hmapdev 的 sdkModuleNames）——
	//   只认最新名会让旧插件被当成「不是 SDK 模块」而静默走错分支。
	//   注意区分：**识别**要宽容，**生成**要精确（就是本常量）。
	SDKModule = "github.com/JianFeeeee/hologramagentsdk"

	// LegacySDKModule 是迁移前的 SDK module path，仍需被识别。
	LegacySDKModule = "gitcode.com/JianFeeeee/homeagent-sdk"

	// CoreModule 是核心仓的 Go module path，供 hmapdev 生成 go.mod 时使用。
	//
	// ★ 2026-10-08 随主仓托管地迁移而更新：仓库在 GitHub
	//   （github.com/JianFeeeee/HologramAgent），module 路径同步。
	//   注意它与 SDKModule 不是同一个仓：插件只依赖 SDK，**不依赖主仓**，
	//   所以改本常量不影响任何已有插件。
	CoreModule = "github.com/JianFeeeee/HologramAgent"

	// CoreVersion 是此 SDK 所兼容的最低核心版本。
	//
	// 1.0.0 是硬下限而非建议值：0.9.x 内核只会 dlopen `.so`，
	// 本版工具链产出的 `plugin.bin` 在旧内核上根本不会被识别。
	//
	// ⚠️ 1.1.0 新增的媒体接口需要核心 **1.1.1+**（更早的核心没有
	// doc.insertWithMedia / io.injectMedia* 这些 RPC，调用会返回 unknown method）。
	// 这里仍写 1.0.0，因为它是「SDK 能在其上运行」的下限；
	// 媒体接口是可选能力，不用就不受影响。
	//
	// ⚠️ 1.2.0 新增的注入标志位同理需要核心 **1.2.0+**：内核在 1.2.0 之前会
	// 忽略注入参数里的 no_memory/context_policy 字段（不会报错，但不生效）。
	// 想用这些标志位的插件应当要求核心 1.2.0+；不用就不受影响。
	CoreVersion = "1.0.0"
)

// FullVersion 返回完整的版本字符串。
func FullVersion() string {
	return SDKName + " v" + Version + " (" + Commit + ")"
}

// ---- 协议兼容性 ----
//
// **不自定协议号**：兼容性只认双方版本号的**主版本段**。
// 主版本相同即兼容（1.x 内全是纯追加）；跨主版本则是破坏性变更
// （如 1.0.0 从 C ABI 改子进程），握手时显式拒绝并提示重编。
//
// C ABI 时代的 ABIVersion / CABINum / 51 个 Core<Method> 整数 ID 已随
// Part 6.2 删除 internal/plugin/cabi/ 一并退场：
//   - 整数 method id 平移为 method 名字符串（proc/protocol.go 的 Method* 常量）
//   - 版本协商改为握手帧里的 core_version / sdk_version 主版本比对
//
// 保留那些常量只会让人以为它们还在生效。
