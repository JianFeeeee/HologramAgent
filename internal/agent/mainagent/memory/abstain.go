package memory

import (
	"fmt"
	"strings"
)

// 拒答判据。
//
// ★ 为什么不用分数阈值
// ---------------------
// 上一轮实测过「真实查询 top1 分数」与「编造查询 top1 分数」的可分性：
//
//	真实  0.6731 / 0.6390 / 0.8041 / 0.6435   最低 0.6390
//	编造  0.4320 / 0.5231 / 0.5890 / 0.5923   最高 0.5923
//	间隔  0.047   n=4
//
// 形式上「可分」，但那是**运气不是能力** —— 0.6390 与 0.5923 分别是
// 「4 个里最差的」，任一侧多一个样本间隔就可能变负。
//
// 而符号存在性是**确定性事实**（strings.Contains），没有浮点抖动，
// 且编造的定义本身就是「库里没有」⇒ 符号必然零命中：
//
//	真实  0.50 / 0.33 / 0.33 / 0.67      全部 > 0
//	编造  0.00 × 6                        全部 = 0
//
// 新增样本（sentry / postgres）依然全 0。
//
// ★ 但它有一个必须承认的局限
// ----------------------------
// 「符号零命中」≠「库里没有相关信息」。泛指词查询会误伤：
//
//	问「那个跑得久的任务」—— 符号可能全零 ⇒ 被拒答（误伤）
//
// 所以泛指词存在时**必须豁免**：那时符号提取本身失效，
// 拒答的判据不成立，不该用它做决定。

// vaguePointers 是泛指词 —— 它们让「符号提取」这个前提失效。
//
// ★ 命中任一就豁免拒答，理由不是「宽松」而是「判据不成立」
//
// ★★ 疑问词（什么/哪个/多少）**不在**这里
// ------------------------------------------
// 第一版把它们也放进泛指词，判据立刻抓到问题：
//
//	「grafana 监控面板的端口是多少」 → 豁免（命中「多少」）
//
// 而那**正是一个编造查询**，应该被拒答。疑问词不影响符号提取 —
// 「端口是多少」照样能提出「端口」。
//
// 真正的区别是：
//
//	泛指词  那个/这个/它/之前  指向不明，无法定位到具体对象
//	疑问词  什么/哪个/多少    只问属性，主体仍然明确
var vaguePointers = []string{
	"那个", "这个", "那些", "这些", "它们", "他们", "它",
	"之前", "以前", "当时", "刚才", "上次", "前面",
	"还有", "另外", "别的",
}

// ★ 判据的能力边界（实测得出，必须写在这里）
// ------------------------------------------
// 「符号零命中 ⇒ 拒答」**无法区分**下面两种查询——
// 它们的符号形态完全一样，都是跨词边界的伪词：
//
//	「grafana 监控面板的端口是多少」 → [grafana 监控面板]  库里真没有
//	「本机服务监听哪些端口」         → [本机服务 监听哪些]  库里有 13010/8081
//
// 两者都零命中。库规模也不是区分信号（3 块库与 1391 块库结果相同）。
//
// ⇒ **零命中只在「查询含数字串/版本串」时才敢拒答。**
//
//	含精确串时零命中的含义明确（那个串库里确实没有）；
//	纯中文时零命中分不清「真没有」与「提取失败」。
//
// 代价（明确承认）：纯中文的编造查询会逃过拒答。
// 但那个方向**安全** —— 拒答的代价（误拒真实查询，让模型答不出
// 已记录的事实）远大于放过的代价（多召回点噪音，模型自己会判断）。
// ⇒ 这是一道**故意偏向召回**的不对称门。
const minSymbolHitRatio = 1.0

// exactSymbolRequired 表示：纯中文查询（无可提取的精确串）不参与拒答。
const exactSymbolRequired = true

// hasExactSymbol 判断符号集里是否含精确数字串/版本串。
func hasExactSymbol(syms []string) bool {
	for _, s := range syms {
		if isNumericSymbol(s) {
			return true
		}
	}
	return false
}

