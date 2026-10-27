package core

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"unicode"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/document"
)

// CrossModalHit 是跨模态检索融合后的一条候选。
//
// 统一的检索单元是**记忆块**（文本块与媒体块同构）。文本路命中文档；
// 视觉路命中媒体块——两者都是块，向量在同一统一空间里可直接比较。
//
// ★ 2026-10-11：不再有「CAS 全库」这个检索面。视觉路直接走
// memory.RecallBlocks（读块自己的向量），因此不存在「召回已无处可归的
// 内容」这回事——能召回的必然是被某层持有的块。
type CrossModalHit struct {
	Doc        *document.Doc       // 文本路命中的文档；视觉路命中时为 nil
	Media      *memory.MemoryBlock // 视觉路命中的媒体块；文本路命中时也可能带关联块
	MediaScore float64             // 视觉路原始 cosine（无则 0）
	DocScore   float64             // 文本路原始 cosine（无则 0）
	Fused      float64             // 归一化加权融合分，供最终排序
	// 该媒体同时被两路命中时 DoubleHit=true —— 双信号确认，应排在只被
	// 一路命中的候选之前。
	DoubleHit bool
}

// CrossModalFusionConfig 控制文本路与视觉路的融合行为。
type CrossModalFusionConfig struct {
	WeightText     float64 // 文本路融合权重（默认 0.5）
	WeightVisual   float64 // 视觉路融合权重（默认 0.5）
	DoubleHitBonus float64 // 双命中额外加分（默认 0.15）
	MinMaxEps      float64 // min-max 归一化除零保护（默认 1e-12）
}

var defaultFusionConfig = CrossModalFusionConfig{
	WeightText:     0.5,
	WeightVisual:   0.5,
	DoubleHitBonus: 0.15,
	MinMaxEps:      1e-12,
}

func (c CrossModalFusionConfig) textWeight() float64 {
	if c.WeightText <= 0 {
		return defaultFusionConfig.WeightText
	}
	return c.WeightText
}
func (c CrossModalFusionConfig) visualWeight() float64 {
	if c.WeightVisual <= 0 {
		return defaultFusionConfig.WeightVisual
	}
	return c.WeightVisual
}
func (c CrossModalFusionConfig) doubleHitBonus() float64 { return c.DoubleHitBonus }
func (c CrossModalFusionConfig) minMaxEps() float64 {
	if c.MinMaxEps <= 0 {
		return defaultFusionConfig.MinMaxEps
	}
	return c.MinMaxEps
}

// retrieveCrossModal 是跨模态并行检索的统一入口。
//
// 策略（两路并行，召回真正最相似的）：
//  1. 文本路：query 整段文本编码后查文档层（Doc.DenseVec 已融合其块的媒体向量），
//     命中文档若持有媒体块，直接带上该块。
//  2. 视觉路：query 经多模态模型文本编码 → 与**媒体块自己的向量**比余弦
//     （memory.RecallBlocks），覆盖文本向量没写到的视觉内容。
//     用的就是块向量——没有第二套向量、没有第二个检索面。
//  3. 融合：两条路候选各自 min-max 归一化到 [0,1]，加权求和后降序，取 topK。
//
// 多模态空间未配置时视觉路为空，退化为纯文本路。
func (a *Agent) retrieveCrossModal(query string, topK int, cfg CrossModalFusionConfig) []CrossModalHit {
	if topK <= 0 {
		topK = 5
	}
	per := topK * 2
	if per < 8 {
		per = 8
	}

	// ---- 文本路 ----
	var textHits []CrossModalHit
	if a.docStore != nil {
		for _, dh := range a.docStore.QueryScored(query, per) {
			hit := CrossModalHit{Doc: dh.Doc, DocScore: dh.Score}
			// 命中文档若持有一等记忆块，把首个媒体块一并带上（用于双命中标记）。
			for i := range dh.Doc.Blocks {
				if dh.Doc.Blocks[i].Modality != memory.BlockText {
					b := dh.Doc.Blocks[i]
					hit.Media = &b
					break
				}
			}
			textHits = append(textHits, hit)
		}
	}

	// ---- 视觉路（多模态文本编码 → 媒体块自己的向量）----
	var visualHits []CrossModalHit
	if a.multimodalSpace != nil && a.multimodalSpace.Loaded() && a.memory != nil {
		qv, err := a.multimodalSpace.VectorizeDense(query)
		if err != nil {
			log.Printf("[crossmodal] multimodal text encoding failed: %v", err)
		} else if hits, err := a.memory.RecallBlocks(memory.BlockRecallQuery{
			Vector:      qv,
			Fingerprint: a.multimodalSpace.Fingerprint(),
			TopK:        per,
			// ★ ExcludeText 必须传：视觉路要的是**媒体块**。
			//
			// 不传的后果（实测回归，块 union 化引入）：CAS 时代这里查的是
			// `FROM media`（只含媒体），结构上不可能被文本块挤掉。
			// 换成全局 RecallBlocks 后，同一个 chineseclip 空间里图像块的
			// 绝对 cosine（0.36~0.41）天然低于文本块（0.65~1.00），
			// per=200 时返回 1000 条里图像块 **0 条** —— 视觉路被饿死。
			// 下面的 `continue` 是防御，不是过滤手段：筛选必须发生在
			// 截断**之前**，否则名额已被文本块占满。
			ExcludeText: true,
		}); err != nil {
			log.Printf("[crossmodal] media block retrieval failed: %v", err)
		} else {
			for _, h := range hits {
				if h.Block.Modality == memory.BlockText {
					continue // 文本块由文本路负责，避免同一内容两路重复
				}
				b := h.Block
				visualHits = append(visualHits, CrossModalHit{
					Media: &b, MediaScore: h.Score,
				})
			}
		}
	}

	return fuseCrossModal(textHits, visualHits, topK, cfg)
}

