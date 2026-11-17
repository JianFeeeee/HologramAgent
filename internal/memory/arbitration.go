package memory

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// 同属性多值的时序仲裁。
//
// ★ 为什么需要（实测，真实 chineseclip + 真库 188 块）
// ----------------------------------------------
// 迁移后的块向量检索在「值覆盖」维度上答错：
//
//	查询 "值班室分机号是多少"  →  top1 = 旧号 4379（score 0.8127）
//	                            新号 4324 的块排不进 top8
//
// 而把长复合句改写成短句后，实测相似度是：
//
//	「值班室分机号 4324」 cos = 0.9284
//	「值班室分机号 4379」 cos = 0.9298   ← 旧号仍高 0.0014
//
// **结论：单靠向量无法解决值覆盖。** 新旧两个值在向量空间里几乎同点，
// 谁更「对」不是相似度能回答的问题 —— 答案是「哪个更新」，而那只有
// 时间戳知道。
//
// ★ 仲裁不是「一律取最新」—— 真库里有两种相反的时间语义
// --------------------------------------------------
// 值覆盖（后一条取代前一条，取最新对）：
//
//	13:28  值班室分机号 4379，值班人 阿李
//	13:44  下周起值班室分机号改为 4324，旧号 4379 停用
//
// 指标收窄（每条都在补充细节，「最新」反而会答错）：
//
//	13:32  峰值 4%~17%，查询接口与下单/退款接口最集中
//	13:35  峰值 4%~17%（30~84批整体），80~84批为 7%~16%
//	13:40  峰值4%~17%(30~84批)，85~95批未再上报   ← 最新这条说的是「停报」
//
// 对后者取最新，答出来的是「85~95批未再上报」，而不是「峰值是多少」。
//
// 所以判据必须先分清「取代」与「补充」，再决定怎么排。这不是靠猜 ——
// 见 supersedes()：两条讲同一属性且**后一条明确提到了前一条的值**时，
// 那是取代；否则是补充。

// blockFact 是从块里解析出的一个事实陈述：谁、什么属性、什么值、何时。
type blockFact struct {
	// Subject/维度/值 由块文本解析（形态见 parseFact）。
	Subject   string
	Dimension string
	Value     string
	// Text 是原始块文本（仲裁日志与回给模型的证据）。
	Text string
	// When 是块的时间戳；零值表示未知（迁移兜底或无 provider）。
	When time.Time
	// Score 是向量相似度（若本次是向量召回）。
	Score float64
}

// 仲裁必须在**同一属性**的候选之间做，而「同一属性」需要看这两个块是不是
// 从同一条原句拆出来的 —— 那是 TimesEqual 的补充维度。
//
// ★ 判据来自实测的一组数据（不是构造的）
//
//	「值班室分机号 4324，值班 老周（下周起；旧号 4379 停用）」
//	  → 值班室分机号|值班分机号=4324   （时间 13:44）
//	  → 值班室分机号|停用旧号=4379     （时间 13:44，同一条原句）
//
// 这两条**不是**新旧替代关系，而是同一次陈述里的两个字段（当前值 +
// 被停用的旧值）。用「后一条提及前一条的值」判会把 4324 那条当成
// 取代 4379 那条 —— 方向反了，且会让真实的旧号信息消失。
//
// 所以判据加一条：时间相同且都来自同一条原句 ⇒ 不是时序替代，只是并列字段。
// 「同一条原句」用 sentences 表的归属判定（BlocksForNode 能拿到）。

// parseFact 从块文本解析出（主语, 维度, 值）。
//
// 支持两种形态，因为库里同时存在：
//  1. 迁移来的整句：「值班室分机号 4379，值班人 阿李」（无 '|' 无 '='）
//     → 解析不出三元组，返回 nil，调用方按「不可仲裁」处理
//  2. 拆分产生的块：「<主语>|<维度>=<值>」
//     → 三段齐全
//
// 为什么不让 memory 引用 distill 的同名函数：distill → memory 已存在
// 依赖（blocks.go 用 memory.MemoryBlock），反向引用会成环。这两个函数
// 是纯字符串切分，各自实现是有意的重复而非疏忽。
func parseFact(text string) (subject, dimension, value string, ok bool) {
	i := strings.Index(text, "=")
	if i <= 0 {
		return "", "", "", false // 无 '='，不是事实块
	}
	dim, val := text[:i], text[i+1:]
	// 主语在首个 '|' 之前
	if j := strings.Index(dim, "|"); j >= 0 {
		return strings.TrimSpace(dim[:j]),
			strings.TrimSpace(dim[j+1:]),
			val, true
	}
	return "", strings.TrimSpace(dim), val, true
}

