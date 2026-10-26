package events

import (
	"fmt"
	"log"
	"sync"
)

type EventType string

const (
	EventRawInput      EventType = "raw_input"
	EventAgentOutput   EventType = "agent_output"
	EventAgentLLMChain EventType = "agent_llm_chain"
	EventToolCall      EventType = "tool_call"
	EventReasoning     EventType = "reasoning"
	EventStage         EventType = "stage"
	// EventScheduler 是输入调度器的状态变更事件（抢占/挂起/恢复），
	// 供状态页与诊断订阅（设计文档 §11 O2）。
	EventScheduler      EventType = "scheduler"
	EventSystem         EventType = "system"
	EventTerminalOutput EventType = "terminal_output"

	// 流式增量事件（LLM token 级）：核心改为流式后每收到一个增量块发布。
	// 订阅者可选订；不认识的旧订阅者自然忽略（Bus 按 EventType 精确匹配分发）。
	// 聚合事件 EventReasoning / EventAgentLLMChain 仍照常在每轮结束时全文发布，
	// 插件体系行为不变。
	EventReasoningDelta EventType = "reasoning_delta"
	EventContentDelta   EventType = "content_delta"

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

	// EventMemoryAccess 是**图记忆读写事件**：谁读了/写了哪几个记忆块。
	//
	// 为什么需要它（2026-10-08）：星图此前的「跟随 agent 活动」靠**工具名猜节点**
	// （把 "memory_recall" 拆成词元去匹配实体名）。而图谱里的实体名是中文概念
	// （小宅/对话/待命），与英文工具名永不交集 —— 实测每个工具名命中 0 个节点，
	// 于是每次都回退到「按 mention_count 取前 8 个」，即每次工具调用点亮的都是
	// **同一批无关节点**。
	//
	// 正确的做法是让**图数据库自己在读写处上报**：它本来就知道自己碰了哪些块 ID。
	// payload 约定：
	//
	//	op:       "recall" | "commit" | "merge" | "purge" | "delete"
	//	blocks:   []string，本次实际读写到的块 ID
	//	created:  []string，本次**新建**的块 ID（驱动「从小变大」生长动画）
	//	removed:  []string，本次**删除/合入消失**的块 ID（驱动粒子消散）
	//	merged:   [][2]string 或 []map，合入对（驱动「两节点消失又出现」）
	//	tool:     触发这次访问的工具名（可空，便于 UI 显示来源）
	//
	// 订阅者（webui SSE）据此按**真实块 ID** 精确高亮，不再猜。
	EventMemoryAccess EventType = "memory_access"

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

	EventAll EventType = "*"
)

type Event struct {
	Type      EventType              `json:"type"`
	Source    string                 `json:"source"`
	Payload   map[string]interface{} `json:"payload"`
	Timestamp int64                  `json:"timestamp"`
}

type Handler func(event *Event)

type Bus struct {
	mu   sync.RWMutex
	subs map[EventType][]Handler
}

func NewBus() *Bus {
	return &Bus{
		subs: make(map[EventType][]Handler),
	}
}

func (b *Bus) Publish(evt *Event) {
	b.mu.RLock()
	allHandlers := make([]Handler, len(b.subs[EventAll]))
	copy(allHandlers, b.subs[EventAll])
	typeHandlers := make([]Handler, len(b.subs[evt.Type]))
	copy(typeHandlers, b.subs[evt.Type])
	b.mu.RUnlock()

	for _, h := range allHandlers {
		b.safeCall(h, evt)
	}
	for _, h := range typeHandlers {
		b.safeCall(h, evt)
	}
}

func (b *Bus) safeCall(h Handler, evt *Event) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[bus] handler panic: %v", r)
		}
	}()
	h(evt)
}

func (b *Bus) Subscribe(eventType EventType, handler Handler) func() {
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
	}
}
