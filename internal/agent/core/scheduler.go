package core

// 输入调度器：四级中断优先级 · 可抢占 · 现场保存/恢复。
//
// 设计依据 docs/zh/input-scheduler-design.md。
//
// # 模型（两类别 + 四级）
//
// 类别由**用哪个注入 API**决定，与通道名无关：
//   - TaskInterrupt：InjectInterrupt* 注入。带级别 L1..L4，可抢占，
//     可被更高级中断打断（被打断的现场压入**中断栈**）。
//   - TaskQueued：InjectText*/InjectInputSync* 与内核自循环。**无级别**，
//     用于“不需及时处理”的场景，可被**任何**中断打断。
//
// 级别只属于中断：
//   - L1..L3 由插件在 InjectOptions.Priority 里声明（见 clampPluginLevel）；
//   - L4 给“立即打断”能力：内核自身（raiseKernelInterrupt：panic / selfip）
//     与**内核级插件**（编译期内置插件，如 WebUI 终止按钮）可声明；
//     外部插件经 proc 桥被夹到 L3，core 里也再判一次来源。
//
// # 选择顺序
//
//	1. immediate —— 刚抢占成功的中断（抢占必须立即生效）
//	2. 中断队列 L4→L1（同级 FIFO）
//	3. 中断栈顶（与 2 的队头比级别，取高者；栈顶无级别时中断必胜）
//	4. 排队队列（FIFO）
//
// # 并发模型（不变量 I2）
//
// queue/running/栈/stats 只由 schedulerLoop 与调度 goroutine 写；
// interruptLoop 只写中断登记与让位信号，**从不碰帧**。外部读取一律经
// DumpScheduler() 加锁取快照。

