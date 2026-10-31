package knowledge

import "sort"

// minLexSignal 是「词法路有真实信号」的最低 top1 分数。
//
// 低于它的原因不是命中，而是 jieba 把查询切出了几个泛滥词（如单字、虚词），
// 于是词法路给近 200 条都打上极小的分。这种情况下词法排序没有意义，
// 应当把召回让给稠密路。
//
// 取值依据（2026-10-31 实测，真实 196 条库 + chineseclip）：真命中在
// 0.056 ~ 0.208；弱命中（`鸿蒙` → openharmony-third-party）0.117。
// 0.02 比最小真命中低一倍多，足够宽松。
const minLexSignal = 0.02

// fuseLexPrimary 以**词法路为主、稠密路为辅**合并两路命中。
//
// # 为什么不是 RRF（试过，被实测否掉）
//
// RRF 只看名次、丢掉分数差距。而这里词法路的名次之间**差距很小**：
// `strip_path` 的 top1 是 0.056、top2 是 0.055 —— 名次差 1，RRF 就当成
// 几乎等强，稠密路对 top2 的加分一叠就把它超了。实测：RRF（词法 1.0 /
// 稠密 0.5）下 `strip_path` 的目标条目直接掉出前五。
//
// # 为什么词法为主（这与「稠密更先进」的直觉相反，但实测如此）
//
// 真实 196 条库 + 真实 chineseclip，同一批查询里两路的名次：
//
//	查询                        稠密名次   词法名次
//	PruneOldBackups                103         1
//	strip_path                     164         1
//	minSceneEvidence               130         1
//	changelog_v1.3.0                 9         1
//	henan_medical_university         4         1
//	上下文裁剪的保护条数受什么约束    157         1
//	更新日志                        86         8
//
// 原因是本项目自己记录过的已知代价：chineseclip 双塔的**纯文本语义弱于
// MLLM 型嵌入器**（见 multimodal-space 文档）。而这里 154/196 条是同一种
// 文档（cangjie），在 512 维里挤成一团，稠密打分对「哪个才对」几乎没有
// 分辨力 —— 目标条目普遍落在 100 名以外。
//
// # 稠密路仍然要接（不是死代码）
//
//  1. **同分裁决**：词法分数相同时按稠密名次排（分数是浮点，同分确实出现）
//  2. **补位**：词法路命中不足 topK 时，按稠密名次追加
//  3. **无信号时接管**：词法 top1 低于 minLexSignal（见该常量注释）
//  4. **词法全空时接管**：查询里没有任何可命中的词
//
// 换嵌入器（比如换成语义更强的 MLLM 型）后本策略应当重测 —— 那时稠密路
// 的名次可能反超，权重需要跟着改。判据见 identifier_recall_test.go 与
// dense_recall_quality_test.go。
func fuseLexPrimary(lex, dense []scoreHit) []scoreHit {
	if len(lex) == 0 {
		return dense
	}
	if lex[0].score < minLexSignal {
		// 词法只是泛滥词的微弱命中，让它接管会把稠密路的语义召回埋掉。
		return dense
	}

	denseRank := make(map[string]int, len(dense))
	for i, h := range dense {
		denseRank[h.id] = i
	}

	out := make([]scoreHit, len(lex))
	copy(out, lex)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].score != out[j].score {
			return out[i].score > out[j].score
		}
		di, iok := denseRank[out[i].id]
		dj, jok := denseRank[out[j].id]
		if iok != jok {
			return iok // 稠密路认识的排前面
		}
		if iok && jok && di != dj {
			return di < dj
		}
		return out[i].id < out[j].id // 兜底：可复现
	})

	// 补位：词法路没覆盖的候选按稠密名次接在尾部。
	seen := make(map[string]bool, len(out))
	for _, h := range out {
		seen[h.id] = true
	}
	for _, h := range dense {
		if !seen[h.id] {
			out = append(out, h)
		}
	}
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
