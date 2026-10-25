package memory

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// 中心向量在库里的持久化。
//
// 为什么必须持久化（而不是每次查询现算）
// ----------------------------------------
// ① 现算要扫全库所有块向量：O(n·d)。实测 260 块 × 512 维成本可忽略，
//    但生产库是 1277 块、上万块时会变成每次查询的固定开销。
// ② 更要紧的是**稳定性**：现算的结果依赖"查询那一刻的块集合"，
//    库一变，同一个查询的相似度就变 —— 那会让"上周的召回结果"
//    不可复现，而召回结果是要进 benchmark 的。
//
// 失效条件（写进表里一起存）
// ----------------------------
//  维度变（换 provider）        → 均值的定义域变了
//  块数变（新增/删除块）        → 均值随样本变，但**不是所有变化都要重算**
//  指纹变（换 embedding 空间）  → 同上
//
// 为什么块数变不总是要重算：中心是对"块的分布"的估计，
// 少量新增不影响它的方向；而频繁重算会让相似度漂移。
// 所以用**相对变化率**：新增/删除超过 20% 才判定失效。

// centroidMeta 是中心向量的元信息（存在 kv 表里）。
type centroidMeta struct {
	// Fingerprint 是这个中心所属的向量空间指纹。
	Fingerprint string `json:"fingerprint"`
	// Dim 是向量维度。
	Dim int `json:"dim"`
	// BlockCount 是建中心时的块数。
	BlockCount int `json:"block_count"`
	// VectoredCount 是建中心时**带向量**的块数（分母用这个，不是总块数）。
	VectoredCount int `json:"vectored_count"`
	// MeanLength 是均值范数（诊断用，Anomalous 的依据）。
	MeanLength float64 `json:"mean_length"`
	// Anomalous 是建中心时的判定结果。
	Anomalous bool `json:"anomalous"`
	// BuiltAt 是建中心时刻。
	BuiltAt time.Time `json:"built_at"`
}

// centroidRebuildRatio 是触发重建的块数相对变化率。
//
// 20% 的依据：中心是分布的估计，10% 以内的变化对方向影响可忽略；
// 而过于敏感会让相似度随每次写入漂移（那比不校正更糟 ——
// 不可复现的召回无法用于回归测试）。
const centroidRebuildRatio = 0.20

// ddlCentroid 是中心向量的表与索引。
const ddlCentroid = `CREATE TABLE IF NOT EXISTS graph_centroid (
	fingerprint   TEXT PRIMARY KEY,
	dim           INTEGER NOT NULL,
	vector_count  INTEGER NOT NULL,
	vector        TEXT NOT NULL,
	stats         TEXT NOT NULL,
	built_at      TIMESTAMP DEFAULT CURRENT_TIMESTAMP
)`

// centroidKVKey 是元信息在 kv 表里的键。
const centroidKVKey = "graph_centroid_meta"

// ensureCentroidSchema 建表。调用方必须持写锁或在初始化里做一次。
func (g *GraphDB) ensureCentroidSchema() error {
	if _, err := g.db.Exec(ddlCentroid); err != nil {
		return fmt.Errorf("graph: create graph_centroid: %w", err)
	}
	return nil
}

