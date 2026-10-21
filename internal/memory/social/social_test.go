package social

import (
	"path/filepath"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

func setupTestDB(t *testing.T) (*memory.GraphDB, *SocialStore) {
	t.Helper()
	db, err := memory.NewGraphDB(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := New(db)
	return db, s
}

func TestNew(t *testing.T) {
	s := New(nil)
	if s == nil {
		t.Fatal("expected non-nil social store")
	}
}

func TestGetPersonNotFound(t *testing.T) {
	_, s := setupTestDB(t)
	defer s.db.Close()

	_, err := s.GetPerson("不存在的人")
	if err == nil {
		t.Fatal("expected error for non-existent person")
	}
}

func TestSetAndGetTrait(t *testing.T) {
	_, s := setupTestDB(t)
	defer s.db.Close()

	err := s.SetTrait("张三", "性格", "开朗")
	if err != nil {
		t.Fatal(err)
	}

	val, found := s.GetTrait("张三", "性格")
	if !found {
		t.Fatal("trait not found")
	}
	if val != "开朗" {
		t.Errorf("expected '开朗', got %q", val)
	}
}

func TestUpdateTrait(t *testing.T) {
	_, s := setupTestDB(t)
	defer s.db.Close()

	s.SetTrait("张三", "性格", "开朗")
	s.SetTrait("张三", "性格", "内向")

	val, found := s.GetTrait("张三", "性格")
	if !found {
		t.Fatal("trait not found after update")
	}
	if val != "内向" {
		t.Errorf("expected '内向', got %q", val)
	}
}

func TestAddAndGetRelation(t *testing.T) {
	_, s := setupTestDB(t)
	defer s.db.Close()

	err := s.AddRelation("张三", "朋友", "李四")
	if err != nil {
		t.Fatal(err)
	}

	rels, err := s.GetRelations("张三")
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 {
		t.Fatalf("expected 1 relation, got %d", len(rels))
	}
	if rels[0].Person != "李四" || rels[0].Relation != "朋友" {
		t.Errorf("unexpected relation: %+v", rels[0])
	}
}

func TestGetNetwork(t *testing.T) {
	_, s := setupTestDB(t)
	defer s.db.Close()

	s.AddRelation("张三", "朋友", "李四")
	s.AddRelation("李四", "同事", "王五")

	network, err := s.GetNetwork("张三", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(network) < 2 {
		t.Errorf("expected at least 2 persons in network, got %d", len(network))
	}
}

func TestListPersons(t *testing.T) {
	_, s := setupTestDB(t)
	defer s.db.Close()

	s.SetTrait("张三", "性格", "开朗")
	s.AddRelation("张三", "朋友", "李四")

	persons, err := s.ListPersons()
	if err != nil {
		t.Fatal(err)
	}
	if len(persons) == 0 {
		t.Fatal("expected at least one person")
	}
}

func TestRemoveRelation(t *testing.T) {
	_, s := setupTestDB(t)
	defer s.db.Close()

	s.AddRelation("张三", "朋友", "李四")
	err := s.RemoveRelation("张三", "朋友", "李四")
	if err != nil {
		t.Fatal(err)
	}

	rels, _ := s.GetRelations("张三")
	if len(rels) != 0 {
		t.Errorf("expected 0 relations after remove, got %d", len(rels))
	}
}

func TestNilDBSafety(t *testing.T) {
	s := New(nil)

	_, err := s.GetPerson("test")
	if err == nil {
		t.Error("expected error with nil db")
	}

	err = s.SetTrait("test", "t", "v")
	if err == nil {
		t.Error("expected error with nil db")
	}

	_, found := s.GetTrait("test", "t")
	if found {
		t.Error("expected not found with nil db")
	}

	_, err = s.ListPersons()
	if err == nil {
		t.Error("expected error with nil db")
	}
}

func TestGetPersonProfile(t *testing.T) {
	_, s := setupTestDB(t)
	defer s.db.Close()

	s.SetTrait("张三", "性格", "开朗")
	s.SetTrait("张三", "职业", "程序员")
	s.AddRelation("张三", "朋友", "李四")

	profile, err := s.GetPerson("张三")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Name != "张三" {
		t.Errorf("expected name '张三', got %q", profile.Name)
	}
	if profile.Traits["性格"] != "开朗" {
		t.Errorf("expected trait '开朗', got %q", profile.Traits["性格"])
	}
	if profile.Traits["职业"] != "程序员" {
		t.Errorf("expected trait '程序员', got %q", profile.Traits["职业"])
	}
	if len(profile.Relations) != 1 || profile.Relations[0].Person != "李四" {
		// BFS 可能返回重复关系，用 set 去重验证
		seen := make(map[string]bool)
		for _, r := range profile.Relations {
			seen[r.Person+"/"+r.Relation] = true
		}
		if !seen["李四/朋友"] {
			t.Errorf("expected relation 李四/朋友, got %+v", profile.Relations)
		}
	}
}
