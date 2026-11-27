package semantic

// clean_text.go —— 向量化前的文本清洗与两个余弦（纯计算，零状态）。

import (
	"math"
	"strings"
)

// CleanText 是向量化前的文本清洗（去首尾标点与空白）。
//
// ★ 规则必须与调用侧**逐字一致**：清洗不一致时，「同一段文本」在两个
// 地方算出不同向量，相似度没有任何意义。
func CleanText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	text = strings.TrimPrefix(text, "，")
	text = strings.TrimPrefix(text, "，")
	return strings.TrimSpace(text)
}

// SparseVector 是稀疏特征向量（feature → weight）。
//
// ★ 刻意是 `map[string]float64`：这是**跨包契约形状**，不是某个实现包的
// 命名类型。内核设施、基座 syscall、类型三者都用它，不必为了类型转换
// 而互相 import。
type SparseVector = map[string]float64

// Cosine 计算两个稀疏向量的余弦相似度（任一为零向量 ⇒ 0）。
func Cosine(a, b SparseVector) float64 {
	var dot, normA, normB float64
	for f, va := range a {
		dot += va * b[f]
		normA += va * va
	}
	for _, vb := range b {
		normB += vb * vb
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// DenseCosine 计算两个稠密向量的余弦相似度。
//
// 长度不等 ⇒ 0（不是报错）：长度不等意味着**两个不同的向量空间**，
// 比值没有任何意义，静默给 0 是唯一正确的行为。
func DenseCosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}
