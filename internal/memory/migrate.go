package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// 旧媒体实体迁移。
//
// 历史上媒体进 L3 的方式是把正文 marker 反解成普通实体三元组：
//
//	[image/png a1b2c3d4e5f6] 一张紫蓝红三色带图
//	  → 实体「图片 a1b2c3d4e5f6」(type=Media) -内容-> 「一张紫蓝红三色带图」
//
// 这条路径把「媒体」伪装成实体 + 用生成的描述文本当语义索引，正是要废弃的
// 将就机制。迁移做的事：把每条这类实体还原成原生记忆块，用
// sentence --contains--> block 结构边挂到它当时所属的句子上，
// 然后删掉旧实体与它的描述关系。块只按自己的向量被检索。
//
// 迁移是幂等的：实体处理完即删除，重复运行不会重复建块。

// legacyMediaDigestPattern 从旧媒体实体名尾部取出短 digest。
// 名字形如「图片 a1b2c3d4e5f6」——旧实现刻意为每种模态加中文前缀。
var legacyMediaDigestPattern = regexp.MustCompile(`([0-9a-f]{8,64})$`)

// LegacyMediaEntityDigest 从旧媒体实体名里取出短 digest，取不到返回空串。
func LegacyMediaEntityDigest(name string) string {
	name = strings.TrimSpace(name)
	if !strings.Contains(name, " ") {
		return ""
	}
	m := legacyMediaDigestPattern.FindStringSubmatch(name)
	if m == nil {
		return ""
	}
	return m[1]
}

// LegacyMediaResolver 把一个短 digest 解析成可用于 L3 的一等记忆块。
// 解析失败（内容已不存在）返回 false，该实体将被直接删除而不建块。
type LegacyMediaResolver func(shortDigest string) (MemoryBlock, bool)

// MigrateLegacyMediaEntities 把 marker 反解出来的旧媒体实体迁移成原生块。
//
// 返回迁移的块数与删除的旧实体数。任何一步失败都会回滚整个迁移，
// 因为半途中断会留下既没有块也没有实体的句子——信息静默消失。
func (g *GraphDB) MigrateLegacyMediaEntities(resolve LegacyMediaResolver) (blocks, entities int, err error) {
	if resolve == nil {
		return 0, 0, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	rows, err := g.db.Query(`SELECT id, name FROM entities WHERE type = 'Media'`)
	if err != nil {
		return 0, 0, err
	}
	type legacyEntity struct {
		id   int64
		name string
	}
	var legacy []legacyEntity
	for rows.Next() {
		var e legacyEntity
		if err := rows.Scan(&e.id, &e.name); err != nil {
			rows.Close()
			return 0, 0, err
		}
		legacy = append(legacy, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, 0, err
	}
	rows.Close()
	if len(legacy) == 0 {
		return 0, 0, nil
	}

	tx, err := g.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	// 同一份内容可能被多个旧实体引用（重复注入的同一张图），迁移后应指向
	// 同一个块。旧实体名里的**短 digest 就是内容身份**（同内容⇒同短 digest），
	// 因此以它为去重键——块自带负载后不再有独立的 payload_digest 字段可依赖。
	blockIDForShort := make(map[string]string)

	for _, e := range legacy {
		short := LegacyMediaEntityDigest(e.name)
		if short != "" {
			if block, ok := resolve(short); ok {
				id, seen := blockIDForShort[short]
				if !seen {
					if err := insertMigratedBlock(tx, block); err != nil {
						return 0, 0, fmt.Errorf("migrate legacy media %s: %w", short, err)
					}
					blockIDForShort[short] = block.ID
					id = block.ID
					blocks++
				}
				n, err := attachBlockToLegacySentences(tx, e.id, id)
				if err != nil {
					return 0, 0, err
				}
				_ = n
			}
		}
		// 无论能否解析出内容，旧实体与它的描述关系都必须删除：
		// 留着就等于继续用描述文本当媒体索引。
		if _, err := tx.Exec(`DELETE FROM relations WHERE source_id = ? OR target_id = ?`, e.id, e.id); err != nil {
			return 0, 0, err
		}
		if _, err := tx.Exec(`DELETE FROM entities WHERE id = ?`, e.id); err != nil {
			return 0, 0, err
		}
		entities++
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return blocks, entities, nil
}

// insertMigratedBlock 写一条迁移来的块（不经过 PutMemoryBlocks，避免重入锁）。
func insertMigratedBlock(tx *sql.Tx, block MemoryBlock) error {
	if block.ID == "" {
		return fmt.Errorf("migrated block id is required")
	}
	if !validBlockModality(block.Modality) {
		return fmt.Errorf("migrated block %s has invalid modality %q", block.ID, block.Modality)
	}
	vectorJSON, err := json.Marshal(block.Vector)
	if err != nil {
		return err
	}
	created := block.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	_, err = tx.Exec(`INSERT INTO memory_blocks (
		id, modality, text_content, data, mime, size, width, height,
		vector, fingerprint, source, tool, created_at, updated_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(id) DO NOTHING`,
		block.ID, block.Modality, block.Text, block.Data, block.MIME,
		block.Size, block.Width, block.Height, string(vectorJSON), block.Fingerprint,
		block.Source, block.Tool, created, time.Now())
	return err
}

// attachBlockToLegacySentences 把迁移出的块挂到该旧实体当时所属的句子上，
// 并保留那些句子（它们可能只有媒体关系，删实体后就再无关系引用）。
func attachBlockToLegacySentences(tx *sql.Tx, entityID int64, blockID string) (int, error) {
	// ★ 取 sentences.**text** 而不是 id —— 挂载点已从 sentences 表行号
	// 改成原句块 ID（Commit 块化，sentences 表退场）。
	// 行号只在这一行里当作 JOIN 键用，不作为边的端点。
	rows, err := tx.Query(`SELECT DISTINCT s.text FROM sentences s
		JOIN relations r ON r.sentence_id = s.id
		WHERE r.source_id = ? OR r.target_id = ?`, entityID, entityID)
	if err != nil {
		return 0, err
	}
	var sentTexts []string
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			rows.Close()
			return 0, err
		}
		if text == "" {
			continue
		}
		sentTexts = append(sentTexts, text)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()

	n := 0
	for _, text := range sentTexts {
		// 端点 kind 从 'sentence' 改成 'block'（原句由块承载）
		if _, err := tx.Exec(`INSERT OR IGNORE INTO memory_block_edges
			(source_kind, source_id, target_kind, target_id, edge_type)
			VALUES ('block', ?, 'block', ?, 'contains')`,
			SentenceBlockID(text), blockID); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
