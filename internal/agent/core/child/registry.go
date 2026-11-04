package child

// registry.go 是**单一子 agent 登记表**与生命周期骨架。
//
// # 它替换了什么
//
// 此前 core.Agent 上有**两张**互不知情的表：
//
//	residentMu + residents  map[string]*residentChild
//	rtMu       + rtChildren map[string]*rtChild
//
// 于是每加一种类型就多一张表、一把锁、一段路由分支。实测新增一种子 agent
// 需要改 18 个生产文件（见 plan.md §D4）。
//
// 现在只有一把锁、一张表，类型差异全部下沉到 Entry 实现。
//
// # 骨架的职责边界
//
// 骨架只做**所有类型都一样**的事，这些事恰恰是最容易"改一处漏一处"的：
//
//	销毁：标记停止 + cancel → 类型 Dispose → 撤销子策略 → 注销入站登记
//
// 差异（输入投递/上下文/输出/工具/残余/通道处置/父工具）一律不进骨架。
//
// ★ 历史教训：`StopRTChild`（单数）注销入站登记，`StopRTChildren`（复数）漏了，
// 残留随 create/destroy 单调累积且只能重启清（engineering-lessons.md §2.3）。
// 现在单数与复数是**同一个函数**，这个形态在结构上不可能复现。

import (
	"log"
	"sort"
	"sync"
	"time"
)

// Registry 是子 agent 登记表。
//
// 零值可用（lazy init），不需要构造函数。
type Registry struct {
	mu    sync.Mutex
	byID  map[string]Entry
	order uint64
}

// Entry 是一个已建立的子 agent 实例。
//
// ★ 登记表存的是**实例**而不是"类型 → 实现"的映射：
// 每个实例自带它的类型行为（rt 实例就是 *rtChild，驻留子实例就是 *residentChild）。
// 这样骨架不需要一张全局 kind 表去反查，也不会出现"实例存在但 kind 没注册"的
// 半残状态。类型清单（kinds.go）只用于装配自检与文档。
type Entry interface {
	// Base 返回公共部分（id / ctx / 状态）。不可为 nil。
	Base() *Child

	// Kind 返回类型标签。
	Kind() Kind

	// Dispose 按本类型语义收尾（关闭记忆句柄、丢弃过期输入等）。
	// 返回给父看的摘要（可为空）。
	//
	// ★ 签名里**没有**残余策略：残余处置（keep/drop）走的是另一条路径
	// （ApplyResidual / FinishResidentWith），它们在 Dispose 之前就已经把
	// 任务取走或丢弃了。曾在这里放过 `policy ResidualPolicy` 参数，
	// 但两个实现都没读它 —— 那是误导性签名（看签名以为这里会处理残余），
	// 按"不留技术债"删掉。
	Dispose() string
}

// Add 登记一个已构造好的实例。
//
// ★ 返回 error 而不是静默覆盖：同名覆盖会让上一代的残留
// （通道 Owner、主体策略、留档）被当成这一代的。
func (r *Registry) Add(e Entry) error {
	if e == nil || e.Base() == nil || e.Base().ID == "" {
		return ErrChildNotFound
	}
	b := e.Base()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byID == nil {
		r.byID = map[string]Entry{}
	}
	if _, dup := r.byID[b.ID]; dup {
		return ErrChildExists
	}
	r.order++
	if b.CreatedAt == 0 {
		b.CreatedAt = time.Now().UnixNano()
	}
	b.Kind = e.Kind()
	r.byID[b.ID] = e
	return nil
}

// Get 按 id 查询。
func (r *Registry) Get(id string) (Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[id]
	return e, ok
}

// Base 按 id 查公共部分（路由热路径用，少一层类型断言）。
func (r *Registry) Base(id string) (*Child, bool) {
	e, ok := r.Get(id)
	if !ok {
		return nil, false
	}
	return e.Base(), true
}

// Remove 摘出登记表并返回被摘的实例（不存在返回 nil）。
//
// ★ 先把条目摘掉再返回：调用方拿到指针后做清理（撤销策略、注销通道），
// 期间新输入不会再路由到它。顺序不能反 —— 否则清理期间仍有输入进来。
func (r *Registry) Remove(id string) Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.byID[id]
	delete(r.byID, id)
	return e
}

