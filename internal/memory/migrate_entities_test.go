package memory

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func newMigrateGraph(t *testing.T) *GraphDB {
	t.Helper()
	g := newTestGraph(t)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// seedLegacy 为迁移测试造**旧表数据**。
//
// ★★ 2026-10-04：不再用 Commit（2026-10-04）
//
// 旧表双写已停 ⇒ Commit 不再写 entities/relations/sentences
// ⇒ 迁移测试的**输入天然为空**
// ⇒ 「迁出 0 块 0 边」竟然通过。
//
// ★★ 那不是「迁移正确」，是「测不到」——
//
//	它让「迁移还对不对」这个问题无法回答，
//	而这正是判据最不该失效的地方。
//
// ⇒ 直接写旧表：迁移的输入是旧表，测试也该直接构造旧表。
//
// ★ 同时写一份块侧数据（用 Commit），因为迁移后的断言查的是块。
func seedLegacy(t *testing.T, g *GraphDB, triples []Triple) {
	t.Helper()

	// 块侧：Commit 现在只写块与边
	if _, _, err := g.Commit(triples, "s", 1); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// 旧表：迁移的输入，逐条写入
	for _, tr := range triples {
		for _, name := range []string{tr.Subject, tr.Object} {
			if name == "" {
				continue
			}
			typ := tr.SubjectType
			if name == tr.Object {
				typ = tr.ObjectType
			}
			if typ == "" {
				typ = "Concept"
			}
			if _, err := g.SeedLegacyEntity(name, typ); err != nil {
				t.Fatalf("seed entity %q: %v", name, err)
			}
		}
		// 关系（source=主语, target=宾语）
		if _, _, err := g.SeedLegacyRelation(tr.Subject, tr.Object,
			tr.Relation, tr.Confidence, "s", 1); err != nil {
			t.Fatalf("seed relation %s/%s: %v", tr.Subject, tr.Relation, err)
		}
		// 原句
		if tr.SentenceText != "" {
			sid, err := g.SeedLegacySentenceRow(tr.SentenceText)
			if err != nil {
				t.Fatalf("seed sentence: %v", err)
			}
			if err := g.LinkLegacyTripleToSentence(
				tr.Subject, tr.Object, tr.Relation, sid); err != nil {
				t.Fatalf("link sentence: %v", err)
			}
		}
	}
}

const fixedEmbed = "fp-migrate"

// ★ 迁移形态：实体 → 句子+块，关系 → 块边。旧表**不删**（由调用方决定）。
func TestMigrateLegacyTextEntities_形态(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{
		{Subject: "值班室分机号", Relation: "是", Object: "4324"},
	})

	res, err := g.MigrateLegacyTextEntities(func(string) ([]float64, string) {
		return []float64{1, 0, 0}, fixedEmbed
	})
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// ★ 2 个实体 → 2 个迁移块 + 2 个原句块（方案 A 后原句也是块）
	//
	// res.Sentences 不再递增：统计口径改为「迁移块数」，
	// 因为 sentences 表已不再被写入。
	if res.Blocks != 2 {
		t.Errorf("2 个实体应得 2 个迁移块，实际 %+v", res)
	}
	if res.Sentences != 0 {
		t.Errorf("迁移不应再创建 sentence 记录（方案 A 退场），实际 %d", res.Sentences)
	}
	// 1 条关系 → 1 条块边
	if res.Edges != 1 {
		t.Errorf("1 条关系应得 1 条边，实际 %d", res.Edges)
	}

	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	// ★ 只统计**迁移自己写的块**：legacy-entity（迁移块）与 sentence（原句块）。
	//
	// 不能用 len(blocks) —— 因为 seedLegacy 走 Commit，而 Commit 在
	// 块化之后**也会写块**（source="triple"），那不是迁移的产物。
	// 实测：未过滤时是 6 = 4 迁移 + 2 Commit 写的值块。
	if n := countBySource(t, blocks, "legacy-entity"); n != 2 {
		t.Errorf("应有 2 个迁移块，实际 %d", n)
	}
	if n := countBySource(t, blocks, SentenceBlockSource); n != 2 {
		t.Errorf("应有 2 个原句块，实际 %d", n)
	}
	for _, b := range blocks {
		// ★ 原句块刻意不带向量（溯源锚点，长整句会稀释召回）
		// ★ 只检查迁移自己写的两种块：原句块（sentence）与迁移块（legacy-entity）。
		//
		// 库里有第三种来源 triple —— 那是 seedLegacy 走 Commit 时写的
		// （Commit 块化之后），**不是迁移的产物**，不该被本测试断言。
		//
		//   原句块      blk_src_<hash>      无向量（溯源锚点）
		//   迁移块      blk_ent_<id>_<hash> 有向量
		//   Commit 块   blk_ent_<hash24>   无向量、无时序
		//
		// 三者是**不同语义的东西**，不是同一个东西的重复。
		switch b.Source {
		case SentenceBlockSource:
			if len(b.Vector) != 0 {
				t.Errorf("原句块 %s 不该带向量，实际 %d 维", b.ID, len(b.Vector))
			}
		case "legacy-entity":
			if len(b.Vector) != 3 || b.Fingerprint != fixedEmbed {
				t.Errorf("迁移块 %s 应带 3 维向量与 %s，实际 %d 维 %q",
					b.ID, fixedEmbed, len(b.Vector), b.Fingerprint)
			}
		case "triple":
			// Commit 块化的产物，本测试不关心（见 TestBlockCommit_*）
		default:
			t.Errorf("块 %s 来源 %q 既不是迁移产物也不是 Commit 产物", b.ID, b.Source)
		}
	}

	// 每块都应有一条 sentence--contains--> 边
	edges, err := g.MemoryBlockEdges()
	if err != nil {
		t.Fatal(err)
	}
	containsCount := 0
	for _, e := range edges {
		// ★ 起点是**原句块**（block），不再是 sentence 表行 —— 方案 A
		if e.Type == "contains" && e.SourceKind == "block" && e.TargetKind == "block" {
			containsCount++
		}
	}
	if containsCount != 2 {
		t.Errorf("应有 2 条 contains 边，实际 %d（总边数 %d）", containsCount, len(edges))
	}

	// ★ 旧表还在（迁移不删数据，可回滚）
	r, err := g.Recall([]string{"值班室分机号"}, nil, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Entities) == 0 {
		t.Error("迁移不应删除 entities（由调用方在验证通过后决定清理）")
	}
}

