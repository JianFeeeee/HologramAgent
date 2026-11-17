package kstate

// state.go —— 内核的**统一挂载点**：纯共享数据 + 能力挂载表。
//
// ## 用户裁定（2026-11-17，逐条对应）
//
// > 「kstate 为什么要分类，它只是一个共享挂载点为什么要持有模块知识？」
//
// > 「以及我反复强调了 kstate 不持有模块知识，之前的实现是错误的你听不懂吗？」
//
// > 「现在内核明明提供了挂载公共类与共享数据的接口，为什么不通过接口解耦而是层层嵌合？」
//
// 上一版这里躺着 **23 个具体类型字段**：`Memory *MemoryBundle`（内含
// `*memory.GraphDB` / `*Indexer` / `*social.SocialStore` / `*pipeline.Distiller`）、
// `Security *security.Runtime`、`PluginRegistry *plugin.Registry`、
// `DocStore *coldcontext.Store`、`Knowledge *knowledge.Store`、
// `LLMManager *api.ProviderManager`、`LuaVM *lua.VM`… 连打包类型
// `MemoryBundle` 本身都是模块产物。
//
// 后果：本包必须 import 十九个实现包 ⇒ 它不再是叶子，任何"想被挂上来"的
// 模块都不能反向 import 它；而消费方读字段就等于**编译期依赖实现**。
// 那正是"层层嵌合"的根。
//
// ## 现在的形态
//
//	真·共享数据（内核自己的，非模块产物）：
//	    Cfg / CfgReg / DataDir / AgentWorkDir / CLISocket / HTTPAddr
//	    Ctx / Stop / BootMode / Closers / Quiescers
//	能力挂载点（**不持有类型**）：
//	    deps map[string]any
//	    Provide(name, impl) / Require(name)
//
// 「谁 Provides 什么名字」的知识只出现在两处，且都是纯字符串：
//
//	internal/kernelapi/kernelapi.go   （设施名常量 + 能力的**接口**定义）
//	internal/kernelboot 的依赖图 + 各 kernelmod/*/mod.go 的 Init
//
// 消费方用法（编译期只依赖接口包与挂载点）：
//
//	impl, ok := ks.Require(kernelapi.FacSecurity)
//	gate, _ := impl.(kernelapi.SecurityGate)
//
// ## 并发
//
// 真·共享数据字段**不加锁**：同层模块并行 Init 时各自只写自己那一个字段
// （字段互不相同 ⇒ 无竞争），读方直接读 + 判 nil。
//
// deps 表必须加锁：它是全部模块共同读写的**同一张表**，装配期写入与运行期
// 读取必然跨 goroutine（且同层模块并行 Provide）。Closers/Quiescers 同理
// （它们是共享可变切片，`append` 无锁会丢元素 —— 实测过）。

