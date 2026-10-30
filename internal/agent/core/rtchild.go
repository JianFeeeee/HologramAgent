package core

// rtchild（实时子 agent）—— 通话场景。设计见 plan.md 第十一节。
//
// ## 它是什么（用户裁定 2026-10-30）
//
// rtchild 是**第三个子 agent 类型**，与前两者并列、互不影响：
//
//	主 agent    完整内核 *Agent           正常交互
//	驻留子      完整内核 *Agent           长期派驻、不丢活
//	工作式轻量子 goroutine + 局部 msgs    一次性子任务
//	rtchild      goroutine + 局部 msgs    ★ 通话 / 视频通话
//
// 它只服务一个场景：**实时对话**。所以延迟是架构约束而不是调优项 ——
// 这条决定了本文件所有的取舍（详见 plan.md 十一.四 空白 2 的生产实测）。
//
// ## 四条内核契约（plan.md 十一.二）
//
//  1. 输入可打断：新输入到达立即中断当前生成，以新输入重开一轮（不排队等它说完）
//  2. 输出连续：流式、不攒整段；被中断时就地截断
//  3. 不吃队列：不保序、不补偿、无残余回灌
//  4. 待机零消耗：无输入即静默，不烧轮次、不烧 token、不轮询
//
// ## 为什么另起一个文件而不是改 spawn.go / resident.go
//
// 用户裁定 R1：**不影响现有的主 agent 与子 agent**。三者的设计目标函数相反：
//
//	四级中断（scheduler.go）：公平、不饥饿、现场可恢复  ← 驻留子/主 agent 要的
//	rt 单槽（本文件）         ：最新者胜、可饿死、丢现场  ← rt 要的
//
// 硬凑在一起会让两边都坏。所以 rt 输入**不进** registerInterrupt（避开
// preemptCooldown=2s、临界区、同级不抢占三道闸，见 plan.md 十一.三 B）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// ErrAgentNotRealtime 表示目标 agent 不是 rtchild，不能接收 rt 通道。
//
// ★ 为何与 io 包的 ErrChannelNotRealtime 分开：它们是两侧校验的两个不同失败。
// io 包只知道自己那一侧（通道能力）；"目标 agent 是不是 rtchild" 是
// agent 类型，属于 core 包（plan.md 十一.三 F）。分开后错误文案能直接指向
// 该动哪一边，而不是一句笼统的"不是 rt"。
var ErrAgentNotRealtime = errors.New("目标 agent 不是 rtchild")

// ErrRTChannelNotPaired 表示 rt 通道没有**成对**给出（缺 input 或 output）。
//
// rt 通话天然成对：听得见用户（inputch）+ 说得给用户（outputch）。
// 只给一半不报错的话，症状会出现在很远的地方（"它自言自语" / "用户听不到回应"），
// 而那两处都不会指向"少划了一条通道"。
var ErrRTChannelNotPaired = errors.New("rt 通道必须成对（inputch + outputch 都要）")

// AsRealtime 把本 agent 标记为 rtchild（类型标签，plan.md 十一.三 F 的 agent 侧）。
//
// ★ 为什么是"标记"而不是"构造参数才可设"：现有三类 agent 的构造点分散
// （New(AgentConfig) / SpawnResident / runChildTask），rt 的骨架要复用其中一部分。
// 但**校验**不能因此变松：AssignRealtime 会读这个标记，未标记的 agent
// 一律拒划 rt 通道（判据 TestAssignRealtimeRejectsNonRTAgent）。
func (a *Agent) AsRealtime() { a.isRealtime = true }

// IsRealtime 报告本 agent 是否为 rtchild。
func (a *Agent) IsRealtime() bool { return a != nil && a.isRealtime }

// AssignRealtime 把一个 **rt 通道**划给本 agent。
//
// 它是「只有 rtchild 可被划分 rt 通道」（裁定 R2）的唯一执行点。
//
// ## 为什么校验是双向的，以及两个方向各是什么
//
//	① 通道侧：通道必须声明 CapRT（"我能承载 rt 流"）—— 插件在
//	   OutputCapabilities()/InputChannel 里声明。没声明 ⇒ 它根本不知道怎么
//	   处理"新值作废旧值"的语义，划进去只会把它当成普通消息收。
//	② agent 侧：本 agent 必须是 rtchild —— 否则一个跑 3–5 秒工具的主 agent
//	   会混进通话链路，用户听到的是长静默（生产实测中位数 2–5s，见 plan.md）。
//
// ## 为什么不复用 Assign
//
// `ChannelRegistry.Assign` 是**通用**划拨（策略、容量、归属），全部 agent
// 都在用，且它**零校验**。往里加 rt 校验会让普通划拨也变得有条件 ——
// 而计划原文明确：「标签不是准入门槛：**不带 rt 标签的普通通道同样可以
// 划给 rt agent**」。所以 rt 校验只在**绑定 rt 通道**这一条路上生效。
//
// ## 为什么 Capacity 置 0
//
// `InputChannel.Capacity` 是**队列容量**（inputch.go:33）。rt 通道要的是
// **单槽覆盖**（新值作废旧值，见 rtSlot），与"容量=1 的队列"行为相反：
// 后者会让新输入**等旧输入跑完**，正是契约 1 禁止的。置 0 = 不设队列容量。
func (a *Agent) AssignRealtime(name string) error {
	if a == nil {
		return fmt.Errorf("rt 通道划拨：agent 为空")
	}
	if a.io == nil {
		return fmt.Errorf("rt 通道划拨：本 agent 没有 io")
	}
	reg := a.io.ChannelRegistry()
	if reg == nil {
		return fmt.Errorf("rt 通道划拨：通道登记表不可用")
	}
	ch, ok := reg.Lookup(name)
	if !ok {
		return agentIO.ErrInputChannelUnknown
	}
	// ① 通道侧：它自己得声明能承载 rt 流。
	if !ch.Capabilities.Supports(agentIO.CapRT) {
		return fmt.Errorf("rt 通道划拨：%w（通道 %q 的 Capabilities 未含 CapRT）",
			agentIO.ErrChannelNotRealtime, name)
	}
	// ② agent 侧：本 agent 必须是 rtchild。
	if !a.isRealtime {
		return fmt.Errorf("rt 通道划拨：%w（agent %q 不是 rtchild，只有 rtchild 可被划分 rt 通道）",
			ErrAgentNotRealtime, a.id)
	}
	// Capacity=0：rt 通道没有队列容量这个概念（见本函数文档末段）。
	return reg.Assign(name, string(a.id), 0)
}

// ── rtSlot：rt 输入的**唯一**入口（单槽覆盖，不排队） ──
//
// 设计见 plan.md 十一.五。它是契约 1 与契约 3 的落地形态：
//
//	契约 1「新输入一到立刻真打断，不是排队等它说完」
//	契约 3「不吃队列、不保序、不补偿」
//
// ## 为什么不能复用调度器（这是本文件最关键的取舍）
//
// 四级中断那套（scheduler.go）有三个机制与 rt 的语义直接冲突：
//
//	① preemptCooldown = 2s   （scheduler.go:174 / interrupt.go:140）
//	   被打断方 2 秒内不能再被打断。攒批 250ms ⇒ 每秒 4 次拐点 ⇒
//	   **约 75% 的打断会静默降级为入队**。
//	② canPreempt 同级不抢占 （scheduler.go:197）
//	   连续两条同级别输入，第二条排队 —— 连续说话场景直接退化成串行。
//	③ 抢占是 suspend 语义   （interrupt.go:129）
//	   它把被打断的现场**压栈等恢复**；而 rt 要的是**丢弃现场**。
//
// 这三条不是 bug，是「公平、不饥饿、现场可恢复」的实现——正是主 agent 与
// 驻留子需要的。rt 需要的是「最新者胜、可饿死、丢现场」，目标函数相反。
//
// ⇒ 所以 rt 输入**不进** registerInterrupt，走下面这条独立单槽。
// 这也满足用户裁定 R1（不影响现有的主 agent 与子 agent）：本类型一行
// 都没改调度器的行为。

