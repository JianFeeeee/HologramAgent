package agent

import (
	"fmt"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
	"log"
	"strings"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
	"github.com/JianFeeeee/HologramAgent/internal/nlp"
)

type ConsolidationTask struct {
	Type   string      `json:"type"`
	Reason string      `json:"reason"`
	Data   interface{} `json:"data"`
}

func (a *Agent) enqueueConsolidationTask(task ConsolidationTask) {
	msg := fmt.Sprintf(
		"【记忆整理任务】\n类型: %s\n说明: %s\n\n注意：\n1. 仅使用 stablemem_merge 合并实体，或使用 stablemem_block_merge 标记不合并\n2. 不要使用 stablemem_commit 写入新的三元组\n3. 不要从这段任务文本中提取任何信息写入图库\n4. 只需要做出合并/不合并的判断并执行对应工具",
		task.Type, task.Reason,
	)
	a.injectSelf(msg)
	log.Printf("[agent] enqueued consolidation task: %s", task.Reason)
}

// ──────────────────────────────────────────────
// 四个独立心跳循环，各自拥有独立的 ticker 和配置
// ──────────────────────────────────────────────

// distillLoop 上下文裁剪（L1→L2），使用 distillInterval

// archiveLoop 冷文档归档（L2→L3），使用 archiveInterval

// mergeLoop 实体合并检测（GraphDB → LLM 裁决），使用 mergeInterval

// reviewLoop 关系复审（GraphDB → ClearSentenceID → CleanupOrphanedSentences），使用 reviewInterval

// ──────────────────────────────────────────────
// 蒸馏逻辑
// ──────────────────────────────────────────────

func (a *Agent) distillContext() {
	if a.coldcontextStore == nil {
		return
	}
	n := a.context.Len()
	if n > a.maxContextSize*2 {
		archived := a.context.Prune("", a.maxContextSize, a.coldcontextStore)
		if archived > 0 {
			log.Printf("[agent] distill: pruned %d low-relevance events to document memory (total=%d)", archived, n)
		}
	}
}

// 归档尝试上限。超过就**降级**（保留块成果、丢弃文档），而不是无限重试。
//
// 为什么必须有上限（2026-11-15 实测）：此前失败只是 `continue`（保留待下一轮），
// 没有计数、没有退避、没有死信。生产日志显示 50 个文档每小时重试一次，
// 从 2026-11-11 起连跑了 3 天共 2950 次，输入完全相同 ⇒ 必然失败的无限循环。
const maxArchiveAttempts = 5

// archiveStats 是一轮归档的汇总。每轮只落一条日志 ——
// 此前只有失败逐条记、成功什么都不记，运维看不出「这轮扫了多少、成了多少」。
//
// 更要命的是 `len(triples) == 0` 那条分支完全静默：生产实测每轮
// `FindColdContexts` 选出 199 个候选，日志只出 37 条 —— **162 个文档
// 走进了那个分支就消失得无影无踪**，看起来像「每轮只处理 37 个」。
type archiveStats struct {
	Scanned   int // FindColdContexts 选出的候选数
	Archived  int // 完全搬完并已删除的文档数
	Blocks    int // 本轮迁移进 L3 的块数
	Triples   int // 本轮真正落库的三元组条数
	NoTriples int // 一步三元组都没抽出来的文档数（只靠块迁移）
	Empty     int // 既无三元组也无块，直接删除的空文档数
	Retried   int // 仍留在 L2 等待下一轮的文档数
	Quarantin int // 达到重试上限、正文已在 L3，文档被丢弃
	Stuck     int // 达到重试上限、**没有任何内容进 L3**，文档保留待人工
}

