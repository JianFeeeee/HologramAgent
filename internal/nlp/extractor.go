package nlp

import "strings"

// ——— 分句 ———

func splitSentences(text string) []string {
	var sentences []string
	buf := strings.Builder{}
	for _, r := range text {
		buf.WriteRune(r)
		if r == '。' || r == '！' || r == '？' || r == '；' || r == '\n' {
			s := strings.TrimSpace(buf.String())
			if s != "" {
				sentences = append(sentences, s)
			}
			buf.Reset()
		}
	}
	if tail := strings.TrimSpace(buf.String()); tail != "" {
		sentences = append(sentences, tail)
	}
	return sentences
}

// ——— 依存句法模板 ———

type depTemplate struct {
	subjRel string
	objRel  string
	score   float64
}

var depTemplates = []depTemplate{
	{subjRel: "SBV", objRel: "VOB", score: 0.9},
	{subjRel: "SBV", objRel: "IOB", score: 0.85},
	{subjRel: "SBV", objRel: "FOB", score: 0.8},
	{subjRel: "SBV", objRel: "POB", score: 0.75},
	{subjRel: "ATT", objRel: "VOB", score: 0.7},
	{subjRel: "ATT", objRel: "IOB", score: 0.65},
	{subjRel: "ATT", objRel: "FOB", score: 0.6},
	{subjRel: "ATT", objRel: "POB", score: 0.55},
}

// extractFromDep 基于依存句法树提取三元组 (Phase 2: 结构初筛)
// 输入：Token 序列（含依存关系）
// 处理：标记名词性节点 → 遍历谓词中心 → 收集 SBV/ATT 主语、VOB/IOB/POB 宾语 → 笛卡尔积 → 赋句法置信度 → ATT 链合并
// 输出：候选三元组列表（带 syntax_conf）
func extractFromDep(result *ParseResult, sentence string) []Triple {
	if len(result.Tokens) < 2 {
		return nil
	}

	// Step 1: 标记所有名词性节点为候选实体（供后续 ATT 合并等使用）
	// （隐式使用，通过 isNounLike 判断）

	// Step 2: 遍历所有动词节点作为谓词中心
	verbIndices := findPredicates(result.POS, result.Tokens)
	var triples []Triple

	for _, vi := range verbIndices {
		// Step 3: 沿依存弧收集主语（SBV/ATT）和宾语（VOB/IOB/FOB/POB）
		var subjIndices, objIndices []int

		for i, head := range result.Heads {
			if head == 0 {
				continue
			}
			parentIdx := head - 1
			if parentIdx != vi {
				continue
			}
			rel := result.DepRels[i]

			if isSubjRel(rel) {
				subjIndices = append(subjIndices, i)
			} else if isObjRel(rel) {
				objIndices = append(objIndices, i)
			}
		}

		// 主语降级：无 SBV/ATT 主语时向左查找最近的名词性节点
		if len(subjIndices) == 0 {
			for j := vi - 1; j >= 0; j-- {
				if isNounLike(result.POS[j]) {
					subjIndices = append(subjIndices, j)
					break
				}
			}
		}

		// 宾语降级：无显式宾语时查找动词的其他名词性依赖
		if len(objIndices) == 0 {
			for i, head := range result.Heads {
				if head == 0 {
					continue
				}
				if head-1 == vi && isNounLike(result.POS[i]) && !isSubjRel(result.DepRels[i]) {
					objIndices = append(objIndices, i)
				}
			}
		}

		if len(subjIndices) == 0 || len(objIndices) == 0 {
			continue
		}

		// Step 4: 笛卡尔积生成候选对，按模板赋予句法置信度
		relLabel := result.Tokens[vi]
		for _, si := range subjIndices {
			for _, oi := range objIndices {
				if si == oi {
					continue
				}
				subj := result.Tokens[si]
				obj := result.Tokens[oi]

				score := 0.8 // 默认句法置信度
				// 匹配模板查询精确置信度
				for _, t := range depTemplates {
					if si < len(result.Heads) && result.Heads[si] == vi+1 &&
						oi < len(result.Heads) && result.Heads[oi] == vi+1 &&
						t.subjRel == result.DepRels[si] && t.objRel == result.DepRels[oi] {
						score = t.score
						break
					}
				}

				triples = append(triples, Triple{
					Subject:     subj,
					Relation:    relLabel,
					Object:      obj,
					Score:       score,
					Src:         "dep",
					SentenceRef: sentence,
				})
			}
		}

		// COO 链扩展：为每个宾语所在的并列结构生成额外三元组
		for _, oi := range objIndices {
			cooExpanded := expandCOO(result, oi, vi)
			for _, cooObj := range cooExpanded {
				if cooObj == result.Tokens[oi] {
					continue
				}
				for _, si := range subjIndices {
					subj := result.Tokens[si]
					triples = append(triples, Triple{
						Subject:     subj,
						Relation:    relLabel,
						Object:      cooObj,
						Score:       0.7,
						Src:         "dep_coo",
						SentenceRef: sentence,
					})
				}
			}
		}
	}

	// Step 5: ATT 链合并多词实体
	triples = mergeAttTriples(result, triples)

	// 去重
	triples = dedupTriples(triples)
	return triples
}

