package core

// 积压任务的**自动转投**：主 agent 长时间忙时，把排队中的任务改投给内核拉起的
// 驻留子，并在原队列位置留一条"已转投"提示。
//
// 为什么要这个（用户 2026-09-19 提出的实际需求）：
//   实测主 agent 被一条长任务占住时（当天现场：12 分 8 秒、69 次工具调用），
//   后来的 QQ 消息全部以 "level insufficient" 排进中断队列干等 —— 同级中断
//   不能抢占同级运行任务（scheduler.canPreempt），只能等前一个跑完。
//   而内核明明有驻留子（独立 agent + 独立调度器 + 共享输出通道视图）可以并行干活。
//
// 与设计文档 §7 的关系（**这是刻意的例外，必须显式记录**）：
//   设计原文写「创建/销毁/回收/查看/发送是父可调用的原语；**决策在父的模型手里**——
//   内核不替父决定」。本特性让**内核**主动创建并使用驻留子，属于对该原则的例外。
//   之所以可接受：父此刻正忙（无法做决策），而积压任务**本来就是空的**——
//   转投只是把"排队干等"换成"有人在做"，不改变任何已提交决策的语义。
//   若不做例外，这个能力就只能由父的模型发起，而它恰恰是忙不过来的那个。

import (
	"fmt"
	"log"
	"strings"
	"time"

	"runtime/debug"

	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
)

// OffloadOptions 是自动转投的判定与执行参数。
type OffloadOptions struct {
	// BusyAfter：运行任务已持续多久算"长时间工作"（0 = 用默认）。
	BusyAfter time.Duration
	// MinPending：至少要积压多少条才值得拉起驻留子（0 = 用默认）。
	MinPending int
	// MaxResidents：为转投而拉起的驻留子上限（0 = 用默认）。
	MaxResidents int
	// Enabled 为 false 时完全关闭（默认关：见 DefaultOffloadOptions 的说明）。
	Enabled bool
}

// 默认参数。
//
// 为什么默认**关闭**：自动拉起是"内核替父做决策"，改变的是系统行为而非修 bug；
// 且它会让日志/账单里凭空多出一个 agent 在干活。默认关闭、由部署方显式打开，
// 与「显式才是特权」（scheduler.DefaultLevel 的同一条理由）一致。
const (
	// defaultOffloadBusyAfter 是「忙多久算长任务」的默认值。
	//
	// 2026-10-18 从 5 分钟调成 1 分钟：实测本机正常任务最长 1.2 分钟，
	// 5 分钟的阈值意味着**永远触发不了** —— 配置开着却一行日志都没有
	// （这正是"看着有、实际没跑"的典型）。1 分钟能覆盖真实的忙不过来的场景，
	// 又不至于把正常的一次工具循环误判成长任务。
	defaultOffloadBusyAfter   = 1 * time.Minute
	defaultOffloadMinPending  = 3
	defaultOffloadMaxResident = 2
)

// DefaultOffloadOptions 返回默认参数（Enabled=false）。
func DefaultOffloadOptions() OffloadOptions {
	return OffloadOptions{
		BusyAfter:    defaultOffloadBusyAfter,
		MinPending:   defaultOffloadMinPending,
		MaxResidents: defaultOffloadMaxResident,
		Enabled:      false,
	}
}

func (o OffloadOptions) normalized() OffloadOptions {
	if o.BusyAfter <= 0 {
		o.BusyAfter = defaultOffloadBusyAfter
	}
	if o.MinPending <= 0 {
		o.MinPending = defaultOffloadMinPending
	}
	if o.MaxResidents <= 0 {
		o.MaxResidents = defaultOffloadMaxResident
	}
	return o
}

// offloadNotice 是替换被转投任务的那条提示的正文。
//
// 它必须**自己说清是系统做的**：用户看到队列里出现一条没人发过的消息时，
// 唯一能解释这件事的就是这句话本身。
//
// 同时要说清"不必重复处理"：那些消息已由子 agent 回复（或已回复"忙碌中"），
// 主 agent 再处理一遍会让用户收到重复回复。
func offloadNotice(count int, residentID string) string {
	return fmt.Sprintf(
		"[系统] %d 条积压消息已在主 agent 忙期间交由临时助手 %s 先行分诊"+
			"（简单的已直接处理并回复，需要你的那些已告知用户「忙碌中，请稍候」）。"+
			"它们**不需要你再处理**了；若其中有需要你后续跟进的，请查看上述通道的会话记录。"+
			"本提示仅用于说明情况，无需回复。",
		count, residentID)
}

// offloadCandidate 是一条可被转投的排队任务。
//
// 只有**纯排队输入**（TaskQueued + KindInput）可转投：
//   - 中断任务带级别语义（可能正在等待抢占时机），转投会打乱中断阶梯；
//   - self 任务是内核内部记账（记忆整理等），与父的记忆面绑定，不能换 agent。
type offloadCandidate struct {
	Event *agentIO.InputEvent
}

