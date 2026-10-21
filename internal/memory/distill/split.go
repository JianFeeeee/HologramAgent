// Package distill implements triple extraction from raw conversation records
// using a small generation model.
//
// 设计依据（2026-10-02 实测，60 条 order-gw 真实运维记录）
//
// 为什么不用现有的 nlp.Extractor：实测 extractKeyTriples 产出 0 条
// （defaultParser 为 nil，POS 模板抽不出），库里 188 个实体全部由模型主动调
// memory_commit 写入整句复合值。后果是跨维度检索全失效 ——
// 「第181批的值班手册是第几版」这类问题基线 0/5。
//
// 为什么必须用 JSON Schema 约束（这是小模型能用的唯一机制）：
//
//	format: schema 对象   6/6 记录零幻觉，30 对字段
//	format: "json" 字符串  只输出单个对象就 stop（0 对）
//	零样本文本提示         聊天腔「根据您提供的信息…」（0 对）
//	few-shot 续写          结构不匹配时复读示例答案（100% 幻觉）
//
// 三道不可省的闸门：
//  1. 值必须原样出现在原文（唯一的幻觉闸门 —— 生成侧没有向量可验证）
//  2. 维度名归一（不归一就建不了边：实测「停机」/「停机时间」并存）
//  3. 主语锚定（拆开后主语与值变成独立实体，答案会丢 —— 规则拆分实测 0/20）
package distill

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/pkg/generation"
)

// fieldsSchema 约束解码目标。实测这是 1.7b 零幻觉的唯一形态。
const fieldsSchema = `{
  "type": "object",
  "properties": {
    "fields": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "name": {"type": "string"},
          "value": {"type": "string"}
        },
        "required": ["name", "value"]
      }
    }
  },
  "required": ["fields"]
}`

// promptTemplate 零样本 + **三示例**（每个示例代表一种记录形态）。
//
// ★ 为什么是三个而不是一个 —— 实测（真实 qwen3:1.7b + 6 条真库记录）
//
//	提示词形态            分机号新值  分机号旧值  复合句  批次型  纯叙述
//	单示例（批次型）           0        0       0      4  ✓    0 ✓
//	单示例（属性型）           3  ✓     2  ✓    0      0      0 ✓
//	无示例                    0        2       0      0      0 ✓
//	★ 三示例               4  ✓     2  ✓    3  ✓    4  ✓    0 ✓
//
// 单示例不管换成哪种形态都会**压掉另一半**：模型把示例的输出形态当成范本，
// 不像示例的句子直接交白卷 `{"fields":[]}`。三示例把三种形态都摆出来，
// 才不会锚定到某一类。这是 1.7b 的能力边界，不是提示词没调好 ——
// 无示例那一行同样说明「去掉示例」并不等于「去掉锚定」。
//
// 另外两条实测约束（都是踩出来的）：
//
//  1. **不能用 <record> 之外的裸文本**：无标签时实测 0 字段。
//  2. **不能写「批次号是主语，不要单独列成字段」这类"负向指令"**：
//     实测加上这条规则后，同一条记录从 4 个字段变成 0 个 —— 模型把
//     「值班手册第4版」也当成批次号相关而一并丢弃。1.7b 处理不了
//     「不要做 X」这种否定式约束，改成只给正例（示例里就没有批次号字段）。
//     主语锚定由 Split() 在 Go 侧做，不靠提示词约束模型。
const promptTemplate = `把下面的记录拆成字段，输出 JSON。

规则：
1. fields 每项是 {"name": 字段名, "value": 数值}
2. value 必须从记录里**原样照抄**，不许改写、换算、补全
3. 同一个含义在不同记录里必须用**完全一样**的字段名
4. 没有可拆的字段就输出 {"fields":[]}

记录：第183批 告警规则9条·值班手册第4版·容量预警70%·排期10月
输出：{"fields":[{"name":"告警规则数","value":"9条"},{"name":"值班手册版本","value":"第4版"},{"name":"容量预警","value":"70%"},{"name":"排期","value":"10月"}]}

记录：值班室分机号 4324，值班 老周
输出：{"fields":[{"name":"值班分机号","value":"4324"},{"name":"值班人","value":"老周"}]}

记录：admin服务端口8861·billing服务端口8499·oauth服务端口8271
输出：{"fields":[{"name":"端口","value":"8861"},{"name":"端口","value":"8499"},{"name":"端口","value":"8271"}]}

记录：连接池从 32/64 扩到 128/256，等待队列长度告警阈值 300~500
输出：{"fields":[{"name":"连接池容量","value":"32/64"},{"name":"连接池容量","value":"128/256"},{"name":"等待队列告警阈值","value":"300~500"}]}

<record>__RECORD__</record>
输出：`

