package core

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// ToolResultItem 是一条工具调用结果。
//
// ★ 2026-10-29 补 CallID：此前只有 Name + Output，于是**同名工具的多次调用
// 无法配对**（两个 qq_get_history 分不清谁是谁），而配对是渲染正确性的前提：
// assistant(tool_calls=[c1..cn]) 后面必须紧跟配对齐全的 tool 消息，
// 否则上游直接 400（insufficient tool messages）。
//
// 生产数据实证（改动前）：tool_results 的键只有 ["name","output"]，
// 参数完全没有留存 —— 这意味着回灌后无法重建"当时到底调了什么"。
type ToolResultItem struct {
	// CallID 回指 ToolCall.ID，是 tool_call 与 tool_result 的配对键。
	//
	// 空值表示旧数据（未记录 CallID），渲染时按位置配对（既有行为）。
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name"`
	Output string `json:"output"`
}

// ToolCallItem 是一次工具调用的**完整记录**（含参数）。
//
// ★ 为什么必须记录参数（2026-10-29 还技术债）
//
// 此前事件里只有 `ToolsUsed []string`（工具名列表）。后果：
//
//	· 回灌后无法知道"当时用**什么参数**调用的"，只能看到结果
//	· 页化（段页式）无法实现 —— 「toolcall 与其 result 配对成一页」
//	  要求 call 侧有完整内容，只有名字的话那页是半空的
//	· 审计/复盘时"这个工具为什么返回这个"无从追溯
//
// 参数本身是模型给出的，正常是别名（sec_xxx），但仍可能被直接传明文
// （插件工具），所以落盘前必须过 scrubEventSecrets（与 Input/Response 同一条闸）。
type ToolCallItem struct {
	// CallID 是本次调用的唯一标识（来自 provider，缺失时内核补一个）。
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name"`
	// Arguments 是解析后的参数。
	Arguments map[string]interface{} `json:"arguments,omitempty"`
	// RawArguments 是模型给出的原始 JSON 字符串，保留它是为了诊断
	// "参数解析失败/被截断"这类问题（Arguments 无法表达残缺）。
	RawArguments string `json:"raw_arguments,omitempty"`
}

type ContextEvent struct {
	// ID 是事件的稳定标识。惰性生成：只有真的要挂媒体块时才赋值。
	//
	// 全量生成会让每条事件都多一个字段进 context.json，而绝大多数对话没有媒体。
	// omitempty 保证存量 context.json 读回来时该字段为空，不影响任何既有行为。
	ID          string           `json:"id,omitempty"`
	Timestamp   time.Time        `json:"timestamp"`
	Source      string           `json:"source"`
	Input       string           `json:"input"`
	Response    string           `json:"response,omitempty"`
	ToolsUsed   []string         `json:"tools_used,omitempty"`
	ToolResults []ToolResultItem `json:"tool_results,omitempty"`

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
	Blocks   []memory.MemoryBlock `json:"blocks,omitempty"` // 一等记忆块（text/image/video/audio）
	Vector   vector.Vector        `json:"-"`                // 稀疏词向量（TF-IDF/fastText 空间）
	DenseVec []float64            `json:"-"`                // 稠密多模态向量（与媒体/文档共享空间）
	DenseFP  string               `json:"-"`                // DenseVec 所属统一空间指纹（缓存字段，不持久化）
}

const contextFlushInterval = 5 * time.Second

type RelevanceContext struct {
	mu         sync.Mutex
	events     []*ContextEvent
	embedder   *memory.StaticEmbedder
	denseSpace vector.MultimodalEmbedder
	savePath   string
	// pendingToolResults 存放"已落盘但事件还没进 L1"的超长工具结果记录。
	//
	// 为什么需要：同一帧可能先落盘、后 append 事件（task.go 的顺序）。
	// 没有它，句柄会在事件进入前悬空，模型读到的就是"未找到"。
	// 事件 append 后由 claimPendingToolResults 认领并清空。
	pendingToolResults map[string][]ToolResultStore
	saveTimer          *time.Timer
	dirty              bool
	toolDefLookup      func(name string) *sdk.ToolDef
	channelDefLookup   func(name string) (sdk.ChannelDef, bool)

	// protectedCount 是裁剪时**无条件保留**的最近事件条数。
	//
	// 为何要可调：它决定「近处信息」与「向量检索」的权重 ——
	// 条数大则不容易丢近处，小则更依赖检索准确度。不同窗口/不同用法
	// （如长期助手 vs 短任务）适合的值不同，所以从 core.agent.context.* 传入。
	// 零值 ⇒ defaultProtectedCount（与历史硬编码 10 一致）。
	//
	// ★ 它现在是**上限**而非固定值：实际保护条数由 effectiveProtectedCount
	// 按预算收紧（见该函数）。原因见那里。
	protectedCount int
}

// effectiveProtectedCount 返回本次裁剪**实际**保护多少条。
//
// ★ 为什么不能固定用 protectedCount（T10c 实测踩出来的自相矛盾）：
// 保护条数 × 单条事件平均 token 可能**超过整个 ContextTokens 预算**——
// 实测 HA 在 50k 窗口下单条事件约 5800 token（工具回灌型），而预算约 40000，
// 于是「钉住 10 条」本身就装不下。后果是裁剪无解：每次只能裁掉零星1-2 条
// 就撞到 protected 下限，积累量仍超页 ⇒ 每轮触发两次超页 ⇒ 两轮后恢复预算
// 耗尽 ⇒ 任务直接终止（实测轮5/6 的 prompt=0）。召回率因此从 44% 掉到 22%。
//
// 现在的规则：**保护上限仍由配置给，但不得超过预算能容纳的条数**。
// budgetCap = (ContextTokens 预算) / (单条平均 token)。取min(配置值, budgetCap)。
// 于是：
//   - 小窗口 / 大事件 ⇒ 自动收紧（宁可有取舍，也不让裁剪无解）；
//   - 大窗口（如 1M）或小事件 ⇒ 仍用满配置值 —— 这正是「1M 下这套调度器
//     能更好」的实现方式：预算越大，可保护的近处越多。
//
// 下限保1：protected=0 会让「最近 N 条一定在候选里」这条保证消失，
// 而 Prune 的 protected 切片是 `events[len-protected:]` —— 0 值会切出空切片
// 后仍走 keep 逻辑，虽不panic，但等于放弃了近处保护的意义。
func (a *Agent) effectiveProtectedCount() int {
	cfgCap := defaultProtectedCount
	if n := a.context.ProtectedCount(); n > 0 {
		cfgCap = n
	}

	budget := a.computeTokenBudget()
	tokens := budget.ContextTokens
	if tokens <= 0 || a.context == nil || a.context.Len() == 0 {
		return cfgCap // 没有样本/预算，按配置来
	}

	avg := a.accumulatedTokens() / a.context.Len()
	if avg <= 0 {
		avg = defaultAvgEventTokens
	}

	budgetCap := tokens / avg
	if budgetCap < 1 {
		budgetCap = 1
	}
	if budgetCap < cfgCap {
		return budgetCap
	}
	return cfgCap
}

