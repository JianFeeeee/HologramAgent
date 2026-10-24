package distill

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// 字段名漂移的自动发现。
//
// ★ 为什么不能靠补词表
// --------------------
// 实测（真库 188 条里 40 条，真实 qwen3:1.7b）漂移有 8 个未归一名字：
//
//	观察比例  观察时长  峰值  峰值范围  批次  批次号  批次范围
//
// 而且 drift 不总是「模型叫错了」——**同一份语义，模型在不同记录里给出
// 不同名字**：
//
//	第112批  灰度10%观察48分后放50%   → 字段名 "发布窗口"
//	第113批  灰度5%观察86分后放50%    → 字段名 "发布窗口"
//	第114批  灰度10%观察70分后放50%   → 字段名 "观察时长"
//
// 三条同构记录、同一语义，两个名字。补词表能补到这一次，补不到下一次。
// 而且补的时候人在猜 —— 「观察时长」该并到「发布窗口」还是「停机时长」？
// 猜错就把两个真维度合并了。
//
// ★ 判据不靠字段名，靠**值的形态**
// -------------------------------
// 「字段名是不是同一个维度」正是要判定的问题，拿字段名去判是循环论证。
// 这里用的是两条独立信号：
//
//	信号1 同 subject  —— 同一个实体上出现的属性
//	信号2 同值形态   —— 值走同一个模板（百分比/时长/版本/日期/区间…）
//
// 两条同时成立**只是候选**，不是结论：仍可能同形态却是不同维度
// （「停机4分」和「观察48分」都是时长，但确实是两个维度）。
// 所以 Discover 返回的是**候选报告**，由人确认后才写进 dimensionAliases。
// 自动合并会把真维度错并，代价（信息丢失）远大于收益（少写几行词表）。

// valueShape 是值的形态模板。
//
// 判据的核心：把具体值抽象成「它长什么样」。百分比和百分比同形，
// 时长和时长同形，版本和版本同形 —— 跨形态几乎必然是不同维度。
type valueShape string

const (
	shapePercent  valueShape = "百分比" // 10%  5%
	shapeDuration valueShape = "时长"  // 4分 48分 30分钟
	shapeVersion  valueShape = "版本"  // v2.28.2  第4版
	shapeRange    valueShape = "区间"  // 4%~17%  30~84批  56~63批
	shapeNumber   valueShape = "数字"  // 8861  4324
	shapeDate     valueShape = "日期"  // 10月  周四
	shapeTime     valueShape = "时刻"  // 凌晨2点
	shapeRatio    valueShape = "比例词" // 均仅 / 全部
	shapeText     valueShape = "文本"  // 兜底
)

var (
	// reISODate 先匹配 ISO 日期：**年-月**（2026-10）与**年月日**（2026-10-02）。
	//
	// ★ 必须在 reRange 之前判，且必须覆盖年-月：
	//  1. 区间正则收了连字符（13-17%），ISO 日期会被它吃掉。
	//  2. 只匹配年月日会漏掉 "2026-10" —— 它同样被 reRange 命中
	//     （实测 reRange("2026-10") = true）。而年-月在迁移数据里很常见：
	//     memory_blocks.created_at 就是 "2026-10" 这种形态。
	reISODate = regexp.MustCompile(`^\d{4}-\d{1,2}(-\d{1,2})?$`)

	// reRange 匹配「A~B」与「A-B」两种区间形态。
	//
	// ★ 三条实测约束，都是踩出来的：
	//  1. 两端要容许单位后缀：初版写成 \d+\s*[~～-]\s*\d+ 时
	//     "4%~17%" 不匹配 —— 而它才是真库里最常见的区间形态（错误率峰值）。
	//     只匹配纯数字对会把最该发现的那类漂移（峰值 vs 峰值范围）漏掉。
	//  2. **连字符必须收进来**：修 1 时把字符类写成 [~～]（漏了 -），
	//     "13-17%" 就退回文本。真库里两种区间写法都有（13~17% 与 13-17%）。
	//  3. 结尾锚定（^…$）：不锚定会让 "10月-12月" 之类被判成区间，
	//     而它属于日期。前缀也锚定，否则 "v2-3" 会被当成区间。
	//  4. 结尾要容许**中文单位**尾巴：初版只放行 [%％]，结果真库里高频的
	//     "30~84批"、"56~63批" 全部退回文本 —— 而那正是批次区间，
	//     正是要靠它发现漂移的形态。
	reRange    = regexp.MustCompile(`^\d+(?:\.\d+)?\s*[%％]?\s*[~～-]\s*\d+(?:\.\d+)?\s*[%％]?[\p{Han}A-Za-z]*$`)
	rePercent  = regexp.MustCompile(`^\d+(\.\d+)?\s*[%％]$`)
	reDuration = regexp.MustCompile(`^\d+(\.\d+)?\s*(分|分钟|秒|小时|min|s)$`)
	// reVersion 容许点号与连字符两种分隔：真库里 "v2.28.2" 和 "v2-3" 都见过，
	// 且 "v2-3" 含连字符 —— 不认的话它会掉进 reRange（区间）或文本。
	reVersion = regexp.MustCompile(`^[vV]\d+(?:[.\-]\d+)*$|^第\d+版$`)
	reNumber  = regexp.MustCompile(`^\d+$`)
	reDate    = regexp.MustCompile(`^\d{1,2}月(\d{1,2}日)?$|^周[一二三四五六日]$`)
	reTime    = regexp.MustCompile(`(凌晨|上午|下午|晚上|中午)?\d{1,2}[点:：]\d{0,2}`)
)