// archiveOneColdDoc 归档**一篇**冷文档。
//
// ## 为什么要把「块迁移」与「三元组」拆开（2026-11-15 修正）
//
// 此前是一个前置门：
//
//	写三元组 → 块数差值==0 ⇒ continue（扣住文档）
//	                        ⇒ 后面的 linkBlocksToDocument **永远轮不到**
//
// 于是「三元组抽不出来」连带「文档自带的块也全部被扣在 L2」。
// 生产实测：702 个文档持有 11994 块，图库里命中 **0** 个。
// 这不是重试浪费，是 L2→L3 的整条块迁移路径被一个前置判断堵死。
//
// 现在的语义：**正文（块）是主资产，三元组（关系）是派生加工**。
//
//	· 块迁移成功 ⇒ 信息已经在 L3，删文档不丢数据
//	· 三元组一条没落库 ⇒ 关系层没建成，重试；超上限则降级（见函数内注释）
//
// toolDeclaresNoMemory 查工具是否声明「输出不参与记忆计算」。
//
// 两处来源，与 textForVector 完全一致：
//  1. 插件工具（含 proc 桥）→ StageHost 的 ToolDef 声明
//  2. 内置工具（core 自己的 switch 分派）→ 包级声明表
//
// 两处都必须查：只查一处会让另一半工具在蒸馏侧享受不到自己的声明，
// 而那正是「机制 ≠ 接线」的经典形态（生产里两条路径各查各的，漂移了没人发现）。
func (a *Agent) toolDeclaresNoMemory(name string) bool {
	if a.stageHost != nil {
		if v, found := a.stageHost.NoMemoryOf(name); found {
			return v
		}
	}
	return noMemoryOf(name)
}

func (a *Agent) archiveOneColdDoc(coldDoc *coldcontext.ColdDoc, st *archiveStats) {
	if coldDoc == nil || a.graphDB() == nil {
		return
	}
	st.Scanned++

	// ── 第一步：三元组（关系层，派生加工）──
	triples := coldDocToTriples(coldDoc, vectorizerFace{e: a.embedder}, a.toolDeclaresNoMemory)
	written := 0
	if len(triples) > 0 {
		w, _, err := a.commitTriplesWithMedia(triples, string(a.id)+"_doc_archival", 0, coldDoc.Blocks)
		if err != nil {
			// 写库失败是**瞬时故障**（值得重试）；「抽不出来」不是，两者必须分开。
			log.Printf("[agent] coldcontext->graph: %s commit failed (%d triples): %v",
				coldDoc.ID, len(triples), err)
			a.noteArchiveFailureWith(coldDoc, st, fmt.Sprintf("commit error: %v", err), false)
			return
		}
		written = w
		st.Triples += written
	} else {
		// ★ 这条分支此前**完全静默**。它是「NLP 在这段原文上抽不出东西」的
		// **常态**（归档上下文以工具输出/终端日志为正文），不是异常。
		// 静默会让运维误判吞吐（实测 199 候选 / 37 条日志，差 162 条无人知晓）。
		st.NoTriples++
	}

	// 空文档：既没抽出三元组、也没有任何块 ⇒ 本来就没有内容可搬。
	//
	// 这不是失败：删掉它不丢任何东西。此前它会走「全部被拒」那条失败路径，
	// 白白重试 5 次才进 STUCK，日志里还会被当成「关系层建不起来」。
	if len(triples) == 0 && len(coldDoc.Blocks) == 0 {
		a.coldcontextStore.Remove(coldDoc.ID)
		st.Empty++
		return
	}

	// ── 第二步：判定「内容有没有进 L3」──
	//
	// 这是**防误删**的唯一判据：交付到 L3 的是「三元组（关系）」或
	// 「文档自带的块（正文）」，任一成功即安全。
	//
	// ★ 为什么不能用块数差值：块 ID 由内容派生，幂等命中时块数根本不涨
	//   （详见 memory.CommitReady 的注释）。生产实测 259 条误判里 77 条
	//   其实写了一两条。
	//
	// ★ 为什么 written>0 就够：那些三元组已经通过 putTripleBlocksTx 落库，
	//   它的端点块与原句块都在 L3 里了。
	//
	// ★ 「全部被拒」**不是**可重试故障：coldDocToTriples 是确定性的，
	//   同一段文本重跑必然得到同一批被拒的三元组。此前把它当失败无限重试，
	//   正是 3 天 2950 次的来源。
	contentSafe := written > 0 || len(coldDoc.Blocks) > 0
	if !contentSafe {
		// 三元组全被拒、且文档没有块 ⇒ 真的什么都没进 L3，删了就是丢数据。
		a.noteArchiveFailureWith(coldDoc, st, fmt.Sprintf(
			"%d triples all rejected and document holds no blocks — nothing reached L3", len(triples)), false)
		return
	}
	if len(triples) > 0 && written == 0 {
		// 有关系候选但一条都没进库：关系层没建成，正文照常迁（下行），
		// 且**不再重试** —— 同一段文本重抽必然同样被拒。
		log.Printf("[agent] coldcontext->graph: %s all %d triples rejected by entity-name validation, "+
			"archiving blocks only (no relation edges for this document)", coldDoc.ID, len(triples))
	}

	// ── 第三步：块迁移（正文）──
	//
	// 必须排在判定之后：块一旦写进 memory_blocks，文档就不该继续留在 L2
	// （「一块在任一层只有一个持有者」）。所以先判「将来能不能删」，
	// 能删才动块 —— 否则会留下「L3 有块、L2 也还留着」的窗口。
	//
	// 迁移不完整时会留下一个短暂的窗口（部分块在 memory_blocks、文档仍在 L2）。
	// 这是可以接受的：PutMemoryBlocks 是 upsert、AddMemoryBlockEdge 显式查重，
	// 下一轮重试幂等收敛（补上缺的边），不会产生重复块。
	if len(coldDoc.Blocks) > 0 {
		bound := a.linkBlocksToDocument(coldDoc.ID, coldDoc.Blocks, memory.ChannelScene(coldDoc.Source))
		if bound != len(coldDoc.Blocks) {
			// 迁移不完整：此时 L3 里已有一部分块，L2 仍是全量。
			// 重试是靠 PutMemoryBlocks/AddMemoryBlockEdge 的幂等性收敛的
			// （已迁的块不会二次插入，缺的边会补上）。
			log.Printf("[agent] coldcontext->graph: %s block migration incomplete (%d/%d)",
				coldDoc.ID, bound, len(coldDoc.Blocks))
			// contentLanded=false：**必须**。bound>0 说明部分块进了 L3，
			// 但 bound<len 说明还有 (len-bound) 块只在 L2 —— 到上限时删掉
			// 文档就丢那部分正文。二者不是"差不多"，是一条数据丢失线。
			a.noteArchiveFailureWith(coldDoc, st,
				fmt.Sprintf("block migration incomplete (%d/%d)", bound, len(coldDoc.Blocks)), false)
			return
		}
		st.Blocks += bound
	}

	a.coldcontextStore.Remove(coldDoc.ID)
	st.Archived++
	log.Printf("[agent] coldcontext->graph: %s archived (%d blocks migrated, %d/%d triples written)",
		coldDoc.ID, len(coldDoc.Blocks), written, len(triples))
}