// drainInboxToQueue 把 io 输入 channel 里**已经到达但尚未被搬运**的输入
// 搬进就绪队列（非阻塞；取空为止）。
//
// 它与 pumpInbox 做的事一样，但**不要求 hasRoom**：pumpInbox 在队列满时
// 会停下以保留背压，而转投场景恰恰是「队列空/不满、但输入堵在 channel 里」
// （因为调度器正忙于执行任务、根本回不到 pumpInbox）。
//
// 队列上限仍由 enqueue 把关：满了就停下，超出的输入留在 channel 里。
func (a *Agent) drainInboxToQueue() {
	for {
		select {
		case evt := <-a.io.InputChan():
			if !a.sched.enqueue(newInputTask(evt)) {
				// 队列满：放不进去。不能丢，也不能阻塞（我们是后台 goroutine，
				// 阻塞会把这个循环永远卡住）——给同步调用方一个终态后丢弃，
				// 与 pumpInbox 的 queue_full 处置一致。
				a.sched.noteBackpressure()
				a.emitSkippedReply(evt, "queue_full")
				return
			}
		default:
			return
		}
	}
}

// 注：转投不走 inputch 名字（那会在投递时重建事件、丢掉 ResponseCh），
// 而是直接跨 agent 推原事件 —— 见 forwardInputToResident。

// offloadPendingTasks 检查是否需要转投，需要则拉起/复用一个驻留子并搬运任务。
//
// 返回实际转投的任务条数（0 = 未触发/未转投）。
//
// ❗并发前提：本函数会被 offloadLoop 在**任务执行期间**调用（那正是它的意义），
// 因此它与 schedulerLoop 是并发跑的。所有对队列的读写都经 scheduler 的锁，
// 而"取走哪些任务"与"放回什么"都在同一次锁内完成，不存在丢任务的窗口。
func (a *Agent) offloadPendingTasks(opts OffloadOptions) int {
	opts = opts.normalized()
	if !opts.Enabled {
		return 0
	}

	// ① 判定：运行任务是否已忙够久。运行任务为空说明压根不忙，不做。
	running := a.sched.runningTask()
	if running == nil {
		return 0
	}
	busyFor := a.sched.runningFor()
	if busyFor < opts.BusyAfter {
		return 0
	}

	// ② 先把积压从 io 的输入 channel **搬进就绪队列**。
	//
	// ！！这是本特性最容易写错的一步（我第一版就错了，写完后线上实测永不触发）：
	// schedulerLoop 是**同步执行**任务的，所以「正忙」期间它根本不会回到循环顶部
	// 去调 pumpInbox —— 这时后到的输入全部堆在 io.inputCh（容量 256）里，
	// **压根没进 sched.queue**。只数 s.queue 会得到 0，转投就永远不触发。
	//
	// 仓库里早记过同一个坑：armStop 的注释写着「pending 是还没被 pumpInbox 搬进
	// 队列的那一段……只数 s.queue 会得到 0（实测），配额随之失效」。
	// 这里必须在同一层把这件事做对，而不是重犯。
	a.drainInboxToQueue()

	// ③ 收集可转投的排队任务；不够量就不值得拉起一个 agent。
	cands := a.sched.takeQueuedInputs(opts.MinPending)
	if len(cands) == 0 {
		return 0
	}

	// ③ 找或拉起一个"转投专用"驻留子。
	residentID, err := a.ensureOffloadResident(opts)
	if err != nil {
		// 拉不起来就把任务**放回队列**，绝不能丢：丢一条输入比多处理一条更糟
		// （与 routeInputByOwner 的兜底同一条理由）。
		a.sched.requeueFront(cands)
		log.Printf("[offload] 无法为 %d 条积压任务准备驻留子，已放回队列: %v", len(cands), err)
		return 0
	}

	// ④ 搬运：逐条投进子的 inputch。
	//
	// 注意这里**逐条转发原文**而不是打包成一条：任务本身带 Source/OutputChannel
	// 等路由信息，打包会让子无法把回复发回正确的通道（qq 私聊 vs 群聊不同）。
	var moved int
	for _, c := range cands {
		if c.Event == nil {
			continue
		}
		if err := a.forwardInputToResident(residentID, c.Event); err != nil {
			// 某条投不进去：放回原队列，其余继续（部分成功好过全部回滚）。
			a.sched.requeueFront([]offloadCandidate{c})
			log.Printf("[offload] 转发任务给驻留子 %s 失败，已放回队列: %v", residentID, err)
			continue
		}
		moved++
	}
	if moved == 0 {
		return 0
	}

	// ⑤ 在**原队列位置**留下提示（用户要求的那条说明）。
	//
	// 为什么留在队列里而不是只记日志：队列顺序就是主 agent 接下来要处理的事；
	// 用户看会话记录时，需要在这里就看到"那几条去哪儿了"，而不是去翻内核日志。
	a.sched.requeueFront([]offloadCandidate{
		{Event: a.syntheticEvent(offloadNotice(moved, residentID))},
	})

	log.Printf("[offload] 主 agent 已忙 %s，把 %d 条积压任务转投给驻留子 %s（队列留 1 条说明）",
		busyFor.Truncate(time.Second), moved, residentID)
	return moved
}

