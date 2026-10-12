package memory

import (
	"fmt"
	"strings"
)

// 块 union 化数据迁移：把媒体字节从旧 CAS 回填进块自身的 data 列。
//
// 背景
// ----
// media.Store/CAS 淘汰后，媒体字节成为块自己的负载（memory_blocks.data）。
// 但存量库里 data 为空、payload_digest 仍指向 CAS 的 blob —— 必须一次性
// 把字节搬进块，否则 CAS 目录一删，BlockData 就取不到字节（图永久丢失）。
//
// 为什么不靠 MediaBlockID 重算身份
// --------------------------------
// 存量块 ID 是旧式 blk_<ts>_<n>（对话期生成），不是内容派生的
// blk_media_<sha>。迁移**保留原块 ID**，只把 data 按 payload_digest 回填 ——
// 边、场景、向量全都挂在原 ID 上，换 ID 会让它们集体悬空。

// BlockDataBackfillStats 报告回填结果。
type BlockDataBackfillStats struct {
	Candidates int // data 为空且 payload_digest 非空的块
	Filled     int // 成功回填字节
	Missing    int // payload_digest 在 CAS 里取不到字节（内容已不存在）
	SizeFilled int // 回填的总字节数（供日志/对账）
}

func (s BlockDataBackfillStats) String() string {
	return fmt.Sprintf("候选 %d，回填 %d（%d 字节），内容缺失 %d",
		s.Candidates, s.Filled, s.SizeFilled, s.Missing)
}

// BlockPayloadResolver 按 CAS digest 取回原始字节；取不到返回 false。
type BlockPayloadResolver func(digest string) ([]byte, bool)

// CountBlockDataBackfillCandidates 统计待回填的块数（data 空、digest 非空）。
// 报告模式用：不改任何数据。
func (g *GraphDB) CountBlockDataBackfillCandidates() (int, error) {
	if err := g.ensurePayloadDigestColumn(); err != nil {
		return 0, err
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	var n int
	err := g.db.QueryRow(`SELECT COUNT(*) FROM memory_blocks
		WHERE payload_digest != ''
		  AND (data IS NULL OR length(data) = 0)`).Scan(&n)
	return n, err
}

// BackfillBlockData 把块负载从旧 CAS 回填到 data 列。
//
// resolve 按 payload_digest 取字节（通常包一层 media.Store.Get）。
// 单事务、全成功或全回滚。幂等：只处理 data 为空且 payload_digest 非空的块，
// 重复运行不会覆盖已回填的字节。
//
// 内容在 CAS 里已不存在的块（resolve 返回 false）计入 Missing 并跳过 ——
// 这是无法补救的既有数据损失（CAS 曾被清理过），不是迁移故障，不该阻断
// 其余块的回填。
//
// ★ 不校验「回填字节的 sha256 == payload_digest」：那是 CAS 的内部不变量，
// 已由 media.Store.Get 在读出时校验（见其 digest mismatch 检查）。这里
// 再算一次 sha256 对上万张图是纯浪费。
func (g *GraphDB) BackfillBlockData(resolve BlockPayloadResolver) (BlockDataBackfillStats, error) {
	var st BlockDataBackfillStats
	if resolve == nil {
		return st, fmt.Errorf("backfill: resolver is required")
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	// ★ 块 union 化后新库的 schema 不再含 payload_digest 列，
	//   迁移只对旧库有意义。新库必须先补上这列，否则查询报 "no such column"。
	if err := g.ensurePayloadDigestColumn(); err != nil {
		return st, err
	}

	rows, err := g.db.Query(`SELECT id, payload_digest FROM memory_blocks
		WHERE payload_digest != ''
		  AND (data IS NULL OR length(data) = 0)
		ORDER BY id`)
	if err != nil {
		return st, err
	}
	type cand struct{ id, digest string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.digest); err != nil {
			rows.Close()
			return st, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return st, err
	}
	rows.Close()
	st.Candidates = len(cands)
	if len(cands) == 0 {
		return st, nil
	}

	tx, err := g.db.Begin()
	if err != nil {
		return st, err
	}
	defer tx.Rollback()

	for _, c := range cands {
		data, ok := resolve(c.digest)
		if !ok || len(data) == 0 {
			st.Missing++
			continue
		}
		if _, err := tx.Exec(
			`UPDATE memory_blocks SET data = ? WHERE id = ?`, data, c.id); err != nil {
			return st, fmt.Errorf("backfill block %s (digest %s): %w", c.id, c.digest, err)
		}
		st.Filled++
		st.SizeFilled += len(data)
	}

	if err := tx.Commit(); err != nil {
		return st, err
	}
	return st, nil
}

// ProbeBackfillAvailability 不写库，只按 payload_digest 逐个试取字节，
// 返回能取到的块数。报告模式用：在 -apply 之前先看 CAS 命中率。
func (g *GraphDB) ProbeBackfillAvailability(resolve BlockPayloadResolver) (int, error) {
	if resolve == nil {
		return 0, fmt.Errorf("probe: resolver is required")
	}
	// ★ 块 union 化后新库的 schema 不再含 payload_digest 列，
	//   迁移只对旧库有意义。新库先补列（空列 → 查询返回 0 候选，安全空操作）。
	if err := g.ensurePayloadDigestColumn(); err != nil {
		return 0, err
	}
	g.mu.RLock()
	rows, err := g.db.Query(`SELECT payload_digest FROM memory_blocks
		WHERE payload_digest != ''
		  AND (data IS NULL OR length(data) = 0)`)
	if err != nil {
		g.mu.RUnlock()
		return 0, err
	}
	var digests []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			g.mu.RUnlock()
			return 0, err
		}
		digests = append(digests, d)
	}
	rerr := rows.Err()
	rows.Close()
	g.mu.RUnlock()
	if rerr != nil {
		return 0, rerr
	}
	hit := 0
	for _, d := range digests {
		if data, ok := resolve(d); ok && len(data) > 0 {
			hit++
		}
	}
	return hit, nil
}

// SampleFilledBlockIDs 返回前 n 个已带字节的块 id（供回填后验证 BlockData）。
func (g *GraphDB) SampleFilledBlockIDs(n int) ([]string, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT id FROM memory_blocks
		WHERE length(data) > 0 ORDER BY id LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ensurePayloadDigestColumn 保证 memory_blocks 里有 payload_digest 列。
//
// 块 union 化后新库的 CREATE 语句已删掉该列；旧库还有它（存量生产库里
// 它正是回填 data 的来源）。回填迁移要对两者都能跑：
//   - 旧库：列在，ALTER 报 "duplicate column" 被忽略 → 正常回填
//   - 新库：列不在，ALTER 加上它（值为空）→ 查询返回 0 候选，安全空操作
//
// SQLite 没有 ALTER ... ADD COLUMN IF NOT EXISTS，靠"错误即忽略"实现幂等。
func (g *GraphDB) ensurePayloadDigestColumn() error {
	_, err := g.db.Exec(`ALTER TABLE memory_blocks ADD COLUMN payload_digest TEXT DEFAULT ''`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("ensure payload_digest: %w", err)
	}
	return nil
}
