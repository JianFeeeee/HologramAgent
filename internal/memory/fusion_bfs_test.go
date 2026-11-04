package memory

import (
	"fmt"
	"testing"
)

// ═══════════════════════════════════════════════════════════════
//  BFS 接入召回链 —— 判据
//
//  ★ 真实缺口（生产探针 coexist 维度 0/1）
//
//  查询「本机服务监听哪些端口」召不回 13010。
//  原因：库里含「端口」二字的块是 14010端口 / 14011端口 那批，
//  而真正的本机端口块文本是「http://127.0.0.1:13010」——
//  **不含「端口」二字**。
//
//  ⇒ 纯向量/符号召回召不回它。
//  ⇒ 而图上是连着的：billing服务 --监听--> 13010
//     只要从「监听」或端口号出发 BFS，就能找回服务名。
//
//  ★ 所以这一格测的不是「关键词召回能不能更强」，
//  而是「召回**是否即联想**」—— 孤立命中之外，
//  还要沿边找回上下文。
// ═══════════════════════════════════════════════════════════════

func seedCoexistGraph(t *testing.T) *GraphDB {
	t.Helper()
	g := newTestGraph(t)
	// 织一张真实形态的网：服务 --监听--> 端口号
	blocks := []MemoryBlock{
		{ID: "b_svc", Modality: BlockText, Text: "billing服务", Vector: []float64{1, 0}, Fingerprint: "fp1"},
		{ID: "b_port", Modality: BlockText, Text: "13010", Vector: []float64{0.9, 0.1}, Fingerprint: "fp1"},
		{ID: "b_noise1", Modality: BlockText, Text: "14010端口", Vector: []float64{0, 1}, Fingerprint: "fp1"},
		{ID: "b_noise2", Modality: BlockText, Text: "14011端口", Vector: []float64{0, 1}, Fingerprint: "fp1"},
		{ID: "b_noise3", Modality: BlockText, Text: "CPU总线与MAR/MDR/ALU连接问题", Vector: []float64{0, 1}, Fingerprint: "fp1"},
	}
	for i := range blocks {
		if err := g.PutMemoryBlocks([]MemoryBlock{blocks[i]}); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []struct{ from, to, typ string }{
		{"b_svc", "b_port", "监听"},
		{"b_noise1", "b_port", "监听"},
		{"b_noise3", "b_svc", "涉及"},
	} {
		if err := g.AddRelationBlockEdge(e.from, e.to, e.typ,
			RelationEdgeData{SessionID: "s1", Confidence: 0.9, Status: EdgeActive}); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// TestBFS召回_从服务找回端口：图上连着的就该召回
func TestBFS召回_从服务找回端口(t *testing.T) {
	g := seedCoexistGraph(t)
	defer func() { _ = g.Close() }()

	// 查询直接命中「billing服务」——一个**孤立的语义节点**，
	// 但图上它连着 13010。
	// 带上查询向量：RecallBlocks 要求非空向量（符号路也依赖融合入口）
	q := BlockRecallQuery{Vector: []float64{1, 0}, TopK: 5, MinScore: -1}
	hits, _, err := g.RecallBlocksFusedBFS(q, "billing服务", 2)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  命中 %d 个: ", len(hits))
	for _, h := range hits {
		fmt.Printf("%q ", h.Text)
	}
	fmt.Println()

	if !containsText(hits, "13010") {
		t.Errorf("★ 召回应沿边找回端口号 13010（召回即联想），实际 %v", textsOfFused(hits))
	}
}

// TestBFS召回_顺序稳定：map 遍历会让输出顺序随机
func TestBFS召回_顺序稳定(t *testing.T) {
	g := seedCoexistGraph(t)
	defer func() { _ = g.Close() }()

	// 带上查询向量：RecallBlocks 要求非空向量（符号路也依赖融合入口）
	q := BlockRecallQuery{Vector: []float64{1, 0}, TopK: 5, MinScore: -1}
	var first []string
	for run := 0; run < 5; run++ {
		hits, _, err := g.RecallBlocksFusedBFS(q, "billing服务", 2)
		if err != nil {
			t.Fatal(err)
		}
		got := textsOfFused(hits)
		if run == 0 {
			first = got
			continue
		}
		if len(got) != len(first) {
			t.Fatalf("第 %d 次长度不同: %v vs %v", run, got, first)
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("★ 第 %d 次顺序不同: %v vs %v（map 遍历导致输出不稳定）", run, got, first)
			}
		}
	}
	fmt.Printf("  5 次顺序一致: %v\n", first)
}

// TestBFS召回_预算有界：不能因为加 BFS 就无限膨胀
func TestBFS召回_预算有界(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	// 织一个 200 节点的链：加 BFS 若无预算，depth=3 会拉进一大片
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("n%03d", i)
		if err := g.PutMemoryBlocks([]MemoryBlock{
			{ID: id, Modality: BlockText, Text: fmt.Sprintf("节点%03d", i),
				Vector: []float64{1, 0}, Fingerprint: "fp1"},
		}); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			if err := g.AddRelationBlockEdge(fmt.Sprintf("n%03d", i-1), id, "next",
				RelationEdgeData{SessionID: "s", Status: EdgeActive}); err != nil {
				t.Fatal(err)
			}
		}
	}

	// 带上查询向量：RecallBlocks 要求非空向量（符号路也依赖融合入口）
	q := BlockRecallQuery{Vector: []float64{1, 0}, TopK: 5, MinScore: -1}
	hits, _, err := g.RecallBlocksFusedBFS(q, "节点000", 3)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("  200 节点链，depth=3 → 召回 %d 个\n", len(hits))
	if len(hits) > 60 {
		t.Errorf("★ BFS 应受预算约束（200 节点链 depth=3 召回 %d 个，过多）", len(hits))
	}
}

