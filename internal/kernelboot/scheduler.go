package kernelboot

// 插件调度器：按 syncbus 状态决定**何时**拉起各插件（设计 §2.6.4）。
//
// ## 核心规则（三条，按优先级）
//
//	未声明依赖              ⇒ 等内核**完全**初始化（默认路径 = 现行为，§2.6.5）
//	声明了依赖且全就绪       ⇒ **立即**拉起
//	声明了依赖但未全就绪     ⇒ 挂起，等 syncbus 更新后再判断
//
// ## ★ 兼容性的落点就在第一条
//
// 存量 42 个插件都不声明依赖 ⇒ 全部走第一条 ⇒ 行为**逐字不变**。
// 这不是"顺便兼容"，而是本设计的硬约束：42 个插件里任何一个若因本设计
// 改变了启动时机，就可能在不该动的时候动（例如假设"Start 时 llm 已就绪"）。
// **静默改变启动时机 = 静默引入竞态。**
//
// ## 为什么"未声明"不能等价于"依赖集为空"
//
// 空依赖集在数学上"已满足"，若按此处理，未声明的插件会在**第一层完成后**
// 就被拉起 —— 比现状早得多，直接破坏兼容性。
// 所以"未声明"与"声明了空集"是**不同**语义，必须分开判断（见 Wants）。
//
// ## 为什么调度器不自带并发控制
//
// 拉起插件的实际动作（load/Start）由插件管理器做 —— 那里已有
// Registry 的锁与生命周期管理。本调度器只负责**判断时机**，
// 保持单一职责，也让判断逻辑可以脱离真实插件系统被单测（判据的关键）。

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// PluginSpec 是调度器需要知道的插件声明（设计 §2.6.4/2.6.6）。
type PluginSpec struct {
	Name string

	// RequiresFacilities 是插件声明需要的设施名。
	//
	// nil/空 ⇒ **未声明**（走"等内核完全初始化"路径）
	// 非空   ⇒ 声明的依赖；全就绪即拉起
	RequiresFacilities []string

	// AutoStart 为 false 表示"不伴随启动"：即使依赖齐了也**不**自动拉起，
	// 等 agent 经 plugin_enable 显式启用（设计 §2.6.6）。
	//
	// 用指针语义的注意：这里用 bool + 一个显式标记区分"未声明"与"false"。
	// 但为了简洁，约定：**缺省（未声明）= true**（伴随启动，与现状一致），
	// 由 manifest 层负责把"未声明"翻译成 true。
	AutoStart bool
}

// Scheduler 按 syncbus 状态调度插件。
type Scheduler struct {
	mu       sync.Mutex
	bus      SyncState
	pending  []PluginSpec
	launched map[string]bool
	launch   func(PluginSpec) error

	// onLaunchErr 记录拉起失败（不中断其他插件）
	onLaunchErr func(PluginSpec, error)
}

// NewScheduler 创建调度器。launch 是实际拉起动作（由插件管理器提供）。
//
// ★ bus 参数类型是 **SyncState**（只读接口），不是 *SyncBus。
// 这就是设计 §2.6.3 的权限隔离：调度器**在编译期就没有**写 syncbus 的能力。
func NewScheduler(bus SyncState, launch func(PluginSpec) error) *Scheduler {
	s := &Scheduler{
		bus:      bus,
		launch:   launch,
		launched: map[string]bool{},
	}
	// ★ 订阅状态变化：syncbus 每次更新（层边界 / 内核完成）就重新评估挂起队列。
	//
	// 这是"自动拉起"的实现方式：调度器不需要轮询（空转）也不需要调用方
	// 记得在每处手动触发（漏了就会静默卡住）。订阅一次，永久生效。
	//
	// 若 bus 不支持订阅（只传了个只读快照实现），退化为"只在 Submit 时评估"——
	// 那种情况下调度器仍可用，只是不会自动跟进后续状态变化。
	if sub, ok := bus.(interface{ OnUpdate(func(SyncState)) }); ok {
		sub.OnUpdate(func(SyncState) { s.evaluate() })
	}
	return s
}