// ★ 幂等：重复迁移不产生重复块。
func TestMigrateLegacyTextEntities_幂等(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{
		{Subject: "主体甲", Relation: "是", Object: "客体乙"},
	})
	embed := func(string) ([]float64, string) { return []float64{1, 0}, fixedEmbed }

	for i := 0; i < 3; i++ {
		if _, err := g.MigrateLegacyTextEntities(embed); err != nil {
			t.Fatalf("第 %d 次迁移: %v", i, err)
		}
	}
	blocks, _ := g.MemoryBlocks()
	// ★ 重复跑不增：迁移块与原句块各 2 个
	//   （不过滤 source='triple' —— 那是 seedLegacy 时 Commit 写的，
	//     重复跑迁移不会再增加它们，但会出现在 blocks 里）
	if n := countBySource(t, blocks, "legacy-entity"); n != 2 {
		t.Fatalf("迁移 3 次后仍应只有 2 个迁移块，实际 %d", n)
	}
	edges, _ := g.MemoryBlockEdges()
	contains := 0
	for _, e := range edges {
		if e.Type == "contains" {
			contains++
		}
	}
	if contains != 2 {
		t.Fatalf("contains 边应仍是 2 条，实际 %d", contains)
	}
}

// ★ 无 embed 时仍迁移，但块不带向量——不编造零向量。
func TestMigrateLegacyTextEntities_无embed不编造(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{{Subject: "主体甲", Relation: "是", Object: "客体乙"}})

	res, err := g.MigrateLegacyTextEntities(nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Blocks != 2 {
		t.Fatalf("无 embed 也应迁移，实际 %d 块", res.Blocks)
	}
	if res.SkippedNoVec != 2 {
		t.Errorf("应统计 2 个无向量块，实际 %d", res.SkippedNoVec)
	}
	blocks, _ := g.MemoryBlocks()
	for _, b := range blocks {
		if len(b.Vector) != 0 {
			t.Errorf("%s 不该有向量，实际 %d 维", b.ID, len(b.Vector))
		}
	}
}

// embed 返回 nil 向量时算「算不出」，迁移继续但不计入成功。
func TestMigrateLegacyTextEntities_embed失败仍迁移(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{{Subject: "主体甲", Relation: "是", Object: "客体乙"}})
	res, err := g.MigrateLegacyTextEntities(func(string) ([]float64, string) {
		return nil, "" // 模型加载失败
	})
	if err != nil {
		t.Fatalf("embed 失败不该让迁移失败: %v", err)
	}
	if res.Blocks != 2 {
		t.Fatalf("应仍迁移出 2 块，实际 %d", res.Blocks)
	}
	if res.SkippedNoVec != 2 {
		t.Errorf("应统计 2 个无向量，实际 %d", res.SkippedNoVec)
	}
}

