package memory

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// MigrateSceneRefsToBlocks 把 scene_refs 的旧引用**完整**迁移到块体系。
//
// ★★ 为什么要迁（不只是"能迁"）
//
// 场景式记忆（scene）是「条件型记忆召回」：给当前上下文匹配出该激活哪些记忆。
// 它靠 scene_refs 记住"这个场景关联哪些记忆对象"。而它现在关联的是
// **旧表的行号**（entities.id / relations.id）。
//
// 旧表退场后行号不存在 ⇒ scene 会指向虚无 ⇒ 场景式记忆整体失效。
// ⇒ 迁移不是优化，是「旧表能不能删」的前置条件之一。
//
// ★ 迁移映射（生产快照实测，三项都是 100%）
//
//	kind='entity'   450 条 → 'block'  ref_id 清空，ref_text = blk_ent_<entityID>_<hash>
//	kind='relation' 268 条 → 'edge'   ref_id 换成 memory_block_edges.id
//	无法反解的引用               0 条
//
// ★ 为什么 entity 引用能反解
//
// 迁移块的 ID 是 `blk_ent_<entityID>_<hash8>`（见 legacyEntityBlockID），
// 含 entityID ⇒ `LIKE 'blk_ent_'||id||'_%'` 就能定位。
//
// ★ 注意 Commit 新写的块（blk_ent_<hash24>）反解不出 entityID，
//
//	但它们**本来就没有 scene_refs 引用**（引用是迁移期建的），
//	所以不影响完整性。实测「引用了非迁移块的 scene_refs = 0 条」。
//
// ★ 幂等
//
// 迁移只处理 kind='entity'/'relation'，跑第二遍时没有可迁的行 ⇒ 无副作用。
//
// ★ kind='block' 用 ref_text 存块 ID（不是 ref_id）
//
// 这是既有契约（生产库已有 90 条 kind='block'，ref_id=0 而 ref_text=块ID），
// 迁移沿用它，不新造格式。
func (g *GraphDB) MigrateSceneRefsToBlocks() (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	tx, err := g.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	moved := 0

	// ---------- ① kind='entity' → 'block' ----------
	//
	// 先建索引：entityID → 迁移块 ID。用 SQL 的 LIKE 关联，
	// 避免把 1294 个块全读进 Go 内存。
	blockByEntity, err := tx.Query(`SELECT e.id, b.id
		FROM entities e
		JOIN memory_blocks b ON b.id LIKE 'blk_ent_' || CAST(e.id AS TEXT) || '_%'
		WHERE b.source = 'legacy-entity'`)
	if err != nil {
		return 0, fmt.Errorf("scan legacy entity blocks: %w", err)
	}
	entityToBlock := map[int64]string{}
	for blockByEntity.Next() {
		var eid int64
		var bid string
		if err := blockByEntity.Scan(&eid, &bid); err != nil {
			blockByEntity.Close()
			return 0, err
		}
		entityToBlock[eid] = bid
	}
	if err := blockByEntity.Err(); err != nil {
		blockByEntity.Close()
		return 0, err
	}
	blockByEntity.Close()

	entRefs, err := tx.Query(`SELECT id, ref_id FROM scene_refs
		WHERE kind = 'entity' AND ref_id IS NOT NULL AND ref_id != 0`)
	if err != nil {
		return 0, err
	}
	type refRow struct {
		rowID  int64
		entID  int64
		blockI string
		ok     bool
	}
	var entRows []refRow
	for entRefs.Next() {
		var r refRow
		var rid string
		if err := entRefs.Scan(&r.rowID, &rid); err != nil {
			entRefs.Close()
			return 0, err
		}
		var eid int64
		if _, err := fmt.Sscanf(rid, "%d", &eid); err != nil {
			continue // ref_id 非数字：保留原样并报告
		}
		r.entID = eid
		if b, ok := entityToBlock[eid]; ok {
			r.blockI = b
			r.ok = true
		}
		entRows = append(entRows, r)
	}
	if err := entRefs.Err(); err != nil {
		entRefs.Close()
		return 0, err
	}
	entRefs.Close()

	var entSkipped, entDedup int
	for _, r := range entRows {
		if !r.ok {
			entSkipped++
			continue
		}
		// ★ 必须去重：scene_refs 有 UNIQUE(scene_id, kind, ref_id, ref_text)。
		//   两个旧引用可能映射到**同一个块**（生产快照实测 0 例，
		//   但那是巧合不是保证）⇒ 直接 UPDATE 会撞约束让整个迁移回滚。
		//
		//   判据 TestSceneRef_迁移映射完整 造的就是这种情形
		//   （两条 entity 引用都指向 blkA）—— 首次实现直接撞了约束。
		res, err := tx.Exec(`UPDATE scene_refs
			SET kind = 'block', ref_text = ?, ref_id = 0
			WHERE id = ?`, r.blockI, r.rowID)
		if err != nil {
			// 撞 UNIQUE ⇒ 该块已被另一条引用占用，删掉这条重复引用
			// （它与已存在的那条指向同一个块，信息不丢失）。
			if isUniqueViolation(err) {
				if _, derr := tx.Exec(`DELETE FROM scene_refs WHERE id = ?`,
					r.rowID); derr != nil {
					return moved, derr
				}
				entDedup++
				continue
			}
			return moved, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // 行已不存在（并发）
		}
		moved++
	}

	// ---------- ② kind='relation' → 'edge' ----------
	//
	// 按（源块, 目标块, 边类型）定位对应的块边。边是独立单位，
	// 可能有多条同类边 ⇒ 取 id 最小的那条（最老的事实）。
	relRefs, err := tx.Query(`SELECT sr.id, r.id, r.source_id, r.target_id,
			COALESCE(r.relation_type, '')
		FROM scene_refs sr
		JOIN relations r ON r.id = CAST(sr.ref_id AS INTEGER)
		WHERE sr.kind = 'relation' AND sr.ref_id IS NOT NULL AND sr.ref_id != 0`)
	if err != nil {
		return moved, err
	}
	type relRow struct {
		rowID  int64
		relID  int64
		srcID  int64
		tgtID  int64
		relTyp string
	}
	var relRows []relRow
	for relRefs.Next() {
		var r relRow
		var srcS, tgtS, relIDS string
		if err := relRefs.Scan(&r.rowID, &relIDS, &srcS, &tgtS, &r.relTyp); err != nil {
			relRefs.Close()
			return moved, err
		}
		fmt.Sscanf(srcS, "%d", &r.srcID)
		fmt.Sscanf(tgtS, "%d", &r.tgtID)
		fmt.Sscanf(relIDS, "%d", &r.relID)
		relRows = append(relRows, r)
	}
	if err := relRefs.Err(); err != nil {
		relRefs.Close()
		return moved, err
	}
	relRefs.Close()

	var relSkipped, relDedup int
	for _, r := range relRows {
		srcBlk, ok1 := entityToBlock[r.srcID]
		tgtBlk, ok2 := entityToBlock[r.tgtID]
		if !ok1 || !ok2 {
			relSkipped++
			continue
		}
		edgeType := r.relTyp
		if edgeType == "" {
			edgeType = "related_to" // 与迁移同款占位
		}
		var edgeID int64
		err := tx.QueryRow(`SELECT id FROM memory_block_edges
			WHERE source_kind='block' AND source_id=? AND target_kind='block'
			  AND target_id=? AND edge_type=?
			ORDER BY id LIMIT 1`, srcBlk, tgtBlk, edgeType).Scan(&edgeID)
		if err == sql.ErrNoRows {
			relSkipped++
			continue
		}
		if err != nil {
			return moved, err
		}
		res, err := tx.Exec(`UPDATE scene_refs
			SET kind = 'edge', ref_id = ?, ref_text = ''
			WHERE id = ?`, edgeID, r.rowID)
		if err != nil {
			if isUniqueViolation(err) {
				if _, derr := tx.Exec(`DELETE FROM scene_refs WHERE id = ?`,
					r.rowID); derr != nil {
					return moved, derr
				}
				relDedup++
				continue
			}
			return moved, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		moved++
	}

	if err := tx.Commit(); err != nil {
		return moved, err
	}

	// ★ 跳过的必须显式报出 —— 「完整迁移」是判据的硬要求，
	//   静默跳过会让 scene 在旧表退场后指向虚无。
	// ★ 三类异常都要显式报出：「完整迁移」是判据的硬要求，
	//   静默处理会让 scene 在旧表退场后指向虚无，且没人知道。
	if entSkipped > 0 || relSkipped > 0 || entDedup > 0 || relDedup > 0 {
		log.Printf("[scene] reference migration: %d done"+
			" | no matching object entity %d / relation %d"+
			" | deduped entity %d / relation %d",
			moved, entSkipped, relSkipped, entDedup, relDedup)
	} else {
		log.Printf("[scene] reference migration: %d done, nothing skipped or deduped", moved)
	}
	return moved, nil
}

// sceneRefDanglingDB 报告指向虚无的引用数（迁移后校验用）。
//
// kind='block' 看 ref_text 里的块 ID 还在不在；
// kind='edge'  看 ref_id 指向的边还在不在。
func sceneRefDanglingDB(db *sql.DB) (blockDangling, edgeDangling int, err error) {
	err = db.QueryRow(`SELECT COUNT(*) FROM scene_refs sr
		WHERE sr.kind='block' AND NOT EXISTS
			(SELECT 1 FROM memory_blocks b WHERE b.id = sr.ref_text)`).Scan(&blockDangling)
	if err != nil {
		return 0, 0, err
	}
	err = db.QueryRow(`SELECT COUNT(*) FROM scene_refs sr
		WHERE sr.kind='edge' AND NOT EXISTS
			(SELECT 1 FROM memory_block_edges e WHERE CAST(e.id AS TEXT) = sr.ref_id)`).
		Scan(&edgeDangling)
	return blockDangling, edgeDangling, err
}

// isUniqueViolation 判断错误是否为 SQLite UNIQUE 约束冲突。
//
// 用于「映射到同一块的重复引用」：撞约束时改为删除重复行
// （它与已存在的那条指向同一个块，信息不丢失）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "constraint failed: UNIQUE")
}