// forwardInputToResident 把一条输入**原文**投给指定驻留子的队列。
//
// ❗必须推**原事件**（DeliverRouted），不能重建：原事件带 ResponseCh，
// 而 cli / a2a / webui 这些**同步**调用方正阻塞等它。重建事件（如用
// InjectInputTo）会把 ResponseCh 丢掉 ⇒ 任务被子处理完、调用方却永远收不到回执。
// 实测：转投生效、子也正常处理（各 ~3s 日志可见），但 HTTP 请求一直挂着不返回。
// 仓库反复警告同一件事（见 Agent.Stop 对 drainPendingInterrupts 的注释：
// “带 ResponseCh 的同步注入方会永久挂起”），这里必须走既有的跨 agent 投递原语。
//
// 用排队语义（isInterrupt=false）：转投的是"待办工作"，不是"打断子"。
func (a *Agent) forwardInputToResident(residentID string, evt *agentIO.InputEvent) error {
	a.residentMu.Lock()
	rc := a.residents[residentID]
	a.residentMu.Unlock()
	if rc == nil || rc.agent == nil || rc.agent.io == nil {
		return fmt.Errorf("驻留子 %s 不存在或不可用", residentID)
	}

	// 带上来源线索（不重建事件，只补充 payload，保留 ResponseCh/RequestID）。
	if evt.Payload == nil {
		evt.Payload = map[string]interface{}{}
	}
	evt.Payload["offloaded_from"] = string(a.id)
	evt.Payload["offloaded_at"] = time.Now().Format(time.RFC3339)

	rc.agent.io.DeliverRouted(evt, false)
	return nil
}

// ensureOffloadResident 返回一个可用于转投的驻留子 id，必要时拉起一个新的。
//
// 复用规则：优先复用"内核为转投而建"且仍 running、还没满的驻留子；
// 都不可用时（在 MaxResidents 内）新建一个。
func (a *Agent) ensureOffloadResident(opts OffloadOptions) (string, error) {
	a.residentMu.Lock()
	var reusable []string
	for id, rc := range a.residents {
		if rc == nil || !rc.offloadOwned {
			continue
		}
		rc.mu.Lock()
		state := rc.state
		rc.mu.Unlock()
		if state == "running" {
			reusable = append(reusable, id)
		}
	}
	a.residentMu.Unlock()

	// 复用：按 id 稳定排序后取第一个，避免每次挑到不同的子（可预测性）。
	if len(reusable) > 0 {
		sortStrings(reusable)
		return reusable[0], nil
	}

	// 计数：只为转投而建的子是否已达上限（人工建的子不计入）。
	a.residentMu.Lock()
	owned := 0
	for _, rc := range a.residents {
		if rc != nil && rc.offloadOwned {
			owned++
		}
	}
	a.residentMu.Unlock()
	if owned >= opts.MaxResidents {
		return "", fmt.Errorf("转投专用驻留子已达上限 %d", opts.MaxResidents)
	}

	id := fmt.Sprintf("offload-%d", time.Now().Unix())
	if a.dataDir == "" {
		return "", fmt.Errorf("未配置 DataDir，无法为驻留子分配 temp 图库路径")
	}
	info, err := a.SpawnResident(ResidentOptions{
		ID: id,
		// 不配 inputch：它是内核的**干活** agent，不接收任何插件的用户输入
		// （用户要求"不配输入通道"）。它只由父经转投拿到任务。
		InputChs: nil,
		// 全部输出通道：它要能把结果发回 qq/webui 等正确通道
		// （用户要求"持有全部输出通道"）。nil = 完整授权。
		AllowedOutputs: nil,
		TempPath:       a.residentTempPath(id),
		OffloadOwned:   true,
		TaskPrompt:     offloadTaskPrompt(),
	})
	if err != nil {
		return "", err
	}
	return info.ID, nil
}