// stopWords 是「这条没在报告值」的标记词。
//
// 用于区分「指标收窄」与「值覆盖」：最新那条若只是说「不再上报」
// 「暂无」「未见」，那它没有给出值，不该把旧值挤掉。
//
// ★ 词必须来自真库实见，不是编的：见真库的
// 「第85批起不再上报错误率峰值」「85~95批未再上报」。
//
// 命名 cutStopWords：cut.go 里已有一个 stopWords（map[string]bool，
// 用于分词剪枝），同名会让 go vet 报 non-boolean condition。
var cutStopWords = []string{
	"不再上报", "未再上报", "不再", "未再", "暂无", "未见", "无异常",
	"未发生", "已恢复", "已结束", "已回滚", "已修复",
}

// reportsValue 判断这条块是否给出了具体值。
//
// 「第85批起不再上报错误率峰值」里有「不再」，是状态陈述不是取值 —
// 它不该因为时间最新就压掉一个真实报出的值。
func reportsValue(text string) bool {
	for _, w := range cutStopWords {
		if strings.Contains(text, w) {
			return false
		}
	}
	return true
}

// supersedes 判断 later 是否取代了 earlier（同一属性的新值）。
//
// 判据是**后一条文本里出现了前一条的值** —— 这是可观察的、不依赖
// 字段名的信号：
//
//	earlier: 值班室分机号 4379，值班人 阿李        值 = 4379
//	later:   下周起值班室分机号改为 4324，旧号 4379 停用
//	                                          ↑ 含 "4379" → 是取代
//
// 而指标收窄那组：
//
//	earlier: 峰值 4%~17%（30~71批整体）           值 = 4%~17%（30~71批整体）
//	later:   峰值 4%~17%（30~84批整体）           ↑ 不含前者的完整值 → 不是取代
//
// 为什么不比「值是否不同」：新旧值不同是常态，但「峰值 4%~17%」到
// 「峰值 4%~17%（30~84批整体）」里主值没变、只是范围细化了，
// 那不是取代 —— 值不同但主值相同的情况必须判为「补充」。
func supersedes(earlier, later blockFact) bool {
	// 必须同一主体，否则谈不上取代（不同服务各自的端口互不取代）。
	if earlier.Subject != later.Subject {
		return false
	}
	if earlier.Value == "" {
		return false
	}
	// 后一条原文里提到了前一条的值 → 旧值被显式取代。
	//
	// ★ 为什么在这里判「提及」而不在维度相等的前提之后：
	// 维度名会漂移（漂移发现实测的候选是
	// 「值班分机号 / 停用旧号 / 旧分机号」），先要求维度严格相等的话，
	// 「旧分机号=4379」永远判不出它取代了「值班分机号=4379」——
	// 而这正是值覆盖最常见的形态（旧值与新值被拆成两个不同维度名）。
	// 「文本里提到了旧值」是更直接的证据，优先于维度名相等。
	//
	// 但仍要防一种误判：指标收窄那组里
	// 「峰值=4%~17%（30~71批）」与「峰值=4%~17%（30~84批）」
	// 后者文本不含前者的**完整**值（含括号与批次范围）⇒ 不判取代。
	// 所以判的是「完整值出现在原文里」，不是「某个数字片段出现」。
	return strings.Contains(later.Text, earlier.Value)
}

// sortHitsByTimeThenScore 按 (时间升序, 分数降序) 排。
//
// 为什么时间相同时要按分数降序：迁移后同批写入的块 created_at 完全相同
// （实测真库 188 块只有 28 个不同时刻），只按时间排等于随机 ——
// 同一批里的「端口」和「值班人」谁先谁后没有意义，但「向量更相关的排前面」
// 有意义。
func sortHitsByTimeThenScore(hits []BlockHit) {
	sort.SliceStable(hits, func(i, j int) bool {
		ti, tj := hits[i].Block.CreatedAt, hits[j].Block.CreatedAt
		if ti.Equal(tj) {
			return hits[i].Score > hits[j].Score
		}
		if ti.IsZero() != tj.IsZero() {
			// 未知时间的排后面（不该被判为「更早」而被取代规则吃掉）
			return tj.IsZero()
		}
		return ti.Before(tj)
	})
}

// ArbitrationResult 是仲裁后的结果。