// AbstainCheck 判断一次查询是否应当拒答。
//
// 返回 (拒答?, 符号覆盖率, 命中的符号)。
func AbstainCheck(query string, blocks []MemoryBlock) (bool, float64, []string) {
	// ★ 泛指词豁免：前提不成立时不拒答
	for _, w := range vaguePointers {
		if strings.Contains(query, w) {
			return false, -1, []string{"含泛指词「" + w + "」，符号提取失效，不拒答"}
		}
	}

	syms := QuerySymbols(query)
	if len(syms) == 0 {
		// 一个符号都提不出来 ⇒ 同样不能据此拒答（前提不成立）
		return false, -1, []string{"查询未提取到符号，不拒答"}
	}

	matched := make([]string, 0, 4)
	valid := 0
	for _, s := range syms {
		if len([]rune(s)) < 2 {
			continue // 单字不成符号
		}
		valid++
		lowered := strings.ToLower(s)
		for _, b := range blocks {
			if b.Text == "" {
				continue
			}
			if strings.Contains(strings.ToLower(b.Text), lowered) {
				matched = append(matched, s)
				break
			}
		}
	}
	if valid == 0 {
		return false, -1, []string{"符号全为单字，不拒答"}
	}

	// ★ 纯中文查询不拒答（见上面的能力边界说明）：
	// 零命中分不清「真没有」与「滑窗伪词提取失败」。
	if exactSymbolRequired && !hasExactSymbol(syms) {
		return false, -1, []string{"查询无可提取的精确串，纯中文零命中无法区分真无与伪词，不拒答"}
	}

	ratio := float64(len(matched)) / float64(valid)
	// ★ 只看**精确串**是否命中，不看整体覆盖率。
	//
	// 实测故障：「本机 13010 服务」在库里只有 8081 时被误拒 ——
	// 因为要求「全部符号命中」，而「本机」命中了、「13010」没命中，
	// 覆盖率 0.33 < 1.0 ⇒ 拒答。但 13010 才是决定性的那个符号。
	//
	// 精确串的语义是「存在与否」：查 13010 而库里没有 ⇒ 该拒答。
	// 而中文片段（跨词边界的伪词）覆盖率没有判据价值。
	for _, s := range syms {
		if !isNumericSymbol(s) || len([]rune(s)) < 2 {
			continue
		}
		lowered := strings.ToLower(s)
		for _, b := range blocks {
			if b.Text != "" && strings.Contains(strings.ToLower(b.Text), lowered) {
				return false, ratio, matched // 精确串命中 ⇒ 不拒答
			}
		}
	}
	return true, ratio, matched
}

// RecallBlocksGuarded 是**带拒答的融合召回**：拒答在图库层，
// 任何调用路径都必经它。
//
// ★ 为什么拒答必须在这一层而不是上层
// -------------------------------------
// 第一版把拒答放在 `internal/agent/agent/toolcall.go` 里
// （recallByBlocks 召回前先查一次），结果：
//
//	生产路径  core → AbstainCheck → RecallBlocksFused    ✔ 有拒答
//	探针路径  probe_prod_test → RecallBlocksFused        ✗ 绕过了
//
// 而探针是判据 —— **判据测不到被测路径，判据就等于不存在**。
// 端到端探针跑出 abstention 0/3 时，生产路径其实是有拒答的，
// 但没人能证明它，因为判据没覆盖。
//
// 下沉到图库层后，两者走同一条路。
func (g *GraphDB) RecallBlocksGuarded(q BlockRecallQuery, query string) (
	[]FusedHit, *AbstainReason, ArbitrationResult, error) {

	all, err := g.MemoryBlocks()
	if err != nil {
		// 读不到块就不能判拒答（前提不成立）—— 直接召回，不猜
		hits, arb, rerr := g.RecallBlocksFused(q, query)
		return hits, nil, arb, rerr
	}
	if refuse, ratio, matched := AbstainCheck(query, all); refuse {
		return nil, &AbstainReason{
			Query: query, Ratio: ratio, Matched: matched,
			Notice: AbstainNotice(query, ratio, matched),
		}, ArbitrationResult{}, nil
	}
	// ★★★ 接 BFS 联想（2026-10-04，jieba 分词修好命中之后）
	//
	// 走过一段弯路，值得记下来：
	//
	//  第一次接（当时命中还是坏的）：coexist 仍 0/1。
	//      直接命中 top8 全是噪音，联想从噪音节点出发，
	//      而 13010 根本没进 top8 ⇒ 无从联想。
	//      ⇒ 当时的结论：「召回即联想救不了召回本身错了的情况」。
	//
	//  接着修好了命中（中文符号切分改用 jieba 词典分词），
	//  现在「本机服务监听哪些端口」能正确召回端口类块 ——
	//  此时联想才真正有意义：
	//
	//      「14010端口」这类块命中 ✓
	//      13010 的块文本是「http://127.0.0.1:13010」，
	//        **不含「端口」二字** ⇒ 符号路仍匹配不到
	//      但图上是连着的：Pi编码助手 --A2A端口--> 13010
	//      ⇒ 从命中节点沿边走一层就能找回它
	//
	// ★ 两句话合起来才是完整结论：
	//   ① 联想救不了错的命中
	//   ② 但命中修好之后，联想能补上**词面不重叠**的那部分
	//      —— 而后者恰恰是词法召回的固有边界（块文本与查询
	//         没有任何共同词，纯路径块就是这样）。
	hits, arb, rerr := g.RecallBlocksFusedBFS(q, query, bfsDepthForRecall)
	return hits, nil, arb, rerr
}