import (
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// Level 是**中断**的优先级，由内核预定义四级。
//
// 语义：它衡量“这项工作有多不能等”，与具体通道名无关。
// 插件在中断注入时通过 InjectOptions.Priority 声明 L1..L3；
// **L4 由内核独占**（panic、内核事件 selfip），插件声明 L4 会被夹到 L3。
//
// 排队输入（InjectText* / InjectInputSync*）**没有级别**：它们本就是
// “不需及时处理”的那一类，可被任何中断打断（见 TaskClass）。
type Level int

const (
	// LevelBackground L1：完全可等。例：QQ/微信这类异步消息、批量通知。
	LevelBackground Level = 1
	// LevelMessage L2：一般提醒。例：插件希望尽快看到、但不紧急的提示。
	LevelMessage Level = 2
	// LevelInteractive L3：需及时处理。例：时钟/定时器到达、终端输出、交互输入。
	LevelInteractive Level = 3
	// LevelCritical L4：**内核独占**。panic 中断、内核事件中断（selfip）。
	// 插件不得声明此级。
	LevelCritical Level = 4
)

// DefaultLevel 是未显式声明时的中断级别。
//
// 取最低级是刻意的：**显式才是特权**，新插件不会默认拿到抢占权。
const DefaultLevel = LevelBackground

// clampPluginLevel 把**非内核级**来源声明的级别夹到 L1..L3。
//
// L4 是“立即打断”能力（panic / 内核事件 / 内核级插件的终止按钮），
// 只给内核与编译期内置插件；外部插件声明 L4 会被夹到 L3。
func clampPluginLevel(l Level) Level {
	if l < LevelBackground {
		return DefaultLevel
	}
	if l > LevelInteractive {
		return LevelInteractive
	}
	return l
}

func (l Level) String() string {
	switch l {
	case LevelBackground:
		return "L1-background"
	case LevelMessage:
		return "L2-message"
	case LevelInteractive:
		return "L3-interactive"
	case LevelCritical:
		return "L4-critical"
	default:
		return "L?-unknown"
	}
}

// ParseLevel 已删除。
//
// 为何不保留：优先级是**内核内部属性**，不是配置项。
// 曾一度做成 `core.agent.priority.<channel>`（配置中心可见），
// 那等于把内核的调度内部属性外化成运维配置，与设计意图相反。
//
// 现在级别的来源只有两个（见 Task/Level 注释）：
//   - 插件在中断注入时声明（InjectOptions.Priority，L1..L3）；
//   - 内核内部产生 L4（panic / selfip）。

// TaskClass 是任务的两大类别——**由“用哪个注入 API”决定，与通道名无关**。
//
// 这是模型的核心区分：
//   - InjectInterrupt*  → TaskInterrupt：带级别，可抢占，可被更高级中断打断（→ 中断栈）
//   - InjectText* / InjectInputSync* / 内核自循环 → TaskQueued：无级别，
//     可被**任何**中断打断（“用于不需要及时处理的场景”）
type TaskClass int

const (
	TaskQueued TaskClass = iota
	TaskInterrupt
)

func (c TaskClass) String() string {
	switch c {
	case TaskQueued:
		return "queued"
	case TaskInterrupt:
		return "interrupt"
	default:
		return "unknown"
	}
}

// TaskKind 区分任务来源。
type TaskKind int

const (
	// TaskKindInput 来自 io.InputChan（外部/插件注入的输入）。
	TaskKindInput TaskKind = iota
	// TaskKindSelf 来自 selfInputCh（内核自循环：记忆整理、子任务通知）。
	TaskKindSelf
)

func (k TaskKind) String() string {
	switch k {
	case TaskKindInput:
		return "input"
	case TaskKindSelf:
		return "self"
	default:
		return "unknown"
	}
}

// Task 是调度器的最小单位。
type Task struct {
	ID    uint64
	Class TaskClass
	// Level 仅对 TaskInterrupt 有意义；TaskQueued 恒为 0（无级别）。
	Level      Level
	Kind       TaskKind
	EnqueuedAt time.Time

	Event *agentIO.InputEvent // Kind == TaskKindInput
	Self  selfInputMsg        // Kind == TaskKindSelf

	// PreemptCount 是本任务被抢占的次数，用于饥饿防护：
	// effectiveLevel = min(L4, Level + min(PreemptCount, 2))。
	PreemptCount int
	// LastPreemptAt 是上次被抢占的时刻，用于抢占冷却。
	LastPreemptAt time.Time
}

// preemptPromotionCap 是抢占计数能带来的最大提升档数。
const preemptPromotionCap = 2

// preemptCooldown 是“刚被抢占过”的冷却期：期内不再被抢占，
// 避免高优先级流把同一任务反复打断到永不完结。
const preemptCooldown = 2 * time.Second

// effectiveLevel 返回任务的**有效**级别。
//
// 排队输入恒为 0（无级别）：任何中断（≥ L1）都大于它——这正好实现
// “排队输入可被任何中断打断”。
//
// 中断则叠加饥饿防护：被抢占越多的中断越“值钱”，逐步追上抢占它的流；
// 封顶 L4，因此它永远不会反过来抢占内核紧急中断。
func effectiveLevel(t *Task) Level {
	if t.Class != TaskInterrupt {
		return 0
	}
	p := t.PreemptCount
	if p > preemptPromotionCap {
		p = preemptPromotionCap
	}
	l := t.Level + Level(p)
	if l > LevelCritical {
		l = LevelCritical
	}
	return l
}

// canPreempt 是唯一的抢占判据。
//
// 由于 effectiveLevel(排队)=0，这一个比较同时覆盖两条规则：
//   - running 是排队任务 → 任何中断（≥L1）都能抢占；
//   - running 是中断 Li    → 只有 Lj > Li 的中断能抢占（严格大于）。
func canPreempt(incoming, running *Task) bool {
	if incoming == nil || running == nil {
		return false
	}
	if incoming.Class != TaskInterrupt {
		return false // 排队输入从不抢占
	}
	return effectiveLevel(incoming) > effectiveLevel(running)
}

// SchedulerStats 是调度器的累计计数（可观测性，设计文档 §11 O2）。
//
// InterruptsByLevel / PreemptsByLevel 按**中断级别**分桶（下标 1..4）：
// “各级中断各登记了多少、各真正抢断了多少次”。按级别验收（而不是只看总数）
// 是这套调度器的核心判据——总数相同、级别分布不同，行为完全不同。
type SchedulerStats struct {
	Enqueued uint64
	Executed uint64
	// Rejected 是因队列满（或深度超限）而未被接纳的次数。
	Rejected uint64
	// Backpressure 是就绪队列满、输入被挡回 channel 的次数
	// （设计 §4.4 / §11.4 Q4：满时阻塞发送方，**必须计数并打日志**）。
	// 与 Rejected 的区别：Rejected 是「丢了」，Backpressure 是「暂时不收、发送方在等」。
	Backpressure uint64
	// Suspended / Resumed 是挂起与恢复的次数。
	// 不变量：系统排空后 Suspended == Resumed（挂起必然被恢复），
	// 因此两者各自只在**一处**计数（suspend / resumeTask）。
	Suspended uint64
	Resumed   uint64
	// InterruptsByLevel[1..4]：各级中断被**登记**的次数（含未抢占成功的）。
	InterruptsByLevel [5]uint64
	// PreemptsByLevel[1..4]：各级中断**判定为可抢占并进入 immediate**的次数。
	// 注意它与 Suspended 不等价：受害者可能在让位信号生效前就自行结束，
	// 此时抢占者仍然"下一个运行"，但没有挂起发生。
	PreemptsByLevel [5]uint64
}

// bumpInterruptLevel 按级别累加（级别必须落在 1..4，否则忽略——
// 排队任务没有级别，不该出现在中断计数里）。
func (st *SchedulerStats) bumpInterruptLevel(dst *[5]uint64, lv Level) {
	if lv >= LevelBackground && lv <= LevelCritical {
		dst[lv]++
	}
}

// SchedulerSnapshot 是调度器的原子快照。
type SchedulerSnapshot struct {
	Running *Task
	// Queue 是排队输入队列（无级别，FIFO）。
	Queue []*Task
	// InterruptQueues[level] 是四条中断队列（下标 1..4，同级 FIFO）。
	InterruptQueues [5][]*Task
	// Immediate 是刚抢占成功、将在下一个安全点立即运行的中断（最多一个）。
	Immediate *Task
	// PendingInterrupts = 四条中断队列 + Immediate（对外的待处理中断总数视图）。
	PendingInterrupts []*Task
	// SuspendStack：中断栈（含嵌套抢占的多个现场），**栈顶**优先恢复。
	SuspendStack []*suspendedTask
	Stats        SchedulerStats
	// MaxInterruptFrames 是中断栈帧数的结构上界（= 中断级数，不是配置项）。
	MaxInterruptFrames int
}

// schedulerStatus 把快照转成对外的状态 DTO（不暴露帧内容）。
func (a *Agent) schedulerStatus() sdk.SchedulerStatus {
	if a.sched == nil {
		return sdk.SchedulerStatus{}
	}
	snap := a.DumpScheduler()
	out := sdk.SchedulerStatus{
		ReadyQueueDepth:   len(snap.Queue),
		PendingInterrupts: len(snap.PendingInterrupts),
		SuspendStack:      len(snap.SuspendStack),
		MaxSuspendDepth:   snap.MaxInterruptFrames,
		InterruptsByLevel: snap.Stats.InterruptsByLevel,
		PreemptsByLevel:   snap.Stats.PreemptsByLevel,
		Enqueued:          snap.Stats.Enqueued,
		Executed:          snap.Stats.Executed,
		Rejected:          snap.Stats.Rejected,
		Suspended:         snap.Stats.Suspended,
		Resumed:           snap.Stats.Resumed,
		Backpressure:      snap.Stats.Backpressure,
	}
	// Preempted 是「各级抢占成功次数之和」，**不是** Suspended：受害者可能在
	// 让位信号生效前就自行结束，此时有抢占而没有挂起（见 PreemptsByLevel 注释）。
	// 此前这里直接拿 Suspended 顶替，导致 DTO 里 preempted 与 preempts_by_level 自相矛盾。
	for lv := LevelBackground; lv <= LevelCritical; lv++ {
		out.Preempted += snap.Stats.PreemptsByLevel[lv]
	}
	if snap.Running != nil {
		out.Running = &sdk.SchedulerTask{
			ID: snap.Running.ID, Level: int(snap.Running.Level), Kind: snap.Running.Kind.String(),
		}
	}
	if snap.Immediate != nil {
		out.Immediate = &sdk.SchedulerTask{
			ID: snap.Immediate.ID, Level: int(snap.Immediate.Level), Kind: snap.Immediate.Kind.String(),
		}
	}
	// 四级队列深度：下标即级别（1..4），下标 0 留 0。
	for lv := LevelBackground; lv <= LevelCritical; lv++ {
		out.InterruptQueues[lv] = len(snap.InterruptQueues[lv])
	}
	// 中断栈帧：栈底 → 栈顶（谁先被压进去、谁又打断了它）。
	for _, f := range snap.SuspendStack {
		if f == nil || f.Task == nil {
			continue
		}
		out.SuspendFrames = append(out.SuspendFrames, sdk.SchedulerFrame{
			Task: sdk.SchedulerTask{
				ID: f.Task.ID, Level: int(f.Task.Level), Kind: f.Task.Kind.String(),
			},
		})
	}
	return out
}

type scheduler struct {
	mu       sync.Mutex
	queue    []*Task
	running  *Task
	seq      uint64
	stats    SchedulerStats
	maxQueue int

	// interruptQueues[level]：四条**中断队列**（level 1..4），同级 FIFO。
	// 未能立即抢占的中断（级别不足，或运行任务在临界区）按级别入队，
	// nextRef 从 L4 到 L1 依次扫描。
	interruptQueues [5][]*Task
	// immediate：刚抢占成功的中断。抢占必须**立即生效**，所以它不经队列，
	// 在下一个安全点直接运行。这也消除了“抢占者与被抢占者同级”的比较问题——
	// 抢占者根本不需要和栈顶比。
	immediate *Task
	// suspendStack：**中断栈**。被抢占后保存现场的任务压栈（LIFO），
	// 用于“中断被中断”的嵌套场景：只有**栈顶**参与恢复选择，栈内不做优先级重排。
	suspendStack []*suspendedTask
	// preemptArmed/preemptLevel：运行任务的“让位信号”。
	// interruptLoop 只写这两个字段与 pendingInterrupts；帧永远只由调度器读写。
	preemptArmed bool
	preemptLevel Level
	// critical 报告运行任务是否在不可抢占临界区（如记忆整理）。
	// 由于 interceptLoop 要读它，必须是原子的：帧仍只由调度器读写。
	critical atomic.Bool
	// backpressured 记录「就绪队列满」这一状态的翻转，用于只打一次日志。
	// 满着的时候 pumpInbox 每轮都会走到，逐轮打日志会把日志刷爆。
	backpressured bool
	// wake 用于把空闲的调度器叫醒：pendingInterrupts 不是 channel，
	// 没有这个信号时“空闲时到达的中断”会一直等下一次输入（设计 §5.1 ③）。
	wake chan struct{}
	// maxInterruptFrames：中断栈帧数的**结构上界**，不是配置项。
	//
	// 链条 = 排队(L0) ← I(L1) ← I(L2) ← I(L3) ← I(L4 运行中)，
	// 被挂起 4 帧；L4 之上没有更高级别，链到此为止。超限只可能是内核 bug，
	// 因此这里只做防御性计数，**不降级、不丢弃帧**。
	maxInterruptFrames int

	// cancelBudget 是「停止」剩下的短路配额（见 Agent.RequestStop）。
	//
	// 语义（用户明确的设计）：停止 = ①立即结束当前 LLM 推理；②对**停止那一刻
	// 已排队**的 x 条消息，后续依次在 pre-action 阶段短路，而不是把它们当
	// 中断/新输入再跑一遍。配额是快照值：停止之后**新到**的输入不受影响。
	cancelBudget int
	// stopArmed 标记“下一条待收尾的任务是因为用户按了停止”。
	// 取消 LLM 后 stepLLM 默认重跑本步；用户停止时必须改为直接收尾。
	stopArmed bool
}

// suspendedTask 是一个被抢占任务的现场。
type suspendedTask struct {
	Task  *Task
	Frame *TaskFrame
}

// nextSelection 标识 nextRef 从哪个集合取出任务。
type nextSelection int

const (
	nextNone nextSelection = iota
	nextReady
	nextInterrupt
	nextImmediate
	nextSuspended
)

func newScheduler(maxQueue int) *scheduler {
	if maxQueue <= 0 {
		maxQueue = 256
	}
	return &scheduler{
		maxQueue:           maxQueue,
		maxInterruptFrames: int(LevelCritical), // 结构推论：= 中断级数
		wake:               make(chan struct{}, 1),
	}
}

// signalWake 非阻塞地唤醒调度器。
func (s *scheduler) signalWake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// runningTask 返回当前正在运行的任务（无则 nil）。
//
// 供自动转投判定"主 agent 是否忙"用。本函数只读取指针，不参与调度决策。
func (s *scheduler) runningTask() *Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// runningFor 返回当前运行任务已持续多久；无运行任务时为 0。
//
// 为什么用 EnqueuedAt 而不是另开一个"开始时间"字段：Task 已有 EnqueuedAt
// （allocateIDLocked 保证非零），它对外排队输入就是"多久没人处理它"，
// 恰好也是转投判定关心的量。新增字段会多一份需要维护的真相。
func (s *scheduler) runningFor() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil || s.running.EnqueuedAt.IsZero() {
		return 0
	}
	return time.Since(s.running.EnqueuedAt)
}

