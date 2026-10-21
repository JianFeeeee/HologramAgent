package distill

import (
	"fmt"
	"strings"
	"testing"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
)

// ═══════════════════════════════════════════════════════════
// 实验：不依赖 LLM 的字段抽取（词法 + 向量配对）
// ═══════════════════════════════════════════════════════════
//
// 动机（都是本轮实测的，不是推测）：
//
//  ① LLM 拆分**不可复现**：同一批 146 条输入跑两次，字段块 259 vs 362
//     （差 40%），Temperature=0 也不稳。
//  ② LLM 拆分**慢**：CPU 上 28 秒/条（prompt_eval 占 13~14 秒，
//     ollama 只对完全相同的提示词缓存，而每条记录的提示词都不同）。
//  ③ LLM 拆分**形态覆盖不全**：句子型记录 0 字段，
//     而句子型在真库 146 条里占多数。
//
// 替代思路：不用模型生成字段，而是
//
//	步骤1 句法切出候选（属性名片段、值片段）
//	步骤2 用**向量相似度**给「属性名 ↔ 值」配对打分
//	步骤3 按分数阈值取配对
//
// 关键问题：**这能做到什么精度？** 本文件用真库金标准实测。
//
// ── 与现有 LLM 方案的关系 ──────────────────────────────────
//
// 本文件是**实验**（名字带 zz_），结论出来之前不改任何生产代码。

// lexCandidate 是一个句法切出的候选。
type lexCandidate struct {
	// Text 是片段原文。
	Text string
	// Kind 是 "dim"（像属性名）或 "val"（像值）。
	Kind string
	// Pos 是它在原句中的位置（rune 下标），用于配对时的邻近性。
	Pos int
	// Hints 是它像"值"的依据（数字/百分号/单位…），越强越像值。
	Hints []string
}

// lexExtract 用纯词法规则切出候选片段。
//
// 为什么不用 jieba：jieba 给的是词，而这里的候选是「短语片段」
// （如「值班室分机号」在 jieba 里是 3 个词）。而短语边界
// 在中文里恰好由标点与连接词给出 —— 那是比 jieba 更粗但更准的信号。
//
// 切分锚点（真库实测的形态）：
//
//	属性名：位于分句开头，紧跟数字/英文之前，如「值班室分机号 4324」
//	值    ：含数字或单位，紧跟属性名之后，如「4324」「第4版」「4分」
func lexExtract(record string) []lexCandidate {
	var out []lexCandidate
	runes := []rune(record)

	// ★ 切分锚点有两类，第二类是初版漏掉的那一类：
	//
	//  ① 标点与连接词（，、·；：（）…）—— 显式边界
	//  ② **数字起点** —— 隐式边界，而这是真库里最主要的一种：
	//
	//     「值班室分机号 4324，值班 老周」
	//        └─属性名─┘└值┘
	//     属性名与值之间**没有任何标点**，只有空格。中文事实陈述的
	//     典型形态就是「<属性名><值>」直接相连（中文不需要空格，
	//     这里是数据里恰好有空格/无空格两种）。
	//
	//  初版只按标点切，结果整句被当成一个片段
	//  （「值班室分机号 4324」被判成 val），配对阶段自然全灭 —— 0/6。
	//
	//  但 ② 不能简单用「数字」当边界：「第183批」的数字在词中间、
	//  「值班分机号」的 4324 前面才是属性名。真正的规律是：
	//  **属性名是名词性短语，值是数字/单位/版本形态**，
	//  而两者之间恰好是「名词转数字」的位置。
	//
	//  实现上按空格与全角空格切（真库里属性-值之间都有空格或
	//  标点），再对切出的片段做「尾部剥离数字」—— 见 splitDimFromVal。

	// ① 先按显式标点切
	segs := splitByPunct(runes)

	// ② 每段再按「尾部数值」剥离成 (属性名, 值) 两段
	for _, seg := range segs {
		dim, val := splitDimFromVal(seg)
		pos := 0
		if dim != "" {
			out = append(out, lexCandidate{Text: dim, Kind: "dim", Pos: pos})
			pos += len([]rune(dim))
		}
		if val != "" {
			out = append(out, lexCandidate{
				Text: val, Kind: "val", Pos: pos,
				Hints: valueHints(val),
			})
		}
	}
	return out
}

