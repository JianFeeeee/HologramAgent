package core

import (
	"fmt"
	"log"
	"runtime/debug"
	"strings"
	"time"

	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/document"
	"github.com/JianFeeeee/HomeAgent/internal/memory/vector"
	"github.com/JianFeeeee/HomeAgent/internal/nlp"
)

type ConsolidationTask struct {
	Type   string      `json:"type"`
	Reason string      `json:"reason"`
	Data   interface{} `json:"data"`
}

func (a *Agent) enqueueConsolidationTask(task ConsolidationTask) {
	msg := fmt.Sprintf(
		"【记忆整理任务】\n类型: %s\n说明: %s\n\n注意：\n1. 仅使用 memory_merge 合并实体，或使用 memory_block_merge 标记不合并\n2. 不要使用 memory_commit 写入新的三元组\n3. 不要从这段任务文本中提取任何信息写入图库\n4. 只需要做出合并/不合并的判断并执行对应工具",
		task.Type, task.Reason,
	)
	a.injectSelf(msg)
	log.Printf("[agent] enqueued consolidation task: %s", task.Reason)
}

// ──────────────────────────────────────────────
// 四个独立心跳循环，各自拥有独立的 ticker 和配置
// ──────────────────────────────────────────────

// distillLoop 上下文裁剪（L1→L2），使用 distillInterval
func (a *Agent) distillLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] distillLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.distillLoop()
		}
	}()
	if a.docStore == nil {
		return
	}
	ticker := time.NewTicker(a.distillInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Printf("[agent] heartbeat distill tick")
			a.distillContext()
			a.autoReloadPlugins()
		case <-a.ctx.Done():
			return
		}
	}
}

// archiveLoop 冷文档归档（L2→L3），使用 archiveInterval
func (a *Agent) archiveLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] archiveLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.archiveLoop()
		}
	}()
	if a.memory == nil {
		return
	}
	ticker := time.NewTicker(a.archiveInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Printf("[agent] heartbeat archive tick")
			a.archiveColdDocs()
		case <-a.ctx.Done():
			return
		}
	}
}

// mergeLoop 实体合并检测（GraphDB → LLM 裁决），使用 mergeInterval
func (a *Agent) mergeLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] mergeLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.mergeLoop()
		}
	}()
	if a.memory == nil {
		return
	}
	ticker := time.NewTicker(a.mergeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Printf("[agent] heartbeat merge tick")
			a.detectEntityMerge()
			a.dedupeScenes()
		case <-a.ctx.Done():
			return
		}
	}
}

// reviewLoop 关系复审（GraphDB → ClearSentenceID → CleanupOrphanedSentences），使用 reviewInterval
func (a *Agent) reviewLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[agent] reviewLoop panic recovered: %v\n%s", r, debug.Stack())
			time.Sleep(time.Second)
			go a.reviewLoop()
		}
	}()
	if a.memory == nil {
		return
	}
	ticker := time.NewTicker(a.reviewInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Printf("[agent] heartbeat review tick")
			a.reviewRelations()
		case <-a.ctx.Done():
			return
		}
	}
}

// ──────────────────────────────────────────────
// 蒸馏逻辑
// ──────────────────────────────────────────────

func (a *Agent) distillContext() {
	if a.docStore == nil {
		return
	}
	n := a.context.Len()
	if n > a.maxContextSize*2 {
		archived := a.context.Prune("", a.maxContextSize, a.docStore)
		if archived > 0 {
			log.Printf("[agent] distill: pruned %d low-relevance events to document memory (total=%d)", archived, n)
		}
	}
}

// ──────────────────────────────────────────────
// 冷文档归档：docStore → GraphDB (L3→L4)
// ──────────────────────────────────────────────

