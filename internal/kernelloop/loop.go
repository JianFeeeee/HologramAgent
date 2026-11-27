// Package kernelloop 是内核的**运行期事件循环**（用户裁定 2026-11-17）。
//
// ## 用户原话
//
// > 「注意 eventloop 是内核的核心循环（处理输入事件路由，输出事件，维持定时器，
// >    维持健康心跳），agentloop 才是各个 agent（含主 agent）的循环。这样各个
// >    agent 与内核之间自然而然的就并行化了」
//
// > 「同时 eventloop 也是协调各个模块以及处理各个工具调用的核心推动者」
//
// > 「装配是装配，事件循环是事件循环，本来就是不相干的两件事」
//
// ## 定位：**不是模块**
//
// 本包不进 `kernelmod` 注册表、不产生 `Provides`/`Requires` 边、不参与分层，
// 也不在别人的 `Init` 里被构造。它是**装配完成之后**才开始转的运行期核心：
// 拿同一张 `ks.Require` 设施表取能力，把已经装好的东西推动起来。
//
//	装配期：模块 Init → Provide(能力)      （互相不认识，只认设施名）
//	运行期：Loop.Start() → 推动它们
//
// ## 它推动什么
//
//	事件投递（Dispatcher）      把一次已过闸事件投给订阅者，而不是让发布者
//	                           在自己的栈上同步跑完所有 handler
//	输入路由（Router）          inputch 按 Owner 送到持有它的 agent
//	定时器（RegisterPeriodic）   各类型注册自己的回调，由本循环统一刻度
//	健康心跳（StartHeartbeat）   触碰 <data>/heartbeat，供 guard 判定存活
//
// ## 与 agentloop 的边界
//
// agentloop（`internal/agent/agent` 里的 Loop 槽位）只管"把到手的输入变成一帧
// 任务、跑本类型状态机、按契约产出"。它不碰路由、不碰设备、不碰刻度 ——
// 那些全在本包。两者只通过**通道**与**投递口**交会，因此天然并行。
package kernelloop