// fuseCrossModal 把文本路与视觉路候选按各自归一化分融合排序。
func fuseCrossModal(textHits, visualHits []CrossModalHit, topK int, cfg CrossModalFusionConfig) []CrossModalHit {
	norm := func(hits []CrossModalHit, pick func(CrossModalHit) float64) []float64 {
		out := make([]float64, len(hits))
		if len(hits) == 0 {
			return out
		}
		maxV, minV := pick(hits[0]), pick(hits[0])
		for _, h := range hits[1:] {
			v := pick(h)
			if v > maxV {
				maxV = v
			}
			if v < minV {
				minV = v
			}
		}
		for i, h := range hits {
			v := pick(h)
			if maxV-minV < cfg.minMaxEps() {
				out[i] = 1
				continue
			}
			out[i] = (v - minV) / (maxV - minV)
		}
		return out
	}
	textN := norm(textHits, func(h CrossModalHit) float64 { return h.DocScore })
	visualN := norm(visualHits, func(h CrossModalHit) float64 { return h.MediaScore })

	byKey := make(map[string]*CrossModalHit)
	var keys []string
	key := func(h CrossModalHit) string {
		if h.Doc != nil {
			return "doc:" + h.Doc.ID
		}
		if h.Media != nil {
			return "media:" + h.Media.ID
		}
		return ""
	}

	for i, h := range visualHits {
		k := key(h)
		if k == "" {
			continue
		}
		clone := h
		clone.Fused = cfg.visualWeight() * visualN[i]
		byKey[k] = &clone
		keys = append(keys, k)
	}
	for i, h := range textHits {
		if h.Doc == nil {
			continue
		}
		if h.Media != nil {
			if ex, ok := byKey["media:"+h.Media.ID]; ok {
				ex.DoubleHit = true
				ex.Doc = h.Doc
				ex.Fused += cfg.textWeight()*textN[i] + cfg.doubleHitBonus()
				continue
			}
		}
		k := "doc:" + h.Doc.ID
		if ex, ok := byKey[k]; ok {
			ex.Doc = h.Doc
			ex.DoubleHit = false
			ex.Fused += cfg.textWeight() * textN[i]
			continue
		}
		clone := h
		clone.Fused = cfg.textWeight() * textN[i]
		byKey[k] = &clone
		keys = append(keys, k)
	}

	var merged []CrossModalHit
	for _, k := range keys {
		if c := byKey[k]; c != nil {
			merged = append(merged, *c)
		}
	}
	sort.SliceStable(merged, func(i, j int) bool {
		if merged[i].DoubleHit != merged[j].DoubleHit {
			return merged[i].DoubleHit
		}
		return merged[i].Fused > merged[j].Fused
	})
	if len(merged) > topK {
		merged = merged[:topK]
	}
	return merged
}

