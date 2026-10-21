package knowledge

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
)

// fakeDense 是可控的稠密向量器：按文本查表，缺省给同一个向量。
// 用它把「稠密路无区分度/给空向量」这类真实故障在单测里复现出来。
type fakeDense struct {
	byText map[string]vector.Vector
	def    vector.Vector
}

func (f fakeDense) Vectorize(text string) vector.Vector {
	if v, ok := f.byText[text]; ok {
		return v
	}
	return f.def
}

// EmbedImage 满足 vector.Vectorizer 接口（本用例只用到文本路）。
func (f fakeDense) EmbedImage([]byte, string) (vector.Vector, error) {
	return f.def, nil
}

func newTestStore(t *testing.T, dense vector.Vectorizer) *Store {
	t.Helper()
	st := NewStore(t.TempDir())
	if dense != nil {
		st.SetVectorizer(dense)
	}
	return st
}

func mustAdd(t *testing.T, st *Store, name, content string) {
	t.Helper()
	if err := st.Add(name, content); err != nil {
		t.Fatalf("add %s: %v", name, err)
	}
}

func names(hits []*Knowledge) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Name
	}
	return out
}

// base36 生成互不重复的短串（造独特词的量级要大，不能用 a..z 循环重复）。
func base36(n int) string {
	const digits = "0123456789abcdefghijklmnopqrstuvwxyz"
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(digits[n%36]) + out
		n /= 36
	}
	return out
}

// 稠密路给**空向量**（真实场景：查询词全在停用词表里，例如"最近更新"）时，
// 词法路必须把结果救回来——修复前这里直接返回空。
func TestSearchLexicalRescuesEmptyDenseQuery(t *testing.T) {
	dense := fakeDense{def: vector.Vector{"0": 1}}
	st := newTestStore(t, dense)
	mustAdd(t, st, "changelog_v1", "最近更新了很多东西 发布说明")
	mustAdd(t, st, "weather_doc", "天气预报 晴转多云")

	// 让"更新"的稠密向量为空（模拟停用词化）
	st.SetVectorizer(fakeDense{
		byText: map[string]vector.Vector{"更新": {}},
		def:    vector.Vector{"0": 1},
	})
	hits := st.Search("更新", 5)
	if len(hits) == 0 {
		t.Fatal("稠密路给空向量时不该返回空结果（词法路应救回来）")
	}
	if hits[0].Name != "changelog_v1" {
		t.Fatalf("应命中含「更新」的条目，实际: %v", names(hits))
	}
}

// 稠密路对所有文本给**同一个向量**（真实故障：词向量平均后各向异性、区分度极低）时，
// 排序必须由词法路决定。
func TestSearchLexicalBreaksDenseTies(t *testing.T) {
	same := vector.Vector{"0": 1, "1": 1}
	st := newTestStore(t, fakeDense{def: same})
	mustAdd(t, st, "plugin_dev_build", "插件构建与部署 hmapdev 命令")
	mustAdd(t, st, "cangjie_manual", "仓颉编程语言知识手册")
	mustAdd(t, st, "privacy_policy", "隐私政策")

	hits := st.Search("hmapdev 构建", 3)
	if len(hits) == 0 || hits[0].Name != "plugin_dev_build" {
		t.Fatalf("稠密路并列时应由词法路选出 plugin_dev_build，实际: %v", names(hits))
	}
}

// 词法路的候选中选阈值必须是 0：TF-IDF 余弦量级只有 0.0~0.2，沿用稠密路的 0.05
// 会把有效候选静默砍掉（真实 KB 实测自检索 MRR 0.307→0.193）。
//
// 判据分两层，各钉一半：
//   - **语义层**由 internal/memory/vector 的 TestSearchScoredRespectsMinScore 证明
//     （同一候选在默认阈值下被过滤、阈值 0 时被召回）；
//   - **接线层**在这里钉住：知识库的词法路用的就是阈值 0 的那个 store。
//     不在这里造「低余弦夹具」的原因：分词器会丢掉纯拉丁 token、也会过滤未登录词，
//     造出来的夹具余弦根本压不到阈值以下（我先试了两种，余弦 0.23/0.27，
//     前提断言直接把这两版夹具否掉了）。
func TestLexicalStoreUsesZeroMinScore(t *testing.T) {
	st := newTestStore(t, fakeDense{def: vector.Vector{"0": 1}})
	if got := st.lex.MinScore(); got != 0 {
		t.Fatalf("词法路阈值必须为 0，实际 %v（沿用稠密路阈值会静默丢候选）", got)
	}
	if got := st.vec.MinScore(); got != vector.DefaultMinScore {
		t.Fatalf("稠密路阈值应保持默认 %v，实际 %v", vector.DefaultMinScore, got)
	}
}

// Add / Remove 必须同时维护两路索引：只维护一路会让被删条目继续被检索命中
// （或新条目只在其中一路可见）。
func TestAddRemoveKeepsBothPaths(t *testing.T) {
	st := newTestStore(t, fakeDense{def: vector.Vector{"0": 1}})
	mustAdd(t, st, "alpha", "alpha 独有词 alphaonly")
	mustAdd(t, st, "beta", "beta 独有词 betaonly")

	has := func(q, want string) bool {
		for _, h := range st.Search(q, 5) {
			if h.Name == want {
				return true
			}
		}
		return false
	}
	if !has("alphaonly", "alpha") {
		t.Fatal("新增条目应可被检索到")
	}
	if err := st.Remove("alpha"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if has("alphaonly", "alpha") {
		t.Fatal("已删除条目仍被检索命中（两路索引有一路没清）")
	}
	if !has("betaonly", "beta") {
		t.Fatal("删除其它条目不应影响 beta")
	}
}

// 分数相同时必须按名字定序，保证结果可重复（否则同一查询两次结果可能不同）。
func TestSearchDeterministicOnTies(t *testing.T) {
	same := vector.Vector{"0": 1}
	st := newTestStore(t, fakeDense{def: same})
	for _, n := range []string{"ccc", "aaa", "bbb"} {
		mustAdd(t, st, n, "完全一样的内容")
	}
	first := names(st.Search("完全一样的内容", 3))
	for i := 0; i < 5; i++ {
		got := names(st.Search("完全一样的内容", 3))
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("结果不确定：第 %d 次 %v != 首次 %v", i, got, first)
			}
		}
	}
}

// 两路都空时不能 panic，且应返回空。
func TestSearchEmptyStore(t *testing.T) {
	st := newTestStore(t, fakeDense{def: vector.Vector{"0": 1}})
	if hits := st.Search("随便", 5); len(hits) != 0 {
		t.Fatalf("空库应返回空，实际 %v", names(hits))
	}
}