func (a *Agent) archiveColdDocs() {
	if a.memory == nil {
		return
	}

	log.Printf("[agent] cold doc archival start")

	if a.indexer != nil {
		if err := a.indexer.Sync(); err != nil {
			log.Printf("[agent] indexer sync error: %v", err)
		}
	}

	// 场景记忆的「用进废退」：久未重现的关联按半衰期淡出。
	//
	// 不做衰减的后果不是"多记一点"，而是**注入预算被一次性巧合吃光**——
	// 场景是每轮都要注入的常驻内容，关联只增不减时，越老的库注入越糊。
	// 半衰期取 30 天：比"这个月没做过这类事"更久，避免把季节性的事误删。
	if a.memory != nil {
		if n, err := a.memory.DecaySceneRefs(30*24*time.Hour, 0.05); err != nil {
			log.Printf("[agent] scene decay error: %v", err)
		} else if n > 0 {
			log.Printf("[agent] 场景关联衰减：清理 %d 条长期未重现的引用", n)
		}
	}

	if a.docStore != nil {
		a.docStore.Reindex()
	}

	if a.docStore != nil {
		coldDocs := a.docStore.FindColdDocs(72*time.Hour, 2)
		for _, doc := range coldDocs {
			triples := docToTriples(doc, a.embedder)
			if len(triples) == 0 {
				continue
			}
			// ★★ 写入前取块数基线（2026-10-04）
			//
			// 停旧表双写后 Commit 的 ec/rc 恒为 0，
			// 「有没有真的写进图库」只剩块数这一个信号。
			blocksBefore, err := a.memory.MemoryBlockCount()
			if err != nil {
				log.Printf("[agent] doc→graph: %s 读块数失败，保留文档: %v", doc.ID, err)
				continue
			}

			ec, rc, blocks, err := a.commitTriplesWithMedia(triples, string(a.id)+"_doc_archival", 0, doc.Blocks)
			if err != nil {
				log.Printf("[agent] doc→graph archival error: %v", err)
				continue
			}

			// 归档的实质是「信息从 L2 搬到 L3」。什么都没写进图库时，
			// 信息并没有搬过去，此时删文档等于直接丢数据。
			//
			// ★★★ 判据必须改用**块数**（2026-10-04）
			//
			// 原判据是 `ec == 0 && rc == 0`。而旧表双写已停，
			// 这两个计数**恒为 0** ⇒ 每个文档都被判定「什么都没搬」
			// ⇒ 文档永远不会被删除，L2 归档完全停摆。
			//
			// ★★ 而且这个防护恰好就是它自己想防的那件事：
			//   它写下的注释说「实测 456 字图片描述得到 0 entities 0 relations，
			//   随后文档被删、图片与描述彻底消失」——
			//   现在它因为同一个原因（0 计数）而**永远保留**，
			//   只是方向反了：从「误删」变成「永不删」。
			//
			// 正确的判据是**块数** —— 那是旧表停写后仍然有效的信号
			// （commitTriplesWithMedia 的第三个返回值）。
			//
			// ★★ 不能用 blocks 判据（2026-10-04 修正）
			//
			// 我第一版改成 `if blocks == 0 { 保留文档 }`，结果文档永不被删
			// —— 因为 **blocks 恒为 0**：docToTriples 产出的三元组
			// 没有 SentenceText（也没有 MediaDigests），
			// 而 blocks 只统计「按原句挂接的媒体块」。
			// 文档的媒体块走的是下面的 linkBlocksToDocument，不是这条路。
			//
			// ★ 真正的判据在下面：linkBlocksToDocument 的绑定数。
			//   本文档没有块时（len(doc.Blocks) == 0），
			//   「三元组有没有写进图库」才是唯一的问题 ——
			//   而那要看**块侧**有没有新增节点。
			//
			// ec/rc 已停用（恒 0），日志里如实标注。
			// ★★★ 「图库有没有真的接住内容」（2026-10-04）
			//
			// 原判据 `ec == 0 && rc == 0` 已失效（旧表停写后恒 0），
			// 它会**永远保留文档** —— 而那段防护的初衷恰恰是
			// 「实测 456 字图片描述得到 0 计数，随后文档被删、
			// 图片与描述彻底消失」。同一个原因，方向反了：
			// 从「误删」变成「永不删」。
			//
			// ★ 中途试过 `blocks == 0`，不对 —— 而**原因不是缺陷**：
			//
			//	blocks 只统计 attachBlocksToSentenceBlock（按原句挂接媒体块），
			//	而归档路径上的媒体块**设计上就不走那条路**：
			//	docToTriples 明确不把媒体放进三元组
			//	（见其注释「媒体不再参与三元组」—— 那是准确的设计，不是遗留），
			//	媒体由 linkBlocksToDocument 以 document --contains--> block
			//	写入 L3。所以 blocks 在归档路径上**本来就该是 0**。
			//
			// ★★ 记一笔我在这里犯的错（2026-10-04）：
			//   我把那句准确的设计注释读成「过时注释」，
			//   于是去查「docToTriples 的三元组没有 SentenceText」，
			//   把它当成「媒体块无处挂接的功能缺口」。
			//   —— 而没有 SentenceText 正是设计本意。
			//   更糟的是我把这个错误判断写进了本注释，
			//   会误导下一个接手的人。
			//
			// ★ 可靠信号是**块总数是否增长**。三条路径任一成功都会加块：
			//
			//	doc.Blocks         → linkBlocksToDocument（媒体块）
			//	triples 的实体名   → putTripleBlocksTx（subject/object 块）
			//	triples 的原句     → 原句块
			//
			blocksAfter, err := a.memory.MemoryBlockCount()
			if err != nil {
				log.Printf("[agent] doc→graph: %s 读块数失败，保留文档: %v", doc.ID, err)
				continue
			}
			if blocksAfter == blocksBefore {
				log.Printf("[agent] doc→graph: %s 未写入任何块，保留文档待下轮重试"+
					"（三元组 %d 条全被实体名校验拒绝）", doc.ID, len(triples))
				continue
			}
			log.Printf("[agent] doc→graph: %s → 新增 %d 块（三元组 %d 条，"+
				"媒体挂接 %d，旧表计数已停用 ec=%d rc=%d）",
				doc.ID, blocksAfter-blocksBefore, len(triples), blocks, ec, rc)

			// 文档持有的一等块写入 L3，并以 document --contains--> block 边关联；
			// 块 ID 原样保留（迁移而非重建）。块迁走后删除文档即完成迁移。
			if len(doc.Blocks) > 0 {
				if bound := a.linkBlocksToDocument(doc.ID, doc.Blocks, memory.ChannelScene(doc.Source)); bound != len(doc.Blocks) {
					log.Printf("[agent] doc→graph: %s 块迁移不完整 (%d/%d)，保留文档待下轮重试",
						doc.ID, bound, len(doc.Blocks))
					continue
				}
			}
			a.docStore.Remove(doc.ID)
		}
	}
}