// takeQueuedInputs 从就绪队列**前端**取出至多 max 条纯排队输入（用于转投）。
//
// 只取 TaskQueued + KindInput：
//   - 中断任务带级别语义，転投会打乱中断阶梯，不动；
//   - self 任务（记忆整理等）与父的记忆面绑定，不能换 agent。
//
// 只从**前端**取：队列是 FIFO，前端就是"最久没人处理"的那几条；
// 从尾部抽会把后来者先送走，反而拉长前面几等的等待。
//
// 少于 min 条时**什么都不取**（返回 nil）：拉起一个 agent 的成本不该为
// 一条任务付；宁等下一轮积到量再一起转。这使得本函数要么不动、要么成批移动。
func (s *scheduler) takeQueuedInputs(min int) []offloadCandidate {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []offloadCandidate
	for _, t := range s.queue {
		if t == nil || t.Class != TaskQueued || t.Kind != TaskKindInput || t.Event == nil {
			continue
		}
		// ❗内核自己造的说明（source=kernel）**不能再被转投**。
		//
		// 它本来就是"告知这条信息不用再处理"的通知，而转投又会在队列里留下
		// 一条新的同款通知 ⇒ 自我循环：每次转投都产生下一轮要转投的东西。
		// 实测（2026-09-19）：5 秒内连续触发两次，子侧不断收到这类噪音。
		if isKernelNotice(t.Event) {
			continue
		}
		out = append(out, offloadCandidate{Event: t.Event})
		if len(out) >= min {
			break
		}
	}
	if len(out) < min {
		return nil
	}
	// 真的取走：重建队列，抽掉刚选中的那些事件（按指针同一性判断）。
	selected := make(map[*agentIO.InputEvent]bool, len(out))
	for _, c := range out {
		selected[c.Event] = true
	}
	kept := s.queue[:0]
	for _, t := range s.queue {
		if t != nil && t.Event != nil && selected[t.Event] {
			continue
		}
		kept = append(kept, t)
	}
	s.queue = kept
	return out
}

// requeueFront 把任务放回队列**前端**（保持原相对顺序）。
//
// 转投失败/未生效时用，保证"输入只多不少"：吞掉一条输入比多处理一条更糟
// （与 routeOnInputByOwner 的兜底同一条理由）。
// self 任务在転投路径上不可能出现（takeQueuedInputs 已排掉），所以这里只需处理 Event。
func (s *scheduler) requeueFront(cands []offloadCandidate) {
	if len(cands) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	restored := make([]*Task, 0, len(cands))
	for _, c := range cands {
		if c.Event == nil {
			continue
		}
		t := newInputTask(c.Event)
		// 保持"这些任务比当前队列里的一切都早"的语义。
		if t.EnqueuedAt.IsZero() {
			t.EnqueuedAt = time.Now()
		}
		restored = append(restored, t)
	}
	if len(restored) == 0 {
		return
	}
	s.queue = append(restored, s.queue...)
}

// setCritical 由调度器 goroutine 在任务进入/离开临界区时设置。
func (s *scheduler) setCritical(v bool) { s.critical.Store(v) }

// inCritical 报告运行任务是否在不可抢占临界区。
func (s *scheduler) inCritical() bool { return s.critical.Load() }

// hasRoom 报告排队队列是否还能接收任务。泵入侧据此节流：
// 队列满则停止从 channel 取，让背压落回 channel 本身。
func (s *scheduler) hasRoom() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue) < s.maxQueue
}

// noteBackpressure 记一次背压，并报告这是否是「从有空间 → 满」的翻转。
//
// 为什么需要翻转信息：满的时候每轮泵入都会调用本函数，逐轮打日志会刷爆；
// 而设计 §4.4 要求「必须计数并打日志」——两者靠这个布尔量同时满足。
func (s *scheduler) noteBackpressure() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Backpressure++
	if s.backpressured {
		return false
	}
	s.backpressured = true
	return true
}

// clearBackpressure 在就绪队列重新可收（泵空）时复位翻转标记。
func (s *scheduler) clearBackpressure() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backpressured = false
}

// allocateIDLocked 分配任务 ID 与入队时刻（调用方持锁）。
func (s *scheduler) allocateIDLocked(t *Task) {
	s.seq++
	t.ID = s.seq
	if t.EnqueuedAt.IsZero() {
		t.EnqueuedAt = time.Now()
	}
}

