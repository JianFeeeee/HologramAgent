package core

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/document"
	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

type ToolResultItem struct {
	Name   string `json:"name"`
	Output string `json:"output"`
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

	// NoMemory 标记这条事件**不参与记忆计算与再归档**。
	//
	// ★ 为 doc_query 的回填而设（2026-10-22）。回填是把 L2 的历史事件
	// 按原时间戳放回 L1 —— 它们**已经**在记忆体系里了。若不加标记：
	//
	//	① 计算层：它们会被重新向量化、参与 Prune 打分 ⇒ 因为与当前输入
	//	   相关而倾向被保留，挤掉真正的新事件；
	//	② 归档层：Prune 会把它们**再次**归档回 L2 ⇒ 下次 doc_query
	//	   又召回同一批 ⇒ 块在 L1↔L2 之间反复搬迁，自激震荡。
	//
	// 语义与 ToolDef.NoMemory / ChannelDef.NoMemory 一致：
	// **原文保留在上下文里可读**，只是不进计算、不再产生新记忆。
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
	mu               sync.Mutex
	events           []*ContextEvent
	embedder         *memory.StaticEmbedder
	denseSpace       vector.MultimodalEmbedder
	savePath         string
	saveTimer        *time.Timer
	dirty            bool
	toolDefLookup    func(name string) *sdk.ToolDef
	channelDefLookup func(name string) (sdk.ChannelDef, bool)

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
	// 事件自带 NoMemory（doc_query 回填的历史事件）：不进计算层。
	// 原文仍在上下文里可读，只是不参与向量化/关键词/蒸馏。
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
			// ★ doc_query 属于这一类 —— 它的返回值含被召回事件的正文片段，
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
func (c *RelevanceContext) channelCleanerForDoc() document.ChannelCleaner {
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
func (c *RelevanceContext) Append(evt ContextEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	scrubEventSecrets(&evt)
	c.computeVector(&evt)
	c.events = append(c.events, &evt)

	c.save()
}

func (c *RelevanceContext) InsertByTimestamp(evt ContextEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	scrubEventSecrets(&evt) // 同 Append：咽喉处拦截
	c.computeVector(&evt)

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
func (c *RelevanceContext) Prune(currentInput string, topK int, docStore *document.Store) int {
	return c.PruneWithProtected(currentInput, topK, docStore, 0)
}

// PruneWithProtected 是带显式保护条数的 Prune。
func (c *RelevanceContext) PruneWithProtected(currentInput string, topK int, docStore *document.Store, pCount int) int {
	c.mu.Lock()
	defer c.mu.Unlock()

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
	if docStore != nil && len(archive) > 0 {
		// ★ 回填事件（NoMemory）**不再归档回 L2**。
		//
		// 这是自激震荡的根：doc_query 把事件从 L2 取到 L1，Prune 又把它们
		// 归档回 L2 —— 下一轮 doc_query 再召回同一批，块在两层间反复搬迁。
		// 它们本来就在记忆体系里（来源可能是 L2 或 L3），无需重新落库。
		filtered := archive[:0]
		skippedNoMemory := 0
		for _, s := range archive {
			if s.event != nil && s.event.NoMemory {
				skippedNoMemory++
				continue
			}
			filtered = append(filtered, s)
		}
		archive = filtered
		if skippedNoMemory > 0 {
			log.Printf("[agent] prune: 跳过 %d 条回填事件（NoMemory，它们在记忆体系里已有归属）", skippedNoMemory)
		}
	}
	if docStore != nil && len(archive) > 0 {
		entries := make([]document.ContextEntry, len(archive))
		for i, s := range archive {
			entries[i] = document.ContextEntry{
				Timestamp:   s.event.Timestamp,
				Source:      s.event.Source,
				Content:     s.event.Input,
				Response:    s.event.Response,
				ToolResults: convertToolResults(s.event.ToolResults),
				Blocks:      append([]memory.MemoryBlock(nil), s.event.Blocks...),
			}
		}
		doc, err := docStore.ContextToDoc("context_archived", entries, c.embedder, nil, c.toolOutputClean, c.channelCleanerForDoc())
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

func convertToolResults(items []ToolResultItem) []document.ToolResultItem {
	if items == nil {
		return nil
	}
	result := make([]document.ToolResultItem, len(items))
	for i, item := range items {
		result[i] = document.ToolResultItem{Name: item.Name, Output: item.Output}
	}
	return result
}