type Field struct {
	Name  string
	Value string
}

// Extractor 把一条记录拆成 (主语, 维度, 值) 三元组。
type Extractor struct {
	gen generation.Provider
	// subjectFrom 由记录文本推导主语（通常是批次号）。返回空则不产出三元组，
	// 只把字段当作无主语的属性。
	subjectFrom func(record string) string
	// subjectsFrom 是多主语推导钩子。nil 时用内置规则 DeriveSubjects。
	subjectsFrom func(record string) []Subject
}

// deriveSubjects 解析本条记录的主语列表。
//
// 优先级：注入的 subjectsFrom → 内置规则 DeriveSubjects → 老的 subjectFrom。
// 保留 subjectFrom 是为了兼容既有调用方（NewExtractor 的第二个参数）。
func (e *Extractor) deriveSubjects(record string) []Subject {
	if e.subjectsFrom != nil {
		return e.subjectsFrom(record)
	}
	if subs := DeriveSubjects(record); len(subs) > 0 {
		return subs
	}
	if e.subjectFrom != nil {
		if s := e.subjectFrom(record); s != "" {
			return []Subject{{Name: s}}
		}
	}
	return nil
}

// SetSubjectsFrom 注入自定义的多主语推导（供测试与特殊场景）。
func (e *Extractor) SetSubjectsFrom(f func(record string) []Subject) {
	e.subjectsFrom = f
}

// NewExtractor 构造一个拆分器。subjectFrom 为 nil 时默认按「批次号」推导。
func NewExtractor(gen generation.Provider, subjectFrom func(string) string) *Extractor {
	if subjectFrom == nil {
		subjectFrom = defaultSubject
	}
	return &Extractor{gen: gen, subjectFrom: subjectFrom}
}