// enqueue 把一个**排队输入**入队；队列满返回 false（调用方负责计数）。
func (s *scheduler) enqueue(t *Task) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) >= s.maxQueue {
		s.stats.Rejected++
		return false
	}
	s.allocateIDLocked(t)
	s.stats.Enqueued++
	s.queue = append(s.queue, t)
	return true
}

// next 取出下一个要执行的任务；队列空返回 nil。
//
// 保留该签名供已有测试使用；调度器自用 nextRef（需要区分是否携带现场）。
func (s *scheduler) next() *Task {
	t, _, _ := s.nextRef()
	return t
}

// queueLen 返回当前排队任务数（**带锁**）。
//
// ★ 2026-10-06 新增：offload_test.go 有 6 处直读 s.queue 取 len。
//
//	那在「无并发写者」时安全（测试用 newRootWithoutSchedulerLoop，
//	刻意不启 schedulerLoop），但**只要有人改成 newRootWith 就变成真竞争**，
//	而且是「今天绿的、改一次就炸」的那种。
//
//	⇒ 生产代码本来就走 takeQueuedInputs/hasRoom 等带锁访问器；
//	这里补一个对称的只读访问器，让测试不必直读内部字段。
func (s *scheduler) queueLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// queueSnapshot 返回排队任务的浅拷贝，供测试断言内容（**带锁**）。
//
// 返回拷贝而非内部切片：调用方拿到的就是某个时刻的快照，
// 后续并发 enqueue/dequeue 不会让它的 len 或 range 在中途变化。
func (s *scheduler) queueSnapshot() []*Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Task, len(s.queue))
	copy(out, s.queue)
	return out
}

// nextRef 选出下一个任务。优先顺序：
//
//  1. immediate —— 刚抢占成功的中断（抢占必须立即生效）
//  2. 中断队列 L4→L1（同级 FIFO）
//  3. 中断栈顶（与 2 比级别取高者；栈顶是排队任务时视为最低）
//  4. 排队队列（FIFO）
func (s *scheduler) nextRef() (*Task, *TaskFrame, nextSelection) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.immediate != nil {
		t := s.immediate
		s.immediate = nil
		s.running = t
		return t, nil, nextImmediate
	}

	qTask, qLevel := s.highestInterruptLocked()

	// 中断栈：只比**栈顶**（严格 LIFO）。栈内不做优先级重排——
	// 嵌套抢占天然使栈自底向上级别递增，且“后被打断的先恢复”才是栈语义。
	if n := len(s.suspendStack); n > 0 {
		top := s.suspendStack[n-1]
		// 栈顶 vs 最高级待处理中断：取高者（持平归栈顶，维持 LIFO 与公平）。
		if qTask == nil || effectiveLevel(top.Task) >= qLevel {
			// 这里只负责“选出”；Resumed 由 resumeTask 计一次（否则会双计，
			// 使“排空后 Suspended == Resumed”这条不变量失真）。
			s.suspendStack = s.suspendStack[:n-1]
			s.running = top.Task
			return top.Task, top.Frame, nextSuspended
		}
	}

	if qTask != nil {
		s.popInterruptLocked(qLevel)
		s.running = qTask
		return qTask, nil, nextInterrupt
	}

	if len(s.queue) > 0 {
		t := s.queue[0]
		s.queue = s.queue[1:]
		s.running = t
		return t, nil, nextReady
	}
	return nil, nil, nextNone
}

// highestInterruptLocked 返回当前最高级非空中断队列的队头及其级别。
func (s *scheduler) highestInterruptLocked() (*Task, Level) {
	for lv := LevelCritical; lv >= LevelBackground; lv-- {
		if q := s.interruptQueues[lv]; len(q) > 0 {
			return q[0], lv
		}
	}
	return nil, 0
}

// popInterruptLocked 弹出某级别中断队列的队头（调用方已确认非空）。
func (s *scheduler) popInterruptLocked(lv Level) {
	s.interruptQueues[lv] = s.interruptQueues[lv][1:]
}

// interruptCountLocked 统计所有待处理中断（含 immediate 槽）。
func (s *scheduler) interruptCountLocked() int {
	n := 0
	for lv := LevelBackground; lv <= LevelCritical; lv++ {
		n += len(s.interruptQueues[lv])
	}
	if s.immediate != nil {
		n++
	}
	return n
}

// setImmediateLocked 登记一个应“立即运行”的抢占者。
//
// 槽只有一格：若已有抢占者且新的级别更高，旧的降级入队；否则新的入队。
// 返回 true 表示 t **确实占住了 immediate 槽**；false 表示它被降级进了自己的
// 级别队列（immediate 是单槽，这是设计要求的降级分支，见设计 §2「至多一个」）。
//
// 调用方必须用返回值决定是否计入 PreemptsByLevel：那条计数器的语义是
// 「进入 immediate 的次数」，被降级的中断从未进过 immediate。
// setImmediateLocked 尝试把 t 放进 immediate 槽。
//
// 返回 true：t 已占住 immediate（若原有抢占者被顶掉，它**已被**降级入队）。
// 返回 false：t 没有进 immediate，且本函数**未动 t** —— 调用方负责按级别入队。
//
// 把「降级入队」的责任留给调用方，是为了让「到底入队了几次」只有一个出口：
// 早先由本函数在返回 false 前自行入队，调用方又照着 false 再入一次，
// 同一任务就会在队列里出现两份（实测：中断任务被执行两次、Executed 虚高）。
func (s *scheduler) setImmediateLocked(t *Task) bool {
	if s.immediate != nil && effectiveLevel(t) <= effectiveLevel(s.immediate) {
		return false
	}
	if s.immediate != nil {
		s.enqueueInterruptLocked(s.immediate)
	}
	s.allocateIDLocked(t)
	s.stats.Enqueued++
	s.immediate = t
	return true
}

// armStop 处理一次「停止」指令：登记短路配额并返回**停止那一刻的排队深度**。
//
// 语义（用户明确的设计）：停止 = ①立即结束当前 LLM 推理；②对停止那一刻
// 已排队的 x 条消息，后续依次在 pre-action 阶段短路。配额是**快照值**：
// 停止之后新到的输入不受影响（否则停止会变成一个永远生效的“黑洞”）。
//
// 多次按停止取**较大值**而不是累加：两个客户端同时按下时配额不应翻倍。
func (s *scheduler) armStop(pending int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	// pending 是**还没被 pumpInbox 搬进队列**的那一段（停在输入 channel 里）。
	// 用户按下停止时，调度器通常正忙于当前任务，其它消息基本都停在 channel；
	// 只数 s.queue 会得到 0（实测），配额随之失效。
	queued := len(s.queue) + pending
	if queued > s.cancelBudget {
		s.cancelBudget = queued
	}
	s.stopArmed = true
	return s.cancelBudget
}

// takeStop 消费「当前任务应被立即结束而不是重试」这一次标记。
//
// 取消 LLM 后 stepLLM 会看到 context.Canceled 并 outcomeContinue 重跑；
// 若这是用户按下的停止，重跑就是错的——应该直接收尾。
func (s *scheduler) takeStop() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.stopArmed {
		return false
	}
	s.stopArmed = false
	return true
}

// consumeCancel 消费一格短路配额；true 表示本任务在 pre-action 阶段直接收尾。
func (s *scheduler) consumeCancel() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelBudget <= 0 {
		return false
	}
	s.cancelBudget--
	return true
}

