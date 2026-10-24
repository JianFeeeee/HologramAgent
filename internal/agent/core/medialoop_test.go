package core

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
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

// failSpace 让指定 mime 的图像嵌入失败，用来验证「失败必须留下原因」。
type failSpace struct {
	failMIME string
}

func (failSpace) VectorizeDense(string) ([]float64, error) { return []float64{1, 0}, nil }

func (f failSpace) EmbedImageDense(_ []byte, mime string) ([]float64, error) {
	if mime == f.failMIME {
		return nil, fmt.Errorf("模拟解码失败: %s", mime)
	}
	return []float64{1, 0}, nil
}

func (failSpace) Fingerprint() string { return "fail-space" }
func (failSpace) Dim() int            { return 2 }
func (failSpace) Loaded() bool        { return true }
func (failSpace) Close()              {}

// 迁移失败必须留下**可诊断的原因**，而不只是一个计数。
//
// ★ 2026-10-23 生产事故：日志只有
//
//	[media] 向量迁移完成: 成功=0 失败=4 总计=4
//
// 没有任何信息说明为什么。反查数据库才发现那 4 个块是 image/webp，
// 而 provider 没注册 webp 解码器。**失败不可观测直接导致排查成本翻倍**。
//
// 本判据钉住：失败原因按 "mime | error" 聚合后必须出现在日志里。
//
// 变异：把 failReasons 的采集与打印删掉 ⇒ 红。
func TestReembedStaleMedia_LogsFailureReasons(t *testing.T) {
	a := newMediaLoopAgent(t)
	// 两个都会失败（同一 mime）—— 应聚合成一条 "2 个: ..."
	for i := 0; i < 2; i++ {
		b := mediaBlock("image/webp", []byte("webp-bytes"))
		if err := a.memory.PutMemoryBlocks([]memory.MemoryBlock{b}); err != nil {
			t.Fatal(err)
		}
	}

	logs := captureLog(t, func() {
		a.multimodalSpace = failSpace{failMIME: "image/webp"}
		a.reembedStaleMedia()
	})

	if !strings.Contains(logs, "失败原因分类") {
		t.Fatalf("迁移失败时必须打印失败原因分类，实际日志:\n%s", logs)
	}
	if !strings.Contains(logs, "image/webp") {
		t.Fatalf("失败原因里必须出现具体 mime（否则无从定位是哪种格式），实际日志:\n%s", logs)
	}
	if !strings.Contains(logs, "模拟解码失败") {
		t.Fatalf("失败原因里必须带上 provider 返回的错误原文，实际日志:\n%s", logs)
	}
	// 计数必须出现（格式 "N 个: ..."）—— 具体数字取决于去重后的块数，
	// 这里只要求"有计数"，不绑死数字（绑死会让判据随去重策略变化而脆）。
	if !regexp.MustCompile(`\d+ 个: image/webp`).MatchString(logs) {
		t.Fatalf("失败原因应带聚合计数（形如「N 个: image/webp | ...」），实际日志:\n%s", logs)
	}
}

// 不支持模态与临时失败必须**分开计数**：前者永远好不了（重试无意义），
// 混在"失败"里会让人误以为重试即可恢复。
func TestReembedStaleMedia_SeparatesUnsupportedFromFailed(t *testing.T) {
	a := newMediaLoopAgent(t)
	// 音频走 fakeSpace 的 ErrModalityUnsupported 分支；但 reembedStaleMedia
	// 只挑 image 模态，所以用图像 mime + 让 provider 返回 unsupported 来验证分类。
	img := mediaBlock("image/tiff", []byte("tiff-bytes"))
	if err := a.memory.PutMemoryBlocks([]memory.MemoryBlock{img}); err != nil {
		t.Fatal(err)
	}

	logs := captureLog(t, func() {
		a.multimodalSpace = unsupportedImageSpace{}
		a.reembedStaleMedia()
	})

	if !strings.Contains(logs, "不支持=1") {
		t.Fatalf("不受支持的模态应单独计数（不支持=1），实际日志:\n%s", logs)
	}
}

// unsupportedImageSpace 对任何图像都返回 ErrModalityUnsupported。
type unsupportedImageSpace struct{}

func (unsupportedImageSpace) VectorizeDense(string) ([]float64, error) { return []float64{1, 0}, nil }

func (unsupportedImageSpace) EmbedImageDense(_ []byte, mime string) ([]float64, error) {
	return nil, fmt.Errorf("%w: %s", vector.ErrModalityUnsupported, mime)
}

func (unsupportedImageSpace) Fingerprint() string { return "unsupported-space" }
func (unsupportedImageSpace) Dim() int            { return 2 }
func (unsupportedImageSpace) Loaded() bool        { return true }
func (unsupportedImageSpace) Close()              {}