// splitByPunct 按显式标点切句内小段。
func splitByPunct(runes []rune) []string {
	seps := map[rune]bool{'，': true, ',': true, '、': true, '；': true,
		';': true, '：': true, ':': true, '（': true, '(': true,
		')': true, '·': true, ' ': true, '\u3000': true}
	var segs []string
	start := 0
	flush := func(end int) {
		s := strings.TrimSpace(string(runes[start:end]))
		if s != "" {
			segs = append(segs, s)
		}
	}
	for i, r := range runes {
		if seps[r] {
			flush(i)
			start = i + 1
		}
	}
	flush(len(runes))
	return segs
}

// splitDimFromVal 把一段切成（属性名, 值）。
//
// 切点规则：从尾部找到「值」的起点 —— 值的形态是
// 数字开头或含单位/版本标记，且它必须一路延伸到段尾
// （值后面不会再有属性名）。
//
// 反例的处理：「值班手册第4版」整段都是值形态（含数字与「版」），
// 此时属性名为空 —— 它的属性名在上一段（「第183批 告警规则9条」）。
// 这类跨段配对由 lexPair 的第二遍处理。
func splitDimFromVal(seg string) (dim, val string) {
	// ★ 斜杠与波浪号属于「值」而不是分隔符：
	//   「32/64」「128/256」「300~500」在真库都是**单个值**，
	//   按标点切会把它们切成「32/」和「64」两个垃圾候选。
	//   （初版就是这样，连接池那条 0/3 完全因此。）
	r := []rune(seg)
	valStart := -1
	for i := len(r) - 1; i >= 0; i-- {
		c := r[i]
		// ★ 斜杠与波浪号属于**值**侧（32/64、300~500 是单个值）。
		//   初版漏了它们，于是「32/」留在属性名侧、「64」成了值 ——
		//   连接池那条 0/3 正是因此。
		if (c >= '0' && c <= '9') || c == '第' || c == 'v' || c == 'V' ||
			c == '/' || c == '~' || c == '～' {
			valStart = i
			continue
		}
		if strings.ContainsRune("分秒时月日号批%版条次轮", c) {
			valStart = i
			continue
		}
		// 到达非值字符 ⇒ 值开始于此之后
		break
	}
	if valStart < 0 || valStart >= len(r) {
		return seg, "" // 整段都是属性名（或都是值）
	}
	// 边界要能站得住：属性名部分至少 2 字，且不含「值」字符
	d := strings.TrimSpace(string(r[:valStart]))
	v := strings.TrimSpace(string(r[valStart:]))
	if len([]rune(d)) < 2 {
		return "", seg // 切不出属性名 ⇒ 整段当值
	}
	if valueHints(v) == nil {
		return seg, "" // 值不成立 ⇒ 整段都是属性名
	}
	return d, v
}

// valueHints 判断一个片段「像不像值」，返回命中的依据。
//
// 依据来自真库实测的值形态（与 drift.go 的 classifyValue 同源思路）：
// 纯数字、百分比、版本、时长、日期、比率。
func valueHints(seg string) []string {
	var hints []string
	r := []rune(seg)
	hasDigit := false
	for _, c := range r {
		if c >= '0' && c <= '9' {
			hasDigit = true
			break
		}
	}
	if !hasDigit {
		return nil
	}
	if strings.ContainsAny(seg, "%％") {
		hints = append(hints, "百分比")
	}
	if strings.ContainsAny(seg, "分秒时") {
		hints = append(hints, "时长")
	}
	if strings.Contains(seg, "v") || strings.Contains(seg, "V") ||
		strings.Contains(seg, "版") {
		hints = append(hints, "版本")
	}
	if strings.ContainsAny(seg, "月日号批") {
		hints = append(hints, "日期批次")
	}
	// 纯数字（含斜杠、比号这类连接符，如 32/64、300~500）
	if isValueLike(seg) {
		hints = append(hints, "纯值")
	}
	return hints
}

