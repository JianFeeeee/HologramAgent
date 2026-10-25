package memory

import (
	"math"
	"testing"
	"time"
)

// ★ 持久化层的判据。三个要点：
//
// ① 往返无损（存进去读出来一致）
// ② 失效判定（块数变化过大 → 不启用，但不是错误）
// ③ 指纹歧义拒绝（库里混了多个向量空间时不能瞎猜）

func TestCentroidStore_往返(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	// ★ 走真实路径：先有块，再 RebuildCentroid。
	//
	// 初版是「外部喂向量给 SaveCentroid」，但那时库里一个块都没有 ——
	// 于是失效判定必然触发（样本 3 vs 库内 0），测试一直红。
	// ★ 教训：绕过了真实调用顺序的测试，测的是我想象的路径。
	const fp = "fp-test-1"
	putVecBlocks(t, g, fp, "b", 6, 4)
	c, anomalous, err := g.RebuildCentroid(fp)
	if err != nil {
		t.Fatal(err)
	}
	st := c.Stats()
	if !anomalous || !st.Anomalous {
		t.Fatalf("构造的数据应 anomalous，实际 %v / 均值范数 %.4f", anomalous, st.MeanLength)
	}
	loaded, use, err := g.LoadCentroid(fp, 4)
	if err != nil {
		t.Fatalf("LoadCentroid: %v", err)
	}
	if !use {
		t.Fatalf("刚存的中心应可用（anomalous=%v）", st.Anomalous)
	}
	if loaded.Dim != 4 {
		t.Errorf("维度应为 4，实际 %d", loaded.Dim)
	}
	// 往返：CenterVector 结果应一致
	v := []float64{1, 0.85, 0.75, 0.65}
	a := c.CenterVector(v)
	b := loaded.CenterVector(v)
	if cosineOf(a, b) < 0.999999 {
		t.Errorf("往返后 CenterVector 结果不一致：cos=%.9f",
			cosineOf(a, b))
	}

	// 状态报告
	status, err := g.CentroidStatusOf(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Saved || status.Dim != 4 || !status.Anomalous {
		t.Errorf("状态报告不对：%+v", status)
	}
	if status.MeanLength < 0.9 {
		t.Errorf("均值范数应保留在状态里，实际 %.4f", status.MeanLength)
	}
	t.Logf("状态: dim=%d 样本=%d 均值范数=%.4f anomalous=%v 建于=%s",
		status.Dim, status.VectoredCount, status.MeanLength,
		status.Anomalous, status.BuiltAt)
}

// ★ 维度不符时不启用（不是错误）——中心化会按错误维度越界。
func TestCentroidStore_维度不符不启用(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	putVecBlocks(t, g, "fp-dim", "d", 5, 2)
	if _, _, err := g.RebuildCentroid("fp-dim"); err != nil {
		t.Fatal(err)
	}
	// 查询向量维度 512，中心维度 2
	_, use, err := g.LoadCentroid("fp-dim", 512)
	if err != nil {
		t.Errorf("维度不符不该是错误，应返回 nil 错误，实际 %v", err)
	}
	if use {
		t.Error("维度不符时不该启用中心化")
	}
	// 维度对得上才启用
	_, use2, err := g.LoadCentroid("fp-dim", 2)
	if err != nil || !use2 {
		t.Errorf("维度一致时应启用，实际 use=%v err=%v", use2, err)
	}
}

// ★ 块数变化过大 → 失效（不是错误）。中心化会随库漂移，那比不校正更糟。
func TestCentroidStore_块数变化失效(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	// 先放 3 块建中心，再加 20 块（3 → 23，变化 667% >> 20%）
	putVecBlocks(t, g, "fp-stale", "a", 3, 2)
	if _, _, err := g.RebuildCentroid("fp-stale"); err != nil {
		t.Fatal(err)
	}
	putVecBlocks(t, g, "fp-stale", "z", 20, 2)
	_, use, err := g.LoadCentroid("fp-stale", 2)
	if err != nil {
		t.Errorf("块数变化不该是错误，实际 %v", err)
	}
	if use {
		t.Error("块数变化 567% 时中心应失效")
	}
	status, _ := g.CentroidStatusOf("fp-stale")
	if !status.Stale {
		t.Errorf("状态应标记 stale，实际 %+v", status)
	}
	t.Logf("状态: 建时样本=%d 现在=%d stale=%v",
		status.VectoredCount, status.CurrentBlocks, status.Stale)
}

// 少量变化不该失效（否则每次写入都让相似度漂移）。
func TestCentroidStore_少量变化不失效(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	// 先放 100 个块建中心，再加 5 个（100 → 105，5% < 20%）
	putVecBlocks(t, g, "fp-stable", "S", 100, 2)
	if _, _, err := g.RebuildCentroid("fp-stable"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		b := MemoryBlock{ID: string(rune('A' + i)), Modality: BlockText,
			Text: string(rune('A' + i)), Vector: []float64{1, 0.5},
			Fingerprint: "fp-stable", CreatedAt: time.Now()}
		if err := g.PutMemoryBlocks([]MemoryBlock{b}); err != nil {
			t.Fatal(err)
		}
	}
	_, use, err := g.LoadCentroid("fp-stable", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !use {
		t.Error("5% 变化不该让中心失效（否则召回结果随每次写入漂移）")
	}
}

// ★ 库里混多个向量空间时，fingerprint 为空必须拒绝而不是瞎猜。
func TestRebuildCentroid_指纹歧义拒绝(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	for i := 0; i < 4; i++ {
		fp := "fp-a"
		if i >= 2 {
			fp = "fp-b"
		}
		b := MemoryBlock{ID: string(rune('a' + i)), Modality: BlockText,
			Text: string(rune('a' + i)), Vector: []float64{1, 0.5, 0.3},
			Fingerprint: fp, CreatedAt: time.Now()}
		if err := g.PutMemoryBlocks([]MemoryBlock{b}); err != nil {
			t.Fatal(err)
		}
	}
	// 显式给指纹 → 只统计该空间的块
	c, _, err := g.RebuildCentroid("fp-a")
	if err != nil {
		t.Fatal(err)
	}
	if c.Count != 2 {
		t.Errorf("显式给指纹时应只统计该空间，实际样本 %d（期望 2）", c.Count)
	}
	// 不给指纹 → 拒绝（两个空间都在）
	_, _, err = g.RebuildCentroid("")
	if err == nil {
		t.Error("指纹歧义时不该默默选一个空间")
	}
	t.Logf("拒绝原因: %v", err)
}

// putVecBlocks 写入 n 个「强共同分量 + 弱个体差异」的块（已归一化）。
//
// 归一化是必须的：初版忘了归一，MeanLength 算出 1.1158 /
// AvgPairCosine 1.245 —— 两个数学上不可能的值，诊断指标本身就不成立。
func putVecBlocks(t *testing.T, g *GraphDB, fp, prefix string, n, dim int) {
	t.Helper()
	for i := 0; i < n; i++ {
		v := make([]float64, dim)
		for k := range v {
			v[k] = 1.0 // 强共同分量（造各向异性）
		}
		v[dim-1] += float64(i) / float64(n+1) // 弱个体差异
		if !l2Normalize(v) {
			t.Fatalf("第 %d 个向量归一化失败", i)
		}
		b := MemoryBlock{
			ID: prefix + "-" + itoaTest(i), Modality: BlockText,
			Text: prefix + "-块-" + itoaTest(i), Vector: v,
			Fingerprint: fp, CreatedAt: time.Now(),
		}
		if err := g.PutMemoryBlocks([]MemoryBlock{b}); err != nil {
			t.Fatal(err)
		}
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// ★ 接线判据：RecallBlocks 必须真的用中心化后的分数。
//
// ★ 这条是补上的 —— 变异自证时发现两个变异都判成通过：
//
//	变异1 查询向量不中心化（只处理块向量）→ 端到端探针仍绿
//	变异2 RecallBlocks 不加载中心        → 端到端探针仍绿
//
// 原因：中心是在测试**运行前**手工建好的，探针跑的是"库里有没有中心"
// 而不是"打分路径有没有用中心"。所以判据必须直接盯分数。
//
// 三个断言，逐层收紧：
//  1. 同一个块，开/关中心化的**分数不同**（路径真的走了）
//  2. SkipCentroid 时分数与不提供中心的基准**完全相同**（开关真的有效）
//  3. 中心化后**整体分数下降**（公共分量被减掉的必然结果）
//
// ★ 我最初写成「构造两个排序相反的块看 top1 是否翻转」，三次都失败：
//
//	手工设计让排序反转的几何关系很容易出错，而失败时无法区分
//	「构造不对」与「代码没生效」。改成直接断言分数，判据就不再依赖构造。
func TestRecallBlocks_接线用中心(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	const fp = "fp-wire"
	const dim = 4

	putRawBlock(t, g, fp, "bulk-0", []float64{1, 1, 0, 0})
	putRawBlock(t, g, fp, "bulk-1", []float64{1, 1, 0.01, 0})
	putRawBlock(t, g, fp, "bulk-2", []float64{1, 1, 0.02, 0})
	putRawBlock(t, g, fp, "target", []float64{0.35, 0.35, 0.9, 0.08})
	if _, _, err := g.RebuildCentroid(fp); err != nil {
		t.Fatal(err)
	}
	qv := []float64{0.2, 0.2, 0.94, 0.1}

	raw, err := g.RecallBlocks(BlockRecallQuery{
		Vector: qv, Fingerprint: fp, TopK: 10, SkipCentroid: true})
	if err != nil {
		t.Fatal(err)
	}
	cen, err := g.RecallBlocks(BlockRecallQuery{
		Vector: qv, Fingerprint: fp, TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || len(cen) == 0 {
		t.Fatal("召回为空")
	}

	// 断言 1：同一块的分数不同
	diffFound := false
	for i := range raw {
		if raw[i].Block.ID == cen[i].Block.ID &&
			raw[i].Score != cen[i].Score {
			t.Logf("  %s: 原始 %.4f → 中心化 %.4f（差 %.4f）",
				raw[i].Block.ID, raw[i].Score, cen[i].Score,
				raw[i].Score-cen[i].Score)
			diffFound = true
		}
	}
	if !diffFound {
		t.Error("开/关中心化的分数完全相同 —— 打分路径很可能没走中心化")
	}

	// 断言 3：中心化后整体分数下降（公共分量被减掉的必然结果）
	var rawSum, cenSum float64
	for _, h := range raw {
		rawSum += h.Score
	}
	for _, h := range cen {
		cenSum += h.Score
	}
	t.Logf("  总分: 原始 %.4f → 中心化 %.4f", rawSum, cenSum)
	if cenSum >= rawSum {
		t.Errorf("中心化后总分应下降（公共分量被减掉），实际 %.4f → %.4f",
			rawSum, cenSum)
	}

	// 断言 2：SkipCentroid 的结果与「库里有中心但被跳过」完全一致
	again, err := g.RecallBlocks(BlockRecallQuery{
		Vector: qv, Fingerprint: fp, TopK: 10, SkipCentroid: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		if again[i].Block.ID != raw[i].Block.ID || again[i].Score != raw[i].Score {
			t.Errorf("SkipCentroid 应完全确定（可复现），第 %d 位不一致：%s/%v vs %s/%v",
				i, again[i].Block.ID, again[i].Score, raw[i].Block.ID, raw[i].Score)
		}
	}
	_ = dim
}

// ★ 双边中心化：查询向量自己也必须中心化。
//
// 这是最容易写错的一处 —— 只减块向量不减查询向量，等于在混两种量纲
// （一半带公共分量、一半不带）。而它**不会报错、不会 panic**，
// 只是让相似度变得没有意义，所以必须专门判。
//
// 构造：查询向量含大量公共分量 [1,1,0,0]，块也含。
// 只减块向量时，查询的公共分量原封不动地参与点积 ⇒ 分数虚高；
// 双边都减时公共分量被抵消 ⇒ 分数回落。
func TestRecallBlocks_双边中心化(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()
	const fp = "fp-both"

	putRawBlock(t, g, fp, "b-0", []float64{1, 1, 0, 0})
	putRawBlock(t, g, fp, "b-1", []float64{1, 1, 0.01, 0})
	putRawBlock(t, g, fp, "b-2", []float64{1, 1, 0.02, 0})
	putRawBlock(t, g, fp, "b-3", []float64{1, 1, 0.03, 0})
	if _, _, err := g.RebuildCentroid(fp); err != nil {
		t.Fatal(err)
	}

	// 查询向量：公共分量占绝对主导（接近全部块的方向）
	qv := []float64{1, 1, 0.05, 0}

	withCentroid, err := g.RecallBlocks(BlockRecallQuery{
		Vector: qv, Fingerprint: fp, TopK: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(withCentroid) == 0 {
		t.Fatal("召回为空")
	}

	// 手工算三种算法，比对实现用的是哪一种
	cent, _, err := g.LoadCentroid(fp, 4)
	if err != nil || cent == nil {
		t.Fatalf("LoadCentroid: %v", err)
	}
	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	var refBoth, refBlockOnly, refNone float64
	for _, b := range blocks {
		refNone += cosine(b.Vector, qv)
		refBlockOnly += cosine(cent.CenterVector(b.Vector), qv)
		refBoth += cosine(cent.CenterVector(b.Vector), cent.CenterVector(qv))
	}
	var got float64
	for _, h := range withCentroid {
		got += h.Score
	}
	t.Logf("  实现总分        %.6f", got)
	t.Logf("  双边中心化参考  %.6f", refBoth)
	t.Logf("  只减块向量参考  %.6f", refBlockOnly)
	t.Logf("  完全不中心化    %.6f", refNone)

	if math.Abs(got-refBoth) > 1e-6 {
		t.Errorf("实现应等于「双边中心化」，实际 %.6f（只减块=%.6f，不减=%.6f）",
			got, refBlockOnly, refNone)
	}
}

func putRawBlock(t *testing.T, g *GraphDB, fp, id string, v []float64) {
	t.Helper()
	if !l2Normalize(v) {
		t.Fatalf("%s 归一化失败", id)
	}
	b := MemoryBlock{ID: id, Modality: BlockText, Text: id, Vector: v,
		Fingerprint: fp, CreatedAt: time.Now()}
	if err := g.PutMemoryBlocks([]MemoryBlock{b}); err != nil {
		t.Fatal(err)
	}
}

// ── 2026-10-25：graph_centroid 必须随库创建，不能等第一次 SaveCentroid ──
//
// 缺陷现场（WSL2 真机诊断报告问题 5）：新建库上每轮召回都打
//
//	`[graph] recall blocks: reading centroid failed, falling back to raw vectors:
//	 graph: load centroid: no such table: graph_centroid`
//
// 417KB 日志里占相当比例，并回退到全量 raw vectors 扫描。
//
// 根因：buildSchemas 的 schemas 列表里漏了 graph_centroid，它只在
// SaveCentroid 内部被 ensureCentroidSchema 创建——而新库在存过第一个
// 中心向量之前从不调 SaveCentroid。
//
// 判据刻意**不先调 SaveCentroid**：只要一存就会建表，那样就盖住了缺陷
// （变异验证：把 graph.go 里的 ddlCentroid 那行删掉，本判据必须变红）。
func TestGraphCentroidTableExistsOnNewDB(t *testing.T) {
	g := newTestGraph(t)
	defer g.Close()

	var name string
	err := g.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='graph_centroid'`).Scan(&name)
	if err != nil {
		t.Fatalf("新建库应已有 graph_centroid 表，实际查询失败: %v", err)
	}
	if name != "graph_centroid" {
		t.Fatalf("表名不符: %q", name)
	}
}

// 判据二：新建库上直接 LoadCentroid 不得报错。
//
// 这条贴着**真实故障路径**（召回时调 LoadCentroid），比只查 sqlite_master
// 更接近用户看到的现象：查表不存在会以 error 形式冒出来。
func TestLoadCentroidOnNewDBDoesNotFail(t *testing.T) {
	g := newTestGraph(t)
	defer g.Close()

	// 空库查询：应当"查不到"（nil, false）而不是"表不存在"（error）。
	c, anomalous, err := g.LoadCentroid("any-fingerprint", 512)
	if err != nil {
		t.Fatalf("空库 LoadCentroid 不应报错（表必须已存在），实际: %v", err)
	}
	if c != nil {
		t.Fatalf("空库不应返回中心向量，实际: %+v", c)
	}
	if anomalous {
		t.Fatal("空库不应标记 anomalous")
	}
}