import (
	"fmt"
	"log"
	"os"
	"runtime/debug"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// Loop 是运行期事件循环。
//
// 零值不可用：必须经 New 构造（内部有队列与注册表）。
type Loop struct {
	// events 是投递队列的通道（有界：满了就背压，不丢事件）。
	events chan dispatchItem
	// stop 关闭后循环退出（由 Stop 关闭，幂等）。
	stop     chan struct{}
	stopOnce sync.Once
	// done 在投递与定时两个 worker 全部退出后关闭。
	done sync.WaitGroup

	// periodic 是定时任务登记（装配完成后由各类型注册，运行期统一刻度）。
	periodicMu sync.Mutex
	periodic   []PeriodicTask

	// sinks 是"哪个 agent 从哪取输入"的登记表（输入路由用）。
	sinksMu sync.RWMutex
	sinks   map[string]InputSink

	// backlog 是投递队列的容量（有界，防订阅者拖垮内核）。
	backlog int

	// taskStops 按 owner 记录定时任务的停止通道（见 taskStopCh）。
	taskStopMu sync.Mutex
	taskStops  map[string]chan struct{}
}

// dispatchItem 是一次待投递的（事件 + 订阅者）组合。
type dispatchItem struct {
	evt      *model.Event
	handlers []model.Handler
}

// PeriodicTask 是一个**由类型注册**的定时任务。
//
// ★ 参数是 name/interval/fn 而不是 `*agent.Agent`：定时器是内核的，
// 它不该认识任何 agent 类型。类型模块在自己的装配点把 `a` 闭进 fn 里。
type PeriodicTask struct {
	// Owner 是登记这个任务的执行体（agent id 等）。空 = 内核自己的任务。
	//
	// ★ 为什么需要它：任务体通常闭包了某个 agent 的实例。该 agent 停下后
	// 任务必须一并注销，否则它会继续跑在一个已经 dead 的对象上 —— 那是
	// 悬空引用，症状是"日志里还有心跳、对象却已销毁"。
	Owner    string
	Name     string
	Interval time.Duration
	Run      func()
}

// InputSink 是"某个 agent 从哪收输入"的登记项（输入路由用）。
//
// ★ 它只说一句话：**把这条输入交给这个 agent**。至于那个 agent 之后
// 怎么处理（排队/抢占/覆盖/丢现场），全是该类型 agentloop 的事。
type InputSink interface {
	// Deliver 把一条输入交给该 agent。
	//
	// ★ 没有"中断还是排队"这个参数：**那是接收方的语义**。标签在
	//   `evt.Payload["priority"]`（L1..L4 纯数据），由该类型的投递口自己解释。
	Deliver(evt any)
}

// DefaultBacklog 是投递队列的默认容量。
//
// 有界是刻意的：无界队列会让"订阅者变慢"悄悄变成"内存涨到 OOM"。
// 满了就阻塞发布者（背压），与输入队列同一纪律。
const DefaultBacklog = 1024

// New 构造事件循环。
func New(backlog int) *Loop {
	if backlog <= 0 {
		backlog = DefaultBacklog
	}
	return &Loop{
		events:  make(chan dispatchItem, backlog),
		stop:    make(chan struct{}),
		sinks:   map[string]InputSink{},
		backlog: backlog,
	}
}

// ───────────────────────── 事件投递（Dispatcher） ─────────────────────────

// Dispatch 实现 `kernelapi.EventDispatcher`：把一次**已过闸、已脱敏**的
// 投递排进队列，由本循环的 worker 推动。
//
// ★ 为什么这里是阻塞入队而不是丢弃：事件是旁路信号，但"丢一条"会让
// 状态面与诊断缓慢失真，且没人会发现。队列满时让发布者等 —— 那是可见的背压。
//
// ★ 为什么在队列满且已 Stop 时不阻塞：停机路径上必须能返回，否则
// CloseAll 会卡在一个已经没人消费的队列上。
func (l *Loop) Dispatch(evt *model.Event, handlers []model.Handler) {
	if l == nil || len(handlers) == 0 {
		return
	}
	item := dispatchItem{evt: evt, handlers: handlers}
	select {
	case l.events <- item:
	case <-l.stop:
		// 已停机：不阻塞、不丢日志。停机后的事件本来就没有订阅者还活着。
	}
}

// runDispatcher 是投递 worker：逐条把事件推给订阅者。
func (l *Loop) runDispatcher() {
	defer l.done.Done()
	for {
		select {
		case item := <-l.events:
			l.deliver(item)
		case <-l.stop:
			// 停机时把队列里**已到达**的投递尽力做完（不丢已在路上的事件），
			// 然后退出。不做无限排空：队列可能一直被新事件填满。
			for {
				select {
				case item := <-l.events:
					l.deliver(item)
				default:
					return
				}
			}
		}
	}
}

// deliver 调用一批订阅者（panic 隔离，与总线的 safeCall 同一纪律）。
func (l *Loop) deliver(item dispatchItem) {
	for _, h := range item.handlers {
		if h == nil {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[kernelloop] event handler panic (isolated): %v\n%s", r, debug.Stack())
				}
			}()
			h(item.evt)
		}()
	}
}

// ───────────────────────── 输入路由 ─────────────────────────

// RegisterInputSink 登记"这个 agent 从哪收输入"（agent 启动时调用）。
//
// 重复登记覆盖：同名重建是常规操作（create → destroy → create）。
func (l *Loop) RegisterInputSink(agentID string, s InputSink) {
	if l == nil || agentID == "" || s == nil {
		return
	}
	l.sinksMu.Lock()
	l.sinks[agentID] = s
	l.sinksMu.Unlock()
}

// UnregisterInputSink 撤销登记（agent 销毁时调用，防同名重建接到旧实例）。
func (l *Loop) UnregisterInputSink(agentID string) {
	if l == nil || agentID == "" {
		return
	}
	l.sinksMu.Lock()
	delete(l.sinks, agentID)
	l.sinksMu.Unlock()
}

// Sink 取某 agent 的输入投递口（未登记返回 false）。
func (l *Loop) Sink(agentID string) (InputSink, bool) {
	if l == nil {
		return nil, false
	}
	l.sinksMu.RLock()
	defer l.sinksMu.RUnlock()
	s, ok := l.sinks[agentID]
	return s, ok
}

