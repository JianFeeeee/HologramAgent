// Package meta 收集 HomeAgent 内核的全部元数据。
// 版本号通过 `go build -ldflags` 注入，默认值为 dev 版本。
// SDK 仓的 meta/meta.go 应与此保持同步。
package meta

var (
	// Version 是 HomeAgent 内核版本号。
	// 通过 `-ldflags="-X github.com/JianFeeeee/HomeAgent/internal/meta.Version=vX.Y.Z"` 注入。
	//
	// 1.0.0：外部插件从 C ABI 动态库迁到子进程 + 共享内存。
	// 这是首个不再加载 `.so`/`.dll` 的版本，与 0.9.x 不兼容（存量插件必须
	// 用新版 hmapdev 重编），故跃到主版本号。
	// 1.1.0：记忆系统支持二进制多媒体节点——CAS 媒体存储 + L0/L2/L3 贯通。
	// 1.1.1：多模态贯通**插件边界**。内核实现公开 SDK 1.1.0 新增的媒体接口
	//        （doc.insertWithMedia、io.injectMedia / injectMediaSync /
	//        injectInterruptMedia），并把 text/image/audio 三条输入路径归一成
	//        一条 processInput 主干。
	// 1.2.0：模型中立的多模态 provider SPI（pkg/embedding）——内核不再适配任何
	//        具体模型，Qwen 实现移到 providers/qwen3vl；插件运行协议升到 2
	//        （统一共享内存区，fd3 布局改变，不支持滚动升级）；并实现 SDK 1.2.0
	//        新增的注入行为标志位（InjectOptions：no_memory / context_policy）
	//        与 ChannelDef.ContextPolicy，使输入/排队注入/中断注入/同步注入都能
	//        声明「是否记入记忆」与「是否据此裁剪上下文」（默认都是否）。
	// 1.3.0：注入优先级贯通（SDK 1.3.0 的 InjectOptions.Priority 与
	//        PriorityL1-L4）——插件可声明自己中断的级别，内核按四级中断阶梯调度；
	//        并实现 UnregisterOutputChannel，使动态通道（远程设备 device/<id>）
	//        能随资源生灭，而不是留下死通道骗模型。
	//
	// ❗main 上此值始终是**下一个未发布中版本**，不随 patch 发布变动
	//（见 docs/git-branching.md §2.1）；已发布的版本号看对应的 release/vX.Y.x 与 tag。
	// ❗1.3.0 已随 release/v1.3.x 发布（承载 v1.3.1–v1.3.6），此号归发布线所有，
	//    main 立即推进到下一个未发布中版本（docs/git-branching.md §2.1/§七.4）。
	Version = "1.4.0"

	// Commit 是构建时的 Git commit hash。
	Commit = "unknown"

	// BuildTime 是构建时间。
	BuildTime = "unknown"

	// KernelName 是内核名称。
	KernelName = "HomeAgent"

	// SDKCompatibleVersion 是此内核可兼容的最高 SDK 版本（semver）。
	//
	// 1.1.0：本内核实现了 SDK 1.1.0 的全部新增方法。
	// 1.2.0：本内核实现了 SDK 1.2.0 的全部新增方法（IOInjector 的六个 *Opts
	//        注入变体、InjectOptions、ChannelDef.ContextPolicy），因此声明为
	//        1.2.0。用 SDK 1.0.0/1.1.0 编的存量插件照旧可用——新增方法由
	//        **插件调用、内核实现**，不调就不受影响，无需重编。
	// 1.3.0：内核实现了 SDK 1.3.0 的全部新增面 —— InjectOptions.Priority 的
	//        分级中断调度、PriorityL1-L4 常量的语义，以及 UnregisterOutputChannel
	//        在通道登记表 + io 设备表上的注销。同样：不调新面就不受影响。
	SDKCompatibleVersion = "1.3.0"

	// SourceURL 是本内核构建所对应的源码地址。
	//
	// AGPL-3.0 §13（Remote Network Interaction）要求：当你把修改过的版本
	// 作为网络服务提供出去时，必须给使用者提供取得 Corresponding Source 的机会。
	// WebUI 的状态页会把这个值渲染成可见链接，所以：
	//
	// ❗**修改后对外部署的分支必须把它改指向自己的源码仓库**，否则链接指向的
	// 不是你实际运行的那份代码，§13 的提供义务并未履行。
	//
	// 构建时可用 -ldflags 覆盖，无需改源码：
	//   -X github.com/JianFeeeee/HomeAgent/internal/meta.SourceURL=<你的仓库>
	SourceURL = "https://github.com/JianFeeeee/HomeAgent"

	// License 是本项目的开源许可标识（SPDX 表达式）。
	//
	// AGPL-3.0-only 带**网络条款**：把修改过的版本作为网络服务对外提供时，
	// 必须给使用者提供取得 Corresponding Source 的机会（§13）。所以 WebUI
	// 总览底部把「许可标识 + 许可全文 + 源码仓库」三者一起渲染成可见的许可卡——
	// 只给一个源码链接、不标明协议名，使用者在界面上根本看不出这是什么许可。
	License = "AGPL-3.0-only"

	// LicenseURL 是上述许可的全文地址。
	//
	// 默认指向 GNU 官方的 AGPL-3.0 全文页：与仓库托管方、分支名、文件路径都
	// 无关，换仓库/换分支都不会失效。若你的发行版把 LICENSE 放在别处，
	// 用 -ldflags 覆盖即可：
	//   -X github.com/JianFeeeee/HomeAgent/internal/meta.LicenseURL=<你的链接>
	LicenseURL = "https://www.gnu.org/licenses/agpl-3.0.html"
)

// FullVersion 返回完整的版本字符串。
func FullVersion() string {
	return KernelName + " v" + Version + " (" + Commit + ")"
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