// rtSlot 缓冲 rt 输入。语义：**最新者胜**，旧值作废，永不排队。
//
// 与「容量=1 的队列」的区别（这条写进判据 TestRTSlot_NeverQueues）：
//
//	容量=1 队列：新输入到时旧输入在跑 ⇒ 新输入**等待**（= "等它说完"）❌
//	单槽覆盖  ：新输入到时旧输入在跑 ⇒ 旧输入**作废**，新输入接管   ✅
type rtSlot struct {
	mu      sync.Mutex
	seq     uint64              // 单调递增，随每次 Push 前进（契约 1 的可验证凭据）
	pending *agentIO.InputEvent // nil = 无待处理；非 nil 时恒为**最新**一条
	// dropped 统计被覆盖掉的输入条数，用于观察「新输入赢了几次旧输入」。
	//
	// 它必须可读：rt 场景下打断是常态，但**如果一条都没被丢弃**，
	// 说明插件的攒批没有真正生效（或速率太低），值得在阶段二看清楚。
	dropped uint64
	wake    chan struct{} // cap=1，非阻塞信号；不承载数据
}

func newRTSlot() *rtSlot {
	return &rtSlot{wake: make(chan struct{}, 1)}
}

// Push 覆盖写入一条输入，返回本次的 seq。
//
// 旧值**直接丢弃**（不是排队、不是补偿）—— 这正是契约 1/3 的实现。
func (s *rtSlot) Push(evt *agentIO.InputEvent) uint64 {
	s.mu.Lock()
	if s.pending != nil {
		s.dropped++ // 上一条还没被取走就被新值覆盖：这就是"作废"
	}
	s.seq++
	s.pending = evt
	seq := s.seq
	s.mu.Unlock()

	// 非阻塞唤醒：槽里已有信号时不必重复（唤醒是电平语义，不是计数语义）。
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return seq
}

// Take 取走当前待处理输入并**清空槽**。没有待处理时返回 (nil, 0, false)。
//
// 清空是必须的：取走后若不清，rtLoop 会反复拿到同一条输入重跑同一轮。
func (s *rtSlot) Take() (*agentIO.InputEvent, uint64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending == nil {
		return nil, 0, false
	}
	evt, seq := s.pending, s.seq
	s.pending = nil
	return evt, seq, true
}

// Seq 返回当前 seq（诊断/判据用；不代表"有待处理"）。
func (s *rtSlot) Seq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// Dropped 返回被覆盖丢弃的输入条数（诊断用）。
func (s *rtSlot) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Pending 报告槽里是否有待处理输入（诊断用；**不要**用它做轮询等待）。
//
// ★ 契约 4 禁止轮询：rtLoop 必须 block 在 Wake() 上，而不是循环查 Pending()。
func (s *rtSlot) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending != nil
}

// Wake 返回阻塞等待新输入的通道（契约 4：待机必须 block，不得轮询）。
//
// ★ 为什么不暴露 Pending() 让调用方轮询：轮询会烧 CPU 且在静默期产生
// 无意义的唤醒（"零轮次零 token" 之外还要求"不轮询"）。rtLoop 的正确形状是
//
//	for {
//	    select {
//	    case <-a.rtSlot.Wake():
//	        evt, seq, ok := a.rtSlot.Take()
//	        if !ok { continue }   // 信号先于数据或数据已被取走：无副作用
//	        ...跑一轮...
//	    case <-a.ctx.Done():
//	        return
//	    }
//	}
func (s *rtSlot) Wake() <-chan struct{} { return s.wake }

// ── R3：rtChild 生命周期 · 独立 ctx · rtLoop ──
//
// ## 为什么每个 rtchild 必须有**自己的** ctx（结构性前提）
//
// 现状 `spawn.go:184` 用 `a.provider.Chat(a.ctx, req)` —— 那是**父 agent 的**
// ctx。对一个 rtchild 调 cancel(a.ctx) 会把父与**全部**子一起停掉。
//
// 用户裁定 R1「不影响现有的主 agent 与子 agent」在代码层面就是这个要求：
//
//	rtA 被新输入打断 ⇒ 只作废 rtA 的当前轮
//	                ⇒ rtB 与主 agent 的 LLM 调用必须**继续**
//
// 判据 TestRTChild_InterruptDoesNotAffectOthers 钉住它。

// rtChild 是一个 rtchild 实例（通话场景的实时子 agent）。
//
// 它**不进** `a.residents`（那是驻留子的登记表，语义是"有任务队列、有残余"），
// 而是自己一张轻表（见 Agent.rtChildren）。
type rtChild struct {
	id     string
	agent  *Agent  // 跑 rt 轮次的载体（已标记 isRealtime）
	slot   *rtSlot // 唯一输入入口：单槽覆盖
	input  string  // inputch(rt) 的名字（采集侧灌入；成对的一半）
	output string  // outputch(rt) 的设备名（内核把文本写进去）

	// round 是本轮序号（单调递增），seq 是本轮所基于的输入 seq。
	//
	// ★ seq 是**可验证凭据**（plan.md 十一.四 空白 3）：输出回传它，
	// 判据才能证明"这一轮是用最新输入跑的"，而不是旧输入的残余。
	mu      sync.Mutex
	round   uint64
	lastSeq uint64

	// curCancel 是**当前这一轮**的 cancel（PushRTInput 用它打断，见 rtStream）。
	// nil = 没有正在跑的轮次。
	curCancel context.CancelFunc
	// critical > 0 表示正在执行工具（不可打断，R5）。
	// 用计数而非 bool：一轮里可能串行执行多个工具。
	critical int
	// suppressedCancels 统计「因处于工具执行期而**故意没取消**」的次数。
	//
	// ★ 为什么需要这个计数器（否则 R6 的守卫是**不可验证**的）：
	// 设备工具执行经 `a.io.ExecuteTool`（toolcall.go:377），它内部传的是
	// `context.Background()` —— 工具**根本收不到** roundCtx。于是"工具不被
	// 中途掐断"这件事在两条路径上都成立，判据无法区分：
	//   · 守卫生效（我们主动不取消）
	//   · 守卫失效但取消也没效果（工具不消费 ctx）
	// 这个计数器把"守卫真的被执行了"变成可观测事实（判据
	// TestRTChild_CriticalSectionSuppressesCancel 钉它）。
	suppressedCancels uint64

	// history 是跨轮累积的对话消息（普通上下文，plan.md 十一.四 修订后）。
	//
	// 它**就是** rtchild 的全部上下文状态：一条通话是一条连续对话，
	// 每轮把「本轮 user 输入 + 模型产出 + 工具结果」追加进去，
	// 进模型前按窗口裁（见 buildRTContext / rtTrimToWindow）。
	//
	// 与另外两类子 agent 的差别只在**生命周期**：
	//   普通子   —— 局部 msgs，跑完即弃
	//   rtchild  —— 同一份 msgs 跨轮保留（通话不能每轮失忆）
	//
	// 由 rtLoop 单 goroutine 串行改，锁只用于让外部读取（stats/判据）安全。
	history []agentAPI.Message
	// stopped 标记本实例已被 Stop（避免 Stop 后再启 loop）。
	stopped bool
}

// RTChildOptions 是创建一个 rtchild 的参数。
type RTChildOptions struct {
	// ID 是实例 id（登记表的键）。
	ID string
	// Input 是 inputch(rt) 的名字——采集侧往里灌实时事件。
	//
	// ## 为什么必须与 Output **成对**给出
	//
	// rt 通道是**成对**的资源：一条通话同时需要"听得见用户"（input）与
	// "说得给用户"（output）。只给一半的形态不是"简化"，是**坏掉一半**：
	//
	//	只给 Output：rtchild 能说话，但**永远听不到用户**（rtSlot 无人灌入）
	//	           ⇒ 表现是"它自言自语，然后静默待机"，且不报任何错
	//	只给 Input ：rtchild 能听，但**输出无处可去**（rtEmit 找不到设备）
	//	           ⇒ 用户听到的是长时间静默
	//
	// 所以 SpawnRTChild 要求两者都给，且**两者都必须是 CapRT 通道**。
	Input string
	// Output 是 outputch(rt) 的设备名——内核把 rt 文本写进它。
	//
	// ★ 内核只产出**文本**（用户裁定 R6）：怎么展示（含 TTS/播放）是插件的活。
	// 内核不做音频合成，也不关心这个设备是不是 CapAudio。
	Output string
	// SystemPrompt 是 rtchild 的系统提示词。空则用 rtChildDefaultPrompt。
	//
	// 调用方（阶段三）通常传 `childSystemPromptTemplate` + 十一.六 的【实时通话】段。
	SystemPrompt string
	// PreferredProvider 是**这个 rtchild** 优先使用的模型源（空 = 继承父的）。
	//
	// 通话场景应选**首字延迟最低**的源：生产实测不同源的单次往返差 2–5 倍
	// （见 plan.md 十一.四 空白 2），而 rt 的首字预算是 500ms 级。
	PreferredProvider string
	// MaxTools 是本轮允许的最大工具批数（0 = 不限）。
	//
	// 与 `spawn_child` 的 max_turns 同义，但默认值必须是 0（不限）：
	// rt 是**常驻对话**而非一次性任务，跑满就结束是错的。
	MaxTools int
}

