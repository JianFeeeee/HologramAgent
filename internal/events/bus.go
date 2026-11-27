package events

import (
	"fmt"
	"log"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

// EventType / Event / Publisher / Handler 的**声明点**在 kernelapi/model
// （接口层的共享值模型）。这里做别名：实现包反向依赖值模型，调用点零改动。
//
// ★ 为什么必须这样（2026-11-17 第⑥次裁定）：安全模块注册的代理面要成为
//
//	**唯一选择**，而它的契约由 `kernelapi`（叶子包）声明 —— 叶子包不能
//	import 本实现包，所以事件的值类型必须声明在更底层。
type EventType = model.EventType

const (
	EventAgentLLMChain EventType = "agent_llm_chain"
	EventReasoning     EventType = "reasoning"
	// EventScheduler 是输入调度器的状态变更事件（抢占/挂起/恢复），
	// 供状态页与诊断订阅（设计文档 §11 O2）。
	EventScheduler      EventType = "scheduler"
	EventSystem         EventType = "system"
	EventTerminalOutput EventType = "terminal_output"

	// ★ 已删六个事件类型（2026-11-25，docs §9.5 第 3 条）：
	//
	//	EventRawInput    → on_input 阶段钩子（RawMessage / UserID / GroupID / Extra）
	//	EventAgentOutput → before_output / after_output 阶段 + EmitOutputTo 本身
	//	EventStage       → 各插件自己在 stage handler 里通知自己的客户端
	//	EventContentDelta    → 输出通道逐 chunk 调用（payload{"delta": true}）
	//	EventReasoningDelta  → 同上（payload{"kind": "reasoning"}）
	//
	// 判据：它们是 agent 状态，不是内核事实。总线只服务**内核模块之间**。

	// skill_detected：clawhubadapter（OpenClaw 兼容层）扫描 skills 目录时
	// 发现纯 SKILL 类型插件后发布，由原生 skillmgr 插件订阅并接管注册。
	// 职责链：发现者（兼容层）→ 移交事件 → 归属者（skillmgr）加载管理。
	EventSkillDetected EventType = "skill_detected"

	// EventToolsReady 在**全部插件加载完成、工具已注册完毕**之后发布一次。
	//
	// 语义边界（关键）：它只保证"工具调用可用"，**不保证 agent 正在运行**。
	// 订阅者若只是执行一段工具调用序列（如 seq 的开机序列），此刻即可开始；
	// 它们的返回值进队列，等 agent 起来后再处理即可。
	//
	// 为什么不是 AgentReady：seq 自动运行**不依赖 agent** —— 它只是一段
	// 调用序列。把事件绑在 agent.Start() 上会让时机被人为推后，且把
	// "何时可跑序列"这个内核判断外包给编排层（main），换个入口就漂移。
	//
	// 发布点：kernelmod/pluginhost 在 `ks.PluginsReady = true` 处发布
	// （那里正是"内置插件也已 LoadSkipping 完成"的时刻）。
	//
	// payload 约定：
	//
	//	plugins:       []string，已加载插件名
	//	plugin_count:  int
	EventToolsReady EventType = "tools_ready"

	// EventPluginsChanged 是**插件可见面发生变化**的事件（用户裁定 2026-11-27）。
	//
	// 语义：某实例"能用哪些插件/工具"这张表变了，订阅者应当**重新拉取**自己的
	// 虚拟插件管理器。**不是**"插件内容变了"——内容变化（重载/升级）也走这里，
	// 因为对实例而言后果一致：它看到的表要重算。
	//
	// 触发者：
	//
	//	· admin_child 改授权（SetGrant / ClearGrant）
	//	· 插件加载 / 卸载 / 重载 / 崩溃重启
	//	· 插件持久禁用 / 启用、运行态启停
	//
	// ★ 为什么不把新表塞进 payload：表是按实例算的，一个事件要服务 N 个实例，
	//   塞进去就得塞 N 份。事件只说"去拉"，拉什么由各实例自己算。
	//
	// payload 约定：
	//
	//	agent_id:      string，**受影响**的实例（空 = 全部实例都要重算）
	//	reason:        string，"grant" | "load" | "unload" | "reload" | "crash"
	//	                  | "enable" | "disable"
	//	plugin:        string，引发变化的插件名（可空）
	EventPluginsChanged EventType = "plugins_changed"

	// ★ 已删 `EventMemoryAccess`（"memory_access"）：它描述的消费方是星图前端
	//   （按真实块 ID 高亮），而星图已不存在。全仓零发布者、零订阅者 ——
	//   留在总线里只会让下一个人以为「图记忆读写有事件可订」。
	//   图记忆的读写事实走槽内工具（graph_recall / graph_commit）的结果。

	// ── 更新子系统（internal/update）的事件 ──
	//
	// 为什么不走"工具返回值 + WebUI 轮询"：
	// 升级是**长耗时且会重启进程**的动作。轮询在重启瞬间必然断，
	// 而断线时用户最需要知道"升级到哪一步了/失败了/已回滚"。
	// 事件总线是内核既有的发布订阅机制（同步投递、handler panic 隔离），
	// 用它不需要新造一条通知通道。

	// EventUpdateCheck 发布一次「更新检查」结果（内核或插件）。
	//
	// 语义：只读检查，不改动任何东西。WebUI 据此刷新版本徽标。
	//
	// payload 约定：
	//
	//	scope:      "kernel" | "plugin"
	//	name:       插件名（scope=plugin 时有值；kernel 时为空）
	//	current:    本地版本
	//	latest:     远端最新版本（查不到时为空）
	//	has_update: bool
	//	reason:     查不到的原因（可空）
	EventUpdateCheck EventType = "update_check"

	// EventUpdateProgress 发布升级过程中的一个步骤。
	//
	// 语义：进度通知。升级会让进程消失，所以每一步都必须**先发事件再动作** ——
	// 顺序反了的话，最后几步的事件永远发不出去（进程已经没了）。
	//
	// payload 约定：
	//
	//	scope:  "kernel" | "plugin"
	//	name:   插件名（scope=plugin 时有值）
	//	step:   "verify" | "backup" | "replace" | "restart" | "verify_version"
	//	         | "rollback" | "done" | "failed"
	//	detail: 该步的人读说明
	EventUpdateProgress EventType = "update_progress"

	// EventUpdateRequest 是**其他插件请求更新**的入口。
	//
	// ## 为什么用事件而不是让各插件直接 import internal/update
	//
	// 请求方只需说"我要更新"，不必知道怎么拉源码、怎么编译、怎么替换、
	// 怎么回滚。那些是本子系统的事。用事件解耦后：
	//
	//	· 请求方连 import 都不需要（外置插件更不可能 import 内核内部包）
	//	· 请求与进度都在总线上，WebUI 能显示"谁请求 → 到哪一步 → 结果如何"
	//	· 多个插件同时请求时天然按到达顺序处理，不会两个 git clone 抢同一工作目录
	//
	// payload 约定：
	//
	//	scope:  "kernel" | "plugin"
	//	name:   插件名（scope=plugin 时必填）
	//	pkg:    本机安装包路径（可选）
	//	release: bool，从仓库最新 Release 自动取包（可选）
	EventUpdateRequest EventType = "update_request"

	// EventUpdateResult 发布升级的最终结果（成功或失败，含回滚轨迹）。
	//
	// payload 约定：
	//
	//	scope:       "kernel" | "plugin"
	//	name:        插件名（scope=plugin 时有值）
	//	ok:          bool
	//	from / to:   版本轨迹（回滚时 to 是回滚后的版本）
	//	rolled_back: bool
	//	error:       失败原因（成功时为空）
	EventUpdateResult EventType = "update_result"

	// EventSecurityReady 在**安全防火墙装配完成**后发布一次。
	//
	// 语义：此后 Core 与外部模块之间的数据流都经过安全闸门。
	// 订阅者（状态面 / healthcheck）据此报告"审查是否真的生效"，
	// 而不是靠读配置猜 —— 配置开了但装配失败是另一回事。
	//
	// payload 约定：
	//
	//	enabled:    bool，是否真的在审查（配置关闭时 false）
	//	variables:  int，变量库条数
	EventSecurityReady EventType = "security_ready"

	// EventDeviceReady 在**设备/工具数据面装配完成**后发布一次。
	//
	// payload 约定：
	//
	//	tools:   int，模型可见的工具数
	//	secured: bool，安全审查是否真的在生效
	EventDeviceReady EventType = "device_ready"

	EventAll EventType = "*"
)

type Event = model.Event

type Handler = model.Handler

type Bus struct {
	mu   sync.RWMutex
	subs map[EventType][]Handler

	// dispatch 是**投递驱动者**（可 nil = 当场同步投递，即改造前的行为）。
	//
	// ★ 用户裁定（2026-11-17 第⑤次）：「publish，订阅这些现在这样就行，
	//   只不过 eventbus 的路由过程由 eventloop 推动了」。
	//   ⇒ 总线的 API 面（Publish/Subscribe/闸门/订阅表）一律不动；
	//     只有「这条事件投给谁、按什么顺序投」这一步交给事件循环来推动。
	//
	// 为什么是"驱动者"而不是"异步化 Publish"：
	//
	//	· 闸门判定必须发生在**发布时刻**（DeniedError 要能当场返回给调用方），
	//	  这是既有契约，不能因为换驱动者就变成"发了、没反应"
	//	· 投递才是有可能耗时的部分（订阅者可能做 IO / 慢解析）
	//
	// 所以 dispatch 只在"闸门已放行、载荷已脱敏"之后被调用一次。
	dispatch Dispatcher

	// guard 是**总线闸门**（可 nil = 不设闸门）。
	//
	// ★ 放在总线上而不是适配层：在此之前，策略只存在于
	// internal/sdk/events.go 那一条路径，而 s.EventBus() 和 s.Subscribe()
	// 拿的是裸总线 —— 任何插件都能绕过它发任意事件（含伪造内核事件）。
	//
	// 现在：Publish / Subscribe 本身先问 guard，绕不过去。
	guard Guard
}

// Dispatcher 是**投递驱动者**的契约（由事件循环实现）。
//
// 语义：把一次已过闸、已脱敏的投递"推动"出去。
// 实现可以是异步队列，也可以是当场同步（后者即改造前的行为）。
//
// ★ 它**不**负责授权与脱敏：那两步已在 publishAs 里完成，传进来的
// handlers/evt 已经是"可以被看见的东西"。把闸门放进 Dispatcher 会让
// "被拒"变成投递期才知道，而调用方那时早已返回。
type Dispatcher = model.Dispatcher

// SetDispatcher 注入投递驱动者（由事件循环在启动时调用一次）。
//
// 传 nil 恢复"当场同步投递"（改造前的行为，单测/独立工具用）。
func (b *Bus) SetDispatcher(d Dispatcher) {
	b.mu.Lock()
	b.dispatch = d
	b.mu.Unlock()
}

// Dispatcher 返回当前投递驱动者（可能 nil = 同步当场投递）。
func (b *Bus) Dispatcher() Dispatcher {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.dispatch
}

func NewBus() *Bus {
	return &Bus{
		subs: make(map[EventType][]Handler),
	}
}

// SetGuard 注入闸门（由装配方调用，通常一次）。
//
// 传 nil 等于"不设闸门"（开发/test 场景）。
func (b *Bus) SetGuard(g Guard) {
	b.mu.Lock()
	b.guard = g
	b.mu.Unlock()
}

// Guard 返回当前闸门（可能 nil）。
func (b *Bus) Guard() Guard {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.guard
}

// Publish 以**内核身份**发布（等价于 PublishAs{Name: ""}）。
//
// 保留这个签名是为了不让几十个内核调用点全部改签名：内核模块本来就
// 是可信主体，它们不需要授权判定。但**载荷脱敏照样执行**。
func (b *Bus) Publish(evt *Event) {
	_ = b.publishAs(Publisher{}, evt)
}

// PublishAs 以指定身份发布（**插件侧必须用这个**）。
//
// 返回 error：被拒时要让调用方知道，而不是静默丢弃。
func (b *Bus) PublishAs(p Publisher, evt *Event) error {
	return b.publishAs(p, evt)
}

func (b *Bus) publishAs(p Publisher, evt *Event) error {
	if evt == nil {
		return nil
	}

	b.mu.RLock()
	g := b.guard
	b.mu.RUnlock()

	if g != nil {
		// ① 授权
		if allowed, reason := g.AllowPublish(p, evt.Type); !allowed {
			log.Printf("[bus] publish denied: publisher=%q type=%q: %s",
				p.Name, evt.Type, reason)
			return &DeniedError{Publisher: p.Name, EventType: string(evt.Type), Reason: reason}
		}
		// ② 载荷脱敏（**内核身份也过**：内核也可能把敏感值拼进事件）
		if evt.Payload != nil {
			evt.Payload = g.ScrubPayload(evt.Type, evt.Payload)
		}
	}

	// ③ 入向清洗（投递前，**一次覆盖全部订阅者**）。
	//
	// 与出向的差别：出向洗的是「发布者拼进去的东西」，入向洗的是
	// 「这个载荷在到订阅者手上之前是否还带明文凭据」。两者都做不是重复：
	// 发布时洗过的载荷中间可能被转发/改写（例如中继模块重新组包），
	// 而订阅者看到的东西必须是安全的。
	if g != nil && evt.Payload != nil {
		evt.Payload = g.ScrubInbound(evt.Type, evt.Payload)
	}

	b.mu.RLock()
	d := b.dispatch
	allHandlers := make([]Handler, len(b.subs[EventAll]))
	copy(allHandlers, b.subs[EventAll])
	typeHandlers := make([]Handler, len(b.subs[evt.Type]))
	copy(typeHandlers, b.subs[evt.Type])
	b.mu.RUnlock()

	// 两轮全部订阅者合成一批（顺序：通配在前、类型在后，与改造前一致）。
	handlers := make([]Handler, 0, len(allHandlers)+len(typeHandlers))
	handlers = append(handlers, allHandlers...)
	handlers = append(handlers, typeHandlers...)

	// ★ 投递由**驱动者**推动（事件循环 / 缺省同步）。
	//   闸门与脱敏已在上面做完了 —— 到这里事件已经"可以被人看见"。
	if d != nil {
		d.Dispatch(evt, handlers)
		return nil
	}
	for _, h := range handlers {
		b.safeCall(h, evt)
	}
	return nil
}

func (b *Bus) safeCall(h Handler, evt *Event) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[bus] handler panic: %v", r)
		}
	}()
	h(evt)
}

