package core

// 驻留子的**工具面**（对照设计 §7 控制面与 §8 处理表）。
//
// 单工具多动作：父侧一个 `resident_agents`（list/create/send/inspect/compress/reclaim/destroy），
// 子侧两个小工具：`notify_parent`（L3 主动汇报）与 `inputch_note`（主动写处理表）。

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
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

// executeResidentAgents 是父的驻留子控制面（单工具多动作）。
func (a *Agent) executeResidentAgents(tc agentAPI.ToolCall) string {
	switch action := strArg(tc, "action"); action {
	case "", "list":
		list := a.Residents()
		if len(list) == 0 {
			return "当前没有驻留子 agent。"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "驻留子 agent（%d 个）:", len(list))
		for _, r := range list {
			fmt.Fprintf(&b, "\n  - %s [%s] inputch=%v 轮次=%d 处理表=%d",
				r.ID, r.State, r.InputChs, r.Rounds, r.TableSize)
			if r.ContextFull {
				b.WriteString("  ⚠️ contextfull")
			}
		}
		return b.String()

	case "create":
		id := strArg(tc, "id")
		tempPath := strArg(tc, "temp_path")
		if tempPath == "" {
			anchor := a.dataDir
			if anchor == "" {
				// 兜底：从**主图库路径**推导（<data>/memory/graph.db ⇒ <data>）。
				// 为什么不静默失败：这条路径只在"配置漏接线"时走到，
				// 静默报错会让线上表现为"工具能调但永远建不出来"（实测就是这样）。
				if a.memory != nil && a.memory.Path() != "" {
					anchor = filepath.Dir(filepath.Dir(a.memory.Path()))
					log.Printf("[resident] data_dir not wired, falling back to main graph dir: %s", anchor)
				}
			}
			if anchor == "" {
				return "创建驻留子需要 data_dir 或显式 temp_path（内核未接线 DataDir）"
			}
			tempPath = filepath.Join(anchor, "residents", id, "graph.db")
		}
		info, err := a.SpawnResident(ResidentOptions{
			ID:             id,
			TaskPrompt:     strArg(tc, "task_prompt"),
			InputChs:       splitArg(strArg(tc, "input_chs")),
			AllowedOutputs: splitArg(strArg(tc, "allowed_outputs")),
			Capacity:       intArg(tc, "capacity"),
			TempPath:       tempPath,
		})
		if err != nil {
			return fmt.Sprintf("创建驻留子失败: %v", err)
		}
		return "已创建驻留子: " + MarshalResidentInfo(info)

	case "send":
		if err := a.SendToResident(strArg(tc, "id"), strArg(tc, "text")); err != nil {
			return fmt.Sprintf("发送失败: %v", err)
		}
		return "已发送（对子而言是 L4 中断）"

	case "inspect":
		id := strArg(tc, "id")
		if id == "" {
			return "inspect 需要 id（或先用 action=list）"
		}
		table, err := a.ResidentTable(id)
		if err != nil {
			return fmt.Sprintf("查看失败: %v", err)
		}
		var b strings.Builder
		fmt.Fprintf(&b, "驻留子 %s 的 inputch 处理表（%d 条）:", id, len(table))
		for _, r := range table {
			kind := "系统写"
			if r.Proactive {
				kind = "主动写"
			}
			fmt.Fprintf(&b, "\n  - [%s][%s] %s", r.InputCh, kind, r.Text)
		}
		if len(table) == 0 {
			b.WriteString("\n  （尚无记录）")
		}
		return b.String()

	case "compress":
		n, err := a.CompressResident(strArg(tc, "id"))
		if err != nil {
			return fmt.Sprintf("压缩失败: %v", err)
		}
		return fmt.Sprintf("已压缩子 agent 上下文（丢弃 %d 条旧事件，并发清理其 inputch 处理表）；子继续存在", n)

	case "reclaim":
		id := strArg(tc, "id")
		// 残余任务先由父**显式**决定保留还是丢弃（用户 2026-09-19 要求）。
		//
		// ★ 用 FinishResident 一把做完，不要拆成 ApplyResidual + ReclaimResident：
		// 拆开时"解冻到销毁"之间子会继续收新任务，那些任务的结果随子一起丢失
		// 且不留痕迹（详见 FinishResident 的说明）。
		// onDestroy 里做记忆收割：读子 temp 的活跃三元组并合入主记忆。
		// 必须在 Destroy 之前 —— Destroy 会关掉 temp（这正是原先
		// ReclaimResident 内部做的顺序）。
		var promoted int
		residual, rmsg, ferr := a.FinishResidentWith(id, residualPolicyOf(tc), channelDispositionOf(tc),
			func(rc *residentChild) error {
				p, herr := a.harvestResidentMemory(rc)
				if herr != nil {
					return fmt.Errorf("合入主记忆失败: %w", herr)
				}
				promoted = p
				return nil
			})
		if ferr != nil {
			return fmt.Sprintf("回收失败: %v", ferr)
		}
		return fmt.Sprintf("残余任务（%d 条）：%s\n已回收（temp 中选中的记录已合入主记忆，promoted=%d，该驻留子已取消）",
			residual, rmsg, promoted)

	case "destroy":
		id := strArg(tc, "id")
		// 同上：销毁前先把残余任务交出去，否则它们会随子一起无声消失。
		residual, rmsg, ferr := a.FinishResidentWith(id, residualPolicyOf(tc), channelDispositionOf(tc), nil)
		if ferr != nil {
			return fmt.Sprintf("销毁失败: %v", ferr)
		}
		return fmt.Sprintf("残余任务（%d 条）：%s\n已销毁并移除该驻留子", residual, rmsg)

	default:
		return fmt.Sprintf("未知 action=%q；可用：list | create | send | inspect | compress | reclaim | destroy", action)
	}
}