// rtChildDefaultPrompt 是 rtchild 的默认系统提示词。
//
// 【实时通话】段是用户裁定 R4 的落地：调用工具**之前**先用 outputch 告知用户稍等。
// 这是**提示词**要求而非内核强制——内核无法判断"这次工具调用是否真的慢"，
// 强行插入会破坏模型的自然表达（plan.md 十一.三 G）。
const rtChildDefaultPrompt = `你正在与用户实时通话。

【实时通话】这是硬性要求：
- 调用任何工具之前，**必须先用输出把这句话说出来**告知用户你要做什么
  （例如「稍等，我查一下」）。工具执行期间用户无法打断你，这句话是他唯一的反馈。
- 你不需要、也不应该追求把话说完整：被打断是常态，不是失败。
- 不要复述用户刚说的话，直接给结论。

【执行纪律】必须实际调用工具去执行，不要只回复「我将要做什么」。
只有已经取得实际结果（或确认无法取得）时，才用纯文本汇报。`

// RTChildren 返回全部 rtchild 的 id（按 id 排序，便于断言与展示稳定）。
func (a *Agent) RTChildren() []string {
	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	out := make([]string, 0, len(a.rtChildren))
	for id := range a.rtChildren {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// SpawnRTChild 创建一个 rtchild 并启动它的 rtLoop。
//
// 与 SpawnResident 的关键差别（这是"三类并列、不复用外壳"的落点）：
//
//	                        驻留子                     rtchild
//	载体                    完整 *Agent（含调度器）     同（但**不启动**调度循环）
//	goroutine 数            Start() 起 7 个           只起 1 个 rtLoop
//	输入                    通道 + 队列（不丢活）       单槽覆盖（最新者胜）
//	残余回灌                ResidualPolicy keep/drop   无（契约 3）
//	上下文                  RelevanceContext + 不裁    局部消息页（plan.md 十一.四）
func (a *Agent) SpawnRTChild(opts RTChildOptions) (string, error) {
	if strings.TrimSpace(opts.ID) == "" {
		return "", fmt.Errorf("rtchild 必须有 id")
	}
	if strings.TrimSpace(opts.Output) == "" {
		return "", fmt.Errorf("rtchild 必须指定 outputch（内核把 rt 文本写进它）")
	}
	if strings.TrimSpace(opts.Input) == "" {
		// rt 通道必须**成对**：没有 input，rtchild 永远听不到用户
		// （rtSlot 无人灌入），表现是"自言自语后静默待机"且不报错。
		return "", fmt.Errorf("rtchild 必须指定 inputch（rt 通道必须成对：%w）", ErrRTChannelNotPaired)
	}
	// ★ 划拨前先确认目标通道**不属于本 agent**（主 agent 没有 rt 能力）。
	//
	// 用户裁定：主 agent 自己**不持有** rt 能力。所以：
	//   · 主 agent 不可能自己去消费 rt 通道（它没有 rtSlot）
	//   · 通道一旦划给 rtchild，Owner 就是 rtchild；rtchild 销毁后
	//     通道**悬空**（Owner 保持指向已销毁的 id），而不是回落到主 agent
	//
	// 这里只做"不能划给自己"的防御：若 Owner 已经是本 agent 自己，
	// 说明这个通道被主 agent 当普通通道占着，划走会静默改变它的路由语义。
	if reg := a.io.ChannelRegistry(); reg != nil {
		if ch, ok := reg.Lookup(opts.Input); ok && ch.Owner != "" && ch.Owner == string(a.id) {
			return "", fmt.Errorf("rt 通道划拨：inputch %q 归属于主 agent 自身，"+
				"主 agent 不持有 rt 能力，不能作为 rt 通道的持有方", opts.Input)
		}
		if ch, ok := reg.Lookup(opts.Output); ok && ch.Owner != "" && ch.Owner == string(a.id) {
			return "", fmt.Errorf("rt 通道划拨：outputch %q 归属于主 agent 自身，"+
				"主 agent 不持有 rt 能力，不能作为 rt 通道的持有方", opts.Output)
		}
	}

	a.rtMu.Lock()
	if a.rtChildren == nil {
		a.rtChildren = map[string]*rtChild{}
	}
	if _, dup := a.rtChildren[opts.ID]; dup {
		a.rtMu.Unlock()
		return "", fmt.Errorf("rtchild %s 已存在", opts.ID)
	}
	a.rtMu.Unlock()

	// ① 独立 ctx：**必须**从父的 ctx 派生（父停则子停，不留孤儿），
	//    但**单独可取消**（打断 rtA 不波及父与 rtB）。见上面的注释。
	ctx, cancel := context.WithCancel(a.ctx)

	// ② 独立的 IOManager：与驻留子同款——自己的输入入口 + 共享通道登记表
	//    + 父视图回退（否则 output_send__* 与设备工具全都不可见）。
	childIO := agentIO.NewIOManager()
	if reg := a.io.ChannelRegistry(); reg != nil {
		childIO.SetChannelRegistry(reg)
	}
	childIO.SetParentIO(a.io)

	sysPrompt := opts.SystemPrompt
	if strings.TrimSpace(sysPrompt) == "" {
		sysPrompt = rtChildDefaultPrompt
	}

	carrier := New(AgentConfig{
		// ★ 必须**逐字等于**登记表 key（opts.ID）。
		//
		// 曾经写成 "rt/"+opts.ID：于是 AssignRealtime 把通道 Owner 记成 "rt/xxx"，
		// 而 routeInputByOwner 按 entry.Owner 去 a.rtChildren 里查（key 是 "xxx"）
		// ⇒ **永远查不到** ⇒ 插件的 rt 输入被兜底给主 agent。
		// 这是"两个地方各拼一次名字"的典型故障（与 resident 的 inboundChannelName 同型）。
		ID:              types.AgentID(opts.ID),
		SystemPrompt:    sysPrompt,
		Provider:        a.provider,
		ProviderManager: a.providerManager,
		// rt 可以有自己更快的源；留空继承父的（父再留空 ⇒ 总默认源）。
		//
		// ⚠️ 注意 rt 仍走 resolveProviders 的回退链（只是把优先源提前）：
		// 硬锁定会让"首选源抖动"直接变成"通话中断"。
		PreferredProvider: firstNonEmpty(opts.PreferredProvider, a.preferredProvider),
		IO:                childIO,
		StageHost:         a.stageHost,
		AllowedOutputs:    []string{opts.Output},
		KernelSource:      string(a.id),
		ParentID:          string(a.id),
		IsRealtime:        true, // ★ 类型标签：只有它可被划 rt 通道
	})
	// ★ 不调 carrier.Start()：那会起 schedulerLoop / interceptLoop / 3 个记忆
	//   整理循环。rt 要的是"不吃队列、无轮询"，这些 goroutine 对它是**反向**的。
	//   只保留一个 rtLoop。
	carrier.ctx, carrier.cancel = ctx, cancel
	carrier.rtSlot = newRTSlot()
	carrier.rtMaxTools = opts.MaxTools

	// ★ 接线 notify_parent —— 这就是计划 §2.4 的 `askparent` 在内核里的形态。
	//
	// ## 为什么必须接线（真实缺陷，本轮抓到）
	//
	// `notify_parent` 出现在子侧工具表里（tooldefs.go:874，条件是 parentID != ""），
	// 而 rtchild 的 carrier 也满足该条件 ⇒ **模型看得见这个工具**。
	// 但初版没接 notifyParent 回调 ⇒ 模型一调就得到
	// 「本 agent 没有上级，无法发送消息」——工具存在、调用必失败。
	// 判据 TestRTChild_NotifyParentIsWired 钉它。
	//
	// ## 为什么用 L4 而不是驻留子的 L3
	//
	// 驻留子的"主动汇报"是进度通知，L3 足够（它不该打断父正在做的事）。
	// rt 的通话里"向父发问"是**用户的话**——父必须立刻看到，否则用户对着
	// 电话说话、对面（父 agent）却在忙别的。所以用 L4。
	//
	// ## 不阻塞 rt 流（计划 §2.4）
	//
	// 投递是**异步**的：InjectInterruptTextOpts 只登记，不等待。
	// rt 轮次继续跑，不被这次发问卡住。父的回话（若有）按 R5 语义
	// **作为一条新的用户输入**注入（走同一个单槽，见 PushRTInput），
	// 因此它可能打断 rt 当前的轮次 —— 这正是 R5 要的行为，不是缺陷。
	//
	// ★ 入站通道**只登记一次**（在这里），回调里只投递。
	//   初版在回调里调 residentInboundChannel（它内部会 Register）⇒ 每发一次
	//   就重复登记一次，而该表按 name 全局唯一 —— 是纯粹的写放大 + 脏登记。
	rtInboundCh := a.residentInboundChannel("rt/" + opts.ID)
	carrier.notifyParent = func(text string) {
		a.io.InjectInterruptTextOpts("rt/"+opts.ID, rtInboundCh, text,
			agentIO.InjectOptions{Priority: "L4"})
	}

	rc := &rtChild{
		id: opts.ID, agent: carrier, slot: carrier.rtSlot,
		input: opts.Input, output: opts.Output,
	}

	// ★ 成对划拨 rt 通道（这是"主 agent 把 rt 通道划给 rtchild"的**唯一**落点）。
	//
	// 两侧都过 AssignRealtime —— 它校验：
	//   ① 通道声明了 CapRT（"我能承载 rt 流"）
	//   ② 目标 agent 是 rtchild（isRealtime）
	// 任一不满足就拒划，且**拒绝路径无副作用**（不写 Owner）。
	//
	// 顺序：先 Input 后 Output。失败时把已成的一半回滚，否则会留下
	// "input 划上了、output 没划上"的半残状态 —— 那正是本函数要防的形态。
	// 划拨前先记下原 Owner：失败回滚时**恢复原状**，而不是"归还为未分配"。
	//
	// 为什么不能归还为 ""：未分配意味着 routeInputByOwner 会把它当"没有归属"
	// 兜底给主 agent —— 而主 agent **不持有 rt 能力**（用户裁定），
	// 那等于把一条 rt 通道静默变成主 agent 的普通通道。
	prevOwner := map[string]string{}
	if reg := a.io.ChannelRegistry(); reg != nil {
		for _, n := range []string{opts.Input, opts.Output} {
			if ch, ok := reg.Lookup(n); ok {
				prevOwner[n] = ch.Owner
			}
		}
	}
	rollback := func() {
		reg := a.io.ChannelRegistry()
		if reg == nil {
			return
		}
		for n, owner := range prevOwner {
			_ = reg.Assign(n, owner, 0)
		}
	}

	if err := carrier.AssignRealtime(opts.Input); err != nil {
		cancel()
		rollback()
		return "", fmt.Errorf("划入 inputch(rt) %q: %w", opts.Input, err)
	}
	if err := carrier.AssignRealtime(opts.Output); err != nil {
		cancel()
		rollback() // 把已经划上的 Input 也恢复回去，别留半残
		return "", fmt.Errorf("划入 outputch(rt) %q: %w", opts.Output, err)
	}

	a.rtMu.Lock()
	a.rtChildren[opts.ID] = rc
	a.rtMu.Unlock()

	go carrier.rtLoop(rc)

	log.Printf("[rtchild] %s started (output=%s)", opts.ID, opts.Output)
	return opts.ID, nil
}

// StopRTChild 停止一个 rtchild 并清出登记表。
//
// 契约 3：**无残余回灌** —— 槽里未处理的那条输入直接丢弃（它是过期状态，
// 回灌给父只会污染主管线，见计划 §七「插件那边的当前态默认不转交」）。
func (a *Agent) StopRTChild(id string) error {
	a.rtMu.Lock()
	rc, ok := a.rtChildren[id]
	if ok {
		delete(a.rtChildren, id)
	}
	a.rtMu.Unlock()
	if !ok {
		return fmt.Errorf("rtchild %s 不存在", id)
	}
	rc.mu.Lock()
	rc.stopped = true
	rc.mu.Unlock()

	// 取消自己的 ctx ⇒ rtLoop 在下一个安全点退出、当前 ChatStream 被截断。
	rc.agent.cancel()
	// 契约 3：无残余。清空槽（丢弃未处理输入，不转交）。
	_, _, _ = rc.slot.Take()
	if a.io != nil {
		if reg := a.io.ChannelRegistry(); reg != nil {
			// ★ rt 通道**销毁后悬空**（用户裁定 2026-10-30），**不归还为空**。
			//
			// 为什么不归还：
			//   · 主 agent **不持有** rt 能力 ⇒ 把它"归还为未分配"等于让一个
			//     没有 rtSlot 的 agent 成为潜在持有方（routeInputByOwner 的
			//     Owner=="" 分支会把输入兜底给主 agent，静默改变语义）
			//   · 悬空（Owner 仍指向已销毁的 id）是**显式**状态：路由查不到
			//     该 owner ⇒ 走既有的"owner 无匹配 agent"兜底 + 留日志，
			//     运维能一眼看出"这条 rt 通道的宿主没了"
			//
			// 换言之：悬空 = 可用日志诊断的坏状态；归还 = 静默改路由的坏状态。
			// 前者可查，后者不可查。
			//
			// 只注销入站登记（那是本内核自己建的 child/<id>，不是插件声明的 rt 通道）。
			reg.Unregister(inboundChannelName("rt/" + id))
		}
	}
	log.Printf("[rtchild] %s stopped", id)
	return nil
}

// StopRTChildren 停止全部 rtchild（父退出时必须；不留孤儿）。
func (a *Agent) StopRTChildren() int {
	a.rtMu.Lock()
	all := make([]*rtChild, 0, len(a.rtChildren))
	for _, rc := range a.rtChildren {
		all = append(all, rc)
	}
	a.rtChildren = map[string]*rtChild{}
	a.rtMu.Unlock()
	for _, rc := range all {
		rc.mu.Lock()
		rc.stopped = true
		rc.mu.Unlock()
		rc.agent.cancel()
		_, _, _ = rc.slot.Take()
	}
	if len(all) > 0 {
		log.Printf("[rtchild] stopped all %d instance(s)", len(all))
	}
	return len(all)
}

// PushRTInput 投递一条 rt 输入（**覆盖式**，返回本次 seq）。
//
// 它是 rt 的唯一输入入口。与 `InjectInterruptTextOpts` 的关键差别：
// 后者进四级中断阶梯（会被 preemptCooldown=2s 降级为入队），
// 本函数直接覆盖单槽（最新者胜，永不排队）。见 plan.md 十一.三 B。
func (a *Agent) PushRTInput(id, text string) (uint64, error) {
	a.rtMu.Lock()
	rc := a.rtChildren[id]
	a.rtMu.Unlock()
	if rc == nil {
		return 0, fmt.Errorf("rtchild %s 不存在", id)
	}
	evt := &agentIO.InputEvent{
		Source:        a.residentParentSource(),
		Type:          "text",
		Payload:       map[string]interface{}{"content": text},
		OutputChannel: rc.output,
	}
	seq := rc.slot.Push(evt)

	// ★ 契约 1 的**执行点**：新输入一到就取消当前轮。
	//
	// 只写槽不取消是错的（初版就是这么写的，判据 TestRTChild_NewInputWinsWithoutRetry
	// 当场抓住）：rtLoop 正 block 在 rtRunOnce 的流里，根本不会回到循环顶部
	// 取新值 —— 表现就是“等它说完”，正是契约 1 明令禁止的。
	//
	// ★ 但**工具执行期间不取消**（用户裁定 R5）：副作用不可回滚，
	// 中途掐断会让“决定执行但未执行”变成尴尬状态。此时只写槽，
	// 工具跑完后下一轮自然用新输入。
	rc.mu.Lock()
	cancel, critical := rc.curCancel, rc.critical
	rc.mu.Unlock()
	if cancel != nil {
		if critical > 0 {
			// 工具执行期：**故意不取消**（R5）。记一笔，让守卫可验证。
			rc.mu.Lock()
			rc.suppressedCancels++
			rc.mu.Unlock()
		} else {
			cancel()
		}
	}
	return seq, nil
}

// StatsRTChild 返回一个 rtchild 的只读状态（诊断/判据用）。
type RTChildStats struct {
	ID      string `json:"id"`
	Rounds  uint64 `json:"rounds"`
	LastSeq uint64 `json:"last_seq"`
	Dropped uint64 `json:"dropped"`
	Pending bool   `json:"pending"`
	// SuppressedCancels 是因工具执行期而**故意未取消**的次数（R6 守卫的可观测凭据）。
	SuppressedCancels uint64 `json:"suppressed_cancels"`
}

// RTChildStats 返回该实例的状态快照。
func (a *Agent) RTChildStats(id string) (RTChildStats, bool) {
	a.rtMu.Lock()
	rc := a.rtChildren[id]
	a.rtMu.Unlock()
	if rc == nil {
		return RTChildStats{}, false
	}
	rc.mu.Lock()
	rounds, lastSeq, suppressed := rc.round, rc.lastSeq, rc.suppressedCancels
	rc.mu.Unlock()
	return RTChildStats{
		ID: id, Rounds: rounds, LastSeq: lastSeq,
		Dropped: rc.slot.Dropped(), Pending: rc.slot.Pending(),
		SuppressedCancels: suppressed,
	}, true
}

// ── rtLoop：rtchild 的执行循环（R3–R6 的载体） ──
//
// 形状由契约决定（plan.md 十一.二）：
//
//	for {
//	    block on Wake                      ← 契约 4：待机零消耗，不轮询
//	    Take 最新输入（单槽，清空）          ← 契约 3：不吃队列
//	    跑一轮：
//	        ChatStream（**流式**，不是 Chat）← 契约 2：不攒整段
//	        每个 chunk 立刻写 outputch(rt)   ← 契约 2：连续输出
//	        工具调用期间不打断                 ← R5「除 toolcall 期间都可打断」
//	    循环中被新输入打断 ⇒本轮的 ChatStream 被 cancel ⇒就地截断
//	}
//
// ## 为什么不用 step 状态机（task.go 的 runTaskSteps）
//
// 主 agent 的 runTaskSteps 承载 7 个 Step + 挂起/恢复/中断栈，那是为
// 「可抢占、现场可恢复」设计的。rt 的规则更窄（plan.md 十一.三 A）：
//
//	ChatStream 期间     ⇒ 可打断（cancel 自己的 ctx）
//	工具执行期间       ⇒ 不可打断（临界区，与 StepToolExec 同义）
//
// 这里用**明确的临界区标记**实现后一条（rtBusy 计数），不用状态机：
// 少一层抽象，且"当前能不能打断"这个语义在代码里一眼可见。
func (a *Agent) rtLoop(rc *rtChild) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[rtchild] %s loop panic recovered: %v\n%s", rc.id, r, debug.Stack())
		}
	}()

	for {
		select {
		case <-a.ctx.Done(): // 父退出 或 本实例被 Stop
			return
		case <-rc.slot.Wake():
		}

		evt, seq, ok := rc.slot.Take()
		if !ok {
			continue // 信号先于数据，或已被取走：无副作用
		}
		a.rtRunOnce(rc, evt, seq)
	}
}

