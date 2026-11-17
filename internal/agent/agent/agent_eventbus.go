package agent

// agent_eventbus.go —— agent 对事件总线的消费方式（**经安全模块代理**）。
//
// ## 用户裁定（2026-11-17）
//
// > 「事件总线是被安全模块代理的吧，原始总线只被安全模块持有」
//
// 对。总线的**唯一持有者**是安全模块（它注入 `Guard`，做发布/订阅授权与
// 出入向载荷脱敏）。其余所有消费方拿到的都是**代理视图**：
//
//	内核模块   → kernelapi.EventPublisher（Publish 一个动作，无裸总线）
//	插件       → sdk.BusHandle（带插件身份，过闸门）
//	agent      → 本文件的 agentEventBus（同上，身份 = 本 agent）
//
// 这样"拿裸总线伪造内核事件"这条路径在类型层面就不存在了
// （见 internal/events/guard.go 顶部那段：它列出的四条绕过路径）。
//
// ## 为什么 agent 也要带身份
//
// agent 是内核模块，但它发的事件里带 agentID（resident/rt 的事件必须
// 能看出是谁发的）。带身份走过闸门，安全模块才能按主体做策略
// （例如"某个子 agent 不许发 terminal_output"）。
//
// 引擎侧只用 Publish 一个动作（订阅全在内核模块与插件里），
// 所以代理面就一条方法 —— 不暴露 Subscribe，避免"agent 也能拦别人的事件"。

import (
	"log"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// agentEventBus 是 agent 的事件发布代理（身份 = 本 agent 的 id）。
//
// ★ 它**不是** `*events.Bus`：拿不到裸总线，就绕不过闸门。
type agentEventBus struct {
	bus  kernelapi.EventBus
	who  string
	name string
}

// SetEventBus 注入**发布代理**（装配期由类型模块调用一次）。
//
// bus 为 nil 时保持 nil（单测/独立工具没有总线）——发布变成 no-op，
// 与"未装配"语义一致（静默不做，而不是 panic）。
func (a *Agent) SetEventBus(bus kernelapi.EventBus, pluginName string) {
	if a == nil || bus == nil {
		return
	}
	a.publisher = agentEventBus{bus: bus, who: string(a.id), name: pluginName}
}

// publish 以本 agent 的身份发布一条事件（**过安全闸门**）。
//
// 被拒只记日志、不阻断业务：事件是旁路信号（状态面/诊断/插件观察），
// 丢了不该让一轮任务失败。但**必须留痕**——静默丢弃是最难查的一类故障。
func (a *Agent) publish(evtType events.EventType, payload map[string]interface{}) {
	if a == nil || a.publisher.bus == nil || evtType == "" {
		return
	}
	evt := &events.Event{
		Type:    evtType,
		Source:  a.publisher.who,
		Payload: payload,
	}
	if err := a.publisher.bus.PublishAs(events.Publisher{Name: a.publisher.name}, evt); err != nil {
		log.Printf("%s publish %s denied: %v", string(a.id), evtType, err)
	}
}