// ──────────────────────────────────────────────
// 实体合并检测：GraphDB → LLM 裁决
// ──────────────────────────────────────────────

func (a *Agent) detectEntityMerge() {
	if a.memory == nil {
		return
	}

	log.Printf("[agent] entity merge detection start")

	result, err := a.memory.Recall(nil, nil, 1, "")
	if err != nil || result == nil || len(result.Entities) < 2 {
		return
	}

	llmCandidates := 0
	maxCandidates := 5

	for i := 0; i < len(result.Entities) && llmCandidates < maxCandidates; i++ {
		for j := i + 1; j < len(result.Entities) && llmCandidates < maxCandidates; j++ {
			ea, eb := result.Entities[i].Name, result.Entities[j].Name
			if ea > eb {
				ea, eb = eb, ea
			}
			key := ea + "||" + eb

			// 跳过已标记"不合并"的实体对
			a.noMergeMu.Lock()
			rounds, ok := a.noMergeMarkers[key]
			if ok {
				rounds--
				if rounds <= 0 {
					delete(a.noMergeMarkers, key)
				} else {
					a.noMergeMarkers[key] = rounds
				}
			}
			a.noMergeMu.Unlock()
			if ok {
				continue
			}

			// 复合相似度：字符二元组 + 语义向量（仅增强检测，不做自动合并）
			sim := entitySimilarity(result.Entities[i].Name, result.Entities[j].Name)
			semSim := entitySemanticSimilarity(result.Entities[i].Name, result.Entities[j].Name, a.embedder)
			if semSim > sim {
				sim = semSim
			}

			if sim > 0.75 {
				llmCandidates++
				a.enqueueConsolidationTask(ConsolidationTask{
					Type: "entity_merge",
					Reason: fmt.Sprintf(
						"实体「%s」(类型:%s, 提及%d次) 与「%s」(类型:%s, 提及%d次) 相似度 %.0f%%，可能指代同一事物，请判断是否需要合并",
						result.Entities[i].Name, result.Entities[i].Type, result.Entities[i].MentionCount,
						result.Entities[j].Name, result.Entities[j].Type, result.Entities[j].MentionCount,
						sim*100,
					),
					Data: map[string]interface{}{
						"entity_a":          result.Entities[i].Name,
						"entity_a_type":     result.Entities[i].Type,
						"entity_a_mentions": result.Entities[i].MentionCount,
						"entity_b":          result.Entities[j].Name,
						"entity_b_type":     result.Entities[j].Type,
						"entity_b_mentions": result.Entities[j].MentionCount,
						"similarity":        sim,
					},
				})
			}
		}
	}

	if llmCandidates > 0 {
		log.Printf("[agent] entity merge: %d merge candidates sent for LLM decision", llmCandidates)
	} else {
		log.Printf("[agent] entity merge: no similar entities found")
	}
}