// rtRunOnce 跑一轮：流式生成 + 可选工具，任一轮次都受"新输入覆盖"影响。
func (a *Agent) rtRunOnce(rc *rtChild, evt *agentIO.InputEvent, seq uint64) {
	input, _ := evt.Payload["content"].(string)
	if strings.TrimSpace(input) == "" {
		return
	}

	rc.mu.Lock()
	rc.round++
	round := rc.round
	rc.lastSeq = seq
	rc.mu.Unlock()

	// 上下文 = 跨轮累积的历史 + 本轮输入，按窗口裁（见 buildRTContext）。
	//
	// history 是 rtChild.history 的**拷贝**：本轮跑的时候（可能几十秒）里，
	// 别的轮次不会并发改它（rtLoop 单 goroutine 串行），但拷贝能避免
	// append 时共享底层数组导致的意外覆盖。
	rc.mu.Lock()
	history := append([]agentAPI.Message(nil), rc.history...)
	rc.mu.Unlock()
	msgs := a.buildRTContext(rc, history, input)
	tools := a.buildToolDefs()
	// 裁掉 output_send__*：rtchild 的输出走 outputch(rt)，不走主动输出门
	// （那张门会要求模型显式点名通道，与"流式连续输出"是两套语义）。
	tools = rtStripOutputTools(tools)

	// ★ roundCtx 作用域是**整轮**（含工具执行），不是单次 LLM 调用。
	//
	// 这是 R5/R6 边界能成立的**结构前提**，也是我第一版写错的地方：
	// 初版把 cancel 装在 rtStream 内部、退出时清空 ⇒ 工具执行期间
	// rc.curCancel 已经是 nil ⇒ PushRTInput 里的 `critical == 0` 判断是**死代码**，
	// 判据只是碰巧通过（工具没被打断是因为"没人取消"，不是因为"守卫生效"）。
	//
	// 现在：cancel 挂在整轮上、只有轮次结束才清 ⇒ 工具期间 curCancel 仍非 nil，
	// 于是 critical 守卫**真的**在决定"这次取消要不要发生"。
	// 这一条已被变异验证：去掉 critical 判断后 TestRTChild_NotInterruptibleDuringToolExec 变红。
	roundCtx, roundCancel := context.WithCancel(a.ctx)
	defer roundCancel()
	rc.mu.Lock()
	rc.curCancel = roundCancel
	rc.mu.Unlock()
	defer func() {
		rc.mu.Lock()
		rc.curCancel = nil
		rc.mu.Unlock()
	}()

	turns := 0
	for {
		// 每轮进 LLM 前看一看：若这期间已有更新的输入进了槽，本轮就不必再跑了
		// （新的一轮马上会用那条输入重开）。这是契约 1 在循环内的落点。
		if a.rtSuperseded(rc, seq) {
			log.Printf("[rtchild] %s round %d superseded by newer input before LLM", rc.id, round)
			return
		}

		resp, ok := a.rtStream(roundCtx, rc, round, msgs, tools)
		if !ok {
			// 被打断或被父停：就地截断，不重试（契约 1/2）。
			//
			// ★ 已产出的部分**写回 history**：它确实说出口了（用户听到了），
			// 下一轮不该假装没发生过 —— 否则模型会重复同一句话。
			a.rtAppendHistory(rc, agentAPI.Message{Role: "assistant", Content: respContentOr(resp)})
			return
		}

		if len(resp.ToolCalls) == 0 {
			// 本轮正常说完：把 user 输入与模型产出都写进 history。
			a.rtAppendHistory(rc,
				agentAPI.Message{Role: "user", Content: input},
				agentAPI.Message{Role: "assistant", Content: resp.Content})
			return
		}
		// 工具批数上限（0 = 不限）。rt 是常驻对话，默认不限；
		// 给了上限是为了让调用方能约束"一次插话最多查几步"。
		if a.rtMaxTools > 0 && turns >= a.rtMaxTools {
			log.Printf("[rtchild] %s round %d reached max tool turns %d", rc.id, round, a.rtMaxTools)
			return
		}
		turns++

		batchStart := len(msgs)
		msgs = append(msgs, agentAPI.Message{
			Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls,
		})
		for _, tc := range resp.ToolCalls {
			// ★ 工具执行期间**不可打断**（R5 / plan.md 十一.三 A 的临界区）：
			// 副作用不可回滚，打断只会让"决定执行但未执行"变成尴尬状态。
			a.rtEnterCritical(rc)
			out := a.executeToolCallOutcome(tc, rc.output)
			a.rtExitCritical(rc)

			log.Printf("[rtchild] %s round %d tool %s -> %d bytes",
				rc.id, round, tc.Name, len(out.Text))
			msgs = append(msgs, agentAPI.Message{
				Role: "tool", ToolCallID: tc.ID, Content: out.Text,
			})
		}
		// 这一批（assistant(tool_calls) + 配对 tool 结果）是**不可分割的一组**，
		// 整组写回 history：切在中间上游会直接 400（见 rtTrimToWindow 注释）。
		a.rtAppendHistory(rc, msgs[batchStart:]...)
	}
}