// bfsDepthForRecall 是生产召回的联想层数。
//
// ★ 为什么是 2
//
// 生产缺口是「端口类块 → 13010」，一层就够。
// 但真实提问常是「谁负责 X 的 Y」—— 主体与属性值可能都隔一层
// （人 --隶属--> 团队 --负责--> 服务），一层找不到。
//
// ★ 为什么不是 3：层数每加一预算就被稀释，且联想分数按 1/(1+d)
//
//	衰减，3 层的结果分数已很低，而工具预算固定
//	—— 加深度不如加精度。
const bfsDepthForRecall = 2

// AbstainReason 是一次拒答的完整记录（含可展示给模型的说明）。

// AbstainNotice 是给模型看的拒答说明。
//
// ★ 为什么措辞是「没找到」而不是「不存在」
// --------------------------------------
// 「不存在」是**对世界的断言**，而我们只验证了「记忆库里没有」——
// 记忆库本来就不可能覆盖一切（这一轮就实测到生产库 98 个块里
// 98 个是图像块）。把检索失败说成事实不存在，会让模型在
// 别处找到答案时认为「记忆系统说它不存在」，从而忽略正确来源。
func AbstainNotice(query string, ratio float64, matched []string) string {
	// ★★★ 两条分支必须带**同一句**限定（2026-10-04）
	//
	// 原实现两条分支文案质量差很大：
	//
	//	部分命中 → 「命中记忆库符号 [本机]（覆盖率 25%）」
	//	零命中   → 「记忆库里没有与「…」相关的记录。注意这**只说明记忆库没有**…」
	//
	// ★★ 第一条有两个问题：
	//
	//	1) 自相矛盾 —— 「命中了」却「拒答」，读起来像 bug。
	//	   实际语义是「**决定性的那个精确串**没命中」，
	//	   而泛词命中与否不参与判定（见 AbstainCheck 的契约）。
	//	2) ★ 它**没有那句关键限定**。
	//	   模型读到「命中记忆库符号」会以为库里有相关记录，
	//	   于是转头去猜答案 —— 而拒答的全部意义就是让它别猜。
	//
	// ⇒ 统一成「决定性精确串未命中」的口径，两条都带限定句。
	if len(matched) > 0 {
		return fmt.Sprintf(
			"记忆库里没有与「%s」直接对应的记录"+
				"（只泛泛提到过 %v，但**确定这个具体对象的记录没有**）。"+
				"注意这**只说明记忆库没有**，不代表该事实不存在 —— "+
				"若用户明确指出过，请以用户的话为准并建议其重新提供。",
			query, matched)
	}
	return fmt.Sprintf(
		"记忆库里没有与「%s」相关的记录。"+
			"注意这**只说明记忆库没有**，不代表该事实不存在 —— "+
			"若用户明确指出过，请以用户的话为准并建议其重新提供。",
		query)
}