// ★ 孤儿关系（端点实体为空名）被跳过并计数，不编造边。
func TestMigrateLegacyTextEntities_孤儿关系跳过(t *testing.T) {
	g := newMigrateGraph(t)
	// 直接构造一个指向空名实体的关系是不可能的（upsert 拒绝空名），
	// 所以这里测的是「关系两端都存在」时边数正确，作为对照基线。
	seedLegacy(t, g, []Triple{
		{Subject: "主体甲", Relation: "是", Object: "客体乙"},
		{Subject: "主体甲", Relation: "属于", Object: "客体丙"},
	})
	res, err := g.MigrateLegacyTextEntities(nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Edges != 2 {
		t.Errorf("2 条关系应得 2 条边，实际 %d", res.Edges)
	}
	if res.SkippedOrphan != 0 {
		t.Errorf("端点齐全时不该有跳过，实际 %d", res.SkippedOrphan)
	}
}

// 空关系类型用占位名而不是被拒绝（关系的**存在**本身是信息）。
func TestMigrateLegacyTextEntities_空关系类型用占位(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{{Subject: "主体甲", Relation: "   ", Object: "客体乙"}})
	res, err := g.MigrateLegacyTextEntities(nil)
	if err != nil {
		t.Fatalf("空关系类型不该让迁移失败: %v", err)
	}
	if res.Edges != 1 {
		t.Fatalf("空关系类型也应建成边，实际 %d", res.Edges)
	}
	edges, _ := g.MemoryBlockEdges()
	found := false
	for _, e := range edges {
		if e.Type == "related_to" {
			found = true
		}
	}
	if !found {
		t.Errorf("应有占位关系类型的边，实际 %+v", edges)
	}
}

// ★ 事务回滚：迁移中途失败时，**库内容保持迁移前状态**。
//
// 失败注入方式：给两个实体起相同的块 ID 是不行的（ID 由 entity id 派生，
// 天然唯一）。改用「让 putBlockTx 失败」——预先占位一个**非块节点类型**
// 的行占住表结构，或直接删掉 memory_blocks 表。
//
// 删表是最直接的：阶段三第一次 putBlockTx 就会报 no such table，
// 此时前面的 ensureSentenceTx 已经在事务里写过句子 —— 若无回滚，
// 那些句子会留在库里。
func TestMigrateLegacyTextEntities_中途失败整体回滚(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{
		{Subject: "主体甲", Relation: "是", Object: "客体乙"},
	})
	beforeBlocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	beforeSentences := countSentences(t, g)

	// 在阶段三开始前破坏块表：此时阶段一（读）已完成、阶段二（embed）正在跑
	calls := 0
	_, err = g.MigrateLegacyTextEntities(func(string) ([]float64, string) {
		calls++
		if calls == 1 {
			g.mu.Lock()
			_, dbErr := g.db.Exec(`DROP TABLE memory_blocks`)
			g.mu.Unlock()
			if dbErr != nil {
				t.Fatalf("构造失败: %v", dbErr)
			}
			t.Cleanup(func() {
				g.mu.Lock()
				_, _ = g.db.Exec(ddlMemoryBlocks)
				g.mu.Unlock()
			})
		}
		return []float64{1, 0}, "fp-x"
	})
	if err == nil {
		t.Fatal("块表被删后迁移应失败")
	}
	t.Logf("迁移如预期失败: %v", err)

	// 关键断言：块与句子都没留下（回滚生效）
	afterSentences := countSentences(t, g)
	if afterSentences != beforeSentences {
		t.Errorf("失败后句子数应不变（回滚），实际 %d → %d",
			beforeSentences, afterSentences)
	}
	_ = beforeBlocks

	// 恢复表后确认库里确实没有残留的块
	g.mu.Lock()
	_, dbErr := g.db.Exec(ddlMemoryBlocks)
	g.mu.Unlock()
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	afterBlocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(afterBlocks) != 0 {
		t.Errorf("失败后不应留下块（回滚），实际 %d 块: %+v", len(afterBlocks), afterBlocks)
	}
}

