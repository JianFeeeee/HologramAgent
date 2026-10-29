package memory

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"time"
)

// 块节点的向量召回。
//
// 为什么需要它：entities 是旧形态且正在退场，而 memory_blocks 才是带
// vector+fingerprint 的节点载体（46f833c 引入）。此前唯一有向量的只有
// memory_blocks，但**没有任何召回路径读它** —— BlocksForNode 只能按
// 端点反查，必须先知道 nodeID。于是「问一个具体问题」时只能退到
// entities 的 jieba+LIKE，实测跨维度定位 0/5。
//
// 本文件补的就是这条缺失的路径：query 向量 → 块向量 → 按余弦排序。
//
// 与 vector.Store 的分工：Store 是内存倒排索引（L0/L1 用），块向量存在
// SQLite（graph.db），生命周期与图一致、可跨进程重启、可被备份脚本覆盖。
//
// 全表扫描的代价（**已量化，不是估算**）：生产库迁移后约 1277 块 × 2048 维
// = 21 MB 读入 + 2.6M 次乘加，每次 memory_recall 一次。当前可接受；
// 块数上到万级时需要换方案（候选：把向量搬进 vector.Store 的内存倒排，
// 或按 modality/scene 预筛减少扫描量）。在此之前不做优化 —— 没有真实
// 规模数据时优化是猜，本会话已经因此浪费过几轮。

// BlockHit 是一条块召回的候选及相似度。
type BlockHit struct {
	Block MemoryBlock `json:"block"`
	Score float64     `json:"score"`
}

// BlockRecallQuery 描述一次块召回请求。
type BlockRecallQuery struct {
	Vector []float64
	// Fingerprint 是发起方所用向量空间的指纹。与库中块不一致的块会被
	// **跳过**（不是报错）：库里可能同时存在两个空间的块（迁移期），
	// 跳过才是正确的——拿新空间的查询向量去比旧空间的块向量，余弦值
	// 没有任何意义，而且不会报错，只是"结果看起来还行但全是错的"。
	Fingerprint string
	TopK        int
	// MinScore 低于此分数的候选被丢弃（0 表示不设下限）。
	MinScore float64
	// Modality 非空时只召回该模态的块（空 = 不限）。
	//
	// ★ 为何需要它（实测回归）：CAS 时代视觉路查 `FROM media`（只含媒体），
	//   结构上不可能被文本块挤掉。块 union 化后视觉路改用本接口（全局
	//   top-K，含数千文本块），而实测同一个 chineseclip 空间里：
	//       查询「图片」→ 图像块 max cosine 0.377，文本块 max 1.000
	//       查询「照片」→ 图像块 max 0.385，文本块 max 0.935
	//   图片的绝对相似度天然低于字面相同的文本，per=200 时视觉路返回
	//   1000 条里**图像块 0 条** —— 视觉路被彻底饿死。
	//   所以“取全局 top-K 再过滤掉文本块”是错的取法：过滤发生在截断之后。
	//   必须在 SQL 层就限定模态，让 top-K 在目标模态内部竞争。
	Modality BlockModality
	// ExcludeText 为 true 时排除文本块（等价 Modality 取非文本全集，
	// 用于“媒体块”语义；模态可能有 image/video/audio 多种）。
	ExcludeText bool
	// SkipCentroid 关闭中心化校正，直接用原始向量算余弦。
	//
	// 默认是要校正的：实测 chineseclip 的块向量均值范数 0.9374，
	// 93.7% 的能量在同一个方向上，导致同属性不同值的分离度只有 0.0095
	// （去均值后 0.0689，提升 7.3 倍）。而中心是**按需加载**的 ——
	// 库里没建过中心、或中心已失效（块数变化 >20%）时自动跳过校正，
	// 所以这个开关只在「明确知道中心存在却想绕开」时才需要。
	SkipCentroid bool
}