// singleEventFitsBudget 报告「单条事件是否就装不进预算」。
//
// 为什么单独判：effectiveProtectedCount 收紧到 1 条仍可能装不下 —— 当单条
// 事件本身就大于 ContextTokens 预算时（实测 T10c：工具回灌型事件可达 40026
// token 而预算只有 10667），裁剪在**任何**保护数下都无解。
//
// 此时正确做法不是继续收紧（收紧也没用），而是让调用方知道「裁剪帮不上忙」，
// 由超页处理走终止路径并报出真实原因，而不是反复裁剪直到预算耗尽。
func (a *Agent) singleEventFitsBudget() bool {
	if a == nil || a.context == nil || a.context.Len() == 0 {
		return true
	}
	avg := a.accumulatedTokens() / a.context.Len()
	if avg <= 0 {
		avg = defaultAvgEventTokens
	}
	return avg <= a.computeTokenBudget().ContextTokens
}

// ──────────────────────────────────────────────
// 段页式淘汰（P5，2026-10-29）
// ──────────────────────────────────────────────

// pageSimilarity 计算某一页与本轮输入的相似度。
//
// 页的相似度取**页内各侧的最大值**：call 侧里往往带着关键实体
// （文件路径、命令、代码），而 result 侧是回显 —— 只看一侧会漏。
func (c *RelevanceContext) pageSimilarity(e *ContextEvent, page int,
	queryDense []float64, queryFP string, queryVec vector.Vector) float64 {

	callText, resText := c.pageTexts(e, page)
	best := math.Inf(-1)

	score := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		// 与事件评分同构：同一统一空间内才比稠密余弦。
		if len(queryDense) > 0 && queryFP != "" && e.DenseFP == queryFP {
			if v, ok := c.vectorForText(text); ok && len(v) == len(queryDense) {
				if s := vector.DenseCosine(queryDense, v); s > best {
					best = s
				}
				return
			}
		}
		if s := vector.CosineSimilarity(queryVec, c.embedder.VectorizeClean(text)); s > best {
			best = s
		}
	}
	score(callText)
	score(resText)
	if math.IsInf(best, -1) {
		return 0
	}
	return best
}

// vectorForText 尝试为任意文本取稠密向量（缓存优先）。
//
// 页内文本（工具参数/结果）不是一等块，没有预存向量；
// 这里按需算，失败就返回 false 让调用方退回稀疏余弦。
func (c *RelevanceContext) vectorForText(text string) ([]float64, bool) {
	if c.denseSpace == nil || !c.denseSpace.Loaded() {
		return nil, false
	}
	v, err := c.denseSpace.VectorizeDense(text)
	if err != nil || len(v) == 0 {
		return nil, false
	}
	return v, true
}

// pageTexts 取某页的 call 侧与 result 侧正文（与 docBlocksForEntry 同构）。
func (c *RelevanceContext) pageTexts(e *ContextEvent, page int) (call, result string) {
	if e == nil {
		return "", ""
	}
	if page == 1 {
		return e.Input, e.Response
	}
	idx := page - 2 // 第 1 页是 llm_round
	if idx < 0 || idx >= len(e.ToolCalls) {
		return "", ""
	}
	tc := &e.ToolCalls[idx]
	call = tc.Name
	if len(tc.Arguments) > 0 {
		if b, err := json.Marshal(tc.Arguments); err == nil {
			call = tc.Name + " " + string(b)
		}
	} else if tc.RawArguments != "" {
		call = tc.Name + " " + tc.RawArguments
	}
	// 配对结果
	if tc.CallID != "" {
		for i := range e.ToolResults {
			if e.ToolResults[i].CallID == tc.CallID {
				return call, e.ToolResults[i].Output
			}
		}
	}
	for i := range e.ToolResults {
		if e.ToolResults[i].Name == tc.Name {
			return call, e.ToolResults[i].Output
		}
	}
	return call, ""
}

// PageCount 返回本事件的页数（1 个 llm_round + N 个 tool_round）。
func (c *RelevanceContext) PageCount(e *ContextEvent) int {
	if e == nil {
		return 0
	}
	return 1 + len(e.ToolCalls)
}

