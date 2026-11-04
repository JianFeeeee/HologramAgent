// 向量 + 符号融合召回。
//
// ★ 为什么必须融合而不是 fallback
// -------------------------------
// 现状（toolcall.go:236）是「块向量在前，符号路兜底」：块一旦有命中
// 就直接 return，符号路的 RecallSorted **根本没被调用**。
// 而实测（生产快照 1391 块，chineseclip 512 维）证明向量侧会失手：
//
//	查询「本机 13010 端口对应什么」
//	  向量 top8  → 「13000端口」「本地网关8081」…（都不含 13010）
//	  词法命中   → 「13010/13011 而非 12011」「http://127.0.0.1:13010」
//
// 设计注释里写的「端口号、分机号这类纯数字串向量天然弱」是对的，
// 但**没有真正生效** —— 架构让符号路没有补位机会。
//
// ★ 三层根因里的第三层
// ----------------------
//  1. 各向异性       → 已修（RebuildCentroid 补上生产调用者）
//  2. 精确串被稀释   → 向量模型固有限制，本文件解决它
//  3. 符号路无补位   → **本文件**
//
// 融合不是「两路结果拼在一起」，而是**给每个候选算一个统一分数**：
// 只有一路命中的候选仍可能被另一路拉上来，而不是排在后面。
package memory

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// FusedHit 是融合后的候选。
type FusedHit struct {
	BlockID   string
	Text      string
	Score     float64 // 统一分数
	VectorHit float64 // 向量路分数（0 = 未命中）
	SymbolHit float64 // 符号路分数（0 = 未命中）
	// ExactHit 表示「查询里的精确数字串/版本串命中了本块」。
	//
	// ★ 这是**布尔信号**不是强度信号：它的价值在于「存在与否」。
	//   混进加权和会被低权重稀释（实测：sym=1.0×0.3=0.30
	//   输给 vec=0.62 + sym=0.33 → 0.534），所以单独一路置顶。
	ExactHit bool
	Why      []string // 可读的归因，给调试与模型解释用
}

// FusionWeights 是两路的权重。
//
// ★ 为什么默认向量主导（0.7/0.3）而不是对半
// ------------------------------------------
// 符号路在**长尾中文实体名**上非常强（「本批第181批的值班手册是第几版」
// 这种问题 jieba+LIKE 就能定位），但它对**同义改写**毫无办法
// （「脚本路径改到哪了」vs「/data/homeagent」零词法重叠）。
// 向量恰好相反。
//
// 取 0.7/0.3 而不是 0.5/0.5：符号路的误报率更高（LIKE 命中泛词
// 就会带出一堆无关块），而它的强项在 2 层能靠「符号命中即置顶」
// 的规则体现，不必靠权重放大。
type FusionWeights struct {
	Vector float64 // 默认 0.7
	Symbol float64 // 默认 0.3
}

func defaultWeights() FusionWeights { return FusionWeights{Vector: 0.7, Symbol: 0.3} }