// SetLaunch 补上拉起动作（供"两段式"调度：装配前订阅、装配后接动作）。
//
// ## 为什么需要它（真实缺口）
//
// 插件调度器必须在**装配开始前**订阅 syncbus，否则"声明提前启动"不会生效
// （提交时全部设施早已就绪 ⇒ 永远退化成"立即拉起"）。
//
// 但**拉起动作**依赖插件注册表，而那是 stage-host 模块的产物
// ⇒ 装配前拿不到。所以分两段：
//
//	· 装配前：NewScheduler(bus, nil) 只订阅
//	· 装配后：SetLaunch(fn) 接上真实动作
//
// ★ 此时可能已有插件被标记 launched（分层过程中达到条件）。
// 调用方需自行补偿"已标记但未真正加载"的那些 —— 见 LaunchedFlag。
func (s *Scheduler) SetLaunch(fn func(PluginSpec) error) {
	s.mu.Lock()
	s.launch = fn
	s.mu.Unlock()
	// ★ 立刻重新评估一次：接上动作之前可能已有插件达到条件并被标记
	// （它们在 syncbus 更新时就评估过，但那时 launch==nil ⇒ 只标记未执行）。
	// 不补这一下，它们要等到**下一次** syncbus 更新才会被真正拉起 ——
	// 而若内核装配已结束（不再有更新），就永远不拉。
	s.evaluate()
}

// LaunchedFlag 报告某插件是否已被标记为"可拉起/已拉起"。
//
// ★ 与 Launched()（返回列表）分开：本方法用于**单个查询**，
// 供两段式调度的补偿逻辑判断"它到条件了但可能还没真正加载"。
func (s *Scheduler) LaunchedFlag(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.launched[name]
}

// SetLaunchErrorHandler 注册拉起失败的观察者（诊断用）。
func (s *Scheduler) SetLaunchErrorHandler(fn func(PluginSpec, error)) {
	s.mu.Lock()
	s.onLaunchErr = fn
	s.mu.Unlock()
}

// Submit 提交一批插件声明。返回**立即可拉起**的那些插件名。
//
// 未达条件的进挂起队列，等 OnUpdate 触发时再评估。
func (s *Scheduler) Submit(specs []PluginSpec) []string {
	// ★ 先校验：声明的设施名必须是**内核注册过的**。
	//
	// ## 为什么这条校验必须存在（修一个"静默永不启动"的缺口）
	//
	// 插件声明 `requires_facilities` 是字符串。拼错时原行为是：
	// 永远等不到 ⇒ **永不拉起**，且**无任何报错**、无超时兜底。
	// 那是本仓反复吃亏的"静默降级"形态（系统看着正常，某插件就是不工作）。
	//
	// 有了这条校验，拼错在**提交那一刻**就报出来，并列出全部已知设施名
	// 供对照（"拼错了"当场可修）。
	//
	// ★ 为什么在这里而不在 canLaunch：canLaunch 是**每轮状态变化**都会调的
	// 热路径，在那里报错会重复刷屏；而"名字不存在"是**一次性**的静态错误，
	// 提交时检查一次即可。
	s.mu.Lock()
	var rejected []PluginSpec
	var valid []PluginSpec
	for _, p := range specs {
		if bad := s.unknownFacilities(p); len(bad) > 0 {
			rejected = append(rejected, p)
			continue
		}
		valid = append(valid, p)
	}
	s.pending = append(s.pending, valid...)
	errFn := s.onLaunchErr
	s.mu.Unlock()

	// 报错在锁外（回调可能反查调度器状态 ⇒ 持锁会死锁）
	for _, p := range rejected {
		bad := s.unknownFacilities(p)
		err := fmt.Errorf("插件 %s 声明的设施名内核**从未注册过**: %v —— "+
			"这通常是拼写错误。内核已知的设施名: %v。"+
			"（若不校验，该插件会**永远等不到就绪**而静默不启动）",
			p.Name, bad, s.bus.KnownFacilities())
		if errFn != nil {
			errFn(p, err)
		}
	}
	return s.evaluate()
}

