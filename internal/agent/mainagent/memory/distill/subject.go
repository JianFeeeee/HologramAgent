package distill

import (
	"strings"
)

// 主语推导（属性型 + 一句多主语）。
//
// ★ 为什么必须改，而不能只调提示词
// --------------------------------
// Split 里有这么一段：
//
//	if subject == "" {
//	    continue   // 无主语直接跳过
//	}
//
// 实测基线（真实 qwen3:1.7b + 6 条真库记录，见 goldcases_test.go）：
// **1/5 通过**。四条失败全是 `subject == ""`：
//
//	主语=""  属性型-分机号新值    "值班室分机号 4324，值班 老周（…）"
//	主语=""  属性型-分机号旧值    "值班室分机号 4379，值班人 阿李"
//	主语=""  属性型-改述句        "下周起值班室分机号改为 4324，…"
//	主语=""  多服务复合句         "admin服务端口8861·billing服务端口8499·…"
//
// 唯一通过的「叙述型-批次多字段」有「第183批」这个显式主语。
// **结论：LLM 的拆分能力和 schema 约束都是好的，坏的是主语只认「第N批」。**
//
// 而且这不是提示词能解决的：1.7b 处理不了「从这句话里找出主语」这种
// 隐含约束（同 promptTemplate 注释里记的「不要做 X」实测把 4 字段变 0 字段）。
// 主语必须在 Go 侧用规则推。

// Subject 是从记录里推导出的一个主语。
type Subject struct {
	// Name 是主语名（如「值班室分机号」「admin服务」）。
	Name string
	// Anchor 是主语在原句里的锚点位置（rune 下标），用于把「字段值」
	// 归属到正确的主语。一句多主语时靠它区分。
	Anchor int
	// Span 是主语覆盖的 rune 区间 [Start, End)，值应当落在这个区间之后。
	// 多主语复合句（admin/billing/oauth）靠它切分。
	Start, End int
}

// 维度词：属性名里表示「这个句子在说什么」的词。这些词后面通常跟值。
//
// 为什么要这份词表：属性型记录的形态是「<属性名> <值>，<属性名2> <值2>」，
// 属性名本身没有显式标记，只能靠词表识别。词表里的词都是实测语料里
// 出现过的属性名成分，不是凭空造的。
var dimensionHeads = []string{
	"分机号", "端口", "值班人", "值班", "告警规则", "容量预警", "排期",
	"值班手册", "版本", "状态", "时间", "时长", "比例", "窗口",
	"地址", "账号", "密码", "联系人", "负责人", "数量", "阈值",
}

// 主体词：复合句里「谁拥有这个属性」的部分。
//
// 用于切分 "admin服务端口8861·billing服务端口8499·oauth服务端口8271" ——
// 三个主体各带一个端口，必须拆成三组三元组，不能混成一条。
var subjectHeads = []string{
	"服务", "节点", "实例", "集群", "系统", "平台", "网关", "队列",
	"订单", "用户", "支付", "结算", "网关",
}

// KnownSubjectAliases 把实测出现过的属性名归一到受控词表。
//
// 与 dimensionAliases 同一套思路：不归一就建不了边。但这里归一的是
// **主语**（如「值班室分机号」vs「值班分机号」），主语不一致会让同一件事
// 在图里裂成两个互不相关的节点。
var knownSubjectAliases = map[string]string{
	"值班室分机号": "值班室分机号",
	"值班分机号":  "值班室分机号",
	"分机号":    "值班室分机号",
	"值班室电话":  "值班室分机号",
	"值班电话":   "值班室分机号",
	// 真库实测的句子型记录（**第四种形态**）：
	//
	//	「连接池从 32/64 扩到 128/256，等待队列长度告警阈值 300~500」
	//
	// 句首是「连接池」而不是属性名，但它确实是这段话的主语。不加它
	// DeriveSubjects 推不出主语 → 字段全被闸门拦掉（零三元组）。
	//
	// ★ 这条是跑真库才发现的：金标准原先 6 条全都落在三种示例形态内
	// （批次型/属性型/复合型），判据太贴合示例，于是「第四种形态就交白卷」
	// 这个缺陷完全看不见。真库 6/6 零字段逼出了它。
	"连接池":  "连接池",
	"等待队列": "等待队列",
	"队列":   "等待队列",
}