// 融合排序的符号提取规则。
//
// ★ 不能直接 jieba：memory 里记过「jieba + LIKE 把『服务』『端口』
// 这类泛词当过滤词，造成大量噪音」。所以符号路只认**高信息量**的词：
//   - 数字串（端口、版本号、日期、批次号）
//   - 英文标识符（plugindev、grafana、kafka）
//   - 中文长词（≥2 字，且不是泛词表里的）
var (
	symbolNumericRe = regexp.MustCompile(
		`\d+(?:[./]\d+)*(?:\.\d+)?%?|[vV]\d+(?:\.\d+)+`)
	symbolLatinRe  = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_-]{2,}`)
	symbolCJKRunRe = regexp.MustCompile(`[一-鿿]+`)
)

// cjkWindow 是中文符号的切分窗口。
//
// ★ 为什么不用词法切分而用滑窗
// -----------------------------
// 正确的做法是 jieba 之类的分词，但 memory 里记过它的问题：
// 「jieba + LIKE 把『服务』『端口』这类泛词当过滤词，造成大量噪音」——
// 它把「监控面板的端口是多少」切成「监控/面板/的/端口/是/多少」，
// 而块文本里的词边界与它不一致，两边对不齐。
//
// ★ 而贪婪长串匹配更糟（实测）
// -----------------------------
//
//	「grafana 监控面板的端口是多少」 → [grafana 监控面板的端口是多少]
//	「脚本路径改到哪个目录了」      → [脚本路径改到哪个目录了]
//
// 整句变成**一个**符号，而任何块都不可能包含整句 ⇒ 符号分恒为 0，
// 符号路对**所有纯中文查询完全失效**（只有含数字串的探针能被救）。
//
// 折中：2~4 字滑窗 + 泛词黑名单。宁可多提候选（靠覆盖率阈值压制
// 噪声），也不要「一个符号都没有」。
const (
	cjkWindowMin = 2
	cjkWindowMax = 4
)

// symbolStopWords 是符号路的泛词黑名单 —— 命中它们不作为信号。
var symbolStopWords = map[string]bool{
	"服务": true, "端口": true, "地址": true, "配置": true, "问题": true,
	"记忆": true, "时间": true, "地方": true, "东西": true, "什么": true,
	"怎么": true, "哪个": true, "是否": true, "可以": true, "需要": true,
	"文件": true, "目录": true, "路径": true, "用户": true, "系统": true,
}

// cjkFunctionWords 是 jieba 分出来的**虚词** —— 它们是词，但不携带语义。
//
// ★ 与 symbolStopWords 的区别（这是 2026-10-04 才想清楚的）
// ------------------
//
//	symbolStopWords  滑窗的跨词伪词（2~4 字重叠窗口）
//	cjkFunctionWords 词典分词认出的虚词
//
// 「端口」曾在 symbolStopWords 里，那是错的 —— 它是实词。
// 但「什么」「哪些」是真的虚词，即便 jieba 认得出它们，
// 作为召回信号也没有区分度（库里有「什么」的块很多，
// 而「哪些」的命中几乎覆盖全库）。
//
// ★ 所以两套标准分开：jieba 分出来的词过这张**虚词**表，
//
//	滑窗产出的候选过 symbolStopWords。
var cjkFunctionWords = map[string]bool{
	"什么": true, "哪些": true, "哪个": true, "怎么": true, "如何": true,
	"是否": true, "可以": true, "需要": true, "应该": true, "必须": true,
	"的": true, "了": true, "吗": true, "呢": true, "吧": true,
	"和": true, "与": true, "或": true, "及": true, "等": true,
	// ★ 语气词（2026-10-04）：判据 TestFuse_无符号退化为向量序 用「嗯」
	//   构造"查询提不出符号"的场景，而 jieba 会把它切出来 ——
	//   于是那条判据测不到退化路径了（它绿着，但测的不是目标行为）。
	//   ★ 这是「修了 A 弄坏 B」的典型：单看每处都对，合起来错。
	"嗯": true, "啊": true, "呀": true, "哦": true, "噢": true,
	"欸": true, "喂": true, "哎": true, "唔": true,
}

// QuerySymbols 提取查询里的高信息量符号。
func QuerySymbols(query string) []string {
	out := make([]string, 0, 8)
	seen := map[string]bool{}
	add := func(s string) {
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, m := range symbolNumericRe.FindAllString(query, -1) {
		add(m)
	}
	for _, m := range symbolLatinRe.FindAllString(query, -1) {
		add(strings.ToLower(m))
	}
	// ★★★ 中文用 jieba 切，滑窗只作降级（2026-10-04）
	//
	// 为什么不用滑窗（它曾是这个函数的唯一实现）：
	//
	//	查询「本机服务监听哪些端口」
	//	  滑窗 → ["本机服务", "监听哪些"]   ← 全是跨词伪词
	//	  jieba → [本机 服务 监听 哪些 端口] ← 「端口」独立成词
	//
	// ★ 这是生产探针 coexist 0/1 的**真正根因**：
	//   「端口」从未单独成窗 ⇒ 符号路对「14010端口」全打 0 分
	//   ⇒ 直接命中全是噪音 ⇒ 图联想也无从谈起
	//   （详见 RecallBlocksFusedBFS 的注释：联想救不了召回本身错了）
	//
	// ★ 为什么两者都产出
	//
	//   jieba 有词典依赖（词库目录）。GetJieba() 拿不到词库时返回 nil，
	//   此时若只靠 jieba，符号会**整个消失** ⇒ 符号路彻底失效。
	//   所以：jieba 成功则用它 + 保留滑窗作为补充；
	//   失败则纯滑窗（退化成今天的行为，不会更差）。
	//
	// ★ 停用词在这里仍然生效（jieba 分出来的虚词会被 add 过滤）：
	//   「哪些」「什么」这类词 jieba 也会切出来，不加过滤会被当信号。
	cjkRuns := symbolCJKRunRe.FindAllString(query, -1)
	cut := GetJieba()
	usedJieba := false
	if cut != nil {
		for _, run := range cjkRuns {
			for _, w := range cut.Cut(run, true) {
				w = strings.TrimSpace(w)
				if w == "" || cjkFunctionWords[w] {
					continue
				}
				// ★★ 停用词表**只对滑窗生效**，对 jieba 不生效。
				//
				// 理由：jieba 是**词典分词**，「端口」「服务」被切成
				// 独立词，是因为它们在语料里确实是词 ——
				// 它们携带语义，不是伪词。
				//
				// 而滑窗切出来的「服务」可能只是「本机服务监听」里
				// 恰好跨界的 2 字串，那种是伪词，该滤。
				//
				// ★ 混用两套标准会出问题：2026-10-04 试过把
				//   「端口/服务」从停用词表移出（数据上它们命中
				//   只占 0.8%，并不泛），但因为**滑窗**同时被改成
				//   全子窗，于是噪音涌入、casual 从 3/4 掉到 2/4。
				//   ⇒ 那次回归的根因不是停用词表，是滑窗。
				//
				// 现在滑窗保持原样（全子窗没启用），jieba 独立成词
				// 不受停用词限制 —— 两套标准分开，问题不会互相污染。
				// ★ 只收**单个词**，不收 jieba 的组合结果
				//   （Cut(hmm=true) 只切不组，理论上不会有组合；
				//   这里防御性过滤，避免将来误用 hmm=false 时把整句当符号）。
				add(w)
			}
		}
		usedJieba = len(out) > 0
	}
	// ★ 滑窗补充：jieba 缺词时仍能给出重叠窗口候选。
	//   注意滑窗产出**会**包含停用词过滤（cjkWindows 内部已过滤）。
	if !usedJieba {
		for _, run := range cjkRuns {
			for _, w := range cjkWindows(run) {
				add(w)
			}
		}
	}
	return out
}

// cjkWindows 把一段连续中文切成 2~4 字滑窗候选。
//
// ★ 去重与裁剪
// ------------
// 同一段中文会产出大量重叠窗口（「监控面板的」→ 监控/控面板/面板的…），
// 但只有**非重叠前缀**保留，避免符号集被同质候选塞满：
//
//	「监控面板的」→ 监控控/控面板/面板的（步长 2，max-1 = 3）
//
// 泛词在调用侧被 symbolStopWords 过滤。
func cjkWindows(run string) []string {
	r := []rune(run)
	if len(r) < cjkWindowMin {
		return nil
	}
	// 去掉句尾的虚词尾巴（「的」「了」「吗」等）——
	// 它们几乎不可能是块内容的一部分
	for len(r) > cjkWindowMin && isTrailingParticle(r[len(r)-1]) {
		r = r[:len(r)-1]
	}
	out := make([]string, 0, 8)
	n := len(r)
	for i := 0; i < n; i++ {
		for l := cjkWindowMax; l >= cjkWindowMin; l-- {
			if i+l > n {
				continue
			}
			w := string(r[i : i+l])
			if symbolStopWords[w] {
				continue
			}
			out = append(out, w)
			// 只取该位置最长的一个窗口，往后步进，避免重叠候选
			i += l - 1
			break
		}
	}
	return out
}

func isTrailingParticle(r rune) bool {
	switch r {
	case '的', '了', '吗', '呢', '啊', '呀', '吧', '是', '在', '个':
		return true
	}
	return false
}

// SymbolScore 用查询符号在块文本里的出现情况打分（0~1）。
//
// ★ 精确串命中权重远高于中文词
// --------------------------------
// 正是因为「13010」这种精确串是唯一能救回向量失手的信号，
// 它的命中必须给满分，而不是和其它词按个数平均。
func SymbolScore(symbols []string, text string) (score float64, matched []string, exact bool) {
	if len(symbols) == 0 || text == "" {
		return 0, nil, false
	}
	lower := strings.ToLower(text)
	exactHit, wordHit := 0, 0
	for _, s := range symbols {
		if strings.Contains(lower, strings.ToLower(s)) {
			if isNumericSymbol(s) {
				exactHit++
			} else {
				wordHit++
			}
			matched = append(matched, s)
		}
	}
	if exactHit > 0 {
		// 有精确数字串命中 ⇒ 满分 + exact 标记。
		// ★ 1.0 在调用方被当作**布尔信号**用（直接置顶），
		//   不参与加权和 —— 否则会被 Symbol 权重稀释。
		return 1.0, matched, true
	}
	// 中文/英文词按覆盖比例给分，不叠加（避免「命中多就满分」）。
	if wordHit > 0 {
		r := float64(wordHit) / float64(len(symbols))
		if r > score {
			score = r
		}
	}
	return score, matched, false
}

func isNumericSymbol(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if unicode.IsDigit(r) {
			return true
		}
	}
	// v2.31.5 这类版本串：首字母 + 数字
	if (s[0] == 'v' || s[0] == 'V') && len(s) > 1 {
		for _, r := range s[1:] {
			if unicode.IsDigit(r) {
				return true
			}
		}
	}
	return false
}

// fuseCandidates 把向量候选与符号候选合并成一个统一排序。
//
// ★ 与仲裁的顺序：融合在**前**，仲裁在**后**
// ---------------------------------------------
// 仲裁判断「谁取代了谁」依赖向量的相关性来决定「给模型看哪些」，
// 若先仲裁后融合，仲裁剔除的块可能正是符号路能救回来的那条。
//
// 注意 candidates 必须来自**未截断**的全量候选集 ——
// 向量路已经把正确块挤掉的话，融合也无从救回（与仲裁同一条理由）。
func fuseCandidates(vectorHits []BlockHit, allBlocks []MemoryBlock,
	query string, w FusionWeights) []FusedHit {

	symbols := QuerySymbols(query)
	if len(symbols) == 0 {
		// 无符号可提 ⇒ 退化成纯向量序（但仍标注 why，便于调试）
		out := make([]FusedHit, 0, len(vectorHits))
		for _, h := range vectorHits {
			out = append(out, FusedHit{
				BlockID: h.Block.ID, Text: h.Block.Text,
				Score: h.Score * w.Vector, VectorHit: h.Score,
				Why: []string{"仅向量命中（查询未提取到符号）"},
			})
		}
		return out
	}

	// 符号路在全量块里找命中（不是只在向量候选里找 ——
	// 那样「向量没召回但符号命中」的块根本没有机会上场）
	// ★ 必须用**索引**而不是 &out[i] 指针。
	//
	// 实测 bug：符号路阶段会 append（向量未召回但符号命中的块），
	// append 触发扩容后切片换了底层数组，之前存的 &out[i] 全部指向
	// 旧数组 —— 于是给已有候选写 SymbolHit 时写进了废弃副本，
	// 分数永远算不出来。判据直接抓到了这个：目标块符号分 0.333
	// 而非满分。
	//
	// 同理第一遍循环里那个 `byID[h.Block.ID] = &f`（f 是循环内临时变量）
	// 也是无效指针，一并去掉。
	idxOf := make(map[string]int, len(vectorHits))
	out := make([]FusedHit, 0, len(vectorHits)+8)
	for _, h := range vectorHits {
		idxOf[h.Block.ID] = len(out)
		out = append(out, FusedHit{
			BlockID: h.Block.ID, Text: h.Block.Text,
			VectorHit: h.Score,
		})
	}

	seenSymbol := map[string]bool{}
	for _, b := range allBlocks {
		if seenSymbol[b.ID] {
			continue
		}
		sc, matched, exact := SymbolScore(symbols, b.Text)
		if sc <= 0 {
			continue
		}
		seenSymbol[b.ID] = true
		if i, ok := idxOf[b.ID]; ok {
			out[i].SymbolHit = sc
			out[i].ExactHit = exact
			out[i].Why = append(out[i].Why,
				fmt.Sprintf("符号命中 %v", matched))
		} else {
			// ★ 向量没召回但符号命中 —— 这就是融合要救的那一类
			out = append(out, FusedHit{
				BlockID: b.ID, Text: b.Text,
				SymbolHit: sc, ExactHit: exact,
				Why: []string{
					fmt.Sprintf("仅符号命中（向量未召回）：%v", matched),
				},
			})
		}
	}

	for i := range out {
		f := &out[i]
		// ★ 精确串命中直接置顶，不参与加权。
		//
		// 实测 bug：只符号命中且符号满分（精确数字串）的块
		//   score = 1.0 × w.Symbol(0.3) = 0.300
		// 竟然输给「符号分只有 0.333」的向量候选：
		//   score = 0.620×0.7 + 0.333×0.3 = 0.534
		//
		// 根因：**满分被低权重稀释了**。而精确串命中正是符号路
		// 不可替代的能力（向量对「13010」这种串天然弱），
		// 它的价值在于「存在与否」，不在于「有多少」。
		//
		// 所以 SymbolScore 给的 1.0 是布尔信号，这里必须按布尔用。
		if f.ExactHit {
			// 向量分只做平手时的微调（不改变「已置顶」这个事实）
			f.Score = 1.0 + f.VectorHit*0.01
			continue
		}
		if f.SymbolHit > 0 && f.VectorHit == 0 {
			f.Score = f.SymbolHit * w.Symbol
			continue
		}
		f.Score = f.VectorHit*w.Vector + f.SymbolHit*w.Symbol
	}

	// 稳定排序：分数降序；同分保持向量原序（不引入额外的不确定性）
	sortFused(out)
	return out
}

func sortFused(out []FusedHit) {
	// 插入排序 —— 候选规模是百级，无需 sort.Slice 的反射开销，
	// 且完全稳定（不依赖 sort.SliceStable 的额外分配）。
	for i := 1; i < len(out); i++ {
		x := out[i]
		j := i - 1
		for j >= 0 && out[j].Score < x.Score {
			out[j+1] = out[j]
			j--
		}
		out[j+1] = x
	}
}

// RecallBlocksFused 是**融合召回的对外入口**。
//
// 与 RecallBlocksWithArbitration 的区别：
//   - RecallBlocksWithArbitration：向量召回 + 时序仲裁（排序仍是余弦序）
//   - RecallBlocksFused：向量召回 + 符号路融合 + 时序仲裁
//
// 返回的 FusedHit 带 Why（归因），便于模型理解「为什么这条被召回」。
func (g *GraphDB) RecallBlocksFused(q BlockRecallQuery, query string) (
	[]FusedHit, ArbitrationResult, error) {

	q2 := q
	q2.TopK = largeTopK
	vecHits, err := g.RecallBlocks(q2)
	if err != nil {
		return nil, ArbitrationResult{}, err
	}
	// 全量块（供符号路扫描）—— 只取有文本的（图像块无符号可匹配）
	all, err := g.MemoryBlocks()
	if err != nil {
		return nil, ArbitrationResult{}, err
	}
	// ★★ 符号路必须也按 fingerprint 过滤 —— 这是一个真实的安全缺陷。
	//
	// 判据立刻抓到（toolcall_block_test.go:124）：
	//
	//	块  {Text: "旧空间的内容", Fingerprint: "old-fp"}
	//	查询「旧空间的内容」
	//	⇒ 期望不召回，实际召回了（找到 1 条）
	//
	// 根因：符号路拿的是 `MemoryBlocks()` 的**全部**带文本块，
	// 而 RecallBlocks 会按 fingerprint 跳过不匹配的块。
	// 文本匹配不依赖向量空间，所以「旧空间的块」在符号路原形毕露 ——
	// 换向量空间后旧块仍会污染结果，正是这条判据要防的事。
	textBlocks := make([]MemoryBlock, 0, len(all))
	for _, b := range all {
		if b.Text == "" {
			continue
		}
		// 指纹不匹配的块一律不进符号路候选
		if q.Fingerprint != "" && b.Fingerprint != "" && b.Fingerprint != q.Fingerprint {
			continue
		}
		textBlocks = append(textBlocks, b)
	}

	fused := fuseCandidates(vecHits, textBlocks, query, defaultWeights())

	// 融合 → 仲裁：仲裁在融合之后，且要喂进 FusedHit 的 id
	vecOrder := make([]BlockHit, 0, len(fused))
	byID := make(map[string]MemoryBlock, len(fused))
	for _, f := range fused {
		byID[f.BlockID] = MemoryBlock{ID: f.BlockID, Text: f.Text}
		vecOrder = append(vecOrder, BlockHit{
			Block: byID[f.BlockID], Score: f.Score,
		})
	}
	arb := arbitrate(g, vecOrder)
	superseded := map[string]bool{}
	for _, h := range arb.Superseded {
		superseded[h.Block.ID] = true
	}

	kept := make([]FusedHit, 0, len(fused))
	for _, f := range fused {
		if superseded[f.BlockID] {
			continue
		}
		// ★ MinScore 按**路**应用，不是套在融合分上。
		//
		// 接入时我曾把 MinScore 直接传 0（理由「量纲变了」），
		// 结果两条判据立刻红了：
		//
		//	toolcall_block_test.go:80  低分块不该出现（MinScore 应滤掉）
		//	toolcall_block_test.go:124 指纹不匹配的块不该被召回
		//
		// ★ 那等于**放弃了噪声过滤** —— 「量纲变了」是事实，
		//   但解法不是丢掉阈值，而是让阈值作用在它该作用的那一路上。
		//
		// 融合分有三段不同量纲：
		//   精确串命中  ≥1.00        布尔置顶，不过滤
		//   纯符号命中  0 ~ w.Symbol 字面强信号
		//   纯向量命中  0 ~ w.Vector×余弦  ← MinScore 说的就是这个
		//
		// 而指纹不匹配的块压根不在候选里（RecallBlocks 已跳过），
		// 它的失败另有原因 —— 见下方说明。
		switch {
		case f.ExactHit:
			// 精确串命中是决定性信号，不受 MinScore 约束
		case f.VectorHit > 0:
			// 有向量分时才用 MinScore 过滤（避免把纯符号命中误杀）
			if q.MinScore > 0 && f.VectorHit < q.MinScore {
				continue
			}
		default:
			// 只有符号分、无向量分：靠覆盖率判断，不套向量阈值
			if f.SymbolHit <= 0 {
				continue
			}
		}
		kept = append(kept, f)
	}
	if k := effectiveTopK(q.TopK); len(kept) > k {
		kept = kept[:k]
	}
	return kept, arb, nil
}

// ═══════════════════════════════════════════════════════════════
//  RecallBlocksFusedBFS —— 召回即联想（2026-10-04）
//
//  ★★ 这个入口是为了验证一个假设：
//
//  生产探针 coexist 0/1：查询「本机服务监听哪些端口」召不回 13010。
//  直觉是「召回该联想」—— 命中节点沿边走一层，找回图上相连的上下文。
//
//  ★ 实测结论：**假设不成立**，而且原因值得记下来。
//
//  调试记录（生产库）：
//
//	直接命中 top8 全是噪音（CPU总线… / sdk/introduce 站部署 / …）
//	联想确实在工作（从噪音节点联出了 5 个）
//	但 13010 **根本没进 top8** ⇒ 联想无从谈起
//
//  往下挖，真正的根因是**符号切分**：
//
//	查询「本机服务监听哪些端口」的符号只有
//	["本机服务","监听哪些"] —— 全是跨词伪词
//	「端口」被中划窗「每位置只取最长窗口」吃掉
//	⇒ 符号路对「14010端口」全打 0 分
//
//  ⇒ ★★ **召回即联想救不了召回本身错了的情况**：
//     联想从**已命中**节点出发，而命中节点本身就是错的。
//     图联想只能补上下文，不能修命中。
//
//  所以本函数**不接入生产路径**，作为独立入口保留：
//  修好命中质量之后，它才有意义。
// ═══════════════════════════════════════════════════════════════

// bfsExpansionBudget 是 BFS 追加节点的总预算上限。
//
// ★ 为什么必须有预算：图不是树 —— 关系边双向、稠密。
//
//	无预算的 BFS 在大图上会把整个连通分量拖进来，
//	而 memory_recall 的工具预算是有限的
//	（历史上曾一次返回 193 个实体、13744 tokens，是预算的 436%）。
//
// ★ 它约束「**追加**的联想节点」，不含直接命中 ——
//
//	直接命中已由 q.TopK 控制。
const bfsExpansionBudget = 60

// RecallBlocksFusedBFS 在向量 + 符号的直接命中之外，
// 对每个命中节点做一层 BFS，把联想到的节点按**发现顺序**追加到尾部。
//
// ★ 追加而非置顶：联想是补充，不是答案。
//
//	混进相关性排序会污染「离查询多近」这个信号。
//
// ★ 联想节点按 base/(1+d) 衰减，越远越弱但仍 > 0
//
//	（否则会被下游 MinScore 滤掉）。
//
// ★ 预算耗尽即停，且已产出的部分照常返回 ——
//
//	截断不该让整次召回失败。
func (g *GraphDB) RecallBlocksFusedBFS(q BlockRecallQuery, query string, depth int) (
	[]FusedHit, ArbitrationResult, error) {

	hits, arb, err := g.RecallBlocksFused(q, query)
	if err != nil {
		return nil, arb, err
	}
	if depth <= 0 || len(hits) == 0 {
		return hits, arb, nil
	}

	seen := make(map[string]bool, len(hits))
	for _, h := range hits {
		seen[h.BlockID] = true
	}
	base := 0.0
	for _, h := range hits {
		if h.Score > base {
			base = h.Score
		}
	}
	if base <= 0 {
		base = 1.0
	}

	appended := 0
	for _, seed := range hits {
		if appended >= bfsExpansionBudget {
			break
		}
		// ★ 真正逐层 BFS（2026-11-04 修）。
		//
		// 原实现是 `for d := 1; d <= depth; d++ { ...; break }` ——
		// 循环体末尾无条件 break，所以 d 循环**只执行一次**；而且
		// NeighbourBlocks 始终传 `seed.BlockID`，每层取的都是同一批直接邻居。
		//
		// 后果两层：
		//   ① depth 参数形同虚设，永远不会走到第 2 层
		//   ② staticcheck SA4008「循环变量在条件里从不改变」—— 这正是它的价值：
		//      它看出来了 `d` 除了打分没有参与寻址
		//
		// 现在 frontier 逐层推进：第 d 层从第 d-1 层新发现的节点继续。
		frontier := []MemoryBlock{{ID: seed.BlockID, Text: seed.Text}}
		for d := 1; d <= depth; d++ {
			var next []MemoryBlock
			for _, cur := range frontier {
				neighbors, err := g.NeighbourBlocks(cur.ID)
				if err != nil {
					// 联想失败不该让整次召回失败 —— 主路结果仍然有用。
					break
				}
				for _, nb := range neighbors {
					if seen[nb.ID] {
						continue
					}
					seen[nb.ID] = true
					if appended >= bfsExpansionBudget {
						return hits, arb, nil
					}
					hits = append(hits, FusedHit{
						BlockID: nb.ID,
						Text:    nb.Text,
						Score:   base / float64(1+d),
						Why: []string{"联想：" + cur.Text + " → " + nb.Text +
							fmt.Sprintf("（%d 层）", d)},
					})
					appended++
					next = append(next, nb)
				}
			}
			if len(next) == 0 {
				break // 该层无新节点 ⇒ 再深也没有
			}
			frontier = next
		}
	}
	return hits, arb, nil
}

// NeighbourBlocks 返回 startID 的**直接邻居**（一层，双向，按发现顺序）。
//
// ★ 与 BFSBlocks 的区别：那个按 depth 逐层展开，
//
//	这里专供召回链逐层调用 —— 因为要按层给不同打分。
//
// ★ 只沿**关系边**展开，不沿 contains 结构边：
//
//	contains 连的是「原句块 → 字段块/关系边」，那是实现细节，
//	把它当语义边会让原句块（长文本）混进召回结果、抢占预算。
func (g *GraphDB) NeighbourBlocks(startID string) ([]MemoryBlock, error) {
	if startID == "" {
		return nil, nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()

	rows, err := g.db.Query(
		`SELECT DISTINCT
		        CASE WHEN e.source_id = ? THEN e.target_id ELSE e.source_id END AS peer
		 FROM memory_block_edges e
		 WHERE (e.source_id = ? OR e.target_id = ?)
		   AND e.source_kind = 'block' AND e.target_kind = 'block'
		   AND e.edge_type != 'contains'
		   AND COALESCE(e.status, '') != 'deleted'
		 ORDER BY e.created_at ASC, e.id ASC`, startID, startID, startID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return g.blocksByIDsLocked(ids)
}
