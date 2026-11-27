package agenttype

// callbacks.go —— 把插件的中立回调**装进本类型自己的运行期**。
//
// ## 没有 Hooks.Loop 槽位（docs §0.3）
//
// 基座不认识"帧"，就不可能有"驱动一帧"的槽位。插件类型的帧是本包自己的
// `pluginTask`，插件的 Loop 在 `Runtime.Start` 起的循环里被调用 ——
// 与其他类型"自己起循环、基座只调 Start"完全同构。
//
// 插件看不到 `*agent.Agent` / 本包的帧；它只看到中立视图（runtime.go / frame.go）。

import (
	"context"
	"log"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
)

// installCallbacks 挂上生命周期与上下文预算（**不挂 Loop**）。
func installCallbacks(def pubsdk.AgentTypeDef, a *agent.Agent) error {
	h := a.Hooks()

	// ── 上下文预算（按 Spec 的 Context 档位）──
	h.ContextBudget = contextBudgetFor(def.Spec.Context, a)

	// ── Runtime 生命周期 ──
	h.Runtime = agent.RuntimeHooks{
		Initialize: func(x *agent.Agent) error { return nil },
		Restore:    func(x *agent.Agent, dir string) error { return nil },
		// ★ 按 Spec 决定起哪些循环（本类型自己的实现，基座只调 Start）：
		//   SchedulerStandard ⇒ 队列循环（排队，逐条跑插件的 Loop）
		//   SchedulerNone     ⇒ 单槽循环（最新者胜）
		Start: func(x *agent.Agent) { startLoop(def, x) },
		Stop:  func(x *agent.Agent) {},
	}

	a.SetHooks(h)
	return a.Validate()
}

// contextBudgetFor 按 Spec 的 Context 档位返回预算函数。
//
//	none       不持有上下文 ⇒ 让插件自己决定看多少历史（给满窗）
//	window     整窗不裁 ⇒ 整个模型窗口
//	relevance  按相关性裁 ⇒ 插件的 Loop 自己按需取
func contextBudgetFor(kind pubsdk.ContextKind, a *agent.Agent) func(*agent.Agent, kit.TokenBudget) int {
	switch kind {
	case pubsdk.ContextNone:
		return func(_ *agent.Agent, b kit.TokenBudget) int { return b.MaxContext }
	}
	return func(_ *agent.Agent, b kit.TokenBudget) int { return b.MaxContext }
}

// startLoop 起**本类型自己的**循环。
//
// ★ 队列 / 帧 / 中断栈都是这一种类型的内部实现（docs §0.1）：
//
//	插件声明 Scheduler 档位，本包按档位起一个本包的循环。
//	基座不认识队列、不认识帧，也不需要认识。
func startLoop(def pubsdk.AgentTypeDef, a *agent.Agent) {
	if a == nil {
		return
	}
	queue := def.Spec.Scheduler == pubsdk.SchedulerStandard
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[agenttype] %s loop panic: %v", a.ID(), r)
			}
		}()
		runLoop(def, a, queue)
	}()
}

// runLoop 是**这一种类型的** agentloop（阻塞流水线）。
//
//	取输入 → 建帧 → 调插件的 Loop → 收帧里的回复 → 发出去 → 下一条
func runLoop(def pubsdk.AgentTypeDef, a *agent.Agent, queue bool) {
	iom := a.IO()
	if iom == nil {
		return
	}
	ctx, cancel := context.WithCancel(a.Ctx())
	defer cancel()

	for {
		evt := nextInput(ctx, iom, queue)
		if evt == nil {
			return
		}
		f := &pluginTask{Input: inputTextOf(evt), evt: evt}
		out := def.Loop(newRuntime(a), newFrame(a, f))

		if text := f.Response; text != "" {
			a.EmitReply(evt, text)
		}
		if out == pubsdk.AgentOutcomeFailed {
			a.Logf("plugin loop reported failure on channel %s", evt.OutputChannel)
		}
	}
}

// nextInput 取下一条输入（nil = ctx 已结束，循环该退出）。
//
// queue=true 按到达顺序；false 用单槽（最新者胜 —— 排队中的旧值被丢弃）。
func nextInput(ctx context.Context, iom *agentIO.IOManager, queue bool) *agentIO.InputEvent {
	if queue {
		select {
		case <-ctx.Done():
			return nil
		case evt := <-iom.InputChan():
			return evt
		}
	}
	// 单槽：先清空通道，只留最后一条（最新者胜）。
	var latest *agentIO.InputEvent
	select {
	case <-ctx.Done():
		return nil
	case evt := <-iom.InputChan():
		latest = evt
	default:
	}
	drain := true
	for drain {
		select {
		case evt := <-iom.InputChan():
			latest = evt
		default:
			drain = false
		}
	}
	if latest != nil {
		return latest
	}
	select {
	case <-ctx.Done():
		return nil
	case evt := <-iom.InputChan():
		return evt
	}
}

// inputTextOf 从输入事件里取文本（插件类型只需要文本）。
func inputTextOf(evt *agentIO.InputEvent) string {
	if evt == nil {
		return ""
	}
	if s, ok := evt.Payload["content"].(string); ok {
		return s
	}
	if s, ok := evt.Payload["text"].(string); ok {
		return s
	}
	return ""
}