// classifyValue 把一个值归到形态模板。
func classifyValue(v string) valueShape {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return shapeText
	case reISODate.MatchString(v):
		// 必须在区间之前：区间正则含连字符，ISO 日期会被它吃掉。
		return shapeDate
	case reRange.MatchString(v):
		return shapeRange
	case rePercent.MatchString(v):
		return shapePercent
	case reDuration.MatchString(v):
		return shapeDuration
	case reVersion.MatchString(v):
		return shapeVersion
	case reNumber.MatchString(v):
		return shapeNumber
	case reTime.MatchString(v):
		return shapeTime
	case reDate.MatchString(v):
		return shapeDate
	default:
		return shapeText
	}
}

// dimObservation 是一次观测：某 subject 上某字段名出现过某形态的值。
type dimObservation struct {
	Subject string
	Dim     string
	Shape   valueShape
	Samples []string
}

// DriftCandidate 是一组疑似同义字段名。
type DriftCandidate struct {
	// Shape 是这组字段名共有的值形态（判别的依据）。
	Shape valueShape
	// Dims 是疑似同义的字段名（按出现次数降序）。
	Dims []string
	// Subjects 是共同出现过这些字段名的 subject（判为「同一实体」）。
	Subjects []string
	// Evidence 是支持这个候选的观测样例，供人工确认。
	Evidence []string
}

// String 便于报告与日志。
func (c DriftCandidate) String() string {
	return fmt.Sprintf("[%s] %s（%d 个 subject，%d 条证据）",
		c.Shape, strings.Join(c.Dims, " / "), len(c.Subjects), len(c.Evidence))
}

// DiscoverDimensionDrift 从观测里找出疑似同义的字段名。
//
// 返回**候选**而不是归并结果 —— 理由见文件头：同形态不同维度很常见
// （停机4分 vs 观察48分），自动合并会丢信息。确认后由人写进 dimensionAliases。
//
// 判据（三条同时成立才列为候选）：
//  1. 值形态相同（classifyValue 相同）
//  2. 共同至少一个 subject（同实体上的属性）
//  3. 字段名不同（否则不是漂移）
func containsDim(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func DiscoverDimensionDrift(observations []dimObservation) []DriftCandidate {
	// (shape) → (subject) → dim → 样例
	type shapeKey struct {
		shape   valueShape
		subject string
	}
	grouped := map[shapeKey]map[string][]string{}

	for _, o := range observations {
		k := shapeKey{shape: o.Shape, subject: o.Subject}
		if grouped[k] == nil {
			grouped[k] = map[string][]string{}
		}
		grouped[k][o.Dim] = append(grouped[k][o.Dim], o.Samples...)
	}

	// shape → 候选（跨 subject 聚合）
	byShape := map[valueShape]*DriftCandidate{}
	for k, dims := range grouped {
		if len(dims) < 2 {
			continue // 只有一种叫法，不是漂移
		}
		cand, ok := byShape[k.shape]
		if !ok {
			cand = &DriftCandidate{Shape: k.shape}
			byShape[k.shape] = cand
		}
		for dim, samples := range dims {
			// ★ 去重：byShape 是跨 subject 累积的，同一 subject 里同一个
			// 字段名出现多次会重复进 Dims（实测真库跑出
			// 「发布窗口 / 发布窗口 / 发布窗口 …」五个重复项）。
			if !containsDim(cand.Dims, dim) {
				cand.Dims = append(cand.Dims, dim)
			}
			if len(samples) > 0 {
				cand.Evidence = append(cand.Evidence,
					fmt.Sprintf("%s·%s=%s", k.subject, dim, samples[0]))
			}
		}
		if !containsDim(cand.Subjects, k.subject) {
			cand.Subjects = append(cand.Subjects, k.subject)
		}
	}

	var out []DriftCandidate
	for _, cand := range byShape {
		sort.Strings(cand.Dims)
		sort.Strings(cand.Subjects)
		sort.Strings(cand.Evidence)
		out = append(out, *cand)
	}
	// 按证据数降序：证据多的先看
	sort.Slice(out, func(i, j int) bool {
		return len(out[i].Evidence) > len(out[j].Evidence)
	})
	return out
}
