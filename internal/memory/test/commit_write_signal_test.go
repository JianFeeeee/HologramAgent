package test

// CommitReady 的模块职责契约：区分「真的一条没写」与「写了但幂等命中」。
//
// ## 钉的是什么（2026-11-15 实测驱动的修正）
//
// 归档路径曾用 `MemoryBlockCount` 的**前后差值**判断「这次提交有没有写进图库」。
// 块 ID 由内容派生（sha256(实体名)）⇒ 同一实体第二次提交走
// `ON CONFLICT DO UPDATE` ⇒ 块数不涨。于是「幂等命中」与「全被拒」在块数上
// 完全一样，文档被永久扣在 L2 每小时重试。
//
// 生产实测（homed_2026-11-15_13-08-13.log）：259 条 `wrote no blocks` 里
// 77 条的「all N triples rejected」与实际 reject 行数差正好 1 ——
// 那一条写进去了，只是两个端点块早已存在。
//
// ⇒ 唯一可靠的信号是「真正走进 putTripleBlocksTx 的条数」。本判据钉住它。

import (
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

func newGraph(t *testing.T) *memory.GraphDB {
	t.Helper()
	g, err := memory.NewGraphDB(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// TestCommitReadyCountsWritesThatCreateNoNewBlocks 是本判据的核心：
// 重复提交同一事实时块数不涨，但写入条数必须照实报告。
//
// 变异验证：
//   - 让 CommitReady 返回 MemoryBlockCount 差值 ⇒ 第二次提交返回 0 ⇒ 本判据红
//   - 让 CommitReady 恒返回 len(triples)（不查校验）⇒ 被拒的三元组也算写
//     ⇒ TestCommitReadyExcludesRejectedTriples 红
func TestCommitReadyCountsWritesThatCreateNoNewBlocks(t *testing.T) {
	g := newGraph(t)

	tri := []memory.Triple{{
		Subject: "量子计算", Relation: "属于", Object: "前沿领域",
		SentenceText: "量子计算属于前沿领域",
	}}

	// 第一次：新建块，块数增长。
	before, err := g.MemoryBlockCount()
	if err != nil {
		t.Fatal(err)
	}
	_, written1, err := g.CommitReady(tri, "s1", 1)
	if err != nil {
		t.Fatalf("CommitReady: %v", err)
	}
	if written1 != 1 {
		t.Fatalf("首次提交 written = %d，期望 1", written1)
	}

	// 第二次：同一事实换会话。块与边都是幂等的 ⇒ 块数不变。
	_, written2, err := g.CommitReady(tri, "s2", 1)
	if err != nil {
		t.Fatalf("CommitReady: %v", err)
	}
	after, err := g.MemoryBlockCount()
	if err != nil {
		t.Fatal(err)
	}
	if after != before+3 {
		// 首次写入产生 3 块：主语块 + 宾语块 + 原句块。
		t.Logf("块数 before=%d after=%d（首次 +3，第二次不该再涨）", before, after)
	}
	if written2 != 1 {
		t.Fatalf("★ 第二次提交 written = %d，期望 1（写入发生了，只是块已存在）", written2)
	}

	// 再第三次，确认不是偶然。
	if _, written3, err := g.CommitReady(tri, "s3", 1); err != nil || written3 != 1 {
		t.Fatalf("第三次 written = %d err = %v，期望 1", written3, err)
	}
}

// TestCommitReadyExcludesRejectedTriples：被实体名校验拒掉的不算写入。
//
// 「写入条数」必须是**通过校验**的条数，否则它又退回成一个恒真信号。
func TestCommitReadyExcludesRejectedTriples(t *testing.T) {
	g := newGraph(t)

	triples := []memory.Triple{
		// 单字标点主语：validEntityName 必拒（长度 < 2 且无字母数字）。
		{Subject: "、", Relation: "是", Object: "模型", SentenceText: "、是模型"},
		// 合法：必须写进去。
		{Subject: "账单服务", Relation: "端口", Object: "8499", SentenceText: "账单服务端口是8499"},
	}

	_, written, err := g.CommitReady(triples, "s1", 1)
	if err != nil {
		t.Fatalf("CommitReady: %v", err)
	}
	if written != 1 {
		t.Fatalf("★ written = %d，期望 1（一条被拒、一条合法）", written)
	}

	// 全被拒：必须返回 0，这是「真的一条没写」的唯一形态。
	_, written, err = g.CommitReady([]memory.Triple{
		{Subject: "、", Relation: "是", Object: "模型"},
		{Subject: "（", Relation: "像", Object: "团"},
	}, "s2", 1)
	if err != nil {
		t.Fatalf("CommitReady: %v", err)
	}
	if written != 0 {
		t.Fatalf("★ 全被拒时 written = %d，期望 0", written)
	}
}

// TestFilterNoiseTriplesDropsPunctuation：纯标点/单字标点不得进入三元组。
//
// ## 为什么（2026-11-15 实测）
//
// 生产日志 7 小时内 588 条 reject，其中 82% 的单字主语是标点：
// 、（140 次）（（91）（（63），（56）「（49）：（28）。
// 全部由 fallback POS 解析器在「终端日志 + ADB 截图回执」这类散文上切出。
//
// 落库闸门 validEntityName 本来会拒，但那是 commit 阶段 —— 既刷日志，
// 又让「一条都没抽出来」与「抽出来全被拒」两种情形混在一起，
// 归档重试的判据因此分不清。收在抽取端才能干净地走「只迁块」路径。
//
// 变异验证：删掉 IsNoiseEntity 里的 hasEntityRune 判断 ⇒ 本判据红。
func TestFilterNoiseTriplesDropsPunctuation(t *testing.T) {
	in := []memory.Triple{
		{Subject: "、", Relation: "弯", Object: "眼"},
		{Subject: "（", Relation: "像", Object: "团"},
		{Subject: "，", Relation: "加载", Object: "界面"},
		{Subject: "「", Relation: "要", Object: "量"},
		{Subject: ":", Relation: "是", Object: "模型"}, // 半角冒号
		{Subject: "账单服务", Relation: "端口", Object: "8499"},
	}
	out := memory.FilterNoiseTriples(in)
	if len(out) != 1 {
		t.Fatalf("★ 过滤后剩 %d 条，期望 1（只有合法那条）: %+v", len(out), out)
	}
	if out[0].Subject != "账单服务" || out[0].Object != "8499" {
		t.Fatalf("★ 留下的不是合法那条: %+v", out[0])
	}
}