// noteArchiveFailureWith 记一次失败尝试；超过上限则**放弃重试**。
//
// ## 为什么必须带 contentLanded（这是唯一会造成数据丢失的分支）
//
// 到上限时的处置只有两种，选错的代价完全不同：
//
//	contentLanded = true   至少一部分内容已在 L3 ⇒ L2 这份是重复的，删掉安全
//	contentLanded = false  **什么都没进 L3** ⇒ L2 是唯一副本，删掉就是数据丢失
//
// 此前只有一条路径（无条件 Remove），因为旧代码里「块迁移」是第一步、
// 一定已经成功。现在块迁移排在判定之后（为了不在 L2/L3 双重持有块），
// 于是「一块都没迁成」变成可达状态 —— 那就**必须保留**。
//
// 「静默卡住」与「每小时重试一次、永远失败」都不可观测，两者都不是答案：
// 前者丢数据，后者把问题推给日志量。所以到上限一律响亮记录，分层处置。
func (a *Agent) noteArchiveFailureWith(coldDoc *coldcontext.ColdDoc, st *archiveStats, reason string, contentLanded bool) {
	attempts := a.coldcontextStore.NoteArchiveFailure(coldDoc.ID, reason)
	if attempts < maxArchiveAttempts {
		st.Retried++
		log.Printf("[agent] coldcontext->graph: %s kept for retry (attempt %d/%d): %s",
			coldDoc.ID, attempts, maxArchiveAttempts, reason)
		return
	}
	// 到上限。此时要分两种情况，**不能一刀切删**：
	if !contentLanded {
		// 什么都没进 L3 ⇒ L2 是唯一副本。删了就是真丢数据。
		// 保留它、不再重试（重试是确定性的必然失败），并响亮告知人工。
		st.Stuck++
		log.Printf("[agent] coldcontext->graph: %s STUCK after %d attempts and NOTHING reached L3 "+
			"(last error: %s). Document kept in L2 as the only copy; automatic retry stopped. "+
			"Action: inspect this document, then either commit the facts explicitly with "+
			"stablemem_commit or delete it manually.", coldDoc.ID, attempts, reason)
		return
	}
	// 正文已在 L3 ⇒ L2 这份是重复的，可以丢。
	//
	// ★ 为什么不静默删：这批文档的关系层永远建不起来，是可观测的能力缺口。
	//   「每小时重试一次、永远失败」也一样不可观测 —— 只是把问题推给日志量。
	a.coldcontextStore.Remove(coldDoc.ID)
	st.Quarantin++
	log.Printf("[agent] coldcontext->graph: %s gave up after %d attempts, blocks stay in L3, "+
		"no relation edges for this document (last error: %s). "+
		"This is expected for archived tool/terminal transcripts; "+
		"if the content mattered, commit the facts explicitly with stablemem_commit.",
		coldDoc.ID, attempts, reason)
}