func removeTask(list []*Task, target *Task) []*Task {
	for i, t := range list {
		if t == target {
			return append(list[:i], list[i+1:]...)
		}
	}
	return list
}

// enqueueInterruptLocked 把一个未立即抢占的中断按其级别入队（调用方持锁）。
//
// 有界：满了丢**最老**的一条并计数（中断是提示性输入，宁可丢旧保新）。
func (s *scheduler) enqueueInterruptLocked(t *Task) {
	s.allocateIDLocked(t)
	if s.interruptCountLocked() >= s.maxQueue {
		for lv := LevelBackground; lv <= LevelCritical; lv++ {
			if len(s.interruptQueues[lv]) > 0 {
				s.popInterruptLocked(lv)
				s.stats.Rejected++
				break
			}
		}
	}
	lv := t.Level
	if lv < LevelBackground || lv > LevelCritical {
		lv = DefaultLevel
	}
	s.interruptQueues[lv] = append(s.interruptQueues[lv], t)
	s.stats.Enqueued++
}

// requestPreempt 登记一次中断请求（class=TaskInterrupt）。
//
// 返回 true 表示“应该尝试取消运行任务正在进行的可取消步骤（LLM 流式）”。
//
// 判据是 canPreempt（由优先级级别系统一承担），并受抢占冷却约束：
//   - running 是排队任务 → 任何中断都抢占；
//   - running 是中断 Li    → 仅 Lj > Li 的中断抢占。
//
// 能抢占时把中断放进 immediate（立即生效）；否则按其级别入队，等当前任务
// 结束或下一个安全点再处理——无论哪种，中断都不会丢。
//
// 临界区（如记忆整理）内不 arm、不取消：中断只入队，等临界区结束后的安全点处理，
// 这是设计 §4.3 的硬要求——那个位置的“不抢占”不能只是不让位，还必须不取消。
// level 必须是**已解析好**的中断级别（含特权判定）：
// 生产路径只有 interruptLoop，它用 (*Agent).interruptLevel 得出 level；
// 内核自身用 requestKernelPreempt（固定 L4）。本函数不再夹取，
// 否则内核级插件的 L4 会被无辜削掉。
func (s *scheduler) requestPreempt(evt *agentIO.InputEvent, level Level) bool {
	return s.registerInterrupt(newInterruptTask(evt, level))
}

// requestKernelPreempt 是**内核**中断入口（panic / 内核事件 selfip）。
//
// 级别固定 L4，且**不夹取**——这是 L4 的唯一来源，插件永远够不到。
func (s *scheduler) requestKernelPreempt(evt *agentIO.InputEvent) bool {
	return s.registerInterrupt(newKernelInterruptTask(evt))
}

// registerInterrupt 是登记中断的公共实现（任务已带好 Class/Level）。
func (s *scheduler) registerInterrupt(t *Task) bool {
	s.mu.Lock()
	running := s.running
	critical := s.critical.Load()
	s.stats.bumpInterruptLevel(&s.stats.InterruptsByLevel, t.Level)
	arm := false
	if !critical && canPreempt(t, running) {
		if running.LastPreemptAt.IsZero() || time.Since(running.LastPreemptAt) >= preemptCooldown {
			// 只有**真的占住 immediate 槽**才算一次抢占，才计入 PreemptsByLevel：
			// immediate 是单槽，若它被另一个更高级的抢占者占着，t 会走上而下的
			// 「否则入队」分支——那种情况 t 从未进入 immediate（否则同一安全点前
			// 到达两条同级中断时该计数会高估）。
			if s.setImmediateLocked(t) {
				arm = true
				s.preemptArmed = true
				s.preemptLevel = t.Level
				s.stats.bumpInterruptLevel(&s.stats.PreemptsByLevel, t.Level)
				// 抢占判决点：此刻 running 还是真正的受害者，记下来才能回答
				// 「我的任务被谁打断了」。放在 executeNewTask 里打是错的——
				// 那时 nextRef 已把 running 换成抢占者自己，日志会写成
				// "victim = 入侵者自己"（实测过这个错误输出）。
				log.Printf("[agent] preempt: %s from %s (L%d) preempts %s (%s)",
					describeTask(t), sourceOf(t, nil), int(t.Level),
					describeTask(running), sourceOf(running, nil))
			}
		}
	}
	if !arm {
		s.enqueueInterruptLocked(t)
		// 入队而不是抢占：这也是要能看见的（否则「中断明明到了却没生效」无从解释）。
		// 两种成因分开写：临界区 vs 级别不够/冷却期。
		reason := "level insufficient"
		if critical {
			reason = "running in critical section"
		} else if canPreempt(t, running) && running != nil && !running.LastPreemptAt.IsZero() && time.Since(running.LastPreemptAt) < preemptCooldown {
			reason = "preempt cooldown"
		}
		log.Printf("[agent] interrupt queued: %s from %s (L%d) vs %s (%s) — %s; queue=%d",
			describeTask(t), sourceOf(t, nil), int(t.Level),
			describeTask(running), sourceOf(running, nil), reason, s.interruptCountLocked())
	}
	s.mu.Unlock()

	if !arm {
		s.signalWake()
	}
	return arm
}

// preemptGrantedFor 报告运行任务是否应在当前安全点让位。
func (s *scheduler) preemptGrantedFor() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.preemptArmed || s.running == nil {
		return false
	}
	return s.preemptLevel > effectiveLevel(s.running)
}

func (s *scheduler) clearPreempt() {
	s.mu.Lock()
	s.preemptArmed = false
	s.preemptLevel = 0
	s.mu.Unlock()
}

// rearmPending 在**安全点重新求值**中断队列（设计 §4.3 / §5.2）。
//
// 为什么必须有这一步：中断只在 registerInterrupt 里被武装一次，而那一刻运行任务
// 可能正在临界区（S_TOOL_EXEC / ONNX / CAS）或处于抢占冷却期，于是请求只能入队。
// 若安全点不再回头看队列，它就永远等不到执行——只能等当前任务**自然结束**，
// 这违背设计承诺的「临界区期间到达的抢占请求……在临界区结束后的第一个安全点
// 重新求值」。可复现症状：WebUI 终止按钮连按两次，第二次（落在 2s 冷却窗内）
// 入队后再也不会被求值，「终止」看起来没反应。
//
// 判据与 registerInterrupt **完全同一套**（canPreempt + 冷却 + 临界区闸门），
// 因此不会凭空制造设计之外的抢占。
func (s *scheduler) rearmPending() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 已有让位信号、或 immediate 槽已被占用：下一个安全点的选择已经在路上，
	// 不必（也不该）重复武装。
	if s.preemptArmed || s.immediate != nil || s.running == nil || s.critical.Load() {
		return
	}
	// 冷却期内不武装：与 registerInterrupt 同一判据（抗饥饿）。
	if !s.running.LastPreemptAt.IsZero() && time.Since(s.running.LastPreemptAt) < preemptCooldown {
		return
	}
	// 中断队列本就按级别组织：从最高级往下找第一条能抢占的队头。
	// （队列里的任务有效级恒等于基础级，故「第一条能抢」= 最高级可抢占者。）
	for lv := LevelCritical; lv >= LevelBackground; lv-- {
		q := s.interruptQueues[lv]
		if len(q) == 0 {
			continue
		}
		t := q[0]
		if !canPreempt(t, s.running) {
			continue
		}
		s.popInterruptLocked(lv)
		if s.setImmediateLocked(t) {
			s.preemptArmed = true
			s.preemptLevel = t.Level
			s.stats.bumpInterruptLevel(&s.stats.PreemptsByLevel, t.Level)
		} else {
			// immediate 槽没拿到（理论上进不来，顶部已判 immediate == nil）：放回队列，
			// 否则任务会凭空消失。
			s.enqueueInterruptLocked(t)
		}
		return
	}
}