func containsText(hits []FusedHit, s string) bool {
	for _, h := range hits {
		if h.Text == s {
			return true
		}
	}
	return false
}

func textsOfFused(hits []FusedHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Text)
	}
	return out
}

// ═══════════════════════════════════════════════════════════════
//  中文字符切分 —— 判据（2026-10-04）
//
//  ★ 这是生产探针 coexist 0/1 的**真正根因**
//
//  滑窗「每位置只取最长窗口」把目标词吃掉了：
//
//	「本机服务监听哪些端口」
//	  滑窗 → ["本机服务", "监听哪些"]   全是跨词伪词
//	  jieba → [本机 服务 监听 哪些 端口] 「端口」独立成词
//
// ⇒ 「端口」从未单独成窗 ⇒ 符号路对「14010端口」全打 0 分
//   ⇒ 直接命中全是噪音 ⇒ 图联想也无从谈起
// ═══════════════════════════════════════════════════════════════

// TestQuerySymbols_目标词必须独立成词
func TestQuerySymbols_目标词必须独立成词(t *testing.T) {
	for _, tc := range []struct {
		query   string
		want    []string // 必须出现的词
		notWant []string // 跨词伪词，出现即失败
	}{
		{
			query:   "本机服务监听哪些端口",
			want:    []string{"端口", "服务", "监听"},
			notWant: []string{"听哪些", "些端口", "务监听哪"},
		},
		{
			query:   "grafana 监控面板的端口是多少",
			want:    []string{"端口", "面板", "grafana"},
			notWant: []string{"控面板的", "板的端口"},
		},
		{
			query: "本机 13010 端口对应什么服务",
			want:  []string{"13010", "端口", "服务"},
		},
	} {
		syms := QuerySymbols(tc.query)
		set := make(map[string]bool, len(syms))
		for _, s := range syms {
			set[s] = true
		}
		fmt.Printf("  %q → %v\n", tc.query, syms)
		for _, w := range tc.want {
			if !set[w] {
				t.Errorf("★ %q 的符号应含 %q（目标词被切分吃掉），实际 %v", tc.query, w, syms)
			}
		}
		for _, w := range tc.notWant {
			if set[w] {
				t.Errorf("★ %q 的符号不该含跨词伪词 %q，实际 %v", tc.query, w, syms)
			}
		}
	}
}