// evictPagesLocked 在单帧超预算时，按相似度剔除页（标注 inactive，不删数据）。
//
// ⚠️ 调用方**必须已持 c.mu**（它内部会 save/computeVector）。
//
// ## 用户裁定的规则
//
//	召回时对段内每个页计算相似度，单条段超限时，
//	按相似度"剔除"相似度最低的页（toolcall 与其 result），
//	实际标注为 inactive，不渲染为消息
//
// ## 三条不能违反的约束
//
//  1. **最小单位是页**：call 与 result 同进同出。只剔一侧会让渲染破配对，
//     上游立刻 400（这不是洁癖，是硬协议要求）。
//  2. **llm_round 永不剔除**：第 1 页是"用户说了什么 + 模型答了什么"。
//     剔掉它，这一帧就只剩工具流水，模型无法理解上下文在讲什么。
//  3. **剔除是标注**：数据留在原位，可随时恢复（InactivePages 清空即恢复）。
//
// 返回本次新剔除的页数。
func (c *RelevanceContext) evictPagesLocked(e *ContextEvent, budgetTokens int, query string) int {
	if e == nil || budgetTokens <= 0 || query == "" {
		return 0
	}
	evicted := 0
	for {
		total := eventTokens(*e)
		if total <= budgetTokens {
			break
		}
		// 候选：所有仍生效的工具页（第 1 页受保护）
		type cand struct {
			page  int
			score float64
		}
		var cands []cand
		for p := 2; p <= c.PageCount(e); p++ {
			if containsInt(e.InactivePages, p) {
				continue
			}
			cands = append(cands, cand{p, 0})
		}
		if len(cands) == 0 {
			// 只剩 llm_round 仍超限 —— 不能靠剔页解决，
			// 交给上层（按帧归档 / 超页恢复）。这里明确停手，不空转。
			break
		}

		queryDense, queryFP := c.queryDenseFor(query)
		queryVec := c.embedder.VectorizeClean(query)
		for i := range cands {
			cands[i].score = c.pageSimilarity(e, cands[i].page, queryDense, queryFP, queryVec)
		}
		sort.Slice(cands, func(i, j int) bool { return cands[i].score < cands[j].score })

		victim := cands[0].page
		e.InactivePages = append(e.InactivePages, victim)
		sort.Ints(e.InactivePages)
		evicted++
		log.Printf("[context] frame %s over budget (%d > %d): evicted page %d "+
			"(score=%.3f, %d pages remain active)",
			e.ID, total, budgetTokens, victim, cands[0].score, c.PageCount(e)-len(e.InactivePages))
	}
	if evicted > 0 {
		// 剔除改变了这一帧的渲染内容 ⇒ 向量必须重算。
		// 不重算的话，下一轮召回仍按"整帧含被剔页"的向量打分，
		// 剔除等于没生效（相似度还带着已隐藏内容的分量）。
		c.computeVector(e)
		c.save()
	}
	return evicted
}

// ──────────────────────────────────────────────
// 召回期的「先估算、再剔除」（用户裁定，2026-10-29）
// ──────────────────────────────────────────────

// estimateColdEventTokens 估算一条 coldcontext（L2）记录回灌成事件后占多少 token。
//
// ★ 为什么按"渲染后"算而不是按 JSON 体积：
// 回灌进 L1 的是**渲染出来的消息**（用户行 + 助手字 + 展开的工具行），
// 而 L2 的存储形态（块、字段）与它并不等长。
// 按存储体积估算会系统性偏离 —— 归档压缩过的文档会被低估，
// 于是"以为装得下、实际装不下"。
func estimateColdEventTokens(d *coldcontext.ColdDoc) int {
	if d == nil {
		return 0
	}
	if len(d.Events) == 0 {
		// 存量文档（无 Events）回灌成一条降级事件
		return EstimateTokens(d.Content) + 40
	}
	total := 0
	for i := range d.Events {
		ev := &d.Events[i]
		input, response, _ := ev.InOut()
		// 与 eventTokens 同构：input + response + 工具输出
		n := EstimateTokens(input) + EstimateTokens(response) + 40
		for _, tr := range ev.ToolResults {
			n += EstimateTokens(tr.Output)
		}
		// 工具页的 call 侧（参数）也要渲染
		for _, tc := range ev.ToolCalls {
			n += EstimateTokens(tc.Name)
			if len(tc.Arguments) > 0 {
				if b, err := json.Marshal(tc.Arguments); err == nil {
					n += EstimateTokens(string(b))
				}
			}
		}
		total += n
	}
	return total
}