// expandCOO 从宾语开始沿 COO 链展开所有并列项
func expandCOO(result *ParseResult, startIdx, excludeParent int) []string {
	var expanded []string
	seen := make(map[int]bool)

	var walk func(idx int)
	walk = func(idx int) {
		if idx < 0 || idx >= len(result.Tokens) || seen[idx] {
			return
		}
		seen[idx] = true
		expanded = append(expanded, result.Tokens[idx])
		for i, head := range result.Heads {
			if head == 0 {
				continue
			}
			if result.DepRels[i] == "COO" && head-1 == idx && i != excludeParent {
				walk(i)
			}
		}
	}
	walk(startIdx)
	return expanded
}

// ——— POS 序列模板（降级） ———

type posTemplate struct {
	pattern []string
	subj    int // 主语在 pattern 中的绝对索引
	verb    int // 谓语在 pattern 中的绝对索引
	obj     int // 宾语在 pattern 中的绝对索引
	score   float64
}

var posTemplates = []posTemplate{
	// 我/r 吃/v 苹果/n
	{pattern: []string{"r", "v", "n"}, subj: 0, verb: 1, obj: 2, score: 0.7},
	// 我/r 吃/v 苹果/n
	{pattern: []string{"r", "v", "nr"}, subj: 0, verb: 1, obj: 2, score: 0.7},
	// 我/r 是/v 学生/n
	{pattern: []string{"r", "v", "n"}, subj: 0, verb: 1, obj: 2, score: 0.7},
	// 小明/nr 喜欢/v 篮球/n
	{pattern: []string{"nr", "v", "n"}, subj: 0, verb: 1, obj: 2, score: 0.7},
	// 小明/nr 打/v 篮球/n
	{pattern: []string{"nr", "v", "nr"}, subj: 0, verb: 1, obj: 2, score: 0.65},
	// 我/r 在/p 杭州/ns 读书/v
	{pattern: []string{"r", "p", "ns", "v"}, subj: 0, verb: 3, obj: 2, score: 0.65},
	// 我/r 在/p 杭州/ns 读书/n（读书被标为 n）
	{pattern: []string{"r", "p", "ns", "n"}, subj: 0, verb: 3, obj: 2, score: 0.55},
	// 我/r 在/p 杭州/ns 工作/vn
	{pattern: []string{"r", "p", "ns", "vn"}, subj: 0, verb: 3, obj: 2, score: 0.6},
	// 小明/nr 在/p 杭州/ns 读书/v
	{pattern: []string{"nr", "p", "ns", "v"}, subj: 0, verb: 3, obj: 2, score: 0.65},
	// 我/r 住在/p 杭州/ns （"住"被标为 v，"在"是 p）
	{pattern: []string{"r", "v", "p", "ns"}, subj: 0, verb: 1, obj: 3, score: 0.6},
	// 小明/nr 住在/p 北京/ns
	{pattern: []string{"nr", "v", "p", "ns"}, subj: 0, verb: 1, obj: 3, score: 0.6},
	// 天气/n 很/d 好/a
	{pattern: []string{"n", "d", "a"}, subj: 0, verb: 2, obj: 2, score: 0.5},
	// 天气/n 很/zg 好/a（很 被标为 zg 而非 d）
	{pattern: []string{"n", "zg", "a"}, subj: 0, verb: 2, obj: 2, score: 0.45},
	// 今天/t 天气/n 好/a
	{pattern: []string{"t", "n", "a"}, subj: 1, verb: 2, obj: 2, score: 0.5},
	// 我/r 喜欢/v 跑步/vn
	{pattern: []string{"r", "v", "vn"}, subj: 0, verb: 1, obj: 2, score: 0.6},
	// 我/r 喜欢/v 游泳/vn
	{pattern: []string{"r", "v", "v"}, subj: 0, verb: 1, obj: 2, score: 0.65},
	// 我/r 叫/v 小明/nr
	{pattern: []string{"r", "v", "nr"}, subj: 0, verb: 1, obj: 2, score: 0.7},
	// 通用：代词/名词 + 动词 + 名词
	{pattern: []string{"r", "v", "ns"}, subj: 0, verb: 1, obj: 2, score: 0.6},
	{pattern: []string{"n", "v", "n"}, subj: 0, verb: 1, obj: 2, score: 0.65},
	// 我/r 吃/v 了/u 苹果/n
	{pattern: []string{"r", "v", "u", "n"}, subj: 0, verb: 1, obj: 3, score: 0.6},
	// 名词跟在代词后作为谓语（打球/n 在 我/r 后）
	{pattern: []string{"r", "n"}, subj: 0, verb: 1, obj: 1, score: 0.5},
	// 小明/x 喜欢/v 吃/v 苹果/n（x 为人名，连动结构）
	{pattern: []string{"x", "v", "v", "n"}, subj: 0, verb: 1, obj: 3, score: 0.55},
	// 小明/x 喜欢/v 苹果/n
	{pattern: []string{"x", "v", "n"}, subj: 0, verb: 1, obj: 2, score: 0.55},
	// 我/r 喜欢/v 吃/v 苹果/n
	{pattern: []string{"r", "v", "v", "n"}, subj: 0, verb: 1, obj: 3, score: 0.6},
	// 通用：x 标签代词 + 动词 + vn
	{pattern: []string{"x", "v", "vn"}, subj: 0, verb: 1, obj: 2, score: 0.5},
	// 小明/x 在/p 北京/ns 工作/v
	{pattern: []string{"x", "p", "ns", "v"}, subj: 0, verb: 3, obj: 2, score: 0.55},
	// 小明/x 在/p 北京/ns 上班/vn
	{pattern: []string{"x", "p", "ns", "vn"}, subj: 0, verb: 3, obj: 2, score: 0.5},
	// 名词/n + 动词/v + 动词/v + 名词/n（连动）
	{pattern: []string{"n", "v", "v", "n"}, subj: 0, verb: 1, obj: 3, score: 0.55},
}

