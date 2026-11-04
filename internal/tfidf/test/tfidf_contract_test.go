package test

// tfidf 的契约判据：输入文本 → 输出向量/检索结果。
//
// ## 为什么这个包值得有判据（2026-11-04）
//
// `internal/tfidf` 306 行、**零测试文件**。它是纯算法库 —— 输入输出就是
// 全部语义，天然适合黑箱判据。
//
// 它服务的是记忆检索的 fallback 路径：没有神经网络 embedder 时，文本相似度
// 完全靠它。判据错 = 检索错，而检索错是静默的（不报错，只是答非所问）。
//
// ## 判据只断言契约，不断言数值
//
// TF-IDF 的具体权重取决于语料统计，写死数值等于把训练集也当成契约。
// 这里只钉那些**换了实现也必须成立**的性质：确定性、判别力、维度、
// 增删生效。

import (
	"math"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/tfidf"
)

// simpleTokenizer 按空白切词。
func simpleTokenizer(text string) []string {
	return strings.Fields(text)
}

func newTrained(t *testing.T, docs []string, dim int) *tfidf.Embedder {
	t.Helper()
	e := tfidf.NewEmbedder(simpleTokenizer, dim)
	e.Train(docs)
	return e
}

// TestVectorizeIsDeterministic 钉住确定性：同输入必须同输出。
//
// 变异：向量累加改成遍历 map 直接相加（不排序）⇒ 可能偶发红。
func TestVectorizeIsDeterministic(t *testing.T) {
	e := newTrained(t, []string{
		"billing service handles payments",
		"auth service handles login",
		"payment gateway retries",
	}, 64)

	const text = "billing service payments"
	a, err := e.VectorizeDense(text)
	if err != nil {
		t.Fatalf("VectorizeDense: %v", err)
	}
	for i := 0; i < 5; i++ {
		b, err := e.VectorizeDense(text)
		if err != nil {
			t.Fatal(err)
		}
		if len(a) != len(b) {
			t.Fatalf("维度不稳定：%d vs %d", len(a), len(b))
		}
		for j := range a {
			if a[j] != b[j] {
				t.Fatalf("★ 第 %d 次调用第 %d 维不同：%v vs %v —— 无确定性", i, j, a[j], b[j])
			}
		}
	}
}

// TestVectorizeHasFixedDimAndNonNegative 钉住维度契约与取值域。
//
// 变异：让 VectorizeDense 返回变长切片 ⇒ 本例红。
func TestVectorizeHasFixedDimAndNonNegative(t *testing.T) {
	const dim = 32
	e := newTrained(t, []string{"alpha beta", "gamma delta", "epsilon"}, dim)

	for _, text := range []string{"", "alpha", "alpha beta gamma", "完全没出现过的词"} {
		v, err := e.VectorizeDense(text)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		if len(v) != dim {
			t.Fatalf("★ %q 的向量维度 = %d，期望 %d", text, len(v), dim)
		}
		for i, x := range v {
			if math.IsNaN(x) || math.IsInf(x, 0) {
				t.Fatalf("★ %q 第 %d 维是 NaN/Inf", text, i)
			}
			if x < 0 {
				t.Fatalf("★ %q 第 %d 维为负（%v）—— TF-IDF 权重不该为负", text, i, x)
			}
		}
	}
}

// TestEmptyTextGivesZeroVector 钉住边界：空文本不该 panic，且给出零向量。
func TestEmptyTextGivesZeroVector(t *testing.T) {
	e := newTrained(t, []string{"alpha beta"}, 16)
	v, err := e.VectorizeDense("")
	if err != nil {
		t.Fatalf("空文本报错: %v", err)
	}
	for i, x := range v {
		if x != 0 {
			t.Fatalf("★ 空文本第 %d 维 = %v，期望 0", i, x)
		}
	}
}

// TestSearchRanksRelatedAboveUnrelated 是**核心质量判据**：
// 相近文本必须排在无关文本之前。
//
// 这正是 engineering-lessons §1.1 记的坑：16 条判据全绿而检索结果完全无关
// （分数全挤在 0.75~0.79）。所以这条不看数值，只看**排序**。
//
// 变异：让 Search 返回未排序结果 ⇒ 本例红。
func TestSearchRanksRelatedAboveUnrelated(t *testing.T) {
	docs := []string{
		"billing service handles payments and invoices",
		"auth service handles login and sessions",
		"billing invoice retry policy",
	}
	e := newTrained(t, docs, 128)
	idx := tfidf.NewSearchableIndex(e)
	for i, d := range docs {
		idx.Add(string(rune('a'+i)), d)
	}

	hits := idx.Search("billing invoice", 3)
	if len(hits) < 2 {
		t.Fatalf("检索只返回 %d 条，期望至少 2 条", len(hits))
	}
	first := hits[0].ID
	if first != "a" && first != "c" {
		t.Fatalf("★ 查 'billing invoice' 排第一的是 %q ——\n"+
			"它既不含 billing 也不含 invoice。检索判别力失效。", first)
	}
	for i, h := range hits {
		if h.ID == "b" {
			t.Fatalf("★ 完全无关的 auth 文档排在第 %d 位（共 %d 条）", i+1, len(hits))
		}
	}
}

// TestAddRemoveChangesResults 钉住索引可变性契约。
//
// 变异：Remove 不真正删除 ⇒ 本例红。
func TestAddRemoveChangesResults(t *testing.T) {
	e := newTrained(t, []string{"alpha beta", "gamma delta"}, 32)
	idx := tfidf.NewSearchableIndex(e)
	idx.Add("x", "alpha beta gamma")

	if got := idx.Search("alpha", 3); len(got) == 0 {
		t.Fatal("加入后检索不到")
	}
	idx.Remove("x")
	if got := idx.Search("alpha", 3); len(got) != 0 {
		t.Fatalf("★ Remove 之后仍检索到 %d 条 —— 删除没生效", len(got))
	}
}

// TestFingerprintAndDim 钉住元信息契约。
func TestFingerprintAndDim(t *testing.T) {
	e := newTrained(t, []string{"a b c"}, 24)
	if e.Dim() != 24 {
		t.Fatalf("Dim() = %d，期望 24", e.Dim())
	}
	if e.Fingerprint() == "" {
		t.Fatal("★ Fingerprint 为空 —— 下游靠它判断缓存是否还有效")
	}
}
