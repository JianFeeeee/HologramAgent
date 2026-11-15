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
	"strings"
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

// TestFilterNoiseTriplesDropsSingleRuneEntities：单字汉字实体必须在**抽取端**被挡。
//
// ## 为什么（2026-11-15 实测）
//
// 22:11 那轮归档的 19 条 reject 全部是「某一端只有 1 个字符」：
// 俺 / 蒋 / 灯 / 词 / 话 / 钱 —— fallback POS 在「你有一条未读会话」这类
// 句子上切出来的。它们通过 IsNoiseEntity（单字汉字不是停用词也不是标点），
// 于是走到 commit 才被 validEntityName 的 `len < 2` 拒掉。
//
// 代价不止「少一条记忆」：
//
//	· 每条拒刷一行错误日志（19 条/轮纯噪声）
//	· 「抽出来但全被拒」与「一条都没抽出来」在日志里长得一样，
//	  归档判据分不清该重试还是该只迁块
//
// 变异验证：把 FilterNoiseTriples 里的 IsValidEntityName 判断删掉 ⇒ 红。
func TestFilterNoiseTriplesDropsSingleRuneEntities(t *testing.T) {
	in := []memory.Triple{
		{Subject: "俺", Relation: "是", Object: "看手相"},
		{Subject: "蒋", Relation: "姓", Object: "历史名人"},
		{Subject: "灯", Relation: "开关", Object: "计数"},
		{Subject: "未读", Relation: "会", Object: "话"},  // object 单字
		{Subject: "这种", Relation: "不用", Object: "钱"}, // object 单字
		// 合法：两端都 ≥2 字符
		{Subject: "账单服务", Relation: "端口", Object: "8499"},
	}
	out := memory.FilterNoiseTriples(in)
	if len(out) != 1 {
		t.Fatalf("★ 过滤后剩 %d 条，期望 1（只有合法那条）: %+v", len(out), out)
	}
	if out[0].Subject != "账单服务" {
		t.Fatalf("★ 留下的不是合法那条: %+v", out[0])
	}
}

// TestFilterNoiseTriplesDropsOverlongEntities：超长实体同样在抽取端挡。
//
// 与上一條同源：`文档 --主题--> <51~79 字的 summary>` 此前会稳定产出
// 一条必然被拒的三元组（validEntityName 上限 50）。
func TestFilterNoiseTriplesDropsOverlongEntities(t *testing.T) {
	long := strings.Repeat("排查漏洞", 15) // 60 rune
	out := memory.FilterNoiseTriples([]memory.Triple{{
		Subject: "文档", Relation: "主题", Object: long,
	}})
	if len(out) != 0 {
		t.Fatalf("★ 超长 object 未被挡下: %+v", out)
	}
}

// TestPurgeNoiseScopeUnchangedByFilterGate 钉住一个**反向**风险：
// 抽取端的收紧不得扩大历史清理（PurgeNoise）的删除范围。
//
// ## 为什么必须有这条（2026-11-15 一次真实走错）
//
// 第一版我把「长度 ≥2」塞进了 IsNoiseEntity。它有两个消费者，语义相反：
//   - FilterNoiseTriples（抽取端预防）—— 收紧是想要的
//   - PurgeNoise（历史清理，按文本反查块 ID 并**删除**）—— 收紧 = 静默扩大删除
//
// 实测生产库有 5 个单字符文本块，其中 "2" 是纯数字 ——
// validEntityName 明确允许纯数字（端口/分机号是合法实体值）。
// 塞进 IsNoiseEntity 就会让 PurgeNoise 把它们当噪音删掉。
//
// 本判据断言：单字/纯数字块**不是** IsNoiseEntity 认定的噪音。
//
// 变异验证：把长度闸移回 IsNoiseEntity ⇒ 本判据红。
func TestPurgeNoiseScopeUnchangedByFilterGate(t *testing.T) {
	for _, name := range []string{"2", "俺", "话", "8499"} {
		if memory.IsNoiseEntity(name) {
			t.Errorf("★ %q 被 IsNoiseEntity 判为噪音 —— PurgeNoise 会把它（及它的块）删掉", name)
		}
	}
	// 真正的噪音仍须被认定（防止把闸门挪走后失效）。
	//
	// 注意：标点（、，）**从来**不在 IsNoiseEntity 的判定范围内 ——
	// 它的口径是「像不像噪音**词**」（停用词/归档标记/模板摘要），
	// 纯标点是「不像名字」，由 validEntityName / IsValidEntityName 管。
	// 所以这里只断言词层面的噪音。
	for _, name := range []string{"", "什么", "的", "那个", "结果"} {
		if !memory.IsNoiseEntity(name) {
			t.Errorf("★ 真噪音 %q 未被 IsNoiseEntity 识别", name)
		}
	}
}
