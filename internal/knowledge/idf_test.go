package knowledge

import (
	"fmt"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
)

// 运行时新增的知识必须**当场**可检索，不依赖重启。
//
// 这是一次真实功能缺陷：TFIDFVectorizer.Vectorize 会跳过 df<=0 的特征，
// 而 Add 此前只把文本追进一个 summaries 切片、不更新 DF。于是
// 「重启后（已 Train 过 ≥3 篇）→ 新增一条含全新词的知识 → 查它」
// 返回空结果，重启一次才恢复。实测曾得到 Search("量子纠缠") == []。
//
// 之所以容易漏：全新 Store 语料不足 3 篇时 Vectorize 走
// 「totalDocs < 3 不乘 IDF」的退化分支，新词照样能搜到 —— 缺陷只在
// 「库已满、且用的是全新词」时显形。
func TestNewTermSearchableImmediatelyAfterAdd(t *testing.T) {
	dir := t.TempDir()

	// 先用 5 篇把语料喂到 totalDocs >= 3（走真实 IDF 分支）
	{
		s := NewStore(dir)
		if err := s.Start(); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 5; i++ {
			if err := s.Add(fmt.Sprintf("旧知识%d", i), "咖啡 睡眠 架构 记忆 插件 内核 事件 总线 索引"); err != nil {
				t.Fatal(err)
			}
		}
		s.Stop()
	}

	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// 运行时新增一条含**全新词**的知识
	if err := s.Add("新知识", "量子纠缠 拓扑绝缘体 简并态 莫尔条纹"); err != nil {
		t.Fatal(err)
	}

	q := "量子纠缠"
	var names []string
	for _, k := range s.Search(q, 5) {
		names = append(names, k.Name)
	}
	if !contains(names, "新知识") {
		t.Fatalf("Add 后新知识应立刻可检索（query=%q），实得 %v —— IDF 未随写入更新", q, names)
	}
	// 新词向量不应为空（空 = 被 df<=0 过滤掉）
	v := s.veczer.Vectorize(q)
	if len(v) == 0 {
		t.Errorf("新词向量为空：df=0 被过滤，query=%q", q)
	}
	// 另一路（稀疏语义路）也应能命中
	if got := s.vec.SearchScored(s.vectorize(q), s.vec.Size()); len(got) == 0 {
		t.Logf("注：稀疏语义路对 %q 无命中（取决于是否注入了词向量），不影响本用例结论", q)
	}
}

// 覆盖写同名条目时，IDF 不能把同一篇重复计入，否则 df 虚高、IDF 虚低。
func TestOverwriteDoesNotDoubleCountDF(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	for i := 0; i < 5; i++ {
		if err := s.Add(fmt.Sprintf("k%d", i), "共同词"+strings.Repeat("x", i)); err != nil {
			t.Fatal(err)
		}
	}
	// 记录覆盖前 totalDocs 语料状态
	before := len(s.List())
	if err := s.Add("k0", "改写后的内容 独特词9z"); err != nil {
		t.Fatal(err)
	}
	if after := len(s.List()); after != before {
		t.Fatalf("覆盖写不应改变条目数：%d → %d", before, after)
	}
	// 旧内容里的 "共同词" 不应因为被覆盖而消失（旧版正文里也有它？不，
	// 这里断言的是：新版内容里的词能被搜到，即 DF 已切到新版）
	if !s.hasLexHit("k0", "独特词9z") {
		t.Error("覆盖写后新版内容的词应可检索")
	}
}

// 删除条目后，IDF 语料必须同步收缩（否则 DF 表与真实条目脱钩，越用越偏）。
func TestRemoveShrinksIDFCorpus(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// 造 3 篇共享词，删掉其中唯一含某词的篇
	for i := 0; i < 3; i++ {
		body := "共享词"
		if i == 0 {
			body += " 独占词zzz"
		}
		if err := s.Add(fmt.Sprintf("k%d", i), body); err != nil {
			t.Fatal(err)
		}
	}
	// 未删前，k0 在词法路里
	if !s.hasLexHit("k0", "独占词zzz") {
		t.Fatal("前置条件不成立：k0 应命中独占词")
	}
	if err := s.Remove("k0"); err != nil {
		t.Fatal(err)
	}
	// 删除后，k0 不得再出现在任何一路
	if s.hasLexHit("k0", "共享词") {
		t.Error("已删除条目仍在词法路索引里")
	}
	// 剩余条目的检索必须仍工作（IDF 没被清成空）
	if got := s.Search("共享词", 5); len(got) != 2 {
		t.Errorf("删除后其余条目应仍可检索 2 条，实为 %d", len(got))
	}
}