// SaveCentroid 存中心向量及其元信息。
//
// 不校验 anomalous —— 上层决定要不要存。存了但 anomalous=false 时，
// LoadCentroid 会返回 (centroid, false, nil)，调用方据此跳过校正。
func (g *GraphDB) SaveCentroid(c *Centroid, fingerprint string) error {
	if c == nil || c.Count == 0 {
		return fmt.Errorf("graph: empty centroid")
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	if err := g.ensureCentroidSchema(); err != nil {
		return err
	}
	stats := c.Stats()
	meta := centroidMeta{
		Fingerprint: fingerprint,
		Dim:         c.Dim,
		BlockCount:  g.countBlocksLocked(),
		// ★ 语义定死：VectoredCount = **参与统计的向量数**（c.Count），
		// 不是「库里当时的块数」。
		//
		// 理由：失效判定要回答的问题是「这个中心还能不能代表当前库的
		// 分布」，而中心是 c.Count 个向量的均值。比较的基准必须是
		// 同一批向量 —— 拿「库当时的块数」会出现两种错法：
		//   库里有无向量的块（维度不匹配/指纹不同被跳过）→ 两者不等 → 误判失效
		//   库当时为空 → 基准为 0 → 除零
		VectoredCount: c.Count,
		MeanLength:    stats.MeanLength,
		Anomalous:     stats.Anomalous,
		BuiltAt:       time.Now(),
	}
	rawStats, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	mean := c.Mean()
	rawMean, err := json.Marshal(mean)
	if err != nil {
		return err
	}
	// vector_count 列存的是 meta.VectoredCount（建中心时刻库里的带向量块数），
	// 与失效判定同源 —— 若这里存 c.Count（参与统计的向量数），两者在
	// 「外部喂向量给 SaveCentroid」时不相等，会误判失效。
	if _, err := g.db.Exec(`INSERT INTO graph_centroid
		(fingerprint, dim, vector_count, vector, stats, built_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(fingerprint) DO UPDATE SET
			dim = excluded.dim,
			vector_count = excluded.vector_count,
			vector = excluded.vector,
			stats = excluded.stats,
			built_at = excluded.built_at`,
		fingerprint, c.Dim, meta.VectoredCount, string(rawMean),
		string(rawStats), meta.BuiltAt); err != nil {
		return fmt.Errorf("graph: save centroid: %w", err)
	}
	log.Printf("[graph] centroid stored: fp=%s dim=%d samples=%d mean_norm=%.4f anomalous=%v",
		shortFP(fingerprint), c.Dim, c.Count, stats.MeanLength, stats.Anomalous)
	return nil
}

// LoadCentroid 读中心向量。返回的 bool 是"应当启用校正"。
//
// 失效判定见文件头。失效时返回 (nil, false, nil) —— 不是错误：
// 「中心还没建」或「该重建」都是正常状态，调用方应跳过校正。
func (g *GraphDB) LoadCentroid(fingerprint string, queryDim int) (*Centroid, bool, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var dim, vecCount int
	var rawMean, rawStats string
	err := g.db.QueryRow(`SELECT dim, vector_count, vector, stats
		FROM graph_centroid WHERE fingerprint = ?`, fingerprint).
		Scan(&dim, &vecCount, &rawMean, &rawStats)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("graph: load centroid: %w", err)
	}
	var meta centroidMeta
	if err := json.Unmarshal([]byte(rawStats), &meta); err != nil {
		return nil, false, fmt.Errorf("graph: parse centroid meta: %w", err)
	}
	// 维度校验：与查询向量维度不符就不能用（中心化会按错误的维度截断/越界）。
	// queryDim <= 0 表示调用方不校验（罕见），此时只查库里的元数据是否自洽。
	if dim <= 0 {
		return nil, false, fmt.Errorf("graph: centroid dim is %d (corrupted?)", dim)
	}
	if queryDim > 0 && dim != queryDim {
		return nil, false, nil
	}
	// 库里的带向量块数 vs 建中心时的样本数 → 相对变化率
	//
	// 两侧不完全相等是正常的（召回时会跳过维度/指纹不匹配的块），
	// 所以判据是「变化率超过 20%」而不是「不相等」。
	// 20% 的依据：中心是分布的估计，10% 以内的变化对方向影响可忽略；
	// 而过于敏感会让相似度随每次写入漂移 —— 那比不校正更糟，
	// 因为不可复现的召回无法用于回归测试。
	curVectored := g.countVectoredBlocksLocked()
	if vecCount <= 0 {
		// 没有统计过任何向量：这不是一个可用的中心。
		if curVectored > 0 {
			log.Printf("[graph] centroid unavailable: built from 0 samples, store now has %d vectored blocks", curVectored)
		}
		return nil, false, nil
	}
	delta := float64(curVectored-vecCount) / float64(vecCount)
	if delta < 0 {
		delta = -delta
	}
	if delta > centroidRebuildRatio {
		log.Printf("[graph] centroid invalidated: samples %d -> store %d (change %.0f%% > %.0f%%)",
			vecCount, curVectored, delta*100, centroidRebuildRatio*100)
		return nil, false, nil
	}
	if !meta.Anomalous {
		// 建的时候判定为各向同性 ⇒ 不启用
		return nil, false, nil
	}
	var mean []float64
	if err := json.Unmarshal([]byte(rawMean), &mean); err != nil {
		return nil, false, fmt.Errorf("graph: parse centroid vector: %w", err)
	}
	if len(mean) != dim {
		return nil, false, fmt.Errorf("graph: centroid dim mismatch: stored %d, meta %d", len(mean), dim)
	}
	// Count 复原：均值已归一化不成，Sum = mean（相对 Scale 无意义，
	// 因此 CenterVector 只用 Mean()，不依赖 Count）。
	c := NewCentroid(dim)
	copy(c.Sum, mean)
	// 把 Sum 转成「均值恰好等于 mean」的 Sum：Mean() = Sum/Count，
	// 所以令 Sum = mean * syntheticCount，Count = syntheticCount。
	// 取 syntheticCount=1 最直接：Sum=mean, Count=1。
	c.Count = 1
	return c, true, nil
}

