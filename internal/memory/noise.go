package memory

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ──────────────────────────────────────────────
// 图记忆「噪音实体」判定与清理
//
// 为什么需要这一层：doc→graph 蒸馏在 1cb3e87 从「CutExact 滑窗词链」换成
// NLP 依存提取器后，落库闸门只剩 validEntityName（长度 2–50、含字母/汉字）。
// validEntityName 挡的是「不像名字的字符串」，不挡「像名字的常用词」——
// 于是「结果 / 什么 / 哪个 / 待命 / 报告」这类词被反复写成实体，
// mention_count 冲到几百，度数却只有 1~2：它们占着热实体位、挤满召回预算，
// 却不带任何结构。CutExact（去停用词 + validEntityName + 去重）本可以做这层
// 过滤，但它当年只挂在 doc→graph 上，换提取器时被整条摘掉，如今只剩测试调用。
//
// 这里把判定收敛到一处，供三个地方共用：
//   - 自动填充路：coldDocToTriples（冷文档归档）、extractKeyTriples（对话蒸馏）
//   - 历史数据清理：GraphDB.PurgeNoise（老库里的存量垃圾）
// ──────────────────────────────────────────────

// 模板摘要的判定片段：summarizeEntries 产出形如
// 「来自 N 个来源的 M 条对话 (src…) 涉及: kw…」。这类串只是「哪些词出现过」
// 的回声，没有独立信息量，写进图库会把「文档 --主题--> …」变成同构垃圾边。
// 判定与 agent/core.isTemplateSummary 保持一致。
const (
	templateSummaryPrefix = "来自 "
	templateSummaryMarker = "条对话"
)

// archivedContextSource 是归档上下文文档写死的 source 标记。
// 它是内部状态而非概念，不应以「来源」实体形式存在于图库中。
const archivedContextSource = "context_archived"

// IsNoiseEntity 判定一个实体名是否属于「不该进图记忆的噪音」。
//
// 判定刻意保守，只覆盖三类**客观**噪音，不做「这个词有没有信息量」的价值判断：
//  1. 停用词表命中的常用词（代词/助词/连词/介词/泛化名词/英文虚词）
//  2. 归档上下文内部标记（context_archived）
//  3. summarizeEntries 的模板摘要串
//
// 开放类词（「报告 / 对话 / 处理」这类真名词真动词）不在此列：它们在别的
// 语境下可能是有意义的实体，挡不挡属于领域决策，不该由这个函数替使用者拍板。
func IsNoiseEntity(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" {
		return true
	}
	if stopWords[n] {
		return true
	}
	if n == archivedContextSource {
		return true
	}
	return IsTemplateSummaryName(n)
}

// IsTemplateSummaryName 判断实体名是否为模板摘要串（供调用方单独使用）。
func IsTemplateSummaryName(name string) bool {
	n := strings.TrimSpace(name)
	return strings.HasPrefix(n, templateSummaryPrefix) && strings.Contains(n, templateSummaryMarker)
}

// FilterNoiseTriples 丢弃任一端为噪音实体的三元组，保持原顺序。
//
// 为什么在自动填充入口过滤、而不是在 Commit 里过滤：Commit 同时是
// memory_commit 工具（模型显式写入）的落库口。模型主动写「结果 --是--> X」
// 是它的自由（也可能它当时真的想记），而自动填充是无人在环的批量产出，
// 必须自己保证质量——闸门开在产生噪声的那一端。
func FilterNoiseTriples(triples []Triple) []Triple {
	if len(triples) == 0 {
		return triples
	}
	out := make([]Triple, 0, len(triples))
	for _, t := range triples {
		if IsNoiseEntity(t.Subject) || IsNoiseEntity(t.Object) {
			continue
		}
		out = append(out, t)
	}
	return out
}

// NoiseEntities 列出库中现存的噪音实体，按 mention_count 降序。
func (g *GraphDB) NoiseEntities() ([]Entity, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.noiseEntitiesLocked()
}

