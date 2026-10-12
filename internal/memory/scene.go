package memory

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// ──────────────────────────────────────────────
// 场景式关联召回
//
// 词法与向量召回都建立在「字面/语义相似」上，而带**条件**的记忆天生不吃这一套：
//
//	QQ回复禁用Markdown格式 --规定--> 纯文本不用Markdown
//	老大 --偏好--> QQ回复禁用Markdown格式
//
// 这些规则约束的是「回 QQ 消息这个场面」，而不是某个话题。用户措辞里没出现
// 「QQ」「Markdown」时它们召不回来；而用户只说了「QQ」时，词法路又会把上百个
// 含 QQ 的实体按建表顺序排前面，把规则本体挤出注入预算（实测：输入「QQ回复格式」
// 命中 148 个实体，规则排第 32，注入只取前 5 —— 规则根本没进去）。
//
// 场景引用把「触发条件」变成一等索引：节点记住自己属于哪个场面，
// 场面重现时按场景直接取回，与措辞无关。
//
// 场景键的形态是**分层字符串**，用 `/` 分隔，由宽到窄：
//
//	chan:qq                          通道级（在 QQ 上收发消息）
//	chan:qq/peer:group_1027993713    再窄一层（具体群）
//	tool:qq_get_message              工具级（取回消息正文这一步）
//	chan:doc/src:qq                  文档归档的来源
//
// 召回按**前缀**匹配：当前场景 `chan:qq` 会取回它自己以及所有更窄的场景
// （`chan:qq/...`）——越窄的场景越具体，不该被漏掉；反向不成立。
// ──────────────────────────────────────────────

// maxSceneKeyLen 是场景键的长度上限。场景键要进索引、要参与前缀比较，
// 过长只说明有人把正文塞进了键里（那就该用句子/实体，而不是场景）。
const maxSceneKeyLen = 96

// NormalizeSceneKey 规范化场景键：按 `/` 分层、每层小写、层内空白与非法字符
// 归一成 `_`（连续多个只留一个）。
//
// 为什么要归一：场景键是**索引键**，`chan:QQ` 与 `chan:qq` 必须是同一个场景，
// 否则同一条规则会因为写入时大小写不同而分裂成两个召不齐的场景。
// 为什么空白不算层级分隔符：来源名里天然带空格（如「老大2026-09-04 12:27
// QQ私聊图片」），把它当层级会把一个平面名字拆成三层伪层级。
// 归一结果为空（全是非法字符）时返回空串，调用方应视为「没有场景」。
func NormalizeSceneKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	key = strings.ToLower(key)

	parts := strings.Split(key, "/")
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		seg := normalizeSceneSegment(p)
		if seg != "" {
			segs = append(segs, seg)
		}
	}
	out := strings.Join(segs, "/")
	if len(out) > maxSceneKeyLen {
		out = strings.TrimRight(out[:maxSceneKeyLen], "/")
	}
	return out
}