// sameSentenceOf 返回每个块所属的原句 id。
//
// 块→原句的归属靠 sentence --contains--> block 边（块表本身没有
// sentence_id 列 —— media_refs 那套设计已废弃）。同一句话拆出的多个字段块
// 因此能识别出来，而它们是**并列字段**而不是时序替代。
//
// 拿不到归属时（块不是从句子拆来的，比如迁移来的整句）返回空 map ——
// 调用方按「无法判定同句」处理，见 sameSentenceOK。
func sameSentenceOf(db *GraphDB, hits []BlockHit) map[string]string {
	if db == nil || len(hits) == 0 {
		return nil
	}
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		if h.Block.ID != "" {
			ids = append(ids, h.Block.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	placeholders := ""
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args = append(args, id)
	}
	// ★★ source_kind 必须是 'block'，不是 'sentence'。
	//
	// 端点类型在 Commit 块化时从「sentences 表行号」改成「原句块」
	// （52e4596 / cdf0726），但这处 SQL 还写着 'sentence'。
	//
	// ⇒ 查询恒空 ⇒ 同句分组全失败 ⇒ 仲裁把**跨句**的块当成互相取代：
	//
	//	同句并列 blk_a/blk_b + 旧句 blk_c
	//	  → blk_c 被误剔除（「三者都该保留」失败）
	//
	// ★ 症状极隐蔽：仲裁测试当场抓到，但它绿了很久 ——
	//   因为在旧形态下这处是对的，只有新形态才暴露。
	rows, err := db.db.Query(`SELECT target_id, source_id FROM memory_block_edges
		WHERE edge_type = 'contains' AND source_kind = 'block'
		AND target_kind = 'block' AND target_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var blockID, sentID string
		if err := rows.Scan(&blockID, &sentID); err != nil {
			continue
		}
		out[blockID] = sentID
	}
	return out
}

// isDeprecatedDim 判断维度名是否在陈述「旧值作废」。
//
// 只挡这一类，不挡所有有兄弟的块 —— 上一版「有兄弟就不外溢」过宽：
// 真库形态里
//
//	「值班室分机号 4324，值班 老周（旧号 4379 停用）」
//	  → 值班分机号=4324   停用旧号=4379   值班人=老周
//
// 其中「停用旧号」带兄弟时不该外溢（它自己就是被停用的那个），
// 但「值班人=老周」带兄弟时若也禁止外溢，就会漏掉真正的取代关系。
//
// 判据是维度名的语义（真库里模型就是这么写的）：
// 停用 / 旧 / 原 / 之前 / 曾经 开头或结尾的维度名。
func isDeprecatedDim(dim string) bool {
	if dim == "" {
		return false
	}
	markers := []string{"旧", "停用", "原", "之前", "曾经", "原有", "历史"}
	for _, m := range markers {
		if strings.Contains(dim, m) {
			return true
		}
	}
	return false
}

// hasSameSentenceSibling 报告该块所在原句是否还拆出了别的块。
//
// 用途：识别「新值 + 旧值并列」的结构 —— 这种句子里已经自带了
// 「旧值作废」的信息，不需要靠外溢取代去表达（见 arbitrate 里的注释）。
//
// ★★ source_kind 必须是 'block'，不是 'sentence'（2026-10-04）。
//
// contains 边的两个端点都是块（source_kind / target_kind 均为 'block'），
// 因为端点类型在 Commit 块化时从「sentences 表行号」改成了「原句块」；
// 而原句块恒在 source 侧 —— 生产库实测 1298 条 contains 边里
// source_id 全部是 blk_src_* 原句块，无一例外。
//
// 句子的 id 也正是从 sameSentenceOf 拿的（它查 target_id=字段块、
// 取 source_id 作原句 id），两处口径必须一致，否则：
//
//	查询恒空（source_kind='sentence' 没有一行匹配）
//	  → 「新值+旧值并列」的结构识别不出来
//	  → 带 isDeprecatedDim 的旧值块被错误地当成取代者执行外溢
//	  → 误剔除本该保留的旧值。
//
// ★ 症状隐蔽：它在同形态下表现正常，只有「旧值块恰好带废弃维度名」
//
//	的那批数据才会偏 —— 而那批数据在长对话里占比不高。
func hasSameSentenceSibling(db *GraphDB, sentenceID, excludeBlockID string) bool {
	if db == nil || sentenceID == "" {
		return false
	}
	db.mu.RLock()
	defer db.mu.RUnlock()
	var n int
	err := db.db.QueryRow(`SELECT COUNT(*) FROM memory_block_edges
		WHERE edge_type = 'contains' AND source_kind = 'block' AND target_kind = 'block'
		AND source_id = ? AND target_id != ?`,
		sentenceID, excludeBlockID).Scan(&n)
	return err == nil && n > 0
}

// supersedesSentence 判断 later 文本是否取代了 earlier 文本。
//
// ★ 专为**整句块**设计（解析不出「主语|维度=值」的那种）：
//
//	判据只要两件事 —— 时间更晚 + 提到了更早那条里的关键值。
//
// 为什么不需要解析主语：值覆盖场景下，「新号生效、旧号作废」这句话
// **必然同时含新旧两个值**：
//
//	early: 值班室分机号 4379，值班人 阿李
//	late:  值班室分机号 4324，值班 老周（旧号 4379 停用）
//	                                          ↑ 含 early 的 4379
//
// 抽出的「关键值」用与 parseFact 同一套值形态正则（数字/版本/百分比），
// 匹配 early 里最长的那个。
var sentenceValueRe = regexp.MustCompile(
	`\d+(?:\.\d+)?(?:/\d+)*(?:~\d+)?%?|[vV]\d+(?:\.\d+)+|第\d+[批号版]?`)

func supersedesSentence(earlier, later string) bool {
	// 取 earlier 里最长的那个值
	best := ""
	for _, m := range sentenceValueRe.FindAllString(earlier, -1) {
		if len(m) > len(best) {
			best = m
		}
	}
	if best == "" {
		return false // earlier 里没有可识别的值，无从判断取代
	}
	// later 提到了它，且 later 不是同一句话（否则是自我引用）
	return best != later && strings.Contains(later, best)
}

// arbitrate 对同属性的候选块做时序仲裁。
//
// db 可为 nil（拿不到块→原句归属时退化为纯时间+文本判据）。
//
// 规则（三条，按顺序）：
//  1. **不可仲裁的直接保留**：解析不出（主语,维度,值）的块、没带时间的块。
//     强行仲裁它们等于丢信息 —— 宁可多返回给模型让它自己判断。
//  2. **被明确取代的剔除**：later 文本含 earlier 的值 ⇒ earlier 出局。
//     被取代的块仍然放进 Superseded 而不是丢弃 —— 「旧号 4379 停用」
//     本身就是有信息量的（它解释了为什么现在打不通）。
//  3. **同一属性的其余块按时间升序保留**，不合并、不去重：
//     指标收窄那组三条都要留着，每条带不同的批次范围。
//
// 返回的 Kept 已按 (When 升序, Score 降序) 排过：时间相同时（迁移后
// 同批写入的多条）用向量分兜底。
func arbitrate(db *GraphDB, hits []BlockHit) ArbitrationResult {
	sentOf := sameSentenceOf(db, hits)
	type parsed struct {
		hit    BlockHit
		fact   blockFact
		ok     bool
		sameOf string // 所属原句 id（空 = 未知）
	}
	var ps []parsed
	for _, h := range hits {
		sub, dim, val, ok := parseFact(h.Block.Text)
		ps = append(ps, parsed{hit: h, fact: blockFact{
			Subject: sub, Dimension: dim, Value: val,
			Text: h.Block.Text, When: h.Block.CreatedAt, Score: h.Score,
		}, ok: ok, sameOf: sentOf[h.Block.ID]})
	}

	var kept, superseded []BlockHit
	dropped := make(map[string]bool)

	for i, a := range ps {
		if !a.ok || a.fact.When.IsZero() {
			// 规则 1（修订）：**没有时间的**不可仲裁 → 保留。
			//
			// 但「有时间的整句块」要参与取代判断 —— 它解析不出
			// (主语,维度,值)，不代表不能判断「谁取代了谁」。
			//
			// 实测故障（真库 legacy-entity 整句块）：
			//
			//	13:28  值班室分机号 4379，值班人 阿李
			//	13:44  值班室分机号 4324，值班 老周（旧号 4379 停用）
			//	13:44  下周起值班室分机号改为 4324，旧号 4379 停用
			//
			// 查询「现在的值班分机号」时旧号排 top1 —— 而 13:44 那两条
			// **文本里含 "4379"**，正是「新号生效、旧号作废」的自述。
			// 判据就是 supersedesSentence：更晚 + 提到更早那条的值 ⇒ 取代。
			// 不需要解析主语，也不需要维度。
			if a.fact.When.IsZero() {
				kept = append(kept, a.hit)
				continue
			}
			supersededByOther := false
			for j, b := range ps {
				if i == j || b.fact.When.IsZero() || !b.fact.When.After(a.fact.When) {
					continue
				}
				if supersedesSentence(a.hit.Block.Text, b.hit.Block.Text) {
					dropped[a.hit.Block.ID] = true
					superseded = append(superseded, a.hit)
					supersededByOther = true
					break
				}
			}
			if !supersededByOther {
				kept = append(kept, a.hit)
			}
			continue
		}
		// 规则 2：被后面任一条显式取代？
		supersededByOther := false
		for j, b := range ps {
			if i == j || !b.ok || b.fact.When.IsZero() {
				continue
			}
			if !b.fact.When.After(a.fact.When) {
				continue // 只看更晚的
			}
			// 同一条原句拆出的字段是**并列**关系，不是时序替代。
			// 实测形态：「值班室分机号 4324，值班 老周（旧号 4379 停用）」
			// 拆出 值班分机号=4324 与 停用旧号=4379 —— 后者提及 4379，
			// 但它不是「取代」4324，两者是同一次陈述里的两个字段。
			if a.sameOf != "" && a.sameOf == b.sameOf {
				continue
			}
			// ★ 同句的并列字段不得「外溢」去取代句外的块。
			//
			// 实测形态（真库）：
			//   原句A 13:28  「值班室分机号 4379，值班人 阿李」
			//        → 值班分机号=4379
			//   原句B 13:44  「值班室分机号 4324，值班 老周（旧号 4379 停用）」
			//        → 值班分机号=4324、停用旧号=4379   ← 与上条并列
			//
			// 原句B 的「停用旧号=4379」文本里含 4379，若允许它外溢，
			// 就会把原句A 的 4379 判成被取代 —— 而实际上被取代的是
			// 原句B **自己**那条并列字段（同句已排除），原句A 那条
			// 恰恰是「历史上真实用过的号」，正是值覆盖要保留的信息。
			//
			// 判据：b 与 a 同句时 continue（同句并列，已在上面处理）；
			// b 与 a 不同句但 b 自己还有个同句的兄弟块时，说明 b 属于
			// 「新值 + 旧值并列」的结构，不该由它单独执行取代。
			if b.sameOf != "" && b.sameOf != a.sameOf &&
				hasSameSentenceSibling(db, b.sameOf, b.hit.Block.ID) &&
				isDeprecatedDim(b.fact.Dimension) {
				continue
			}
			// 拿不到块→原句归属时的兜底：时间完全相同且值出现在同一句里，
			// 多半是同一次陈述拆出的并列字段，而不是时序替代。
			//
			// ★ 判据不能是「值文本相同」——那会把
			// 「值班分机号=4379」与「停用旧号=4379」也判成并列，而它们
			// 恰好**就该**是并列。真正的信号是「同一时刻 + 不同维度名」：
			// 同一条原句拆出的字段时间戳必然相同，而不同维度名正是
			// 漂移发现报告里的「值班分机号 / 停用旧号 / 旧分机号」。
			// 真正的时序替代会有严格的时间差（真库里是 13:28 → 13:44）。
			if a.sameOf == "" && b.sameOf == "" &&
				a.fact.When.Equal(b.fact.When) &&
				a.fact.Dimension != b.fact.Dimension &&
				a.fact.Subject == b.fact.Subject {
				continue
			}
			if supersedes(a.fact, b.fact) {
				dropped[a.hit.Block.ID] = true
				superseded = append(superseded, a.hit)
				supersededByOther = true
				break
			}
		}
		if !supersededByOther {
			kept = append(kept, a.hit)
		}
	}

	// 规则 3：**不重排**，保持调用方给的向量序。
	//
	// ★ 这是实测打回来的：第一版在这里按 (时间升序, 分数降序) 重排，
	// 结果端到端探针 5/7 → 0/7 —— 因为迁移来的整句块时间戳最早
	// （13:20:43），时间升序把它们全顶到 top3：
	//
	//	仲裁后 top3: 随时追问细节 | order-gw 运维进展 | 每天
	//
	// 时间只该用来**判断取代关系**（哪条更新），不该决定**展示顺序** ——
	// 展示顺序是相关性问题，由向量分决定。仲裁的职责是剔除被取代的块，
	// 不重排。
	//
	// 这与 RecallBlocks「按余弦排序」的既有契约是同一件事：
	// 那是召回层的语义，仲裁层不该改。
	//
	// Superseded 同理：剔除的块按时间升序便于人工核对「谁取代了谁」。
	sortHitsByTimeThenScore(superseded)

	status := "no-arbitration"
	switch {
	case len(superseded) > 0:
		status = "superseded"
	case len(kept) < len(hits):
		status = "partial"
	}
	return ArbitrationResult{Kept: kept, Superseded: superseded, Status: status}
}