// unknownFacilities 返回插件声明里**内核从未注册过**的设施名（拼错检测）。
func (s *Scheduler) unknownFacilities(p PluginSpec) []string {
	var bad []string
	for _, f := range p.RequiresFacilities {
		if !s.bus.Known(f) {
			bad = append(bad, f)
		}
	}
	sort.Strings(bad)
	return bad
}

// Pending 返回仍在挂起的插件名（诊断/判据用）。
func (s *Scheduler) Pending() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.pending))
	for _, p := range s.pending {
		out = append(out, p.Name)
	}
	sort.Strings(out)
	return out
}

// Launched 返回已拉起的插件名（判据用）。
func (s *Scheduler) Launched() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.launched))
	for k := range s.launched {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// evaluate 挑出当前可拉起的插件并拉起它们。
//
// 返回本次真正拉起的插件名。
func (s *Scheduler) evaluate() []string {
	s.mu.Lock()
	// 挑出可拉起的 + 留下的。
	//
	// ★★ 必须**就地去重**：pending 里可能有同一插件的多份副本，
	// 因为并发 Submit 各自把同一批插件追加进来。
	//
	// 实测（压力测试 TestStressSchedulerConcurrentStateChange）：
	// 8 个 goroutine 并发提交 200 个插件 + 内核并发标记就绪
	// ⇒ 同一插件在**同一次** evaluate 的 ready 列表里出现多次
	// ⇒ `launched` 查重救不了（它只在**跨轮**生效）⇒ 被拉起 203 次。
	//
	// 后果：插件被重复加载（spawn 两个进程 / 工具注册冲突）。
	//
	// 去重用 seen 集合（同一次 evaluate 内），并把它记进 launched
	// 以确保跨轮也幂等 —— 两者都需要。
	var ready []PluginSpec
	var still []PluginSpec
	seen := map[string]bool{}
	for _, p := range s.pending {
		if s.launched[p.Name] || seen[p.Name] {
			continue // 已拉起（跨轮）或本批已选中（同批）⇒ 丢弃
		}
		if s.canLaunch(p) {
			seen[p.Name] = true
			ready = append(ready, p)
		} else {
			still = append(still, p)
		}
	}
	s.pending = still
	for _, p := range ready {
		s.launched[p.Name] = true
	}
	launchFn := s.launch
	errFn := s.onLaunchErr
	s.mu.Unlock()

	// ★ 在**不持锁**的情况下执行拉起动作：
	//   拉起可能很慢（spawn 进程、握手），持锁会让其他插件全卡住；
	//   且拉起过程可能回调调度器（如插件注册新插件）⇒ 死锁。
	var names []string
	for _, p := range ready {
		names = append(names, p.Name)
		if launchFn == nil {
			continue
		}
		if err := launchFn(p); err != nil && errFn != nil {
			errFn(p, err)
		}
	}
	sort.Strings(names)
	return names
}

// canLaunch 判断单个插件当前是否可拉起。
func (s *Scheduler) canLaunch(p PluginSpec) bool {
	// ① 不伴随启动：依赖齐了也**不**自动拉起（§2.6.6）
	if !p.AutoStart {
		return false
	}
	// ② 未声明依赖 ⇒ 等内核**完全**初始化（§2.6.5，存量插件走这条）
	if len(p.RequiresFacilities) == 0 {
		return s.bus.KernelComplete()
	}
	// ③ 声明了依赖 ⇒ 全就绪即拉起
	return s.bus.AllReady(p.RequiresFacilities)
}

// WaitString 渲染"还差什么"（供日志：让等待可解释，而不是静默卡住）。
func (s *Scheduler) WaitString(p PluginSpec) string {
	if !p.AutoStart {
		return "不伴随启动（等显式启用）"
	}
	if len(p.RequiresFacilities) == 0 {
		return "等内核完全初始化"
	}
	// AllReady 需要 *SyncBus；这里只做只读渲染，用 Snapshot 判断
	snap := s.bus.Snapshot()
	var missing []string
	for _, f := range p.RequiresFacilities {
		if !snap[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		return "依赖已就绪"
	}
	sort.Strings(missing)
	return fmt.Sprintf("还差: %s", strings.Join(missing, ","))
}