// countBlocksLocked 返回块总数。调用方必须持锁。
func (g *GraphDB) countBlocksLocked() int {
	var n int
	_ = g.db.QueryRow(`SELECT COUNT(*) FROM memory_blocks`).Scan(&n)
	return n
}

// countVectoredBlocksLocked 返回带向量的块数。调用方必须持锁。
func (g *GraphDB) countVectoredBlocksLocked() int {
	var n int
	_ = g.db.QueryRow(`SELECT COUNT(*) FROM memory_blocks
		WHERE vector IS NOT NULL AND vector != '' AND vector != 'null'`).Scan(&n)
	return n
}

// RebuildCentroid 从库里所有带向量的块重建中心。
//
// fingerprint 为空时从块的 fingerprint 字段推断（取多数派）——
// 库里混着多个空间时不能瞎猜，宁可要求调用方显式给出。
func (g *GraphDB) RebuildCentroid(fingerprint string) (*Centroid, bool, error) {
	g.mu.RLock()
	rows, err := g.db.Query(`SELECT vector, fingerprint FROM memory_blocks
		WHERE vector IS NOT NULL AND vector != '' AND vector != 'null'`)
	if err != nil {
		g.mu.RUnlock()
		return nil, false, err
	}
	type row struct {
		vec []float64
		fp  string
	}
	var all []row
	var dim int
	for rows.Next() {
		var raw, fp string
		if err := rows.Scan(&raw, &fp); err != nil {
			rows.Close()
			g.mu.RUnlock()
			return nil, false, err
		}
		v, err := decodeVector(raw)
		if err != nil {
			continue
		}
		if dim == 0 {
			dim = len(v)
		}
		if len(v) != dim {
			continue // 维度不同（混了 provider）→ 跳过
		}
		if fingerprint != "" && fp != "" && fp != fingerprint {
			continue
		}
		all = append(all, row{vec: v, fp: fp})
	}
	rows.Close()
	g.mu.RUnlock()

	if len(all) == 0 {
		return nil, false, fmt.Errorf("graph: no vectored blocks to build centroid")
	}
	c := NewCentroid(dim)
	for _, r := range all {
		c.Add(r.vec)
	}
	// fingerprint 为空且库里指纹不唯一 → 拒绝
	if fingerprint == "" {
		uniq := map[string]bool{}
		for _, r := range all {
			uniq[r.fp] = true
		}
		if len(uniq) > 1 {
			return c, false, fmt.Errorf(
				"graph: centroid fingerprint ambiguous (%d spaces present); pass it explicitly",
				len(uniq))
		}
		for fp := range uniq {
			fingerprint = fp
		}
	}
	if err := g.SaveCentroid(c, fingerprint); err != nil {
		return c, false, err
	}
	return c, c.Stats().Anomalous, nil
}

