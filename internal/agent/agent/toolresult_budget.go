package agent

import (
	"fmt"
	"log"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent/child"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
)

// 本文件实现「工具结果**只统计不裁剪**」（方案 B）。
//
// 现状与风险（已核实）：工具结果进 f.Msgs 时**没有任何长度上限**
// （task.go 直接 `Content: result`），内核也**不预检**是否超长 ——
// 超限由上游 API 报错。时间线那一侧有预算（ContextTokens = 0.8×窗口，
// 进消息前就裁过），但那只管 a.context 的历史事件，**不管单条工具结果**。
// ⇒ 一条巨大工具结果可能直接冲破预算，而内核不会提前发现。
//
// 为什么**不裁剪**（与方案 A 的取舍）：
//   · 截断会让模型拿到**残缺**信息，而截断位置由内核武断决定；
//   · 模型无法得知"这里被截断了"，会基于残缺数据下结论
//     —— 这与本仓反复吃亏的「静默降级」是同一族问题；
//   · 处置权应交给调度器/上层（可以告警、可以拒绝、可以让模型自己决定
//     换更小的查询），而不是内核单方面替模型决定。
//
// ⇒ 本文件只做两件事：**计数**与**报告**。

// 默认阈值：单条工具结果超过 1/8 目标预算即报告。
//
// 取 1/8 而非"整个预算"：一条结果吃掉全部预算时，上下文里其它内容
// （记忆召回、时间线、对话历史）就全被挤掉了 —— 那才是真正需要预警的
// 场景。具体数值可由配置覆盖（见 SetToolResultWarnTokens）。
const defaultToolResultWarnDivisor = 8

// toolResultReporter 报告一次「工具结果过大」。
// 抽成接口是为了让判据能观测报告内容，而不必去抓日志。
type toolResultReporter interface {
	report(tool string, tokens, budget int, msg string)
}

// logReporter 是默认实现：写日志。
//
// 为什么默认只记日志、不给模型发消息：给模型发"你刚才的输出太大了"
// 是在**已经花掉的 token 之上**再加一条 system 消息，且它对当前这轮
// 决策毫无帮助。真正需要处置的是**下一轮**的调度（是否还塞更多上下文）。
// 交由上层订阅日志或替换 reporter 决定。
//
// ★ 它持有 LogIdentity 而不是直接 log.Printf：报告要能看出**是谁**
// 产出了这条超大结果（主 agent / 驻留子 / rtchild 共用同一条执行路径）。
// 身份由基类补，这里只转交。
type logReporter struct {
	id *child.LogIdentity
}

func (r logReporter) report(tool string, tokens, budget int, msg string) {
	if r.id == nil {
		log.Printf("[agent] tool %s result is large: %d tokens (%.0f%% of budget %d) — %s",
			tool, tokens, percentOf(tokens, budget), budget, msg)
		return
	}
	r.id.Logf("tool %s result is large: %d tokens (%.0f%% of budget %d) — %s",
		tool, tokens, percentOf(tokens, budget), budget, msg)
}

func percentOf(v, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(v) * 100 / float64(total)
}

// checkToolResultSize 统计单条工具结果的大小，必要时报告。
//
// ⚠️ **只统计，不裁剪**（见文件头）。返回 true 表示「已报告过」。
func (a *Agent) checkToolResultSize(tool, result string) bool {
	if a == nil || result == "" {
		return false
	}
	tokens := kit.EstimateTokens(result)
	limit := a.toolResultWarnLimit()
	if tokens <= limit {
		return false // 正常，不打扰
	}
	// ⚠️ 文案**必须带工具名**：报告是给日志/状态面看的，不带名字就无法
	// 判断是哪个工具在稳定产出超大结果（判据 TestReportIsActionable 钉住）。
	msg := fmt.Sprintf("工具 %s 的结果约 %d token，超出单条上限 %d（占预算 %.0f%%）。"+
		"建议：收窄查询条件、分页取、或把大结果转存后只取摘要。**结果未被裁剪**，模型看到的是完整内容。",
		tool, tokens, limit, percentOf(tokens, limit))
	r := a.mainState().toolResultReporter
	if r == nil {
		r = logReporter{id: &a.logIdentity}
	}
	r.report(tool, tokens, limit, msg)
	return true
}

// toolResultWarnLimit 返回本 agent 的单条工具结果告警阈值。
func (a *Agent) toolResultWarnLimit() int {
	if a.mainState().toolResultWarnTokens > 0 {
		return a.mainState().toolResultWarnTokens
	}
	budget := a.computeTokenBudget()
	base := budget.ContextTokens
	if base <= 0 {
		base = budget.TargetUsage
	}
	if base <= 0 {
		base = 8192
	}
	return base / defaultToolResultWarnDivisor
}

// SetToolResultWarnTokens 覆盖告警阈值（0 = 用默认值）。
//
// 供部署方按模型窗口调整：窗口大的源（1M）用默认阈值，窗口小的源
// （32K）可能需要更小的单条上限。
func (a *Agent) SetToolResultWarnTokens(n int) { a.mainState().toolResultWarnTokens = n }

// noteOversizeTool 记账一次「过大工具结果」（只统计，不裁剪）。
func noteOversizeTool(f *TaskFrame, name string) {
	if f == nil {
		return
	}
	f.OversizeTools++
	for _, n := range f.OversizeToolNames {
		if n == name {
			return
		}
	}
	f.OversizeToolNames = append(f.OversizeToolNames, name)
}

// OversizeToolNames 返回出现过超大结果的工具名（去重、保序）。
func OversizeToolNames(f *TaskFrame) []string {
	if f == nil || len(f.OversizeToolNames) == 0 {
		return nil
	}
	out := make([]string, len(f.OversizeToolNames))
	copy(out, f.OversizeToolNames)
	return out
}

// oversizeHintFor 供状态面渲染用的一行摘要。
func oversizeHintFor(f *TaskFrame) string {
	if f == nil || f.OversizeTools == 0 {
		return ""
	}
	names := OversizeToolNames(f)
	return fmt.Sprintf("本任务有 %d 次超大工具结果（%s）——未被裁剪，但已占用大量上下文预算",
		f.OversizeTools, strings.Join(names, ", "))
}