// ColdRecallPlan 是 coldcontext 召回的执行计划：哪些整篇召回、哪些要剔页、剔哪些。
type ColdRecallPlan struct {
	// Docs 是本次要回灌的文档（按相关度降序）。
	Docs []*coldcontext.ColdDoc
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

// planColdRecall 估算整批 coldcontext 召回的 token，超预算则按页相似度剔除。
//
// ## 用户裁定的算法（逐字）
//
//	召回过程中要先估算召回后的 token，如果整段召回不超预算，则直接召回，
//	若超了则对段内每一页算相似度，将最不相似的几个页标记为 inactive，
//	直到不超预算
//
// ## 三个要害
//
//  1. **先估算再动手**：装得下就一个字都不剔。剔除是有代价的
//     （模型看到的是残缺上下文），不该在有余量时发生。
//  2. **跨帧剔除，全局取舍**：不是"每篇自己管自己装不装得下"，
//     而是把整批候选放在一个池子里，每次剔**全局最不相似**的那一页。
//     否则会出现「A 篇剔得只剩骨架、B 篇一字未动」的荒谬结果。
//  3. **第 1 页（llm_round）不参与剔除**：那是"用户说了什么+模型答了什么"，
//     剔掉这一帧就只剩工具流水，模型无法理解它在讲什么。
//
// 返回的计划由调用方执行（本函数**不改任何状态**，纯计算）。
func (c *RelevanceContext) planColdRecall(docs []*coldcontext.ColdDoc, query string, budget int) ColdRecallPlan {
	plan := ColdRecallPlan{Docs: docs, Budget: budget, Evict: map[string][]int{}}

	if len(docs) == 0 || budget <= 0 {
		return plan
	}
	// ① 估算整批
	for _, d := range docs {
		plan.Need += estimateColdEventTokens(d)
	}
	plan.OriginalNeed = plan.Need
	if plan.Need <= budget {
		return plan // 装得下：一个字都不剔
	}

	// ② 超预算：把所有「可剔的页」摊到一个池子里
	type pageCand struct {
		docIdx   int
		eventIdx int
		page     int
		tokens   int
		score    float64
	}
	queryDense, queryFP := c.queryDenseFor(query)
	queryVec := c.embedder.VectorizeClean(query)

	var pool []pageCand
	for di, d := range docs {
		if d == nil || len(d.Events) == 0 {
			continue // 存量文档没有页结构，无法剔
		}
		for ei := range d.Events {
			ev := &d.Events[ei]
			// 第 1 页（llm_round）永不可剔
			for page := 2; page <= 1+len(ev.ToolCalls); page++ {
				pool = append(pool, pageCand{
					docIdx:   di,
					eventIdx: ei,
					page:     page,
					tokens:   pageTokens(ev, page),
				})
			}
		}
	}
	if len(pool) == 0 {
		return plan // 没有可剔的页：交给上层（提示超限或整篇放弃）
	}

	// ③ 算相似度（只算一次，避免循环里重复向量化）
	for i := range pool {
		d := docs[pool[i].docIdx]
		pool[i].score = coldPageSimilarity(c, d, pool[i].eventIdx, pool[i].page,
			queryDense, queryFP, queryVec)
	}

	// ④ 从最不相似开始剔，直到装得下
	sort.Slice(pool, func(i, j int) bool { return pool[i].score < pool[j].score })
	need := plan.Need
	for _, p := range pool {
		if need <= budget {
			break
		}
		d := docs[p.docIdx]
		plan.Evict[d.ID] = append(plan.Evict[d.ID], p.page)
		need -= p.tokens
		plan.Over = true
	}
	plan.Need = need
	return plan
}

// recallBudget 返回本次召回可用的 token 预算。
//
// 取 ContextTokens 减去**当前时间线已占用**：
// 召回内容与已有事件要一起渲染进同一个窗口，
// 只看整窗会在"加上原有内容"之后才发现爆了。
//
// 下限保护：至少给 1/4 的 ContextTokens。
// 否则时间线已接近满窗时，预算变成 0 或负数 ——
// 那样**任何**召回都会被判超预算而全部剔空，等于召不回来。
func (a *Agent) recallBudget() int {
	if a == nil || a.context == nil {
		return 0
	}
	tb := a.computeTokenBudget()
	avail := tb.ContextTokens - a.accumulatedTokens()
	floor := tb.ContextTokens / 4
	if avail < floor {
		avail = floor
	}
	if avail < 0 {
		avail = 0
	}
	return avail
}

// countPlanPages 统计计划里被剔除的页数。
func countPlanPages(evict map[string][]int) int {
	n := 0
	for _, pages := range evict {
		n += len(pages)
	}
	return n
}

// pageTokens 估算一页渲染后的 token（call 侧 + result 侧）。
func pageTokens(ev *coldcontext.ColdEvent, page int) int {
	if page == 1 {
		in, resp, _ := ev.InOut()
		return EstimateTokens(in) + EstimateTokens(resp)
	}
	idx := page - 2
	if idx < 0 || idx >= len(ev.ToolCalls) {
		return 0
	}
	tc := &ev.ToolCalls[idx]
	n := EstimateTokens(tc.Name)
	if len(tc.Arguments) > 0 {
		if b, err := json.Marshal(tc.Arguments); err == nil {
			n += EstimateTokens(string(b))
		}
	}
	if res, ok := coldToolResult(ev, tc); ok {
		n += EstimateTokens(res)
	}
	return n
}

// coldToolResult 按 CallID（退化到名字）配对工具结果。
func coldToolResult(ev *coldcontext.ColdEvent, tc *coldcontext.ToolCallItem) (string, bool) {
	if tc.CallID != "" {
		for i := range ev.ToolResults {
			if ev.ToolResults[i].CallID == tc.CallID {
				return ev.ToolResults[i].Output, true
			}
		}
	}
	for i := range ev.ToolResults {
		if ev.ToolResults[i].Name == tc.Name {
			return ev.ToolResults[i].Output, true
		}
	}
	return "", false
}

// coldPageSimilarity 算 coldcontext 某一页与查询的相似度（页内两侧取最大值）。
//
// ★ 页由 (eventIdx, pageSeq) 唯一确定，**不能只用 pageSeq**。
//
//	第一版只传 page 序号、遍历所有事件取最大值，于是同一文档里
//	多个事件的"第 2 页"被混成一个分数（跨事件串味）。判据实测：
//	6 个工具页拿到完全相同的 0.5774 —— 那是第 1 页 llm_round 的分数
//	反复覆盖的结果，页与页之间完全没有区分度，剔除变成随机。
func coldPageSimilarity(c *RelevanceContext, d *coldcontext.ColdDoc, eventIdx, page int,
	queryDense []float64, queryFP string, queryVec vector.Vector) float64 {

	if d == nil || eventIdx < 0 || eventIdx >= len(d.Events) {
		return 0
	}
	ev := &d.Events[eventIdx]
	best := math.Inf(-1)
	score := func(text string) {
		if strings.TrimSpace(text) == "" {
			return
		}
		if len(queryDense) > 0 && queryFP != "" {
			if v, ok := c.vectorForText(text); ok && len(v) == len(queryDense) {
				if s := vector.DenseCosine(queryDense, v); s > best {
					best = s
				}
				return
			}
		}
		if s := vector.CosineSimilarity(queryVec, c.embedder.VectorizeClean(text)); s > best {
			best = s
		}
	}
	if page == 1 {
		in, resp, _ := ev.InOut()
		score(in)
		score(resp)
	} else {
		idx := page - 2
		if idx < 0 || idx >= len(ev.ToolCalls) {
			return 0
		}
		tc := &ev.ToolCalls[idx]
		score(tc.Name)
		if len(tc.Arguments) > 0 {
			if b, err := json.Marshal(tc.Arguments); err == nil {
				score(string(b))
			}
		}
		if res, ok := coldToolResult(ev, tc); ok {
			score(res)
		}
	}
	if math.IsInf(best, -1) {
		return 0
	}
	return best
}

// queryDenseFor 取查询的稠密向量（与 PruneWithProtected 同构）。
func (c *RelevanceContext) queryDenseFor(query string) ([]float64, string) {
	if c.denseSpace == nil || !c.denseSpace.Loaded() {
		return nil, ""
	}
	dv, err := c.denseSpace.VectorizeDense(query)
	if err != nil || len(dv) == 0 {
		return nil, ""
	}
	return dv, c.denseSpace.Fingerprint()
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// ProtectedCount 返回配置给的保护条数**上限**（实际值见 effectiveProtectedCount）。
func (c *RelevanceContext) ProtectedCount() int {
	if c == nil || c.protectedCount <= 0 {
		return defaultProtectedCount
	}
	return c.protectedCount
}

func NewRelevanceContext(savePath string, embedder *memory.StaticEmbedder) *RelevanceContext {
	rc := &RelevanceContext{
		embedder:       embedder,
		savePath:       savePath,
		protectedCount: defaultProtectedCount,
	}
	if savePath != "" {
		rc.load()
	}
	return rc
}

// SetDenseSpace 注入稠密多模态向量空间。配置后 L0 相关性裁剪可用稠密向量
// 余弦（与媒体检索、文档检索共享同一空间），未配置时退化到稀疏词向量。
//
// 注入时**回填已有事件**的稠密向量。为什么必须回填：NewRelevanceContext 先
// load()、再 SetDenseSpace，载入时 c.denseSpace 还是 nil，旧事件只算了稀疏
// 向量；若这里只赋值不回填，Prune 里旧事件因 DenseFP 为空、长度不符而全部
// 走稀疏余弦，新事件走稠密余弦 —— 同一次排序里两种尺度混排，谁留下谁归档
// 取决于事件新旧而非相关性。对齐 DocStore.BuildDenseIndex 的做法。
//
// 注意 DenseVec/DenseFP 刻意不持久化（json:"-"）：这是每次启动一次性重算的
// 缓存，不落盘，因此这里也不需要 Save。
func (c *RelevanceContext) SetDenseSpace(ds vector.MultimodalEmbedder) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.denseSpace = ds
	if ds == nil || !ds.Loaded() {
		return
	}
	fp := ds.Fingerprint()
	dim := ds.Dim()
	filled := 0
	for _, evt := range c.events {
		if evt == nil {
			continue
		}
		if evt.DenseFP == fp && len(evt.DenseVec) == dim {
			continue
		}
		c.computeVector(evt)
		filled++
	}
	if filled > 0 {
		log.Printf("[agent] context dense backfill: %d events", filled)
	}
}

func (c *RelevanceContext) SetToolDefLookup(fn func(name string) *sdk.ToolDef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolDefLookup = fn
}

func (c *RelevanceContext) SetChannelDefLookup(fn func(name string) (sdk.ChannelDef, bool)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.channelDefLookup = fn
}

func (c *RelevanceContext) load() {
	data, err := os.ReadFile(c.savePath)
	if err != nil {
		return
	}
	var events []*ContextEvent
	if err := json.Unmarshal(data, &events); err != nil {
		return
	}
	for _, evt := range events {
		c.computeVector(evt)
	}
	c.events = events
}

func textForVector(evt *ContextEvent, toolDefLookup func(name string) *sdk.ToolDef, channelDefLookup func(name string) (sdk.ChannelDef, bool)) string {
	// 事件自带 NoMemory（coldcontext_query 回填的历史事件）：不进计算层。
	//
	// 为什么回填事件不参与计算：它们的内容刚刚在 L2 里被向量化过一次，
	// 再算一次只会让"同一内容"在向量空间里被重复加权。但它们**照常归档**。
	if evt.NoMemory {
		return ""
	}
	var text string
	switch {
	case evt.Source == "agent" && evt.Response != "":
		text = evt.Response
	case evt.Source == "cold_storage":
		text = evt.Input + " " + evt.Response
	default:
		text = evt.Input
	}

	// 计算层：应用输入通道的 Cleaner（不改原文，仅在计算层清洗）
	if channelDefLookup != nil {
		if chDef, ok := channelDefLookup(evt.Source); ok && chDef.Cleaner != nil {
			text = chDef.Cleaner(text)
		}
		if chDef, ok := channelDefLookup(evt.Source); ok && chDef.NoMemory {
			return ""
		}
	}

	// 计算层：附加工具输出，NoMemory 跳过，其余经 Cleaner 过滤
	{
		noMemory := make(map[string]bool)
		for _, tr := range evt.ToolResults {
			// ① 插件工具（含 proc 桥）：走 StageHost 的 ToolDef 声明
			if toolDefLookup != nil {
				if def := toolDefLookup(tr.Name); def != nil && def.NoMemory {
					noMemory[tr.Name] = true
					continue
				}
			}
			// ② 内置工具（core 自己的 switch 分派）：走包级声明表。
			//
			// ★ coldcontext_query 属于这一类 —— 它的返回值含被召回事件的正文片段，
			//   而那些正文已在记忆体系里（来自 L2/L3）。若返回值再参与向量化
			//   与蒸馏，就是把同一内容重新注入记忆，与「回填事件不再归档」
			//   构成同一个自激回路的两个入口。
			if noMemoryOf(tr.Name) {
				noMemory[tr.Name] = true
			}
		}
		for _, tr := range evt.ToolResults {
			if noMemory[tr.Name] {
				continue
			}
			cleaned := tr.Output
			if toolDefLookup != nil {
				if def := toolDefLookup(tr.Name); def != nil && def.Cleaner != nil {
					cleaned = def.Cleaner(cleaned)
				}
			}
			text += " " + cleaned
		}
	}

	return memory.CleanText(text)
}

// toolOutputClean 根据工具定义的 NoMemory/Cleaner 清洗输出，用于计算层。
// 返回 "" 表示跳过（NoMemory），否则返回清洗后文本（Cleaner 或原文）。
func (c *RelevanceContext) toolOutputClean(name, output string) string {
	if c.toolDefLookup == nil {
		return output
	}
	def := c.toolDefLookup(name)
	if def == nil {
		return output
	}
	if def.NoMemory {
		return ""
	}
	if def.Cleaner != nil {
		return def.Cleaner(output)
	}
	return output
}

// inputChannelClean 根据输入通道的 Def 清洗输入文本，用于计算层。
func (c *RelevanceContext) inputChannelClean(source, input string) string {
	if c.channelDefLookup == nil {
		return input
	}
	chDef, ok := c.channelDefLookup(source)
	if !ok {
		return input
	}
	if chDef.Cleaner != nil {
		return chDef.Cleaner(input)
	}
	return input
}

// channelCleanerForDoc 返回 ChannelCleaner，使 document 包在存档时能按来源查找 Cleaner。
func (c *RelevanceContext) channelCleanerForDoc() coldcontext.ChannelCleaner {
	if c.channelDefLookup == nil {
		return nil
	}
	return func(source string) func(string) string {
		chDef, ok := c.channelDefLookup(source)
		if !ok {
			return nil
		}
		return chDef.Cleaner
	}
}

func (c *RelevanceContext) computeVector(evt *ContextEvent) {
	text := textForVector(evt, c.toolDefLookup, c.channelDefLookup)
	// 稀疏向量始终计算（TF-IDF/fastText，退化时仍可用）
	if text != "" {
		evt.Vector = c.embedder.Vectorize(text)
	}
	// 稠密向量：文本向量 ⊕ 本事件持有的一等记忆块媒体向量（同一统一空间）。
	// 只有媒体的输入（无文本）也要有可比较的坐标，因此不再按 text=="" 提前返回。
	if c.denseSpace != nil && c.denseSpace.Loaded() {
		fp := c.denseSpace.Fingerprint()
		var parts [][]float64
		if text != "" {
			if dv, err := c.denseSpace.VectorizeDense(text); err == nil && len(dv) > 0 {
				parts = append(parts, dv)
			}
		}
		for _, b := range evt.Blocks {
			// 只融合同指纹的块向量：另一套坐标系的向量混进来会算出
			// 两边都不像的方向。
			if len(b.Vector) > 0 && b.Fingerprint == fp {
				parts = append(parts, b.Vector)
			}
		}
		evt.DenseVec = vector.FuseVectors(parts...)
		evt.DenseFP = fp
	}
}

func (c *RelevanceContext) Save() error {
	if c.savePath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.savePath), 0755); err != nil {
		return err
	}
	data, err := json.Marshal(c.events)
	if err != nil {
		return err
	}
	return os.WriteFile(c.savePath, data, 0644)
}