// dedupeScenes 是图整备的场景侧去重：把「同一个场面的两个键」合成一个。
//
// 为什么与 detectEntityMerge 分开而不塞进它的双重循环：
//   - 实体：全库两两 bigram + LLM 裁决。1 万实体实测 5000 万次配对、
//     ~224GB 瞬时分配每轮（见 plan「不做的事」），已是独立问题。
//   - 场景：判重口径是**归一化后是否同名**——同名即同一场面，**不需要 LLM
//     裁决**（键相同本身就是证据）。且 declared 场景不进相似度空间，
//     场景之间「像不像」由 EnterScene 的聚类负责，不是这里的事。
//
// 所以这里只做确定性的同键合并，不做相似度合并：把「chan:qq 与
// chan:webui 很像」也合并是危险的，那会把不同场面糊成一个。
func (a *Agent) dedupeScenes() {
	if a.memory == nil {
		return
	}
	merged, err := a.memory.DedupeScenes()
	if err != nil {
		log.Printf("[agent] 场景去重失败（下轮重试）: %v", err)
		return
	}
	if merged > 0 {
		log.Printf("[agent] 场景去重：合并 %d 组同键场景（同一场面的重复键）", merged)
	}
}

// ──────────────────────────────────────────────
// 关系复审：GraphDB → ClearSentenceID → CleanupOrphanedSentences
// ──────────────────────────────────────────────

func (a *Agent) reviewRelations() {
	if a.memory == nil {
		return
	}

	log.Printf("[agent] relation review start")

	reviewCount := 0
	const maxReviewBatch = 5
	relResult, err := a.memory.Recall(nil, nil, 1, "")
	if err != nil || relResult == nil {
		return
	}
	for _, rel := range relResult.Relations {
		if reviewCount >= maxReviewBatch {
			break
		}
		if rel.SentenceID == 0 || rel.SentenceText == "" {
			continue
		}

		a.enqueueConsolidationTask(ConsolidationTask{
			Type: "relation_review",
			Reason: fmt.Sprintf(
				"【关系复审】原始句子: '%s'\n当前三元组: (%s → %s → %s) 置信度 %.2f\n请判断是否需要修正（如相对引用未解析、主宾颠倒、噪音三元组等），如需修正请用 memory_edit 工具",
				rel.SentenceText, rel.SourceName, rel.RelationType, rel.TargetName, rel.Confidence,
			),
			Data: map[string]interface{}{
				"relation_id":   rel.ID,
				"source":        rel.SourceName,
				"relation_type": rel.RelationType,
				"target":        rel.TargetName,
				"confidence":    rel.Confidence,
				"sentence":      rel.SentenceText,
			},
		})

		// 清除句子引用（复审后解除关联）
		if err := a.memory.ClearSentenceID(rel.ID); err != nil {
			log.Printf("[agent] clear sentence_id for relation %d: %v", rel.ID, err)
		}
		reviewCount++
	}

	if reviewCount > 0 {
		// 清理无引用的句子
		if deleted, err := a.memory.CleanupOrphanedSentences(); err != nil {
			log.Printf("[agent] cleanup orphaned sentences: %v", err)
		} else if deleted > 0 {
			log.Printf("[agent] cleanup %d orphaned sentences", deleted)
		}
		log.Printf("[agent] relation review: %d relations sent for review", reviewCount)
	}
}

// entitySemanticSimilarity 使用词嵌入向量余弦相似度计算实体名语义相似度
func entitySemanticSimilarity(a, b string, embedder *memory.StaticEmbedder) float64 {
	if a == "" || b == "" || embedder == nil || !embedder.Loaded() {
		return 0
	}
	va := embedder.Vectorize(a)
	vb := embedder.Vectorize(b)
	if len(va) == 0 || len(vb) == 0 {
		return 0
	}
	return vector.CosineSimilarity(va, vb)
}

func entitySimilarity(a, b string) float64 {
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 1.0
	}
	runesA, runesB := []rune(a), []rune(b)
	if len(runesA) < 2 || len(runesB) < 2 {
		if len(runesA) == len(runesB) && len(runesA) == 1 {
			if runesA[0] == runesB[0] {
				return 1.0
			}
		}
		return 0
	}

	setA := make(map[string]bool)
	for i := 0; i < len(runesA)-1; i++ {
		setA[string(runesA[i:i+2])] = true
	}

	setB := make(map[string]bool)
	for i := 0; i < len(runesB)-1; i++ {
		setB[string(runesB[i:i+2])] = true
	}

	intersect := 0
	for bg := range setA {
		if setB[bg] {
			intersect++
		}
	}

	union := len(setA) + len(setB) - intersect
	if union <= 0 {
		return 0
	}

	return float64(intersect) / float64(union)
}

