package distill

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/pkg/generation"
	_ "github.com/JianFeeeee/HologramAgent/providers/ollama"
)

// goldResult 是单条金标准的判定结果。
type goldResult struct {
	name        string
	ok          bool
	gotSubject  string
	gotFields   []Field
	wantFields  int
	wantSubject string
	noTriple    bool
	note        string
}

// runGoldCases 对每条金标准跑一次真实拆分。
//
// ★ 判据独立性：期望值来自 goldcases_test.go（人工核对），
// 与 LLM 输出无关。LLM 的输出只被拿来「比对」，不当参照。
// goldCache 缓存 runGoldCases 的结果。
//
// ★ 为什么需要它
// ------------
// 实测（最终回归）：distill 包跑 500 秒，其中
//
//	TestGold_Baseline      250.95s
//	TestGold_ShowFailures  251.64s
//
// **两个测试各调了一遍 runGoldCases**，而它会打开 ollama 并逐条调 LLM
// ⇒ 同一批金标准用例被跑了两遍模型，浪费 250 秒。
//
// 缓存只在同一进程内有效，且带 subjectFrom 的调用不共享
// （那批测的是另一条主语路径）。
var goldCache struct {
	done bool
	out  []goldResult
}

// 变体缓存（带 subjectFrom 的那条路径）
var goldAltCache = map[string][]goldResult{}

func runGoldCases(t *testing.T, subjectFrom func(string) string) []goldResult {
	t.Helper()
	if subjectFrom == nil {
		if goldCache.done {
			return goldCache.out
		}
	} else {
		key := runtime.FuncForPC(reflect.ValueOf(subjectFrom).Pointer()).Name()
		if v, ok := goldAltCache[key]; ok {
			return v
		}
	}
	out := runGoldCasesUncached(t, subjectFrom)
	if subjectFrom == nil {
		goldCache.out, goldCache.done = out, true
	} else {
		key := runtime.FuncForPC(reflect.ValueOf(subjectFrom).Pointer()).Name()
		goldAltCache[key] = out
	}
	return out
}

// runGoldCasesUncached 是真正跑一遍模型的那份实现。
func runGoldCasesUncached(t *testing.T, subjectFrom func(string) string) []goldResult {
	t.Helper()

	t.Helper()
	gen, err := generation.Open("ollama", generation.Config{
		Options: map[string]string{
			"model":      envOr("GOLD_MODEL", "qwen3:1.7b"),
			"num_thread": "10",
			"think":      "false",
			"keep_alive": "30m",
		},
	})
	if err != nil {
		t.Skipf("generation provider 打开失败: %v", err)
	}
	defer gen.Close()

	ex := NewExtractor(gen, subjectFrom)
	var out []goldResult
	for _, c := range goldCases {
		r := goldResult{name: c.name, wantSubject: c.wantSubject,
			wantFields: len(c.wantFields), noTriple: c.noTriple}

		// 主语推导（不调模型，先看纯规则的部分）。
		// ★ 打印的必须是 Split 真正会用的那条路径（Extractor.deriveSubjects），
		// 否则报告里的主语是另一套规则算的，看不出真实行为 —— 初版就踩过：
		// 改动生效后报告仍显示 主语=""，差点误判成「没生效」。
		var subjDump []string
		for _, sub := range ex.deriveSubjects(c.record) {
			subjDump = append(subjDump, sub.Name)
		}
		r.gotSubject = strings.Join(subjDump, "|")

		triples, err := ex.Split(context.Background(), c.record)
		if err != nil {
			r.note = "拆分报错: " + err.Error()
			r.ok = false
			out = append(out, r)
			continue
		}
		for _, tr := range triples {
			r.gotFields = append(r.gotFields, Field{Name: tr.Relation, Value: tr.Object})
		}

		// 判定
		if c.noTriple {
			r.ok = len(triples) == 0
			if !r.ok {
				r.note = fmt.Sprintf("期望零三元组，实际 %d 个: %+v", len(triples), r.gotFields)
			}
		} else {
			r.ok = judgeFields(c, triples, r.gotSubject, &r)
		}
		out = append(out, r)
	}
	return out
}