// defaultSubject 从记录里取批次号作主语（如「第183批」）。
//
// 为什么必须有主语：规则拆分实测 0/20 —— 拆开后「第183批」和
// 「值班手册第4版」是两个互不相关的实体，图中无边可循，答案就丢了。
// 主语是把这个绑定显式建成边的锚。
func defaultSubject(record string) string {
	// 取最靠前的「第N批」或「第N/M批」形态。
	//
	// 按 rune 迭代：中文是 3 字节，按 byte 比较会因溢出得到错误的结论。
	runes := []rune(record)
	for i := 0; i < len(runes); i++ {
		if runes[i] != '第' {
			continue
		}
		j := i + 1
		for j < len(runes) && (isDigit(runes[j]) || runes[j] == '~' ||
			runes[j] == '/' || runes[j] == '、') {
			j++
		}
		if j < len(runes) && runes[j] == '批' {
			return string(runes[i : j+1])
		}
	}
	return ""
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// Split 把一条记录拆成三元组。
//
// 返回的 triples 已过三道闸门：值原样校验、维度名归一、字段名长度限制。
// 全部字段被闸门拦下时返回空切片而非错误 —— 「这条记录没有可拆的字段」
// 是正常结果（例如「第129~143批均仅评审通过」这类纯叙述句），
// 与「模型没跑起来」是两回事，调用方必须能区分。
func (e *Extractor) Split(ctx context.Context, record string) ([]memory.Triple, error) {
	if strings.TrimSpace(record) == "" {
		return nil, nil
	}
	if e.gen == nil {
		return nil, fmt.Errorf("distill: no generation provider")
	}
	if !e.gen.Info().SupportsJSONSchema {
		// 静默忽略 schema 会让调用方拿到自由文本去 parse JSON ——
		// 那种失败查不出是哪一层的错。必须显式报。
		return nil, fmt.Errorf("distill: provider %q cannot honor json schema: %w",
			e.gen.Info().Model, generation.ErrSchemaUnsupported)
	}

	resp, err := e.gen.Generate(ctx, generation.Request{
		Prompt:      strings.Replace(promptTemplate, "__RECORD__", record, 1),
		MaxTokens:   256,
		Temperature: 0,
		JSONSchema:  fieldsSchema,
	})
	if err != nil {
		return nil, fmt.Errorf("distill: generate: %w", err)
	}

	fields, err := parseFields(resp.Text)
	if err != nil {
		// ★ 错误里必须带上原始响应。
		//
		// 实测故障（生产蒸馏 60 条 → 0 字段，837 秒）：判据只显示
		// 「零字段 60 条」，看不出是数据没得拆还是模型返回了垃圾。
		// 而根因是 ollama schema 污染 —— 模型返回
		//   {"fields":[{"name":"text","value":"commit f91b27a…"}]}
		// 这种「字段名=字段类型、值=整句」的垃圾，被三道闸门全拦掉。
		//
		// ★ **判据能显示 0，是因为它只看得到 0。**
		//   观测面不够 ⇒ 只能看到失败，看不到原因。
		raw := resp.Text
		if len(raw) > 400 {
			raw = raw[:400] + "…"
		}
		if resp.Truncated {
			// 截断的 JSON 不可救，也不该救 —— 重试比丢弃更划算（模型是随机的）。
			return nil, fmt.Errorf(
				"distill: output truncated at max tokens（原始输出 %d 字）: %w\n  原始: %s",
				len(resp.Text), err, raw)
		}
		return nil, fmt.Errorf(
			"distill: parse fields（原始输出 %d 字）: %w\n  原始: %s",
			len(resp.Text), err, raw)
	}

	subjects := e.deriveSubjects(record)
	var triples []memory.Triple
	for _, f := range fields {
		name := NormalizeDimension(f.Name)
		if name == "" {
			continue
		}
		// 闸门 1：值必须原样来自原文。
		if f.Value == "" || !strings.Contains(record, f.Value) {
			continue
		}
		// 闸门 3：字段名上限。超长的多半是把整句抄了进来当字段名。
		if len([]rune(name)) > 12 {
			continue
		}
		// 无主语时写不出 subject，直接跳过 —— 宁可少记也不写悬空属性。
		if len(subjects) == 0 {
			continue
		}
		// 字段归属到哪个主语？一句多主语时（如三个服务各自的端口），
		// 按值在原句里的位置落段：值落在哪个主语的锚点之后，就归那个主语。
		// 落不进任何区间时归最后一个 —— 宁可归属存疑，也不要丢字段。
		owner := subjects[len(subjects)-1]
		if valPos := indexOfRunes([]rune(record), []rune(f.Value)); valPos >= 0 {
			for _, sub := range subjects {
				if valPos >= sub.Anchor {
					owner = sub
				}
			}
		}
		// 自环过滤：主语已在三元组的位置，字段名等于主语则这条没有新信息。
		//
		// 提示词里写「批次号是主语，不要单独列成字段」会让 1.7b 把**所有**
		// 字段都当成批次号相关而丢弃（实测 4 字段 → 0 字段），所以那条
		// 负向指令已从提示词移除；代价就是它会把批次号也拆成字段。
		// 过滤放在 Go 侧，不依赖模型听懂否定指令。
		if name == owner.Name || f.Value == owner.Name {
			continue
		}
		triples = append(triples, memory.Triple{
			Subject:  owner.Name,
			Relation: name,
			Object:   f.Value,
			// 记原句，便于日后从图谱回到原文。
			SentenceText: record,
		})
	}
	return triples, nil
}

func parseFields(text string) ([]Field, error) {
	var envelope struct {
		Fields []Field `json:"fields"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		return nil, fmt.Errorf("invalid json: %w", err)
	}
	return envelope.Fields, nil
}

// dimensionAliases 把实测出现的同义异名归一到受控词表。
//
// 不归一的后果是建不了边：实测同一含义出现「停机」/「停机时间」两种命名，
// 跨记录查询时两种都要查，且边连不上。
//
// 加词时的判据：这个说法在真实语料里出现过，且确实是同一含义。
// 归一失败保持原名（不丢弃）—— 丢弃会丢信息。
var dimensionAliases = map[string]string{
	"停机时间":   "停机时长",
	"停机时长":   "停机时长",
	"停机":     "停机时长",
	"回滚版本":   "回滚版本",
	"回滚":     "回滚版本",
	"灰度比例":   "灰度比例",
	"灰度":     "灰度比例",
	"发布窗口":   "发布窗口",
	"发布时间":   "发布窗口",
	"时间":     "发布窗口",
	"值班手册":   "值班手册版本",
	"值班手册版本": "值班手册版本",
	"值班手册版":  "值班手册版本",
	"告警规则":   "告警规则数",
	"告警规则数":  "告警规则数",
	"容量预警":   "容量预警",
	"容量":     "容量预警",
	"排期":     "排期",
	"批次号":    "批次号",
	"批号":     "批次号",
	"端口":     "端口",
	"验证状态":   "验证状态",
	"评审状态":   "评审状态",
	"版本数":    "版本数",
	"日期":     "日期",
}

// NormalizeDimension 把字段名归一到受控词表。未命中时返回裁剪后的原名。
func NormalizeDimension(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Trim(name, "*·•-—:：,， ")
	if name == "" {
		return ""
	}
	if canonical, ok := dimensionAliases[name]; ok {
		return canonical
	}
	// 包含式匹配：模型有时输出「停机时长(分钟)」这类带后缀的写法。
	//
	// 挑**最长**匹配别名而不是最短：「停机时长」比「停机」长但更具体，
	// 先命中它才不会退化成更泛的「停机时长」（虽然此处二者同 canonical，
	// 但「排期」vs「排期月」这类一对多映射下，最长匹配才是对的规则）。
	//
	// ★ 但「最长匹配」本身有个反例，必须加防误伤：
	//
	//	NormalizeDimension("连接池容量") → "容量预警"   ← 错，两个不同维度
	//
	// 原因：「容量」是 2 字符泛词，命中了「连接池**容量**」的后半截。
	// 泛词跨词边界吞掉更具体的名字，会把两个真维度静默合并 —— 而维度名
	// 是建边的依据，合并后边就建错了。
	//
	// 判据：泛别名若只是**尾部子串**（name 的前缀是别的词），且前缀本身
	// 不在词表里，就认为是误伤，保持原名不归一。宁可留一个未归一的维度名
	// （还能被漂移发现机制报出来），也不要把两个维度错并成一个。
	bestAlias := ""
	for alias := range dimensionAliases {
		if len(alias) <= len(bestAlias) {
			continue
		}
		if !strings.Contains(name, alias) {
			continue
		}
		if isCrossWordTailMatch(name, alias) {
			continue
		}
		bestAlias = alias
	}
	if bestAlias != "" {
		return dimensionAliases[bestAlias]
	}
	return name
}

// isCrossWordTailMatch 判断这次包含匹配是否是「跨词边界吞掉泛词」。
//
// 命中位置 > 0 且前缀不含任何词表条目 ⇒ 前缀是另一个独立词，
// 于是整个 name 是「<独立词><泛词>」结构，属于被误伤，不该归一。
//
// 例：
//
//	"连接池容量" + "容量" → 前缀「连接池」不在维度词表 → 误伤
//	"停机时长(分钟)" + "停机时长" → 位置 0 → 不是尾部误伤 → 正常归一
//	"值班手册第8版" + "值班手册" → 位置 0 → 正常归一
func isCrossWordTailMatch(name, alias string) bool {
	i := strings.Index(name, alias)
	if i <= 0 {
		return false
	}
	prefix := name[:i]
	// 前缀若命中任何词表条目，说明 name 整体属于那个更长的词，不算误伤。
	for other := range dimensionAliases {
		if other != alias && strings.Contains(prefix, other) {
			return false
		}
	}
	// 泛词（≤3 字符）才容易误伤；更长的别名更可能是完整的维度名
	return len([]rune(alias)) <= 3
}
