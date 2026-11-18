package kernelmod

import (
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// facilities.go —— 设施名常量（模块间的协议）。
//
// ★ 全部转发到 internal/kernelapi：设施名与能力的**接口定义**必须在同一个
// 叶子上（消费方 import 接口包就够了），而本包 import kernelboot（agent 子包
// 不许依赖它）。转发常量让既有 `kernelmod.FacMemory` 写法逐字不变。
//
// ★ 为什么用常量而不是裸字符串：拼错依赖名时**编译不过**，而不是"永远等不到
// 就绪"（运行期表现是静默挂起 / 静默拿到 nil，极难排查）。
//
// ★ 公开 SDK（third_party/hologramagentsdk/sdk）里有逐字一致的一份 ——
// 那是插件的对外契约（`requires_facilities`）。CI 门禁守这个一致性。
const (
	// ── 插件可依赖的设施（与公开 SDK 的 sdk.Facility* 逐字一致） ──
	FacConfig        = kernelapi.FacConfig
	FacKernelIO      = kernelapi.FacKernelIO
	FacMemory        = kernelapi.FacMemory
	FacLua           = kernelapi.FacLua
	FacSupervisor    = kernelapi.FacSupervisor
	FacTracker       = kernelapi.FacTracker
	FacTextMem       = kernelapi.FacTextMem
	FacLLM           = kernelapi.FacLLM
	FacDocStore      = kernelapi.FacDocStore
	FacMultimodal    = kernelapi.FacMultimodal
	FacONNX          = kernelapi.FacONNX
	FacStageHost     = kernelapi.FacStageHost
	FacAgent         = kernelapi.FacAgent
	FacAgentRegistry = kernelapi.FacAgentRegistry
	FacAgentIndex    = kernelapi.FacAgentIndex

	// ── 内核**内部**顺序标记（**不是**给插件的设施）──
	//
	// ★ 为什么不与上面的混在一起：这两类东西的**消费者不同**。
	//   · 上面的：插件按它声明启动时机（对外契约，必须进公开 SDK）
	//   · 这里的：只用于**内核模块之间**表达顺序（如"迁移必须先于扫盘"）
	//
	// 混在一起会让"内核内部标记"被误当成"插件可选设施"
	// ⇒ 插件可能声明它（然后永远等不到 —— 因为它的就绪时机是装配中途）。

	// FacVersion 标记"内核版本自检与升级回滚判定已完成"（update 模块提供）。
	//
	// 语义：读到它时，内核版本已经是**回滚后的最终版本**。
	// 需要按版本行事的模块（healthcheck 上报版本、pluginhost 判插件兼容性）
	// 应 Requires 它，否则可能读到升级前那个已被回滚的版本。
	FacVersion = "kernel.version"

	// FacSecurity 是**安全防火墙**设施（security 模块提供）。
	//
	// ★ 它不是"某个能力"，而是横跨 Core 与全部外部模块的**数据面闸门**：
	//   Core → 外部、外部 → Core 的每一条数据流都要经过它（显式代理）。
	FacSecurity = kernelapi.FacSecurity

	// FacPluginRegistry 是插件注册表（stage-host 模块提供）。
	FacPluginRegistry = kernelapi.FacPluginRegistry

	// FacDevice 是**设备/工具数据面**设施（device 模块提供）。
	//
	// 统一拥有：工具注册与分发、inputch、outputch、设备登记。
	FacDevice = kernelapi.FacDevice
)

// MustValue 从挂载点取一个**具名设施**并断言到目标类型。
//
// ★ 取不到时返零值：调用点由模块的 Requires 声明保证（依赖图先验），
// 若仍为 nil 就是**装配缺陷**，由消费方自己的 nil 检查/构造期校验响亮暴露。
// 刻意不做 panic：模块装配期 panic 会让整机启动失败却丢失"哪个设施缺"的
// 精确信息（判据也不好写）。
func MustValue[T any](ks *kstate.State, name string) T {
	var zero T
	if ks == nil {
		return zero
	}
	impl, ok := ks.Require(name)
	if !ok || impl == nil {
		return zero
	}
	typed, _ := impl.(T)
	return typed
}

// MustValueAny 取任意实现（用于"断言到具体类型"的场景）。
func MustValueAny(ks *kstate.State, name string) any {
	if ks == nil {
		return nil
	}
	impl, _ := ks.Require(name)
	return impl
}