// Append 追加一条事件。
//
// ★ 这里是**持久化记忆的唯一咽喉**：全部 5 处写 context 的调用都经过它
// （另一处 InsertByTimestamp 同样处理）。所以明文拦截放在这里，
// 而不是在每个调用点 —— 逐个补必漏（实测踩到：只拦了 f.Msgs，
// 而 f.CleanInput / f.ToolResults / f.Response 都是独立副本）。
// snapshotEvents 返回当前事件的浅拷贝（判据/诊断用；调用方不得修改元素）。
func (c *RelevanceContext) snapshotEvents() []*ContextEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*ContextEvent, len(c.events))
	copy(out, c.events)
	return out
}

func (c *RelevanceContext) Append(evt ContextEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	scrubEventSecrets(&evt)
	c.computeVector(&evt)
	c.events = append(c.events, &evt)

	// 认领"先落盘、后进 L1"的超长工具结果记录（已在锁内，直接操作）。
	//
	// 为什么挂在 Append 而不是各调用点：Append 是事件进入 L1 的唯一咽喉
	// （task.go 有三处调用），散点调用必然漏一条路径——那一条上的句柄
	// 就永久悬空（模型拿到 event_id 去读得到"未找到"）。
	c.claimPendingToolResultsLocked(&evt)

	c.save()
}