// crossModalMarkdown 把融合候选渲染成注入提示词的文本。
//
// ## 为什么改
//
// 旧输出是这样的：
//
//	【跨模态相关记忆】
//	[1] 来自 4 个来源的 61 条对话 (cold_storage, agentcli...) (来源:context_archived) 相关度:0.78（文本命中）
//
// 三个问题，都被生产日志证实：
//
//  1. 只有**统计描述**（几个来源几条对话），不是内容 —— 模型无法判断相关性；
//  2. **没有一个字**说"你可以把这些原文取回来" —— 模型看完只能忽略；
//  3. 来源写了 `context_archived` 这种内部标记，对模型无意义。
//
// 现在每条给出「时间 · 来源 · 正文开头」，并在块尾**明确告知 coldcontext_query 的作用**：
// 这才是让预填充真正被用起来的必要条件。
func (a *Agent) crossModalMarkdown(hits []CrossModalHit) string {
	if len(hits) == 0 {
		return ""
	}
	var lines []string
	for i, h := range hits {
		var parts []string
		parts = append(parts, fmt.Sprintf("[%d]", i+1))

		// 时间 + 来源：让模型知道"这是什么时候、谁的事"
		if h.Doc != nil {
			if h.Doc.HasEvents() {
				first := h.Doc.Events[0].Timestamp
				last := h.Doc.Events[len(h.Doc.Events)-1].Timestamp
				if first.Equal(last) {
					parts = append(parts, first.Format("2006-01-02 15:04"))
				} else {
					parts = append(parts, fmt.Sprintf("%s→%s",
						first.Format("01-02 15:04"), last.Format("01-02 15:04")))
				}
				// 真实来源（跳过内部归档标记，它对模型无意义）
				if src := firstRealSource(h.Doc.Events); src != "" {
					parts = append(parts, src)
				}
			} else {
				parts = append(parts, h.Doc.CreatedAt.Format("2006-01-02 15:04"))
			}
		}

		// 正文开头：真实内容，模型可据此判断相关性
		if h.Doc != nil {
			if head := docHeadlineForPrompt(h.Doc); head != "" {
				parts = append(parts, head)
			}
			// ★ 摘要行常常只是"我回复过什么"，不含被谈论的东西本身
			//   （实测：召回到「已回复老大：原来是「魔芋爽」这个谐音梗」——
			//   模型读得懂"我回复过"，但不知道梗是什么，只好答不知道）。
			//   所以补一段**正文/工具回执**摘录：真实内容就在那里。
			if body := docBodyExcerptForPrompt(h.Doc, 80); body != "" {
				parts = append(parts, "正文:"+body)
			}
		}
		// 媒体只报类型与数量，不编描述（描述式索引已在 v1.2.0 拆除）
		if h.Media != nil {
			label := string(h.Media.Modality)
			if h.Media.MIME != "" {
				label = h.Media.MIME
			}
			parts = append(parts, "["+label+"]")
		}
		if h.DoubleHit {
			parts = append(parts, "（图文双命中）")
		}
		parts = append(parts, fmt.Sprintf("相关度:%.2f", h.Fused))
		lines = append(lines, strings.Join(parts, " · "))
	}

	var b strings.Builder
	b.WriteString("【历史记忆（L2，自动预填充）】\n")
	b.WriteString("下面是按语义相似度从历史层挑出的相关片段 —— **不是当前对话**。\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n要其中某条的完整原文与当时过程：用 coldcontext_query，它会把那些事件" +
		"按原始时间戳插回你的对话时序。")
	return b.String()
}

// firstRealSource 取事件里第一个非内部标记的来源。
//
// `context_archived` 是内核归档动作的标记，不是"谁说的"，对模型无意义。
func firstRealSource(events []document.DocEvent) string {
	for _, ev := range events {
		if ev.Source != "" && ev.Source != "context_archived" && ev.Source != "cold_storage" {
			return ev.Source
		}
	}
	if len(events) > 0 {
		return events[0].Source
	}
	return ""
}