// RecallBlocks 按向量相似度召回块节点。
//
// 返回的 hit 已按 Score 降序。库里没有任何块的向量时返回空切片而非错误
// ——「还没回填」是正常状态，不是故障。
func (g *GraphDB) RecallBlocks(q BlockRecallQuery) ([]BlockHit, error) {
	if len(q.Vector) == 0 {
		return nil, fmt.Errorf("recall blocks: empty query vector")
	}
	topK := q.TopK
	if topK <= 0 {
		topK = 10
	}

	// 中心在 RLock 之前加载：LoadCentroid 内部自己拿锁，
	// 而 g.mu 是写锁优先的 Mutex（不可重入），RLock 里再 RLock 虽可行
	// 但夹着写锁时会出现窗口。分开更稳。
	var centroid *Centroid
	if !q.SkipCentroid && q.Fingerprint != "" {
		c, use, err := g.LoadCentroid(q.Fingerprint, len(q.Vector))
		if err != nil {
			// 中心读取失败不该让召回失败 —— 退回原始向量即可，
			// 只是少了个校正。记日志以便发现。
			log.Printf("[graph] recall blocks: reading centroid failed, falling back to raw vectors: %v", err)
		} else if use {
			centroid = c
		}
	}

	g.mu.RLock()
	defer g.mu.RUnlock()

	// 模态过滤下沉到 SQL：文本块动辄数千，把它们读进来再丢弃是白花
	// 向量反序列化与余弦计算的钱（实测 3639 文本块 vs 112 图像块）。
	query := `SELECT ` + blockColumns + `
		FROM memory_blocks
		WHERE vector IS NOT NULL AND vector != '' AND vector != 'null'`
	var args []interface{}
	if q.Modality != "" {
		query += ` AND modality = ?`
		args = append(args, string(q.Modality))
	}
	if q.ExcludeText {
		query += ` AND modality != ?`
		args = append(args, string(BlockText))
	}
	rows, err := g.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dim := len(q.Vector)
	var hits []BlockHit
	var skippedDim, skippedFP, scanned int
	for rows.Next() {
		b, err := scanBlockRow(rows)
		if err != nil {
			return nil, err
		}
		scanned++
		vec := b.Vector
		// 维度不一致：跳过并计数。**不截断、不填充**——那会造出无意义的分数。
		if len(vec) != dim {
			skippedDim++
			continue
		}
		// 指纹不一致：跳过并计数。查询方声明的空间与块所属空间不同。
		if q.Fingerprint != "" && b.Fingerprint != "" && b.Fingerprint != q.Fingerprint {
			skippedFP++
			continue
		}
		// 模态过滤已在 SQL 层完成（见上）。这里保留一段防御式判断：
		// 调用方若直接拼 SQL 绕过（未来新路径），语义不变。
		if q.Modality != "" && b.Modality != q.Modality {
			continue
		}
		if q.ExcludeText && b.Modality == BlockText {
			continue
		}
		// 中心化：查询向量与块向量**都要**中心化后再算余弦。
		// 只处理一边的话等于在混两种量纲（一半带公共分量、一半不带），
		// 算出的余弦没有意义。
		score := centerBoth(centroid, vec, q.Vector)
		if q.MinScore > 0 && score < q.MinScore {
			continue
		}
		b.Vector = vec
		hits = append(hits, BlockHit{Block: b, Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if skippedDim > 0 {
		log.Printf("[graph] recall blocks: skipped %d blocks with mismatched dimension (query %d dims, scanned %d)",
			skippedDim, dim, scanned)
	}
	if skippedFP > 0 {
		log.Printf("[graph] recall blocks: skipped %d blocks with mismatched fingerprint (query space %s, scanned %d)",
			skippedFP, shortFP(q.Fingerprint), scanned)
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > topK {
		hits = hits[:topK]
	}

	// ★ 记录召回时间（P6，2026-10-29）。
	//
	// 为什么必须记：回收判定看的是"这块记忆还有没有用"，而"被召回"
	// 是有用的**唯一实证信号** —— 创建时间只说明它什么时候写进来。
	// 不记的话，所有块看起来都"很久没用过"，回收就等于按入库时间瞎删。
	//
	// 为什么在解锁后做：本函数持 RLock，写库会撞锁（SQLite 单写）。
	// 所以先把命中的 ID 复制出来，defer 到 RUnlock 之后再批量更新。
	// 失败只记日志：召回本身已经成功，不能因为"记不上时间"就报错。
	hitIDs := make([]string, len(hits))
	for i := range hits {
		hitIDs[i] = hits[i].Block.ID
	}
	if len(hitIDs) > 0 {
		defer func() { g.markRecalled(hitIDs) }()
	}
	return hits, nil
}

// markRecalled 批量刷新 last_recalled_at。
//
// 单条 UPDATE ... IN (...) 而不是逐条：召回 topK=10~50，
// 逐条就是 10~50 次独立写事务，在 SQLite 上会明显拖慢每轮对话。
//
// 失败只记日志：这是"统计信号"，不是正确性数据 ——
// 记不上时间最多让回收判据偏保守（少删），绝不会删错。
func (g *GraphDB) markRecalled(ids []string) {
	if len(ids) == 0 {
		return
	}
	ph := make([]string, len(ids))
	args := make([]interface{}, 0, len(ids)+1)
	args = append(args, time.Now())
	for i, id := range ids {
		ph[i] = "?"
		args = append(args, id)
	}
	if _, err := g.db.Exec(
		`UPDATE memory_blocks SET last_recalled_at = ?
		 WHERE id IN (`+strings.Join(ph, ",")+`)`, args...); err != nil {
		log.Printf("[graph] mark recalled failed (%d blocks): %v", len(ids), err)
	}
}

// RecallBlocksWithArbitration 与 RecallBlocks 相同，但额外返回仲裁详情
// （被取代的块、结论状态）。
//
// 分成两个入口的原因：调用方大多只关心「该给模型看什么」，仲裁详情是
// 调试与跑分用的。把它塞进返回签名会让 90% 的调用点多做无用功。
func (g *GraphDB) RecallBlocksWithArbitration(q BlockRecallQuery) ([]BlockHit, ArbitrationResult, error) {
	// ★ 仲裁前必须拿到**未截断**的全量候选。
	//
	// 实测故障形态（真实 chineseclip + 真库 188 块）：查询「值班室分机号
	// 是多少」，旧号 4379 以 0.8127 排 top1，新号 4324 那条**进不了
	// top8**。正确记录在召回阶段就被挤掉了 —— 事后仲裁无从挽回，
	// 因为被判取代的旧值和新值都不在候选里。
	//
	// 所以顺序是：全量打分（TopK 放大）→ 仲裁 → 按调用方的 TopK 截断。
	//
	// 为什么 RecallBlocks 本身不仲裁：它是通用召回接口，
	// 返回顺序按余弦相似度是它的**既有契约**（有测试钉着）。
	// 仲裁改变的是「给模型看哪些、按什么顺序看」，属于上层策略 ——
	// 混进召回层会让这个接口的语义变得含糊（调用方说不清拿到的是什么序）。
	q2 := q
	q2.TopK = largeTopK
	hits, err := g.RecallBlocks(q2)
	if err != nil {
		return nil, ArbitrationResult{}, err
	}
	arb := arbitrate(g, hits)
	kept := arb.Kept
	if k := effectiveTopK(q.TopK); len(kept) > k {
		kept = kept[:k]
	}
	return kept, arb, nil
}

// largeTopK 是仲裁前的候选上限。
//
// ★ 为什么要有上限而不是真·全量：仲裁是 O(n²)，候选数若等于库里全部
// 带向量的块（实测 188，回填后可能上万），比较次数会到亿级。
// 这个值远大于正常 TopK（默认 10 / 生产 5~20），实际效果是「足够全」，
// 同时给出一个确定的性能上界。
const largeTopK = 2000

// effectiveTopK 归一 TopK（与 RecallBlocks 内部一致）。
func effectiveTopK(k int) int {
	if k <= 0 {
		return 10
	}
	return k
}

// BlockVectorStats 报告块向量的回填状态，用于运维判断「要不要跑回填」。
type BlockVectorStats struct {
	Total        int      `json:"total"`
	WithVector   int      `json:"with_vector"`
	MissingFP    int      `json:"missing_fingerprint"`
	Fingerprints []string `json:"fingerprints"`
	Dimensions   []int    `json:"dimensions"`
}

func (g *GraphDB) BlockVectorStats() (BlockVectorStats, error) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	var st BlockVectorStats
	fpSet := map[string]bool{}
	dimSet := map[int]bool{}
	rows, err := g.db.Query(`SELECT vector, fingerprint FROM memory_blocks`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var vectorJSON, fp string
		if err := rows.Scan(&vectorJSON, &fp); err != nil {
			return st, err
		}
		st.Total++
		if vectorJSON == "" || vectorJSON == "null" {
			continue
		}
		vec, err := decodeVector(vectorJSON)
		if err != nil {
			continue
		}
		st.WithVector++
		if fp == "" {
			st.MissingFP++
		} else {
			fpSet[fp] = true
		}
		dimSet[len(vec)] = true
	}
	for fp := range fpSet {
		st.Fingerprints = append(st.Fingerprints, fp)
	}
	for d := range dimSet {
		st.Dimensions = append(st.Dimensions, d)
	}
	sort.Strings(st.Fingerprints)
	sort.Ints(st.Dimensions)
	return st, rows.Err()
}

// ── 向量工具 ──

func decodeVector(s string) ([]float64, error) {
	var vec []float64
	if err := json.Unmarshal([]byte(s), &vec); err != nil {
		return nil, fmt.Errorf("decode vector: %w", err)
	}
	return vec, nil
}

func cosine(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
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
	s := dot / (math.Sqrt(na) * math.Sqrt(nb))
	// NaN 会让排序结果不确定（NaN 的比较全 false），显式归零。
	if math.IsNaN(s) {
		return 0
	}
	return s
}

func shortFP(fp string) string {
	if len(fp) > 12 {
		return fp[:12]
	}
	return fp
}