// ──────────────────────────────────────────────
// 冷文档归档：coldcontextStore → GraphDB (L2→L3)
// ──────────────────────────────────────────────

func (a *Agent) archiveColdDocs() {
	if a.graphDB() == nil {
		return
	}

	log.Printf("[agent] coldcontext archival start")

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
	if a.graphDB() != nil {
		if n, err := a.graphDB().DecaySceneRefs(30*24*time.Hour, 0.05); err != nil {
			log.Printf("[agent] scene decay error: %v", err)
		} else if n > 0 {
			log.Printf("[agent] scene-association decay: pruned %d long-unseen references", n)
		}
	}

	if a.coldcontextStore != nil {
		// 文档有变化才重建 TF-IDF 索引。
		//
		// 此前每轮无条件 Reindex：它重置 sync.Once、重建 8.3MB 训练文本、
		// 全量 Train 一次并持写锁 —— 每小时一次的 O(全库) 停顿，
		// 与「这轮到底有没有新文档」无关。721 个文档时那是纯浪费。
		if a.coldcontextStore.Dirty() {
			a.coldcontextStore.Reindex()
		}
	}

	if a.coldcontextStore != nil {
		coldDocs := a.coldcontextStore.FindColdContexts(72*time.Hour, 2)
		st := archiveStats{}
		for _, coldDoc := range coldDocs {
			a.archiveOneColdDoc(coldDoc, &st)
		}
		// 一轮一条汇总日志：扫了多少、成了多少、多少只迁了块、多少在重试。
		//
		// 没有这条日志时，运维只能看到失败逐条打印，
		// 而「199 个候选里 162 个静默跳过」完全不可见。
		log.Printf("[agent] coldcontext archival done: scanned=%d archived=%d empty=%d quarantined=%d stuck=%d retried=%d "+
			"blocks=%d triples=%d no_triples=%d",
			st.Scanned, st.Archived, st.Empty, st.Quarantin, st.Stuck, st.Retried,
			st.Blocks, st.Triples, st.NoTriples)
	}
}

// ──────────────────────────────────────────────
// 实体合并检测：GraphDB → LLM 裁决
// ──────────────────────────────────────────────

func (a *Agent) detectEntityMerge() {
	if a.graphDB() == nil {
		return
	}

	log.Printf("[agent] entity merge detection start")

	result, err := a.graphDB().Recall(nil, nil, 1, "")
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
	if a.graphDB() == nil {
		return
	}
	merged, err := a.graphDB().DedupeScenes()
	if err != nil {
		log.Printf("[agent] scene dedup failed (will retry next round): %v", err)
		return
	}
	if merged > 0 {
		log.Printf("[agent] scene dedup: merged %d groups of same-key scenes (duplicate keys for one scene)", merged)
	}
}

// ──────────────────────────────────────────────
// 关系复审：GraphDB → ClearSentenceID → CleanupOrphanedSentences
// ──────────────────────────────────────────────

func (a *Agent) reviewRelations() {
	if a.graphDB() == nil {
		return
	}

	log.Printf("[agent] relation review start")

	reviewCount := 0
	const maxReviewBatch = 5
	relResult, err := a.graphDB().Recall(nil, nil, 1, "")
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
				"【关系复审】原始句子: '%s'\n当前三元组: (%s → %s → %s) 置信度 %.2f\n请判断是否需要修正（如相对引用未解析、主宾颠倒、噪音三元组等），如需修正请用 stablemem_edit 工具",
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
		if err := a.graphDB().ClearSentenceID(rel.ID); err != nil {
			log.Printf("[agent] clear sentence_id for relation %d: %v", rel.ID, err)
		}
		reviewCount++
	}

	if reviewCount > 0 {
		// 清理无引用的句子
		if deleted, err := a.graphDB().CleanupOrphanedSentences(); err != nil {
			log.Printf("[agent] cleanup orphaned sentences: %v", err)
		} else if deleted > 0 {
			log.Printf("[agent] cleanup %d orphaned sentences", deleted)
		}
		log.Printf("[agent] relation review: %d relations sent for review", reviewCount)
	}
}