// reclaimKeepAll 是回收时的默认策略：把子 temp 的活跃记录全部纳入主记忆
// （"哪些纳入"由父的模型决定——这里给的是"全要"这一档）。
func reclaimKeepAll(_ []InputchRecord, triples []memory.Triple) []memory.Triple { return triples }

// residualPolicyOf 从工具参数读残余任务的处置策略（keep/drop）。
//
// 默认 keep：父没明确说丢时，一律转回自己而不是丢弃。
// "宁可多做一件，不可默默丢一条"——吞掉一条输入比多处理一条更糟。
func residualPolicyOf(tc agentAPI.ToolCall) ResidualPolicy {
	switch strings.ToLower(strArg(tc, "residual")) {
	case "drop", "discard":
		return ResidualDrop
	case "stash", "park":
		// 暂存到原通道，等通道被接回时放出。
		return ResidualStash
	case "takeover", "take_over", "take":
		return ResidualTakeover
	default:
		return ResidualKeep
	}
}

// channelDispositionOf 从工具参数读**通道**的处置方式（release/suspend/takeover）。
//
// 默认 release = 原有行为（归还未分配），保持向后兼容。
func channelDispositionOf(tc agentAPI.ToolCall) ChannelDisposition {
	switch strings.ToLower(strArg(tc, "channels")) {
	case "suspend", "park":
		return ChannelSuspend
	case "takeover", "take_over", "take":
		return ChannelTakeover
	default:
		return ChannelRelease
	}
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
func (a *Agent) executeNotifyParent(tc agentAPI.ToolCall) string {
	return a.notifyParentFrom(strArg(tc, "text"))
}

// executeInputchNote 是子"主动写入本轮 inputch 的处理信息"。
// 主动写过 ⇒ 本轮系统不再自动写（见 autoRecordInputch）。
func (a *Agent) executeInputchNote(tc agentAPI.ToolCall) string {
	text := strArg(tc, "text")
	if text == "" {
		return "text 不能为空"
	}
	a.recordInputchNote(text)
	return "已记录本轮 inputch 处理信息（本轮系统不会再自动写）"
}

// childInboundChannelHint 是给子看的"父会怎么把消息投给你"的提示（不参与调度）。
func childInboundChannelHint(a *Agent) string { return "sub/" + string(a.id) }

// residentTempDir 返回某个驻留子 temp 存储所在目录（销毁时连同目录丢弃）。
func residentTempDir(tempPath string) string { return filepath.Dir(tempPath) }

var _ = agentIO.InputChannel{} // 保持 agentIO 依赖（工具面未来会用通道登记）