// rtAppendHistory 把若干条消息追加进跨轮历史（唯一写入点）。
func (a *Agent) rtAppendHistory(rc *rtChild, msgs ...agentAPI.Message) {
	if len(msgs) == 0 {
		return
	}
	rc.mu.Lock()
	rc.history = append(rc.history, msgs...)
	rc.mu.Unlock()
}

// respContentOr 取响应的正文（resp 可能为 nil）。
func respContentOr(resp *agentAPI.CompletionResponse) string {
	if resp == nil {
		return ""
	}
	return resp.Content
}

// rtStream 跑一次**流式**生成，把每个增量立刻写给 outputch(rt)。
//
// 返回 ok=false 表示被打断（ctx 被 cancel）——调用方必须**就地截断、不重试**
// （契约 2；也对应 plan.md 十一.三 C 的"ChatStream 被 cancel 即截断"）。
func (a *Agent) rtStream(ctx context.Context, rc *rtChild, round uint64, msgs []agentAPI.Message, tools []interface{}) (*agentAPI.CompletionResponse, bool) {
	req := &agentAPI.CompletionRequest{
		Messages:        msgs,
		MaxTokens:       4096,
		Tools:           tools,
		ToolChoice:      "auto",
		DisableThinking: true, // rt 不等思考链：首字延迟是硬指标
	}

	// ctx 由调用方（rtRunOnce）按**整轮**提供，本函数不再自己派生：
	// 否则工具执行期间 curCancel 已被清空，R6 的临界区守卫会变成死代码
	// （见 rtRunOnce 里 roundCtx 的注释）。
	ch, err := a.provider.ChatStream(ctx, req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, false
		}
		log.Printf("[rtchild] %s round %d stream connect failed: %v", rc.id, round, err)
		return nil, false
	}

	resp := &agentAPI.CompletionResponse{}
	var acc []*agentAPI.ToolCall
	for {
		select {
		case <-ctx.Done():
			return nil, false // 被打断：就地截断
		case chunk, ok := <-ch:
			if !ok {
				resp.ToolCalls = rtFlushToolCalls(acc)
				return resp, true
			}
			if chunk.Content != "" {
				// 契约 2：**每个增量立刻外发**，不攒整段。
				// 只给文本（用户裁定 R6）：怎么展示是插件的活。
				a.rtEmit(rc, chunk.Content)
				resp.Content += chunk.Content
			}
			if chunk.ToolCall != nil {
				acc = append(acc, chunk.ToolCall)
			}
			for i := range chunk.ToolCalls {
				acc = append(acc, &chunk.ToolCalls[i])
			}
			if chunk.Done {
				resp.ToolCalls = rtFlushToolCalls(acc)
				return resp, true
			}
		}
	}
}

