package memory

import (
	"path/filepath"
	"testing"
)

// BackfillBlockData 把旧 CAS（payload_digest）的字节搬进块自身的 data 列。
//
// ★ 测试不 import internal/memory/media：字节来源是注入的 resolver，
//   正是为了让 memory 包对 CAS 零依赖（step⑥ 要删 media 包）。
//   真实 CAS 读盘 + 生产数据 e2e 由 cmd/homed-media-backfill 承担。
//
// 判据是**不变量**而不是「函数被调过」：
//  1. 回填后 BlockData(id) 能取回字节，且与来源逐字节一致
//  2. 幂等：再跑一次是空操作（已回填的块不再处理）
//  3. 来源取不到字节的块计 Missing 并跳过，不阻断其余回填
//  4. 回填只改 data 列，不动块 ID / 不建块 / 不删块
func TestBackfillBlockData_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	g, err := NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	// 旧 CAS 的等价物：digest → 字节
	cas := map[string][]byte{
		"d1": []byte("img-one"),
		"d2": []byte("img-two"),
		"d3": []byte("img-three"),
	}

	// 造 3 个媒体块，带 payload_digest（旧形态：块 ID 不是内容派生）
	// ★ 必须用原生 SQL 写 payload_digest：MemoryBlock 结构体已删该字段，
	//   PutMemoryBlocks 写不进去。这正是存量生产库的形态——
	//   块 ID 是 blk_<ts>，data 为空，payload_digest 指向 CAS。
	// ★ 新库的 CREATE 不含该列（block union 化后删掉了），而「旧库有这列」
	//   正是迁移的前提，所以先补列来模拟旧库形态。
	if err := g.ensurePayloadDigestColumn(); err != nil {
		t.Fatalf("ensure payload_digest: %v", err)
	}
	for _, b := range []struct{ id, digest string }{
		{"blk_old_1", "d1"},
		{"blk_old_2", "d2"},
		{"blk_old_3", "d3"},
	} {
		if _, err := g.db.Exec(`INSERT INTO memory_blocks
			(id, modality, payload_digest, mime, created_at, updated_at)
			VALUES (?, 'image', ?, 'image/png', datetime('now'), datetime('now'))`,
			b.id, b.digest); err != nil {
			t.Fatalf("insert %s: %v", b.id, err)
		}
	}

	// 回填前：data 列为空（块形态对，但字节不在块里）
	before, _ := g.CountBlockDataBackfillCandidates()
	if before != 3 {
		t.Fatalf("回填前候选应为 3，实际 %d", before)
	}
	for _, b := range []string{"blk_old_1", "blk_old_2", "blk_old_3"} {
		if data, err := g.BlockData(b); err == nil && len(data) > 0 {
			t.Fatalf("回填前块 %s 不该有字节", b)
		}
	}

	resolve := func(digest string) ([]byte, bool) {
		data, ok := cas[digest]
		return data, ok
	}

	// 执行回填
	st, err := g.BackfillBlockData(resolve)
	if err != nil {
		t.Fatalf("BackfillBlockData: %v", err)
	}
	if st.Candidates != 3 || st.Filled != 3 || st.Missing != 0 {
		t.Fatalf("回填统计不对: %+v", st)
	}
	if st.SizeFilled != 7+7+9 {
		t.Errorf("SizeFilled = %d，期望 23", st.SizeFilled)
	}

	// 判据 1：BlockData 取回的字节与来源逐字节一致
	for _, b := range []struct{ id, digest string }{
		{"blk_old_1", "d1"},
		{"blk_old_2", "d2"},
		{"blk_old_3", "d3"},
	} {
		got, err := g.BlockData(b.id)
		if err != nil {
			t.Fatalf("BlockData(%s): %v", b.id, err)
		}
		if string(got) != string(cas[b.digest]) {
			t.Errorf("块 %s 字节 = %q，期望 %q", b.id, got, cas[b.digest])
		}
	}

	// 判据 2：幂等 —— 再跑一次是空操作
	st2, err := g.BackfillBlockData(resolve)
	if err != nil {
		t.Fatalf("幂等回填: %v", err)
	}
	if st2.Candidates != 0 || st2.Filled != 0 {
		t.Fatalf("幂等应空操作，实际 %+v", st2)
	}

	// 判据 4：候选清零，且块 ID 原样保留（迁移不换身份）
	after, _ := g.CountBlockDataBackfillCandidates()
	if after != 0 {
		t.Errorf("回填后候选应为 0，实际 %d", after)
	}
	ids, _ := g.SampleFilledBlockIDs(10)
	if len(ids) != 3 {
		t.Errorf("带字节块数应为 3，实际 %d", len(ids))
	}
}

