package kstate

// 内核统一挂载点：本进程唯一的公共数据持有者 + 它的存取入口。
//
// ## 用户设计（本包存在的理由）
//
// > 内核通过一个结构体收集全部包级共享变量，其他模块通过这个结构体访问数据，
// > 各个模块的公共数据也统一注册到内核这个公共结构体中。
//
// > 结构体成员初始置 nil 加读取前检查不就好了
//
// > 所有共享数据统一挂载一个 kernel 提供的挂载点上暴露。
//
// ## 为什么本包必须与装配引擎分开（2026-11-04 拆分）
//
// 拆分前 `KernelState` 与装配引擎同居 `internal/kernelboot`。数据结构要认识
// 各业务类型（`*security.Runtime`、`*plugin.Registry`、`*agentCore.StageHost`…），
// 于是 `kernelboot` 被迫依赖 **19 个业务包**：
//
//	kernelboot ──▶ agent/core
//	           ──▶ plugin
//	           ──▶ security
//	           ──▶ memory / knowledge / text / coldcontext / …
//
// 而装配引擎本身（kernelboot.go / scheduler.go / funcmodule.go）
// **一行业务代码都不需要** —— 依赖全在数据结构这一个文件里。
//
// 后果是所有被依赖的包都无法反向引用 kernelboot，于是它们各自造通道：
//
//	core      → Agent 字段（靠每种 agent 构造点记得传）
//	plugin    → 12 个 SetXxx
//	sdk       → SDKConfig 传参
//	security  → 包级单例 Global() / RuntimeGlobal() / ConfigCodec()
//
// 四条通道都指向同一个根因。把数据结构搬出来（本包），环就断了：
//
//	kstate     ← 纯数据，谁都能 import（它就是挂载点本身）
//	kernelboot ← 装配引擎，零业务包
//
// ## 并发约定
//
// 状态结构体上的字段**不加锁**（用户设计）：同层模块并行 Init 时各自只写
// **自己那一个字段**，字段互不相同 ⇒ 无数据竞争。读方直接读字段 + 判 nil。
//
// 但**挂载点本身**要加锁：它是全部调用方共同读的单一变量，而装配期写入
// 与运行期读取必然跨 goroutine（`Mount` 在启动时一次，`Get` 在热路径）。

import "sync"

var (
	mountMu sync.RWMutex
	mount   *State
)

// Mount 装入本进程的挂载点。**装配期调用一次**（由 kernelboot 的 Run 完成）。
//
// 重复调用会覆盖：便于测试与热重启路径（后装的生效）。
func Mount(s *State) {
	mountMu.Lock()
	mount = s
	mountMu.Unlock()
}

// Get 取挂载点。未装配时返回 **nil**（不 panic）。
//
// ★ 为什么返回 nil 而不是空结构体：调用点散布在内核热路径上，未装配
// （单元测试、独立工具如 memgc）时必须**零成本跳过**。所有调用方写成：
//
//	if ks := kstate.Get(); ks != nil && ks.Xxx != nil { … }
//
// 返回空结构体看似更"安全"，实则让"未装配"与"装配了但字段为 nil"两种
// 完全不同的情况混为一谈 —— 前者是环境问题，后者是装配缺陷。
func Get() *State {
	mountMu.RLock()
	defer mountMu.RUnlock()
	return mount
}

// MountForTest 装入一个测试用挂载点，返回**恢复函数**。
//
// 测试里必须这样写：
//
//	restore := kstate.MountForTest(fake)
//	t.Cleanup(restore)
//
// 为什么不直接用 Mount：并行测试（`t.Parallel()`）会互相覆盖全局挂载点，
// 表现为随机红——而随机红是判据可信度的头号杀手（本仓已有教训：
// 判据在 -race 下才暴露竞态）。恢复函数让每个测试回到"进入前的状态"
// 而不是"nil"，嵌套/串行测试也正确。
func MountForTest(s *State) (restore func()) {
	mountMu.Lock()
	prev := mount
	mount = s
	mountMu.Unlock()

	return func() {
		mountMu.Lock()
		mount = prev
		mountMu.Unlock()
	}
}
