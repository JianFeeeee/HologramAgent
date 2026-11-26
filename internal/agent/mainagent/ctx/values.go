package ctx

import (
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
	"github.com/JianFeeeee/HologramAgent/internal/vector"
)

// values.go —— **主 agent 上下文自己的**值类型（docs §0.3 铁律）。
//
// ★ 原先在 `agenttypes/contextvalues.go`（公共值层）。按「agent 间是隔离实例，
//   任何情况不允许共享」，这些归**用它的那个上下文实现**：
//   主 agent 的这份在这里，驻留子的那份在 resident 包里各自定义。
//   两份今天字段相同 —— 那是现状，不是允许共用的理由。

type ContextEvent struct {
	// ID 是事件的稳定标识。惰性生成：只有真的要挂媒体块时才赋值。
	//
	// 全量生成会让每条事件都多一个字段进 context.json，而绝大多数对话没有媒体。
	// omitempty 保证存量 context.json 读回来时该字段为空，不影响任何既有行为。
	ID        string    `json:"id,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	Source    string    `json:"source"`
	Input     string    `json:"input"`
	Response  string    `json:"response,omitempty"`

	// ReasoningContent 是本轮 assistant 的思考内容（thinking 模式的思维链）。
	//
	// ## 为什么必须存并必须回传（2026-11-11 生产故障）
	//
	// 生产报：
	//
	//	all 2 providers failed, last error: api error 400:
	//	"The reasoning_content in the thinking mode must be passed back to the API."
	//
	// DeepSeek 官方规则（api-docs.deepseek.com/guides/thinking_mode）：
	//
	//	后续请求是否要回传 reasoning_content，取决于请求是否带 tools：
	//	带 tools ⇒ **所有历史轮的 reasoning_content 都要传回**，并会拼进上下文
	//
	// 而内核**每一轮都带 tools**（工具定义常驻），所以这条必触发。
	//
	// 此前这里没有这个字段：resp.ReasoningContent 收到了（只用于发事件、
	// 显示思考过程），但**没进 L1**，下一轮 renderL1Messages 自然带不上
	// ⇒ 只要对话里出现过一次工具调用 + 思考，整个会话就永久 400
	// （上游多家 agent 项目都踩了同一个坑：langchain #37179、n8n #34924、
	//   opencode #24722、codex #24500）。
	//
	// 语义边界：这是**协议字段**，不是记忆内容 ——
	//   · 不向量化、不进 L2/L3 蒸馏（思维链不是事实）
	//   · 只在渲染 provider 消息时回填
	//   · omitempty：存量 context.json 读回来为空串，不伪造
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolsUsed        []string         `json:"tools_used,omitempty"`
	ToolResults      []ToolResultItem `json:"tool_results,omitempty"`

	// ToolCalls 是本次事件里**每一次**工具调用的完整记录（含参数与配对键）。
	//
	// 与 ToolsUsed 的关系：ToolsUsed 是「用过的工具名」的便捷视图（保留是为了
	// 存量读取与统计），ToolCalls 才是配对与页化所需的事实源。
	// 两者都由 appendTurnToolCall 同步维护，不手写其中一份。
	ToolCalls []ToolCallItem `json:"tool_calls,omitempty"`

	// InactivePages 是**本次渲染**中不参与渲染的页序号。
	//
	// ★ 它是渲染期状态，不是记忆属性（用户裁定，2026-10-29）：
	//
	//	"inactive 是在 context 渲染时的标记，被淘汰到 doc 后还保留干啥？
	//	 甚至可以说 inactive 是每次 prune 后被重置的"
	//
	// 因此：
	//   · **不持久化**（json:"-"）—— 重启后由本轮渲染重新判定
	//   · **不迁移到 L2** —— 淘汰（归档）意味着整帧离开 L1，
	//     剔除本来就是"在有限窗口里少渲染几页"的临时手段，
	//     帧都走了再记着"它当时少渲染了哪页"毫无意义
	//   · **每次 Prune 后清空** —— 新一轮窗口重新规划，旧标记失效
	//
	// 语义：剔除 = 标注，不是删除。数据仍在（ToolCalls/ToolResults/Blocks），
	// 只是不渲染成消息。清空本列表即恢复渲染。
	//
	// 为什么最小单位是页而不是块：渲染时 `assistant(tool_calls)` 后面必须
	// 紧跟**配对齐全**的 `tool` 消息，否则上游直接 400（task.go:160 有生产事故
	// 记录）。所以只能整页剔除 —— 剔掉 call 却留下 result 就是造一个必崩的帧。
	//
	// 页序约定（与 coldcontext 的 DocEvent 分页一致）：
	//   1        = llm_round（用户输入 ↔ 模型输出）
	//   2..n+1   = tool_round（第 i 个 ToolCalls ↔ 配对结果）
	InactivePages []int `json:"-"`

	// ToolResultStores 是本事件捕获的「超大工具结果」落盘记录。
	//
	// ★ 与 Blocks 同一套处置：正文随事件目录持久化，**不维护引用账本**，
	//   存活期由事件的存活期决定（事件离开 L1 ⇒ 正文目录一并删除）。
	//   见 toolresult_store.go 的说明。
	//
	// 这里只存元信息（工具名、文件名、总字节/行数），正文在
	// <context.json 同级>/toolresults/<eventID>/tool_N.txt —— 因为
	// context.json 每 5s 全量重写一次，内联大正文会让每次 flush 都写几 MB。
	ToolResultStores []ToolResultStore `json:"tool_result_stores,omitempty"`

	// NoMemory 标记这条事件**不参与记忆计算**（不向量化、不提关键词、不蒸馏），
	// 但**照常参与归档**（离开 L1 时会写回 L2）。
	//
	// ★ 与 ToolDef.NoMemory 语义一致：只影响计算层，不影响存储层。
	//
	// ★★ 为什么不能顺手把归档也关掉（2026-10-22 踩过的坑）：
	//
	//	coldcontext_query 的 Consume 已经**从 L2 删除**了原文。若回填事件在 L1
	//	被裁掉时又不归档，它就**三层都不在**了 —— 内容被永久丢弃。
	//	L1 是它最后的落脚点，必须能落回去。
	//
	//	所以"回填事件不再归档"是错的。它确实该重新归档（那才是迁移闭环），
	//	该抑制的只是"重新计算向量/再次进蒸馏"—— 那才是自激的入口。
	NoMemory bool `json:"no_memory,omitempty"`
	// --- 原生多模态记忆 ---
	// 一等记忆块：块本身随事件在层间迁移，身份不变，不建引用计数。
	Blocks   []model.MemoryBlock `json:"blocks,omitempty"` // 一等记忆块（text/image/video/audio）
	Vector   vector.Vector       `json:"-"`                // 稀疏词向量（TF-IDF/fastText 空间）
	DenseVec []float64           `json:"-"`                // 稠密多模态向量（与媒体/文档共享空间）
	DenseFP  string              `json:"-"`                // DenseVec 所属统一空间指纹（缓存字段，不持久化）
}

type CompactPlan struct {
	// Old 是即将被压掉的事件（按时间正序）。可能为空（没什么可压的）。
	Old []*ContextEvent
	// Keep 是保留的最近事件数（仅供日志/判据核对）。
	Keep int
	// Total 是压缩前的事件总数。
	Total int
	// PrefixText 是把 Old 渲染成供摘要用的纯文本。
	PrefixText string
}

type ColdRecallPlan struct {
	// Docs 是本次要回灌的文档（按相关度降序）。
	Docs []*model.ColdDoc
	// Evict 记录每篇要剔除的页序号（key = 文档 ID）。
	//
	// 空 map 表示无需剔除（整批装得下）。
	Evict map[string][]int
	// OriginalNeed 是剔除**前**整批估算需要的 token（用于日志对比）。
	OriginalNeed int
	// Need 是剔除后（或无需剔除时）的 token 需求。
	Need int
	// Budget 是可用预算。
	Budget int
	// Over 表示是否发生了剔除。
	Over bool
}

type ToolResultStore struct {
	// Tool 是产出这条结果的工具名（模型据此判断"是哪个工具给的"）。
	Tool string `json:"tool"`
	// File 是正文相对事件目录的文件名（如 tool_0.txt），不含绝对路径——
	// 数据目录整体搬迁后仍然有效。
	File string `json:"file"`
	// TotalBytes / TotalLines 供模型规划怎么读（先读尾部？还是按行定位？）。
	TotalBytes int `json:"total_bytes"`
	TotalLines int `json:"total_lines"`
	// TruncatedBytes 是**没有**落盘的字节数。非 0 表示落盘时触到了硬上限，
	// 必须让模型知道（否则它以为全文就这么长）。默认 0。
	TruncatedBytes int `json:"truncated_bytes,omitempty"`
}

// ToolResultItem 是一条工具结果（本上下文自己的那份）。
type ToolResultItem struct {
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name"`
	Output string `json:"output"`
}

// ToolCallItem 是一次工具调用的完整记录（含参数）。
type ToolCallItem = model.ToolCallItem
