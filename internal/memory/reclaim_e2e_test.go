package memory

// stablemem 回收的 e2e 判据（P6，2026-10-29）。
//
// ## 钉住的四件事
//
//	① last_recalled_at 在召回时被刷新（否则回收等于按入库时间瞎删）
//	② 只前进不后退（轻量写回不能把"用过"抹成"从没用过"）
//	③ 零值存 NULL（"从未召回" ≠ "很久以前召回过"）
//	④ 删块必须连带删边（memory_block_edges 无外键，不清理就留悬挂边）

import (
	"testing"
	"time"
)

// ① 召回后 last_recalled_at 被写回。
func TestRecallMarksLastRecalledAt(t *testing.T) {
	g := newTestGraph(t)
	b := MemoryBlock{
		ID: "blk_txt_recal1", Modality: BlockText, Text: "关于磁盘清理的办法",
		Vector: []float64{1, 0, 0}, Fingerprint: "fp-r",
	}
	if err := g.PutMemoryBlocks([]MemoryBlock{b}); err != nil {
		t.Fatal(err)
	}

	hits, err := g.RecallBlocks(BlockRecallQuery{
		Vector: []float64{1, 0, 0}, TopK: 5, Fingerprint: "fp-r",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("未召回")
	}
	// markRecalled 是 defer 调用的，这里等一下再读（它同步执行，defer 在返回前）
	got, ok, err := g.BlockByIDPrefix("blk_txt_recal1")
	if err != nil || !ok {
		t.Fatalf("读回失败: %v ok=%v", err, ok)
	}
	if got.LastRecalledAt.IsZero() {
		t.Fatalf("★ 召回未记录 last_recalled_at —— 回收会按入库时间瞎删")
	}
}

// ② 只前进不后退：轻量写回（零值）不能抹掉已记录的召回时间。
func TestLastRecalledAtOnlyMovesForward(t *testing.T) {
	g := newTestGraph(t)
	past := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	full := MemoryBlock{
		ID: "blk_txt_recal2", Modality: BlockText, Text: "正文",
		LastRecalledAt: past,
	}
	if err := g.PutMemoryBlocks([]MemoryBlock{full}); err != nil {
		t.Fatal(err)
	}

	// 轻量写回：不带 last_recalled_at
	if err := g.PutMemoryBlocks([]MemoryBlock{
		{ID: "blk_txt_recal2", Modality: BlockText, Text: "正文"},
	}); err != nil {
		t.Fatal(err)
	}

	got, _, _ := g.BlockByIDPrefix("blk_txt_recal2")
	if got.LastRecalledAt.IsZero() {
		t.Fatalf("★ 轻量写回把 last_recalled_at 抹成零值 —— 「活块」会被误判为死块删掉")
	}
	if !got.LastRecalledAt.Equal(past) {
		t.Fatalf("时间被改写: %v（期望 %v）", got.LastRecalledAt, past)
	}

	// 更新的时间要能推进
	newer := past.Add(72 * time.Hour)
	if err := g.PutMemoryBlocks([]MemoryBlock{
		{ID: "blk_txt_recal2", Modality: BlockText, Text: "正文", LastRecalledAt: newer},
	}); err != nil {
		t.Fatal(err)
	}
	got2, _, _ := g.BlockByIDPrefix("blk_txt_recal2")
	if !got2.LastRecalledAt.Equal(newer) {
		t.Fatalf("更新的召回时间未生效: %v（期望 %v）", got2.LastRecalledAt, newer)
	}
}

// ③ 从未召回存 NULL（区别于"很久以前召回过"）。
func TestNeverRecalledStoredAsNull(t *testing.T) {
	g := newTestGraph(t)
	if err := g.PutMemoryBlocks([]MemoryBlock{
		{ID: "blk_txt_never", Modality: BlockText, Text: "从未被召回"},
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := g.db.QueryRow(
		`SELECT COUNT(*) FROM memory_blocks WHERE id='blk_txt_never' AND last_recalled_at IS NULL`).
		Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("★ 从未召回应存 NULL（零值时间会与'很久以前召回过'混淆）")
	}
}

// ④ 删块必须连带删边（否则留悬挂边）。
//
// memory_block_edges **没有外键约束**（边是独立单位，见 DDL 注释），
// 所以不能指望 DB 级联 —— 清理必须在删除代码里显式做。
//
// 走真实路径 DeleteEntity：它按 text_content 精确匹配找出块，
// 然后①删关联边 ②删块 ③同步删旧表。
func TestDeleteBlockRemovesEdges(t *testing.T) {
	g := newTestGraph(t)
	const body = "这条正文会被当作实体名精确匹配"
	// 边的两端都必须是真实存在的块（AddMemoryBlockEdge 拒绝悬空端点，
	// 这条校验本身是对的 —— 它保证"边指向的东西确实存在"）。
	if err := g.PutMemoryBlocks([]MemoryBlock{
		{ID: "blk_txt_orphan", Modality: BlockText, Text: body},
		{ID: "blk_other", Modality: BlockText, Text: "对端块"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := g.AddMemoryBlockEdge("block", "blk_txt_orphan", "block", "blk_other", "contains"); err != nil {
		t.Fatal(err)
	}

	before := countBlockEdges(t, g, "blk_txt_orphan")
	if before == 0 {
		t.Fatal("前置条件失败：边未建立")
	}

	if _, err := g.DeleteEntity(body); err != nil {
		t.Fatal(err)
	}

	after := countBlockEdges(t, g, "blk_txt_orphan")
	if after != 0 {
		t.Fatalf("★ 删除块后残留 %d 条悬挂边（无外键，必须显式清理）", after)
	}
}

// countBlockEdges 数某个块作为端点的边。
func countBlockEdges(t *testing.T, g *GraphDB, blockID string) int {
	t.Helper()
	var n int
	if err := g.db.QueryRow(
		`SELECT COUNT(*) FROM memory_block_edges
		 WHERE (source_kind='block' AND source_id=?) OR (target_kind='block' AND target_id=?)`,
		blockID, blockID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