// offloadTaskPrompt 是转投专用驻留子的**分诊职责**说明。
//
// 为什么必须给：不给的话子完全不知道自己为什么存在（只知道自己是"小宅"），
// 拿到一条转投消息时不知道它是"用户正在等回复的请求"，
// 也不知道自己只有两条路可走（直接办 / 报忙碌）。
//
// 用户的定位（2026-09-19 明确）：这不是"内核替父决定"，而是**及时反馈** ——
// 主 agent 忙时不该让用户干等（实测有 13 分钟的现场）。
// 子的职责是**分诊**（triage）：
//   - 简单、不需主 agent 介入的 → 直接办完并回复；
//   - 需要主 agent 介入的 → 立刻回「忙碌中，请稍候」，**不要勉强做**。
func offloadTaskPrompt() string {
	return `你是主 agent 的临时助手，负责在主 agent 忙不过来时**分诊**它的积压消息。

背景：主 agent 正在执行一个长任务，短时间无法处理新消息。你被临时拉起，
专门承接这些积压的请求，**避免用户干等**（此前用户可能要等十几分钟）。

对每一条消息，你只有两条路：

1. 【直接办】如果这件事简单、明确、不需要主 agent 的全局上下文或长期规划
   （例如：查个信息、跑个小命令、读个文件、简单问答）——
   **直接做完，并把结果发回原通道**。

2. 【报忙碌】如果这件事需要主 agent 介入（需要它的长期记忆、正在进行的任务上下文、
   需要它做多步决策，或你无法确定怎么做）——
   **不要勉强尝试**。立刻回复用户：主 agent 当前忙碌中，请稍候。

重要约束：
- **必须把回复发到用户原本的通道**。面向 qq、wechat 等异步通道时，
  纯文本返回会被丢弃 —— 必须显式调用 output_send__{通道名}，否则用户收不到，
  而你会以为已经回过了。

- ★ **只能发给“这条消息的发送者本人”，必须用私聊。绝对不要发到群聊。**

  这是硬限制（2026-10-18 现场事故）：一个临时助手把本该私聊给用户的
  进度播报发到了群里。在群里发言是**公开可见**的，而你只是一个临时分诊
  助手 —— 你看不到群里的上下文、分不清每句话是回给谁的。

  具体做法：调 output_send__{通道} 时，meta 里用 user_id（那个人的号），
  **不要用 group_id**。即使消息来自群聊，你的回复也走私聊。

  （如果你认为这件事必须在群里回，那就走【报忙碌】：让 主 agent 自己发。
  它有完整上下文和用户身份判定。）

- 不要向用户暴露"我是被临时拉起的助手"这类内部细节，用主 agent 的口吻回复。
- 拿不准属于哪一类时，选【报忙碌】。宁可让用户稍后得到准确答复，
  也不要给出错误的直接回答。`
}

// residentTempPath 计算某个驻留子的 temp 图记忆路径（与既有约定一致）。
func (a *Agent) residentTempPath(id string) string {
	return strings.TrimRight(a.dataDir, "/") + "/residents/" + id + "/graph.db"
}

// syntheticEvent 造一条"内核自己发的"输入事件（用于队列里的转投说明）。
//
// Source 取 kernel：这条消息不是任何用户发来的，日志与用户界面里都应看得出。
// 不带 ResponseCh：没有同步调用方在等它（它只是给主 agent 看的一句说明）。
func (a *Agent) syntheticEvent(text string) *agentIO.InputEvent {
	return &agentIO.InputEvent{
		Source:        "kernel",
		Type:          "text",
		OutputChannel: "kernel",
		Payload:       map[string]interface{}{"content": text},
	}
}

// sortStrings 是一个不引入 sort 依赖的小排序（候选集极小，插入排序足够）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// offloadLoop 周期性检查「主 agent 是否被长任务占住 + 是否有积压」。
//
// 为什么必须是**独立 goroutine**而不是 schedulerLoop 里的一步：
//
//	schedulerLoop 是**同步执行**任务的（executeNewTask 会一直阻塞到任务结束），
//	所以「正忙」期间它根本不会回到循环顶部 —— 把检查放在那里等于永不触发。
//	这正是本特性存在的理由（主 agent 忙时无人处理积压），不能在实现上重犯。
//
// 检查间隔取 BusyAfter 的 1/5（不低于 1 秒）：保证在跨过阈值后能在合理时间内
// 触发，又不至于空转打日志。
func (a *Agent) offloadLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] offloadLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.offloadLoop()
		}
	}()

	if !a.offload.Enabled {
		return // 未启用：不占 goroutine，也不打日志（默认关闭是常态）
	}
	// 只让**根 agent** 做转投：驻留子自己也可能忙，但让子再去拉孙子会形成
	// 无界增殖（每层都能拉 MAX 个），而积压的源头是根那一条调度链。
	if a.parentID != "" {
		return
	}

	interval := a.offload.BusyAfter / 5
	if interval < time.Second {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			a.offloadPendingTasks(a.offload)
		case <-a.ctx.Done():
			return
		}
	}
}
