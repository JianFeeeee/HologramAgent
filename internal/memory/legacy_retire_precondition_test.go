package memory

import (
	"fmt"
	"os"
	"testing"
)

// ★★ 存量清理的前置判据：清理**之前**必须能证明块侧数据完整。
//
// 清理是不可逆动作（删表），所以判据必须回答：
//
//	① 每个 entity 都有对应的块吗？
//	② 每条 relation 都有对应的边吗？（去重口径）
//	③ 每条 sentence 都有对应的原句块吗？
//	④ 有没有边指向将被删除的节点？（悬空边）
func TestRetire_清理前置_块侧覆盖完整(t *testing.T) {
	path := os.Getenv("RETIRE_DB")
	if path == "" {
		t.Skip("需要 RETIRE_DB（生产库快照副本）")
	}
	g, err := NewGraphDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()

	// ① entities → legacy-entity 块
	var entCount, entBlocks int
	mustScalar(t, g, `SELECT COUNT(*) FROM entities`, &entCount)
	mustScalar(t, g, `SELECT COUNT(*) FROM memory_blocks WHERE source='legacy-entity'`, &entBlocks)
	fmt.Printf("  ① entities %d → legacy-entity 块 %d %s\n",
		entCount, entBlocks, markEq(entCount, entBlocks))

	// 每个 entity 的名字都能找到一个块（不只数数量）
	missing := missingBlocks(t, g)
	fmt.Printf("     缺块的 entity: %d 个%s\n", len(missing),
		excerpt(missing))
	if len(missing) > 0 {
		t.Errorf("★ 有 %d 个 entity 没有对应块 —— 清理会丢数据", len(missing))
	}

	// ② relations → 关系边
	var relCount, relDistinct int
	mustScalar(t, g, `SELECT COUNT(*) FROM relations`, &relCount)
	mustScalar(t, g, `SELECT COUNT(*) FROM (
		SELECT DISTINCT source_id, target_id, COALESCE(relation_type,'') FROM relations)`,
		&relDistinct)
	mustScalar(t, g, `SELECT COUNT(*) FROM memory_block_edges WHERE edge_type<>'contains'`, &relBlocks)
	fmt.Printf("  ② relations %d（去重后 %d）→ 关系边 %d %s\n",
		relCount, relDistinct, relBlocks, markEq(relDistinct, relBlocks))
	if relDistinct != relBlocks {
		t.Errorf("★ 关系边 %d ≠ 去重后关系数 %d —— 清理会丢边", relBlocks, relDistinct)
	}

	// ③ sentences → 原句块
	//
	// ★ 块 ID 是 sha256(Text[:12])，**SQL 里算不了** ⇒ 在 Go 侧比对。
	//   （SQLite 没有 sha256 函数，尝试 `SentenceBlockID(...)` 会报
	//   "no such function" —— 判据要自己算，别指望 SQL 能做哈希。）
	allBlocks := map[string]bool{}
	bs, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bs {
		allBlocks[b.ID] = true
	}
	sents := g.sentenceTexts(t)
	var sentWithBlock int
	var noBlock []string
	for _, txt := range sents {
		if allBlocks[SentenceBlockID(txt)] {
			sentWithBlock++
		} else {
			noBlock = append(noBlock, txt)
		}
	}
	fmt.Printf("  ③ sentences %d → 有原句块的 %d %s%s\n",
		len(sents), sentWithBlock, markEq(len(sents), sentWithBlock),
		excerpt(noBlock))
	if len(noBlock) > 0 {
		t.Errorf("★ %d 条 sentence 没有原句块 —— 清理会丢原句", len(noBlock))
	}

	// ④ 悬空边：指向 sentence 表的边
	var dangling int
	mustScalar(t, g, `SELECT COUNT(*) FROM memory_block_edges e
		WHERE e.source_kind='sentence'
		  AND NOT EXISTS (SELECT 1 FROM sentences s WHERE CAST(s.id AS TEXT)=e.source_id)`,
		&dangling)
	fmt.Printf("  ④ 悬空的 sentence→块 边: %d\n", dangling)
	if dangling > 0 {
		t.Errorf("★ %d 条边指向不存在的 sentence —— 清理后会成为悬空边", dangling)
	}

	// ⑤ 块边的两端都必须存在（通用悬空检查）
	var badEnds int
	mustScalar(t, g, `SELECT COUNT(*) FROM memory_block_edges e
		WHERE (e.source_kind='block' AND NOT EXISTS
			(SELECT 1 FROM memory_blocks b WHERE b.id=e.source_id))
		   OR (e.target_kind='block' AND NOT EXISTS
			(SELECT 1 FROM memory_blocks b WHERE b.id=e.target_id))`, &badEnds)
	fmt.Printf("  ⑤ 端点不存在的块边: %d\n", badEnds)
	if badEnds > 0 {
		t.Errorf("★ %d 条块边的一端不存在", badEnds)
	}
}

var relBlocks int

