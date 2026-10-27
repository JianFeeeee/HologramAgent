package memory

// 场景召回必须按**块向量余弦**与当前 query 相关，而不是按历史热度/时间。
//
// 现场（2026-10-27）：群里聊平板/机型，注入的 1833 字记忆里九成是
// `rt 标签 / rtchild / inputch` 这类老图库高频三元组 —— 模型"忘事"就是
// 这么来的。块里本来就存着向量，所以相关性只是一次余弦比较。

import (
	"math"
	"strings"
	"testing"
)

func TestDenseCosineBasics(t *testing.T) {
	if got := denseCosine([]float64{1, 0}, []float64{1, 0}); math.Abs(got-1) > 1e-9 {
		t.Fatalf("同向余弦应为 1，实际 %v", got)
	}
	if got := denseCosine([]float64{1, 0}, []float64{0, 1}); math.Abs(got) > 1e-9 {
		t.Fatalf("正交余弦应为 0，实际 %v", got)
	}
	if got := denseCosine([]float64{0, 0}, []float64{1, 0}); got != 0 {
		t.Fatalf("零向量余弦应为 0，实际 %v", got)
	}
}

// fakeEmbedder 把文本映射成固定向量：包含 "平板" ⇒ 与 query 同向；
// "rt" ⇒ 正交。用来验证"闸门按语义而不是字面"。
type fakeEmbedder struct{}

func (fakeEmbedder) Loaded() bool        { return true }
func (fakeEmbedder) Dim() int            { return 2 }
func (fakeEmbedder) Fingerprint() string { return "fake" }
func (fakeEmbedder) VectorizeDense(text string) ([]float64, error) {
	if hasSub(text, "平板") || hasSub(text, "机型") {
		return []float64{1, 0}, nil
	}
	return []float64{0, 1}, nil
}
func (fakeEmbedder) Vectorize([]string) [][]float64 { return nil }

// EmbedImageDense / Close 满足 vector.MultimodalEmbedder 的完整接口
// （测试只用文本路；图片与生命周期在真空间里才有意义）。
func (fakeEmbedder) EmbedImageDense([]byte, string) ([]float64, error) { return nil, nil }

func (fakeEmbedder) Close() {}

func hasSub(s, sub string) bool {
	return strings.Contains(s, sub)
}

func TestRankSceneRelationsByVectorDropsUnrelated(t *testing.T) {
	idx := NewIndexer(nil)
	idx.SetDenseSpace(fakeEmbedder{})
	rels := []Relation{
		{SourceName: "rt标签", TargetName: "内核", SourceVec: []float64{0, 1}, TargetVec: []float64{0, 1}},
		{SourceName: "平板", TargetName: "promax", SourceVec: []float64{1, 0}, TargetVec: []float64{0.9, 0.1}},
	}
	kept := idx.rankSceneRelationsByVector("群友在聊平板机型", rels)
	if len(kept) != 1 || kept[0].SourceName != "平板" {
		t.Fatalf("只应保留与 query 语义相关的三元组，实际 %+v", kept)
	}
}

func TestRankSceneRelationsByVectorNoEmbedderKeepsOrder(t *testing.T) {
	idx := NewIndexer(nil) // 未注入稠密空间
	rels := []Relation{{SourceName: "a"}, {SourceName: "b"}}
	kept := idx.rankSceneRelationsByVector("随便问问", rels)
	if len(kept) != 2 || kept[0].SourceName != "a" {
		t.Fatalf("无稠密空间时应保留原顺序（不能用字面匹配冒充语义），实际 %+v", kept)
	}
}