// suspend 保存现场。
//
// 深度上界是**结构推论**（= 中断级数），不是配置项：安全点上的 canSuspend 已提前
// 拦下超限情况，此处仅在竞态下兜底计数——绝不丢弃帧（帧丢了会丢副作用记录）。
func (s *scheduler) suspend(t *Task, f *TaskFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.suspendStack) >= s.maxInterruptFrames {
		s.stats.Rejected++
	}
	s.suspendStack = append(s.suspendStack, &suspendedTask{Task: t, Frame: f})
	s.stats.Suspended++
	// 饥饿防护：抢占计数 +1（提升有效级）并记录冷却起点。
	t.PreemptCount++
	t.LastPreemptAt = time.Now()
	if s.running == t {
		s.running = nil
	}

	// D1=B：中断任务在上一个任务之前的完整状态上开始运行，
	// 因此这里**不**把被打断任务的任何内容交给它。
	s.preemptArmed = false
	s.preemptLevel = 0
}

// sourceOf 从任务/帧里取一个**可辨识来源**，用于抢占日志与故障定位。
//
// 为什么必须记这个：此前 suspend/resume 只发事件不落日志（见
// executeNewTask/resumeTask），于是生产上「我的任务被谁打断了」完全不可查——
// 日志里只有 `interrupt from X` 和 `LLM request cancelled by preemption` 两行，
// **看不到受害者是谁**。排查时只能按时间猜，把相邻的输入误认成凶手。
//
// 取静态注入源（evt.Source）而不是 evt.OutputChannel：前者回答「谁送来的」
// （qq / homeagent-mail-bridge / timer / child/xxx），后者是回答要投到哪个通道，
// 两者在多数场景下同名，但前者才是因果链上的那一环。
func sourceOf(t *Task, f *TaskFrame) string {
	if f != nil && f.Evt != nil {
		if f.Evt.Source != "" {
			return f.Evt.Source
		}
		return f.Evt.OutputChannel
	}
	if t != nil && t.Event != nil {
		if t.Event.Source != "" {
			return t.Event.Source
		}
		return t.Event.OutputChannel
	}
	if t != nil && t.Kind == TaskKindSelf {
		if t.Self.channel != "" {
			return "self:" + t.Self.channel
		}
		return "self"
	}
	return "?"
}

// describeTask 把任务的类别/级别拼成一段可读标签（日志用）。
func describeTask(t *Task) string {
	if t == nil {
		return "<nil>"
	}
	return fmt.Sprintf("task#%d class=%s level=%d", t.ID, t.Class, t.Level)
}

// canSuspend 报告还有下潜余量（安全点用它决定是否真的让位）。
func (s *scheduler) canSuspend() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.suspendStack) < s.maxInterruptFrames
}

// suspendDepth 返回当前中断栈深度（仅用于日志）。
//
// 单独一个方法而不是让调用方直接读 s.suspendStack：那个字段只允许在 s.mu 下访问，
// 而日志点不应该自己摸调度器内部状态（也不应该为了打一行日志多持一次锁的窗口）。
func (s *scheduler) suspendDepth() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.suspendStack)
}

// pendingEvents 收集**尚未执行**（排队队列 / 四条中断队列 / immediate）与
// **已挂起**（中断栈）任务所携带的、且带同步回执通道的输入事件。
//
// 用途只有一个：停机收尾。这些任务不会再被调度，若不给它们补终态，
// 无超时的同步注入方（cli / clawhubadapter）会永久挂起（设计 §7 I5、§11.3 X2/X4）。
func (s *scheduler) pendingEvents() []*agentIO.InputEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*agentIO.InputEvent
	add := func(t *Task) {
		if t != nil && t.Event != nil && t.Event.ResponseCh != nil {
			out = append(out, t.Event)
		}
	}
	for _, t := range s.queue {
		add(t)
	}
	for lv := LevelBackground; lv <= LevelCritical; lv++ {
		for _, t := range s.interruptQueues[lv] {
			add(t)
		}
	}
	add(s.immediate)
	for _, f := range s.suspendStack {
		if f != nil {
			add(f.Task)
		}
	}
	return out
}

// takeAllPendingEvents 取走**全部尚未执行**的输入事件（含无回执通道的异步输入），
// 并从队列中移除它们。返回的事件不再会被本调度器执行。
//
// 与 pendingEvents 的区别（两者用途完全不同，不要混用）：
//   - pendingEvents 只挑**带 ResponseCh** 的，用途是给同步调用方补终态；
//   - 本函数**不筛通道**，因为回收/销毁驻留子时要向父交代的是"手头还有哪些活"，
//     而 QQ/微信这类异步消息本来就没有 ResponseCh —— 它们恰恰是最容易被无声丢掉的。
//
// 只取 TaskQueued/KindInput 与中断队列里的输入事件；self 任务是内核内部记账
// （记忆整理），换 agent 没有意义，原地丢弃即可（不计入返回）。
func (s *scheduler) takeAllPendingEvents() []*agentIO.InputEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []*agentIO.InputEvent
	for _, t := range s.queue {
		if t != nil && t.Kind == TaskKindInput && t.Event != nil {
			out = append(out, t.Event)
		}
	}
	s.queue = nil
	for lv := LevelBackground; lv <= LevelCritical; lv++ {
		for _, t := range s.interruptQueues[lv] {
			if t != nil && t.Event != nil {
				out = append(out, t.Event)
			}
		}
		s.interruptQueues[lv] = nil
	}
	if s.immediate != nil && s.immediate.Event != nil {
		out = append(out, s.immediate.Event)
	}
	s.immediate = nil
	return out
}

// done 标记任务执行结束。
func (s *scheduler) done(t *Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == t {
		s.running = nil
	}
	// 任务正常结束：让位信号不再有意义（中断已在中断队列/immediate 里）。
	s.preemptArmed = false
	s.preemptLevel = 0
	s.stats.Executed++
}

// currentLevel 返回当前正在执行任务的级别；无 running 时为默认级。
//
// 用于在 prepare 段把级别写进帧（抢占比较的基准）。
// interruptLevel 返回一次**中断注入**的级别。
//
// 级别是“这项工作有多不能等”，由来源在 InjectOptions.Priority 里声明
// （排队注入没有级别，它们的 TaskClass 是 TaskQueued）。
//
// privileged 表示来源是**内核级插件**（编译期内置插件，见 isKernelLevelSource）：
//   - privileged=true  → 可用到 L4（实现“立即打断”，如 WebUI 终止按钮）
//   - privileged=false → 夹到 L1..L3；空/非法一律降级为 DefaultLevel（L1）
//
// 另有完全绕过本函数的 L4 来源：内核自身的 raiseKernelInterrupt（panic / selfip）。
func interruptLevel(evt *agentIO.InputEvent, privileged bool) Level {
	if evt == nil || evt.Payload == nil {
		return DefaultLevel
	}
	raw, _ := evt.Payload["priority"].(string)
	l, ok := parseInterruptLevel(raw)
	if !ok {
		return DefaultLevel
	}
	if privileged {
		return l
	}
	return clampPluginLevel(l)
}