// RouteInput 把一条输入交给**持有该 inputch 的 agent**。
//
// owner 是 inputch 登记表里的归属（空 = 未分配，由根内核处理）。
// 返回 true 表示已被某个 agent 的投递口接管，调用方不再处理。
//
// ★ 路由只做一件事：**按归属转发**。悬空（orphaned）与暂存（stash）的语义
// 在通道登记表里（`io.ChannelRegistry`），不在这里 —— 本函数不认识它们，
// 只按调用方给出的 owner 查表。
func (l *Loop) RouteInput(agentID string, evt any) bool {
	if l == nil || agentID == "" {
		return false
	}
	s, ok := l.Sink(agentID)
	if !ok {
		// 归属到一个不存在（或已销毁、登记表尚未归还）的 agent：
		// **不吞输入** —— 返回 false 让根内核兜底，并由调用方留痕。
		return false
	}
	s.Deliver(evt)
	return true
}

// ───────────────────────── 定时器 ─────────────────────────

// RegisterPeriodic 注册一个定时任务（装配完成后、Start 之前调用）。
//
// 同名重复被拒：两个任务同名意味着注册点写错了，而"哪个生效"取决于
// 注册顺序 —— 那是不可判定的故障。
func (l *Loop) RegisterPeriodic(t PeriodicTask) bool {
	if l == nil || t.Name == "" || t.Interval <= 0 || t.Run == nil {
		return false
	}
	l.periodicMu.Lock()
	defer l.periodicMu.Unlock()
	for _, existing := range l.periodic {
		if existing.Name == t.Name {
			log.Printf("[kernelloop] periodic task %q already registered — ignoring duplicate", t.Name)
			return false
		}
	}
	l.periodic = append(l.periodic, t)
	return true
}

// PeriodicTasks 返回已注册的定时任务快照（诊断用）。
func (l *Loop) PeriodicTasks() []PeriodicTask {
	if l == nil {
		return nil
	}
	l.periodicMu.Lock()
	defer l.periodicMu.Unlock()
	return append([]PeriodicTask(nil), l.periodic...)
}

// runPeriodic 是单个定时任务的主循环（含 panic 隔离与重启）。
func (l *Loop) runPeriodic(t PeriodicTask, stopCh <-chan struct{}) {
	defer l.done.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[kernelloop] periodic task %q panicked: %v\n%s", t.Name, r, debug.Stack())
			select {
			case <-time.After(time.Second):
				l.done.Add(1)
				go l.runPeriodic(t, stopCh)
			case <-l.stop:
			case <-stopCh:
			}
		}
	}()
	ticker := time.NewTicker(t.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.Run()
		case <-l.stop:
			return
		case <-stopCh:
			return
		}
	}
}

// ───────────────────────── 生命周期 ─────────────────────────

// Start 启动运行期循环：投递 worker + 全部已登记定时任务。
//
// ★ 调用点必须是**装配完成之后**（kernelboot 报 MarkKernelComplete 之后）。
// 它不是装配的一部分：装配期一行都不该因为它改。
func (l *Loop) Start() {
	if l == nil {
		return
	}
	l.done.Add(1)
	go l.runDispatcher()

	l.periodicMu.Lock()
	tasks := append([]PeriodicTask(nil), l.periodic...)
	l.periodicMu.Unlock()
	for _, t := range tasks {
		l.done.Add(1)
		go l.runPeriodic(t, l.taskStopCh(t))
	}
	log.Printf("[kernelloop] started: dispatcher backlog=%d, %d periodic task(s)", l.backlog, len(tasks))
}

// Stop 停止循环（幂等）。等待两个 worker 退出，最多等 timeout。
func (l *Loop) Stop() {
	if l == nil {
		return
	}
	l.stopOnce.Do(func() { close(l.stop) })
	waitTimeout(&l.done, 5*time.Second, "kernelloop")
}