// claimPendingToolResultsLocked 是 claimPendingToolResults 的持锁版本，
// 供 Append/InsertByTimestamp 这类已持锁的路径调用。
func (c *RelevanceContext) claimPendingToolResultsLocked(evt *ContextEvent) {
	if c == nil || evt == nil || evt.ID == "" || len(c.pendingToolResults) == 0 {
		return
	}
	if sts, ok := c.pendingToolResults[evt.ID]; ok {
		evt.ToolResultStores = append(evt.ToolResultStores, sts...)
		delete(c.pendingToolResults, evt.ID)
		c.dirty = true
	}
}

func (c *RelevanceContext) InsertByTimestamp(evt ContextEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	scrubEventSecrets(&evt) // 同 Append：咽喉处拦截
	c.computeVector(&evt)
	c.claimPendingToolResultsLocked(&evt) // 同 Append：认领待登记的落盘记录

	idx := sort.Search(len(c.events), func(i int) bool {
		return c.events[i].Timestamp.After(evt.Timestamp)
	})

	c.events = append(c.events, nil)
	copy(c.events[idx+1:], c.events[idx:])
	c.events[idx] = &evt

	c.save()
}

func (c *RelevanceContext) save() error {
	if c.savePath == "" {
		return nil
	}
	if !c.dirty {
		c.dirty = true
		if c.saveTimer == nil {
			c.saveTimer = time.AfterFunc(contextFlushInterval, c.flush)
		} else {
			c.saveTimer.Reset(contextFlushInterval)
		}
	}
	return nil
}

func (c *RelevanceContext) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return
	}
	data, err := json.Marshal(c.events)
	if err != nil {
		return
	}
	if err := os.WriteFile(c.savePath, data, 0644); err != nil {
		return
	}
	c.dirty = false
}

// scoredEvent 是 Prune 里按相关度排序的事件。
//
// 提为包级类型：Prune 需要把待归档列表传给后续处理。
type scoredEvent struct {
	event *ContextEvent
	score float64
	idx   int
}

// pCount <= 0 时用protectedCount 上限。调用方（pruneByQuery）传
// effectiveProtectedCount，按预算收紧后的值。
func (c *RelevanceContext) Prune(currentInput string, topK int, coldcontextStore *coldcontext.Store) int {
	return c.PruneWithProtected(currentInput, topK, coldcontextStore, 0)
}

// resetInactivePagesLocked 清空所有事件的剔除标记（调用方须持锁）。
//
// 剔除是渲染期的临时决定，不是记忆属性 —— 见 ContextEvent.InactivePages 的注释。
func (c *RelevanceContext) resetInactivePagesLocked() {
	for _, e := range c.events {
		if e != nil && len(e.InactivePages) > 0 {
			e.InactivePages = nil
		}
	}
}