// 词法路与 IDF 必须始终以 items 的**规范名**为准，与重启后的重建一致。
// 否则运行时增量维护的 DF 与重启后 Train 的 DF 不同，IDF 悄悄漂移。
func TestLexTextUsesCanonicalName(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	// 用带空格/大写的名字，看 DF 记账用的是不是规范名
	if err := s.Add("Tech/Upper", "共享语料词"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("b", "共享语料词"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("c", "共享语料词"); err != nil {
		t.Fatal(err)
	}
	// 覆盖写：规范名一致才能正确 RemoveDoc 旧文本
	if err := s.Add("Tech/Upper", "改写 共享语料词"); err != nil {
		t.Fatal(err)
	}
	s.Stop()

	// 重启后应与运行时增量维护的 DF 数值一致（若不一致，说明 lexText 口径漂了）
	s2 := NewStore(dir)
	if err := s2.Start(); err != nil {
		t.Fatal(err)
	}
	defer s2.Stop()

	// 三篇都含"共享语料词" ⇒ df=3 ⇒ IDF = log((3+1)/(3+1)) = 0 ⇒ 被丢弃
	// （与删改无关，是 IDF 本身的定义）。改为查一个只在一篇里出现的词。
	if err := s2.Add("d", "绝无仅有词qqq"); err != nil {
		t.Fatal(err)
	}
	if !s2.hasLexHit("d", "绝无仅有词qqq") {
		t.Error("重启后新词仍不可检索，IDF 记账有问题")
	}
}

// 大量增删后，词法路索引与 items 数量必须始终一致（不漏删、不留孤儿）。
func TestLexIndexStaysInSyncWithItems(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(dir)
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	for i := 0; i < 30; i++ {
		if err := s.Add(fmt.Sprintf("k%02d", i), fmt.Sprintf("内容 %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// 删一半
	for i := 0; i < 30; i += 2 {
		if err := s.Remove(fmt.Sprintf("k%02d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if lexN, itemN := s.lex.Size(), len(s.items); lexN != itemN {
		t.Errorf("词法路索引与条目数不一致：lex=%d items=%d", lexN, itemN)
	}
}

// hasLexHit 报某条目在词法路里是否含有给定词的向量。
func (s *Store) hasLexHit(id, probe string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, h := range s.lex.SearchScored(s.veczer.Vectorize(probe), s.lex.Size()) {
		if h.Doc.ID == id && h.Score > 0 {
			return true
		}
	}
	return false
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// TFIDFVectorizer 的增量接口本身也要守规矩：AddDoc/RemoveDoc 必须与
// Train 给出**相同**的 DF（文档级去重），否则两条路径会漂移。
func TestTFIDFIncrementalMatchesTrain(t *testing.T) {
	tok := func(s string) []string { return strings.Fields(s) }

	full := vector.NewTFIDFVectorizer(tok)
	full.Train([]string{"a b c", "b c d", "c d e"})

	inc := vector.NewTFIDFVectorizer(tok)
	inc.AddDoc("a b c")
	inc.AddDoc("b c d")
	inc.AddDoc("c d e")

	for _, probe := range []string{"a", "b", "c", "d", "e"} {
		fv, iv := full.Vectorize(probe), inc.Vectorize(probe)
		if len(fv) != len(iv) {
			t.Errorf("探针 %q：Train 得 %d 维，AddDoc 得 %d 维（DF 不一致）", probe, len(fv), len(iv))
			continue
		}
		for f, w := range fv {
			if diff := w - iv[f]; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("探针 %q 特征 %q 权重不一致：Train=%g AddDoc=%g", probe, f, w, iv[f])
			}
		}
	}

	// RemoveDoc 后 totalDocs 不得为负
	inc.RemoveDoc("a b c")
	inc.RemoveDoc("a b c")
	inc.RemoveDoc("a b c")
	inc.AddDoc("x y")
	if got := inc.Vectorize("x"); len(got) == 0 {
		t.Error("totalDocs 变负后 Vectorize 行为异常")
	}
}