// rtFlushToolCalls 把累积的 tool call 过滤掉 nil。
func rtFlushToolCalls(acc []*agentAPI.ToolCall) []agentAPI.ToolCall {
	out := make([]agentAPI.ToolCall, 0, len(acc))
	for _, tc := range acc {
		if tc != nil && tc.Name != "" {
			out = append(out, *tc)
		}
	}
	return out
}

// rtEmit 把一个文本增量写进 outputch(rt)。
//
// ★ 内核只产出**文本**（R6）。这里不判断设备是否 CapAudio，也不做 TTS：
// 插件拿到 text 后怎么呈现（文字/语音/字幕）完全是它的事。
func (a *Agent) rtEmit(rc *rtChild, text string) {
	if text == "" {
		return
	}
	if a.io == nil {
		return
	}
	dev := a.io.GetDevice(rc.output)
	if dev == nil {
		return
	}
	if _, err := dev.Execute(rc.output, map[string]interface{}{"text": text}); err != nil {
		log.Printf("[rtchild] %s emit to %s failed: %v", rc.id, rc.output, err)
	}
}

// rtEnterCritical / rtExitCritical 标记"正处于不可打断的临界区"（工具执行）。
//
// 用计数而非 bool：一轮里可能串行执行多个工具，用 bool 会在第二个工具
// 开始时就把标记误清。这与主 agent 的 StepToolExec 是同一个语义
// （task.go:56「副作用不可回滚，执行中不是安全点」）。
func (a *Agent) rtEnterCritical(rc *rtChild) {
	rc.mu.Lock()
	rc.critical++
	rc.mu.Unlock()
}

func (a *Agent) rtExitCritical(rc *rtChild) {
	rc.mu.Lock()
	if rc.critical > 0 {
		rc.critical--
	}
	rc.mu.Unlock()
}

// rtSuperseded 报告"槽里已有更新的输入"（本轮已过期，不必再跑）。
func (a *Agent) rtSuperseded(rc *rtChild, seq uint64) bool {
	return rc.slot.Seq() > seq
}

// buildRTContext 装配 rtchild 的上下文：**普通上下文 + 上下文窗口**。
//
// ## 就是普通对话历史，没有别的结构
//
// 用户 2026-10-30 裁定：rtchild 的上下文模型 = **普通上下文 + 上下文窗口**。
// 所以这里没有"页"、没有"锚点"、没有"自降级阶梯" —— 那些是我先前把问题
// 设计复杂了（plan.md 十一.四 空白 1 的初版）。rt 的上下文就是：
//
//	system 提示词 + 正常累积的消息序列（user / assistant / tool）
//
// 与 `runChildTask`（普通子）的局部 `msgs` 完全同构 —— 区别只在于 rtchild
// 跨轮保留它（一次通话是一条连续对话），且每轮进模型前按窗口裁一次。
//
// ## 唯一的机制：窗口裁剪
//
// 裁剪单位是**消息**，但 tool_call 与其 tool 结果**必须成组进出**：
// assistant(tool_calls=[c1..cn]) 之后紧跟的那 n 条 tool 消息是不可分割的，
// 切在中间上游会直接 400（insufficient tool messages，见 context.go:102 的
// 生产事故记录）。所以按「组」从**最旧**开始丢，system 永不参与。
// ★★ 裁剪只作用于**本轮的视图**，绝不修改 history（用户 2026-10-30 裁定）：
//
//	「老内容不再进入 LLM，但是**原文保留**，rtchild 回收时依旧向主 agent
//	  展示全部上下文。」
//
// 这两件事必须分开，否则会静默丢数据：
//
//	视图（进入 LLM 的东西）  ⇒ 按窗口裁，旧内容不再进模型
//	存储（rtChild.history）  ⇒ **全文永不丢弃**
//
// 实现上靠"copy 一份再裁"保证：history 是入参的拷贝（调用方已 append 出
// 新切片），rtTrimToWindow 只对这个临时切片动手。任何"就地裁 history"的
// 改法都会让回收时的上下文残缺 —— 判据 TestRTChild_HistoryIsNeverTrimmed 钉它。
func (a *Agent) buildRTContext(rc *rtChild, history []agentAPI.Message, input string) []agentAPI.Message {
	msgs := make([]agentAPI.Message, 0, len(history)+2)
	msgs = append(msgs, agentAPI.Message{Role: "system", Content: rc.agent.systemPrompt})
	msgs = append(msgs, history...)
	msgs = append(msgs, agentAPI.Message{Role: "user", Content: input})
	// 只裁这份临时视图；history 本身保持完整。
	return rtTrimToWindow(msgs, rc.agent.contextWindowTokens())
}

// rtTrimToWindow 把消息序列裁到窗口内：**从最旧的整组开始丢**。
//
// 「组」= 一条 assistant(tool_calls) 加上紧随其后的全部 tool 消息。
// 其余消息各自成组。这样裁剪永远不会把 tool_call 和它的结果拆开。
//
// 保留顺序：
//  1. msgs[0]（system）永不裁
//  2. 最后一条（本轮 user 输入）永远保留 —— 它是这一轮要回答的东西
//  3. 其余按组从旧到新丢，直到总量进窗口
func rtTrimToWindow(msgs []agentAPI.Message, window int) []agentAPI.Message {
	if len(msgs) <= 2 || window <= 0 {
		return msgs
	}

	// ① 先按「组」切分（不含首尾两条）。
	type group struct{ msgs []agentAPI.Message }
	var groups []group
	for i := 1; i < len(msgs)-1; i++ {
		m := msgs[i]
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			g := group{msgs: []agentAPI.Message{m}}
			// 紧跟的 tool 消息属于同一组（配对不可拆）。
			for i+1 < len(msgs)-1 && msgs[i+1].Role == "tool" {
				i++
				g.msgs = append(g.msgs, msgs[i])
			}
			groups = append(groups, g)
			continue
		}
		groups = append(groups, group{msgs: []agentAPI.Message{m}})
	}

	// ② 总量估算 + 从旧到新丢组。
	total := EstimateTokens(msgs[0].Content) + EstimateTokens(msgs[len(msgs)-1].Content)
	for _, g := range groups {
		for _, m := range g.msgs {
			total += rtMsgTokens(m)
		}
	}
	drop := 0
	for drop < len(groups) && total > window {
		for _, m := range groups[drop].msgs {
			total -= rtMsgTokens(m)
		}
		drop++
	}

	// ③ 组装：system + 剩余组 + 本轮输入。
	out := make([]agentAPI.Message, 0, len(msgs)-drop)
	out = append(out, msgs[0])
	for _, g := range groups[drop:] {
		out = append(out, g.msgs...)
	}
	out = append(out, msgs[len(msgs)-1])
	return out
}

