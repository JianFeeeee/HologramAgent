package memory

// P1 段页式字段的判据（C5：迁移后 ID / 帧号 / 指纹逐字节相同）。
//
// ## 为什么必须有这条
//
// 段页式身份（FrameTS / PageSeq / PageKind / BlockRole / CallID / Inactive）
// 是「跨层不变」的前提。一旦某个写入路径把字段抹掉，块就再也归不回原来的页，
// 渲染时会破配对 —— 而这类错**不报错**，只是行为悄悄变了。
//
// 本文件钉四件事：
//  ① 六个字段能写进去、能读回来（往返不丢）
//  ② 轻量读取路径（不带这些字段的块）写回时**不覆盖**已有页身份
//  ③ inactive 是标注，能被显式改回 false（与"空值不覆盖"不同）
//  ④ 零值 FrameTS 存成 NULL，不写成 0001-01-01

import (
	"testing"
	"time"
)

// ① 六个字段往返。
func TestBlockPageFieldsRoundTrip(t *testing.T) {
	g := newTestGraph(t)
	ts := time.Date(2026, 10, 29, 15, 4, 5, 123456789, time.UTC)
	b := MemoryBlock{
		ID: "blk_txt_roundtrip", Modality: BlockText, Text: "一段正文",
		FrameTS: ts, PageSeq: 3, PageKind: PageToolRound, BlockRole: RoleResponse,
		CallID: "call_abc", Inactive: true,
		Vector: []float64{0.1, 0.2}, Fingerprint: "fp-1",
	}
	if err := g.PutMemoryBlocks([]MemoryBlock{b}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := g.BlockByIDPrefix("blk_txt_roundtrip")
	if err != nil || !ok {
		t.Fatalf("读回失败: %v ok=%v", err, ok)
	}
	if !got.FrameTS.Equal(ts) {
		t.Errorf("FrameTS 往返不一致: 写 %v 读 %v", ts, got.FrameTS)
	}
	if got.PageSeq != 3 || got.PageKind != PageToolRound ||
		got.BlockRole != RoleResponse || got.CallID != "call_abc" || !got.Inactive {
		t.Errorf("页字段往返不一致: %+v", got)
	}
	if got.Fingerprint != "fp-1" || len(got.Vector) != 2 {
		t.Errorf("指纹/向量往返不一致: fp=%q vec=%v", got.Fingerprint, got.Vector)
	}
}

// ② 轻量写回不覆盖页身份。
//
// 场景：BlockByIDPrefix 这类 lean 读取拿到的块不带页字段，
// 某些路径会把它重新 Put 回去（如挂场景）。若直接覆盖，
// 页身份被抹 → 块归不回页 → 渲染破配对。
func TestPageFieldsNotOverwrittenByLeanWrite(t *testing.T) {
	g := newTestGraph(t)
	ts := time.Date(2026, 10, 29, 16, 0, 0, 0, time.UTC)
	full := MemoryBlock{
		ID: "blk_txt_keep", Modality: BlockText, Text: "正文",
		FrameTS: ts, PageSeq: 2, PageKind: PageLLMRound, BlockRole: RoleRequest,
		CallID: "call_keep",
	}
	if err := g.PutMemoryBlocks([]MemoryBlock{full}); err != nil {
		t.Fatal(err)
	}

	// 模拟 lean 写回：只有 ID + 文本，页字段全零
	lean := MemoryBlock{ID: "blk_txt_keep", Modality: BlockText, Text: "正文"}
	if err := g.PutMemoryBlocks([]MemoryBlock{lean}); err != nil {
		t.Fatal(err)
	}

	got, _, _ := g.BlockByIDPrefix("blk_txt_keep")
	if !got.FrameTS.Equal(ts) || got.PageSeq != 2 || got.CallID != "call_keep" {
		t.Fatalf("★ 轻量写回抹掉了页身份: FrameTS=%v PageSeq=%d CallID=%q",
			got.FrameTS, got.PageSeq, got.CallID)
	}
}

// ③ inactive 必须能被显式改回 false（"读回后恢复渲染"）。
func TestInactiveCanBeCleared(t *testing.T) {
	g := newTestGraph(t)
	b := MemoryBlock{ID: "blk_txt_inact", Modality: BlockText, Text: "x", Inactive: true}
	if err := g.PutMemoryBlocks([]MemoryBlock{b}); err != nil {
		t.Fatal(err)
	}
	b.Inactive = false
	if err := g.PutMemoryBlocks([]MemoryBlock{b}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := g.BlockByIDPrefix("blk_txt_inact")
	if got.Inactive {
		t.Fatal("★ inactive 无法改回 false —— 「剔除后可恢复渲染」就失效了")
	}
}

// ④ 零值 FrameTS 存 NULL，不能写成 0001-01-01。
//
// 若写成 0001-01-01，迁移脚本会把它当成一个真实的极早帧，
// 于是所有存量块被错误归入同一页。
func TestZeroFrameTSStoredAsNull(t *testing.T) {
	g := newTestGraph(t)
	if err := g.PutMemoryBlocks([]MemoryBlock{
		{ID: "blk_txt_noft", Modality: BlockText, Text: "旧数据"},
	}); err != nil {
		t.Fatal(err)
	}
	var nullCount int
	if err := g.db.QueryRow(
		`SELECT COUNT(*) FROM memory_blocks WHERE id='blk_txt_noft' AND frame_ts IS NULL`).
		Scan(&nullCount); err != nil {
		t.Fatal(err)
	}
	if nullCount != 1 {
		t.Fatal("★ 零值 FrameTS 应存成 NULL，否则存量块会被错误聚成同一帧")
	}
}
