package resident

// 驻留子的**工具面**（对照设计 §7 控制面与 §8 处理表）。
//
// 单工具多动作：父侧的入口是 `admin_child(type="resident", action=...)`（list/create/send/inspect/compress/reclaim/destroy），
// 子侧两个小工具：`notify_parent`（L3 主动汇报）与 `inputch_note`（主动写处理表）。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agenttypes"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
	"path/filepath"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

func strArg(tc agentAPI.ToolCall, key string) string {
	s, _ := tc.Arguments[key].(string)
	return strings.TrimSpace(s)
}

func splitArg(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// reclaimKeepAll 是回收时的默认策略：把子 temp 的活跃记录全部纳入主记忆
// （"哪些纳入"由父的模型决定——这里给的是"全要"这一档）。
func reclaimKeepAll(_ []agenttypes.InputchRecord, triples []model.Triple) []model.Triple {
	return triples
}

// residualPolicyOf 从工具参数读残余任务的处置策略（keep/drop）。
//
// 默认 keep：父没明确说丢时，一律转回自己而不是丢弃。
// "宁可多做一件，不可默默丢一条"——吞掉一条输入比多处理一条更糟。
func residualPolicyOf(tc agentAPI.ToolCall) agenttypes.ResidualPolicy {
	switch strings.ToLower(strArg(tc, "residual")) {
	case "drop", "discard":
		return agenttypes.ResidualDrop
	case "stash", "park":
		// 暂存到原通道，等通道被接回时放出。
		return agenttypes.ResidualStash
	case "takeover", "take_over", "take":
		return agenttypes.ResidualTakeover
	default:
		return agenttypes.ResidualKeep
	}
}

// channelDispositionOf 从工具参数读**通道**的处置方式。
//
// 2026-11-17 收敛为两种（原先 release/suspend 机制上等价，只有一行日志的差别）：
//
//	orphan（默认）      ⇒ 悬空：输入写进 stash，等显式接回或划拨
//	handover / takeover ⇒ 显式交接给目标 agent（缺省父），交出暂存
//
// ★ 旧的 `suspend`/`park` 参数值**仍被接受**并映射到 orphan —— 不能因为
// 内部收敛就把模型已经学会的参数名变成报错（那会让存量行为突然失效）。
// 但它们现在与 orphan 完全同义，不再声称有额外语义。
func channelDispositionOf(tc agentAPI.ToolCall) agenttypes.ChannelDisposition {
	switch strings.ToLower(strArg(tc, "channels")) {
	case "handover", "takeover", "take_over", "take":
		return agenttypes.ChannelHandover
	default:
		// orphan / suspend / park / 空 —— 都是悬空。
		return agenttypes.ChannelOrphan
	}
}

// handoverTargetOf 读交接目标（空 = 父自己）。
//
// 为什么单独一个字段：`channels` 是**处置方式**，目标 agent 是另一件事。
// 塞进同一个字符串（"handover:res_1"）会让参数解析变成字符串切片游戏，
// 且模型很容易写错分隔符。分开更不容易错。
func handoverTargetOf(tc agentAPI.ToolCall) string {
	return strings.TrimSpace(strArg(tc, "channel_target"))
}

func intArg(tc agentAPI.ToolCall, key string) int {
	switch v := tc.Arguments[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// executeNotifyParent 是子的"主动向父发消息"（父侧阶梯 = **L3 中断**）。
func (m *M) ExecuteNotifyParent(tc agentAPI.ToolCall) string {
	return m.NotifyParentFrom(strArg(tc, "text"))
}

// executeInputchNote 是子"主动写入本轮 inputch 的处理信息"。
// 主动写过 ⇒ 本轮系统不再自动写（见 autoRecordInputch）。
func (m *M) ExecuteInputchNote(tc agentAPI.ToolCall) string {
	text := strArg(tc, "text")
	if text == "" {
		return "text 不能为空"
	}
	m.RecordInputchNote(text)
	return "已记录本轮 inputch 处理信息（本轮系统不会再自动写）"
}

// childInboundChannelHint 是给子看的"父会怎么把消息投给你"的提示（不参与调度）。
func childInboundChannelHint(a *agent.Agent) string { return "sub/" + string(a.ID()) }

// residentTempDir 返回某个驻留子 temp 存储所在目录（销毁时连同目录丢弃）。
func residentTempDir(tempPath string) string { return filepath.Dir(tempPath) }

var _ = agentIO.InputChannel{} // 保持 agentIO 依赖（工具面未来会用通道登记）