// rtMsgTokens 估算一条消息的 token（正文 + 工具调用参数 + 工具结果）。
func rtMsgTokens(m agentAPI.Message) int {
	n := EstimateTokens(m.Content)
	for _, tc := range m.ToolCalls {
		n += EstimateTokens(tc.Name)
		if tc.Arguments != nil {
			for k, v := range tc.Arguments {
				n += EstimateTokens(k) + EstimateTokens(fmt.Sprintf("%v", v))
			}
		}
	}
	return n + 8 // 角色/结构开销
}

// rtStripOutputTools 从工具表里剥掉 output_send__* 与 output_list_channels。
//
// 理由：rtchild 的输出走 outputch(rt) 的流式管线，与"模型显式点名通道调用
// output_send__X"是两套语义。让两套同时存在会出现"模型选了 output_send__voice
// 于是那条消息不经过流式管线"的分叉 —— 用户听到的与界面上看到的不一致。
func rtStripOutputTools(tools []interface{}) []interface{} {
	out := make([]interface{}, 0, len(tools))
	for _, t := range tools {
		m, ok := t.(map[string]interface{})
		if !ok {
			out = append(out, t)
			continue
		}
		fn, ok := m["function"].(map[string]interface{})
		if !ok {
			out = append(out, t)
			continue
		}
		name, _ := fn["name"].(string)
		if strings.HasPrefix(name, "output_send__") || name == "output_list_channels" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// ── 回收时的上下文交接（用户裁定 2026-10-30） ──
//
// 「老内容不再进入 LLM，但是**原文保留**，rtchild 回收时依旧向主 agent
//   展示全部上下文。」
//
// 所以 rtchild 的生命周期与普通子是**反的**：
//
//	普通子 spawn_child —— 跑完把最终答案交回父（局部 msgs 丢弃）
//	rtchild            —— 回收时把**完整对话**交回父（一条通话的全部原文）
//
// 为什么 rt 要交全文而不是"最终答案"：通话没有"最终答案"这种东西。
// 一条 10 分钟通话由几百轮构成，任何单条摘要都会丢掉"用户到底说过什么"。
// 主 agent 需要的是能复盘的事实，而不是 rtchild 的自我总结（后者已由
// 模型在对话里说过一次了）。

// FormatRTTranscript 把完整上下文渲染成给主 agent 看的纯文本。
//
// 渲染口径（与 formatMergedTimeline 的取向一致：**可复盘**优先）：
//
//	[user]      用户的每一句原话
//	[assistant] 模型说出口的每一句
//	[tool]      工具调用（名字 + 参数摘要）与结果（截断保护见下）
//
// 工具结果**截断到 rtTranscriptToolMaxRunes**：它们是上下文里体积的大头
// （实测单条可达几十万 token，见 toolresult_budget.go 的记录），
// 而复盘需要的是"调了什么、拿到什么结论"，不是整页 HTML。
// 其余消息（user/assistant）**逐字保留**。
func FormatRTTranscript(msgs []agentAPI.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	var sb strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			if strings.TrimSpace(m.Content) != "" {
				fmt.Fprintf(&sb, "[assistant] %s\n", strings.TrimSpace(m.Content))
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&sb, "[tool_call] %s %s\n", tc.Name, rtArgsSummary(tc.Arguments))
			}
		case "tool":
			fmt.Fprintf(&sb, "[tool_result] %s\n", truncateStr(strings.TrimSpace(m.Content), rtTranscriptToolMaxRunes))
		default: // user 及其它
			if strings.TrimSpace(m.Content) != "" {
				fmt.Fprintf(&sb, "[%s] %s\n", m.Role, strings.TrimSpace(m.Content))
			}
		}
	}
	return sb.String()
}

// rtArgsSummary 把工具参数压成一行（用于复盘，不用于重放）。
func rtArgsSummary(args map[string]interface{}) string {
	if len(args) == 0 {
		return ""
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 稳定输出：判据与人工复盘都需要可复现
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, truncateStr(fmt.Sprintf("%v", args[k]), 80)))
	}
	return strings.Join(parts, " ")
}

// rtTranscriptToolMaxRunes 是回收交接时**单个工具结果**的保留上限（rune）。
//
// 只限工具结果：它们是体积大头且复盘只需结论。user/assistant 的原话逐字保留
// ——「展示全部上下文」的口径是对**对话**成立，不包括把整页网页塞回来。
const rtTranscriptToolMaxRunes = 500

// CloseRTChild 是 rtchild 的**回收**入口：把完整上下文交回主 agent，然后停止。
//
// ## 交接走「contextfull 式中断」，不塞正文（用户裁定 2026-10-30）
//
// 用户明确：「交还是类似驻留式子 agent 的 contextfull 中断，让主 agent
// **自行选择**保留的内容。」
//
// 所以回收**不**把通话原文推进主 agent 的上下文，而只推一条 L4 中断：
//
//	内核事件：实时子 agent <id> 已回收，共 N 条消息（约 M 字符）。
//	如需复盘，可调用 rt_transcript 取回原文并按需选取保留内容。
//
// 这与驻留子的 contextfull 是**同一套协议**（"推信号 + 拉状态"）：
//   - 推：raiseVector 出一条 L4，只带规模，不带正文
//   - 拉：主 agent 调 RTTranscript 取全文，自己决定留哪些
//
// 为什么必须由主 agent 选：通话原文动辄几十万字符，全灌进去会直接顶爆
// 它的上下文；而"哪些值得留"是**业务判断**（用户说了什么要紧的事），
// 内核无从替它决定 —— 与 resident-subagent-design.md §7 "原语在内核、
// 决策在父的模型"是同一条原则。
//
// 返回交接了多少条消息（0 表示没有历史）。
func (a *Agent) CloseRTChild(id string) (int, error) {
	a.rtMu.Lock()
	rc := a.rtChildren[id]
	a.rtMu.Unlock()
	if rc == nil {
		return 0, fmt.Errorf("rtchild %s 不存在", id)
	}

	// ① 先取全文规模（此时实例还活着，history 完整）。
	transcript, _ := a.RTTranscript(id)
	text := FormatRTTranscript(transcript)
	n, chars := len(transcript), len(text)

	// ② 再停（顺序不能反）。
	if err := a.StopRTChild(id); err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil // 没有历史：不推中断，别拿空事件烦主 agent
	}

	// ③ 推 L4 信号（contextfull 同款），正文留在 RTTranscript 里等按需拉取。
	//
	// ★ 实例已出登记表，但原文必须仍可取 —— 所以把 transcript 存进
	//   a.rtTranscripts 留档（见 RTTranscript 的读取顺序）。
	a.rtMu.Lock()
	if a.rtClosedTranscripts == nil {
		a.rtClosedTranscripts = map[string][]agentAPI.Message{}
	}
	a.rtClosedTranscripts[id] = transcript
	a.rtMu.Unlock()

	a.raiseVector(VectorRTChildClosed, vectorArgs{RTID: id, RTCount: n, RTChars: chars})
	log.Printf("[rtchild] %s closed: %d messages (%d chars) kept for on-demand transcript", id, n, chars)
	return n, nil
}