func docToTriples(doc *document.Doc, embedder nlp.Vectorizer) []memory.Triple {
	var triples []memory.Triple
	if doc == nil {
		return triples
	}

	if doc.Source == "graph" || doc.Source == "" {
		return nil
	}

	// 文档归档的知识是有**来源场面**的：来自 QQ 的对话归档，其三元组就该
	// 钉在 chan:qq 上。这样「又来一条 QQ 消息」时，这批知识靠场景就能取回，
	// 不必指望本轮措辞与它们字面重合。
	docScene := memory.ChannelScene(doc.Source)

	isArchivedContext := doc.Meta != nil && doc.Meta["is_archived_context"] == "true"

	// 文档元数据:仅当 summary 合理(非空、非模板化、长度适中)时才写「主题」
	if !isArchivedContext && doc.Summary != "" && len([]rune(doc.Summary)) < 80 && !isTemplateSummary(doc.Summary) {
		triples = append(triples, memory.Triple{
			Subject:     "文档",
			SubjectType: "Concept",
			Relation:    "主题",
			Object:      doc.Summary,
			ObjectType:  "Topic",
			Confidence:  1.0,
			Scene:       docScene,
		})
	}

	// 媒体不再参与三元组：它作为一等块由 linkBlocksToDocument
	// 写入 L3 并以 document --contains--> block 边关联，
	// 不经过文本描述与 NLP 提取器。

	// NLP 通用提取
	e := nlp.NewExtractor(nil)
	if embedder != nil {
		e.SetEmbedder(embedder)
	}
	result := e.Extract(doc.Content)
	if result != nil {
		for _, nt := range result.Triples {
			mt := nlp.ToMemoryTriple(nt)
			if mt.Subject != "" && mt.Relation != "" && mt.Object != "" {
				mt.Scene = docScene
				triples = append(triples, mt)
			}
		}
	}

	// 仅当来源非归档上下文且非空时写「来源」——归档文档写死模板三元组属于垃圾
	if doc.Source != "" && doc.Source != "context_archived" {
		triples = append(triples, memory.Triple{
			Subject:     "文档",
			SubjectType: "Concept",
			Relation:    "来源",
			Object:      doc.Source,
			ObjectType:  "Source",
			Confidence:  1.0,
			Scene:       docScene,
		})
	}

	// 噪音闸门：NLP 提取器不认常用词（「结果 / 什么 / 待命」都能当主语），
	// 而落库闸门 validEntityName 只管名字像不像名字。这一层是防止
	// 「每个文档的常用词都变成实体」的唯一防线（CutExact 时代的那层已随
	// 提取器换代丢失，见 memory.IsNoiseEntity 的说明）。
	return memory.FilterNoiseTriples(triples)
}

// isTemplateSummary 识别 summarizeEntries 生成的模板化摘要
// （形如「来自 N 个来源的 M 条对话 (src1, src2) 涉及: kw1, kw2」），
// 这类摘要无独立信息量,不应作为「主题」实体写入图库。
func isTemplateSummary(s string) bool {
	if s == "" {
		return true
	}
	return strings.HasPrefix(s, "来自 ") && strings.Contains(s, "条对话")
}

func (a *Agent) emitMemoryCandidate(source, input, response string, toolResults []ToolResultItem, toolsUsed []string) {
	payload := map[string]interface{}{
		"source":       source,
		"input":        input,
		"response":     response,
		"tool_results": toolResults,
		"tools_used":   toolsUsed,
		"agent_id":     string(a.id),
		"timestamp":    time.Now().Unix(),
	}
	// ★ 记忆候选是**不经 context** 的另一条持久化侧路（走 EmitOutput）。
	// 不在这里拦，明文会直接进长期记忆 —— 而记忆跨会话保留、每轮被召回。
	scrubMemoryCandidate(payload)
	a.io.EmitOutput("memory", "memory_candidate", payload)
}

func (a *Agent) processConsolidation(evt *agentIO.InputEvent, input string) {
	start := time.Now()

	stageCtx := a.stageCtxFromInput(input, evt.Source, "")
	stageCtx.Extra["output_channel"] = evt.OutputChannel
	a.injectSourceContext(stageCtx, evt)

	_, toolsUsed, _, err := a.process(input, stageCtx)
	if err != nil {
		log.Printf("[agent] consolidation error: %v", err)
		return
	}

	log.Printf("[agent] consolidation done (%dms, tools=%v)", time.Since(start).Milliseconds(), toolsUsed)
}