func (g *GraphDB) noiseEntitiesLocked() ([]Entity, error) {
	// ★★★ 改扫 memory_blocks（2026-10-04）
	//
	// 旧实现扫 entities 表。旧表退场后返回空 ⇒ PurgeNoise 认定「库里没有噪音」
	// ⇒ 直接 return 0,0,nil —— 而块侧的噪音块一条不少。
	//
	// 这比「删不掉」更糟：用户点清理，报告显示「已清理 0 个噪音」，
	// 看起来是**成功**的。
	//
	// ★ ID 口径：Entity.ID 是 int64，块 ID 是字符串。
	//   这里用**行号**（ROW_NUMBER 不便，改用 OFFSET 累计的序号）占位，
	//   因为 PurgeNoise 真正用来删的是块 ID，不是这个数字。
	//   排序仍然按 created_at（块的），语义上最接近旧的 mention_count ——
	//   mention_count 已无处可取，而「先出现的更可能是早期噪音」是同向的。
	rows, err := g.db.Query(
		`SELECT id, text_content, modality, created_at, updated_at
		 FROM memory_blocks WHERE text_content != '' ORDER BY created_at ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entity
	var seq int
	for rows.Next() {
		var id, text, modality string
		var created, updated time.Time
		if err := rows.Scan(&id, &text, &modality, &created, &updated); err != nil {
			return nil, err
		}
		seq++
		if !IsNoiseEntity(text) {
			continue
		}
		out = append(out, Entity{
			ID:           int64(seq), // 占位：PurgeNoise 用块 ID 删，不用它
			Name:         text,
			Type:         "block",
			MentionCount: 1, // 块无 mention_count；排序时用 created_at
			CreatedAt:    created,
			UpdatedAt:    updated,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MentionCount != out[j].MentionCount {
			return out[i].MentionCount > out[j].MentionCount
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// PurgeNoise 清理库中已存在的噪音实体及其关系，返回删除的实体数与关系数。
//
// dryRun 为 true 时只统计、不写库——清理生产库前先看清楚要动什么。
// 关系按「任一端是噪音实体」删除；删完实体后顺带清理失去引用的孤儿句子
// （复用 cleanupOrphanedSentencesLocked，不在这里重写一遍判定）。
func (g *GraphDB) PurgeNoise(dryRun bool) (int, int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	junk, err := g.noiseEntitiesLocked()
	if err != nil {
		return 0, 0, err
	}
	if len(junk) == 0 {
		return 0, 0, nil
	}

	ids := make([]interface{}, 0, len(junk))
	for _, e := range junk {
		// ★ junk 里的 ID 是占位序号，真正要的是**块 ID**。
		//   块 ID 由内容派生（TripleBlockID / SentenceBlockID），
		//   所以按文本反查而不是信任那个数字。
		blk, err := g.blocksByTextTx(e.Name)
		if err != nil {
			return 0, 0, err
		}
		for _, b := range blk {
			ids = append(ids, b.ID)
		}
	}
	if len(ids) == 0 {
		// ★ 识别到噪音但一个块 ID 都取不到 —— 这是**不一致**，
		//   不能当成「无事可做」静默返回。
		return 0, 0, fmt.Errorf("noise purge: %d junk names matched no blocks",
			len(junk))
	}
	// ★ ph 必须按 **ids** 的长度生成，不能用 len(junk)。
	//
	//   junk 是「噪音实体」数，ids 是它们反查到的**块 ID** 数。
	//   两者只在「一个噪音文本对应一个块」时相等 ——
	//   而迁移期的历史数据里同文本块可以重复（blk_ent_a / blk_ent_b）。
	//
	//   ★ 用 len(junk) 会让占位符与参数个数不符，
	//     SQLite 直接报错（好）；
	//   ★ 更坏的是个数**恰好相同但内容不同** ——
	//     那就是静默删错块。所以这里按 ids 生成。
	ph := placeholders(len(ids))
	args := append(append([]interface{}{}, ids...), ids...)

	// 关系数按 DISTINCT id 统计：两端都是噪音的关系不能被算两次。
	var relCount int
	if err := g.db.QueryRow(
		// ★ 改查 memory_block_edges（2026-10-04）
		//   旧表退场后 relations 空 ⇒ relCount 恒 0 ⇒
		//   PurgeNoise 的 dry-run 报告会说「0 条关系」而实际要删很多。
		`SELECT COUNT(DISTINCT id) FROM memory_block_edges
			 WHERE source_kind='block' AND target_kind='block'
			   AND (source_id IN (`+ph+`) OR target_id IN (`+ph+`))`,
		args...,
	).Scan(&relCount); err != nil {
		return 0, 0, err
	}

	if dryRun {
		return len(junk), relCount, nil
	}

	tx, err := g.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	// ★★★ 改删边而不是旧表（2026-10-04）
	//
	// 旧实现删 relations + entities。旧表退场后这段变成**空操作但报成功** ——
	// 而块侧的边还在 active ⇒ 场景召回照样返回那些噪音记忆。
	//
	// 这正是「写入成功但静默失效」的另一种形态：
	// 用户点了「清理噪音」，界面说成功，记忆一条没少。
	//
	// ★ 只删**关系边**，不删结构边（contains）：
	//   结构边连的是原句块与媒体块，删了会让块体系出现悬空端点。
	//
	// ★★ 块本身要删（2026-10-04 修正）
	//
	//   我一开始写的是「块不删，孤立块交给 OrphanEntities」——
	//   理由是旧实现也有同一条纪律（「一次改动只做一件事」）。
	//   ★★ 但那条纪律针对的是**孤儿块**（边没了、块还在，可能被别人引用），
	//   而 PurgeNoise 删的是**已判定为噪音的块**：
	//   它被删边的后果正是变成孤儿，而留着它等于清理没生效 ——
	//   NoiseEntities 会把它们继续报成噪音，PurgeNoise 再跑又报 4 个。
	//
	//   判据「清理后仍有噪音实体」当场抓住了这一点。
	//
	//   只删**文本块**（这些是噪音实体）：媒体块、原句块不在 junk 里。
	if _, err := tx.Exec(
		`DELETE FROM memory_block_edges
		 WHERE source_kind='block' AND target_kind='block'
		   AND edge_type != 'contains'
		   AND (source_id IN (`+ph+`) OR target_id IN (`+ph+`))`,
		args...); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(
		`DELETE FROM memory_blocks WHERE id IN (`+ph+`)`, ids...); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}

	if _, err := g.cleanupOrphanedSentencesLocked(); err != nil {
		return len(junk), relCount, err
	}
	// 节点没了，场景引用必须跟着对齐：残留引用会让场景看着大、召回却是空的。
	if _, err := g.purgeStaleSceneRefsLocked(); err != nil {
		return len(junk), relCount, err
	}
	return len(junk), relCount, nil
}

// OrphanEntities 列出「没有任何关系的孤立实体」，按 mention_count 降序。
//
// 孤立实体在图里只剩一个名字：关系召回永远够不到它，唯一的副作用是
// 混进实体名向量索引、被自动注入当成相关实体。它们出现的典型路径是
// 噪音实体被清理后留下的另一端（边没了，节点还在），或提取器把关系写重了。
func (g *GraphDB) OrphanEntities() ([]Entity, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.orphanEntitiesLocked()
}

func (g *GraphDB) orphanEntitiesLocked() ([]Entity, error) {
	// ★★★ 改扫 **memory_blocks**（2026-10-04）
	//
	// 旧实现扫 entities 并用 relations 判孤立 —— 而这两张表
	// 在停双写后**都不再增长、不再被写**，于是：
	//
	//	① 扫描结果是迁移时的快照，永远不变
	//	② 新建的块（真正可能孤立的那批）根本不在其中
	//	③ 而它还在排除 source_kind='entity' 的边 ——
	//	   那种 kind 在块化之后已经不产生了
	//
	// 实测（判据 TestPurgeOrphans）：孤立的块一个都没识别出来。
	//
	// ★ 块侧的口径：
	//   · 没有任何**关系边**指向它  → 孤立
	//   · 与媒体块/原句块有 contains 结构边 → 不算孤立
	//     （那是 sentence/document --contains--> block 体系的一部分，
	//      删了会让结构边悬空）
	rows, err := g.db.Query(
		`SELECT b.id, b.text_content, COALESCE(b.semantic_type,''),
		        b.created_at, b.updated_at
		 FROM memory_blocks b
		 WHERE b.text_content != ''
		   AND NOT EXISTS (
		     SELECT 1 FROM memory_block_edges e
		     WHERE (e.source_id = b.id OR e.target_id = b.id)
		       AND e.source_kind = 'block' AND e.target_kind = 'block'
		       AND e.edge_type != 'contains')
		   AND NOT EXISTS (
		     SELECT 1 FROM memory_block_edges e
		     WHERE (e.source_id = b.id OR e.target_id = b.id)
		       AND COALESCE(e.session_id,'') = ''
		       AND e.edge_type = 'contains')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entity
	for rows.Next() {
		var e Entity
		var blockID string
		if err := rows.Scan(&blockID, &e.Name, &e.Type,
			&e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		// ★ ID 退化为序号；blockKey 才是真实块 ID
		//   （PurgeOrphans 按 blockKey 删除 —— 见该函数注释）。
		e.ID = int64(len(out) + 1)
		e.blockKey = blockID
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MentionCount != out[j].MentionCount {
			return out[i].MentionCount > out[j].MentionCount
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// PurgeOrphans 删除没有任何关系的孤立实体，返回删除数。
//
// dryRun 为 true 时只统计。与 PurgeNoise 分开：噪音是「这个名字本身不该在」，
// 孤立是「这个名字虽然可能合理，但它已经不在图里了」——两件事，别混在一个开关里。
func (g *GraphDB) PurgeOrphans(dryRun bool) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	orphans, err := g.orphanEntitiesLocked()
	if err != nil {
		return 0, err
	}
	if dryRun || len(orphans) == 0 {
		return len(orphans), nil
	}

	// ★★ 删的是**块**，不是旧表行（2026-10-04）
	//
	// orphanEntitiesLocked 已改扫 memory_blocks —— 它返回的 Entity.ID
	// 是「本次查询内的序号」，blockKey 才是真实块 ID。
	//
	// 原实现把 e.ID 当旧表行号去 DELETE FROM entities ⇒ 删错对象
	// （而删不到任何行时静默报 0）。
	ids := make([]interface{}, 0, len(orphans))
	for _, e := range orphans {
		if e.blockKey == "" {
			// 没有块 ID = 不是块侧的孤立实体（理论上不该出现）
			continue
		}
		ids = append(ids, e.blockKey)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	if _, err := g.db.Exec(
		`DELETE FROM memory_blocks WHERE id IN (`+placeholders(len(ids))+`)`, ids...); err != nil {
		return 0, err
	}
	if _, err := g.purgeStaleSceneRefsLocked(); err != nil {
		return 0, err
	}
	return len(orphans), nil
}