// TestQuerySymbols_数字与英文精确串不受分词影响
func TestQuerySymbols_数字与英文精确串(t *testing.T) {
	syms := QuerySymbols("本机 13010 端口对应 grafana 9.99.99 吗")
	set := map[string]bool{}
	for _, s := range syms {
		set[s] = true
	}
	for _, want := range []string{"13010", "grafana", "9.99.99"} {
		if !set[want] {
			t.Errorf("★ 精确串 %q 必须原样保留，实际 %v", want, syms)
		}
	}
}

// TestSymbolScore_目标词能匹配到对应块：端到端的一环
func TestSymbolScore_目标词能匹配到对应块(t *testing.T) {
	syms := QuerySymbols("本机服务监听哪些端口")
	for _, tc := range []struct {
		text        string
		shouldMatch bool
	}{
		{"14010端口", true},
		{"端口", true},
		{"13010/13011 而非 12011", false}, // 不含目标词
	} {
		_, matched, _ := SymbolScore(syms, tc.text)
		got := len(matched) > 0
		if got != tc.shouldMatch {
			t.Errorf("★ %q 是否匹配 %q：期望 %v，实际 %v（matched=%v）",
				tc.text, "端口", tc.shouldMatch, got, matched)
		}
	}
}

// TestBFS召回_层深真实生效 钉住 depth 参数真的有语义。
//
// ## 守的是什么（2026-11-04 修的实测缺陷）
//
// 原实现：
//
//	for d := 1; d <= depth; d++ {
//	    neighbors, _ := g.NeighbourBlocks(seed.BlockID)   // ← 永远是 seed
//	    ...
//	    break                                             // ← 只跑一轮
//	}
//
// 三个问题叠在一起：
//
//	① 循环体末尾无条件 break ⇒ d 循环只执行一次
//	② NeighbourBlocks 每层传的还是 seed.BlockID ⇒ 第 2 层取的是同一批邻居
//	③ 于是 depth 参数**形同虚设**：传 2 和传 3 召回结果完全一样
//
// staticcheck 的 SA4008（"variable in loop condition never changes"）
// 正是从这里报出来的 —— 它看出来 `d` 除了打分没有参与寻址。
//
// ## 判据为什么必须是"链"而不是"星型"
//
// 星型（一个中心连 N 个叶子）在 depth=1 就能召回全部叶子，第 2 层没有新节点
// ⇒ 断不出"有没有真的走深"。必须是链：n0-n1-n2-n3，depth=d 恰好召回 d+1 个。
func TestBFS召回_层深真实生效(t *testing.T) {
	g := newTestGraph(t)
	defer func() { _ = g.Close() }()

	// 造一条 4 节点链：n0 → n1 → n2 → n3
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("n%d", i)
		if err := g.PutMemoryBlocks([]MemoryBlock{{
			ID: id, Modality: BlockText, Text: fmt.Sprintf("链节点%d", i),
			Vector: []float64{1, 0}, Fingerprint: "fp1",
		}}); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			if err := g.AddRelationBlockEdge(fmt.Sprintf("n%d", i-1), id, "next",
				RelationEdgeData{SessionID: "s", Status: EdgeActive}); err != nil {
				t.Fatal(err)
			}
		}
	}

	q := BlockRecallQuery{Vector: []float64{1, 0}, TopK: 1, MinScore: -1}
	got := map[int]int{}
	for _, d := range []int{1, 2, 3} {
		hits, _, err := g.RecallBlocksFusedBFS(q, "链节点0", d)
		if err != nil {
			t.Fatalf("depth=%d: %v", d, err)
		}
		got[d] = len(hits)
	}

	// depth=d 应召回命中(1) + d 层邻居 = d+1 个。
	for _, d := range []int{1, 2, 3} {
		if got[d] != d+1 {
			t.Fatalf("★ depth=%d 召回 %d 个，期望 %d ——\n"+
				"说明 BFS 没有真的逐层推进（depth 参数形同虚设）。\n"+
				"实测各层: %v", d, got[d], d+1, got)
		}
	}
	// 交叉验证：不同 depth 必须给出**不同**结果，否则参数无意义。
	if got[1] == got[3] {
		t.Fatalf("★ depth=1 与 depth=3 召回数相同（%d）—— depth 没生效", got[1])
	}
}