func countSentences(t *testing.T, g *GraphDB) int {
	t.Helper()
	g.mu.RLock()
	defer g.mu.RUnlock()
	var n int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM sentences`).Scan(&n); err != nil {
		t.Fatalf("count sentences: %v", err)
	}
	return n
}

// 块 id 含实体 id 前缀（可读 + 稳定）。
func TestLegacyEntityBlockID_稳定可读(t *testing.T) {
	a := legacyEntityBlockID(42, "值班室分机号")
	b := legacyEntityBlockID(42, "值班室分机号")
	c := legacyEntityBlockID(43, "值班室分机号")
	if a != b {
		t.Error("同实体应得同块 ID")
	}
	if a == c {
		t.Error("不同实体应得不同块 ID")
	}
	if !strings.Contains(a, "blk_ent_42") {
		t.Errorf("块 ID 应含实体 id 便于排查，实际 %q", a)
	}
}

// ★ 值覆盖维度依赖时序：迁移必须保住实体的先后关系。
//
// 背景：实测（真实 chineseclip + 真库）同一属性先后给两个值时，
// 「值班室分机号 4324」与「值班室分机号 4379」短句向量相似度是
// 0.9284 vs 0.9298 —— 数值上区分不了。若迁移把块的时间全写成 NOW()，
// overwrite 检索就彻底没救了。
//
// ★ 断言覆盖**全部**块：初版只查了含关键字的那一个，结果把
// 「CreatedAt 不传」的变异判成通过 —— 假绿。实际漏掉的是没被 UPDATE
// 到的那些实体（它们本就该落 NOW()，但保护要能区分两种情况）。
func TestMigrateLegacyTextEntities_保留时序(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{
		{Subject: "值班室分机号", Relation: "是", Object: "四三七九"},
	})
	// 给**两个**实体都写同一个历史时刻
	g.mu.Lock()
	_, err := g.db.Exec(`UPDATE entities SET created_at = '2026-01-01 10:00:00',
		updated_at = '2026-01-01 10:00:00'`)
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := g.MigrateLegacyTextEntities(nil); err != nil {
		t.Fatal(err)
	}

	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) == 0 {
		t.Fatal("无块")
	}
	want := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	for _, b := range blocks {
		// ★ 只检查**迁移块**（source=legacy-entity）。
		//
		// 库里有第三种来源 triple —— seedLegacy 走 Commit 时写的
		// （Commit 块化之后）。那些块的 created_at 由 PutMemoryBlocks
		// 填成 now，**本就不该继承实体时间**。
		//
		// 第一版没过滤 source，于是断言去检查 Commit 块，
		// 报「时序被抹平」—— 而被抹平的是 Commit 块，不是迁移块。
		// ★ 差点误判成「块化破坏了迁移时序」，那会推翻正确的实现。
		if b.Source != "legacy-entity" {
			continue
		}
		if b.CreatedAt.IsZero() {
			t.Errorf("迁移块 %s created_at 为零值", b.ID)
			continue
		}
		if !b.CreatedAt.Equal(want) {
			t.Errorf("迁移块 %s 应继承实体时间 %v，实际 %v（时序被抹平/未传递）",
				b.ID, want, b.CreatedAt)
		}
	}
}

// 多个实体的时间戳必须**不同**（全都落成同一时刻等于抹平时序）。
func TestMigrateLegacyTextEntities_时序不被抹平(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{
		{Subject: "值班室分机号", Relation: "是", Object: "四三七九"},
		{Subject: "新分机号码", Relation: "是", Object: "四三二四"},
	})
	// 主语（较早）与后写入的实体（较晚）
	g.mu.Lock()
	_, err := g.db.Exec(`UPDATE entities SET
		created_at = CASE WHEN name IN ('值班室分机号', '四三七九')
		                  THEN '2026-01-01 10:00:00' ELSE '2026-01-01 11:00:00' END,
		updated_at = CASE WHEN name IN ('值班室分机号', '四三七九')
		                  THEN '2026-01-01 10:00:00' ELSE '2026-01-01 11:00:00' END`)
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := g.MigrateLegacyTextEntities(nil); err != nil {
		t.Fatal(err)
	}
	blocks, _ := g.MemoryBlocks()
	// 主语+宾语各成一块：「值班室分机号」「新分机号码」「四三七九」「四三二四」
	// ★ 4 个实体 → 4 迁移块（+ 4 原句块）
	//
	// 不能用 len(blocks)：seedLegacy 走 Commit，块化之后它也写块
	// （source="triple"，无向量、无实体时序）。实测混进来是 12。
	// ⇒ 只数迁移自己的两种块。
	if n := countBySource(t, blocks, "legacy-entity"); n != 4 {
		t.Fatalf("应得 4 个迁移块，实际 %d（另含 triple 块 %d 个）", n,
			countBySource(t, blocks, "triple"))
	}
	// 判据：4 个块必须分属两个不同时刻（主语+首宾语 10:00，
	// 次宾语对 11:00）。若全落同一时刻，时序就被抹平了。
	// ★ 注意：不能断言「排序后首两块不等」—— 10:00 那组本来就有 2 块，
	// 同刻是正常的。判据是「时刻的分布」而不是「相邻两块是否相等」。
	// ★ 只统计**迁移块**（source=legacy-entity）。
	//   Commit 块（source=triple）的 created_at 是 PutMemoryBlocks 填的 now
	//   —— 它会把 seen 变成 3 个时刻（多了 2026-10-04）而误判「时序被抹平」。
	seen := map[string]int{}
	for _, b := range blocks {
		if b.Source != "legacy-entity" {
			continue
		}
		seen[b.CreatedAt.UTC().Format("2006-01-02 15:04")]++
	}
	if len(seen) != 2 {
		t.Errorf("块应分属 2 个不同时刻，实际 %d 个：%v", len(seen), seen)
	}
	// ★ 现在只数迁移块 ⇒ 每组 2 个实体 = 2 个迁移块
	//   （原句块不参与时序判据：它们的时间是 Commit 填的 now）
	if seen["2026-01-01 10:00"] != 2 {
		t.Errorf("10:00 组应有 2 个迁移块，实际 %d", seen["2026-01-01 10:00"])
	}
	if seen["2026-01-01 11:00"] != 2 {
		t.Errorf("11:00 组应有 2 个迁移块，实际 %d", seen["2026-01-01 11:00"])
	}
}

// 旧库时间格式不认得时：宁可零值兜底，也不编造错的时刻。
func TestParseLegacyTime(t *testing.T) {
	cases := []struct {
		in   string
		want string // 空 = 期望零值
	}{
		{"2026-01-01 10:00:00", "2026-01-01 10:00:00"},
		{"2026-01-01T10:00:00Z", "2026-01-01 10:00:00"},
		{"2026-01-01 10:00:00.123456", "2026-01-01 10:00:00.123456"},
		{"2026-01-01", "2026-01-01 00:00:00"},
		{"", ""},
		{"   ", ""},
		{"去年某天", ""},
	}
	for _, c := range cases {
		got := parseLegacyTime(c.in)
		if c.want == "" {
			if !got.IsZero() {
				t.Errorf("parseLegacyTime(%q) 应为零值，实际 %v", c.in, got)
			}
			continue
		}
		want, err := time.Parse("2006-01-02 15:04:05", c.want)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(want) {
			t.Errorf("parseLegacyTime(%q) = %v，期望 %v", c.in, got, want)
		}
	}
}

// ★★ 报告数必须等于实际写入数。
//
// 生产快照实测（1294 实体 / 980 relations）：
//
//	迁移报告   边 980
//	边表实际   959        ← 差 21
//
// 根因：addBlockEdgeTx 用 `INSERT OR IGNORE` 且**不检查 RowsAffected**，
// 而 `res.Edges++` 照加。relations 表里有 20 组 (src,tgt,type) 完全重复
// （各 2 次），980 → 去重 959。
//
// ★ 与 941e6b9 同类：那是 relations 口径（active vs 全表），
//
//	这是边去重口径。两次都是「报告数 ≠ 实际写入数」，
//	而用户会把报告数当承诺。
func TestMigrate_报告数等于实际写入数(t *testing.T) {
	g := newMigrateGraph(t)
	seedLegacy(t, g, []Triple{
		{Subject: "值班室分机号", Relation: "是", Object: "4324"},
	})
	// ★ 生产库里有 20 组 (src,tgt,type) 完全重复的关系行。
	//   seedLegacy 用 Upsert（第二次覆盖第一次）⇒ 造不出重复，
	//   所以直接插库 —— 这也是之前判据「通过」却没测到问题的原因。
	if _, err := g.db.Exec(`INSERT INTO relations
		(source_id, target_id, relation_type, confidence, status)
		SELECT source_id, target_id, relation_type, confidence, status
		FROM relations LIMIT 1`); err != nil {
		t.Fatalf("插入重复关系: %v", err)
	}

	embed := func(string) ([]float64, string) { return []float64{1, 0}, fixedEmbed }
	res, err := g.MigrateLegacyTextEntities(embed)
	if err != nil {
		t.Fatal(err)
	}

	edges, err := g.MemoryBlockEdges()
	if err != nil {
		t.Fatal(err)
	}
	// ★ 只数**迁移块之间**的边（两端都是 blk_ent_<entityID>_<hash> 形态）。
	//
	// 边表里还有 Commit 块化写的边（source=triple，两端是 blk_ent_<hash24>）
	// —— 那不是迁移的产物，而 res.Edges 只数迁移自己写的。
	// 第一版按「非 contains」统计，把 Commit 的边也算进去了 ⇒ 误报。
	relEdges := 0
	for _, e := range edges {
		if e.Type == "contains" {
			continue
		}
		if isLegacyEntityBlockID(e.SourceID) && isLegacyEntityBlockID(e.TargetID) {
			relEdges++
		}
	}

	if res.Edges != relEdges {
		t.Errorf("★ 报告边数 %d ≠ 实际写入 %d —— 报告会骗人", res.Edges, relEdges)
	}
}

// ★★ 迁移必须为 sentences 表的原文补建原句块。
//
// 实测缺口（清理前置判据，生产快照）：
//
//	sentences 66 条  →  原句块 0 个
//
// 而迁移只从 `entities` 读（readLegacySnapshot 里只有 entities 查询），
// 从没为 sentences 建过载体。sentences 表的价值就在**原文本身**
// （entities 是提炼后的名字）⇒ 直接清理该表会丢 66 条原句。
func TestMigrate_sentences补建原句块(t *testing.T) {
	g := newMigrateGraph(t)
	// 直接插 sentences（原句），不经过 Commit（Commit 只造 entity）
	// ★ sentences.text 有 UNIQUE 约束 ⇒ 同一句只能插一次。
	//   所以幂等性不能靠「重复插入」来测（那是数据库层的约束，
	//   根本到不了迁移代码）—— 幂等要靠**迁移跑两遍**来测。
	for _, txt := range []string{
		"值班室分机号改为 4324，旧号 4379 停用",
		"第 114 批周日凌晨停机 4 分，回滚 v2.28.4",
	} {
		if _, err := g.db.Exec(
			`INSERT INTO sentences (text) VALUES (?)`, txt); err != nil {
			t.Fatal(err)
		}
	}

	embed := func(string) ([]float64, string) { return []float64{1, 0}, fixedEmbed }
	res, err := g.MigrateLegacyTextEntities(embed)
	if err != nil {
		t.Fatal(err)
	}
	if res.Sentences == 0 {
		t.Error("迁移应统计 sentences 的原句块数（res.Sentences）")
	}

	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	// 幂等：迁移跑第二遍不应新增原句块
	if _, err := g.MigrateLegacyTextEntities(embed); err != nil {
		t.Fatal(err)
	}

	var srcBlocks int
	seen := map[string]bool{}
	for _, b := range blocks {
		if b.Source == SentenceBlockSource {
			if seen[b.ID] {
				t.Errorf("原句块 ID 重复：%s", b.ID)
			}
			seen[b.ID] = true
			srcBlocks++
		}
	}
	if srcBlocks != 2 {
		t.Errorf("应有 2 个原句块（迁移两遍仍不增），实际 %d", srcBlocks)
	}

	// ★ 每条 sentence 的原文都能由内容派生出对应的块
	for _, txt := range []string{
		"值班室分机号改为 4324，旧号 4379 停用",
		"第 114 批周日凌晨停机 4 分，回滚 v2.28.4",
	} {
		id := SentenceBlockID(txt)
		if !seen[id] {
			t.Errorf("sentence %q 缺原句块 %s", truncT(txt, 24), id)
		}
	}
}

func truncT(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// countBySource 按 source 统计块数。
//
// ★ 迁移测试必须用它而不是 len(blocks)：块化之后 Commit 也写块
//
//	（source="triple"），那些不是迁移的产物。
func countBySource(t *testing.T, blocks []MemoryBlock, src string) int {
	t.Helper()
	n := 0
	for _, b := range blocks {
		if b.Source == src {
			n++
		}
	}
	return n
}

// isLegacyEntityBlockID 判断块 ID 是否是迁移块形态（blk_ent_<entityID>_<hash8>）。
//
// ★ 与 Commit 块化的 blk_ent_<hash24> 区分：
//
//	迁移块  blk_ent_1234_abcd1234   （中间有下划线 + 数字行号）
//	Commit  blk_ent_abcdef1234…      （纯 hash，无下划线）
func isLegacyEntityBlockID(id string) bool {
	if !strings.HasPrefix(id, "blk_ent_") {
		return false
	}
	rest := strings.TrimPrefix(id, "blk_ent_")
	return strings.Contains(rest, "_")
}

// ★ LegacyRowCount 的三条约束（2026-10-04）
//
// 它接收完整 SQL 串，所以必须证明「不该接受的被拒绝」。
// 判据直接来自它的三条 guard：前缀 / JOIN+GROUP BY / 分号。
func TestLegacyRowCount_只允许单表COUNT(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	if _, _, err := g.Commit([]Triple{
		{Subject: "甲一", Relation: "是", Object: "乙一", Confidence: 1.0},
	}, "s", 0); err != nil {
		t.Fatal(err)
	}

	// ★ 用 SeedLegacyEntity 造旧表数据（2026-10-04）
	//   Commit 已不写旧表（原判据用 Commit，断言恒失败）。
	if _, err := g.SeedLegacyEntity("旧表实体", "Concept"); err != nil {
		t.Fatal(err)
	}
	n, err := g.LegacyRowCount("SELECT COUNT(*) FROM entities")
	if err != nil {
		t.Fatalf("合法查询应通过: %v", err)
	}
	fmt.Printf("  entities 计数 %d\n", n)
	if n != 1 {
		t.Errorf("★ 刚种下一个旧表实体，计数应为 1，实际 %d", n)
	}

	// ★ 非 COUNT 前缀必须拒绝
	for _, bad := range []string{
		"DELETE FROM entities",
		"DROP TABLE entities",
		"SELECT * FROM entities",
		"UPDATE entities SET name = 'x'",
		"INSERT INTO entities (name) VALUES ('x')",
	} {
		if _, err := g.LegacyRowCount(bad); err == nil {
			t.Errorf("★ 危险查询 %q 被接受了", bad)
		}
	}
	// ★ JOIN / GROUP BY / 分号必须拒绝
	for _, bad := range []string{
		"SELECT COUNT(*) FROM entities JOIN relations ON 1=1",
		"SELECT COUNT(*) FROM entities GROUP BY type",
		"SELECT COUNT(*) FROM entities; DROP TABLE entities",
	} {
		if _, err := g.LegacyRowCount(bad); err == nil {
			t.Errorf("★ 越界查询 %q 被接受了", bad)
		}
	}
}

// ★★★ 迁移必须带上旧关系的属性（2026-10-04）
//
// 缺陷：legacyRel 此前只读 id/src/tgt/type，confidence / session_id /
// turn_id / status **根本没被读取**，于是迁出的边全是空属性。
//
// ★ 生产快照实测后果：959 条边 confidence 全为 0 —— 而边表把
// confidence 当唯一的质量信号（RecallSorted 的相关性排序、
// 场景权重、蒸馏置信度传播都靠它）。
//
// ★ 更隐蔽的一层：迁移原先用 addBlockEdgeTx —— 那是**结构边**写入器，
// 去重条件带 `COALESCE(session_id,”)=”`。
// 而要迁移的关系**恰恰带 session_id** ⇒ 每一条都被判成「已存在」跳过。
// 这层缺陷只有在「测试数据带 session」时才暴露 —— 所以判据必须造它。
func TestMigrateLegacy_关系属性完整迁移(t *testing.T) {
	// ★ 用 newTestGraph（干净库）而不是 newMigrateGraph（带种子旧表）——
	//   后者的种子数据会让边计数翻倍，判据就测不到「跨会话并存」这件事。
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	// 造旧表数据：同一对实体在**不同会话**各有一条同类型关系。
	// 迁移后必须是两条独立的边（关系边按设计允许并存）。
	// ★ 同时写块侧（Commit）与旧表（Seed*）——
	//   迁移的输入是旧表，而 Commit 已不写旧表（2026-10-04）。
	for i, sid := range []string{"sess-A", "sess-B"} {
		conf := 0.3 + 0.4*float64(i)
		if _, _, err := g.Commit([]Triple{
			{Subject: "迁移甲", Relation: "维护", Object: "迁移乙", Confidence: conf},
		}, sid, i+1); err != nil {
			t.Fatal(err)
		}
		if _, _, err := g.SeedLegacyRelation("迁移甲", "迁移乙", "维护",
			conf, sid, i+1); err != nil {
			t.Fatal(err)
		}
	}

	res, err := g.MigrateLegacyTextEntities(nil)
	if err != nil {
		t.Fatalf("迁移: %v", err)
	}
	fmt.Printf("  迁移 %d 边（去重 %d）\n", res.Edges, res.DedupedEdges)

	// ★ 基线数：Commit 自己已经写了块侧边（块化路径）。
	//   判据要比的是「迁移之后」而不是「迁移贡献了多少」——
	//   前者才是「迁移有没有搬过来」这个真问题。
	var baseEdges int
	if err := g.db.QueryRow(`SELECT COUNT(*) FROM memory_block_edges
		WHERE edge_type='维护'`).Scan(&baseEdges); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  迁移后「维护」边共 %d 条（Commit 原有 %d + 迁移 %d）\n",
		baseEdges, baseEdges-res.Edges, res.Edges)

	g.mu.RLock()
	defer g.mu.RUnlock()

	// ★ 跨会话的两条都必须存在（不能被 session_id 的去重条件吃掉）。
	//
	// ★ 只数**迁移新增**的那些：Commit 本身也写了块侧边（块化），
	//   所以库里本来就有 2 条。判据要测的是「迁移有没有把旧的 2 条
	//   也搬过来」—— 两者 session_id 不同，共存正是关系边的设计意图。
	// ★ 关键判据：迁移去重口径按 session。
	//
	//   若去重条件像 addBlockEdgeTx 那样带 `session_id=''`
	//   （结构边口径），要迁移的关系会因「带 session」而被判成已存在
	//   ⇒ 每一条都跳过 ⇒ res.Edges 为 0。
	//
	//   所以这里的判据是 **res.Edges == 旧表关系数**，
	//   而不是「库里共有几条边」—— 后者会被 Commit 自己写的块侧边干扰。
	if res.Edges != 2 {
		t.Errorf("★ 迁移应新增 2 条边（带 session 的不得被去重吃掉），实际 %d", res.Edges)
	}

	// ★ 迁移出的边必须带 session_id（Recall 的 sessionFilter 依赖它）
	var nMigratedSess int
	if err := g.db.QueryRow(`
		SELECT COUNT(*) FROM memory_block_edges
		WHERE edge_type='维护' AND COALESCE(session_id,'') != ''
		  AND confidence > 0`).Scan(&nMigratedSess); err != nil {
		t.Fatal(err)
	}
	if nMigratedSess < 2 {
		t.Errorf("★ 迁出的边应带 session_id 与 confidence，实际 %d 条符合", nMigratedSess)
	}

	// ★ confidence 必须迁过来
	var confidences []float64
	rows, err := g.db.Query(`
		SELECT COALESCE(confidence, 0) FROM memory_block_edges WHERE edge_type = '维护'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c float64
		_ = rows.Scan(&c)
		confidences = append(confidences, c)
	}
	rows.Close()
	fmt.Printf("  迁出的 confidence: %v\n", confidences)
	for _, c := range confidences {
		if c == 0 {
			t.Error("★ confidence 未迁移（生产实测 959 条边全 0）")
		}
	}

	// ★ session_id / turn_id 必须迁过来（Recall 的 sessionFilter 依赖）
}

