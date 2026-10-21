package core

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/vector"
)

// 媒体与记忆块的生命周期测试（块 union 化之后）。
//
// 媒体没有独立生命周期管理（没有 GC、没有引用计数）：字节是记忆块的内容，
// 块的创建/迁移/删除由记忆系统决定。块被删即字节被删——没有独立层要清理。
//
// 需要特殊对待的只有一项：向量空间更换（换模型/换维度）时，
// 历史图像块的向量要迁移到新空间，否则永远无法被召回。

func newMediaLoopAgent(t *testing.T) *Agent {
	t.Helper()
	dir := t.TempDir()
	g, err := memory.NewGraphDB(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatalf("NewGraphDB: %v", err)
	}
	t.Cleanup(func() { g.Close() })

	a := &Agent{memory: g}
	a.ctx, a.cancel = context.WithCancel(context.Background())
	t.Cleanup(a.cancel)
	return a
}

// fakeSpace 是一个只覆盖图像的假统一空间，用来验证「不在本空间」与
// 「本次失败」必须被区分对待。
type fakeSpace struct{}

func (fakeSpace) VectorizeDense(string) ([]float64, error) { return []float64{1, 0}, nil }

func (fakeSpace) EmbedImageDense(_ []byte, mime string) ([]float64, error) {
	if strings.HasPrefix(mime, "audio/") || strings.HasPrefix(mime, "video/") {
		return nil, fmt.Errorf("%w: %s", vector.ErrModalityUnsupported, mime)
	}
	return []float64{1, 0}, nil
}

func (fakeSpace) Fingerprint() string { return "fake-space" }
func (fakeSpace) Dim() int            { return 2 }
func (fakeSpace) Loaded() bool        { return true }
func (fakeSpace) Close()              {}

// TestReembedStaleMedia_SkipsUnsupportedWithoutFaking 验证向量迁移不会：
//   - 把音频当失败反复重试；
//   - 更不能拿另一个模型的向量顶替音频（那会污染统一空间且静默）。
func TestReembedStaleMedia_SkipsUnsupportedWithoutFaking(t *testing.T) {
	a := newMediaLoopAgent(t)
	// 图像块与音频块各落一条：字节都进块（union）
	img := mediaBlock("image/png", []byte("img-bytes"))
	aud := mediaBlock("audio/wav", []byte("aud-bytes"))
	if err := a.memory.PutMemoryBlocks([]memory.MemoryBlock{img, aud}); err != nil {
		t.Fatal(err)
	}

	a.multimodalSpace = fakeSpace{}
	a.reembedStaleMedia()

	// 图像块应拿到本空间向量（fp 写回，dim=2）
	imgGot, ok, err := a.memory.BlockByIDPrefix(img.ID)
	if err != nil || !ok {
		t.Fatalf("BlockByIDPrefix(img): %v / %v", ok, err)
	}
	if len(imgGot.Vector) != 2 || imgGot.Fingerprint != "fake-space" {
		t.Fatalf("图像应拿到本空间向量，实际 vec=%v model=%q", imgGot.Vector, imgGot.Fingerprint)
	}

	// 音频块不得被写入任何向量（不能用别的模型顶替），fp 应保持为空
	audGot, ok, err := a.memory.BlockByIDPrefix(aud.ID)
	if err != nil || !ok {
		t.Fatalf("BlockByIDPrefix(aud): %v / %v", ok, err)
	}
	if len(audGot.Vector) != 0 || audGot.Fingerprint != "" {
		t.Fatalf("音频不得被写入任何向量（不能用别的模型顶替），实际 vec=%v model=%q",
			audGot.Vector, audGot.Fingerprint)
	}
}

// 无多模态空间可用时迁移路径必须整体跳过，不报错也不写脏向量。
func TestReembedStaleMedia_NilSpaceNoop(t *testing.T) {
	a := newMediaLoopAgent(t)
	b := mediaBlock("image/png", []byte("x"))
	if err := a.memory.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
		t.Fatal(err)
	}
	// multimodalSpace 为 nil：整体跳过
	a.multimodalSpace = nil
	a.reembedStaleMedia()
	got, _, _ := a.memory.BlockByIDPrefix(b.ID)
	if len(got.Vector) != 0 || got.Fingerprint != "" {
		t.Errorf("无空间可用时不该写向量: vec=%v model=%q", got.Vector, got.Fingerprint)
	}
}