// waitTimeout 等 WaitGroup 完成，超时就如实记日志并继续。
//
// 不能无限等：某个订阅者可能正在做慢 IO。超时后停机继续走 ——
// 让一条慢 handler 拖住整个进程退出是更坏的结果。
func waitTimeout(wg *sync.WaitGroup, timeout time.Duration, what string) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		log.Printf("[kernelloop] %s did not stop within %s (a subscriber or task is still running)", what, timeout)
	}
}

// InstallDispatcher 把本循环的投递 worker 挂到事件总线上。
//
// ★ 经 facility 表的**注入点**（`kernelapi.FacEventDispatch`）而不是拿裸总线：
// 裸总线始终不出安全模块的手（第⑥次裁定）。本函数拿到的是
// `kernelapi.EventDispatchSink`，只有一个 SetDispatcher 动作。
func (l *Loop) InstallDispatcher(sink kernelapi.EventDispatchSink) error {
	if l == nil {
		return fmt.Errorf("kernelloop: nil loop")
	}
	if sink == nil {
		return fmt.Errorf("kernelloop: event dispatch sink is not provided — " +
			"the security module must provide it (FacEventDispatch)")
	}
	sink.SetDispatcher(l)
	return nil
}

// 编译期确认：本循环就是接口层要的投递驱动者。
var _ kernelapi.EventDispatcher = (*Loop)(nil)

// ───────────────────────── 输出事件的消费者 ─────────────────────────

// OutputEvent 是"内核输出面"上的一条事件（供常驻消费者看）。
//
// ★ 为什么本包要定义它：常驻消费者（如"把 memory_candidate 写进文本记忆"）
// 是**内核侧协调**的一部分（用户裁定「eventloop 也是协调各个模块的核心推动者」）。
// 它们是运行期起点，不该再散落在装配点里各自起 goroutine。
// 这里用最小字段集，避免本包认识 agent/io 的具体类型。
type OutputEvent struct {
	Target  string
	Type    string
	Payload map[string]interface{}
}

// OutputSource 是"输出事件从哪来"的最小契约（由 IO 适配）。
type OutputSource interface {
	// Output 返回输出事件的只读通道。
	Output() <-chan OutputEvent
}

// StopChan 返回循环的停止信号（供"随循环生灭"的常驻消费者共用）。
//
// ★ 只读用途：调用方 SELECT 它来退出，不许关闭它 —— 关闭权只在 Stop()。
func (l *Loop) StopChan() <-chan struct{} {
	if l == nil {
		return nil
	}
	return l.stop
}

// StartOutputConsumer 把一个常驻消费者挂到循环里（loop 停止时它一并退出）。
//
// ★ 语义：**推动**而不是"另起一个不受管的 goroutine"。消费者随循环生命周期，
// 停机时统一收口 —— 这正是"装配点不该有运行期起点"的落点。
func (l *Loop) StartOutputConsumer(name string, src OutputSource, handle func(OutputEvent)) bool {
	if l == nil || src == nil || handle == nil {
		return false
	}
	l.done.Add(1)
	go func() {
		defer l.done.Done()
		out := src.Output()
		for {
			select {
			case <-l.stop:
				return
			case evt, ok := <-out:
				if !ok {
					return
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("[kernelloop] output consumer %q panic (isolated): %v\n%s", name, r, debug.Stack())
						}
					}()
					handle(evt)
				}()
			}
		}
	}()
	log.Printf("[kernelloop] output consumer %q started", name)
	return true
}

// ───────────────────────── 健康心跳 ─────────────────────────

// Heartbeat 每 interval 触碰一个文件，供外部守护（guard/systemd）判定
// 本进程是否存活/卡死。
//
// ★ 为什么心跳归事件循环：它是"内核还在转"的信号，不是"某个 agent 还在转"。
//
//	一个 agent 卡住不该让整个进程被 guard 杀掉；反过来，内核循环停了
//	就必须被杀 —— 那正是本文件要表达的事实。
type Heartbeat struct {
	// Path 是心跳文件路径（通常是 `<data>/heartbeat`）。
	Path string
	// Interval 是触碰间隔（<=0 用 5s 默认）。
	Interval time.Duration
	// Write 是每次心跳的写入动作。nil 时用默认实现（截断重写 `t=<unix>`）。
	//
	// ★ 为什么可注入：worker 侧还要在同一文件里维护 `cmd=` 行（guard 的控制
	//   指令），那是部署细节，不该被写死进内核循环。
	Write func(path string, now time.Time)
}