// Subscribe 以**内核身份**订阅（内核模块用）。
func (b *Bus) Subscribe(eventType EventType, handler Handler) func() {
	fn, err := b.subscribeAs(Publisher{}, eventType, handler)
	if err != nil {
		// 内核身份不会被拒（策略只约束插件），但保留返回值以防将来收紧
		log.Printf("[bus] kernel subscribe rejected: %v", err)
	}
	return fn
}

// SubscribeAs 以指定身份订阅（**插件侧必须用这个**）。
func (b *Bus) SubscribeAs(p Publisher, eventType EventType, handler Handler) (func(), error) {
	return b.subscribeAs(p, eventType, handler)
}

func (b *Bus) subscribeAs(p Publisher, eventType EventType, handler Handler) (func(), error) {
	b.mu.RLock()
	g := b.guard
	b.mu.RUnlock()

	// EventAll 是内部通配订阅，不做逐类型判定（只允许内核使用：
	// 插件走 s.Events().Subscribe 时本来就限制了具体类型）。
	if g != nil && eventType != EventAll {
		if allowed, reason := g.AllowSubscribe(p, eventType); !allowed {
			log.Printf("[bus] subscribe denied: publisher=%q type=%q: %s",
				p.Name, eventType, reason)
			return func() {}, &DeniedError{
				Publisher: p.Name, EventType: string(eventType), Reason: reason, Subscribe: true,
			}
		}
	}

	b.mu.Lock()
	b.subs[eventType] = append(b.subs[eventType], handler)
	b.mu.Unlock()

	return func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		list := b.subs[eventType]
		for i, h := range list {
			if fmt.Sprintf("%p", h) == fmt.Sprintf("%p", handler) {
				b.subs[eventType] = append(list[:i], list[i+1:]...)
				break
			}
		}
	}, nil
}
