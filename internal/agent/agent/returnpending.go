package agent

// returnpending.go —— **任务退回**的基座原语（用户裁定 2026-11-27）。
//
// # 语义（一句话）
//
// **调度器取走事件 → 通过本接口退回。** 接口只负责**路由**，不负责取。
//
// 取走是类型内部的事（调度器归各类型），退回是跨实例的事（路由归内核）。
// 混在一起就会让每个类型自己实现一套"丢给谁"的逻辑 —— 而那是同一件事。
//
// # 为什么基座需要它
//
// 退回本来散在各处：
//
//	mainagent/offload.go     父→子推（转投，也是一种退回）
//	resident/resident.go     子→父退（AcquireResidual → 逐条归位通道）
//	rtchild                  自己一套
//	软重启 RecreateChild     丢弃（换了实现，任务没有意义）
//
// 三套实现各写一遍路由，必然漂移（本仓已有实测教训：同一个 ID 抹除流程
// 在单数/复数函数里漏了一处，残留单调累积）。
//
// # 两个流向
//
//	① 退回到**某个通道**：逐条按任务自己的 OutputChannel 归位（不混存）
//	② 退回到**某个 agent**：转投给另一个实例（保留原事件与 ResponseCh）
//
// ★ 只有主 agent 能退给 agent：它才是"父"（唯一能建子、唯一知道有谁可承受
// 的实例）。子 agent 退给 agent 等于绕过父做调度，那不是它的权限。
// 退给通道则任何实例都可以 —— 那是"还给用户"。
//
// # 丢弃不在这里
//
// 「drop」是**类型的高层语义**（软重启、父明确决定丢弃），由类型在拿到
// 取走的事件后自己决定不调本接口。基座不为"丢弃"提供原语：丢弃一旦成为
// 一个可选参数，就会有调用方顺手传它，而不可追溯的丢弃是本仓明令禁止的。
// 要丢弃的类型必须自己逐条留痕并给调用方终态（用它已有的能力）。

import (
	"fmt"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// ReturnTarget 指定退回的流向。
//
// 二选一：Channel 或 AgentID。两者都空 ⇒ 按**逐条自身通道**归位（最常见）。
type ReturnTarget struct {
	// Channel 是目标通道名。空 ⇒ 逐条按任务自己的 OutputChannel 归位。
	//
	// ★ 为什么默认是"逐条归位"而不是"统一塞一个通道"：一个实例可能同时
	//   收 qq 和 webui 的消息，混存会导致接回 qq 的人拿到本该给 webui 的活。
	//   （这条理由来自 resident 的 stash 实现，是实测教训。）
	Channel string

	// AgentID 是转投目标实例。非空 ⇒ 退给那个 agent。
	//
	// 仅主 agent 可用（理由见文件头）。
	AgentID string
}

// ReturnPending 把**已从调度器取走**的事件按 target 退回。
//
// 调用方（类型的 Admin / 收尾流程）负责先用自己调度器的取走原语把事件取出来；
// 本函数只做路由。这样"取"留在类型内（它的队列语义只有它知道），
// "退"收在基座（跨实例路由只有内核知道）。
//
// 返回实际路由成功数与给模型/调用方看的摘要。
func (a *Agent) ReturnPending(events []*agentIO.InputEvent, target ReturnTarget) (int, string, error) {
	if a == nil {
		return 0, "", fmt.Errorf("return: agent 为空")
	}
	if len(events) == 0 {
		return 0, "无待退回任务", nil
	}

	// ① 退给另一个 agent（转投）
	if target.AgentID != "" {
		if a.IsChildAgent() {
			// 子退回给另一个实例 = 绕过父做调度，不是它的权限。
			return 0, "", fmt.Errorf("return: 只有主 agent 能把任务转投给其他 agent")
		}
		if target.AgentID == string(a.id) {
			return 0, "", fmt.Errorf("return: 不能转投给自己")
		}
		moved := 0
		for _, evt := range events {
			if evt == nil {
				continue
			}
			// ★ 推**原事件**（不重建）：原事件带 ResponseCh，cli/a2a 这类
			//   同步调用方正阻塞等它。重建事件会让子处理完、调用方却永久挂起
			//   （本仓实测过的坑，offload 的注释里写着）。
			if !a.RouteInputTo(target.AgentID, evt) {
				// 目标不可用：给同步调用方终态，否则它会永久挂起。
				a.EmitSkippedReply(evt, "return_target_unavailable")
				continue
			}
			moved++
		}
		if moved == 0 {
			return 0, "", fmt.Errorf("return: 目标 %q 不可用，%d 条任务已给调用方终态", target.AgentID, len(events))
		}
		return moved, fmt.Sprintf("已把 %d 条任务转投给 %s", moved, target.AgentID), nil
	}

	// ② 退给通道
	return a.returnToChannels(events, target.Channel)
}

// returnToChannels 把事件退回到通道（空 ⇒ 逐条按自身 OutputChannel 归位）。
//
// 通道已不存在的条目必须：① 留痕 ② 给同步调用方终态。
// 只丢不留痕是禁止的（"这条消息为什么没人回"永远查不出来）。
func (a *Agent) returnToChannels(events []*agentIO.InputEvent, channel string) (int, string, error) {
	var reg *agentIO.ChannelRegistry
	if io := a.IO(); io != nil {
		reg = io.ChannelRegistry()
	}
	if reg == nil {
		return 0, "", fmt.Errorf("return: 无通道登记表，无法退回任务")
	}

	moved, failed := 0, 0
	for _, evt := range events {
		if evt == nil {
			continue
		}
		// ★ 归位用 **Source**（消息进来的那个通道），不是 OutputChannel：
		//   `InputEvent.Source` 就是入站通道标识 —— 主 agent 的场景记忆正是
		//   靠它（`ChannelScene(evt.Source)` → `chan:<通道>`）把每轮对话归到
		//   对应来源。OutputChannel 是"回程往哪送"，任务本身属于它进来的那条
		//   通道；接回通道的人应该拿到"从我这个通道进来的活"。
		ch := channel
		if ch == "" {
			ch = evt.Source
			if ch == "" {
				ch = evt.OutputChannel // 无来源标识时用回程通道兜底
			}
		}
		if ch == "" {
			failed++
			a.EmitSkippedReply(evt, "return_no_channel")
			continue
		}
		if err := reg.Stash(ch, []*agentIO.InputEvent{evt}); err != nil {
			// 通道已不存在（插件卸载等）：给终态，不让调用方挂死。
			failed++
			a.EmitSkippedReply(evt, "return_channel_gone")
			continue
		}
		moved++
	}

	switch {
	case moved == 0:
		return 0, "", fmt.Errorf("return: %d 条任务的目标通道都已不存在（已给调用方终态）", failed)
	case failed > 0:
		return moved, fmt.Sprintf("已退回 %d 条任务到通道；%d 条因通道不存在无法退回（已给调用方终态）", moved, failed), nil
	}
	return moved, fmt.Sprintf("已退回 %d 条任务到通道", moved), nil
}
