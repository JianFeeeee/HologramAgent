package test

// internal/memory/vector 的黑盒判据：纯数学函数 + Store 公开方法，输入 → 输出。

import (
	"math"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
)

// TestCosineSimilarity：同向=1、正交=0、零向量=0。
func TestCosineSimilarity(t *testing.T) {
	cases := []struct {
		name string
		a, b vector.Vector
		want float64
	}{
		{"同向", vector.Vector{"x": 2, "y": 3}, vector.Vector{"x": 4, "y": 6}, 1},
		{"正交", vector.Vector{"x": 1}, vector.Vector{"y": 1}, 0},
		{"零向量", vector.Vector{}, vector.Vector{"x": 1}, 0},
	}
	for _, c := range cases {
		got := vector.CosineSimilarity(c.a, c.b)
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("★ %s: CosineSimilarity=%v，期望 %v", c.name, got, c.want)
		}
	}
}

// TestDenseCosine：稠密向量同样的三种基本情形。
func TestDenseCosine(t *testing.T) {
	if got := vector.DenseCosine([]float64{1, 0}, []float64{1, 0}); math.Abs(got-1) > 1e-9 {
		t.Fatalf("★ 同向 = %v，期望 1", got)
	}
	if got := vector.DenseCosine([]float64{1, 0}, []float64{0, 1}); got != 0 {
		t.Fatalf("★ 正交 = %v，期望 0", got)
	}
	if got := vector.DenseCosine([]float64{0, 0}, []float64{1, 1}); got != 0 {
		t.Fatalf("★ 零向量 = %v，期望 0", got)
	}
	// 非单位向量：不归一化会得到点积 32 而不是余弦 1。
	if got := vector.DenseCosine([]float64{4, 4}, []float64{2, 2}); math.Abs(got-1) > 1e-9 {
		t.Fatalf("★ 非单位同向 = %v，期望 1（未归一化会得 32）", got)
	}
}

// TestStoreInsertSearchRemove：Insert → Search 命中 → Remove → 不再命中。
func TestStoreInsertSearchRemove(t *testing.T) {
	s := vector.NewStore()
	s.SetMinScore(0)
	if got := s.Size(); got != 0 {
		t.Fatalf("★ 新 Store Size=%d，期望 0", got)
	}
	s.Insert("doc-a", "账单服务", vector.Vector{"账单": 1, "发票": 1}, nil)
	s.Insert("doc-b", "登录会话", vector.Vector{"登录": 1}, nil)
	if got := s.Size(); got != 2 {
		t.Fatalf("★ Insert 后 Size=%d，期望 2", got)
	}
	hits := s.Search(vector.Vector{"账单": 1}, 2)
	if len(hits) == 0 || hits[0].ID != "doc-a" {
		t.Fatalf("★ Search 排第一的是 %+v，期望 doc-a", hits)
	}
	s.Remove("doc-a")
	if got := s.Size(); got != 1 {
		t.Fatalf("★ Remove 后 Size=%d，期望 1", got)
	}
	hits = s.Search(vector.Vector{"账单": 1}, 2)
	for _, h := range hits {
		if h.ID == "doc-a" {
			t.Fatal("★ Remove 后仍能检索到 doc-a")
		}
	}
}