// extractFromPOS 基于 POS 序列匹配模板提取三元组 (Phase 2 降级路径)
func extractFromPOS(result *ParseResult, sentence string) []Triple {
	if len(result.Tokens) < 2 {
		return nil
	}

	var triples []Triple
	pos := result.POS
	tokens := result.Tokens

	for _, tpl := range posTemplates {
		pat := tpl.pattern
		if len(pat) > len(pos) {
			continue
		}
		for i := 0; i <= len(pos)-len(pat); i++ {
			if !matchPOS(pos[i:i+len(pat)], pat) {
				continue
			}

			subj := tokens[i+tpl.subj]
			verb := tokens[i+tpl.verb]
			obj := tokens[i+tpl.obj]
			if subj == "" || verb == "" || obj == "" {
				continue
			}
			// 跳过自指谓语/无宾语谓语
			if subj == obj {
				continue
			}
			// 跳过谓语等于宾语（形容词谓语等无实际宾语的情况）
			if verb == obj {
				continue
			}
			triples = append(triples, Triple{
				Subject:     subj,
				Relation:    verb,
				Object:      obj,
				Score:       tpl.score,
				Src:         "pos",
				SentenceRef: sentence,
			})
		}
	}

	// 去重（相同 subj/rel/obj 只保留一个）
	triples = dedupTriples(triples)
	return triples
}

func matchPOS(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func dedupTriples(triples []Triple) []Triple {
	seen := make(map[string]bool)
	var out []Triple
	for _, t := range triples {
		key := t.Subject + "\x00" + t.Relation + "\x00" + t.Object
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
	}
	return out
}

// ——— ATT 链合并 ———

// mergeAttTriples ATT 链合并：将定语合并到被修饰词
func mergeAttTriples(result *ParseResult, triples []Triple) []Triple {
	attMap := make(map[int][]int)
	for i, head := range result.Heads {
		if head == 0 {
			continue
		}
		if i >= len(result.DepRels) {
			continue
		}
		if result.DepRels[i] == "ATT" {
			parentIdx := head - 1
			attMap[parentIdx] = append(attMap[parentIdx], i)
		}
	}
	if len(attMap) == 0 {
		return triples
	}
	for i := range triples {
		for headIdx, attIds := range attMap {
			if headIdx >= len(result.Tokens) {
				continue
			}
			headWord := result.Tokens[headIdx]
			var attWords []string
			for _, aid := range attIds {
				if aid < len(result.Tokens) {
					attWords = append(attWords, result.Tokens[aid])
				}
			}
			if len(attWords) == 0 {
				continue
			}
			expanded := strings.Join(attWords, "") + headWord
			if triples[i].Subject == headWord {
				triples[i].Subject = expanded
			}
			if triples[i].Object == headWord {
				triples[i].Object = expanded
			}
		}
	}
	return triples
}

// ——— helper ———

func findPredicates(pos []string, tokens []string) []int {
	var indices []int
	for i, p := range pos {
		if isVerb(p) || isAdj(p) {
			indices = append(indices, i)
			continue
		}
		if isNounLike(p) && i > 0 && isPronoun(pos[i-1]) {
			indices = append(indices, i)
			continue
		}
		if isNounLike(p) && i > 0 && isNounLike(pos[i-1]) {
			indices = append(indices, i)
			continue
		}
	}
	return indices
}

func isVerb(p string) bool {
	return p == "v" || p == "vd" || strings.HasPrefix(p, "v")
}

func isNounLike(p string) bool {
	return p == "n" || p == "nr" || p == "ns" || p == "nt" || p == "nz" ||
		p == "an" || p == "vn" || p == "x" ||
		strings.HasPrefix(p, "n")
}

func isPronoun(p string) bool {
	return p == "r"
}

func isAdj(p string) bool {
	return p == "a"
}

func isSubjRel(rel string) bool {
	return rel == "SBV" || rel == "ATT"
}

func isObjRel(rel string) bool {
	return rel == "VOB" || rel == "IOB" || rel == "FOB" || rel == "POB"
}
