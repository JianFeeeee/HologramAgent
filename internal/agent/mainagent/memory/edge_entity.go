package memory

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
)

// ★★★ 关系边：独立单位
//
// ============================================================
// 为什么边必须是独立实体，而不是「两点 + 一个类型字符串」
// ============================================================
// 原 schema 有：
//
//	UNIQUE(source_kind, source_id, target_kind, target_id, edge_type)
//
// ⇒ 同一对节点间只能存**一条**同类型边。后果实测：
//
//	写 A --属于--> B (session=1, conf=0.9)
//	写 A --属于--> B (session=2, conf=0.5)
//	⇒ 第二条被 INSERT OR IGNORE 静默丢弃，session/confidence 全丢
//
//	而这正是「报告边数 980、实际写入 959」的根因（dd2c996）——
//	当时我把它当成「重复数据」报出来就完事了，
//	**真正的问题是边表从设计上就存不下多条同类边**。
//
// ============================================================
// 边作为独立单位的三条要求
// ============================================================
//	① 同类边可并存    —— 不同 session / turn / confidence 是不同的事实
//	② 边承载属性      —— confidence / session_id / status 挂在边上而非关系上
//	③ 边有自己的身份  —— 可以被引用（merged_into 指向它）而不只是被遍历
//
// 对应旧 relations 表的列：confidence / status / session_id / turn_id /
// date_bucket。date_bucket 是索引优化而非语义，先不入。

// 边状态。
const (
	// EdgeActive 是有效边。软删除/仲裁靠它判断旧值是否作废
	// （与旧 relations.status='active' 语义一致）。
	EdgeActive = "active"
	// EdgeDeleted 是被取代/被删除的边。**保留而非物理删除** ——
	// 「旧号 4379 停用」本身是有信息量的（它解释了为什么现在打不通），
	// 与 arbitration.go 的 Superseded 同一套语义。
	EdgeDeleted = "deleted"
	// EdgeMerged 是源节点被合并后，其边重定向到目标块；
	// 源块记 merged_into 指向目标块，历史边留在源块上。
	EdgeMerged = "merged"
)

// RelationEdgeData 是关系边的属性集。
type RelationEdgeData struct {
	// Confidence 是置信度（0~1）。旧 relations 表同名列。
	Confidence float64
	// SessionID / TurnID 记录边的来源会话与轮次。
	// Recall 的 sessionFilter 依赖它。
	SessionID string
	TurnID    int
	// Status 是 EdgeActive / EdgeDeleted / EdgeMerged。
	Status string
	// MergedInto 在 EdgeMerged 时指向目标块 ID。
	MergedInto string
}