// PruneWithProtected 是带显式保护条数的 Prune。
func (c *RelevanceContext) PruneWithProtected(currentInput string, topK int, coldcontextStore *coldcontext.Store, pCount int) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	// ★ 每次 Prune 都重置剔除标记（用户裁定，2026-10-29）。
	//
	// 为什么必须重置：InactivePages 是"本次渲染在有限窗口里少渲染几页"的
	// 临时决定，它依赖当时的**会话窗口状态**（哪些帧在做、查询是什么、
	// 预算还剩多少）。Prune 之后这些都变了 —— 沿用旧标记就是把
	// 上一轮的预算压力带到这一轮，于是某个帧"永久性"缺几页。
	//
	// 放在函数最前面（包括提前返回的那条路径之前）：只要 Prune 被调用，
	// 就说明窗口被重新规划过，旧标记一律失效。
	c.resetInactivePagesLocked()

	if len(c.events) <= topK {
		return 0
	}

	if pCount <= 0 {
		pCount = c.ProtectedCount()
	}
	if pCount > len(c.events) {
		pCount = len(c.events)
	}
	protected := c.events[len(c.events)-pCount:]
	candidates := c.events[:len(c.events)-pCount]

	if len(candidates) == 0 {
		return 0
	}

	// 优先使用稠密向量余弦（与媒体/文档共享空间）；退化到稀疏词向量。
	var queryDense []float64
	useDense := false
	queryFP := ""
	if c.denseSpace != nil && c.denseSpace.Loaded() {
		if dv, err := c.denseSpace.VectorizeDense(currentInput); err == nil {
			queryDense = dv
			queryFP = c.denseSpace.Fingerprint()
			useDense = true
		}
	}
	queryVec := c.embedder.VectorizeClean(currentInput)

	scoredEvents := make([]scoredEvent, len(candidates))
	for i, evt := range candidates {
		var score float64
		// 只在同一统一空间内比稠密余弦：换了模型/维度后旧事件的向量
		// 属于另一个坐标系，拿来比会得到无意义的分数。
		if useDense && evt.DenseFP == queryFP && len(evt.DenseVec) == len(queryDense) {
			score = vector.DenseCosine(queryDense, evt.DenseVec)
		} else {
			score = vector.CosineSimilarity(queryVec, evt.Vector)
		}
		scoredEvents[i] = scoredEvent{event: evt, score: score, idx: i}
	}

	sort.Slice(scoredEvents, func(i, j int) bool {
		return scoredEvents[i].score > scoredEvents[j].score
	})

	keepCount := topK - len(protected)
	if keepCount < 0 {
		keepCount = 0
	}
	keep := scoredEvents
	if len(keep) > keepCount {
		keep = keep[:keepCount]
	}
	archive := scoredEvents[keepCount:]

	c.events = make([]*ContextEvent, 0, len(keep)+len(protected))
	for _, s := range keep {
		c.events = append(c.events, s.event)
	}
	c.events = append(c.events, protected...)

	sort.Slice(c.events, func(i, j int) bool {
		return c.events[i].Timestamp.Before(c.events[j].Timestamp)
	})

	archived := 0
	if coldcontextStore != nil && len(archive) > 0 {
		// ★ 回填事件（NoMemory）**照常归档** —— 这是数据不丢的关键。
		//
		// 曾经在这里跳过它们（理由是"防自激"），那是错的：
		// coldcontext_query 的 Consume 已经把 L2 副本删掉了，L1 是唯一副本。
		// 跳过归档 = 裁掉即丢失（L1、L2、L3 三层都不在）。
		//
		// 自激的入口不在"归档"这一侧，而在"重新计算向量"那一侧 ——
		// 那已经由 textForVector 里 `if evt.NoMemory { return "" }` 挡住。
	}
	if coldcontextStore != nil && len(archive) > 0 {
		entries := make([]coldcontext.ContextEntry, len(archive))
		for i, s := range archive {
			entries[i] = coldcontext.ContextEntry{
				Timestamp:   s.event.Timestamp,
				Source:      s.event.Source,
				Content:     s.event.Input,
				Response:    s.event.Response,
				ToolResults: convertToolResults(s.event.ToolResults),
				ToolCalls:   toolCallItemsToDoc(s.event.ToolCalls),
				Blocks:      append([]memory.MemoryBlock(nil), s.event.Blocks...),
			}
		}
		doc, err := coldcontextStore.ContextToColdContext("context_archived", entries, c.embedder, nil, c.toolOutputClean, c.channelCleanerForDoc())
		if err == nil && doc != nil {
			archived = len(entries)
			// 一等记忆块的迁移：块随归档事件离开 L0、进入 L2。
			// 迁移的是块本身（ID 不变、只换持有层），不是复制也不是保活引用；
			// 因此归档后清空源事件的块，确保同一块不同时留在两层。
			for _, s := range archive {
				if s.event != nil {
					s.event.Blocks = nil
				}
			}
			// 超长工具结果全文同样**跟随事件**离开 L1，不单独过期。
			//
			// 与 Blocks 一致：块迁移到 L2（块本身进 doc），而全文是纯现场
			// 物料、不进 doc，所以这里直接删目录。删除失败只在日志留痕
			// （readToolResultBody 会答复"已随事件淘汰"），不会静默成功。
			for _, s := range archive {
				if s.event != nil && s.event.ID != "" && len(s.event.ToolResultStores) > 0 {
					c.purgeToolResultDir(s.event.ID)
					s.event.ToolResultStores = nil
				}
			}
		}
	}

	c.save()

	return archived
}

