package mainagent

// dense_adapter.go —— 把基座的**稠密语义计算 syscall** 包成本类型自己的
// `vector.MultimodalEmbedder`，供 ctx / indexer / coldcontext 三个实现使用。
//
// ## 为什么必须有这一层
//
// 那三个实现要的是 `vector.MultimodalEmbedder`（`VectorizeDense` /
// `EmbedImageDense` / `Fingerprint` / `Dim` / `Loaded` / `Close`）。
// 而**基座不能把内核对象交给类型**（docs §0.1 结论 12）：主 agent 拿不到
// `kernelapi.MultimodalSpace`，只能调 `EmbedTextsDense` / `ImageDense` /
// `DenseFingerprint` / `DenseDim` 这几个 syscall。
//
// 于是适配器住在**类型侧**：它自己不持有任何内核对象，每个方法都只是
// 一次 syscall 转发。资产（模型、ONNX 会话）仍只有内核那一份。
//
// ★ 为什么不是"基座导出空间对象"：那样换 provider 就要回头改类型，
// 而且类型的生命周期（Close）会作用到内核共享的那一份。

import "github.com/JianFeeeee/HologramAgent/internal/agent/agent"

// denseAdapter 实现 `vector.MultimodalEmbedder`（本类型私有）。
type denseAdapter struct {
	a *agent.Agent
}

// DenseCall 返回本类型的稠密空间适配器（未挂载 ⇒ nil）。
//
// 装配期由 installState 调一次；运行期三个实现各自持有它。
func (m *Module) DenseCall() *denseAdapter {
	if m == nil || m.a == nil || m.a.DenseFingerprint() == "" {
		return nil
	}
	return &denseAdapter{a: m.a}
}

// VectorizeDense 单句 → 稠密向量（会话语料里的"一次一句"路径）。
func (d *denseAdapter) VectorizeDense(text string) ([]float64, error) {
	if d == nil || d.a == nil {
		return nil, errDenseUnavailable
	}
	vecs, ok := d.a.EmbedTextsDense([]string{text})
	if !ok || len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, errDenseUnavailable
	}
	return vecs[0], nil
}

// VectorizeDenseBatch 批量句 → 稠密向量（docs §3.4：一 call 多发）。
//
// ★ 这是**新增**于接口之外的批量能力：`vector.MultimodalEmbedder` 只有
// 单发方法，但 L1 事件进上下文、候选页剔除、冷文档重建索引全都是批量场景。
// 调用方按类型断言取它（`interface{ VectorizeDenseBatch([]string) ([][]float64, bool) }`），
// 拿不到就退单发 —— 接口不被迫改，批量能力仍然可用。
func (d *denseAdapter) VectorizeDenseBatch(texts []string) ([][]float64, bool) {
	if d == nil || d.a == nil {
		return nil, false
	}
	return d.a.EmbedTextsDense(texts)
}

// EmbedImageDense 单图 → 稠密向量。
func (d *denseAdapter) EmbedImageDense(img []byte, mime string) ([]float64, error) {
	if d == nil || d.a == nil {
		return nil, errDenseUnavailable
	}
	v, ok := d.a.ImageDense(img, mime)
	if !ok {
		return nil, errDenseUnsupported
	}
	return v, nil
}

// Fingerprint 是空间指纹（类型用它做**空间隔离**判定）。
func (d *denseAdapter) Fingerprint() string {
	if d == nil || d.a == nil {
		return ""
	}
	return d.a.DenseFingerprint()
}

// Dim 返回空间维度。
func (d *denseAdapter) Dim() int {
	if d == nil || d.a == nil {
		return 0
	}
	return d.a.DenseDim()
}

// Loaded 报告空间是否可用。
func (d *denseAdapter) Loaded() bool { return d.Fingerprint() != "" }

// Close 是**空操作**。
//
// ★ 刻意不转调内核空间的 Close：那份空间是全进程共享的一份（多个实例、
// 多个类型都在用）。类型"自己的"关闭动作不成立 —— 它不拥有它。
func (d *denseAdapter) Close() {}

// 两个错误：区分"空间没配好"与"这个 provider 不做图像"。
var (
	errDenseUnavailable = denseErr("dense space unavailable")
	errDenseUnsupported = denseErr("dense space does not support this modality")
)

type denseErr string

func (e denseErr) Error() string { return string(e) }