// judgeFields 按「期望字段必须全部命中 + 不得有多余幻觉字段」判定。
//
// 只看「命中率」是不够的：模型可能拆出 10 个字段蒙中 4 个，
// 其余 6 个是编的（金标准价值 4 条里只信 2 条）。所以两个方向都要卡。
func judgeFields(c goldCase, triples []memory.Triple, gotSubject string, r *goldResult) bool {
	// 值原样校验（幻觉闸门的等价检查，独立于 Split 内部实现）
	for _, tr := range triples {
		if tr.Object == "" || !strings.Contains(c.record, tr.Object) {
			r.note = fmt.Sprintf("幻觉：值 %q 不在原句里", tr.Object)
			return false
		}
	}

	// 多主语用例：额外校验「值归对了主语」——
	// 三个服务的端口各归各主语，混配了（如 billing→8861）同样算错。
	if len(c.wantSubjects) > 0 {
		for _, sub := range c.wantSubjects {
			// 该主语名在句中对应的段（找它后面最近的值）
			val := valueAfterSubject(c.record, sub)
			if val == "" {
				r.note = fmt.Sprintf("金标准：主语 %q 后找不到值", sub)
				return false
			}
			if !hasFieldWithSubject(triples, sub, val) {
				r.note = fmt.Sprintf("错配：期望 %s→%s，实际没有这条三元组（现有：%s）",
					sub, val, formatTriples(triples))
				return false
			}
		}
		return true
	}

	// 期望字段全部命中
	var missing []string
	for _, w := range c.wantFields {
		if !hasField(triples, w.name, w.value) {
			missing = append(missing, w.name+"="+w.value)
		}
	}
	if len(missing) > 0 {
		r.note = "缺: " + strings.Join(missing, ", ")
		return false
	}
	return true
}

// valueAfterSubject 取该主语名后面最近的那个值（第一个数字串）。
func valueAfterSubject(record, subject string) string {
	runes := []rune(record)
	i := indexOfRunes(runes, []rune(subject))
	if i < 0 {
		return ""
	}
	rest := runes[i+len([]rune(subject)):]
	var digits []rune
	for _, r := range rest {
		if isDigit(r) {
			digits = append(digits, r)
			continue
		}
		if len(digits) > 0 {
			break
		}
	}
	return string(digits)
}

func hasFieldWithSubject(triples []memory.Triple, subject, value string) bool {
	for _, tr := range triples {
		if tr.Subject == subject && tr.Object == value {
			return true
		}
	}
	return false
}

func formatTriples(triples []memory.Triple) string {
	if len(triples) == 0 {
		return "(无)"
	}
	var parts []string
	for _, tr := range triples {
		parts = append(parts, tr.Subject+"-"+tr.Relation+"->"+tr.Object)
	}
	return strings.Join(parts, ", ")
}

func hasField(triples []memory.Triple, name, value string) bool {
	for _, tr := range triples {
		if tr.Relation == name && tr.Object == value {
			return true
		}
	}
	return false
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// TestGold_Baseline 记录现状基线。
//
// ★ 这个测试的作用不是「验证通过」，而是**把当前能力钉在数字上**。
// 现状已知：属性型记录主语全空 → 零三元组。
// 后续每次改动都重跑，看数字是否真的动了。
func TestGold_Baseline(t *testing.T) {
	if testing.Short() {
		t.Skip("需要 ollama")
	}
	t0 := time.Now()
	results := runGoldCases(t, nil)

	pass, total, zeroFieldCases := 0, 0, 0
	for _, r := range results {
		// noTriple 的用例单独统计：它考的是「不该拆的时候别拆」，
		// 混进命中率分母会让「不敢拆」的策略显得分高。
		if r.noTriple {
			zeroFieldCases++
			if r.ok {
				pass++
			}
			status := "✘"
			if r.ok {
				status = "✓"
			}
			t.Logf("%s %-20s （零字段用例，应产出空）  %s", status, r.name, r.note)
			continue
		}
		total++
		if r.ok {
			pass++
		}
		status := "✘"
		if r.ok {
			status = "✓"
		}
		t.Logf("%s %-20s 主语=%-12q 期望字段%d 实得%d  %s",
			status, r.name, r.gotSubject, r.wantFields, len(r.gotFields), r.note)
	}
	t.Logf("\n基线（defaultSubject）：%d/%d 条通过，用时 %.1fs", pass, total, time.Since(t0).Seconds())
}

// TestGold_ShowFailures 单独跑并打印失败细节，供排查用。
func TestGold_ShowFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("需要 ollama")
	}
	results := runGoldCases(t, nil)
	for _, r := range results {
		if r.ok {
			continue
		}
		t.Logf("── %s", r.name)
		t.Logf("   记录: %s", func() string {
			if len(r.gotSubject) > 0 {
				return ""
			}
			return "(主语空)"
		}())
		t.Logf("   主语: %q（期望 %q）", r.gotSubject, r.wantSubject)
		t.Logf("   实得字段: %s", formatFields(r.gotFields))
		t.Logf("   原因: %s", r.note)
	}
}

func formatFields(fs []Field) string {
	if len(fs) == 0 {
		return "(无)"
	}
	var parts []string
	for _, f := range fs {
		parts = append(parts, f.Name+"="+f.Value)
	}
	return strings.Join(parts, ", ")
}