// 来源里取不到字节的块计 Missing 并跳过，不阻断其余回填。
func TestBackfillBlockData_MissingDigestSkipped(t *testing.T) {
	dir := t.TempDir()
	g, err := NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	if err := g.PutMemoryBlocks([]MemoryBlock{
		{ID: "blk_ok", Modality: BlockImage, MIME: "image/png"},
		{ID: "blk_lost", Modality: BlockImage, MIME: "image/png"},
	}); err != nil {
		t.Fatal(err)
	}
	// payload_digest 用原生 SQL 注入（同上原因；先确保列存在）
	if err := g.ensurePayloadDigestColumn(); err != nil {
		t.Fatalf("ensure payload_digest: %v", err)
	}
	if _, err := g.db.Exec(`UPDATE memory_blocks SET payload_digest = 'exists' WHERE id = 'blk_ok'`); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`UPDATE memory_blocks SET payload_digest = 'gone' WHERE id = 'blk_lost'`); err != nil {
		t.Fatal(err)
	}

	// resolver 只认 "exists"
	resolve := func(digest string) ([]byte, bool) {
		if digest == "exists" {
			return []byte("bytes"), true
		}
		return nil, false
	}

	st, err := g.BackfillBlockData(resolve)
	if err != nil {
		t.Fatalf("BackfillBlockData: %v", err)
	}
	if st.Candidates != 2 || st.Filled != 1 || st.Missing != 1 {
		t.Fatalf("统计不对: %+v", st)
	}

	// ok 块有字节，lost 块仍空
	if data, _ := g.BlockData("blk_ok"); string(data) != "bytes" {
		t.Errorf("blk_ok 字节不对: %q", data)
	}
	if data, _ := g.BlockData("blk_lost"); len(data) != 0 {
		t.Errorf("blk_lost 不该有字节: %q", data)
	}
}

// 迁移是幂等的：连跑三次，结果与跑一次相同（不重复计数、不覆盖已有字节）。
func TestBackfillBlockData_IdempotentThreeRuns(t *testing.T) {
	dir := t.TempDir()
	g, _ := NewGraphDB(filepath.Join(dir, "graph.db"))
	t.Cleanup(func() { g.Close() })

	if err := g.ensurePayloadDigestColumn(); err != nil {
		t.Fatal(err)
	}
	if _, err := g.db.Exec(`INSERT INTO memory_blocks (id, modality, payload_digest, created_at, updated_at)
		VALUES ('b1', 'image', 'k', datetime('now'), datetime('now'))`); err != nil {
		t.Fatal(err)
	}

	resolve := func(string) ([]byte, bool) { return []byte("payload"), true }

	total := 0
	for i := 0; i < 3; i++ {
		st, err := g.BackfillBlockData(resolve)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		total += st.Filled
	}
	if total != 1 {
		t.Errorf("三次回填累计 Filled = %d，期望 1（只有第一次真回填）", total)
	}
	if data, _ := g.BlockData("b1"); string(data) != "payload" {
		t.Errorf("字节丢失: %q", data)
	}
}

// 全新库（CREATE 不含 payload_digest 列、无历史数据）上回填必须安全。
//
// 这是 ensurePayloadDigestColumn 存在的理由：block union 化后新建的库
// 根本没有该列，而迁移命令可能被误跑在新库上——此时应当补列后得到
// 「0 候选」，而不是报 "no such column" 崩掉。
func TestBackfillBlockData_FreshDBSafeNoOp(t *testing.T) {
	dir := t.TempDir()
	g, _ := NewGraphDB(filepath.Join(dir, "graph.db"))
	t.Cleanup(func() { g.Close() })

	// 放几个正常写入的块（新形态：字节直接在 data，无 payload_digest）
	if err := g.PutMemoryBlocks([]MemoryBlock{
		{ID: "blk_new_1", Modality: BlockImage, MIME: "image/png", Data: []byte("already-inline")},
		{ID: "blk_new_2", Modality: BlockText, Text: "hello"},
	}); err != nil {
		t.Fatal(err)
	}

	// 候选查询不得因缺列而报错
	n, err := g.CountBlockDataBackfillCandidates()
	if err != nil {
		t.Fatalf("CountBlockDataBackfillCandidates 在新库上报错: %v", err)
	}
	if n != 0 {
		t.Errorf("新库候选应为 0，实际 %d", n)
	}

	// 回填是空操作，不碰已有字节
	st, err := g.BackfillBlockData(func(string) ([]byte, bool) { return nil, false })
	if err != nil {
		t.Fatalf("新库回填报错: %v", err)
	}
	if st.Candidates != 0 || st.Filled != 0 {
		t.Errorf("新库回填应空操作，实际 %+v", st)
	}
	if data, _ := g.BlockData("blk_new_1"); string(data) != "already-inline" {
		t.Errorf("新库已有字节被改: %q", data)
	}
}