// RTTranscript 返回一个 rtchild 的**完整**上下文原文（不过窗口裁剪）。
//
// 读顺序：先查**已回收留档**（CloseRTChild 保存的），再查在用实例的 history。
// 后者保证"通话进行中也能看"，前者保证"回收之后仍能取" —— 两者缺一，
// 「回收时向主 agent 展示全部上下文」就会在某些时序下拿不到原文。
func (a *Agent) RTTranscript(id string) ([]agentAPI.Message, bool) {
	a.rtMu.Lock()
	archived, hasArchived := a.rtClosedTranscripts[id]
	rc := a.rtChildren[id]
	a.rtMu.Unlock()

	if hasArchived {
		out := make([]agentAPI.Message, len(archived))
		copy(out, archived)
		return out, true
	}
	if rc == nil {
		return nil, false
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	// 深拷贝：调用方可能长期持有，不能被后续轮次 append 影响。
	out := make([]agentAPI.Message, len(rc.history))
	copy(out, rc.history)
	return out, true
}

// ── rt_transcript：主 agent 按需拉取 rtchild 的完整上下文 ──

// executeRTTranscript 是主 agent 侧的 `rt_transcript` 工具实现。
//
// 它是 CloseRTChild 那一半"拉状态"的入口 —— 交接协议是「推信号 + 拉状态」：
//
//	推：回收时一条 L4（"已回收，共 N 条，要复盘请调 rt_transcript"）
//	拉：本工具，返回原文，**由主 agent 自己决定保留哪些**
//
// 为什么不让内核直接选：见 CloseRTChild 的文档 —— "哪些值得留"是业务判断，
// 内核无从替父决定（与 resident-subagent-design.md §7 同一原则）。
//
// 参数：
//
//	id        rtchild 标识（必填）
//	offset    从第几条消息开始（0 = 从头）—— 让父能分段读大记录
//	limit     最多返回多少条（0 = 默认 rtTranscriptDefaultLimit）
//	format    "text"（默认，给人/模型读的渲染）/ "raw"（原始消息 JSON 风格逐条）
func (a *Agent) executeRTTranscript(tc agentAPI.ToolCall) string {
	id := strArg(tc, "id")
	if id == "" {
		return "请提供 id 参数（rtchild 标识）"
	}
	msgs, ok := a.RTTranscript(id)
	if !ok {
		return fmt.Sprintf("rtchild %s 不存在，且没有已回收的留档", id)
	}

	offset := rtIntArg(tc, "offset", 0)
	limit := rtIntArg(tc, "limit", rtTranscriptDefaultLimit)
	if limit <= 0 {
		limit = rtTranscriptDefaultLimit
	}
	if offset >= len(msgs) {
		return fmt.Sprintf("rtchild %s 共 %d 条消息，offset=%d 越界", id, len(msgs), offset)
	}
	end := offset + limit
	if end > len(msgs) {
		end = len(msgs)
	}
	page := msgs[offset:end]

	if strings.EqualFold(strArg(tc, "format"), "raw") {
		b, err := json.MarshalIndent(page, "", "  ")
		if err != nil {
			return fmt.Sprintf("序列化失败：%v", err)
		}
		return fmt.Sprintf("【rtchild %s 原文 %d-%d / 共 %d 条】\n%s", id, offset, end, len(msgs), string(b))
	}

	return fmt.Sprintf("【rtchild %s 上下文 %d-%d / 共 %d 条】\n%s",
		id, offset, end, len(msgs), FormatRTTranscript(page))
}

// rtIntArg 取一个整数参数，**同时兼容 JSON 数值与 Go int**。
//
// 命名带 rt 前缀：core 包里 resident_tools.go 已有同签名的 intArg（只认出参
// 不存在时回落零值），两者语义相近但默认值策略不同，混用会静默取错默认。
//
// ★ 为什么必须兼容两种类型（真实缺陷，初版只认 float64）：
// 工具参数经模型 JSON 过来是 float64，但**同一份实现也会被其它 Go 调用方
// 直接调**（比如测试、未来的插件内嵌调用），它们传的是 int。只认 float64
// 时 int 会静默落回默认值 —— 表现为"分页参数填了却不生效"，而且不报错。
func rtIntArg(tc agentAPI.ToolCall, key string, def int) int {
	v, ok := tc.Arguments[key]
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return def
	}
}

// rtTranscriptDefaultLimit 是 rt_transcript 一次返回的默认消息条数。
//
// 不一次全给：一条 10 分钟通话有几百条，整段返回会把调用方自己的上下文顶爆。
// 分段读 + offset 续读，让主 agent 按需决定读到哪儿。
const rtTranscriptDefaultLimit int = 40

// ── rt_children：主 agent 的 rtchild 控制面（单工具多动作） ──

// executeRTChildren 是父对 rtchild 的控制面。
//
// 形状刻意与 `resident_agents` 同构（单工具多动作），但**动作集更小**：
// rtchild 没有任务队列、没有残余回灌、没有 contextfull 三处置
// （它自己的上下文按窗口裁，交接走 contextfull 式中断，见 CloseRTChild）。
//
// 所以只有四个动作：
//
//	list    列出全部 rtchild 与其规模
//	create  创建（**必须成对**给 input + output）
//	send    向 rtchild 注入一条新输入（= 打断当前轮，契约 1）
//	stop    回收（先推交接信号，再停；正文由 rt_transcript 按需拉）
func (a *Agent) executeRTChildren(tc agentAPI.ToolCall) string {
	switch action := strArg(tc, "action"); action {
	case "", "list":
		a.rtMu.Lock()
		ids := make([]string, 0, len(a.rtChildren))
		for id := range a.rtChildren {
			ids = append(ids, id)
		}
		a.rtMu.Unlock()
		if len(ids) == 0 {
			return "当前没有实时子 agent（rtchild）。"
		}
		sort.Strings(ids)
		var b strings.Builder
		fmt.Fprintf(&b, "实时子 agent（rtchild，%d 个）:", len(ids))
		for _, id := range ids {
			if st, ok := a.RTChildStats(id); ok {
				fmt.Fprintf(&b, "\n  - %s 轮次=%d 最新输入seq=%d 覆盖丢弃=%d",
					id, st.Rounds, st.LastSeq, st.Dropped)
				if st.Pending {
					b.WriteString("  ⏳ 有待处理输入")
				}
			}
		}
		b.WriteString("\n（rt 通道：新输入会**打断**当前轮，不排队；回收后原文可用 rt_transcript 取回）")
		return b.String()

	case "create":
		id := strArg(tc, "id")
		input := strArg(tc, "inputch")
		output := strArg(tc, "outputch")
		if input == "" || output == "" {
			// 提示要**可操作**：直接列出当前登记表里哪些通道带 rt 能力。
			return fmt.Sprintf("创建 rtchild 必须成对给出 inputch 与 outputch（当前 inputch=%q outputch=%q）。\n%s",
				input, output, a.listRTChannels())
		}
		got, err := a.SpawnRTChild(RTChildOptions{
			ID:           id,
			Input:        input,
			Output:       output,
			SystemPrompt: strArg(tc, "system_prompt"),
			MaxTools:     rtIntArg(tc, "max_tools", 0),
		})
		if err != nil {
			return fmt.Sprintf("创建 rtchild 失败: %v\n%s", err, a.listRTChannels())
		}
		return fmt.Sprintf("已创建实时子 agent: %s（inputch=%s，outputch=%s）。"+
			"现在往 %s 投递的输入会**立即打断**它的当前轮；它说出的文本会写进 %s。",
			got, input, output, input, output)

	case "send":
		id := strArg(tc, "id")
		text := strArg(tc, "text")
		if id == "" || text == "" {
			return "send 需要 id 与 text"
		}
		seq, err := a.PushRTInput(id, text)
		if err != nil {
			return fmt.Sprintf("发送失败: %v", err)
		}
		return fmt.Sprintf("已投递（seq=%d）。这是**打断式**输入：rtchild 当前的轮次会被取消，"+
			"它会用这条新输入重开一轮。", seq)

	case "stop":
		id := strArg(tc, "id")
		if id == "" {
			return "stop 需要 id"
		}
		n, err := a.CloseRTChild(id)
		if err != nil {
			return fmt.Sprintf("回收失败: %v", err)
		}
		if n == 0 {
			return fmt.Sprintf("已回收 rtchild %s（本次通话没有上下文需要交接）。", id)
		}
		return fmt.Sprintf("已回收 rtchild %s。本次通话共 %d 条消息，已作为一条中断交给你；"+
			"**是否保留、保留哪些由你决定** —— 用 rt_transcript(id=%s, offset, limit) 分段取回，不要把整段塞进上下文。",
			id, n, id)

	default:
		return fmt.Sprintf("未知 action %q（可用：list / create / send / stop）", action)
	}
}

// listRTChannels 列出登记表里带 CapRT 的通道，供 create 失败的提示用。
//
// 为什么要把这个信息塞进错误里：模型看不到登记表，只说"通道未注册/未声明 rt"
// 它就无从修正 —— 会反复重试同一个错名字（本仓有过"4 次 files_write 失败"
// 的先例）。直接列出可选值，一次就能改对。
func (a *Agent) listRTChannels() string {
	if a.io == nil {
		return ""
	}
	reg := a.io.ChannelRegistry()
	if reg == nil {
		return ""
	}
	var names []string
	for _, ch := range reg.List() {
		if ch.Capabilities.Supports(agentIO.CapRT) {
			owner := ch.Owner
			if owner == "" {
				owner = "未分配"
			}
			names = append(names, fmt.Sprintf("%s(归属:%s)", ch.Name, owner))
		}
	}
	if len(names) == 0 {
		return "当前没有任何 **rt 通道**（需要插件用 ChannelDef{Realtime:true} 注册 inputch/outputch）。"
	}
	return "当前可用的 rt 通道：" + strings.Join(names, "、")
}
