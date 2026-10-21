package distill

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/pkg/generation"
)

// 拆解产物的块落库。判据核心：三元组应是【句子】【contains 边】【块】，
// 而不是 entities/relations（本会话最初的错误做法）。

func newBlockGraph(t *testing.T) *memory.GraphDB {
	t.Helper()
	g, err := memory.NewGraphDB(t.TempDir() + "/graph.db")
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func recGraphTriples(rec string) *fakeGen {
	return &fakeGen{text: `{"fields":[
		{"name":"停机时长","value":"4分"},
		{"name":"回滚版本","value":"v2.29.5"},
		{"name":"灰度比例","value":"10%"}]}`}
}

const blockRec = "第112批周四凌晨2点·停机4分·回滚v2.29.5·灰度10%"

// ★ 落库形态：句子 + 块 + contains 边，**零 entity**。
func TestWritePayload_句子块边形态(t *testing.T) {
	g := newBlockGraph(t)
	e := NewExtractor(recGraphTriples(blockRec), nil)

	payload, err := e.Blocks(context.Background(), blockRec)
	if err != nil {
		t.Fatalf("Blocks: %v", err)
	}
	if payload.Sentence != blockRec {
		t.Errorf("原句应保留，实际 %q", payload.Sentence)
	}
	if len(payload.Fields) != 3 {
		t.Fatalf("期望 3 个字段，实际 %d: %+v", len(payload.Fields), payload.Fields)
	}

	n, err := WritePayload(context.Background(), g, payload, nil)
	if err != nil {
		t.Fatalf("WritePayload: %v", err)
	}
	if n != 3 {
		t.Fatalf("应写入 3 个块，实际 %d", n)
	}

	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	// ★ 原句块 + 3 个字段块 = 4（原为 3，方案 A 让原句也成了块）
	if len(blocks) != 4 {
		t.Fatalf("库中应有 4 个块（1 原句 + 3 字段），实际 %d", len(blocks))
	}
	edges, err := g.MemoryBlockEdges()
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 3 {
		t.Fatalf("应有 3 条 contains 边，实际 %d", len(edges))
	}
	for _, ed := range edges {
		if ed.SourceKind != "block" || ed.TargetKind != "block" || ed.Type != "contains" {
			t.Errorf("边形态应为 sentence--contains-->block，实际 %+v", ed)
		}
	}

	// ★ 关键：块路径**不写旧表 entities**。
	//
	// ★ 判据口径变了（2026-10-04）：原来断言「Recall 返回 0 个实体」。
	//   那是在 Recall 仍读旧表时的写法 —— Recall 切块之后，
	//   它必然返回块（那正是块路径该做的事），于是判据恒红。
	//
	//   判据的**意图**是「没有走旧表双写」，所以直接查旧表本身 ——
	//   与实现无关，且比间接断言更可靠。
	legacy, err := g.LegacyEntities(0, 1)
	if err != nil {
		t.Fatalf("LegacyEntities: %v", err)
	}
	if len(legacy) != 0 {
		t.Errorf("块路径不该写 entities（旧表 %d 个）: %+v", len(legacy), legacy)
	}
}

// 块文本承载「维度=值」的完整语义。
func TestWritePayload_块文本形态(t *testing.T) {
	g := newBlockGraph(t)
	e := NewExtractor(recGraphTriples(blockRec), nil)
	payload, _ := e.Blocks(context.Background(), blockRec)
	if _, err := WritePayload(context.Background(), g, payload, nil); err != nil {
		t.Fatal(err)
	}
	blocks, _ := g.MemoryBlocks()
	texts := map[string]bool{}
	for _, b := range blocks {
		texts[b.Text] = true
	}
	// ★ 块文本形态是「<主语>|<维度>=<值>」而不是「<维度>=<值>」。
	//
	// 本轮补的缺口：上一版 FieldBlock 没有 Subject，块文本里只有维度与值。
	// 单主语时就丢了「这条是关于谁的」（第112批），一句话多主语时更致命
	// （三个服务的端口会全变成「端口=8861」而无法区分）。所以这里跟着改成
	// 带主语的形态断言 —— 不是放宽，是把旧形态的漏洞写进判据。
	for _, want := range []string{
		"第112批|停机时长=4分",
		"第112批|回滚版本=v2.29.5",
		"第112批|灰度比例=10%",
	} {
		if !texts[want] {
			t.Errorf("缺少块文本 %q，实际 %v", want, texts)
		}
	}
}

// ★ 幂等：同一记录重复拆解得到同一批块 ID，不堆重复记忆。
//
// 蒸馏是可重试的（distillOnce 失败会把记录写回队列），随机 ID 会让
// 每次重试都产生新块，同一句记忆堆成 N 份。
func TestWritePayload_幂等(t *testing.T) {
	g := newBlockGraph(t)
	e := NewExtractor(recGraphTriples(blockRec), nil)

	for i := 0; i < 3; i++ {
		payload, _ := e.Blocks(context.Background(), blockRec)
		if _, err := WritePayload(context.Background(), g, payload, nil); err != nil {
			t.Fatalf("第 %d 次写入: %v", i, err)
		}
	}
	blocks, _ := g.MemoryBlocks()
	// ★ 同上：1 原句块 + 3 字段块，重复写不增
	if len(blocks) != 4 {
		t.Fatalf("重复写入 3 次后仍应只有 4 个块，实际 %d", len(blocks))
	}
}

// ★ 无 provider 时块仍入库但不带向量——**不编造零向量**。
//
// 编造零向量会让它参与向量检索并永远排在最后，那是静默的错误记忆。
func TestWritePayload_无provider不编造向量(t *testing.T) {
	g := newBlockGraph(t)
	e := NewExtractor(recGraphTriples(blockRec), nil)
	payload, _ := e.Blocks(context.Background(), blockRec)
	if _, err := WritePayload(context.Background(), g, payload, nil); err != nil {
		t.Fatal(err)
	}
	blocks, _ := g.MemoryBlocks()
	for _, b := range blocks {
		if len(b.Vector) != 0 {
			t.Errorf("无 provider 时不该有向量，%s 有 %d 维", b.ID, len(b.Vector))
		}
		if b.Fingerprint != "" {
			t.Errorf("无 provider 时不该有指纹，%s 有 %q", b.ID, b.Fingerprint)
		}
	}
	// 但它们仍可被按边查到
	sid := SentenceBlockID(blockRec)
	got, err := g.BlocksForNode("block", sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("无向量的块仍应按边可查，实际 %d", len(got))
	}
}

// 有 provider 时向量与指纹都落库。
func TestWritePayload_带向量落库(t *testing.T) {
	g := newBlockGraph(t)
	e := NewExtractor(recGraphTriples(blockRec), nil)
	payload, _ := e.Blocks(context.Background(), blockRec)
	embed := func(string) ([]float64, string) {
		return []float64{1, 0, 0}, "fp-test"
	}
	if _, err := WritePayload(context.Background(), g, payload, embed); err != nil {
		t.Fatal(err)
	}
	blocks, _ := g.MemoryBlocks()
	var withVec, sentenceBlocks int
	for _, b := range blocks {
		if b.Source == "sentence" {
			// ★ 原句块**刻意不带向量**
			//
			// 它是溯源锚点，不参与向量召回。理由：
			// 长整句的句向量会把关键值稀释掉 ——
			// 实测「13000端口」(7字) 容易被召回，
			// 而「13010/13011 而非 12011」(20字) 召不回。
			// 让原句进召回只会引入噪声。
			sentenceBlocks++
			if len(b.Vector) != 0 {
				t.Errorf("原句块 %s 不该带向量，实际 %d 维", b.ID, len(b.Vector))
			}
			continue
		}
		withVec++
		if len(b.Vector) != 3 || b.Fingerprint != "fp-test" {
			t.Errorf("%s 应带 3 维向量与 fp-test，实际 %d 维 %q",
				b.ID, len(b.Vector), b.Fingerprint)
		}
	}
	if sentenceBlocks != 1 {
		t.Errorf("应恰好 1 个原句块，实际 %d", sentenceBlocks)
	}
	if withVec == 0 {
		t.Error("字段块都该带向量")
	}
}

// 空字段是正常结果：0 块、不报错、不写句子。
func TestWritePayload_空字段(t *testing.T) {
	g := newBlockGraph(t)
	e := NewExtractor(&fakeGen{text: `{"fields":[]}`}, nil)
	payload, err := e.Blocks(context.Background(), "第129~143批均仅评审通过")
	if err != nil {
		t.Fatal(err)
	}
	n, err := WritePayload(context.Background(), g, payload, nil)
	if err != nil {
		t.Fatalf("空字段不该报错: %v", err)
	}
	if n != 0 {
		t.Fatalf("应写入 0 块，实际 %d", n)
	}
	blocks, _ := g.MemoryBlocks()
	if len(blocks) != 0 {
		t.Fatalf("空字段不该产生块，实际 %d", len(blocks))
	}
}

// 空 payload / nil graph 必须显式报错，不能静默成功。
func TestWritePayload_非法输入报错(t *testing.T) {
	g := newBlockGraph(t)
	if _, err := WritePayload(context.Background(), g, nil, nil); err == nil {
		t.Error("nil payload 应报错")
	}
	if _, err := WritePayload(context.Background(), nil,
		&BlockPayload{Sentence: "x", Fields: []FieldBlock{{Dimension: "d", Value: "v"}}}, nil); err == nil {
		t.Error("nil graph 应报错")
	}
	if _, err := WritePayload(context.Background(), g, &BlockPayload{}, nil); err == nil {
		t.Error("空句子应报错")
	}
}

// 模型失败时透传错误（调用方据此重试）。
func TestBlocks_模型失败透传(t *testing.T) {
	g := newBlockGraph(t)
	e := NewExtractor(&fakeGen{err: errors.New("model down")}, nil)
	if _, err := e.Blocks(context.Background(), blockRec); err == nil {
		t.Fatal("应透传模型错误")
	}
	_ = g
}

// BlockID 内容派生且稳定。
func TestBlockID_稳定且区分(t *testing.T) {
	a := BlockID("s", "主语", "d", "v")
	b := BlockID("s", "主语", "d", "v")
	if a != b {
		t.Error("同输入应得同 ID（幂等的前提）")
	}
	if !strings.HasPrefix(a, "blk_") {
		t.Errorf("ID 应有 blk_ 前缀，实际 %q", a)
	}
	// ★ 三个输入分量都必须参与派生。
	//
	// 变异自证抓出来的漏洞：上一版只测了「不同 value」，于是把 dimension
	// 从派生里去掉后测试仍然通过——而那会让「停机时长」与「回滚版本」在
	// 同一句话里撞成同一个块 ID，后者覆盖前者，**静默丢失一条记忆**。
	distinct := map[string]string{
		"同句同值不同维度": BlockID("s", "主语", "d2", "v"),
		"同句同维度不同值": BlockID("s", "主语", "d", "v2"),
		"同维度同值不同句": BlockID("s2", "主语", "d", "v"),
		// ★ 主语必须参与派生：三个服务的端口同维度同值时，
		// 不含 subject 就会撞 ID —— 静默丢一条记忆（无任何报错）。
		"同维度同值不同主语": BlockID("s", "主语2", "d", "v"),
	}
	for name, id := range distinct {
		if id == a {
			t.Errorf("%s 应与基准 ID 不同（该分量未参与派生）: %q", name, id)
		}
	}
	// 两两之间也要不同
	seen := map[string]string{}
	for name, id := range distinct {
		if prev, dup := seen[id]; dup {
			t.Errorf("%s 与 %s 撞 ID: %q", name, prev, id)
		}
		seen[id] = name
	}
}

// ★ 同一句话里不同维度必须得到不同块 ID（上面的漏洞会造成静默覆盖）。
func TestWritePayload_同句不同维度不撞块(t *testing.T) {
	g := newBlockGraph(t)
	e := NewExtractor(&fakeGen{text: `{"fields":[
		{"name":"停机时长","value":"4分"},
		{"name":"回滚版本","value":"4分"}]}`}, nil) // 值相同、维度不同
	payload, err := e.Blocks(context.Background(), "第112批 停机4分 回滚4分")
	if err != nil {
		t.Fatal(err)
	}
	n, err := WritePayload(context.Background(), g, payload, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocks, _ := g.MemoryBlocks()
	// ★ n 是**字段块**数（WritePayload 的返回值语义），不含原句块。
	//   总块数 = n 字段 + 1 原句。
	if len(blocks) != n+1 || len(blocks) != 3 {
		t.Fatalf("值相同但维度不同应产生 3 个块（1 原句 + 2 字段），实际 %d（块 ID 撞了会少）", len(blocks))
	}
}

// Dimension/Value 从块文本取回结构化字段。
func TestDimensionValue(t *testing.T) {
	for _, c := range []struct{ text, dim, val string }{
		{"停机时长=4分", "停机时长", "4分"},
		{"回滚版本=v2.29.5", "回滚版本", "v2.29.5"},
		{"无等号", "", "无等号"},
	} {
		if got := Dimension(c.text); got != c.dim {
			t.Errorf("Dimension(%q)=%q，期望 %q", c.text, got, c.dim)
		}
		if got := Value(c.text); got != c.val {
			t.Errorf("Value(%q)=%q，期望 %q", c.text, got, c.val)
		}
	}
}

func itoa64(i int64) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

var _ = generation.ErrSchemaUnsupported

// ★ 主语必须进块文本（不只是进 ID）。
//
// 这是本轮修的真缺口：一句多主语时
// 「admin服务端口8861·billing服务端口8499·oauth服务端口8271」
// 拆出三条同维度不同值的字段，少了主语就只剩「端口=8861」，
// 三个服务的端口落库后无法区分 —— 查「billing 的端口」时块文本里
// 根本没有 billing，向量也召回不到。
func TestFieldBlock_主语进块文本(t *testing.T) {
	f := FieldBlock{Subject: "billing服务", Dimension: "端口", Value: "8499"}
	b := f.ToMemoryBlock("admin服务端口8861·billing服务端口8499·oauth服务端口8271")

	if b.Text == "端口=8499" {
		t.Errorf("块文本必须含主语，实际 %q", b.Text)
	}
	if got := BlockSubject(b.Text); got != "billing服务" {
		t.Errorf("BlockSubject(%q) = %q，期望 billing服务", b.Text, got)
	}
	if got := Dimension(b.Text); got != "端口" {
		t.Errorf("Dimension(%q) = %q，期望 端口（不该混进主语）", b.Text, got)
	}
	if got := Value(b.Text); got != "8499" {
		t.Errorf("Value(%q) = %q，期望 8499", b.Text, got)
	}
	// 往返：文本 → 三段全还原
	if BlockSubject(b.Text)+"|"+Dimension(b.Text)+"="+Value(b.Text) != b.Text {
		t.Errorf("往返不一致：%q", b.Text)
	}
}

// 三个服务的端口必须是三个不同的块（ID 不撞、文本可区分）。
func TestFieldBlock_多主语不撞ID(t *testing.T) {
	sent := "admin服务端口8861·billing服务端口8499·oauth服务端口8271"
	fields := []FieldBlock{
		{Subject: "admin服务", Dimension: "端口", Value: "8861"},
		{Subject: "billing服务", Dimension: "端口", Value: "8499"},
		{Subject: "oauth服务", Dimension: "端口", Value: "8271"},
	}
	ids := map[string]string{}
	for _, f := range fields {
		b := f.ToMemoryBlock(sent)
		if prev, dup := ids[b.ID]; dup {
			t.Errorf("块 ID 撞了：%s 与 %s 同为 %s", prev, b.Text, b.ID)
		}
		ids[b.ID] = b.Text
	}
	if len(ids) != 3 {
		t.Errorf("三个服务的端口应得 3 个块，实际 %d", len(ids))
	}
}

// 无主语时（不该出现，但 BlockPayload 可能来自别的路径）文本不带 '|'，
// 切分函数仍要正确工作而不是崩。
func TestFieldBlock_无主语时切分(t *testing.T) {
	f := FieldBlock{Dimension: "停机时长", Value: "4分"}
	b := f.ToMemoryBlock("第112批周四凌晨2点·停机4分")
	if b.Text != "停机时长=4分" {
		t.Errorf("无主语时文本不该带分隔符，实际 %q", b.Text)
	}
	if got := BlockSubject(b.Text); got != "" {
		t.Errorf("无主语时 BlockSubject 应为空，实际 %q", got)
	}
	if got := Dimension(b.Text); got != "停机时长" {
		t.Errorf("Dimension = %q", got)
	}
	if got := Value(b.Text); got != "4分" {
		t.Errorf("Value = %q", got)
	}
}

// Blocks() 必须把 Split 算出的主语带到 FieldBlock 里。
//
// ★ 这条直接盯着本轮修的缺口：上一版 FieldBlock 没有 Subject 字段，
// 于是多主语在落库时被丢弃 —— 编译通过、单测全绿，但数据是错的。
func TestBlocks_主语透传到字段块(t *testing.T) {
	e := NewExtractor(&fakeGen{text: `{"fields":[{"name":"端口","value":"8861"},{"name":"端口","value":"8499"}]}`}, nil)
	payload, err := e.Blocks(context.Background(),
		"admin服务端口8861·billing服务端口8499")
	if err != nil {
		t.Fatal(err)
	}
	if len(payload.Fields) != 2 {
		t.Fatalf("应拆出 2 个字段，实际 %d", len(payload.Fields))
	}
	if payload.Fields[0].Subject != "admin服务" {
		t.Errorf("字段0 主语应为 admin服务，实际 %q", payload.Fields[0].Subject)
	}
	if payload.Fields[1].Subject != "billing服务" {
		t.Errorf("字段1 主语应为 billing服务，实际 %q", payload.Fields[1].Subject)
	}
}

// ★★ 方案 A 判据：蒸馏落块**不得再写 sentences 表**。
//
// 这是退场的最后依赖点 —— distill 曾是 sentences 表的写入者之一。
func TestWritePayload_不写sentences表(t *testing.T) {
	g := newBlockGraph(t)
	defer g.Close()

	pl := &BlockPayload{
		Sentence: "值班室分机号 4324，值班 老周",
		Fields: []FieldBlock{
			{Subject: "值班室", Dimension: "分机号", Value: "4324"},
			{Subject: "值班", Dimension: "人员", Value: "老周"},
		},
	}
	n, err := WritePayload(context.Background(), g, pl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应写 2 个字段块，实际 %d", n)
	}

	// ★ sentences 表必须仍是空的
	sentCount, err := g.CountSentences()
	if err != nil {
		t.Fatal(err)
	}
	if sentCount != 0 {
		t.Errorf("★ distill 写了 %d 条 sentences —— 方案 A 要求退场该表", sentCount)
	}

	// 字段块与原句块都要在
	blocks, err2 := g.MemoryBlocks()
	err = err2
	if err != nil {
		t.Fatal(err)
	}
	var hasSentence, hasField int
	for _, b := range blocks {
		if b.Source == "sentence" {
			hasSentence++
		}
		if b.Source == "distill" {
			hasField++
		}
	}
	if hasSentence != 1 {
		t.Errorf("应恰好 1 个原句块，实际 %d", hasSentence)
	}
	if hasField != 2 {
		t.Errorf("应恰好 2 个字段块，实际 %d", hasField)
	}
}

// ★ 边必须是 block→block（不再是 sentence→block）。
func TestWritePayload_边是块到块(t *testing.T) {
	g := newBlockGraph(t)
	defer g.Close()

	pl := &BlockPayload{
		Sentence: "连接池容量 128/256 扩容后",
		Fields:   []FieldBlock{{Subject: "连接池", Dimension: "容量", Value: "128/256"}},
	}
	if _, err := WritePayload(context.Background(), g, pl, nil); err != nil {
		t.Fatal(err)
	}
	edges, err := g.MemoryBlockEdges()
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range edges {
		if e.Type == "contains" {
			found++
			if e.SourceKind != "block" {
				t.Errorf("contains 边起点应是 block，实际 %q", e.SourceKind)
			}
			if e.TargetKind != "block" {
				t.Errorf("contains 边终点应是 block，实际 %q", e.TargetKind)
			}
		}
	}
	if found != 1 {
		t.Errorf("应恰好 1 条 contains 边，实际 %d", found)
	}
}

// ★ 幂等：同一原句重复跑不产生重复块，也不产生重复边。
//
//	—— 记忆里明确记着「重试不能产生重复记忆」。
func TestWritePayload_重复跑幂等(t *testing.T) {
	g := newBlockGraph(t)
	defer g.Close()

	pl := &BlockPayload{
		Sentence: "第130批 v2.31.5 评审通过",
		Fields:   []FieldBlock{{Subject: "第130批", Dimension: "版本", Value: "v2.31.5"}},
	}
	for i := 0; i < 3; i++ {
		if _, err := WritePayload(context.Background(), g, pl, nil); err != nil {
			t.Fatalf("第 %d 次失败: %v", i+1, err)
		}
	}
	blocks, err := g.MemoryBlocks()
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 {
		ids := make([]string, 0, len(blocks))
		for _, b := range blocks {
			ids = append(ids, b.ID)
		}
		t.Errorf("重复跑 3 次后应仍是 2 个块（原句+字段），实际 %d: %v", len(blocks), ids)
	}
	edges, err := g.MemoryBlockEdges()
	if err != nil {
		t.Fatal(err)
	}
	var contains int
	for _, e := range edges {
		if e.Type == "contains" {
			contains++
		}
	}
	if contains != 1 {
		t.Errorf("重复跑 3 次后应仍是 1 条 contains 边，实际 %d", contains)
	}
}

// ★ 变异自证：把原句块 ID 改成非确定性，幂等判据必须变红。
func TestSentenceBlockID_确定性(t *testing.T) {
	s := "值班室分机号 4324，值班 老周"
	a := SentenceBlockID(s)
	b := SentenceBlockID(s)
	if a != b {
		t.Fatalf("同原句必须得到同 ID：%s vs %s", a, b)
	}
	// ★ 首尾空格**应当**得到同 ID —— TrimSpace 的目的正是如此：
	//   原句带的换行/空格不该让它与另一个库里的同一句话分成两块。
	if a != SentenceBlockID(s+"  \n") {
		t.Error("首尾空白差异不该产生不同 ID（那是同一句话）")
	}
	if a == SentenceBlockID("别的句子") {
		t.Error("不同原句必须得到不同 ID")
	}
	if len(a) < 8 || a[:8] != "blk_src_" {
		t.Errorf("ID 前缀应为 blk_src_，实际 %q", a)
	}
}