// isKernelLevelSource 报告某来源是否是**内核级插件**（编译期内置插件）。
//
// 只有它们能声明 L4（见 interruptLevel）。判据是插件注册表里的“内置工厂”，
// 而不是插件自报的名字本身——外部插件经 proc 桥时已被夹到 L3，这里是第二道闸。
//
// source 的约定是 `插件名` 或 `插件名/实例`（如 webui/<deviceID>），故取第一段。
func (a *Agent) isKernelLevelSource(source string) bool {
	if source == "" {
		return false
	}
	name := source
	if i := strings.IndexByte(name, '/'); i > 0 {
		name = name[:i]
	}
	// ① 编译期内置插件（根 agent 的 L4 来源之一）。
	if a.pluginReg != nil && a.pluginReg.IsBuiltinPlugin(name) {
		return true
	}
	// ② **本 agent 的上级**（驻留子的父）—— 设计 §6.1 的 L4 通则：
	//    子的阶梯上只有父能产生 L4，所以父的"发送消息"一定能打断子。
	return a.kernelSource != "" && name == a.kernelSource
}

// parseInterruptLevel 解析插件声明的级别字符串（"L1".."L3"）。
// 只认字面量：拼写错误必须降级成默认级而不是被静默当成别的级别。
func parseInterruptLevel(s string) (Level, bool) {
	switch s {
	case "L1", "l1":
		return LevelBackground, true
	case "L2", "l2":
		return LevelMessage, true
	case "L3", "l3":
		return LevelInteractive, true
	case "L4", "l4":
		// 内核级：解析出来但会被 clamp 夹到 L3。
		return LevelCritical, true
	default:
		return 0, false
	}
}

// newInputTask 把一个**排队输入**包装成任务（无级别）。
func newInputTask(evt *agentIO.InputEvent) *Task {
	return &Task{Class: TaskQueued, Kind: TaskKindInput, Event: evt, EnqueuedAt: time.Now()}
}

// newInterruptTask 把一个中断请求包装成任务（带级别）。
func newInterruptTask(evt *agentIO.InputEvent, level Level) *Task {
	return &Task{Class: TaskInterrupt, Kind: TaskKindInput, Level: level, Event: evt, EnqueuedAt: time.Now()}
}

// newSelfTask 包装内核自循环输入——它是**排队任务**：记忆整理/子任务通知
// 不需要及时处理，可被任何中断打断。
func newSelfTask(msg selfInputMsg) *Task {
	return &Task{Class: TaskQueued, Kind: TaskKindSelf, Self: msg, EnqueuedAt: time.Now()}
}

// newKernelInterruptTask 构造一个**内核级中断**（L4）。
//
// 这是 L4 的唯一来源：panic 中断、内核事件中断（selfip）。
// 插件永远拿不到这个入口——它不经 InjectOptions，也不经 proc 桥。
func newKernelInterruptTask(evt *agentIO.InputEvent) *Task {
	return &Task{Class: TaskInterrupt, Kind: TaskKindInput, Level: LevelCritical, Event: evt, EnqueuedAt: time.Now()}
}

// DumpScheduler 返回调度器的原子快照（供状态页/测试断言）。
// roundsExecuted 返回本 agent 已执行的轮次数（供驻留子状态面展示）。
//
// 一轮 = 一次被执行的输入（排队与中断都算）。为什么不用 inputch 处理表的条数：
// 那张表记的是"当前上下文窗口内"的轮次，压缩会清空（设计 §8.3）——
// 拿它当轮次会让父看到轮次倒退。
func (a *Agent) roundsExecuted() int {
	if a.sched == nil {
		return 0
	}
	return int(a.DumpScheduler().Stats.Executed)
}

func (a *Agent) DumpScheduler() SchedulerSnapshot {
	if a.sched == nil {
		return SchedulerSnapshot{}
	}
	a.sched.mu.Lock()
	defer a.sched.mu.Unlock()
	snap := SchedulerSnapshot{Running: a.sched.running, Stats: a.sched.stats}
	snap.Queue = append(snap.Queue, a.sched.queue...)
	snap.Immediate = a.sched.immediate
	for lv := LevelBackground; lv <= LevelCritical; lv++ {
		snap.InterruptQueues[lv] = append(snap.InterruptQueues[lv], a.sched.interruptQueues[lv]...)
		snap.PendingInterrupts = append(snap.PendingInterrupts, a.sched.interruptQueues[lv]...)
	}
	if a.sched.immediate != nil {
		snap.PendingInterrupts = append(snap.PendingInterrupts, a.sched.immediate)
	}
	snap.SuspendStack = append(snap.SuspendStack, a.sched.suspendStack...)
	snap.MaxInterruptFrames = a.sched.maxInterruptFrames
	return snap
}

// schedulerLoop 是唯一的任务执行者（取代原 eventLoop 的输入处理）。
func (a *Agent) schedulerLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] schedulerLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.schedulerLoop()
		}
	}()

	for {
		// ★★ 退出检查必须在**取任务之前**（2026-10-18 修）。
		//
		// 原实现只在"队列空"分支里 select ctx.Done：
		//
		//	if kind == nextNone {
		//		select { ... case <-a.ctx.Done(): return }
		//	}
		//
		// 于是只要队列里有任务，循环就**永远不会看 ctx.Done** ——
		// 一直取、一直执行，直到队列被取空才可能退出。
		//
		// 这就是"cancel 了却停不下来"的根因：销毁流程调 child.cancel()
		// 之后，子仍把队列里的任务全取走执行，而那些任务的结果随销毁丢失。
		// （之前我为此加了一个 freeze 标志绕过 —— 那是把一个状态机的
		//   缺陷用另一个状态掩盖；正确的修法是在这里尊重 ctx。）
		select {
		case <-a.ctx.Done():
			return
		default:
		}

		a.pumpInbox()

		t, f, kind := a.sched.nextRef()
		if kind == nextNone {
			// 无待办：阻塞等新输入、新中断（wake）或退出。
			select {
			case evt := <-a.io.InputChan():
				if !a.sched.enqueue(newInputTask(evt)) {
					a.emitSkippedReply(evt, "queue_full")
				}
			case msg := <-a.selfInputCh:
				_ = a.sched.enqueue(newSelfTask(msg))
			case <-a.sched.wake:
				// 中断已入 pendingInterrupts，回到循环顶部重新挑选。
			case <-a.ctx.Done():
				return
			}
			continue
		}

		if kind == nextSuspended {
			a.resumeTask(t, f)
			continue
		}
		a.executeNewTask(t)
	}
}

// schedStopTimeout 是等调度器停下的上限。
//
// 不能无限等：子可能正跑一个长工具调用（几十秒）。超时就如实记日志并继续 ——
// 宁可让那条正在执行的任务由 Stop 的 drainPendingInterrupts 补一个终态，
// 也不要让父的控制面调用永久挂住。
const schedStopTimeout = 5 * time.Second