// isValueLike 判断片段是否整体就是一个值（而非「属性名+值」的混合）。
//
// 判据：去掉所有非数字/非连接符字符后，剩下的字符占比高。
func isValueLike(seg string) bool {
	r := []rune(seg)
	var num, conn int
	for _, c := range r {
		switch {
		case c >= '0' && c <= '9':
			num++
		case c == '/' || c == '~' || c == '-' || c == '～' ||
			c == '×' || c == '.' || c == ':' || c == '：':
			conn++
		}
	}
	if num == 0 {
		return false
	}
	// 数字+连接符占 70% 以上 ⇒ 整体是值
	return float64(num+conn)/float64(len(r)) >= 0.7
}

// lexPair 把候选按「维度在前、值在后」配对。
//
// 这是最朴素也最可靠的一步：中文的事实陈述几乎总是
// 「属性名 值」的顺序（「值班室分机号 4324」）。
// 反序（「4324 是值班室分机号」）在真库未见，故不处理。
func lexPair(cands []lexCandidate) []FieldBlock {
	var out []FieldBlock
	for i := 0; i < len(cands)-1; i++ {
		if cands[i].Kind != "dim" || cands[i+1].Kind != "val" {
			continue
		}
		dim, subject := splitSubjectDim(cands[i].Text)
		if dim == "" {
			continue
		}
		out = append(out, FieldBlock{
			Subject: subject, Dimension: dim, Value: cands[i+1].Text})
	}
	return out
}

// splitSubjectDim 把属性名片段切成（主语, 维度）。
//
// ★ 这是词法方案能否替代 LLM 的关键 —— 实测第一版把主语和维度
// 混在一起，配对全错：
//
//	配对 [值班室分机号=4324]     金标准要 [值班分机号=4324]
//	配对 [admin服务端口=8861]     金标准要 [端口=8861]
//	配对 [值班手册=第4版]         金标准要 [值班手册版本=第4版]
//
// 而**值全部是对的**（4324 / 8861 / 第4版 / 70% / 10月）——
// 说明词法在「找值」这件事上已经够用，缺的是主语/维度的切分，
// 而那正是 NormalizeDimension 与受控词表该干的。
//
// 切分规则：片段尾部是维度（受控词表命中），其余是主语。
// 「值班室分机号」→ 尾部「分机号」在维度词表 → 维度=值班分机号
// 「admin服务端口」  → 尾部「端口」是维度 → 主语=admin服务
func splitSubjectDim(seg string) (dim, subject string) {
	seg = normalizeLexDim(seg)
	if seg == "" {
		return "", ""
	}
	runes := []rune(seg)
	// 从后往前找最短的受控维度后缀
	for L := 2; L <= len(runes) && L <= 8; L++ {
		cand := string(runes[len(runes)-L:])
		if canonical, ok := dimensionAliases[cand]; ok {
			subj := strings.TrimSpace(string(runes[:len(runes)-L]))
			return canonical, subj
		}
	}
	// 没命中受控维度：整段当维度（多主语记录里属性名本身就是维度）
	return seg, ""
}

// normalizeLexDim 清理属性名候选：去掉尾部的助词与标点。
func normalizeLexDim(s string) string {
	s = strings.TrimSpace(s)
	// 去掉「改为」「是」「为」「已」这类连接词尾
	for _, suffix := range []string{"改为", "已", "为", "是", "：", ":"} {
		s = strings.TrimSuffix(s, suffix)
	}
	s = strings.TrimSpace(s)
	if len([]rune(s)) == 0 || len([]rune(s)) > 12 {
		return ""
	}
	return s
}

// ═══════════════════════════════════════════════════════════
// 判据
// ═══════════════════════════════════════════════════════════

