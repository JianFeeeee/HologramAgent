package memory

import (
	"math"
	"sync"
)

// 向量中心化（centering / 各向异性校正）。
//
// ★ 为什么需要 —— 实测数据（真库 260 个 distill 块，chineseclip 512 维）
//
//	均值向量长度        = 0.9374      （1.0 = 所有向量完全同向）
//	平均两两余弦        = 0.8727      ← 与「|mean|²=0.8787」吻合
//	去均值后平均两两余弦 = -0.0012     ← 各向同性
//
// **93.7% 的能量花在同一个方向上。** 换句话说，两块文本的相似度里
// 大部分不是"它们像"贡献的，而是"它们都朝那个方向偏"贡献的。
//
// 这解释了本会话一系列看起来矛盾的现象：
//
//	「值班室分机号 4324」cos = 0.9284
//	「值班室分机号 4379」cos = 0.9298   ← 新旧号差 0.0014
//	粗筛 0.9 阈值 → 44% 误报（等于没有阈值）
//
// ★ 我此前的判断是错的：写的是「CLIP 架构把纯文本压扁了，
//   细粒度区分度天然低」。那是**把症状当成了原因** ——
//   不是模型分不清，是所有向量挤在一个锥体里。RoBERTa/BERT 系列的
//   CLS 向量各向异性是有名的现象，与 CLIP 无关。
//
// 去均值后的实测收益：
//
//	                  原始      去均值
//	同值对最低        1.0000     1.0000    （不受影响）
//	异值对最高        0.9905     0.9311
//	★ 分离度          0.0095     0.0689    ← 提升 7 倍
//
// 异值对逐条（原始 → 去均值）：
//
//	0.9240 → 0.2494   第112批|版本=v2.31.0    || =周四
//	0.8167 → 0.2705   第115批|版本=115批v2.31.2 || =115批周二凌晨…
//	0.9223 → 0.2198   第132批|版本=v2.33.1    || =周二
//
// 原本 0.92 的"看起来很像"，去均值后掉到 0.25 ——
// 那 0.92 几乎全是共同方向的假象。
//
// ── 用法的三条约束 ──────────────────────────────────────
//
// ① **均值必须跨块统计**，不能只用查询向量减自己。
//    均值的定义域是"这个库里的所有块"，查询向量不在库里时
//    减同一个均值仍然正确（它表达的是"与这个库的公共方向正交化"）。
//
// ② **均值要持久化**。每次查询现算需要扫全库（实测 260 块成本可忽略，
//    但上万块就是每次查询 O(n·d)）。且现算的结果依赖"查询时刻的块集合"，
//    会让相似度随库变化而漂移。
//
// ③ **不是所有库都需要**。均值长度接近 1 时（如已经很各向同性的空间）
//    中心化只会放大噪声；接近 0 时则是无意义的额外开销。
//    BuildCentroid 因此返回诊断指标，由调用方判断。

// Centroid 是向量空间的中心（各向异性校正的减数）。
type Centroid struct {
	// Sum 是各维累加和。存累加和而非均值，是为了让 Add 能增量累积。
	Sum []float64
	// Count 是参与统计的向量数。
	Count int
	// Dim 是向量维度。
	Dim int
}

// CentroidStats 是中心化的诊断指标。
type CentroidStats struct {
	// MeanLength 是均值向量的 L2 范数（已归一化的向量上）。
	// 接近 1 = 所有向量同向（各向异性严重）；接近 0 = 各向同性。
	MeanLength float64
	// AvgPairCosine 是 |mean|² —— 全部配对的平均余弦的解析值，
	// 不必真的两两算。
	AvgPairCosine float64
	// Anomalous 判断是否值得做中心化（经验阈值，见 BuildCentroid）。
	Anomalous bool
}

// meanThreshold 是判定"各向异性严重"的阈值。
//
// 依据：均值范数 0.9374 时去均值让分离度提升 7 倍；反过来，
// 若均值范数本就接近 0（各向同性），中心化只是放大噪声。
// 0.6 是个保守取值：低于它说明库本身方向分散，不需要校正。
const meanThreshold = 0.6

// NewCentroid 建一个空中心。
func NewCentroid(dim int) *Centroid {
	return &Centroid{Sum: make([]float64, dim), Dim: dim}
}