// waitSchedulerStopped 等调度循环不再持有运行中的任务。
//
// 前提：调用方已 cancel（关 ctx.Done）。调度循环在**下次取任务前**看到
// ctx.Done 就退出（见 schedulerLoop 顶部的修复），于是 running 归零。
//
// 为什么不用一个 frozen 标志：调度器本来就有停止语义（ctx），
// 再引入一个状态会让"谁负责停"含糊，且忘了解除就永久僵住。
func (s *scheduler) waitSchedulerStopped(timeout time.Duration, name string) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if running == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	log.Printf("[resident] %s 调度器 %s 内未停止（仍在执行任务），"+
		"残余任务可能少回收一条（那条会由 Stop 补终态）", name, timeout)
}

// pumpInbox 把 channel 里**已就绪**的输入搬进就绪队列（非阻塞）。
//
// 为什么不直接边收边执行：先把已到达的输入收进队列，选择函数才有意义——
// M3 起抢占必然要看"队列里还压着什么"，而 channel 不是可枚举的结构。
//
// 队列满即停止泵入（背压落回 channel，语义与设计文档 §4.4 一致）。
func (a *Agent) pumpInbox() {
	for a.sched.hasRoom() {
		select {
		case evt := <-a.io.InputChan():
			// 返回值必须处理：静默丢弃会让同步调用方永久挂起（回执路径 E）。
			if !a.sched.enqueue(newInputTask(evt)) {
				a.sched.noteBackpressure()
				a.emitSkippedReply(evt, "queue_full")
			}
		case msg := <-a.selfInputCh:
			// 自循环输入没有同步调用方，满时记一次背压即可。
			if !a.sched.enqueue(newSelfTask(msg)) {
				a.sched.noteBackpressure()
			}
		case <-a.ctx.Done():
			return
		default:
			a.sched.clearBackpressure()
			return
		}
	}
	// 队列满：输入留在 channel 里，发送方阻塞（设计 §4.4「阻塞发送方」）。
	// 必须计数并打日志——否则运维看到 Rejected=0 会以为没背压，而输入正卡在 channel。
	if a.sched.noteBackpressure() {
		log.Printf("[agent] ready queue full (%d), input channel backpressured", a.sched.maxQueue)
	}
}

// executeTask 执行一个任务（测试与旧调用方的入口）；见 executeNewTask。
// raiseKernelInterrupt 是 **L4 的唯一入口**：panic 中断与内核事件中断（selfip）。
//
// 它不经 io.InputChan（那是外部/插件输入），而是直接向调度器登记一条内核中断：
// 级别固定 L4、不夹取、不受插件声明影响。这正是“L4 只有内核持有”的落点。
//
// 能否抢占由调度器按统一判据决定；若会抢占，则顺手取消可取消的 LLM 流式步骤
// （与 interceptLoop 对插件中断的处理完全一致）。
func (a *Agent) raiseKernelInterrupt(source, channel, text string) {
	if a.sched == nil {
		return
	}
	evt := &agentIO.InputEvent{
		Source:        source,
		Type:          "interrupt",
		OutputChannel: channel,
		Payload: map[string]interface{}{
			"content":           text,
			"interrupt":         true,
			"interrupt_source":  source,
			"interrupt_channel": channel,
			"kernel":            true,
		},
	}
	if a.sched.requestKernelPreempt(evt) {
		a.cancelCurrentLLM()
	}
}

// reportTaskPanic 把一个任务 panic 报告成内核 L4 中断。
//
// 递归保护是**结构性**的：若 panic 的任务本身就是 L4 内核中断，则不再产生新的
// L4——否则同一个 panic 会自我放大成中断风暴，与“内核事件”应有的语义相反。
func (a *Agent) reportTaskPanic(t *Task, r interface{}) {
	if t.Class == TaskInterrupt && t.Level >= LevelCritical {
		return
	}
	a.raiseKernelInterrupt("kernel", "kernel",
		fmt.Sprintf("内核事件：任务 #%d 发生 panic：%v（该任务已被丢弃，调度器存活）", t.ID, r))
}

func (a *Agent) executeTask(t *Task) {
	a.executeNewTask(t)
}

// executeNewTask 执行一个**新建**任务，并做任务级 panic 隔离（不变量 I6）。
//
// 与改造前的差异（有意）：原 eventLoop 在 panic 后重启整个循环，
// 现在一个任务的 panic 只丢弃该任务，调度器与其它任务不受影响。
func (a *Agent) executeNewTask(t *Task) {
	var f *TaskFrame
	var out stepOutcome = outcomeDone

	func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[agent] task#%d (%s) panic recovered: %v\n%s",
					t.ID, t.Level, r, debug.Stack())
				a.reportTaskPanic(t, r)
			}
		}()
		switch t.Kind {
		case TaskKindInput:
			f, out = a.runInputTask(t.Event)
		case TaskKindSelf:
			f, out = a.runInputTask(selfEvent(t.Self))
		}
	}()

	if out == outcomeSuspended && f != nil {
		// 挂起必须落日志：此前只发事件不落盘，导致生产日志里
		// 「谁把谁挤下去了」完全查不到（只有 interrupt from / cancelled 两行）。
		// 曾因此把时间上相邻的输入误判成凶手。
		log.Printf("[agent] suspend: %s (%s) yields to an interrupt; suspendStack=%d",
			describeTask(t), sourceOf(t, f), a.sched.suspendDepth())
		a.sched.suspend(t, f)
		a.publishEvent(events.EventScheduler, map[string]interface{}{
			"action": "suspend", "task": t.ID, "level": int(t.Level),
		})
		return
	}
	a.sched.done(t)
}

// resumeTask 从保存的现场继续一个被抢占的任务。
//
// 关键：不重建帧、不重跑 prepare 段——否则会重复提交上下文与事件。
// resumeTask 从保存的现场继续一个被抢占的任务。
//
// 关键：不重跑 prepare 段（否则会重复提交上下文与事件），而是先把基础前缀
// 重建到「中断任务之上」，再把本任务自己的现场接回去（见 rebaseFramePrefix）。
func (a *Agent) resumeTask(t *Task, f *TaskFrame) {
	a.sched.mu.Lock()
	a.sched.stats.Resumed++
	a.sched.mu.Unlock()
	log.Printf("[agent] resume: %s (%s) resumes after the interrupt finished",
		describeTask(t), sourceOf(t, f))
	a.publishEvent(events.EventScheduler, map[string]interface{}{
		"action": "resume", "task": t.ID, "level": int(t.Level),
	})
	a.rebaseFramePrefix(f)
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] resume task#%d panic recovered: %v\n%s",
				t.ID, r, debug.Stack())
			a.reportTaskPanic(t, r)
			a.sched.done(t)
		}
	}()

	out := a.runTaskSteps(f)
	if out == outcomeSuspended {
		a.sched.suspend(t, f)
		return
	}
	a.finishInputTask(f, out)
	a.sched.done(t)
}

// kernelNoticeSource 是内核自己造的输入事件的来源名（见 offload.syntheticEvent）。
//
// 它存在的主要理由是**可被识别**：转投等机制必须能排除"自己造的说明"，
// 否则会在队列里自我循环（说明 → 被转投 → 产生新说明 → …）。
const kernelNoticeSource = "kernel"

// isKernelNotice 判断一条输入是不是内核自己造的通知。
func isKernelNotice(evt *agentIO.InputEvent) bool {
	return evt != nil && evt.Source == kernelNoticeSource
}