// All 返回全部实例快照（按 id 排序，便于断言与展示稳定）。
func (r *Registry) All() []Entry {
	r.mu.Lock()
	out := make([]Entry, 0, len(r.byID))
	for _, e := range r.byID {
		out = append(out, e)
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Base().ID < out[j].Base().ID })
	return out
}

// IDs 返回全部 id（排序）。
func (r *Registry) IDs() []string {
	all := r.All()
	out := make([]string, len(all))
	for i, e := range all {
		out[i] = e.Base().ID
	}
	return out
}

// ByKind 返回某类型的全部实例。
func (r *Registry) ByKind(k Kind) []Entry {
	all := r.All()
	out := make([]Entry, 0, len(all))
	for _, e := range all {
		if e.Kind() == k {
			out = append(out, e)
		}
	}
	return out
}

// Len 返回登记数量。
func (r *Registry) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.byID)
}

// StopDeps 是清理时需要的外部动作（由 core 注入，避免本包依赖 core 的类型）。
type StopDeps struct {
	// RevokePolicy 撤销父为该子声明的主体策略。
	//
	// ★ 必须注入：主体表以调用方给的名字为键，同名重建会继承旧策略，
	// 且表会随 create/destroy 单调增长（engineering-lessons.md §3.2）。
	RevokePolicy func(childID string)
	// OnDispose 在类型 Dispose **之后**调用，供需要父资源（调度器/IO）的
	// 类型收尾（如驻留子把接回的暂存任务转入父队列）。
	OnDispose func(e Entry)
	// UnregisterInbound 注销该子在父侧的入站 inputch。
	//
	// ★ 这一步曾经在"复数路径"上漏过 —— 就是本包要消灭的形态。
	UnregisterInbound func(channelName string)
}

// Cleanup 对一个已摘出登记表的实例执行**骨架级**清理。
//
// 单数与复数路径**共用**它，这是"单数改了复数漏了"那个缺陷的结构性预防。
func (r *Registry) Cleanup(e Entry, deps StopDeps) {
	if e == nil || e.Base() == nil {
		return
	}
	b := e.Base()
	// ① 先标记停止 + cancel：让正在跑的轮次在下一个安全点退出。
	//    必须在 Dispose 之前 —— Dispose 要看当前状态决定残余处置。
	b.Stop()
	// ② 类型自己的收尾（残余/通道）。类型可能读 ctx，故排在 cancel 之后但
	//    只读状态、不依赖 ctx 存活。
	if msg := e.Dispose(); msg != "" {
		log.Printf("[child] %s (%s) disposed: %s", b.ID, e.Kind(), msg)
	}
	if deps.OnDispose != nil {
		deps.OnDispose(e)
	}
	// ③ 撤销主体策略。
	if deps.RevokePolicy != nil {
		deps.RevokePolicy(b.ID)
	}
	// ④ 注销入站登记（"child/<id>"）。
	if deps.UnregisterInbound != nil && b.InboundCh != "" {
		deps.UnregisterInbound(b.InboundCh)
	}
	log.Printf("[child] %s (%s) stopped", b.ID, e.Kind())
}

// StopOne 停止一个子（单数语义：不存在时返回错误）。
func (r *Registry) StopOne(id string, deps StopDeps) error {
	e := r.Remove(id)
	if e == nil {
		return ErrChildNotFound
	}
	r.Cleanup(e, deps)
	return nil
}

// StopAll 停止某类型（kind 为空表示全部）的子，返回停止数量。
//
// ★ 与 StopOne **共用 Cleanup**，不是复制一份循环体。
func (r *Registry) StopAll(kind Kind, deps StopDeps) int {
	all := r.All()
	var targets []Entry
	for _, e := range all {
		if kind == "" || e.Kind() == kind {
			targets = append(targets, e)
		}
	}
	for _, e := range targets {
		r.Remove(e.Base().ID)
		r.Cleanup(e, deps)
	}
	if len(targets) > 0 {
		log.Printf("[child] stopped %d instance(s) (kind=%q)", len(targets), kind)
	}
	return len(targets)
}