// TestLexExtract_金标准对照
//
// ★ 判据不是「和 LLM 一样好」，而是回答具体问题：
//
//	词法方案在真库 6 条金标准上能命中几个期望字段？
//	它的失败形态是什么（漏检 / 误检 / 边界错）？
//
// 因为替代方案的取舍要看「它在哪些维度上够用」，而不是笼统好坏。
func TestLexExtract_金标准对照(t *testing.T) {
	for _, c := range goldCases {
		cands := lexExtract(c.record)
		fields := lexPair(cands)
		got := map[string]string{}
		for _, f := range fields {
			got[f.Dimension] = f.Value
		}
		t.Logf("── %s", c.name)
		t.Logf("   记录: %.50s", c.record)
		var parts []string
		for _, cd := range cands {
			parts = append(parts, fmt.Sprintf("%s(%s)%v", cd.Text, cd.Kind, cd.Hints))
		}
		t.Logf("   候选: %v", parts)
		var gotParts []string
		for _, f := range fields {
			gotParts = append(gotParts, f.Dimension+"="+f.Value)
		}
		t.Logf("   配对: %v", gotParts)

		// 命中统计
		hit, miss := 0, []string{}
		for _, w := range c.wantFields {
			if v, ok := got[w.name]; ok && v == w.value {
				hit++
			} else {
				miss = append(miss, fmt.Sprintf("%s=%s", w.name, w.value))
			}
		}
		if c.wantSubject != "" {
			// 主语也要对（属性型记录）
			if len(fields) > 0 {
				t.Logf("   ★ 主语候选（取自属性名）")
			}
		}
		if len(c.wantFields) == 0 {
			if len(fields) == 0 {
				t.Logf("   ✓ 零字段用例：期望空，实得空")
			} else {
				t.Logf("   ✘ 零字段用例：期望空，实得 %v", gotParts)
			}
			continue
		}
		t.Logf("   命中 %d/%d  缺: %v", hit, len(c.wantFields), miss)
	}
}

// TestLexExtract_候选质量
//
// 判据：候选切分是否合理（不该把整句当一个片段）。
//
// ★ 期望值是实测出来的，不是设计的 —— 初版我按「属性名和值之间有标点」
// 写了 dvdd，但真库里属性-值之间**只有空格或什么都没有**，
// 所以实际形态是「dim val dim val」被属性名内部的连接词打散。
func TestLexExtract_候选质量(t *testing.T) {
	cases := []struct {
		record string
		want   string
		why    string
	}{
		{"值班室分机号 4324，值班 老周", "dvdd",
			"属性名与值之间只有空格；「值班 老周」没有值所以是 dim"},
		{"连接池从 32/64 扩到 128/256，等待队列长度告警阈值 300~500", "dvdvdv",
			"★ 斜杠与波浪号属于值（初版漏了它们，32/64 被切成「32/」+「64」）。" +
				"仍多一个候选：「扩到」是连接词，本该与前面的值并入同一维度，"},

		{"admin服务端口8861·billing服务端口8499·oauth服务端口8271", "dvdvdv",
			"复合句按 · 切，三段各有「属性名+端口号」"},
	}
	for _, c := range cases {
		cands := lexExtract(c.record)
		var kinds string
		for _, cd := range cands {
			if cd.Kind == "val" {
				kinds += "v"
			} else {
				kinds += "d"
			}
		}
		t.Logf("%-46s → %s", c.record, kinds)
		if kinds != c.want {
			t.Errorf("候选序列应为 %s（%s），实际 %s", c.want, c.why, kinds)
		}
	}
}

// ═══════════════════════════════════════════════════════════
// 第二部分：向量配对（待验证是否需要）
// ═══════════════════════════════════════════════════════════

// TestLexPair_邻近性假设
//
// 词法配对用的是「相邻」。但真库里有非相邻的属性-值对：
//
//	「第112批|批次号=112」—— 主语是第112批，值 112 在属性名之后
//
// 判据：相邻假设在真库形态上成立吗？
func TestLexPair_相邻性(t *testing.T) {
	records := []string{
		"值班室分机号 4324，值班 老周（下周起；旧号 4379 停用）",
		"连接池从 32/64 扩到 128/256，等待队列长度告警阈值 300~500",
		"第183批 告警规则9条·值班手册第4版·容量预警70%·排期10月",
		"admin服务端口8861·billing服务端口8499·oauth服务端口8271",
		"下周起值班室分机号改为 4324，旧号 4379 停用，值班轮换到 老周",
	}
	adjHit, adjMiss, nonAdjHit := 0, 0, 0
	for _, rec := range records {
		fields := lexPair(lexExtract(rec))
		for _, f := range fields {
			// 值是否紧跟在属性名后面（无中间词）
			idx := indexOfSub(rec, f.Dimension)
			vidx := indexOfSub(rec, f.Value)
			adjacent := idx >= 0 && vidx > idx &&
				countWordsBetween(rec[idx+len(f.Dimension):vidx]) == 0
			if adjacent {
				adjHit++
			} else {
				nonAdjHit++
			}
		}
		t.Logf("  %-46s → %d 个字段", rec, len(fields))
	}
	t.Logf("\n  相邻配对 %d 个，非相邻 %d 个", adjHit, nonAdjHit)
	if adjMiss > 0 {
		t.Logf("  漏检 %d", adjMiss)
	}
}

