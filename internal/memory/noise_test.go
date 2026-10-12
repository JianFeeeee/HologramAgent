package memory

import (
	"os"
	"testing"
)

func TestIsNoiseEntity(t *testing.T) {
	cases := []struct {
		name string
		want bool
		why  string
	}{
		{"结果", true, "停用词表内的泛化名词"},
		{"什么", true, "疑问代词"},
		{"哪个", true, "疑问代词"},
		{"咱俩", true, "人称代词（2026-09 补全）"},
		{"任何", true, "限定词"},
		{"部分", true, "泛指名词"},
		{"context_archived", true, "归档上下文内部标记"},
		{"来自 1 个来源的 2 条对话 (agent) 涉及: qq, 通道", true, "模板摘要回声"},
		{"来自 2 个来源的 2 条对话 (cli, agent)", true, "模板摘要回声（无涉及段）"},
		{"", true, "空名"},
		{"   ", true, "纯空白"},
		{"文档", false, "doc→graph 的模板主语，保留"},
		{"小宅", false, "人名"},
		{"CodeGraph", false, "专名"},
		{"报告", false, "开放类词：可能是有意义的实体，不由本函数拦截"},
		{"对话", false, "开放类词"},
	}
	for _, c := range cases {
		if got := IsNoiseEntity(c.name); got != c.want {
			t.Errorf("IsNoiseEntity(%q) = %v, want %v (%s)", c.name, got, c.want, c.why)
		}
	}
}

func TestFilterNoiseTriples(t *testing.T) {
	in := []Triple{
		{Subject: "结果", Relation: "是", Object: "问题"},                       // 两端噪音
		{Subject: "小宅", Relation: "需要", Object: "什么"},                      // 一端噪音
		{Subject: "文档", Relation: "主题", Object: "来自 1 个来源的 2 条对话 (agent)"}, // 一端噪音（模板摘要）
		{Subject: "小宅", Relation: "使用", Object: "CodeGraph"},               // 干净
		{Subject: "小宅", Relation: "继续", Object: "待命"},                      // 开放类词：本层不拦
	}
	out := FilterNoiseTriples(in)
	if len(out) != 2 {
		t.Fatalf("FilterNoiseTriples 保留 %d 条，want 2: %+v", len(out), out)
	}
	if out[0].Object != "CodeGraph" || out[1].Object != "待命" {
		t.Errorf("留下的不是那两条干净三元组: %+v", out)
	}

	// 空输入不应被改写成非 nil（调用方按 len 判断，别制造意外）
	if FilterNoiseTriples(nil) != nil {
		t.Error("nil 输入应原样返回 nil")
	}
}

