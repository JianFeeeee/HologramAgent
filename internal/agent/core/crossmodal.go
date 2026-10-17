package core

import (
	"fmt"
	"log"
	"sort"
	"strings"

	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/document"
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
			log.Printf("[crossmodal] 多模态文本编码失败: %v", err)
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
			log.Printf("[crossmodal] 媒体块检索失败: %v", err)
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

// crossModalMarkdown 把融合候选渲染成注入上下文的文本。
// 文档行给出摘要；媒体行只给 MIME + 短块 ID（不再有生成的描述）。
func (a *Agent) crossModalMarkdown(hits []CrossModalHit) string {
	if len(hits) == 0 {
		return ""
	}
	var lines []string
	for i, h := range hits {
		marker := ""
		switch {
		case h.DoubleHit:
			marker = "（图文双命中）"
		case h.Doc != nil:
			marker = "（文本命中）"
		case h.Media != nil:
			marker = "（视觉命中）"
		}
		parts := []string{fmt.Sprintf("[%d]", i+1)}
		if h.Doc != nil {
			parts = append(parts, h.Doc.Summary)
			if h.Doc.Source != "" {
				parts = append(parts, fmt.Sprintf("(来源:%s)", h.Doc.Source))
			}
		}
		if h.Media != nil {
			if line := blockLabel(h.Media); line != "" {
				parts = append(parts, line)
			}
		}
		parts = append(parts, fmt.Sprintf("相关度:%.2f%s", h.Fused, marker))
		lines = append(lines, strings.Join(parts, " "))
	}
	return "【跨模态相关记忆】\n" + strings.Join(lines, "\n")
}