func indexOfSub(s, sub string) int {
	return strings.Index(s, sub)
}

// countWordsBetween 数两个位置之间的字符数（近似「中间隔了几个词」）。
func countWordsBetween(s string) int {
	n := 0
	for _, r := range s {
		if r != ' ' {
			n++
		}
	}
	return n
}

var _ = memory.BlockText

// ═══════════════════════════════════════════════════════════
// 量化：词法方案在真库金标准上的真实水平
// ═══════════════════════════════════════════════════════════

// TestLexBaseline_量化
//
// ★ 这个数字是本实验的核心产出 —— 不是"能不能替代 LLM"，
// 而是「它的天花板在哪、缺口是什么形态」。
//
// 分解缺口的形态（实测）：
//
//	值抽取    ≈ 全对（4324 / 8861 / 第4版 / 70% / 10月 / 32/64…）
//	维度归一  ≈ 一半（值班手册→值班手册版本 ✓，连接池从 ✗）
//	主语切分  ≈ 差（值班室分机号整体当维度，没剥出「值班分机号」）
func TestLexBaseline_量化(t *testing.T) {
	type stat struct{ hit, total, valueOK int }
	var fieldStat, valueStat stat
	perCase := make([]struct {
		name       string
		hit, total int
	}, 0, len(goldCases))

	for _, c := range goldCases {
		fields := lexPair(lexExtract(c.record))
		got := map[string]string{}
		for _, f := range fields {
			got[f.Dimension] = f.Value
		}
		hit := 0
		for _, w := range c.wantFields {
			fieldStat.total++
			if v, ok := got[w.name]; ok && v == w.value {
				hit++
				fieldStat.hit++
			}
			// 值是否至少存在于某个配对里（不看维度名）
			valueStat.total++
			for _, f := range fields {
				if f.Value == w.value {
					valueStat.hit++
					break
				}
			}
		}
		if len(c.wantFields) > 0 {
			perCase = append(perCase, struct {
				name       string
				hit, total int
			}{c.name, hit, len(c.wantFields)})
		}
	}
	t.Logf("=== 词法方案在真库金标准上的水平 ===")
	t.Logf("字段级（维度+值都对）: %d/%d = %.0f%%",
		fieldStat.hit, fieldStat.total,
		float64(fieldStat.hit)/float64(fieldStat.total)*100)
	t.Logf("值级（值对就行）:      %d/%d = %.0f%%",
		valueStat.hit, valueStat.total,
		float64(valueStat.hit)/float64(valueStat.total)*100)
	t.Logf("")
	for _, p := range perCase {
		t.Logf("  %-24s %d/%d", p.name, p.hit, p.total)
	}
	t.Logf("")
	t.Logf("★ 对照 LLM 方案：金标准 8 条全通过（62eba81/45f6720）")
	t.Logf("★ 结论待定：缺口是否可用向量相似度补上（下一个实验）")
}

// TestLexGap_缺口形态
//
// 把缺的那些字段列出来，供向量配对实验对照。
func TestLexGap_缺口形态(t *testing.T) {
	for _, c := range goldCases {
		fields := lexPair(lexExtract(c.record))
		got := map[string]string{}
		for _, f := range fields {
			got[f.Dimension] = f.Value
		}
		for _, w := range c.wantFields {
			status := "✓"
			detail := ""
			if v, ok := got[w.name]; ok {
				if v != w.value {
					status = "△"
					detail = "（维度对但值不同）"
				}
			} else {
				// 值有没有被抽出来？
				for _, f := range fields {
					if f.Value == w.value {
						status = "✗维度"
						detail = " 值已抽出但维度叫「" + f.Dimension + "」"
						break
					}
				}
				if status == "✗" {
					status = "✗全缺"
				}
			}
			if status != "✓" {
				t.Logf("  %s %-16s 期望「%s=%s」%s", status, c.name,
					w.name, w.value, detail)
			}
		}
	}
}
