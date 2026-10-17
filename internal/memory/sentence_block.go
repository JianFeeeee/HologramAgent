package memory

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// SentenceBlockID 由**原句文本**派生原句块的 ID。
//
// ★ 为什么在 memory 包而不是 distill
// -------------------------------
// 两条路径都需要它：迁移（MigrateLegacyTextEntities）与
// 蒸馏（distill.WritePayload）。而 distill 已导入 memory，
// 若放在 distill 会形成 memory → distill 的导入环。
//
// ★ 为什么 ID 必须由内容派生
// --------------------------
// 方案 A 要删 sentences 表，原句就得由块承载，而块 ID 必须**幂等**
// —— 迁移与蒸馏都会重复跑同一条原句。
// 迁移块自己用的是 blk_ent_<entityID>_<hash>（依赖行号，不是内容派生），
// 不能复用；这里用 sha256(TrimSpace(text))。
func SentenceBlockID(text string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(text)))
	return "blk_src_" + hex.EncodeToString(h[:12])
}

// SentenceBlockSource 是原句块的 source 标记。
const SentenceBlockSource = "sentence"

// NewSentenceBlock 构造承载原句的块。
//
// ★ 刻意**不带向量**
// ------------------
// 它是溯源锚点，不参与向量召回。理由是实测过的：
//
//	「13000端口」(7字)               容易被召回
//	「13010/13011 而非 12011」(20字)  召不回
//
// 长整句的句向量会把关键值稀释掉 —— 让原句进召回只会引入噪声。
// 省一次 embedding，也省一次无用的向量存储。
func NewSentenceBlock(text string, createdAt, updatedAt time.Time) MemoryBlock {
	return MemoryBlock{
		ID:        SentenceBlockID(text),
		Modality:  BlockText,
		Text:      strings.TrimSpace(text),
		Source:    SentenceBlockSource,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

// TripleBlockID 由三元组内容派生「值块」的 ID。
//
// ★ 为什么三元组要变成**两个块 + 一条边**，而不是一个块
// ------------------------------------------------
// 三元组是「主语 --关系--> 宾语」。若压成一个块（`<主语>|<关系>=<宾语>`），
// 就无法回答「X 的关系有哪些」「Y 被哪些主语指向」这类图查询 ——
// 而那正是 Recall 的主要用法（`docs/zh/recall-capability-tiers.md`）。
//
// ⇒ 主语块、宾语块各自可独立被召回（它们是独立的事实），
//
//	关系由边承载。
//
// 幂等：ID 完全由内容派生，重复提交同一三元组得到同一对块 ⇒ 天然去重。
func TripleBlockID(name string) string {
	h := sha256.Sum256([]byte(strings.TrimSpace(name)))
	return "blk_ent_" + hex.EncodeToString(h[:12])
}

// putTripleBlocksTx 把一条三元组写成「主语块 --关系--> 宾语块」。
//
// ★ 不带向量
// ----------
// Commit 这条路径上**没有 embedding provider**（Commit 的签名里就没有）。
// 而编造零向量比不写更坏：零向量块会参与召回并永远排在最后，
// 那是「静默的错误记忆」（见 ToMemoryBlock 的注释）。
//
// ⇒ 块先入库、无向量；召回侧按「有无向量」分别处理
//
//	（有向量的走语义召回，无向量的靠符号路按内容串匹配 ——
//	实测跑分里 `[exact]` 那 14 条正是这类）。
//
// ★ 时间戳：不写 now，而是**留空**
// --------------------------------
// 第一版给块打了 time.Now()，结果被 putBlockTx 的 ON CONFLICT 覆盖了
// 迁移块的时序 —— 实测迁移测试直接抓到：
//
//	blk_ent_edb5a71b11d814fdc36e8332 应继承实体时间 2026-01-01 10:00
//	实际 2026-10-04 08:04（时序被抹平）
//
// 而时序是**仲裁的前提**（internal/memory/arbitration.go 靠
// CreatedAt 判断「谁取代了谁」）。抹平时序会让仲裁失效。
//
// ⇒ 留空的语义是「时间未指定」：putBlockTx 不会用它覆盖既有值，
//
//	也不该被仲裁当成时间依据（仲裁跳过无时间的块 —— 见其规则 1）。
//	真正需要时序的块由迁移/蒸馏显式传 CreatedAt。
//
// putTripleBlocksTx 把一条三元组写成两端块 + 一条关系边。
//
// ★ 返回两端块 ID 与边 ID（2026-10-04）
//
// 调用方需要它们来挂场景引用 —— 场景引用的端点必须是**块 ID**
// （kind='block'）与**边 ID**（kind='edge'）。此前这些值无处可取，
// 于是 Commit 只能传旧表的 relationID/entityID，
// 写出一批退场后会悬空的引用。
//
// 返回值顺序：sourceBlockID, targetBlockID, edgeID, createdBlockIDs（无块时为空/0）。
//
// ★ sessionID / turnID 来自 commit() 的参数而非 Triple ——
//
//	Triple 是「三元组内容」，会话是「这次写入的上下文」，
//	两者本就不该混在一个结构里。
func putTripleBlocksTx(tx *sql.Tx, t Triple, sessionID string, turnID int) (string, string, int64, []string, error) {
	// created 收集本次**真正新建**的块 ID（已存在则为 UPDATE，不算新建）。
	//
	// ★ 为什么要细分到每个块：星图的「生长」动画只该在真·新块上播。
	//   若按「本次 commit 有写入」就算新建，重复提交同一事实（LLM 高频重试）
	//   会让旧块反复「长出来」—— 看着像新记忆，实际是同一个东西。
	var created []string
	// ★ SemanticType 必须带（2026-10-04）
	//
	//   Triple.SubjectType / ObjectType 此前只写旧 entities.type，
	//   块侧完全丢 —— 于是 Commit 标注的「这是 Person / 那是 Animal」
	//   在召回时读回来是 ("block","block")。
	//   判据 TestGraphCommit_CarriesAllFields 抓的就是这个。
	//
	// ★ 留空是合法的（很多三元组不带类型），block.go 的 ON CONFLICT
	//   对该列用「空值不覆盖」语义，后写的不带标注不会抹掉已有的。
	src := MemoryBlock{
		ID:           TripleBlockID(t.Subject),
		Modality:     BlockText,
		Text:         strings.TrimSpace(t.Subject),
		Source:       "triple",
		SemanticType: strings.TrimSpace(t.SubjectType),
	}
	dst := MemoryBlock{
		ID:           TripleBlockID(t.Object),
		Modality:     BlockText,
		Text:         strings.TrimSpace(t.Object),
		Source:       "triple",
		SemanticType: strings.TrimSpace(t.ObjectType),
	}
	for _, b := range []MemoryBlock{src, dst} {
		isNew, err := putBlockTx(tx, b)
		if err != nil {
			return "", "", 0, nil, err
		}
		if isNew {
			created = append(created, b.ID)
		}
	}
	// ★ status 必须显式写 'active'（2026-10-04）
	//
	//   边表的 status 有 DEFAULT，但那个默认值是**空串**。
	//   而所有读取路径都按 status='active' 过滤 ——
	//   于是写进去的边**永远召不回来**，且没有任何报错。
	//
	//   这是典型的「写入成功但静默失效」：只有端到端判据能发现，
	//   而 scene 的 4 个测试全部同时变红，才把它逼出来。
	//
	// ★ 用**关系边**写入器（addBlockEdgeTx 是迁移专用的去重插入器：
	//   INSERT OR IGNORE 会把同对节点的第二条同类型边静默吞掉，
	//   而关系边按设计允许并存多条 —— 52e4596 已把全局 UNIQUE 移除）。
	// ★★ 幂等：同 (source, target, edge_type, session_id) 必须收敛为一条（2026-10-04）
	//
	// 旧 relations 表靠 UNIQUE(source_id,target_id,relation_type,session_id) 保证；
	// 而 52e4596 为支持「同类边并存」把边表的 UNIQUE **全部移除**了。
	//
	// ⇒ 后果：重复提交同一三元组会产生**重复的边**。
	//   实测（判据 TestCommitDedupSameSession）：同会话提交两次 → 2 条边。
	//   而记忆写入是**高频重试**的（LLM 反复提交同一事实），
	//   重复边会让召回刷屏、场景权重虚高。
	//
	// ★ 去重口径必须含 session_id：
	//
	//	同会话内重复 = 同一条事实（收敛）
	//	跨会话重复   = 多次陈述（并存，见 TestCommitDedupDifferentSession）
	//
	//   若不含 session_id，跨会话的多次陈述会被错误压成一条。
	var existingEdge int64
	err := tx.QueryRow(`SELECT id FROM memory_block_edges
		WHERE source_kind = 'block' AND source_id = ?
		  AND target_kind = 'block' AND target_id = ?
		  AND edge_type = ? AND COALESCE(session_id, '') = ?
		ORDER BY id LIMIT 1`,
		src.ID, dst.ID, strings.TrimSpace(t.Relation), sessionID).Scan(&existingEdge)
	if err == nil {
		return src.ID, dst.ID, existingEdge, created, nil
	}
	if err != sql.ErrNoRows {
		return "", "", 0, nil, err
	}

	res, err := tx.Exec(
		// ★★ session_id / turn_id 必须写进去（2026-10-04）
		//
		//   Recall 的 sessionFilter 依赖边表的这两列。缺了它，
		//   任何按会话过滤的召回都返回空 —— 而测试若不显式带
		//   sessionFilter 就发现不了（无过滤时召回照常工作）。
		// ★★ confidence 必须写进去（2026-10-04）
		//
		//   它不只是「插件标注的置信度被丢弃」那么局部：
		//   confidence 是边表里**唯一的质量信号**，被三处依赖 ——
		//     ① RecallSorted 的 SortRelevance 排序（`ORDER BY confidence DESC`）
		//     ② 场景排序（tagSceneTripleTx 拿它当 weight）
		//     ③ 蒸馏时的置信度传播
		//
		//   不写 ⇒ 全部为 0 ⇒ 排序退化成插入序、场景权重恒 0，
		//   而**没有任何报错**。
		//
		// ★ 同时写 updated_at 不可行：边表**没有**这一列
		//   （升格成独立边时只加了 confidence/session/turn/status/merged_into）。
		//   旧 relations 表有，所以照抄会报 no such column。
		`INSERT INTO memory_block_edges
		 (source_kind, source_id, target_kind, target_id, edge_type,
		  confidence, status, session_id, turn_id)
		 VALUES ('block', ?, 'block', ?, ?, ?, 'active', ?, ?)`,
		src.ID, dst.ID, strings.TrimSpace(t.Relation), t.Confidence, sessionID, turnID)
	if err != nil {
		return "", "", 0, nil, err
	}
	edgeID, err := res.LastInsertId()
	return src.ID, dst.ID, edgeID, created, err
}

// ── 媒体块 ──────────────────────────────────────────────

// MediaBlockID 由**媒体内容字节**派生媒体块的 ID。
//
// ★ 为什么媒体块的 ID 也必须内容派生
// ---------------------------------
// 块 ID 是本仓的记忆身份，既有约定一律内容派生：
//   - 原句块 blk_src_<sha256(text)[:12]>
//   - 实体块 blk_ent_<sha256(name)[:12]>
//   - 蒸馏块 blk_<sha256(sentence+subject+dim+value)[:12]>
//
// 而媒体块此前用时间戳+序号（blk_<unixnano>_<n>），是唯一的例外。
//
// 淘汰 CAS 之后，块自己承载内容，于是「同一份内容只应有一个块」这条
// 不再由外部的内容寻址层保证，而必须由**块 ID 本身**保证：
// 同内容 ⇒ 同 ID ⇒ PutMemoryBlocks 的 ON CONFLICT 天然去重。
// 顺带让跨插件边界的引用（只带 ID 前缀）也能唯一定位。
//
// ★ 为何把 MIME 也掺进去：同样字节在不同 MIME 下是不同的媒体
// （同一段字节按 image/png 与 application/octet-stream 解读得到不同结果），
// 且 MIME 参与向量计算，掺入可避免「先入者定 MIME」的静默错配。
func MediaBlockID(mime string, data []byte) string {
	h := sha256.New()
	h.Write([]byte(strings.TrimSpace(mime)))
	h.Write([]byte{0})
	h.Write(data)
	return "blk_media_" + hex.EncodeToString(h.Sum(nil)[:12])
}

// BlockByIDPrefix 按块 ID 的**前缀**取块（前缀唯一时命中）。
//
// 用途：跨进程/跨层的轻量引用（如插件只带块 ID 的前 12 位）。
// 前缀匹配到多条时返回 false —— 宁可不解析也不猜一个（猜错=挂错媒体）。
func (g *GraphDB) BlockByIDPrefix(prefix string) (MemoryBlock, bool, error) {
	prefix = strings.TrimSpace(prefix)
	// 过短前缀不解析：块 ID 形如 blk_media_<12hex>，
	// 一个一两字符的前缀会把一大批块都 LIKE 中，猜一个等于挂错媒体。
	// 至少要求 12 字符（内容派生 hash 的常用短引用长度）。
	if len(prefix) < 12 {
		return MemoryBlock{}, false, nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT `+blockColumns+`
		FROM memory_blocks WHERE id LIKE ? LIMIT 2`, prefix+"%")
	if err != nil {
		return MemoryBlock{}, false, err
	}
	defer rows.Close()
	var got []MemoryBlock
	for rows.Next() {
		b, err := scanBlockRow(rows)
		if err != nil {
			return MemoryBlock{}, false, err
		}
		got = append(got, b)
	}
	if err := rows.Err(); err != nil {
		return MemoryBlock{}, false, err
	}
	if len(got) != 1 {
		return MemoryBlock{}, false, nil // 0 条或歧义（≥2）都不解析
	}
	return got[0], true, nil
}

// BlockData 取单个块的负载字节（按需加载）。
//
// ★ 为何单独一个方法：blockColumns 刻意不含 data（召回只比向量，
// 带上负载会让每次全表召回读入几十 MB）。需要字节的路径显式调本方法。
func (g *GraphDB) BlockData(blockID string) ([]byte, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	var data []byte
	err := g.db.QueryRow(`SELECT data FROM memory_blocks WHERE id = ?`, blockID).Scan(&data)
	if err != nil {
		return nil, err
	}
	return data, nil
}

// StaleBlockVectors 返回向量为空、或向量 fingerprint 与当前空间不符的块。
//
// 用途：启动时把历史媒体块/文档块的向量重算到当前多模态空间（换模型、
// 换维度、首次接入嵌入服务、嵌入服务离线后重上线）。
//
// 为何需要它：向量是召回的唯一依据，而历史块可能是在另一个空间里算的。
// fingerprint 不一致的向量不能参与余弦比较（两个坐标系的值比大小无意义），
// 必须重算而不是留着当"看起来还行"的噪声。
//
// 只取非文本块：文本块的向量由文本路径维护，不在多模态空间迁移范围内。
func (g *GraphDB) StaleBlockVectors(currentFP string) ([]MemoryBlock, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	// 只取元数据（blockColumns 不含 data）：负载按需由 BlockData 单取。
	// 迁移路径逐块处理，不需要把全部字节一次性读进内存。
	rows, err := g.db.Query(`SELECT `+blockColumns+`
		FROM memory_blocks
		WHERE modality != ?
		  AND (vector IS NULL OR vector = '' OR vector = 'null' OR fingerprint != ?)
		ORDER BY created_at`, string(BlockText), currentFP)
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

// SetBlockVector 给单个块写入新向量与指纹（迁移用）。只改向量列，不碰负载。
func (g *GraphDB) SetBlockVector(blockID string, vec []float64, fp string) error {
	vectorJSON, err := json.Marshal(vec)
	if err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	_, err = g.db.Exec(`UPDATE memory_blocks SET vector = ?, fingerprint = ?, updated_at = ?
		WHERE id = ?`, string(vectorJSON), fp, time.Now(), blockID)
	return err
}