func TestPurgeNoise(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	triples := []Triple{
		{Subject: "结果", Relation: "是", Object: "问题", Confidence: 1.0},
		{Subject: "小宅", Relation: "继续", Object: "待命", Confidence: 1.0},
		{Subject: "文档", Relation: "来源", Object: "context_archived", Confidence: 1.0},
		{Subject: "文档", Relation: "主题", Object: "来自 1 个来源的 2 条对话 (agent)", Confidence: 1.0},
		{Subject: "小宅", Relation: "使用", Object: "CodeGraph", Confidence: 1.0},
	}
	if _, _, err := g.Commit(triples, "test", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}

	before, err := g.NoiseEntities()
	if err != nil {
		t.Fatalf("NoiseEntities: %v", err)
	}
	if len(before) != 4 {
		// 4 个噪音：结果 / 问题 / context_archived / 模板摘要串
		// （「待命」是开放类词，不在停用词表内，故意不算噪音）
		t.Fatalf("噪音实体 %d 个，want 4: %+v", len(before), before)
	}
	// 按 mention_count 降序；都为 1 时按名字升序，只验证顺序单调。
	for i := 1; i < len(before); i++ {
		if before[i-1].MentionCount < before[i].MentionCount {
			t.Errorf("NoiseEntities 未按 mention_count 降序: %+v", before)
			break
		}
	}

	// dry-run 不得写库
	de, dr, err := g.PurgeNoise(true)
	if err != nil {
		t.Fatalf("PurgeNoise(dryRun): %v", err)
	}
	// 3 条关系：结果→问题（两端噪音算一次）、文档→context_archived、文档→模板摘要
	if de != 4 || dr != 3 {
		t.Errorf("dry-run 统计 entities=%d relations=%d, want 4/3", de, dr)
	}
	if again, _ := g.NoiseEntities(); len(again) != 4 {
		t.Fatalf("dry-run 改了库：噪音实体剩 %d 个", len(again))
	}

	// 真清理
	de, dr, err = g.PurgeNoise(false)
	if err != nil {
		t.Fatalf("PurgeNoise: %v", err)
	}
	if de != 4 || dr != 3 {
		t.Errorf("清理统计 entities=%d relations=%d, want 4/3", de, dr)
	}

	left, err := g.NoiseEntities()
	if err != nil {
		t.Fatalf("NoiseEntities: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("清理后仍有噪音实体: %+v", left)
	}

	// 干净的那条必须活着
	res, err := g.Recall(nil, nil, 1, "")
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	names := make(map[string]bool)
	for _, e := range res.Entities {
		names[e.Name] = true
	}
	if !names["小宅"] || !names["CodeGraph"] {
		t.Errorf("清理误伤干净实体，现存: %v", names)
	}
	for _, n := range []string{"结果", "问题", "context_archived"} {
		if names[n] {
			t.Errorf("噪音实体 %q 仍在库中", n)
		}
	}
	// 「文档」是 doc→graph 的模板主语，有意保留（清理只摘它的噪音边）
	if !names["文档"] {
		t.Error("模板主语「文档」不应被清理")
	}
	if !names["待命"] {
		t.Error("开放类词「待命」不应被清理（它不在停用词表内）")
	}

	// 幂等：再清一次应为 0/0
	de, dr, err = g.PurgeNoise(false)
	if err != nil {
		t.Fatalf("PurgeNoise 二次: %v", err)
	}
	if de != 0 || dr != 0 {
		t.Errorf("二次清理 entities=%d relations=%d, want 0/0", de, dr)
	}
}

// TestPurgeNoiseKeepsBlockBackedSentences 钉住 PurgeNoise 不能误删仍被媒体块
// 引用的句子——它复用 CleanupOrphanedSentences，那个判定必须照旧生效。
func TestPurgeNoiseKeepsBlockBackedSentences(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	sentence := "小宅 说 结果 很好"
	if _, _, err := g.Commit([]Triple{
		{Subject: "结果", Relation: "是", Object: "问题", SentenceText: sentence, Confidence: 1.0},
	}, "test", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// ★★★ 全面改用块体系（2026-10-04）
	//
	// 原句由**原句块**承载（sentences 表停写），
	// 而媒体块通过「原句块 --contains--> 媒体块」挂在它上面
	// —— 端点 kind 也从 "sentence" 变成 "block"（13c3292）。
	//
	// 判据的**意图**不变：「清理噪音块时，不能连带删掉仍被引用的原句」。
	sid := SentenceBlockID(sentence)
	if blk, err := g.BlockByText(sentence); err != nil || blk == nil {
		t.Fatalf("原句块未写入: %v", err)
	}
	if _, err := g.db.Exec(
		`INSERT INTO memory_blocks (id, modality, data, mime)
		 VALUES ('blk1', 'image', X'64696765737431', 'image/png')`); err != nil {
		t.Fatalf("insert block: %v", err)
	}
	if _, err := g.db.Exec(
		`INSERT INTO memory_block_edges
		 (source_kind, source_id, target_kind, target_id, edge_type, session_id)
		 VALUES ('block', ?, 'block', 'blk1', 'contains', '')`,
		sid); err != nil {
		t.Fatalf("insert edge: %v", err)
	}

	if _, _, err := g.PurgeNoise(false); err != nil {
		t.Fatalf("PurgeNoise: %v", err)
	}

	// ★ 原句块必须还在（它被媒体块的 contains 边引用）
	var n int
	if err := g.db.QueryRow(
		`SELECT COUNT(*) FROM memory_blocks WHERE id = ?`, sid).Scan(&n); err != nil {
		t.Fatalf("count sentence block: %v", err)
	}
	if n != 1 {
		t.Error("PurgeNoise 删掉了仍被媒体块 contains 边引用的原句块")
	}

	// ★ 媒体块也必须还在
	var m int
	if err := g.db.QueryRow(
		`SELECT COUNT(*) FROM memory_blocks WHERE id = 'blk1'`).Scan(&m); err != nil {
		t.Fatalf("count media block: %v", err)
	}
	if m != 1 {
		t.Error("PurgeNoise 误删了媒体块")
	}

	// ★ 而噪音块（结果/问题）应已被清理 —— 这才是 PurgeNoise 的本职工作
	var noiseLeft int
	if err := g.db.QueryRow(
		`SELECT COUNT(*) FROM memory_blocks
		 WHERE text_content IN ('结果','问题')`).Scan(&noiseLeft); err != nil {
		t.Fatalf("count noise: %v", err)
	}
	if noiseLeft != 0 {
		t.Errorf("PurgeNoise 应清掉噪音块，实际剩 %d 个", noiseLeft)
	}
}

func TestPurgeOrphans(t *testing.T) {
	g := newTestGraph(t)
	defer os.Remove(g.dbPath)
	defer g.Close()

	if _, _, err := g.Commit([]Triple{
		{Subject: "小宅", Relation: "使用", Object: "CodeGraph", Confidence: 1.0},
		{Subject: "结果", Relation: "是", Object: "问题", Confidence: 1.0},
	}, "test", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// 清掉噪音后，「结果」「问题」两个节点被删、边也没了；
	// 但先看清理前的孤立集合：应为 0（都有边）。
	if list, err := g.OrphanEntities(); err != nil {
		t.Fatalf("OrphanEntities: %v", err)
	} else if len(list) != 0 {
		t.Fatalf("清理前不应有孤立实体: %+v", list)
	}

	// ★ 造「边被清掉、节点还在」的壳（2026-10-04）
	//
	// 原来 `DELETE FROM relations WHERE relation_type='是'` ——
	// 那是旧表，而旧表停双写后既不被写也不被读，**空操作**。
	// ⇒ 块侧的边一直在 ⇒ 「结果」「问题」永远不被判为孤立。
	//
	// ⇒ 必须删**块侧**的边。
	if _, err := g.db.Exec("DELETE FROM memory_block_edges WHERE edge_type = '是'"); err != nil {
		t.Fatalf("delete block edge: %v", err)
	}
	// 再补一个从未有过边的孤立实体
	if _, _, err := g.Commit([]Triple{{Subject: "孤零零", Relation: "涉及", Object: "小宅", Confidence: 1.0}}, "test", 0); err != nil {
		t.Fatalf("commit: %v", err)
	}
	// ★ 制造孤立：删**块侧**的关系边（2026-10-04）
	//
	// 原来删旧表 relations —— 而 orphanEntitiesLocked 已改扫块，
	// 块侧的边还在，于是没有块被判为孤立。
	//
	// ★ 这也说明「测试用旧表制造状态」这条路已经彻底断了：
	//   旧表不再增长、不再被读，任何依赖它的造数都是空操作。
	if _, err := g.db.Exec("DELETE FROM memory_block_edges " +
		"WHERE edge_type != 'contains' " +
		"AND source_id IN (SELECT id FROM memory_blocks WHERE text_content = '孤零零')",
	); err != nil {
		t.Fatalf("delete block edge: %v", err)
	}

	list, err := g.OrphanEntities()
	if err != nil {
		t.Fatalf("OrphanEntities: %v", err)
	}
	names := make(map[string]bool)
	for _, e := range list {
		names[e.Name] = true
	}
	for _, want := range []string{"结果", "问题", "孤零零"} {
		if !names[want] {
			t.Errorf("%q 应被识别为孤立实体，实际: %+v", want, list)
		}
	}
	if names["小宅"] || names["CodeGraph"] {
		t.Errorf("有边的实体被误判为孤立: %+v", list)
	}

	// dry-run 不写库
	if n, err := g.PurgeOrphans(true); err != nil || n != len(list) {
		t.Fatalf("PurgeOrphans(dryRun) = %d, %v; want %d", n, err, len(list))
	}
	if again, _ := g.OrphanEntities(); len(again) != len(list) {
		t.Fatal("dry-run 改了库")
	}

	n, err := g.PurgeOrphans(false)
	if err != nil {
		t.Fatalf("PurgeOrphans: %v", err)
	}
	if n != len(list) {
		t.Errorf("删除 %d 个，want %d", n, len(list))
	}
	if left, _ := g.OrphanEntities(); len(left) != 0 {
		t.Errorf("清理后仍有孤立实体: %+v", left)
	}
	// 幂等
	if n, err := g.PurgeOrphans(false); err != nil || n != 0 {
		t.Errorf("二次清理 = %d, %v; want 0", n, err)
	}
}
