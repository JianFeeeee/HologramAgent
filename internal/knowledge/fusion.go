package knowledge

import "sort"

// rrfK 是 RRF（Reciprocal Rank Fusion）的平滑常数。
//
// 60 是 RRF 的通用取值（Cormack et al. 2009）：它让「第 1 名」与「第 2 名」
// 的贡献足够接近（1/61 vs 1/62），又让第 100 名的贡献掉到 1/160 —— 既压住
// 长尾噪声，又不至于让名次之间的差异被抹平。调大它更平权，调小更信头部。
const rrfK = 60

// fuseRRF 按倒数排名融合两路命中。
//
// 输入必须是**已按分数降序**的候选（SearchScored / denseHits 都保证这一点），
// 因为 RRF 只用名次、不看分数。
//
// 为什么用名次而不做分数归一化：两路量纲完全不可比（稠密余弦 0~1，TF-IDF 余弦
// 可以低到 0.02）。2026-10-08 淘汰「三路融合」时踩的坑正是逐路按查询内最大值
// 归一到 1.0 —— 那会把 0.02 的噪声抬到与 1.0 的信号等权。RRF 没有这个问题：
// 它只问「你在这路里排第几」，权重由调用方显式给。
//
// 任一路为空时，另一路的贡献不受影响（缺席的路径只是不加分）。
func fuseRRF(dense, lex []scoreHit, wDense, wLex float64, k int) []scoreHit {
	if k <= 0 {
		k = rrfK
	}
	scores := make(map[string]float64, len(dense)+len(lex))
	addRanks := func(hits []scoreHit, w float64) {
		if w <= 0 {
			return
		}
		for i, h := range hits {
			// i 从 0 起，名次从 1 起
			scores[h.id] += w / float64(k+i+1)
		}
	}
	addRanks(dense, wDense)
	addRanks(lex, wLex)

	out := make([]scoreHit, 0, len(scores))
	for id, s := range scores {
		out = append(out, scoreHit{id: id, score: s})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		// 同分按 id 稳定排序，避免 map 迭代顺序导致结果不可复现
		return out[i].id < out[j].id
	})
	return out
}

// lexHits 用词法路（TF-IDF 索引）给查询打分，返回**按分数降序**的候选。
//
// 调用方必须已持有 s.mu（读锁即可）：这里只读 s.lex 与 s.veczer。
// s.lex 自己另有一把锁，与 s.mu 不同，不存在锁序问题。
func (s *Store) lexHits(query string) []scoreHit {
	if s.lex == nil || s.lex.Size() == 0 || s.veczer == nil {
		return nil
	}
	return toHits(s.lex.SearchScored(s.veczer.Vectorize(query), s.lex.Size()))
}

// searchLexOnly 是**只有词法路**时的检索入口。
//
// 用在稠密路向量化为空/失败的场合（空查询、模型未就绪）：此时词法路不依赖模型，
// 仍能按字面命中。它不取 s.mu —— 唯一调用方 SearchIn 已持读锁。
func (s *Store) searchLexOnly(query string, inScope func(string) bool, topK int) []*Knowledge {
	hits := s.lexHits(query)
	if len(hits) == 0 {
		return nil
	}
	out := make([]*Knowledge, 0, topK)
	for _, h := range hits {
		if !inScope(h.id) {
			continue
		}
		if k, ok := s.items[h.id]; ok {
			out = append(out, k)
		}
		if len(out) >= topK {
			break
		}
	}
	return out
}