// AddRelationBlockEdge 写一条**带属性的关系边**。
//
// ★ 与 AddMemoryBlockEdge 的区别（后者保留给结构边）
//
//	AddMemoryBlockEdge      结构边（contains / 迁移关系）
//	                       —— 不并存多条，无属性，用 UNIQUE 保证幂等
//	AddRelationBlockEdge    关系边（主语--关系-->宾语）
//	                       —— ★ 可并存多条、承载属性
//
// 两者共用一张表（memory_block_edges），靠属性列是否为空区分，
// 但**唯一性约束不同**：结构边受 UNIQUE 保护，关系边不受。
func (g *GraphDB) AddRelationBlockEdge(sourceID, targetID, edgeType string,
	data RelationEdgeData) error {

	g.mu.Lock()
	defer g.mu.Unlock()

	if sourceID == "" || targetID == "" || edgeType == "" {
		return fmt.Errorf("edge: endpoints and type required")
	}
	// 端点必须存在 —— 沿用 AddMemoryBlockEdge 的校验，
	// 避免建出指向虚无节点的边。
	for _, id := range []string{sourceID, targetID} {
		var n int
		if err := g.db.QueryRow(
			`SELECT COUNT(*) FROM memory_blocks WHERE id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("edge: block %s does not exist", id)
		}
	}

	status := data.Status
	if status == "" {
		status = EdgeActive
	}

	// ★ 不带 OR IGNORE / 不受 UNIQUE 约束 ——
	//   这正是「边是独立单位」的落点：同一对节点可并存多条同类边。
	_, err := g.db.Exec(`INSERT INTO memory_block_edges
		(source_kind, source_id, target_kind, target_id, edge_type,
		 confidence, session_id, turn_id, status, merged_into)
		VALUES ('block', ?, 'block', ?, ?, ?, ?, ?, ?, ?)`,
		sourceID, targetID, edgeType, data.Confidence,
		nullIfEmpty(data.SessionID), data.TurnID, status,
		nullIfEmpty(data.MergedInto))
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// RelationEdgesBetween 取两个块之间的全部关系边（**不合并同类**）。
//
// 这是「边是独立单位」的直接体现：返回切片长度可以 > 1。
func (g *GraphDB) RelationEdgesBetween(sourceID, targetID string) ([]MemoryBlockEdge, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.relationEdgesTx(sourceID, targetID)
}

func (g *GraphDB) relationEdgesTx(sourceID, targetID string) ([]MemoryBlockEdge, error) {
	rows, err := g.db.Query(`SELECT id, source_kind, source_id, target_kind, target_id,
			edge_type, confidence, session_id, turn_id, status, merged_into
		FROM memory_block_edges
		WHERE source_kind='block' AND source_id=? AND target_kind='block' AND target_id=?
		ORDER BY confidence DESC, id ASC`, sourceID, targetID)
	if err != nil {
		return nil, err
	}
	return scanRelationEdges(rows)
}

// AllRelationEdges 返回全部关系边（供遍历/合并用）。
func (g *GraphDB) AllRelationEdges() ([]MemoryBlockEdge, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT id, source_kind, source_id, target_kind, target_id,
			edge_type, confidence, session_id, turn_id, status, merged_into
		FROM memory_block_edges ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return scanRelationEdges(rows)
}

func scanRelationEdges(rows *sql.Rows) ([]MemoryBlockEdge, error) {
	defer func() { _ = rows.Close() }()
	var out []MemoryBlockEdge
	for rows.Next() {
		var e MemoryBlockEdge
		var sess, merged sql.NullString
		var conf sql.NullFloat64
		var status sql.NullString
		var turn sql.NullInt64
		if err := rows.Scan(&e.ID, &e.SourceKind, &e.SourceID, &e.TargetKind, &e.TargetID,
			&e.Type, &conf, &sess, &turn, &status, &merged); err != nil {
			return nil, err
		}
		e.Confidence = conf.Float64
		e.SessionID = sess.String
		e.Status = status.String
		e.MergedInto = merged.String
		if turn.Valid {
			e.TurnID = int(turn.Int64)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ensureRelationEdgeUniqueness 处理**既有表**上的 UNIQUE 约束。
//
// ★ 为什么必须重建表
// ------------------
// SQLite 的 UNIQUE 约束是**表定义的一部分**，没有 ALTER TABLE ... DROP CONSTRAINT。
// 而旧库（生产 98 块 / 590 边、测试库 448 块）建表时都带了这个约束，
// 它让「边是独立单位」在既有库上无法生效 —— 同一对节点写第二条同类边会被丢。
//
// 做法：读出既有边 → 建新表（无 UNIQUE）→ 搬回去 → 删旧表 → 改名。
// 全程在一个事务里；任何一步失败整体回滚。
//
// ★ 为什么不能在 detect 阶段默默跳过
//
// 跳过会让「既有库不支持并存同类边、新库支持」成为事实，
// 而这种不一致极难排查（本地能测、生产静默丢边）。
// ⇒ 这里宁可重建表也要保证语义一致，并打日志说明发生了什么。
func ensureRelationEdgeUniqueness(tx *sql.Tx) {
	// 检测既有表是否带 UNIQUE
	rows, err := tx.Query(`SELECT sql FROM sqlite_master
		WHERE type='table' AND name='memory_block_edges'`)
	if err != nil {
		return
	}
	var ddl string
	if rows.Next() {
		_ = rows.Scan(&ddl)
	}
	_ = rows.Close()
	if ddl == "" || !strings.Contains(ddl, "UNIQUE") {
		return // 新表或已升格
	}

	log.Printf("[graph] edge table upgrade: rebuilding memory_block_edges (dropping UNIQUE to allow same-kind edges to coexist)")
	if _, err := tx.Exec(`CREATE TABLE memory_block_edges_new (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_kind TEXT NOT NULL,
		source_id TEXT NOT NULL,
		target_kind TEXT NOT NULL,
		target_id TEXT NOT NULL,
		edge_type TEXT NOT NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		confidence REAL DEFAULT 0,
		session_id TEXT DEFAULT '',
		turn_id INTEGER DEFAULT 0,
		status TEXT DEFAULT '',
		merged_into TEXT DEFAULT ''
	)`); err != nil {
		log.Printf("[graph] edge table upgrade failed (keeping original constraint): %v", err)
		return
	}
	// 搬数据（只搬旧表已有的 6 列，新列留默认值）
	if _, err := tx.Exec(`INSERT INTO memory_block_edges_new
		(id, source_kind, source_id, target_kind, target_id, edge_type, created_at)
		SELECT id, source_kind, source_id, target_kind, target_id, edge_type, created_at
		FROM memory_block_edges`); err != nil {
		log.Printf("[graph] edge table data migration failed, rolling back: %v", err)
		if _, derr := tx.Exec(`DROP TABLE memory_block_edges_new`); derr != nil {
			log.Printf("[graph] edge table upgrade: rollback of temp table failed: %v", derr)
		}
		return
	}
	if _, err := tx.Exec(`DROP TABLE memory_block_edges`); err != nil {
		log.Printf("[graph] edge table drop failed, rolling back: %v", err)
		if _, derr := tx.Exec(`DROP TABLE memory_block_edges_new`); derr != nil {
			log.Printf("[graph] edge table upgrade: rollback of temp table failed: %v", derr)
		}
		return
	}
	if _, err := tx.Exec(`ALTER TABLE memory_block_edges_new RENAME TO memory_block_edges`); err != nil {
		log.Printf("[graph] edge table rename failed: %v", err)
		return
	}
	// 索引要重建（DROP TABLE 会带走）
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_memory_block_edges_source
			ON memory_block_edges(source_kind, source_id)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_block_edges_target
			ON memory_block_edges(target_kind, target_id)`,
	} {
		_, _ = tx.Exec(idx)
	}
	log.Printf("[graph] edge table upgrade complete")
}

// BFSBlocks 从 startID 出发，沿**关系边**展开 depth 层，返回全部可达节点。
//
// ★★★ 这就是「联想」的实现（docs/zh/recall-as-association.md）
//
// 人听到「值班室分机号是多少」时：想起主语 → 想起值 → 想起旧值 → 想起人。
// 节点是召回对象，N 层 BFS 是把「自动附带的上下文」取回来。
//
// ★ 为什么必须是「节点 + BFS」而不是「只召回主语块」
//
//	宾语块 "4324" 孤立召回时不知道自己来自哪里。
//	若只召回主语块 ⇒ 要么丢它（漏掉「用户直接问值」的场景），
//	要么加「来源标注」特判（联想本来就不需要标注）。
//
// ★ 双向遍历
//
//	人联想到「老周值班」是从「老周 --值班分机--> 4324」**反向**走来的。
//	所以正反向都要展开，否则网会退化成有向森林。
//
// ★ depth=0 只返回自身（不是空）
//
//	命中节点本身就是要返回的内容之一。
func (g *GraphDB) BFSBlocks(startID string, depth int) ([]MemoryBlock, error) {
	if depth < 0 {
		depth = 0
	}
	if startID == "" {
		return nil, fmt.Errorf("bfs: empty start id")
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	// 起点必须存在
	var exists int
	if err := g.db.QueryRow(
		`SELECT COUNT(*) FROM memory_blocks WHERE id = ?`, startID).Scan(&exists); err != nil {
		return nil, err
	}
	if exists == 0 {
		return nil, fmt.Errorf("bfs: start block %s does not exist", startID)
	}

	// visited 同时充当去重集合：网会织成"部分连通"，
	// 不去重会在环上无限展开。
	visited := map[string]bool{startID: true}
	frontier := []string{startID}
	// ★ ordered 记录 BFS 的**发现顺序**：起点在前，邻居按层、按到达顺序。
	//   它是「离查询多近」这个语义的载体 —— 丢了顺序，
	//   「最近的上下文在前」就没了（同图两次 BFS 结果不同）。
	ordered := []string{startID}

	for d := 0; d < depth && len(frontier) > 0; d++ {
		var next []string
		for _, cur := range frontier {
			// ★ 正向 + 反向：前驱（谁指向我）与后继（我指向谁）都是邻居。
			//
			// ★★ 这里**不能**加 COALESCE(session_id,'')='' 的过滤 ——
			//   那个条件是给结构边去重用的（结构边 session_id 恒空），
			//   而关系边的 session_id 恰恰**非空**（它是边的属性）。
			//   加上它会把所有关系边滤掉 ⇒ BFS 一个邻居都走不到。
			//   实测：边写对了（session=s1 status=active）但 BFS 只返回起点。
			rows, err := g.db.Query(`SELECT target_id FROM memory_block_edges
				WHERE source_kind='block' AND source_id=?
				UNION
				SELECT source_id FROM memory_block_edges
				WHERE target_kind='block' AND target_id=?`,
				cur, cur)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var nb string
				if err := rows.Scan(&nb); err != nil {
					rows.Close()
					return nil, err
				}
				if nb != "" && !visited[nb] {
					visited[nb] = true
					// ★ 记录**发现顺序** —— BFS 的语义全在这里。
					//   去重用 map，出序用这个 slice，两者不能混。
					ordered = append(ordered, nb)
					next = append(next, nb)
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return nil, err
			}
		}
		frontier = next
	}

	// ★ 取块内容必须按**发现顺序**，不能用 map 遍历。
	//
	// visited 是 map（判定去重要 O(1)），而它的迭代顺序是随机的 ——
	// 于是同一个图连续两次 BFS 得到不同顺序的结果，
	// 「离查询多近」这个语义就没了，测试与用户输出都会飘。
	//
	// 正确做法：用入队时累积的 ordered（见上），终点直接传它。
	//
	// 兜底：若 ordered 为空（depth=0 时不展开），退回 map 遍历 ——
	// 此时只有一个节点，顺序无所谓。
	if len(ordered) == 0 {
		for id := range visited {
			ordered = append(ordered, id)
		}
	}
	return g.blocksByIDsLocked(ordered)
}

func (g *GraphDB) blocksByIDsLocked(ids []string) ([]MemoryBlock, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := g.db.Query(`SELECT `+blockColumns+`
		FROM memory_blocks WHERE id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemoryBlock
	for rows.Next() {
		b, err := scanBlockRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// BlockNeighbour 是一条关系边连同它的对端块 —— 「谁和谁有什么关系」的完整读法。
//
// ★ 为什么不复用 RelationEdgeData：那个类型只是**属性载荷**
//
//	（confidence/session/turn/status/merged_into），刻意不含 ID 与端点，
//	因为 AddRelationBlockEdge 的入参就是「端点已知、只传属性」。
//
//	读的方向相反：要端点、要 ID。所以另立类型而不是硬塞进去。
type BlockNeighbour struct {
	// EdgeID 是 memory_block_edges.id。场景引用 kind='edge' 指向它。
	EdgeID int64
	// SourceID / TargetID 是两端块 ID。
	SourceID string
	TargetID string
	// EdgeType 是关系名（「偏好」「使用」…）。
	EdgeType string
	// Peer 是**对端**块：SourceID == 查询块时为 Target，反之为 Source。
	// 查询块自己是起点，不需要重复带出。
	Peer MemoryBlock
	// IsOutgoing 标明方向：true 表示 Peer 是这条关系的宾语。
	// social 层区分「A 认识 B」与「B 认识 A」时必须看它。
	IsOutgoing bool

	RelationEdgeData
}

// ═══════════════════════════════════════════════════════════════
//  MergeBlocks —— 块合并（2026-10-04）
//
//  这是旧表退场的最后一环：MergeEntities 只改 entities/relations，
//  块侧完全不动 ⇒ 「张先生」改名后，块还叫「张先生」，
//  召回照样命中它（判据 TestMergeEntities 就是这么红的）。
//
//  ★★ 与旧实现的本质差异
//
//  旧（entities/relations）
//      改 relations.source_id / target_id 即可 —— 实体 ID 不变，
//      改名（UPDATE entities.name）就完成了合并的「改名」语义。
//
//  新（blocks/edges）
//      块 ID 是**内容派生**的：blk_ent_<hash(name)>。
//      「张先生」与「张三」是两个不同的块，合并意味着**源块消失**，
//      它的边改指向目标块。
//
//  ⇒ 所以本函数不能只重定向端点，还要：
//      ① 删源块（不留 @merged_ 残留，与旧实现同款）
//      ② 处理结构边（contains）—— 否则原句块指向已删的块，端点悬空
//      ③ 同步 scene_refs —— 否则场景里挂一条永远召不回的幽灵
//
//  ★ 边的合并语义：**重定向 + 去自环**，不合并属性。
//    「李四喜欢咖啡」+「李四讨厌咖啡」合并到「王五」后都变成
//    「王五—喜欢/讨厌→咖啡」—— 这是**两条不同的关系**（edge_type 不同），
//    都保留；而「李四喜欢咖啡」+「王五喜欢咖啡」合并后同端同类型，
//    那才是重复，必须收敛（否则「王五喜欢咖啡」出现两次）。
// ═══════════════════════════════════════════════════════════════

// MergeBlocks 把 sourceText 块的边合并进 targetText 块，源块随之消失。
//
// 返回**重定向的边数**（不含被去重丢弃的重复边）。
// 幂等：源块不存在时返回 (0, nil)。
func (g *GraphDB) MergeBlocks(sourceText, targetText string) (int, error) {
	n, srcID, dstID, err := g.mergeBlocksLocked(sourceText, targetText)
	// ★ 上报必须在锁释放后（见 GraphDB.onAccess 的死锁说明）。
	//   合入事件驱动星图的「两节点消失又出现」：源块被删、目标块接纳，
	//   所以 Removed 给 src、Blocks 给 dst（前端据此播「消失→出现」时序）。
	if err == nil && srcID != "" {
		g.emitAccess(AccessEvent{
			Op:      "merge",
			Blocks:  []string{dstID},
			Removed: []string{srcID},
			Merged:  []MergePair{{From: srcID, To: dstID}},
		})
	}
	return n, err
}

// mergeBlocksLocked 是 MergeBlocks 的实体（持 g.mu），
// 额外返回源/目标块 ID 供外部上报（上报不能在持锁时做）。
func (g *GraphDB) mergeBlocksLocked(sourceText, targetText string) (int, string, string, error) {
	sourceText = strings.TrimSpace(sourceText)
	targetText = strings.TrimSpace(targetText)
	if sourceText == "" || targetText == "" {
		return 0, "", "", fmt.Errorf("merge requires both source and target")
	}
	if sourceText == targetText {
		return 0, "", "", fmt.Errorf("merge source and target are identical: %q", sourceText)
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	src, err := g.blockByTextTxLocked(sourceText)
	if err != nil {
		return 0, "", "", err
	}
	if src == nil {
		// ★ 必须报错，不能返回 (0, nil)。
		//
		//   我一开始写的是「幂等：源块已合并过」—— 那是对的**理由**
		//   配上了错的**行为**：源块不存在有两种截然不同的原因：
		//
		//     a) 已经合并过了（重试，正常路径）
		//     b) 名字拼错了 / 从没写过（调用方的 bug）
		//
		//   返回 (0, nil) 把两者都变成「静默成功」——
		//   而这正是记忆系统里最贵的一类 bug：调用方以为合并了，
		//   库里其实什么都没变。
		//
		//   旧 MergeEntities 在这里返回 error，是对的。
		//   幂等由**调用方**判断（合并后不要再合并同一个源），
		//   不该由存储层替它猜。
		return 0, "", "", fmt.Errorf("merge source block %q does not exist", sourceText)
	}
	dst, err := g.blockByTextTxLocked(targetText)
	if err != nil {
		return 0, "", "", err
	}
	if dst == nil {
		// ★ 目标不存在必须报错。
		//   返回 0 会让调用方以为合并成功了，而实际上什么都没做 ——
		//   那正是「静默失效」的一种。
		return 0, "", "", fmt.Errorf("merge target block %q does not exist", targetText)
	}
	if src.ID == dst.ID {
		return 0, "", "", nil
	}

	tx, err := g.db.Begin()
	if err != nil {
		return 0, "", "", err
	}
	defer tx.Rollback()

	// ── ① 重定向关系边：source → target ──────────────────────
	//
	// ★ 只动**关系边**，不动结构边（contains）。
	//   contains 的端点是「原句块」与「关系边」或「块」，
	//   把它改成 target 会让原句指向一个语义不同的块。
	res, err := tx.Exec(
		`UPDATE memory_block_edges SET source_id = ?
		 WHERE source_kind = 'block' AND source_id = ?
		   AND COALESCE(session_id,'') != ''`,
		dst.ID, src.ID)
	if err != nil {
		return 0, "", "", fmt.Errorf("redirect source edges: %w", err)
	}
	nOut, _ := res.RowsAffected()

	res, err = tx.Exec(
		`UPDATE memory_block_edges SET target_id = ?
		 WHERE target_kind = 'block' AND target_id = ?
		   AND COALESCE(session_id,'') != ''`,
		dst.ID, src.ID)
	if err != nil {
		return 0, "", "", fmt.Errorf("redirect target edges: %w", err)
	}
	nIn, _ := res.RowsAffected()

	// ── ② 去自环 ─────────────────────────────────────────────
	//
	// 重定向后 source→X 与 target→X 都成了 target→X。
	// 同端**同类型**的边是重复（重复陈述同一件事）；
	// 同端**不同类型**的边不是（喜欢 vs 讨厌 是两回事）。
	//
	// ★ 判定必须含 edge_type：只按 (source,target) 去重会把
	//   「王五喜欢咖啡」与「王五讨厌咖啡」误删一条。
	//
	// 保留 id 最小的那条，其余标记 deleted（不硬删）：
	//   scene_refs 可能还指向它们。
	if _, err := tx.Exec(
		`UPDATE memory_block_edges SET status = 'deleted'
		 WHERE id NOT IN (
		     SELECT MIN(id) FROM memory_block_edges
		     WHERE source_kind='block' AND target_kind='block'
		       AND (source_id = ? OR target_id = ?)
		       AND COALESCE(session_id,'') != ''
		     GROUP BY source_id, target_id, edge_type
		 )
		   AND source_kind='block' AND target_kind='block'
		   AND (source_id = ? OR target_id = ?)
		   AND COALESCE(session_id,'') != ''
		   AND COALESCE(status,'') != 'deleted'`,
		dst.ID, dst.ID, dst.ID, dst.ID); err != nil {
		return 0, "", "", fmt.Errorf("dedupe self-loops: %w", err)
	}

	// ── ③ 清理指向源块的结构边 ────────────────────────────────
	//
	// contains 的端点可能是块（媒体挂载、迁移关系）。
	// 源块删了之后这些边端点悬空 —— 而悬空端点会让
	// graphNodeExists 校验失败，后续任何引用它的写入都被拒。
	if _, err := tx.Exec(
		`DELETE FROM memory_block_edges
		 WHERE (source_kind='block' AND source_id=? AND COALESCE(session_id,'')='')
		    OR (target_kind='block' AND target_id=? AND COALESCE(session_id,'')='')`,
		src.ID, src.ID); err != nil {
		return 0, "", "", fmt.Errorf("drop structural edges of source: %w", err)
	}

	// ── ④ 删源块 ─────────────────────────────────────────────
	if _, err := tx.Exec(`DELETE FROM memory_blocks WHERE id = ?`, src.ID); err != nil {
		return 0, "", "", fmt.Errorf("delete source block: %w", err)
	}

	// ── ⑤ 场景引用同步 ───────────────────────────────────────
	//
	// 源块的引用必须换成目标块 —— 而不是删掉：
	// 「张先生那次值班」这个场景仍然存在，只是人换了名字。
	if _, err := tx.Exec(
		`UPDATE scene_refs SET ref_text = ?
		 WHERE kind = 'block' AND ref_text = ?`,
		dst.ID, src.ID); err != nil {
		return 0, "", "", fmt.Errorf("repoint scene refs: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, "", "", err
	}
	return int(nOut + nIn), src.ID, dst.ID, nil
}

// blockByTextTxLocked 按文本取最早的块（调用方已持锁）。
func (g *GraphDB) blockByTextTxLocked(text string) (*MemoryBlock, error) {
	blocks, err := g.blocksByTextTx(text)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, nil
	}
	return &blocks[0], nil
}
