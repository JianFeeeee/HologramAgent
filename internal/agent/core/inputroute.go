package core

import (
	"log"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
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
func (a *Agent) routeInputByOwner(evt *agentIO.InputEvent, isInterrupt bool) bool {
	if evt == nil || evt.OutputChannel == "" || a.io == nil {
		return false
	}
	entry, ok := a.io.LookupInputChannel(evt.OutputChannel)
	if !ok || entry.Owner == "" || entry.Owner == string(a.id) {
		return false // 未分配 / 归自己 ⇒ 本内核处理
	}

	a.residentMu.Lock()
	rc := a.residents[entry.Owner]
	a.residentMu.Unlock()
	if rc == nil || rc.agent == nil || rc.agent.io == nil {
		// 归属到一个不存在（或已销毁、登记表尚未归还）的 agent：
		// **不吞输入** —— 由本内核兜底处理并留痕。吞掉一条输入比多处理一条更糟：
		// 用户会看到"消息发出去了却没人理"，而日志里什么都没有。
		log.Printf("[route] inputch %s 归属 %s 无对应 agent，输入由 %s 兜底", evt.OutputChannel, entry.Owner, a.id)
		return false
	}
	rc.agent.io.DeliverRouted(evt, isInterrupt)
	return true
}