// normalizeSceneSegment 归一单层场景：小写、空白与非法字符 → `_`（压缩连续）。
func normalizeSceneSegment(seg string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range seg {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r >= '\u4e00' && r <= '\u9fff',
			r == ':' || r == '-' || r == '.':
			b.WriteRune(r)
			lastUnderscore = false
		case r == '_' || unicode.IsSpace(r):
			if !lastUnderscore && b.Len() > 0 {
				b.WriteRune('_')
				lastUnderscore = true
			}
		default:
			// 其它字符（标点、表情）归一成 `_` 而不是静默丢弃：
			// 「A/B」与「A_B」是两个不同的来源，不能塌成一个场景。
			if !lastUnderscore && b.Len() > 0 {
				b.WriteRune('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// ChannelScene 由输入/输出通道名构造场景键（`qq` → `chan:qq`）。
func ChannelScene(source string) string {
	s := NormalizeSceneKey(source)
	if s == "" {
		return ""
	}
	return "chan:" + s
}

// ToolScene 由工具名构造场景键（`qq_get_message` → `tool:qq_get_message`）。
func ToolScene(tool string) string {
	s := NormalizeSceneKey(tool)
	if s == "" {
		return ""
	}
	return "tool:" + s
}

// SceneStat 是单个场景的规模摘要（供 introspection / 运维观察）。
type SceneStat struct {
	Key       string `json:"key"`
	Refs      int    `json:"refs"`
	Relations int    `json:"relations"`
	Entities  int    `json:"entities"`
	// Strength 是该场景被重现（强化）的次数；Features 是它长出的特征数。
	// 两者一起说明「这个场景是不是真的在涌现」，而不是被一次性写出来的。
	Strength int `json:"strength"`
	Features int `json:"features"`
	// Origin 是这条场景来自哪条路：declared（主动声明）或 emergent（被动涌现）。
	Origin    string    `json:"origin"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SceneRecall 是一次场景召回的产物。
type SceneRecall struct {
	Scenes    []string   `json:"scenes"`
	Relations []Relation `json:"relations"`
	Entities  []Entity   `json:"entities"`
	// Blocks 是该场景下的一等记忆块（图/音/文）。
	//
	// 为什么场景要能取回块：块是流水线里最细的子项目，而「那场对话里发过来的
	// 那张图」只记住名字是没用的——场面重现时要把块本身带回来。
	Blocks []MemoryBlock `json:"blocks,omitempty"`
	// Documents 是该场景下的 L3 文档节点 id（文档的场景由来源派生，见 TagSceneDocument）。
	Documents []string `json:"documents,omitempty"`
}

// tagSceneTx 在事务内把「关系 + 实体」挂到场景上（幂等 upsert）。
//
// weight 取关系的置信度：场景内的记忆也要能排序，置信度是目前唯一现成的
// 质量信号。重复写入同一节点只刷新 weight 与时间，不产生重复引用。
func tagSceneTx(tx *sql.Tx, sceneKey string, relationID int64, entityIDs []int64, weight float64) error {
	key := NormalizeSceneKey(sceneKey)
	if key == "" {
		return nil
	}
	if relationID != 0 {
		if err := tagSceneRefTx(tx, key, "relation", relationID, "", weight); err != nil {
			return err
		}
	}
	for _, eid := range entityIDs {
		if eid != 0 {
			if err := tagSceneRefTx(tx, key, "entity", eid, "", weight); err != nil {
				return err
			}
		}
	}
	return nil
}

// tagSceneRefTx 在事务内把一个节点挂到场景上（幂等 upsert）。
//
// kind ∈ relation | entity | block | document。textID 供非数值主键的节点使用
// （块与文档的 id 是字符串），数值型节点传 0 并用 id。
//
// 为什么 weight 取 MAX 而不是覆盖：场景内的记忆也要能排序，置信度是目前唯一
// 现成的质量信号；同一节点被低置信度的重复写入命中的，不该把它从场景前排挤下去。
func tagSceneRefTx(tx *sql.Tx, sceneKey, kind string, id int64, textID string, weight float64) error {
	key := NormalizeSceneKey(sceneKey)
	if kind == "" || (id == 0 && textID == "") {
		return nil
	}
	if key == "" {
		return nil
	}
	if _, err := tx.Exec(
		`INSERT INTO scenes (key) VALUES (?)
		 ON CONFLICT(key) DO UPDATE SET updated_at = CURRENT_TIMESTAMP`, key); err != nil {
		return fmt.Errorf("upsert scene %q: %w", key, err)
	}
	var sceneID int64
	if err := tx.QueryRow(`SELECT id FROM scenes WHERE key = ?`, key).Scan(&sceneID); err != nil {
		return fmt.Errorf("select scene %q: %w", key, err)
	}
	if weight <= 0 {
		weight = 1.0
	}
	if _, err := tx.Exec(
		`INSERT INTO scene_refs (scene_id, kind, ref_id, ref_text, weight) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(scene_id, kind, ref_id, ref_text)
		 DO UPDATE SET weight = MAX(weight, excluded.weight), decayed_at = CURRENT_TIMESTAMP`,
		sceneID, kind, id, textID, weight); err != nil {
		return fmt.Errorf("upsert scene ref %s/%d%s: %w", kind, id, textID, err)
	}
	return nil
}

// TagScene 给一批已有的关系补挂场景（存量记忆的场景标注入口）。
//
// 为什么需要「事后标注」：场景是后引入的维度，此前写下的规则（那批 QQ 规则
// 就是典型）没有任何场景引用，不补挂就永远吃不到场景召回。
func (g *GraphDB) TagScene(sceneKey string, relationIDs []int64) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	key := NormalizeSceneKey(sceneKey)
	if key == "" || len(relationIDs) == 0 {
		return 0, nil
	}
	tx, err := g.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	n := 0
	for _, rid := range relationIDs {
		var sourceID, targetID int64
		var confidence float64
		if err := tx.QueryRow(
			`SELECT source_id, target_id, confidence FROM relations WHERE id = ?`, rid,
		).Scan(&sourceID, &targetID, &confidence); err != nil {
			if err == sql.ErrNoRows {
				continue
			}
			return n, err
		}
		if err := tagSceneTx(tx, key, rid, []int64{sourceID, targetID}, confidence); err != nil {
			return n, err
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// TagSceneByEntityGlob 把「任一端实体名匹配 GLOB pattern」的活跃关系标进场景，
// 返回标注的关系数。dryRun 时只统计。
//
// 这是**存量引导**用的窄口子：pattern 由调用方显式给出，不做任何自动猜测
// ——猜错的代价是把无关记忆钉死在某个场景上，之后每次进入该场景都会被注入，
// 比漏标更难发现。
//
// 为什么用 GLOB 而不是 LIKE：LIKE 对 ASCII **不区分大小写**，于是 `%QQ%`
// 会把对象里带 `/data/homeagent` 的路径类记忆（生产数据目录、email-mcp、
// dify-ops技能路径…实测 7 条）一起卷进「QQ 场景」。GLOB 区分大小写，
// `*QQ*` 只命中真正写作 QQ 的那些名字。
func (g *GraphDB) TagSceneByEntityGlob(sceneKey, pattern string, dryRun bool) (int, error) {
	key := NormalizeSceneKey(sceneKey)
	if key == "" || pattern == "" {
		return 0, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	// ★★★ 改走块体系（2026-10-04）
	//
	// 旧实现查 relations JOIN entities 按名字 GLOB。entities/relations 退场后
	// 这条路直接断 —— 而 scene 是「条件型记忆召回」的底座，它断了整个
	// 场景式记忆失效。
	//
	// 块体系里的等价物：按**块文本**匹配找关系边的两端。
	// 只取非 deleted 的关系边（与旧 WHERE r.status='active' 同义）。
	//
	// ★ 用 LIKE 而不是 GLOB：GLOB 模式串里 `*?[]` 都是元字符，
	// 而 memgc --entity-glob 传的是**用户输入**，容易带出意外模式。
	rows, err := g.db.Query(
		`SELECT e.id, e.source_id, e.target_id, e.confidence
		 FROM memory_block_edges e
		 JOIN memory_blocks sb ON sb.id = e.source_id
		 JOIN memory_blocks tb ON tb.id = e.target_id
		 WHERE e.source_kind = 'block' AND e.target_kind = 'block'
		   AND COALESCE(e.status, '') != 'deleted'
		   AND (sb.text_content LIKE ? OR tb.text_content LIKE ?)`,
		globToLike(pattern), globToLike(pattern))
	if err != nil {
		return 0, err
	}
	type cand struct {
		edgeID           int64
		sourceID, target string
		confidence       float64
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.edgeID, &c.sourceID, &c.target, &c.confidence); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if dryRun {
		return len(cands), nil
	}

	tx, err := g.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, c := range cands {
		// ★ 端点都是**块 ID** ⇒ kind 用 'block'（不是 'entity'）；
		//   关系边本身另挂一条 kind='edge'。
		if err := tagSceneRefTx(tx, key, "block", 0, c.sourceID, c.confidence); err != nil {
			return 0, err
		}
		if err := tagSceneRefTx(tx, key, "block", 0, c.target, c.confidence); err != nil {
			return 0, err
		}
		if err := tagSceneRefTx(tx, key, "edge", c.edgeID, "", c.confidence); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(cands), nil
}

// RecallByScene 按当前场景取回被钉在该场景上的记忆（前缀匹配，越窄越算命中）。
//
// 排序：weight（=写入时置信度）降序 → 关系时间降序。取回的是**关系全文**
// （含 relation_type 与 JOIN 出的原句），不只是实体名——带条件的规则本体
// 长在关系上，只给名字等于没召回。
//
// limit 同时约束关系数与实体数，避免一个场景把注入预算吃光。
func (g *GraphDB) RecallByScene(scenes []string, limit int) (*SceneRecall, error) {
	out := &SceneRecall{}
	var keys []string
	seen := make(map[string]bool)
	for _, s := range scenes {
		k := NormalizeSceneKey(s)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return out, nil
	}
	out.Scenes = keys
	if limit <= 0 {
		limit = 8
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	// 前缀条件：(key = ? OR key LIKE ? || '/%')，用 '/' 兜底防止
	// `chan:qq` 误吞 `chan:qq2` 这种同前缀但不同层的场景。
	conds := make([]string, 0, len(keys))
	args := make([]interface{}, 0, len(keys)*2)
	for _, k := range keys {
		conds = append(conds, `(s.key = ? OR s.key LIKE ? || '/%')`)
		args = append(args, k, k)
	}
	where := "(" + strings.Join(conds, " OR ") + ")"

	// ★★★ 改走块体系（2026-10-04）
	//
	// 旧实现 JOIN relations + entities + sentences。旧表退场后这条查询
	// 直接断 —— 而 RecallByScene 是**场景式记忆的取回端**，
	// 它断了比写入端更致命：写入端断了会报错，取回端断了会**静默召回空**。
	//
	// 块体系里的等价物：
	//
	//	关系    → memory_block_edges（边自带 relation_type / confidence /
	//	         session_id / turn_id / status / created_at）
	//	两端名  → memory_blocks.text_content
	//	原句    → 原句块 blk_src_<hash>，走 contains 边回溯
	//
	// ★ 原句不能丢：带条件的规则本体长在句子里，只给关系名等于没召回。
	relQuery := `SELECT e.id, sb.id, tb.id, sb.text_content, tb.text_content,
			e.edge_type, e.confidence, COALESCE(e.status, ''), e.session_id,
			e.turn_id, e.created_at, COALESCE(srt.text, ''),
			MAX(sr.weight) AS w
		FROM scene_refs sr
		JOIN scenes s ON sr.scene_id = s.id
		JOIN memory_block_edges e ON sr.kind = 'edge' AND sr.ref_id = e.id
		JOIN memory_blocks sb ON sb.id = e.source_id
		JOIN memory_blocks tb ON tb.id = e.target_id
		LEFT JOIN (
			SELECT c.target_id AS edge_id, MIN(b.text_content) AS text
			FROM memory_block_edges c
			JOIN memory_blocks b ON b.id = c.source_id
			WHERE c.edge_type = 'contains' AND c.target_kind = 'edge'
			GROUP BY c.target_id
		) srt ON srt.edge_id = e.id
		WHERE ` + where + ` AND COALESCE(e.status, '') = 'active'
		GROUP BY e.id
		ORDER BY w DESC, e.created_at DESC, e.id DESC
		LIMIT ?`
	relArgs := append(append([]interface{}{}, args...), limit)
	rows, err := g.db.Query(relQuery, relArgs...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rel Relation
		var w float64
		// ★ 端点扫进块 ID 字段（旧的 int64 SourceID/TargetID 留空 —— 它们是
		//   relations 行号，块体系下无意义）。
		// ★ DateBucket / SentenceID 不再有来源：旧表退场后边表不存这两列，
		//   日期可从 created_at 推，原句 ID 由 contains 边回溯得到。
		if err := rows.Scan(&rel.ID, &rel.SourceBlockID, &rel.TargetBlockID,
			&rel.SourceName, &rel.TargetName,
			&rel.RelationType, &rel.Confidence, &rel.Status, &rel.SessionID,
			&rel.TurnID, &rel.CreatedAt, &rel.SentenceText, &w); err != nil {
			rows.Close()
			return nil, err
		}
		// 旧表退场后边表不存 date_bucket，从 created_at 推日期桶。
		if rel.DateBucket == "" && !rel.CreatedAt.IsZero() {
			rel.DateBucket = rel.CreatedAt.Format("2006-01-02")
		}
		out.Relations = append(out.Relations, rel)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// ★★★ 改走块体系（2026-10-04）
	//
	// 旧实现 JOIN entities。旧表退场后这条路断 —— 而「两端实体」是场景
	// 召回里**能重建关系语义的那一半**（「值班室分机号 → 4324」与
	// 「billing服务 → 4324」只凭关系名无法区分）。
	//
	// 块体系里两端块已在 rel.SourceBlockID / rel.TargetBlockID 上，
	// 不必再查一次 —— 但排序口径要保持：weight 降序 → mention_count → id。
	// 块没有 mention_count，用 created_at 兼之（语义近似：先出现的更稳定）。
	//
	// ★ 用 map 去重：两端块可能出现在多条关系的端点上，
	//   而关系查询已按 limit 截断 —— 实体列表不应再套一次 limit 而漏掉
	//   已召回关系的两端。
	seenBlock := make(map[string]bool)
	for _, rel := range out.Relations {
		for _, blk := range []struct {
			id   string
			name string
		}{{rel.SourceBlockID, rel.SourceName}, {rel.TargetBlockID, rel.TargetName}} {
			if blk.id == "" || blk.name == "" || seenBlock[blk.id] {
				continue
			}
			seenBlock[blk.id] = true
			out.Entities = append(out.Entities, Entity{
				ID:        0, // 旧表行号已无意义
				Name:      blk.name,
				Type:      "block",
				CreatedAt: rel.CreatedAt,
				UpdatedAt: rel.CreatedAt,
			})
		}
	}

	blockQuery := `SELECT b.id, b.modality, b.text_content, b.mime,
			b.size, b.width, b.height, b.fingerprint, b.source, b.tool,
			COALESCE(b.scene, ''), COALESCE(b.semantic_type, ''),
			b.created_at, b.updated_at, MAX(sr.weight) AS w
		FROM scene_refs sr
		JOIN scenes s ON sr.scene_id = s.id
		JOIN memory_blocks b ON sr.kind = 'block' AND b.id = sr.ref_text
		WHERE ` + where + `
		GROUP BY b.id
		ORDER BY w DESC, b.created_at DESC
		LIMIT ?`
	blockArgs := append(append([]interface{}{}, args...), limit)
	brows, err := g.db.Query(blockQuery, blockArgs...)
	if err != nil {
		return nil, err
	}
	defer brows.Close()
	for brows.Next() {
		var b MemoryBlock
		var w float64
		if err := brows.Scan(&b.ID, &b.Modality, &b.Text, &b.MIME,
			&b.Size, &b.Width, &b.Height, &b.Fingerprint, &b.Source, &b.Tool,
			&b.Scene, &b.SemanticType, &b.CreatedAt, &b.UpdatedAt, &w); err != nil {
			return nil, err
		}
		out.Blocks = append(out.Blocks, b)
	}
	if err := brows.Err(); err != nil {
		return nil, err
	}

	docQuery := `SELECT sr.ref_text, MAX(sr.weight) AS w
		FROM scene_refs sr JOIN scenes s ON sr.scene_id = s.id
		WHERE ` + where + ` AND sr.kind = 'document'
		GROUP BY sr.ref_text ORDER BY w DESC LIMIT ?`
	docArgs := append(append([]interface{}{}, args...), limit)
	drows, err := g.db.Query(docQuery, docArgs...)
	if err != nil {
		return nil, err
	}
	defer drows.Close()
	for drows.Next() {
		var id string
		var w float64
		if err := drows.Scan(&id, &w); err != nil {
			return nil, err
		}
		out.Documents = append(out.Documents, id)
	}
	return out, drows.Err()
}

// SceneStats 返回各场景的规模，按引用数降序。
func (g *GraphDB) SceneStats() ([]SceneStat, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	rows, err := g.db.Query(
		`SELECT s.key,
		        COUNT(sr.id),
		        -- ★ kind 口径已随 scene_refs 迁移改变（07b8c23 / 2026-10-04）
		        --   旧：'relation' / 'entity' —— 两者都已退场，归零
		        --   新：'edge' / 'block'
		        SUM(CASE WHEN sr.kind = 'edge' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN sr.kind = 'block' THEN 1 ELSE 0 END),
		        COALESCE(s.strength, 1),
		        (SELECT COUNT(*) FROM scene_features f WHERE f.scene_id = s.id),
		        COALESCE(s.origin, 'emergent'),
		        s.updated_at
		 FROM scenes s LEFT JOIN scene_refs sr ON sr.scene_id = s.id
		 GROUP BY s.id ORDER BY COUNT(sr.id) DESC, s.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SceneStat
	for rows.Next() {
		var st SceneStat
		var rels, ents sql.NullInt64
		if err := rows.Scan(&st.Key, &st.Refs, &rels, &ents, &st.Strength, &st.Features, &st.Origin, &st.UpdatedAt); err != nil {
			return nil, err
		}
		st.Relations = int(rels.Int64)
		st.Entities = int(ents.Int64)
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Refs > out[j].Refs })
	return out, nil
}

// PurgeStaleSceneRefs 清理指向已不存在节点的场景引用，返回删除数。
//
// 节点被清理（PurgeNoise / PurgeOrphans / memory_delete_entity）时不会级联
// 删 scene_refs（见建表注释），残留引用会让场景看起来很大却召回出空结果，
// 也会让 SceneStats 说谎。这个函数把它们对齐。
func (g *GraphDB) PurgeStaleSceneRefs() (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.purgeStaleSceneRefsLocked()
}

func (g *GraphDB) purgeStaleSceneRefsLocked() (int, error) {
	// ★★ kind='edge' 分支必须存在
	//
	// scene_refs 现在多了一种引用：指向 memory_block_edges.id（关系边）。
	// 旧实现只清 relation/entity/block/document —— 于是边被删后，
	// 那批 edge 引用**静默悬空**，而 purgeStaleSceneRefs 看着跑成功了。
	//
	// ★ relation/entity 两个分支保留但已无数据来源
	//   （07b8c23 把 718 条全迁到 block/edge），
	//   它们现在是空转；等旧表删除时这两个分支也要删。
	res, err := g.db.Exec(`DELETE FROM scene_refs WHERE
		(kind = 'edge' AND ref_id NOT IN (SELECT id FROM memory_block_edges))
		OR (kind = 'relation' AND ref_id NOT IN (SELECT id FROM relations))
		OR (kind = 'entity' AND ref_id NOT IN (SELECT id FROM entities))
		OR (kind = 'block' AND ref_text NOT IN (SELECT id FROM memory_blocks))
		OR (kind = 'document' AND ref_text NOT IN (SELECT id FROM documents))`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ScenesOfRelation 取回一条关系当前所属的全部场景键。
//
// 用途：memory_edit 是「删旧写新」——旧关系的 scene_refs 会随节点一起失效，
// 新关系若不重新挂上场景，这条记忆就**静默地脱离场景**，此后场面重现也召不回。
func (g *GraphDB) ScenesOfRelation(relationID int64) ([]string, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(
		// ★ 端点已是**关系边**，故 kind='edge'（不是 'relation'）。
		//   旧表退场后 kind='relation' 恒空 ⇒ 返回空列表 ⇒
		//   「这条记忆属于哪些场景」的信息彻底丢失，且无报错。
		`SELECT s.key FROM scene_refs sr JOIN scenes s ON sr.scene_id = s.id
		 WHERE sr.kind = 'edge' AND sr.ref_id = ?`, relationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// TagSceneDocument 把 L3 文档节点挂到场景上（document 层与场景模型兼容）。
//
// 文档的「场景」不由存储字段决定，而由**来源**派生（chan:<source>）——存一份
// 冗余的 Doc.Scene 会随来源改名而说谎，是同一事实的第二份真相。
// 这里只登记「这份文档属于哪些场面」，供 scene 侧枚举与统计。
func (g *GraphDB) TagSceneDocument(sceneKey, docID string) error {
	if docID == "" {
		return nil
	}
	key := NormalizeSceneKey(sceneKey)
	if key == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	tx, err := g.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tagSceneRefTx(tx, key, "document", 0, docID, 1.0); err != nil {
		return err
	}
	return tx.Commit()
}

// ──────────────────────────────────────────────
// 场景去重（整备）：把「同一个场面的两个键」合成一个
// ──────────────────────────────────────────────

// DedupeScenes 合并归一化后同名的场景，返回合并组数。
//
// 存在的原因：scenes.key 有 UNIQUE 约束，但**归一化口径曾经不统一**——
// 建键路径用 "auto:" + Label(2) 而 Label 拼的是 "+"，不过 NormalizeSceneKey；
// 而写侧（effectiveScenes）、读侧（RecallByScene）、声明建键（EnsureScene）
// 三处都过了归一化。于是 "+" 与 "_" 成为两个都合法的主键，UNIQUE 拦不住：
//
//	auto:chan:qq+part:morning   strength=270  6 features  0 refs
//	auto:chan:qq_part:morning   strength=1    0 features  201 refs
//
// 两个节点互不可见：聚类只读 scene_features，所以 0-features 的那个
// 永远不被看见；而 0-refs 的那个收不到任何写侧记忆。实测这对双胞胎
// 从建库起累积到 strength=270 都没人发现——因为图整理心跳（mergeLoop）
// 的遍历入口 Recall(nil,nil,1,"") 只查 entities 与 relations，scenes
// 不在其中。
//
// 合并口径：**只有归一化后完全同名才算重复**。相似但不同的场面
// （chan:qq 与 chan:webui）绝不合并——去重不是"把像的一律合并"。
// 场景之间的相似度判定是 EnterScene 的聚类职责，那是另一件事。
//
// 存活规则：保留 id 最小的那一行（先来者），其余并入它。
// 强度相加、特征取并集（权重取大）、引用全部重定向。
// 跨 origin 也合：现网存在 origin='emergent' 却长得像声明键的
// chan:context_archived。
func (g *GraphDB) DedupeScenes() (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	rows, err := g.db.Query(`SELECT id, key FROM scenes ORDER BY id`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id  int64
		key string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.key); err != nil {
			rows.Close()
			return 0, err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(all) < 2 {
		return 0, nil
	}

	// 按归一化后的键分组，组内 id 最小者为存活者。
	groups := make(map[string][]row)
	order := make([]string, 0, len(all))
	for _, r := range all {
		nk := NormalizeSceneKey(r.key)
		if nk == "" {
			// 键归一化后为空：无法判定它与谁重复，跳过（不擅自删数据）。
			continue
		}
		if _, seen := groups[nk]; !seen {
			order = append(order, nk)
		}
		groups[nk] = append(groups[nk], r)
	}

	merged := 0
	for _, nk := range order {
		gp := groups[nk]
		if len(gp) < 2 {
			continue
		}
		keep := gp[0] // ORDER BY id ⇒ 最早创建的那行

		tx, err := g.db.Begin()
		if err != nil {
			return merged, err
		}
		err = func() error {
			for _, dup := range gp[1:] {
				// 强度相加。
				if _, err := tx.Exec(
					`UPDATE scenes SET strength = COALESCE(strength,1) +
					   COALESCE((SELECT strength FROM scenes WHERE id = ?), 0),
					   updated_at = CURRENT_TIMESTAMP
					 WHERE id = ?`, dup.id, keep.id); err != nil {
					return err
				}
				// 特征取并集，权重取大。
				if _, err := tx.Exec(
					`INSERT INTO scene_features (scene_id, feature, weight)
					 SELECT ?, feature, weight FROM scene_features WHERE scene_id = ?
					 ON CONFLICT(scene_id, feature) DO UPDATE
					   SET weight = MAX(weight, excluded.weight)`,
					keep.id, dup.id); err != nil {
					return err
				}
				if _, err := tx.Exec(
					`DELETE FROM scene_features WHERE scene_id = ?`, dup.id); err != nil {
					return err
				}
				// 引用重定向。UNIQUE(scene_id,kind,ref_id,ref_text) 会与存活者
				// 上的同一条冲突——冲突即同一条记忆，取权重大的那条。
				if _, err := tx.Exec(
					`INSERT INTO scene_refs (scene_id, kind, ref_id, ref_text, weight)
					 SELECT ?, kind, ref_id, ref_text, weight FROM scene_refs WHERE scene_id = ?
					 ON CONFLICT(scene_id, kind, ref_id, ref_text) DO UPDATE
					   SET weight = MAX(weight, excluded.weight)`,
					keep.id, dup.id); err != nil {
					return err
				}
				if _, err := tx.Exec(
					`DELETE FROM scene_refs WHERE scene_id = ?`, dup.id); err != nil {
					return err
				}
				if _, err := tx.Exec(`DELETE FROM scenes WHERE id = ?`, dup.id); err != nil {
					return err
				}
				merged++
			}
			// 存活者的键也归一化，避免下次又认不出自己。
			if _, err := tx.Exec(`UPDATE scenes SET key = ? WHERE id = ?`, nk, keep.id); err != nil {
				return err
			}
			return nil
		}()
		if err != nil {
			tx.Rollback()
			return merged, err
		}
		if err := tx.Commit(); err != nil {
			return merged, err
		}
	}
	return merged, nil
}

// globToLike 把 GLOB 模式转成 LIKE（只换通配符：* → %、? → _）。
//
// ★ 代价：LIKE 不支持 `[...]` 字符类（GLOB 支持）。
//
//	若调用方需要字符类，应改用显式的 name LIKE 参数，而不是走 glob。
func globToLike(pattern string) string {
	return strings.NewReplacer("*", "%", "?", "_").Replace(pattern)
}

// tagSceneTripleTx 在事务内把「两端块 + 关系边」挂到场景上。
//
// ★ 这是 tagSceneTx 的块版（2026-10-04）
//
// 旧 tagSceneTx 收 relationID + []entityID，写出 kind='relation'
// 与 kind='entity' 的引用 —— 旧表退场后这两类引用全部悬空。
//
// 三条引用缺一不可：
//
//	block(两端)  场景召回时要能把关系还原成「谁 — 什么 — 谁」
//	edge(关系)  ★ 只有两端块拿不到关系语义 ——
//	                「值班室分机号 → 4324」和
//	                「billing服务 → 4324」只凭两端无法区分
//
// weight 用边的置信度：场景内的记忆也要能排序，这是目前唯一现成的质量信号。
func tagSceneTripleTx(tx *sql.Tx, sceneKey, srcBlockID, dstBlockID string,
	edgeID int64, weight float64) error {
	key := NormalizeSceneKey(sceneKey)
	if key == "" {
		return nil
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO scenes (key) VALUES (?)`, key); err != nil {
		return err
	}
	var sceneID int64
	if err := tx.QueryRow(`SELECT id FROM scenes WHERE key = ?`, key).Scan(&sceneID); err != nil {
		return err
	}
	for _, id := range []string{srcBlockID, dstBlockID} {
		if id == "" {
			continue
		}
		if err := tagSceneRefTx(tx, key, "block", 0, id, weight); err != nil {
			return err
		}
	}
	if edgeID != 0 {
		if err := tagSceneRefTx(tx, key, "edge", edgeID, "", weight); err != nil {
			return err
		}
	}
	return nil
}
