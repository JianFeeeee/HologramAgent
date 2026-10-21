package core

// 图记忆的**共同面**（设计 docs/zh/resident-subagent-design.md §16.0）。
//
// 只有两个方法 —— 因为按调用方实测分类后，`Agent.memory` 的 42 处使用里，
// 真正"根与子都要"的只有：
//
//	Recall  读（上下文检索 / memory_recall）
//	Commit  写（自动写入路径的图部分 / memory_commit）
//
// 其余 40 处全是**主 agent 整理记忆**与**记忆整理流水线**：
//   - 记忆整理流水线：distill.go 的 archive/review/merge 循环
//   - 记忆块 + 媒体桥：graphmedia.go / medialoop.go
//   - 记忆整理工具：memory_merge / memory_delete_entity / memory_block_merge /
//     memory_purge / memory_edit / memory_introspect
//
// 所以驻留子的轻量内核**不该有那些代码路径**：它用 `*memory.LightMemory` 接上 `graph`，
// 而 `a.memory`（整理面）保持 nil —— 既有的 22 处 `if a.memory != nil` 关卡
// 会自动把整理面全部禁掉，不需要写"每个方法都返回错误"的受限包装。

import "github.com/JianFeeeee/HologramAgent/internal/memory"

// isLightKernel 报告本 agent 是不是**轻量内核**（驻留子）。
//
// 轻量内核 = 传统上下文 + 图记忆（作用域化）：它**没有**动态上下文能力
// （预算裁剪 / 按相关度裁剪并向 doc 记忆归档），那些是父 agent 专属的。
func (a *Agent) isLightKernel() bool { return a != nil && a.parentID != "" }

// contextTokenBudget 返回拼装时间线时可用的 token 预算。
//
//	完整内核：用动态上下文算出来的 ContextTokens（按相关度/预算**策略性**裁时间线）
//	轻量内核：**用整个窗口** —— 传统上下文只受"模型能收多少"这个**硬上限**约束，
//	          不做任何策略性裁剪（不按相关度挑、不向 doc 记忆归档）；
//	          而且在撞到硬上限之前，contextfull（90% 窗口）已按 L4 上报父 agent
//	          决策（压缩/回收/销毁）—— 丢事件的决定权在父，不在内核。
func (a *Agent) contextTokenBudget(b TokenBudget) int {
	if a.isLightKernel() {
		if b.MaxContext > 0 {
			return b.MaxContext
		}
		return defaultMaxContextTokens
	}
	return b.ContextTokens
}

// GraphMemory 是任意 agent 都能用的图记忆面。
//
// 实现者：
//   - 根 agent：直接就是 *memory.GraphDB
//   - 驻留子：*memory.LightMemory（读 temp∪main，只写 temp）
type GraphMemory interface {
	// Recall 按关键词/种子实体召回（子的实现是两空间并集）。
	Recall(keywords []string, seedEntities []string, depth int, sessionFilter string) (*memory.RecallResult, error)
	// RecallSorted 是可指定呈现顺序的召回（相关性 / 时间倒序）。
	//
	// 刻意与 Recall 并存而不是改签名：Recall 有 6 个调用方（webui / cli /
	// healthcheck / sdk / 记忆通道注入），全部依赖默认相关性语义。
	// ★ fingerprint 是**向量空间隔离键**（2026-10-04）。
	//   它不在 SDK 的公开契约里，只在 core 内部这个接口上 ——
	//   加它是因为「兜底召回路径绕过向量空间隔离」是真回归
	//   （判据 TestMemoryRecall_指纹不匹配的块被跳过）。
	RecallSorted(keywords []string, seedEntities []string, depth int, sessionFilter, fingerprint string, mode memory.SortMode) (*memory.RecallResult, error)

	// Commit 写入三元组（子的实现只落自己的 temp 空间）。
	Commit(triples []memory.Triple, sessionID string, turnID int) (int, int, error)
}