func (c *RelevanceContext) Format() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.events) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("【近期事件】\n")
	for _, e := range c.events {
		sb.WriteString(fmt.Sprintf("[%s] %s: %s", e.Timestamp.Format("15:04:05"), e.Source, e.Input))
		if e.Response != "" {
			sb.WriteString(fmt.Sprintf(" → %s", truncateStr(e.Response, 80)))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func (c *RelevanceContext) Recent(n int) []ContextEvent {
	c.mu.Lock()
	defer c.mu.Unlock()

	if n <= 0 || n > len(c.events) {
		n = len(c.events)
	}
	result := make([]ContextEvent, n)
	for i, evt := range c.events[len(c.events)-n:] {
		result[i] = *evt
	}
	return result
}

// Blocks 返回当前上下文持有的一等记忆块（供跨层存活判定）。
// 迁移后源事件已被清空，因此这里只会拿到真正属于 L0 的块。
func (c *RelevanceContext) Blocks() []memory.MemoryBlock {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []memory.MemoryBlock
	for _, e := range c.events {
		out = append(out, e.Blocks...)
	}
	return out
}

// TrimKeepRecent 只保留最近 n 条事件，丢弃更旧的（返回丢弃条数）。
//
// 这是**压缩上下文**（保留语义）的机械原语：不归档、不写任何记忆，直接丢弃旧事件。
// 用于轻量内核（驻留子）：它没有 doc 记忆与记忆整理流水线，压缩只能是"保留最近的"。
func (c *RelevanceContext) TrimKeepRecent(n int) int {
	c.mu.Lock()
	if n < 1 {
		n = 1
	}
	if len(c.events) <= n {
		c.mu.Unlock()
		return 0
	}
	dropped := len(c.events) - n
	kept := make([]*ContextEvent, n)
	copy(kept, c.events[dropped:])
	c.events = kept
	c.dirty = true
	c.mu.Unlock()
	c.save()
	return dropped
}

func (c *RelevanceContext) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

// toolCallItemsToDoc 把 L1 的工具调用记录转成 L2 类型。
//
// ★ 名字刻意不与 process.go 的 convertToolCalls 冲突：那个是
// api.ToolCall → sdk.ToolCall（跨进程边界），本函数是 L1 → L2（跨记忆层）。
// 两者语义不同，共用名字会让调用点看错。
//
// 必须逐字段搬运（而不是忽略）：L2 缺参数的话，回灌后「这一页的 call 侧
// 是空的」，段页式的配对校验与页化都失效。
func toolCallItemsToDoc(in []ToolCallItem) []coldcontext.ToolCallItem {
	if len(in) == 0 {
		return nil
	}
	out := make([]coldcontext.ToolCallItem, len(in))
	for i, tc := range in {
		out[i] = coldcontext.ToolCallItem{
			CallID:       tc.CallID,
			Name:         tc.Name,
			Arguments:    tc.Arguments,
			RawArguments: tc.RawArguments,
		}
	}
	return out
}

// convertToolResults 把 L1 的工具结果转成 L2 类型。
//
// ★ CallID 必须搬（2026-10-29 修）。
//
// 此前只搬 Name/Output，于是**归档一次就永久丢掉配对键**：
// 召回回填后 ToolResults 的 CallID 全空，页化与配对校验都失效
// （端到端判据 TestSegPagingRoundTripsThroughL2 实测抓到了这个）。
//
// 这类错误的共同点是「加字段时只改结构、不改搬运函数」——
// 结构体里字段看起来都在，真跑一遍才发现在某一段路上被丢了。
func convertToolResults(items []ToolResultItem) []coldcontext.ToolResultItem {
	if items == nil {
		return nil
	}
	result := make([]coldcontext.ToolResultItem, len(items))
	for i, item := range items {
		result[i] = coldcontext.ToolResultItem{
			CallID: item.CallID,
			Name:   item.Name,
			Output: item.Output,
		}
	}
	return result
}

// ── 超长工具结果全文的持久化（记忆面 IO 的合法落点）──────────────────
//
// 放这里而不是 toolresult_store.go：本包有架构守卫
// TestKernelHasNoMediaFileReading，内核除「记忆面持久化」（本文件）外不得
// 做本地文件读。工具结果全文落盘本质是"记住一份东西"，属同一职责。

// persistToolResult 把正文写入事件目录，返回元信息。
func (c *RelevanceContext) persistToolResult(evtID, tool, text string) (*ToolResultStore, error) {
	if c == nil {
		return nil, fmt.Errorf("toolresult: context 为空")
	}
	if evtID == "" {
		return nil, fmt.Errorf("toolresult: evtID 为空（调用方须先分配事件 ID）")
	}
	base := filepath.Join(filepath.Dir(c.savePath), toolResultDirName, evtID)
	if err := os.MkdirAll(base, 0755); err != nil {
		return nil, fmt.Errorf("toolresult: 建目录 %s: %w", base, err)
	}

	name := fmt.Sprintf("tool_%d.txt", nextToolResultSeq(base))

	body := text
	truncated := 0
	if len(body) > toolResultStoreHardCap {
		truncated = len(body) - toolResultStoreHardCap
		body = body[:toolResultStoreHardCap]
	}
	if err := os.WriteFile(filepath.Join(base, name), []byte(body), 0644); err != nil {
		return nil, fmt.Errorf("toolresult: 写正文: %w", err)
	}

	return &ToolResultStore{
		Tool:           tool,
		File:           name,
		TotalBytes:     len(text),
		TotalLines:     strings.Count(body, "\n") + 1,
		TruncatedBytes: truncated,
	}, nil
}

// nextToolResultSeq 数出事件目录里已有的 tool_*.txt，返回下一个序号。
//
// 不用内存计数器：进程重启后计数器归零会覆盖旧文件。数目录是幂等的。
func nextToolResultSeq(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		var idx int
		if _, err := fmt.Sscanf(e.Name(), "tool_%d.txt", &idx); err == nil && idx >= n {
			n = idx + 1
		}
	}
	return n
}

// readToolResultBody 按字节区间读取正文。
//
// offset 从 0 开始；limit<=0 表示读到结尾。返回内容与「是否已到末尾」。
func (c *RelevanceContext) readToolResultBody(evtID string, st *ToolResultStore, offset, limit int) (string, bool, error) {
	if c == nil || st == nil {
		return "", true, fmt.Errorf("toolresult: 无记录")
	}
	if offset < 0 {
		offset = 0
	}
	p := filepath.Join(filepath.Dir(c.savePath), toolResultDirName, evtID, st.File)
	f, err := os.Open(p)
	if err != nil {
		// 事件已被淘汰时走到这里。给**明确**答复而不是静默空串：
		// 模型应当知道"这份结果已不可读"，而不是以为结果是空的。
		if os.IsNotExist(err) {
			return "", true, fmt.Errorf("toolresult: 该结果已随事件淘汰（%s）", st.File)
		}
		return "", true, fmt.Errorf("toolresult: 打开正文: %w", err)
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(int64(offset), 0); err != nil {
			return "", true, fmt.Errorf("toolresult: 定位 %d: %w", offset, err)
		}
	}
	if limit <= 0 {
		limit = st.TotalBytes - offset
	}
	if limit < 0 {
		limit = 0
	}
	buf := make([]byte, limit)
	n, rerr := io.ReadFull(f, buf)
	if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
		return "", true, fmt.Errorf("toolresult: 读取: %w", rerr)
	}
	out := string(buf[:n])
	atEnd := offset+n >= st.TotalBytes
	return out, atEnd, nil
}

// purgeToolResultDir 删除某事件的全文目录。
//
// 由 Prune 在事件离开 L1 时调用：全文跟随事件生命周期，不单独过期。
func (c *RelevanceContext) purgeToolResultDir(evtID string) {
	if c == nil || c.savePath == "" || evtID == "" {
		return
	}
	p := filepath.Join(filepath.Dir(c.savePath), toolResultDirName, evtID)
	if err := os.RemoveAll(p); err != nil {
		// 删不掉不影响正确性（readToolResultBody 会给"已淘汰"），但要留痕，
		// 否则残留会无声地占满磁盘。
		log.Printf("[agent] purge toolresult dir %s: %v", p, err)
	}
}