// entitySemanticSimilarity 使用词嵌入向量余弦相似度计算实体名语义相似度
func entitySemanticSimilarity(a, b string, embedder Embedder) float64 {
	if a == "" || b == "" || embedder == nil || !embedder.Loaded() {
		return 0
	}
	va := embedder.VectorizeMap(a)
	vb := embedder.VectorizeMap(b)
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

func coldDocToTriples(coldDoc *coldcontext.ColdDoc, embedder nlp.Vectorizer, toolNoMemory func(name string) bool) []model.Triple {
	var triples []memory.Triple
	if coldDoc == nil {
		return triples
	}

	if coldDoc.Source == "graph" || coldDoc.Source == "" {
		return nil
	}

	// 文档归档的知识是有**来源场面**的：来自 QQ 的对话归档，其三元组就该
	// 钉在 chan:qq 上。这样「又来一条 QQ 消息」时，这批知识靠场景就能取回，
	// 不必指望本轮措辞与它们字面重合。
	docScene := memory.ChannelScene(coldDoc.Source)

	isArchivedContext := coldDoc.Meta != nil && coldDoc.Meta["is_archived_context"] == "true"

	// 文档元数据:仅当 summary 合理(非空、非模板化、**能落库**)时才写「主题」
	//
	// ★ 阈值必须与落库闸门同口径（2026-11-15 修正）。
	//
	// 此前写的是 `< 80`，而 validEntityName 要求 `len <= 50`：
	// 长度 51~79 的 summary 会稳定产出一条**必然被拒**的三元组，
	// 每轮刷一行 reject 日志，且让「抽不出来」与「抽出来全被拒」在日志里
	// 长得一样（归档判据因此分不清该重试还是该只迁块）。
	//
	// 实测样本：`文档 --主题--> 排查 106 飞牛是否存在 Tailscale 内置 SSH
	// 提权漏洞：未装 tailscale，不适用`（这条超 50 字，被拒）。
	if !isArchivedContext && coldDoc.Summary != "" && len([]rune(coldDoc.Summary)) < 80 && !isTemplateSummary(coldDoc.Summary) &&
		// 再按**落库闸门**判一次：能被 FilterNoiseTriples 留下才值得造。
		// 两处口径（80 是「值不值得是主题」的语义阈值，50 是「能不能落库」
		// 的存储约束）都必须满足，缺一个就会产生必然被拒的三元组。
		memory.IsValidEntityName(coldDoc.Summary) {
		triples = append(triples, memory.Triple{
			Subject:     "文档",
			SubjectType: "Concept",
			Relation:    "主题",
			Object:      coldDoc.Summary,
			ObjectType:  "Topic",
			Confidence:  1.0,
			Scene:       docScene,
		})
	}

	// 媒体不再参与三元组：它作为一等块由 linkBlocksToDocument
	// 写入 L3 并以 document --contains--> block 边关联，
	// 不经过文本描述与 NLP 提取器。

	// NLP 通用提取。
	//
	// ★ 输入必须是**原文**，不能是视图。
	//
	// 旧代码抽的是 coldDoc.Content —— 那在 Events 引入前是对事件的压缩渲染
	// （Response 截 100 rune、工具输出截 200）。拿压缩过的文本做三元组抽取，
	// 等于在丢失的信息上再丢一次：句子被截断后依存句法往往解析不出主谓宾。
	//
	// 现在优先用事件里的完整正文；存量文档没有 Events 才退回 Content。
	e := nlp.NewExtractor(nil)
	if embedder != nil {
		e.SetEmbedder(embedder)
	}
	result := e.Extract(coldcontextOriginalText(coldDoc, toolNoMemory))
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
	if coldDoc.Source != "" && coldDoc.Source != "context_archived" {
		triples = append(triples, memory.Triple{
			Subject:     "文档",
			SubjectType: "Concept",
			Relation:    "来源",
			Object:      coldDoc.Source,
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

// reasoning 是本轮模型的思考内容（thinking 模式）。空串表示无思考。
//
// ★ 为何是参数而不是从 a 上读字段：本轮思考属于**这个 TaskFrame**
// （f.TurnReasoning），不是 agent 的全局状态。agent 级字段在并发任务下
// 会互相覆盖 —— 而记忆候选一旦写错就是持久化的错。
func (a *Agent) emitMemoryCandidate(source, input, response, reasoning string, toolResults []ToolResultItem, toolsUsed []string) {
	payload := map[string]interface{}{
		"source":       source,
		"input":        input,
		"response":     response,
		"tool_results": toolResults,
		"tools_used":   toolsUsed,
		"agent_id":     string(a.id),
		"timestamp":    time.Now().Unix(),
	}
	// ★ 思考内容也要进记忆候选（2026-11-11 用户裁定：L2/L3 都包含 reasoning）。
	//
	// memory_candidate 是**不经 context** 的另一条持久化侧路，所以它不会
	// 从 ContextEvent 那里自动带上 reasoning —— 必须显式放进去。
	// 漏了它，走这条路的消费方拿到的记忆就只有"做了什么"，没有"为何"。
	// 空串不放：不向消费方伪造一个空字段。
	if reasoning != "" {
		payload["reasoning_content"] = reasoning
	}
	// ★ 记忆候选是**不经 context** 的另一条持久化侧路（走 EmitOutput）。
	// 不在这里拦，明文会直接进长期记忆 —— 而记忆跨会话保留、每轮被召回。
	a.scrubMemoryCandidate(payload)
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

// coldcontextOriginalText 取文档的**原文**用于抽取/蒸馏。
//
// 有 Events 时按事件顺序拼正文（原文来自文本块）；存量文档退回 Content 视图。
// 这是「视图不是记忆」在蒸馏侧的落地：抽取必须基于原文。
//
// ## 工具输出也受 NoMemory 约束（2026-11-15 修正）
//
// `textForVector`（L1 计算层）会按 `ToolDef.NoMemory` 跳过该工具的输出，
// 而本函数此前**无条件**把全部 ToolResults 拼进蒸馏原文 —— 同一个问题、
// 两条路径两种待遇。后果是声明了 NoMemory 的工具（如 coldcontext_query 的
// 回填内容）在第二次蒸馏时被放行，构成自激回路的另一个入口。
//
// 声明查询由调用方注入（toolNoMemory），因为它依赖 agent 的工具表；
// nil 时按「无声明」处理（与 SDK 零值语义一致，保守参与）。
func coldcontextOriginalText(coldDoc *coldcontext.ColdDoc, toolNoMemory func(name string) bool) string {
	if coldDoc == nil {
		return ""
	}
	if !coldcontext.HasEvents(coldDoc) {
		return coldDoc.Content
	}
	var b strings.Builder
	for _, ev := range coldDoc.Events {
		// ★ NoMemory 事件不参与蒸馏（2026-11-15 修正）。
		//
		// 声明（InjectOptions.NoMemory / ChannelDef.NoMemory）说的是
		// 「不参与记忆计算（向量化/关键词/**蒸馏**）」。L1 的 textForVector
		// 照做了，但 L3 这条路径此前**完全无视声明**：它把 L2 里的事件原文
		// 无条件拼起来重抽三元组。
		//
		// 生产后果（实测 2026-11-15）：归档上下文以终端日志 / ADB 截图回执
		// 为正文，NLP 抽出「、」「（」「，」当主语（82% 是单字标点），
		// 被 validEntityName 全数拒掉 ⇒ 一条都没落库 ⇒ 文档被永久扣在 L2，
		// 每小时重试一次、连跑 3 天 2950 次。
		//
		// ★ 正文（块）不受影响：块迁移在 archiveOneColdDoc 的第一步就已完成，
		//   跳过蒸馏只意味着「这批内容不进关系网」，不丢原始材料。
		if ev.NoMemory {
			continue
		}
		// ★ 思考内容必须进蒸馏原文（2026-11-11 用户裁定）。
		//
		// 只取 Input/Response 会丢掉决策依据："排除掉了 B，因为 B 的接口
		// 不返回时间戳"这类判断就只在思考里。丢掉它，蒸馏出的三元组就只剩
		// "选了 A"，没有"为何不是 B"——而后者往往才是可复用的知识。
		//
		// 顺序按**时序**：输入 → 思考 → 回复。思考发生在回复之前。
		in, resp, reasoning, _ := coldcontext.InOutWithReasoning(&ev)
		if in != "" {
			b.WriteString(in)
			b.WriteString("\n")
		}
		if reasoning != "" {
			b.WriteString(reasoning)
			b.WriteString("\n")
		}
		if resp != "" {
			b.WriteString(resp)
			b.WriteString("\n")
		}
		for _, tr := range ev.ToolResults {
			if toolNoMemory != nil && toolNoMemory(tr.Name) {
				continue
			}
			b.WriteString(tr.Output)
			b.WriteString("\n")
		}
	}
	return b.String()
}