// CentroidStatus 供运维/CLI 报告中心状态。
type CentroidStatus struct {
	Saved         bool    `json:"saved"`
	Fingerprint   string  `json:"fingerprint"`
	Dim           int     `json:"dim"`
	VectoredCount int     `json:"vectored_count"`
	CurrentBlocks int     `json:"current_vectored_blocks"`
	MeanLength    float64 `json:"mean_length"`
	AvgPairCosine float64 `json:"avg_pair_cosine"`
	Anomalous     bool    `json:"anomalous"`
	Stale         bool    `json:"stale"`
	BuiltAt       string  `json:"built_at,omitempty"`
}

// DominantBlockFingerprint 返回库内带向量块中出现最多的 provider 指纹。
//
// 独立重建中心时用它代替「用户手输指纹」—— 运维不该去翻配置找那串 hex。
// 库内混了多个 provider 时（迁移期换过模型），取数量最多的那个，
// 并把实际取值打印出来让用户确认。
func (g *GraphDB) DominantBlockFingerprint() (string, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	rows, err := g.db.Query(`SELECT fingerprint, COUNT(*) AS n
		FROM memory_blocks
		WHERE vector IS NOT NULL AND vector != '' AND fingerprint IS NOT NULL
		GROUP BY fingerprint ORDER BY n DESC LIMIT 1`)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return "", nil
	}
	var fp string
	var n int
	if err := rows.Scan(&fp, &n); err != nil {
		return "", err
	}
	return fp, nil
}

// CentroidStatusOf 报告中心状态，不启用校正。
func (g *GraphDB) CentroidStatusOf(fingerprint string) (CentroidStatus, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var st CentroidStatus
	st.Fingerprint = fingerprint
	st.CurrentBlocks = g.countVectoredBlocksLocked()

	var dim, vecCount int
	var rawStats string
	var builtAt time.Time
	err := g.db.QueryRow(`SELECT dim, vector_count, stats, built_at
		FROM graph_centroid WHERE fingerprint = ?`, fingerprint).
		Scan(&dim, &vecCount, &rawStats, &builtAt)
	if err == sql.ErrNoRows {
		return st, nil // Saved=false
	}
	if err != nil {
		return st, err
	}
	var meta centroidMeta
	if err := json.Unmarshal([]byte(rawStats), &meta); err != nil {
		return st, err
	}
	st.Saved = true
	st.Dim = dim
	st.VectoredCount = vecCount
	st.MeanLength = meta.MeanLength
	st.Anomalous = meta.Anomalous
	st.AvgPairCosine = meta.MeanLength * meta.MeanLength
	st.BuiltAt = builtAt.Format(time.RFC3339)
	if vecCount > 0 {
		delta := float64(st.CurrentBlocks-vecCount) / float64(vecCount)
		if delta < 0 {
			delta = -delta
		}
		st.Stale = delta > centroidRebuildRatio
	} else {
		st.Stale = st.CurrentBlocks > 0
	}
	return st, nil
}

// centerBoth 把查询向量与块向量都中心化后重算余弦。
//
// ★ 两边都必须中心化：中心化是"相对于这个库的公共方向做正交化"，
//
//	只处理一边的话结果没有意义 —— 一边带公共分量、另一边不带，
//	算出来的余弦是两种量纲的混合。
func centerBoth(c *Centroid, blockVec, queryVec []float64) float64 {
	if c == nil {
		return cosine(blockVec, queryVec)
	}
	cb := c.CenterVector(blockVec)
	cq := c.CenterVector(queryVec)
	return cosine(cb, cq)
}
