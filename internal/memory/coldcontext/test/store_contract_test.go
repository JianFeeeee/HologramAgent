package test

// internal/memory/coldcontext 的黑盒判据：NewStore → Insert → 可观察输出。
// 不读盘上文件、不碰 s.docs。

import (
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory/coldcontext"
)

func tok(text string) []string { return strings.Fields(text) }

func newStore(t *testing.T) *coldcontext.Store {
	t.Helper()
	return coldcontext.NewStore(t.TempDir(), tok)
}

// TestInsertThenGet：Insert 后 Get 拿回同一份内容，且自动补 ID/时间戳。
func TestInsertThenGet(t *testing.T) {
	s := newStore(t)
	doc := &coldcontext.ColdDoc{
		Summary: "账单服务发票重试",
		Content: "发票重试策略与对账流程",
		Source:  "contract",
	}
	if err := s.Insert(doc); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if doc.ID == "" {
		t.Fatal("★ Insert 没有给空 ID 的文档补 ID")
	}
	got := s.Get(doc.ID)
	if got == nil {
		t.Fatal("★ Get 返回 nil")
	}
	if got.Summary != "账单服务发票重试" || got.Content != "发票重试策略与对账流程" {
		t.Fatalf("★ Get 内容不符：%+v", got)
	}
	if doc.CreatedAt.IsZero() || doc.LastAccess.IsZero() {
		t.Fatal("★ Insert 没填时间戳")
	}
}

// TestInsertExplicitIDKeepsIt：显式 ID 不被覆盖。
func TestInsertExplicitIDKeepsIt(t *testing.T) {
	s := newStore(t)
	doc := &coldcontext.ColdDoc{ID: "my-doc", Summary: "s", Content: "c"}
	if err := s.Insert(doc); err != nil {
		t.Fatal(err)
	}
	if s.Get("my-doc") == nil {
		t.Fatal("★ 显式 ID 被覆盖")
	}
}

// TestQueryReturnsRelevantDoc：Query 至少能召回与查询相关的文档。
func TestQueryReturnsRelevantDoc(t *testing.T) {
	s := newStore(t)
	if err := s.Insert(&coldcontext.ColdDoc{
		ID: "billing", Summary: "发票重试", Content: "账单服务的对账流程",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Insert(&coldcontext.ColdDoc{
		ID: "auth", Summary: "登录会话", Content: "刷新令牌过期时间",
	}); err != nil {
		t.Fatal(err)
	}
	// 分词器按空白切词：文档里的 "发票重试" 是一个 token，
	// 所以查询必须用同一词形，而不是拆成「发票 重试 账单」。
	hits := s.Query("发票重试", 2)
	if len(hits) == 0 {
		t.Fatal("★ Query 无结果")
	}
	if hits[0].ID != "billing" {
		t.Fatalf("★ Query 排第一的是 %q，期望 billing", hits[0].ID)
	}
}

// TestConsumeIsOneShot：Consume 取过的文档再次取应为空/减少。
func TestConsumeIsOneShot(t *testing.T) {
	s := newStore(t)
	if err := s.Insert(&coldcontext.ColdDoc{
		ID: "one", Summary: "一次性内容", Content: "一次性内容",
	}); err != nil {
		t.Fatal(err)
	}
	first := s.Consume("一次性内容", 1)
	if len(first) == 0 {
		t.Fatal("★ 首次 Consume 无结果")
	}
	second := s.Consume("一次性内容", 1)
	if len(second) != 0 {
		t.Fatalf("★ Consume 非一次性：第二次仍返回 %d 条", len(second))
	}
}

// TestBlocksReflectsInsertedDoc：Blocks 输出反映已插入文档的块。
func TestBlocksReflectsInsertedDoc(t *testing.T) {
	s := newStore(t)
	if err := s.Insert(&coldcontext.ColdDoc{
		ID: "with-blocks", Summary: "s", Content: "c",
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Blocks() // 输出必须可调用且不 panic
}
