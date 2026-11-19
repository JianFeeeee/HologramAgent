package agent

import (
	"log"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/kernelloop"
)

// routeInputByOwner 实现**输入路由**：inputch 是最基本的输入路由单位，
// 划给某个 agent 之后，该通道的输入**只流向那个 agent**，本内核看不到它
// （docs/zh/resident-subagent-design.md §4.1：「路由发生在进内核之前」）。
//
// 为什么必须在进内核之前做：插件注入输入的收口是**父**的 IOManager
// （`cmd/homed` 里 pluginReg 拿到的就是它），而父的内核是该 io 唯一的消费者。
// 如果不按归属路由，登记表里的 Owner 就只是个标签 —— 现场表现正是如此：
// 子挂着 `inputch=[timer]`，timer 的输入却打在父身上，子的轮次永远是 0。
//
// 返回 true = 本次注入已被"持有该 inputch 的 agent"接管，本内核不再处理。
//
// 已知边界：路由只在本 agent 的**直接**驻留子里找。若孙辈的 inputch 由子划拨，
// 而插件注入打在根 io 上，根解析不到那个 owner ⇒ 兜底给根处理（有日志）。
// 这一层要等"孙辈 + 根可见的 agent 表"再收口，此处不静默丢输入。
func (a *Agent) RouteInputByOwner(evt *agentIO.InputEvent) bool {
	if evt == nil || evt.OutputChannel == "" || a.io == nil {
		return false
	}
	// ★ 路由的**推动者**是内核事件循环（2026-11-17 裁定：「处理输入事件路由」
	//   是 eventloop 的核心职责）。本函数只做两件 agent 侧的事：
	//   ① 判定归属（它读的是通道登记表，这是内核数据，不是 agent 私有状态）
	//   ② 把事件交给循环登记的投递口
	//   —— rt 的"单槽覆盖"等语义由 rtchild 的投递口自己实现（类型知识不进内核）。
	entry, ok := a.io.LookupInputChannel(evt.OutputChannel)
	if !ok {
		return false // 通道不存在 ⇒ 走兜底（下面有日志）
	}

	// ★ 悬空通道（2026-11-17 新增）：**不归任何人**，输入写进 stash。
	//
	// 这是"释放"与"悄悄接回"的分水岭。此前 Owner 清空后这里直接
	// `return false` ⇒ 输入落给根 agent，而根以为通道已经放手了 ——
	// 同一个状态、两种相反的预期，日志里还什么都看不到。
	//
	// 现在：悬空 ⇒ 暂存，等父显式接回（AssignAndDrainStash）或划给别的子时
	// 一并交出。悬空期间**既不丢输入，也不让别人偷偷处理**。
	//
	// 判据 TestOrphanedChannelStashesInputInsteadOfFallingBackToParent 钉它。
	if entry.Orphaned && entry.Owner == "" {
		if reg := a.io.ChannelRegistry(); reg != nil {
			if err := reg.Stash(evt.OutputChannel, []*agentIO.InputEvent{evt}); err != nil {
				log.Printf("[route] orphaned inputch %s: stash failed (%v) — input handled by fallback %s",
					evt.OutputChannel, err, a.id)
				return false
			}
			log.Printf("[route] orphaned inputch %s: input stashed (source=%s request=%s), "+
				"waiting for explicit assign/handover", evt.OutputChannel, evt.Source, evt.RequestID)
			return true // 已被"通道暂存"接管，本内核不处理
		}
	}

	if entry.Owner == "" || entry.Owner == string(a.id) {
		return false // 未分配 / 归自己 ⇒ 本内核处理
	}

	// ★ 纯**查表转发**：本函数只判定归属，然后交给 owner 自己登记的投递口。
	//
	// 这里**不认识任何类型** —— rt 的"单槽覆盖 + 取消当前轮"是 rtchild 在
	// `Start` 里注册的 InputSink 自己的行为；驻留子用基座的默认 sink
	// （`DeliverRouted`）。加一种新类型时它会注册自己的 sink，本函数零改动。
	//
	// 归属到一个不存在（或已销毁、登记表尚未归还）的 agent 时**不吞输入**：
	// 返回 false 让本内核兜底并留痕（吞掉一条输入比多处理一条更糟）。
	if delivered(entry.Owner, evt) {
		return true
	}
	return false
}

// delivered 经内核循环把事件交给 owner 登记的投递口。
//
// 返回 false 表示循环未装配（单测/独立工具），调用方退回直投。
func delivered(owner string, evt *agentIO.InputEvent) bool {
	l := kernelloop.Current()
	if l == nil {
		return false
	}
	return l.RouteInput(owner, evt)
}

// inputSink 是**路由的目标端**：内核按 inputch 归属把输入交给它，
// 它把输入放进本 agent 的 io（单通道 + priority 标签随事件走）。
//
// ★ 它与 `RouteInputByOwner` 是一对：一个是"这条输入归谁"，一个是
//
//	"归我之后放哪"。扣在同一个文件里，因为删掉任何一半另一半都没有意义。
//
// ★ 它不做分拣：`io.pushLocal` 只有一条通道，`priority` 标签由**接收方**
//
//	的 agentloop 自己解释（rt 单槽覆盖 / 主·驻留四级阶梯）。
type inputSink struct{ a *Agent }

func (s *inputSink) Deliver(evt any) {
	if s == nil || s.a == nil || s.a.io == nil {
		return
	}
	e, ok := evt.(*agentIO.InputEvent)
	if !ok || e == nil {
		return
	}
	s.a.io.DeliverRouted(e)
}
