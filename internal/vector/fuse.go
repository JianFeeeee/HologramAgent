package vector

import "math"

// FuseVectors 把同一统一空间里的多个向量融合为一个向量：
// 逐维求和后重新 L2 归一化。
//
// 用途：文档/上下文事件既带文本、又带若干一等记忆块（图片/视频），
// 二者的向量来自同一模型、同一 fingerprint、同一维度。融合后，
// 一篇文档既能按文字、也能按它携带的图片内容被召回——
// 图片由自己的向量参与检索，不依赖任何生成的描述文本。
//
// 约定：调用方传入的向量应已是 L2 归一化的同空间向量。长度不一致的
// 向量会被跳过（不同模型/维度的残留）；全空或全零返回 nil。
func FuseVectors(vectors ...[]float64) []float64 {
	dim := 0
	for _, v := range vectors {
		if len(v) > dim {
			dim = len(v)
		}
	}
	if dim == 0 {
		return nil
	}
	out := make([]float64, dim)
	used := 0
	for _, v := range vectors {
		if len(v) != dim {
			continue
		}
		for i, x := range v {
			out[i] += x
		}
		used++
	}
	if used == 0 {
		return nil
	}
	var norm float64
	for _, x := range out {
		norm += x * x
	}
	if norm == 0 {
		return nil
	}
	norm = math.Sqrt(norm)
	for i := range out {
		out[i] /= norm
	}
	return out
}
