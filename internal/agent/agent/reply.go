package agent

import (
	"log"
)

// reply.go —— **跳过回执**（基座原语）。
//
// 队列满 / 背压 / 目标不存在时，基座必须给同步调用方一个明确回执 ——
// 否则对方永久挂起（这是本仓反复栽过的形态：吞掉一条输入比多处理一条更糟）。
//
// ★ 它是基座的：判定"队列满"发生在**基座的调度驱动**里，回执必须与判定同层。

import agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"

// EmitReply 把一帧的回复交付出去（**基座的出口原语**）。
//
// ## 为什么它在基座
//
// 两条投递都是"把一段文本交给通道"，与类型无关：
//
//	① 输出通道（`EmitOutputTo`）  —— 用户真正看到它的地方
//	② 同步回执（`ResponseCh`）    —— 带 ResponseCh 的注入方靠它解除阻塞
//
// 丢一条的后果都不是报错而是**静默**：只发①则 cli/a2a 永久挂起，
// 只发②则异步通道（qq 等）根本收不到。
//
// ## 为什么不做阶段钩子
//
// `before_output` / `after_output` 是**类型**的阶段（它知道本轮 payload 该
// 透传什么、FinalText 该被谁改写）。基座只做投递；类型的循环在调本方法之前
// 自己跑阶段。
func (a *Agent) EmitReply(evt *agentIO.InputEvent, text string) {
	if a == nil || evt == nil {
		return
	}
	ch := evt.OutputChannel
	if ch == "" {
		ch = evt.Source
	}
	payload := map[string]interface{}{
		"content":    text,
		"request_id": evt.RequestID,
	}
	if a.io != nil && ch != "" && text != "" {
		a.io.EmitOutputTo(ch, ch, "text", payload)
	}
	if evt.ResponseCh != nil {
		select {
		case evt.ResponseCh <- &agentIO.OutputEvent{
			RequestID:     evt.RequestID,
			Target:        evt.Source,
			Type:          "text",
			Payload:       payload,
			Done:          true,
			OutputChannel: ch,
		}:
		default:
			log.Printf("[agent] %s: ResponseCh full, terminal receipt dropped (request=%s)", a.id, evt.RequestID)
		}
	}
}

// emitSkippedReply 给被跳过任务的调用方一个终态。
//
// 为什么要单独一条路径而不是复用 emitResponse：跳过意味着“我们没有处理这条输入”，
// 不应对外发 agent_output 事件（否则 WebUI 聊天记录会凭空多出一条空消息），
// 但必须写 ResponseCh——否则 cli/clawhub 这类无超时的同步注入会永久挂起。
//
// ❗异步来源（qq / wechat / rss 等）**没有 ResponseCh**，于是这里以前是直接 return。
// 后果是任务被丢弃时**完全无声**：用户什么都没收到、日志里也没痕迹，
// 他只会以为消息丢了。转投子被回收/销毁时队列里的积压正落在这个盲区里
// （父可随时对子 reclaim/destroy，而子手上可能还握着几条 QQ 消息）。
// 现在至少留一条带来源与通道的日志，让“这条消息为什么没回”可被追溯。
//
// 非阻塞写：ResponseCh 由同步调用方以 cap=1 创建，调用方超时离开后仍可写入。
func (a *Agent) EmitSkippedReply(evt *agentIO.InputEvent, reason string) {
	if evt == nil {
		return
	}
	if evt.ResponseCh == nil {
		// 无可回执的通道：不静默。异步来源本就靠 agent 主动 output_send，
		// 丢弃后没有任何东西会告诉用户，因此这条日志是唯一的线索。
		log.Printf("[agent] %s: dropping input on channel without receipt support (source=%s channel=%s request=%s reason=%s)",
			a.id, evt.Source, evt.OutputChannel, evt.RequestID, reason)
		return
	}
	ch := evt.OutputChannel
	if ch == "" {
		ch = evt.Source
	}
	payload := map[string]interface{}{
		"content":    "",
		"request_id": evt.RequestID,
		"skipped":    true,
		"reason":     reason,
	}
	select {
	case evt.ResponseCh <- &agentIO.OutputEvent{
		RequestID:     evt.RequestID,
		Target:        evt.Source,
		Type:          "text",
		Payload:       payload,
		Done:          true,
		OutputChannel: ch,
	}:
	default:
	}
}