// DeriveSubjects 从一条记录推导主语列表。
//
// 返回空切片表示「推不出主语」—— 调用方据此不产出三元组（宁可少记，
// 不写没有主语的悬空属性）。
//
// 三条推导规则，按优先级：
//  1. 显式「第N批」→ 单一主语（叙述型，原有行为）
//  2. 属性名开头（「<属性名> <值>」）→ 该属性名作主语（属性型，缺的主路径）
//  3. 复合分隔符（·、，）里每段各自带主体 → 多主语（一句多组三元组）
func DeriveSubjects(record string) []Subject {
	runes := []rune(record)

	// 规则 1：显式批次号
	if s := defaultSubject(record); s != "" {
		idx := indexOfRunes(runes, []rune(s))
		if idx < 0 {
			idx = 0
		}
		return []Subject{{Name: s, Anchor: idx, Start: 0, End: idx + len([]rune(s))}}
	}

	// 规则 2 与规则 3 的先后**实测无影响**（变异自证：调换顺序后 6 条
	// 金标准用例的主语推导结果完全一致）—— 因为两者互斥：
	// deriveMultiSubject 要求「≥2 段含主体词」，而属性型记录
	// 「值班室分机号 4324，值班 老周」里没有任何主体词，它本来就推不出主语。
	//
	// （早先这里写的是「顺序反了会把属性型句拆成 值班/旧号 两个假主语」——
	//  那是 indexOfRunes 未找到返回 0 的 bug 造成的假象：中文主体词在 ASCII
	//  串上全部误命中位置 0。bug 修好后该现象不复存在，注释同步改正。）
	//
	// 规则 2：句首是**受控别名**（“值班室分机号…”）→ 整句一个主语。
	// 必须是受控别名而不是启发式猜出来的：启发式会把
	// “admin服务端口8861” 的句首也当成属性名（它在第一个数字处切，
	// 切出 “admin服务端口” 含“端口”），于是复合句永远轮不到规则 3。
	if s := deriveControlledSubject(runes); s != "" {
		return []Subject{{Name: s, Anchor: 0, Start: 0, End: len([]rune(s))}}
	}

	// 规则 3：复合句里每段自带主体（“admin服务端口8861·billing服务端口8499…”）
	if subs := deriveMultiSubject(runes); len(subs) > 0 {
		return subs
	}

	// 规则 4：句首不是受控名但形如“<名词><值>”时，用启发式试一次。
	if s := deriveAttributeSubject(runes); s != "" {
		return []Subject{{Name: s, Anchor: 0, Start: 0, End: len([]rune(s))}}
	}
	return nil
}

// deriveControlledSubject 只认 knownSubjectAliases 里的受控名（句首前缀）。
//
// 与 deriveAttributeSubject 的区别是**宁可推不出**：启发式会把任何
// “句子开头到第一个数字之间”都当属性名，包括 “admin服务端口8861” 这种
// 复合句的**首段**。而首段是复合句的一部分，不该独占主语。
func deriveControlledSubject(runes []rune) string {
	// 按别名长度降序试，保证命中最长的那个（“值班室分机号” 优先于 “分机号”）。
	best := ""
	for alias := range knownSubjectAliases {
		if len([]rune(alias)) <= len(best) {
			continue
		}
		if len(alias) > len(runes) {
			continue
		}
		if indexOfRunes(runes, []rune(alias)) == 0 {
			best = alias
		}
	}
	if best == "" {
		return ""
	}
	return knownSubjectAliases[best]
}