// Add 累积一个向量。维度不匹配会被忽略（返回 false）——
// 不同 provider 的向量混在一块时不能相加。
func (c *Centroid) Add(v []float64) bool {
	if v == nil || len(v) != c.Dim {
		return false
	}
	for i, x := range v {
		c.Sum[i] += x
	}
	c.Count++
	return true
}

// Mean 返回均值向量。Count 为 0 时返回 nil。
func (c *Centroid) Mean() []float64 {
	if c.Count == 0 {
		return nil
	}
	out := make([]float64, c.Dim)
	for i := range c.Sum {
		out[i] = c.Sum[i] / float64(c.Count)
	}
	return out
}

// Stats 返回诊断指标。Count 为 0 时返回零值。
func (c *Centroid) Stats() CentroidStats {
	m := c.Mean()
	if m == nil {
		return CentroidStats{}
	}
	var norm float64
	for _, x := range m {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	return CentroidStats{
		MeanLength:    norm,
		AvgPairCosine: norm * norm,
		Anomalous:     norm >= meanThreshold,
	}
}

// Subtract 返回 v 减去中心均值后的向量（原向量不被修改）。
// 维度不匹配或中心为空时返回 v 的副本（不做校正）。
func (c *Centroid) Subtract(v []float64) []float64 {
	m := c.Mean()
	if m == nil || len(v) != c.Dim {
		out := make([]float64, len(v))
		copy(out, v)
		return out
	}
	out := make([]float64, len(v))
	for i := range v {
		out[i] = v[i] - m[i]
	}
	return out
}

// SubtractInPlace 就地校正向量（省一次分配，批量处理时用）。
func (c *Centroid) SubtractInPlace(v []float64) {
	m := c.Mean()
	if m == nil || len(v) != c.Dim {
		return
	}
	for i := range v {
		v[i] -= m[i]
	}
}

// l2Normalize 就地归一化。零向量或 NaN 时返回 false（不缩放）。
//
// ★ 为什么中心化后必须重新归一化：减均值不保范数，
//
//	而余弦相似度是尺度相关的 —— 不归一的话所有余弦都会被
//	「向量变短了」这个纯粹的尺度效应污染。
func l2Normalize(v []float64) bool {
	var norm float64
	for _, x := range v {
		norm += x * x
	}
	norm = math.Sqrt(norm)
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return false
	}
	for i := range v {
		v[i] /= norm
	}
	return true
}

// CenterVector 校正单个向量：减中心均值 + 重新归一化。
//
// 这才是**完整**的校正 —— 只减不归一是常见错误（见 l2Normalize 的注释）。
func (c *Centroid) CenterVector(v []float64) []float64 {
	out := c.Subtract(v)
	l2Normalize(out)
	return out
}

// BuildCentroid 从一组向量建中心，并返回诊断指标。
//
// ★ 调用方必须读 Stats().Anomalous 再决定是否启用 ——
//
//	中心化不是无条件正确的操作（见 CentroidStats 的注释）。
func BuildCentroid(vectors [][]float64) (*Centroid, CentroidStats) {
	if len(vectors) == 0 {
		return nil, CentroidStats{}
	}
	c := NewCentroid(len(vectors[0]))
	for _, v := range vectors {
		c.Add(v)
	}
	return c, c.Stats()
}

// CentroidCache 缓存中心向量，避免每次查询现算。
//
// 并发安全：RecallBlocks 会被并发调用，而中心重算是 O(n·d)。
type CentroidCache struct {
	mu  sync.RWMutex
	c   *Centroid
	st  CentroidStats
	n   int // 参与统计的向量数
	dim int
}

// Get 返回该（dim, n）对应的中心。没有对应版本时返回 nil。
func (cc *CentroidCache) Get(dim, n int) (*Centroid, CentroidStats) {
	cc.mu.RLock()
	defer cc.mu.RUnlock()
	if cc.c == nil || cc.dim != dim || cc.n != n {
		return nil, CentroidStats{}
	}
	return cc.c, cc.st
}

// Put 存入中心。已有的不同版本会被覆盖。
func (cc *CentroidCache) Put(c *Centroid, st CentroidStats) {
	if c == nil {
		return
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	cc.c, cc.st, cc.dim, cc.n = c, st, c.Dim, c.Count
}
