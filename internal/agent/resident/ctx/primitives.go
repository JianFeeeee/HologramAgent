package ctx

// primitives.go —— 上下文事件的**纯计算原语**（token 估算 / 页索引）。
//
// ## 为什么这些是本包的导出函数而不是 Agent 方法
//
// 它们是**上下文本身的属性**：一条事件占多少 token、工具结果怎么配对成页。
// 基座（`agent/overflow.go` 的超页数字）与类型（主 agent 的策略）都要用它们，
// 而实现只该有一份 —— 两份必然漂移，且漂移的后果是"渲染剔了、统计没剔"
// （实测：事件少 7 条、token 只降 10%，恢复预算 2/2 用尽）。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
)

// EventTokens 估算单条上下文事件的 token 占用。
//
// ★ ToolResults 必须计入 —— v4 与 T10 的 prompt 峰值（17 万~40 万 token）
// 几乎全部来自工具回灌，而 ContextEvent 里工具输出是**独立字段**，
// 不算它的话水位会返回 0，超页判据永远不触发。
//
// ★ 必须排除 InactivePages：段页式淘汰把某些工具页标成不可见，渲染时整页
// 不出现。若统计仍按全量算，「渲染的少了、统计的没少」⇒ 页剔了水位却不动
// ⇒ L4 空中断反复触发直到恢复预算耗尽。
//
// 判据口径：**统计视图必须等于渲染视图**。
func EventTokens(e ContextEvent) int {
	n := kit.EstimateTokens(e.Input) + kit.EstimateTokens(e.Response)
	if len(e.ToolResults) == 0 {
		return n
	}
	pageOf := ToolResultPageIndex(&e)
	for i, tr := range e.ToolResults {
		if i < len(pageOf) && ContainsInt(e.InactivePages, pageOf[i]) {
			continue
		}
		n += kit.EstimateTokens(tr.Output)
	}
	return n
}

// ToolResultPageIndex 把每个 ToolResult 映射到它所属的**页号**。
//
// 配对规则（CallID 优先、退化到名字、配不上记 0 永不剔除）只能有一份：
// 渲染侧与统计侧都用它，否则"哪一页被剔了"两边会各算一样。
//
// 页号 0 表示"没有对应的调用"（永不剔除）；其余从 2 开始（第 1 页是 llm_round）。
func ToolResultPageIndex(e *ContextEvent) []int {
	if e == nil || len(e.ToolResults) == 0 {
		return nil
	}
	out := make([]int, len(e.ToolResults))
	for i := range e.ToolResults {
		out[i] = 0
		for j := range e.ToolCalls {
			tc := &e.ToolCalls[j]
			hit := false
			if tc.CallID != "" && e.ToolResults[i].CallID != "" {
				hit = tc.CallID == e.ToolResults[i].CallID
			} else {
				hit = tc.Name == e.ToolResults[i].Name
			}
			if hit {
				out[i] = j + 2
				break
			}
		}
	}
	return out
}

// ContainsInt 报告切片里是否含某个值。
func ContainsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// DefaultAvgEventTokens 是"没有样本时"的单条事件 token 估计。
const DefaultAvgEventTokens = 2000