// deriveMultiSubject 处理 "admin服务端口8861·billing服务端口8499·oauth服务端口8271"。
//
// 切分依据是「主体词 + 属性词」的重复模式：每段开头是主体（「admin服务」
// 「billing服务」），后面跟属性词（「端口」）和值。
//
// ★ 主体边界不能包含属性词：取「最靠前的属性词」之前的内容会把属性词也
// 吃进主语，得到 "admin服务端口" 而不是 "admin服务" —— 实测踩过。
// 正确做法是找**主体词**的位置，主体 = 段首到主体词末尾；找不到主体词时
// 才退回属性词位置（段首本身就是主体名的情况）。
//
// 为什么必须走这条路而不是当整体一条：三个服务的端口混在一句里，向量会
// 把三个数字平均掉 —— 实测查询「admin 服务的端口」能命中（因为整句都含
// admin），但一旦拆成三元组，admin→8861 就是一条精确的边。
func deriveMultiSubject(runes []rune) []Subject {
	// 按复合分隔符切段
	seps := map[rune]bool{'·': true, '、': true, '；': true, ';': true}
	var segs [][]rune
	cur := []rune{}
	for _, r := range runes {
		if seps[r] {
			segs = append(segs, cur)
			cur = []rune{}
			continue
		}
		cur = append(cur, r)
	}
	segs = append(segs, cur)
	if len(segs) < 2 {
		return nil
	}

	// 至少两段同时含「主体词」才认定是一句多主语 —— 否则只是普通分句。
	hits := 0
	for _, seg := range segs {
		if containsAnyRune(seg, subjectHeads) {
			hits++
		}
	}
	if hits < 2 {
		return nil
	}

	var subs []Subject
	offset := 0
	for _, seg := range segs {
		// 主体 = 段首到主体词末尾；找不到主体词才退回属性词位置。
		subject := ""
		attrIdx := indexOfAnyRune(seg, dimensionHeads)
		if i := indexOfAnyRune(seg, subjectHeads); i >= 0 {
			subject = string(seg[:i+len([]rune(matchedHead(seg, subjectHeads)))])
		} else if attrIdx > 0 {
			subject = string(seg[:attrIdx])
		}
		subject = strings.Trim(subject, "，, 　·")
		if subject == "" || attrIdx < 0 {
			offset += len(seg) + 1
			continue
		}
		subs = append(subs, Subject{
			Name:   subject,
			Anchor: offset + attrIdx,
			Start:  offset,
			End:    offset + len(seg),
		})
		offset += len(seg) + 1
	}
	return subs
}

// matchedHead 返回 seg 中最先命中的那个主体词（用于确定主体右边界）。
func matchedHead(seg []rune, heads []string) string {
	bestIdx, bestWord := -1, ""
	for _, h := range heads {
		if i := indexOfRunes(seg, []rune(h)); i >= 0 && (bestIdx < 0 || i < bestIdx) {
			bestIdx, bestWord = i, h
		}
	}
	return bestWord
}

// deriveAttributeSubject 处理 "值班室分机号 4324，值班 老周（…）"。
//
// 形态：句首是属性名（命中 dimensionHeads 或 knownSubjectAliases），
// 属性名本身就是主语 —— 「分机号是多少」这类查询要能命中它。
func deriveAttributeSubject(runes []rune) string {
	// 从句首找最长的属性词
	for i := 0; i < len(runes) && i < 12; i++ {
		// 逐步加长，匹配最长的属性名
		for j := len(runes); j > i; j-- {
			cand := string(runes[i:j])
			if canonical, ok := knownSubjectAliases[cand]; ok {
				return canonical
			}
		}
	}
	// 属性名不是词表里的受控名时，用「句首连续名词 + 后接值」的启发式：
	// 取句首到第一个数字/逗号之间的部分。
	cut := len(runes)
	for i, r := range runes {
		if i > 0 && (isDigit(r) || r == '，' || r == ',' || r == ' ') {
			cut = i
			break
		}
	}
	if cut == 0 || cut > 12 {
		return ""
	}
	cand := strings.TrimSpace(string(runes[:cut]))
	if canonical, ok := knownSubjectAliases[cand]; ok {
		return canonical
	}
	// 至少含一个已知属性词成分才算数，否则是「均仅评审通过」这类纯叙述
	if !containsAnyRune([]rune(cand), dimensionHeads) {
		return ""
	}
	if canonical, ok := knownSubjectAliases[cand]; ok {
		return canonical
	}
	return cand
}

// indexOfRunes 返回 needle 在 haystack 中的起始下标，未找到返回 -1。
//
// ★ 契约：未找到必须是 -1，不能是 0。
// 初版写的是「n == 0 || n > len(haystack) 时 return 0」—— 把「needle 比
// haystack 长」当成了「命中在位置 0」。后果是主体词表里的中文词
// （“节点”/“队列”/“网关”…）在 ASCII 串 "admin服务端口8861" 上全部
// 报「命中于 index 0」，主语被截成 "ad"/"bi"/"oa"。
// 判据要求“未找到”和“找到第一处”是两件可区分的事，否则所有 >= 0 的
// 判断全部退化成真。
func indexOfRunes(haystack, needle []rune) int {
	n := len(needle)
	if n == 0 || n > len(haystack) {
		return -1
	}
	for i := 0; i+n <= len(haystack); i++ {
		match := true
		for j := 0; j < n; j++ {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func containsAnyRune(haystack []rune, needles []string) bool {
	for _, n := range needles {
		if indexOfRunes(haystack, []rune(n)) >= 0 {
			return true
		}
	}
	return false
}

func indexOfAnyRune(haystack []rune, needles []string) int {
	best := -1
	for _, n := range needles {
		if i := indexOfRunes(haystack, []rune(n)); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	return best
}
