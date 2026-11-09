package test

// internal/memory/social 的黑盒判据：New(GraphDB) → SetTrait/AddRelation → 输出。
// 用真实内存图库，不碰 s.db 内部。

import (
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/social"
)

func newStore(t *testing.T) *social.SocialStore {
	t.Helper()
	db, err := memory.NewGraphDB(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return social.New(db)
}

// TestNilDBReportsError：未装配图库时每个写操作都报错（不静默）。
func TestNilDBReportsError(t *testing.T) {
	s := social.New(nil)
	if err := s.SetTrait("张三", "职业", "工程师"); err == nil {
		t.Fatal("★ 无图库却 SetTrait 成功")
	}
	if err := s.AddRelation("张三", "朋友", "李四"); err == nil {
		t.Fatal("★ 无图库却 AddRelation 成功")
	}
}

// TestSetTraitThenGetTrait：特质写入后能读回。
func TestSetTraitThenGetTrait(t *testing.T) {
	s := newStore(t)
	if err := s.SetTrait("张三", "职业", "工程师"); err != nil {
		t.Fatalf("SetTrait: %v", err)
	}
	got, ok := s.GetTrait("张三", "职业")
	if !ok || got != "工程师" {
		t.Fatalf("★ GetTrait = %q / %v，期望 工程师 / true", got, ok)
	}
}

// TestSetTraitOverwrites：同人同特质再设值覆盖，不出现两份。
func TestSetTraitOverwrites(t *testing.T) {
	s := newStore(t)
	_ = s.SetTrait("张三", "职业", "工程师")
	_ = s.SetTrait("张三", "职业", "教师")
	got, _ := s.GetTrait("张三", "职业")
	if got != "教师" {
		t.Fatalf("★ 覆盖后 GetTrait = %q，期望 教师", got)
	}
}

// TestAddRelationThenGetRelations：关系写入后能读回。
func TestAddRelationThenGetRelations(t *testing.T) {
	s := newStore(t)
	if err := s.AddRelation("张三", "朋友", "李四"); err != nil {
		t.Fatalf("AddRelation: %v", err)
	}
	rels, err := s.GetRelations("张三")
	if err != nil {
		t.Fatalf("GetRelations: %v", err)
	}
	if len(rels) == 0 {
		t.Fatal("★ GetRelations 返回空")
	}
}

// TestListPersons：写入人物后 ListPersons 含该人名。
func TestListPersons(t *testing.T) {
	s := newStore(t)
	_ = s.SetTrait("王五", "职业", "医生")
	persons, err := s.ListPersons()
	if err != nil {
		t.Fatalf("ListPersons: %v", err)
	}
	found := false
	for _, p := range persons {
		if p == "王五" {
			found = true
		}
	}
	if !found {
		t.Fatalf("★ ListPersons = %v，缺 王五", persons)
	}
}
