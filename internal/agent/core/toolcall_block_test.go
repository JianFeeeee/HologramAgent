package core

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// 块召回接入 memory_recall 工具后的行为判据。
//
// 为什么这几条重要：块向量召回是**新增的优先路径**，它出错有两种静默后果——
//  ① 命中垃圾却返回，把符号路的正确结果挤掉；
//  ② 向量侧未就绪时整体失败，让模型完全失去记忆（比召回差得多）。
// 两条都必须钉住。

// 造一个带块向量的 agent。
func newBlockRecallAgent(t *testing.T, blocks []memory.MemoryBlock) *Agent {
	t.Helper()
	a := newTestAgent(nil)
	db, err := memory.NewGraphDB(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	if err := db.PutMemoryBlocks(blocks); err != nil {
		t.Fatalf("PutMemoryBlocks: %v", err)
	}
	a.memory = db
	a.multimodalSpace = fixedSpace{vec: []float64{1, 0}, fp: "fp1"}
	t.Cleanup(func() { db.Close() })
	return a
}

// fixedSpace 返回固定向量，用于可预测的召回排序。
type fixedSpace struct {
	vec    []float64
	fp     string
	loaded bool
}

func (s fixedSpace) VectorizeDense(string) ([]float64, error) { return s.vec, nil }
func (s fixedSpace) EmbedImageDense([]byte, string) ([]float64, error) {
	return s.vec, nil
}
func (s fixedSpace) Fingerprint() string { return s.fp }
func (s fixedSpace) Dim() int            { return len(s.vec) }
func (s fixedSpace) Loaded() bool {
	// 零值 fixedSpace{} 视为未加载 —— 让"未就绪"可被显式构造。
	if s.vec == nil {
		return s.loaded
	}
	return true
}
func (s fixedSpace) Close() {}

func callRecall(t *testing.T, a *Agent, q string) string {
	t.Helper()
	return a.executeMemoryTool(api.ToolCall{
		ID: "c1", Name: "memory_recall",
		Arguments: map[string]interface{}{"query_intent": q},
	}, nil)
}

// ★ 块有命中时，结果里必须出现块文本（而不是走符号路）。
func TestMemoryRecall_块召回优先(t *testing.T) {
	a := newBlockRecallAgent(t, []memory.MemoryBlock{
		{ID: "hit", Modality: memory.BlockText, Text: "第112批 停机4分",
			Vector: []float64{1, 0}, Fingerprint: "fp1"},
		{ID: "miss", Modality: memory.BlockText, Text: "完全无关",
			Vector: []float64{0, 1}, Fingerprint: "fp1"},
	})

	got := callRecall(t, a, "停机多久")
	if !strings.Contains(got, "第112批") {
		t.Fatalf("块召回应命中并返回块文本，实际: %s", got)
	}
	if strings.Contains(got, "完全无关") {
		t.Errorf("低分块不该出现（MinScore 应滤掉），实际: %s", got)
	}
	if !strings.Contains(got, "相关记忆片段") {
		t.Errorf("应标明这是块召回结果，实际: %s", got)
	}
}

// ★ 向量侧未加载时必须回退符号路，不能整体失败。
// 判据：不出现"检索失败"，且仍返回某种结果（符号路的实体或未找到提示）。
func TestMemoryRecall_向量未加载则回退(t *testing.T) {
	a := newBlockRecallAgent(t, []memory.MemoryBlock{
		{ID: "hit", Modality: memory.BlockText, Text: "有向量",
			Vector: []float64{1, 0}, Fingerprint: "fp1"},
	})
	a.multimodalSpace = fixedSpace{} // 未加载

	got := callRecall(t, a, "有向量")
	if strings.Contains(got, "检索失败") {
		t.Fatalf("向量未加载不该让检索失败，实际: %s", got)
	}
	if strings.Contains(got, "相关记忆片段") {
		t.Errorf("未加载时不该走块路，实际: %s", got)
	}
}

// 未配置多模态空间（nil）时同样回退，不 panic。
func TestMemoryRecall_无向量空间则回退(t *testing.T) {
	a := newBlockRecallAgent(t, nil)
	a.multimodalSpace = nil
	got := callRecall(t, a, "随便问问")
	if strings.Contains(got, "检索失败") {
		t.Fatalf("无向量空间不该让检索失败，实际: %s", got)
	}
}

// ★ 指纹不匹配的块必须被跳过（换向量空间后旧块不能污染结果）。
func TestMemoryRecall_指纹不匹配的块被跳过(t *testing.T) {
	a := newBlockRecallAgent(t, []memory.MemoryBlock{
		{ID: "old", Modality: memory.BlockText, Text: "旧空间的内容",
			Vector: []float64{1, 0}, Fingerprint: "old-fp"},
	})

	got := callRecall(t, a, "旧空间的内容")
	if strings.Contains(got, "旧空间的内容") {
		t.Fatalf("指纹不匹配的块不该被召回，实际: %s", got)
	}
}

// 向量化报错时回退（不把错误抛给模型——那会让它以为"记忆不存在"）。
func TestMemoryRecall_向量化失败则回退(t *testing.T) {
	a := newBlockRecallAgent(t, []memory.MemoryBlock{
		{ID: "x", Modality: memory.BlockText, Text: "内容",
			Vector: []float64{1, 0}, Fingerprint: "fp1"},
	})
	a.multimodalSpace = errSpace{}
	got := callRecall(t, a, "内容")
	if strings.Contains(got, "检索失败") {
		t.Fatalf("向量化失败不该让检索失败，实际: %s", got)
	}
}

type errSpace struct{}

func (errSpace) VectorizeDense(string) ([]float64, error) { return nil, errTest }
func (errSpace) EmbedImageDense([]byte, string) ([]float64, error) {
	return nil, errTest
}
func (errSpace) Fingerprint() string { return "fp" }
func (errSpace) Dim() int            { return 2 }
func (errSpace) Loaded() bool        { return true }
func (errSpace) Close()              {}

var errTest = &testError{"向量化失败"}

type testError struct{ s string }

func (e *testError) Error() string { return e.s }
