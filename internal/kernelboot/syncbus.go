package kernelboot

// syncbus：内核就绪状态的同步快照，驱动插件调度（设计 §2.6）。
//
// ## 一句话
//
// 内核每完成一层装配就把该层贡献的设施标记为"就绪"；
// 插件管理器**只读**这份状态，据此拉起"依赖已齐"的插件。
//
// ## 为什么要有它（解决什么）
//
// 现状（`cmd/homed/main.go:291 loadPlugins`）是**一刀切**：
// 12 个 init* 全部做完才加载全部插件。
// 于是"只读本地文件"的插件也要等多模态空间（ONNX 加载）+ 知识库建索引做完。
//
// 而内核那 12 步本身有明确先后（§2.3 算出 4 层），"阶段"是天然存在的，
// 只是此前没有对外发布。syncbus 就是那个发布物。
//
// ## ★ 权限：插件管理器只读（这不是习惯，是设计）
//
// 若插件管理器能写 syncbus，它就能**谎报内核状态**，让插件在自己依赖的东西
// 还不存在时启动 —— 那正是病 A（静默顺序错）。
//
// 所以类型层面分离：内核拿 `*SyncBus`（可写），
// 插件管理器只拿 `SyncState` 接口（**上面没有写方法**）。
// 想写也写不了 —— 编译期保证，而非"约定不要写"。
// 同 `Get[T]` 泛型取用（类型错=编译不过）是一个思路。

import (
	"sort"
	"sync"
)

// SyncState 是 syncbus 的**只读**视图。
//
// ★ 刻意不含 MarkReady/Reset：插件管理器拿到的就是这个接口，
// 因此它**在编译期就不可能**篡改内核状态。
type SyncState interface {
	// IsReady 报告某设施是否已就绪。
	IsReady(facility string) bool
	// Snapshot 返回当前全部就绪状态的副本（调用方改不动内部）。
	Snapshot() map[string]bool
	// ReadyInOrder 按**就绪顺序**返回设施名（调试/判据用）。
	ReadyInOrder() []string
	// KernelComplete 报告内核是否已完全初始化（最后一层完成）。
	KernelComplete() bool
	// AllReady 报告给定设施是否**全部**就绪（插件调度器的核心判断）。
	//
	// ★ 空列表返回 false（不是 true）：见实现处的说明 ——
	// "没有依赖"与"依赖全就绪"是不同语义，混淆会让未声明依赖的插件提前拉起。
	AllReady(facilities []string) bool

	// Known 报告某设施名**是否被内核注册过**（不论是否已就绪）。
	//
	// ## ★ 为什么必须有它（修一个"静默永不启动"的缺口）
	//
	// 插件声明 `requires_facilities` 是**字符串**。若拼错（如写 "llm " 或
	// "llmservice"），原本的行为是：它永远等不到 → **永不拉起**，
	// 且**没有任何报错**、没有超时兜底（我第一版设计文档写了"超时兜底"
	// 但从未实现）。
	//
	// 这正是本仓反复吃亏的"静默降级"形态：系统看着正常，某插件就是不工作。
	//
	// ⇒ 有了 Known，调度器能在**提交时**立刻报错：
	//   "插件 X 声明了内核从未注册过的设施 Y（可能是拼写错误）"
	//   并列出全部已知设施名供对照。
	Known(facility string) bool

	// KnownFacilities 返回内核**注册过的全部设施名**（含尚未就绪的）。
	//
	// 用途：错误信息里列出可选值（让"拼错了"当场可修）。
	KnownFacilities() []string
}

// SyncBus 是可写的同步总线（内核持有）。
type SyncBus struct {
	mu sync.RWMutex
	// known 是内核**注册过的全部设施名**（不论是否已就绪）。
	//
	// ★ 它存在的唯一目的是让"插件拼错设施名"从**静默永不启动**
	// 变成**提交时立刻报错**（见 SyncState.Known 的说明）。
	known    map[string]bool
	ready    map[string]bool
	order    []string
	complete bool
	onUpdate []func(SyncState)
	notifyMu sync.Mutex
}

// NewSyncBus 创建空总线。
func NewSyncBus() *SyncBus {
	return &SyncBus{ready: map[string]bool{}, known: map[string]bool{}}
}

// MarkReady 标记一个设施就绪。**只有内核该调它。**
//
// 幂等：重复标记同一设施不报错（便于层边界回调重入）。
// 但**不重复记录**顺序，保证 ReadyInOrder 可复现。
func (b *SyncBus) MarkReady(facility string) {
	if facility == "" {
		return
	}
	b.mu.Lock()
	if b.ready[facility] {
		b.mu.Unlock()
		return
	}
	b.ready[facility] = true
	b.order = append(b.order, facility)
	b.mu.Unlock()
	b.fire()
}