import (
	"context"
	"log"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// State 是内核**唯一的公共挂载点**。
type State struct {
	// ── 真·共享数据（内核自己立起来的，所有模块只读） ──

	// Cfg 是配置快照（config 模块产出）。
	Cfg *types.Config

	// CfgReg 是配置中心（Group 0 产出，所有模块只读用）。
	CfgReg *config.ConfigRegistry

	// DataDir 是数据目录（Group 0 产出）。
	DataDir string

	// AgentWorkDir 是 agent 工作目录（ensureDataDirs 产出）。
	AgentWorkDir string

	// CLISocket 是 CLI 插件的 unix socket 路径（--cli-socket 产出）。
	CLISocket string

	// HTTPAddr 是 WebUI 监听地址覆盖值（--http-addr 产出）。
	HTTPAddr string

	// BootMode 是启动模式（"daemon" / "gui" / "cli" 等）。
	BootMode string

	// Ctx / Stop 是运行期取消上下文（装配后交给 IPC / supervisor 等）。
	Ctx  context.Context
	Stop func()

	// Closers 是各模块注册的关闭函数（按注册顺序，关闭时逆序执行）。
	Closers []func()

	// closerMu 保护 Closers（同层模块并行 Init 都会 AddCloser）。
	closerMu sync.Mutex

	// Quiescers 是可暂停/恢复的写者（伪关机用，见 lifecycle.go）。
	Quiescers []Quiescer

	quiesceMu sync.Mutex

	// ── 能力挂载点（**不持有任何模块类型**） ──

	mu   sync.RWMutex
	deps map[string]any
}

// New 创建公共挂载点（所有字段零值 = nil，由各模块填充）。
func New() *State {
	return &State{deps: map[string]any{}}
}

// Provide 把一个能力挂到指定设施名下（模块在 Init 里调用，只做这一件事）。
//
// ★ 同名重复 Provide 会**响亮报错**（本函数无返回值，因此写日志 + 保留首个）：
// 两个模块声称提供同一设施名意味着装配计划有错（Provides 冲突），
// 静默覆盖会让"到底谁在提供"变成不可判定。
func (s *State) Provide(name string, impl any) {
	if s == nil || name == "" || impl == nil {
		return
	}
	s.mu.Lock()
	if s.deps == nil {
		s.deps = map[string]any{}
	}
	if old, dup := s.deps[name]; dup && old != impl {
		s.mu.Unlock()
		log.Printf("[kstate] facility %q provided twice by different implementations — "+
			"first one kept; this is an assembly-plan error (two modules claim the same facility)", name)
		return
	}
	s.deps[name] = impl
	s.mu.Unlock()
}

// Require 取指定设施名上的能力实现。
//
// ★ 返回 (nil, false) 而不是 panic：热路径上的调用点散布各处，
// "未装配"必须能零成本跳过（单元测试、独立工具如 memgc）。
// 但**消费方必须显式判 ok 并响亮报错**（见 kernelapi 的文档约定）；
// 静默降级是本仓反复栽过的形态。
func (s *State) Require(name string) (any, bool) {
	if s == nil {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	impl, ok := s.deps[name]
	return impl, ok
}

// Has 报告某设施是否已装配（不取值，供诊断与装配自检）。
func (s *State) Has(name string) bool {
	_, ok := s.Require(name)
	return ok
}

// FacilityNames 返回已装配的设施名快照（按名排序由调用方决定，便于断言）。
//
// 用途：装配自检与 healthcheck 的"内核装了什么"只读视图。
func (s *State) FacilityNames() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.deps))
	for k := range s.deps {
		out = append(out, k)
	}
	return out
}

// AddCloser 注册一个关闭函数（模块在 Init 里调用）。
//
// 关闭时按**注册顺序的逆序**执行（LIFO，与 `defer` 语义一致）：
// 先初始化的一般是底层（config → memory → llm），停机应反向拆。
func (s *State) AddCloser(fn func()) {
	if fn == nil {
		return
	}
	// ★ 必须加锁：同层模块**并行** Init 时都会调它（见 Closers 字段的注释）。
	s.closerMu.Lock()
	s.Closers = append(s.Closers, fn)
	s.closerMu.Unlock()
}

// CloseAll 逆序执行全部关闭函数。
//
// ★ 失败策略：**继续执行剩余关闭**（返回首个错误）。
// 一个模块关不掉不该阻止其他模块关 —— 那会造成更多泄漏。
// 关闭函数 panic 也被隔离（否则一个模块的问题会连带其他模块都关不掉）。
func (s *State) CloseAll() error {
	// ★ 先取快照再执行（不持锁执行回调）：关闭函数可能很慢
	// （停进程/flush 数据库），持锁会让并发调用 AddCloser 的模块卡住；
	// 且回调可能反查 State ⇒ 死锁。
	s.closerMu.Lock()
	fns := make([]func(), len(s.Closers))
	copy(fns, s.Closers)
	s.Closers = nil
	s.closerMu.Unlock()

	for i := len(fns) - 1; i >= 0; i-- {
		fn := fns[i]
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[kernelboot] shutdown function panicked (isolated): %v", r)
				}
			}()
			fn()
		}()
	}
	return nil
}