// docHeadlineForPrompt 取文档正文的第一句，作为注入行里的内容提示。
//
// 不截断成"摘要"：这里只是一行提示，完整内容在 L2 里、需要时用 coldcontext_query 取。
// docBodyExcerptForPrompt 取文档里**真实内容**的首段摘录。
//
// 顺序：事件正文（块里的文本）→ 工具回执 → 文档 Content。
// 摘要行（"已回复…"）不在这里出现——它由 docHeadlineForPrompt 负责，
// 两者互补：一个说"发生了什么"，一个说"内容是什么"。
func docBodyExcerptForPrompt(d *document.Doc, n int) string {
	if d == nil {
		return ""
	}
	// 最多取两段：事件正文（可能是用户原话）与工具回执（可能是被谈到的内容）。
	// 只取一段时，遇到"已回复…"这类复述就会把真实内容挤掉——实测正是这样：
	// 模型看到"原来是这个谐音梗"，却看不到梗是什么。
	var cands []string
	for _, ev := range d.Events {
		for _, b := range ev.Blocks {
			if b.Modality != "" && b.Modality != "text" {
				continue
			}
			if line := firstMeaningfulLineForPrompt(b.Text); line != "" {
				cands = append(cands, line)
			}
		}
		for _, tr := range ev.ToolResults {
			if line := toolResultExcerptForPrompt(tr.Output); line != "" {
				cands = append(cands, line)
			}
		}
		if len(cands) >= 2 {
			break
		}
	}
	if len(cands) == 0 {
		cands = append(cands, firstMeaningfulLineForPrompt(d.Content))
	}
	var uniq []string
	for _, c := range cands {
		if strings.TrimSpace(c) == "" {
			continue
		}
		dup := false
		for _, u := range uniq {
			if u == c {
				dup = true
				break
			}
		}
		if !dup {
			uniq = append(uniq, c)
		}
		if len(uniq) >= 2 {
			break
		}
	}
	return truncateRunesForPrompt(strings.Join(uniq, " ｜ "), n)
}

// toolResultExcerptForPrompt 把工具回执压成一行可读正文。
//
// qq_get_message 这类工具返回 JSON（{"content":"因为魔芋爽"}），直接截断会
// 让模型看到一堆花括号；这里优先取其中的字符串值，取不到才退回原文。
func toolResultExcerptForPrompt(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
		var v interface{}
		if err := json.Unmarshal([]byte(raw), &v); err == nil {
			if vals := collectJSONStrings(v, 0); len(vals) > 0 {
				return strings.Join(vals, " ")
			}
		}
	}
	return firstMeaningfulLineForPrompt(raw)
}

// collectJSONStrings 收集 JSON 里的字符串值（跳过键名与内部字段），深度受限。
func collectJSONStrings(v interface{}, depth int) []string {
	if depth > 3 {
		return nil
	}
	var out []string
	switch t := v.(type) {
	case map[string]interface{}:
		for _, k := range []string{"content", "text", "message", "msg", "summary", "data", "result"} {
			if sub, ok := t[k]; ok {
				out = append(out, collectJSONStrings(sub, depth+1)...)
			}
		}
	case []interface{}:
		for _, sub := range t {
			out = append(out, collectJSONStrings(sub, depth+1)...)
		}
	case string:
		if s := strings.TrimSpace(t); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func docHeadlineForPrompt(d *document.Doc) string {
	if d == nil {
		return ""
	}
	if d.HasEvents() {
		for _, ev := range d.Events {
			in, resp, _ := ev.InOut()
			for _, cand := range []string{in, resp} {
				if line := firstMeaningfulLineForPrompt(cand); line != "" {
					return truncateRunesForPrompt(line, 60)
				}
			}
		}
		return ""
	}
	return truncateRunesForPrompt(firstMeaningfulLineForPrompt(d.Content), 60)
}

func firstMeaningfulLineForPrompt(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		hasWord := false
		for _, r := range line {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				hasWord = true
				break
			}
		}
		if hasWord {
			return strings.Join(strings.Fields(line), " ")
		}
	}
	return ""
}

func truncateRunesForPrompt(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
