package test

// internal/knowledge 的黑盒判据：NewStore(root) → Add → 可观察输出。
// 不读 .index.json、不碰 s.items、不引用文档。

import (
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
)

func newStore(t *testing.T) *knowledge.Store {
	t.Helper()
	return knowledge.NewStore(t.TempDir())
}

// TestAddThenListAndStats：Add 之后 List / Stats 必须反映该条目。
func TestAddThenListAndStats(t *testing.T) {
	s := newStore(t)
	if err := s.Add("tech/go/gc", "Go 的垃圾回收使用三色标记"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got := s.List()
	if len(got) != 1 || got[0] != "tech/go/gc" {
		t.Fatalf("★ List = %v，期望 [tech/go/gc]", got)
	}
	st := s.Stats()
	if st["knowledge_count"] != 1 {
		t.Fatalf("★ knowledge_count = %v，期望 1", st["knowledge_count"])
	}
}

// TestRemoveActuallyRemoves：Remove 之后 List 不再含该条目，且二次 Remove 报错。
func TestRemoveActuallyRemoves(t *testing.T) {
	s := newStore(t)
	if err := s.Add("a", "内容A"); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("a"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("★ Remove 后 List = %v，期望空", got)
	}
	if err := s.Remove("a"); err == nil {
		t.Fatal("★ 删除不存在的条目应报错，实际返回 nil")
	}
}

// TestPathTraversalIsRejected：名字里的 ".." 不得逃出知识根目录。
//
// 这是真实事故形态：早期 Remove("..") 直接删掉整个 data 目录。
func TestPathTraversalIsRejected(t *testing.T) {
	s := newStore(t)
	for _, name := range []string{"..", "../escape", "a/../b", ".hidden", ""} {
		if err := s.Add(name, "x"); err == nil {
			t.Fatalf("★ Add(%q) 被接受了 —— 路径可逃出知识根", name)
		}
	}
	if got := s.List(); len(got) != 0 {
		t.Fatalf("★ 非法名字留下了条目：%v", got)
	}
}

// TestCategoriesIncludeIntermediateLevels：分类树必须给出中间层。
func TestCategoriesIncludeIntermediateLevels(t *testing.T) {
	s := newStore(t)
	if err := s.Add("tech/go/concurrency", "goroutine 调度"); err != nil {
		t.Fatal(err)
	}
	cats := s.Categories()
	// Category 是**父路径**：tech/go/concurrency 条目的 Category 是 tech/go。
	want := map[string]bool{"tech": false, "tech/go": false}
	for _, c := range cats {
		if _, ok := want[c]; ok {
			want[c] = true
		}
	}
	for c, hit := range want {
		if !hit {
			t.Fatalf("★ Categories() = %v，缺中间层 %q", cats, c)
		}
	}
}

// TestSearchRanksRelevantAboveUnrelated：只问排序，不钉数值。
//
// 分数全挤在 0.75~0.79 时 16 条判据全绿而检索结果完全无关 ——
// 所以判据必须断言「相关条目排在无关条目之前」。
func TestSearchRanksRelevantAboveUnrelated(t *testing.T) {
	s := newStore(t)
	if err := s.Add("billing/invoice", "账单服务的发票重试策略与对账流程"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("auth/session", "登录会话的过期时间与刷新令牌"); err != nil {
		t.Fatal(err)
	}
	hits := s.Search("发票 重试 账单", 2)
	if len(hits) == 0 {
		t.Fatal("★ 检索无结果")
	}
	if hits[0].Name != "billing/invoice" {
		t.Fatalf("★ 排第一的是 %q，期望 billing/invoice（检索判别力失效）", hits[0].Name)
	}
}