// ★ 非 active 的旧关系不得迁成 active（否则已删除的记忆会复活）
func TestMigrateLegacy_已删除关系不复活(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	// ★ 同时写块侧与旧表（Commit 已不写旧表，2026-10-04）
	if _, _, err := g.Commit([]Triple{
		{Subject: "待删甲", Relation: "曾经", Object: "待删乙", Confidence: 1.0},
	}, "sess-X", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := g.SeedLegacyRelation("待删甲", "待删乙", "曾经",
		1.0, "sess-X", 1); err != nil {
		t.Fatal(err)
	}
	// 软删**块侧**（Purge 现在只改块）⇒ 形成判据要验的混合态
	if _, err := g.Purge(map[string]string{"subject_contains": "待删甲"}, "soft"); err != nil {
		t.Fatal(err)
	}

	// ★★ 判据要造的正是**混合态**：旧表行仍是 active，块侧已 deleted。
	//
	//   这不是人为 contrived —— 它是读侧切块后的**真实状态**：
	//   Purge 只改块侧，旧 relations 行停在 active。
	//   生产库有 14 条 deleted 关系 + memory_purge 工具调用历史，
	//   两者叠加就是这个形态。
	//
	//   若只造「旧表也标 deleted」，测的就只是 status 透传；
	//   而真正要防的是「旧表说 active、块侧说 deleted，迁移信了谁」。
	g.mu.Lock()
	var nStillActive int
	if err := g.db.QueryRow(
		`SELECT COUNT(*) FROM relations WHERE status='active'`).Scan(&nStillActive); err != nil {
		g.mu.Unlock()
		t.Fatal(err)
	}
	if nStillActive == 0 {
		g.mu.Unlock()
		t.Fatal("★ 判据前提不成立：旧表不应有 active 行（否则测不到混合态）")
	}
	g.mu.Unlock()
	if _, err := g.MigrateLegacyTextEntities(nil); err != nil {
		t.Fatalf("迁移: %v", err)
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	var nActive int
	if err := g.db.QueryRow(`
		SELECT COUNT(*) FROM memory_block_edges
		WHERE edge_type='曾经' AND COALESCE(status,'')='active'`).Scan(&nActive); err != nil {
		t.Fatal(err)
	}
	if nActive != 0 {
		t.Errorf("★ 已删除的旧关系被迁成 active（记忆会复活），实际 %d 条", nActive)
	}
}