// MarkLayerReady 在**层边界**标记该层所有模块贡献的设施（设计 §2.6.2）。
//
// ★ 为何以"层"而不是"单个模块"为同步点：
// 层内是**并发**执行的，若层内逐个模块标记就绪，就绪顺序会**不确定**，
// 而判据要求可复现。层边界天然是一个确定的同步点。
func (b *SyncBus) MarkLayerReady(provides []string) {
	// 先在锁内一次性写入，再统一通知（避免 N 次通知、N 次唤醒）
	b.mu.Lock()
	changed := false
	for _, f := range provides {
		if f == "" {
			continue
		}
		b.known[f] = true // 注册过（不论是否首次就绪）
		if b.ready[f] {
			continue
		}
		b.ready[f] = true
		b.order = append(b.order, f)
		changed = true
	}
	b.mu.Unlock()
	if changed {
		b.fire()
	}
}

// MarkDeclared 在**执行前**登记一批设施名（视为"内核注册过"）。
//
// ## ★ 为什么需要它（时序）
//
// 插件的 `requires_facilities` 校验要在**插件提交时**做（那是错误最该被发现
// 的时刻，紧挨着写错的插件作者）。但插件的提交发生在内核装配**完成之后**
// —— 那时所有层都跑完了，Known 自然齐了。
//
// 但仍然需要本方法，因为：
//
//	· 装配**中途失败**时，后面的层没跑 ⇒ 那些设施名从未登记 ⇒
//	  若此时还有插件提交，会报"内核从未注册过 X"（**误报**：其实是没跑到）
//	· 判据要能在**不执行**的情况下校验"插件声明 ⊆ 内核声明"
//	  （只看声明，不看执行结果）
//
// ⇒ 装配开始时把**全部模块的 Provides** 一次性登记为 known。
// 这样 Known 的语义精确为"**模块声明过**这个设施"，与"是否已就绪"解耦。
func (b *SyncBus) MarkDeclared(facilities []string) {
	b.mu.Lock()
	for _, f := range facilities {
		if f != "" {
			b.known[f] = true
		}
	}
	b.mu.Unlock()
}

// MarkKernelComplete 标记内核已完全初始化。
//
// 这是**未声明依赖的插件**的启动信号（默认路径 = 现行为，§2.6.5）。
func (b *SyncBus) MarkKernelComplete() {
	b.mu.Lock()
	if b.complete {
		b.mu.Unlock()
		return
	}
	b.complete = true
	b.mu.Unlock()
	b.fire()
}

// OnUpdate 注册状态变化回调（插件调度器用它重新检查挂起的插件）。
//
// 回调在**不持锁**的情况下调用 —— 否则回调里再读 syncbus 会死锁。
// （这个坑很容易踩：回调显然要读状态，持锁调用必然死锁。）
func (b *SyncBus) OnUpdate(fn func(SyncState)) {
	b.notifyMu.Lock()
	b.onUpdate = append(b.onUpdate, fn)
	b.notifyMu.Unlock()
}

func (b *SyncBus) fire() {
	b.notifyMu.Lock()
	cbs := make([]func(SyncState), len(b.onUpdate))
	copy(cbs, b.onUpdate)
	b.notifyMu.Unlock()
	for _, fn := range cbs {
		fn(b) // b 实现了 SyncState（只读方法）
	}
}

// --- SyncState 实现（只读） ---

func (b *SyncBus) IsReady(facility string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ready[facility]
}

func (b *SyncBus) Snapshot() map[string]bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]bool, len(b.ready))
	for k, v := range b.ready {
		out[k] = v
	}
	return out
}

func (b *SyncBus) ReadyInOrder() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, len(b.order))
	copy(out, b.order)
	return out
}

func (b *SyncBus) KernelComplete() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.complete
}

// Known 报告某设施名是否被内核注册过（不论是否已就绪）。
//
// ★ 为何"注册过但未就绪"也算 Known：那是**正常的中间状态**
// （如 llm 声明了但还在第 1 层）。真正的错误是"内核从未注册过这个名字"。
func (b *SyncBus) Known(facility string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.known[facility]
}

// KnownFacilities 返回内核注册过的全部设施名（升序，便于对照阅读）。
func (b *SyncBus) KnownFacilities() []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.known))
	for k := range b.known {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AllReady 报告给定的设施**全部**就绪（插件调度器的核心判断）。
//
// 空列表 = 真的全就绪 → 返回 false。
//
// ★ 为何空列表返回 false 而不是 true：
// "没有依赖" 与 "依赖全就绪" 是**不同语义**，混淆会让"未声明依赖的插件"
// 被当成"依赖已满足"而**提前拉起** —— 那就破坏了 §2.6.5 的存量兼容。
// 未声明走的是 KernelComplete 那条路径，与这条刻意分开。
func (b *SyncBus) AllReady(facilities []string) bool {
	if len(facilities) == 0 {
		return false
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, f := range facilities {
		if !b.ready[f] {
			return false
		}
	}
	return true
}

// Missing 返回尚未就绪的设施名（诊断用：告诉用户"还差什么"）。
func (b *SyncBus) Missing(facilities []string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out []string
	for _, f := range facilities {
		if !b.ready[f] {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}