func mustScalar(t *testing.T, g *GraphDB, q string, dst *int) {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	if err := g.db.QueryRow(q).Scan(dst); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// sentenceTexts 读出 sentences 表的原文。
func (g *GraphDB) sentenceTexts(t *testing.T) []string {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT text FROM sentences`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// missingBlocks 找出没有对应块的 entity
func missingBlocks(t *testing.T, g *GraphDB) []string {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT e.name FROM entities e
		WHERE NOT EXISTS (SELECT 1 FROM memory_blocks b
			WHERE b.source='legacy-entity' AND b.text_content = e.name)`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

func markEq(a, b int) string {
	if a == b {
		return "✓"
	}
	return "✘"
}

func excerpt(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	s := " 例: " + ss[0]
	if len(ss) > 1 {
		s += fmt.Sprintf(" …(共 %d)", len(ss))
	}
	return s
}

// ★★ scene 读方适配：TagSceneByEntityGlob 改走块体系
//
// 现状（scene.go:260）：按实体名 GLOB 查 relations：
//
//	SELECT r.id, r.source_id, r.target_id, r.confidence
//	FROM relations r JOIN entities e1 ... JOIN entities e2 ...
//	WHERE r.status='active' AND (e1.name GLOB ? OR e2.name GLOB ?)
//
// 块体系里等价于：按**块文本** GLOB 找关系边的两端，
// 再把两端块都挂到场景上（边本身也挂，kind='edge'）。
//
// ★ 这条路径有真实调用方（cmd/memgc），所以必须有判据。
func TestSceneAdapt_ByEntityGlob走块体系(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	// 织一张网：值班室分机号 --是--> 4324
	for _, b := range []MemoryBlock{
		{ID: "blk_subject", Modality: BlockText, Text: "值班室分机号"},
		{ID: "blk_value", Modality: BlockText, Text: "4324"},
		{ID: "blk_other", Modality: BlockText, Text: "billing服务"},
	} {
		if err := g.PutMemoryBlocks([]MemoryBlock{b}); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.AddRelationBlockEdge("blk_subject", "blk_value", "是",
		RelationEdgeData{SessionID: "s1", Confidence: 0.9, Status: EdgeActive}); err != nil {
		t.Fatal(err)
	}

	// 按名字 GLOB 找关系（dryRun，不真写）
	n, err := g.TagSceneByEntityGlob("值班场景", "*分机号*", true)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  GLOB '*分机号*' 匹配 %d 条关系\n", n)
	if n != 1 {
		t.Errorf("★ 应匹配 1 条（值班室分机号 --是--> 4324），实际 %d", n)
	}

	// 不匹配的名字不该匹配
	n2, err := g.TagSceneByEntityGlob("值班场景", "*不存在的名字*", true)
	if err != nil {
		t.Fatal(err)
	}
	if n2 != 0 {
		t.Errorf("★ 不该匹配任何关系，实际 %d", n2)
	}

	// ★ 真写（dryRun=false）后，场景应挂到块上
	if _, err := g.TagSceneByEntityGlob("值班场景", "*分机号*", false); err != nil {
		t.Fatal(err)
	}
	counts := sceneRefCounts(t, g)
	fmt.Printf("  写入后 scene_refs: %v\n", counts)
	if counts["block"] == 0 {
		t.Error("★ 场景应挂到块上（kind='block'），实际无")
	}
	if counts["edge"] == 0 {
		t.Error("★ 场景应挂到边上（kind='edge'），实际无")
	}
	// ★ 旧表读方不应再被使用 —— 但引用不应退回 entity/relation
	if counts["entity"] != 0 || counts["relation"] != 0 {
		t.Errorf("★ 不该产生旧表引用，实际 %v", counts)
	}
}

// ★★ 悬空引用清理要认 kind='edge'
//
// 旧实现只清 kind='relation'/'entity'/'block'/'document'，
// 而 scene_refs 现在多了一种：**kind='edge'**（指向 memory_block_edges.id）。
//
// ★ 不加这个分支的后果：边被删后那批 edge 引用永远悬空，
//
//	而且是**静默**的 —— purgeStaleSceneRefs 看着跑成功了，
//	实际没清掉任何东西。
func TestSceneAdapt_悬空清理认edge(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	if err := g.PutMemoryBlocks([]MemoryBlock{
		{ID: "blk_a", Modality: BlockText, Text: "甲"},
		{ID: "blk_b", Modality: BlockText, Text: "乙"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.AddRelationBlockEdge("blk_a", "blk_b", "是",
		RelationEdgeData{SessionID: "s1", Confidence: 0.9}); err != nil {
		t.Fatal(err)
	}

	// 挂三条引用：两个有效、一个指向不存在的块
	g.mu.Lock()
	for _, ref := range []struct{ kind, text, id string }{
		{"block", "blk_a", "0"},
		{"edge", "0", "99999"}, // 指向不存在的边
		{"block", "blk_gone", "0"},
	} {
		if _, err := g.db.Exec(
			`INSERT INTO scene_refs (scene_id, kind, ref_id, ref_text, weight)
			 VALUES (1, ?, CAST(? AS INTEGER), ?, 1.0)`,
			ref.kind, ref.id, ref.text); err != nil {
			g.mu.Unlock()
			t.Fatal(err)
		}
	}
	g.mu.Unlock()

	n, err := g.PurgeStaleSceneRefs()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  清理 %d 条悬空引用\n", n)
	if n != 2 {
		t.Errorf("★ 应清理 2 条（悬空的 edge 与 block），实际 %d", n)
	}

	counts := sceneRefCounts(t, g)
	fmt.Printf("  清理后: %v\n", counts)
	if counts["edge"] != 0 {
		t.Errorf("★ 悬空的 kind='edge' 未被清理（缺该分支）")
	}
	if counts["block"] != 1 {
		t.Errorf("★ 有效引用应保留，实际 %v", counts)
	}
}

// ═══════════════════════════════════════════════════════════════
//  停旧表双写 —— 判据（2026-10-04）
//
//  ★ 目标不是「旧表被删」，而是「旧表停止增长、冻结为历史」。
//    删表是不可逆的，且读方虽已全切块，仍需要一段时间观察。
//
//  ★ 判据的核心是**增长量**，不是「有没有 INSERT 语句」——
//    后者是文本检查，前者才是行为检查。
// ═══════════════════════════════════════════════════════════════

// Test停双写_旧表不再增长
func Test停双写_旧表不再增长(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	// 先写一批，让旧表有内容
	if _, _, err := g.Commit([]Triple{
		{Subject: "初始甲", Relation: "属于", Object: "初始乙", Confidence: 1.0},
	}, "sess-0", 0); err != nil {
		t.Fatal(err)
	}
	snap := legacyCounts(t, g)
	fmt.Printf("  首批后（旧表应已冻结）: %v\n", snap)

	// ★ 首批之后旧表就应该是 0 —— 双写已停（2026-10-04）。
	//   原断言是「首批应写入旧表」，那是**停双写之前**的前提，
	//   改完之后它必然失败 ⇒ 判据自己抓出了自己过期。
	//
	//   ★ 这正是「行为判据优于文本判据」的又一例：
	//     我们不是在查「有没有 INSERT 语句」，
	//     而是在看 Commit 之后旧表**实际长没长**。
	for _, table := range []string{"entities", "relations", "sentences"} {
		if snap[table] != 0 {
			t.Fatalf("★ 首批 Commit 后旧表 %s 仍有 %d 行（双写未停）",
				table, snap[table])
		}
	}

	// 再写一批 —— 旧表**不应**再增长
	for i := 0; i < 5; i++ {
		if _, _, err := g.Commit([]Triple{
			{Subject: fmt.Sprintf("新主体%d", i), Relation: "属性",
				Object: fmt.Sprintf("值%d", i), Confidence: 1.0,
				SentenceText: fmt.Sprintf("第 %d 句测试", i)},
		}, fmt.Sprintf("sess-%d", i+1), i+1); err != nil {
			t.Fatal(err)
		}
	}

	after := legacyCounts(t, g)
	fmt.Printf("  五批后: %v\n", after)
	for _, table := range []string{"entities", "relations", "sentences"} {
		if after[table] != snap[table] {
			t.Errorf("★ 旧表 %s 仍在增长: %d → %d（应冻结）",
				table, snap[table], after[table])
		}
	}

	// ★ 但块侧**必须**照常增长 —— 双写停了，单写不能停
	if blk, err := g.MemoryBlocks(); err != nil {
		t.Fatal(err)
	} else {
		fmt.Printf("  块数 %d（应 ≥ 12：首批 2 + 新批 10）\n", len(blk))
		if len(blk) < 12 {
			t.Errorf("★ 块侧写入被误伤，只剩 %d 个块", len(blk))
		}
	}
}

// Test停双写_召回不受影响：旧表冻结后读方仍要工作
func Test停双写_召回不受影响(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	for i := 0; i < 3; i++ {
		if _, _, err := g.Commit([]Triple{
			{Subject: "召回主体", Relation: "属性", Object: fmt.Sprintf("召回值%d", i),
				Confidence: 1.0},
		}, "sess", i); err != nil {
			t.Fatal(err)
		}
	}
	res, err := g.Recall([]string{"召回主体"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  召回实体 %d，关系 %d\n", len(res.Entities), len(res.Relations))
	if len(res.Entities) == 0 {
		t.Error("★ 旧表冻结后召回不该失效（读方已全切块）")
	}
	if len(res.Relations) == 0 {
		t.Error("★ 旧表冻结后关系召回不该失效")
	}
}

func legacyCounts(t *testing.T, g *GraphDB) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tbl := range []string{"entities", "relations", "sentences"} {
		n, err := g.LegacyRowCount("SELECT COUNT(*) FROM " + tbl)
		if err != nil {
			t.Fatalf("count %s: %v", tbl, err)
		}
		out[tbl] = n
	}
	return out
}