// StartHeartbeat 把心跳登记为定时任务（Start 之后开始刻度）。
func (l *Loop) StartHeartbeat(hb Heartbeat) bool {
	if l == nil || hb.Path == "" {
		return false
	}
	interval := hb.Interval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	write := hb.Write
	if write == nil {
		write = defaultHeartbeatWrite
	}
	// 首次写入**同步**：停机后重启时文件 mtime 停在上一代，
	// 异步首写会让守护先看到旧 mtime 而误判 stale（生产实测过）。
	write(hb.Path, time.Now())
	return l.RegisterPeriodic(PeriodicTask{
		Name:     "heartbeat",
		Interval: interval,
		Run:      func() { write(hb.Path, time.Now()) },
	})
}

// defaultHeartbeatWrite 截断重写心跳文件（只保留当前一行）。
//
// 历史行没有读者：唯一读内容的是 guard 的 consumeHeartbeatCmd，它只关心
// `cmd=` 行。累加会让文件随时间线性增长（实测 14.6MB），而 mtime 语义不变。
func defaultHeartbeatWrite(path string, now time.Time) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(f, "t=%d\n", now.Unix())
	_ = f.Close()
}

// UnregisterPeriodicByOwner 注销某个执行体登记的全部定时任务。
//
// 该执行体停下时必须调：任务体闭包了它的实例，继续跑就是悬空引用。
func (l *Loop) UnregisterPeriodicByOwner(owner string) int {
	if l == nil || owner == "" {
		return 0
	}
	l.periodicMu.Lock()
	defer l.periodicMu.Unlock()
	kept := l.periodic[:0]
	n := 0
	for _, t := range l.periodic {
		if t.Owner == owner {
			n++
			continue
		}
		kept = append(kept, t)
	}
	l.periodic = kept
	// 通知正在跑的那个任务实例退出（它 select 在自己的 stopCh 上）。
	l.taskStopMu.Lock()
	if ch, ok := l.taskStops[owner]; ok {
		close(ch)
		delete(l.taskStops, owner)
	}
	l.taskStopMu.Unlock()
	return n
}

// taskStopCh 返回某个 owner 的停止通道（按 owner 共享一个）。
//
// 为什么按 owner 而不是按任务：一个 agent 的全部定时任务应当同时停 ——
// 它们通常一起创建、一起销毁。
func (l *Loop) taskStopCh(t PeriodicTask) <-chan struct{} {
	if t.Owner == "" {
		return nil // 内核自己的任务只随 Stop 退出
	}
	l.taskStopMu.Lock()
	defer l.taskStopMu.Unlock()
	if l.taskStops == nil {
		l.taskStops = map[string]chan struct{}{}
	}
	if ch, ok := l.taskStops[t.Owner]; ok {
		return ch
	}
	ch := make(chan struct{})
	l.taskStops[t.Owner] = ch
	return ch
}

// ───────────────────────── 全局可达的循环实例 ─────────────────────────

var (
	currentMu sync.RWMutex
	current   *Loop
)

// SetCurrent 登记本进程正在跑的循环（装配完成后由 startRuntime 调用一次）。
//
// ★ 为什么需要全局可达：内核内部的消费方（agent 的输入路由、待投递的
// 常驻消费者）要在运行期找到"推动者是谁"，但它们不该持有它 ——
// 那是把运行期结构塞进模块字段（本设计一直在消除的形态）。
// 与 `kstate.Get()` 同一手法：装配后登记，未装配返回 nil（零成本跳过）。
func SetCurrent(l *Loop) {
	currentMu.Lock()
	current = l
	currentMu.Unlock()
}

// Current 取本进程正在跑的循环（未装配/已停机时返回 nil）。
func Current() *Loop {
	currentMu.RLock()
	defer currentMu.RUnlock()
	return current
}
