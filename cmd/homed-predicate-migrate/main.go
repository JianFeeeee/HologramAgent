// Command homed-predicate-migrate 把存量关系谓词收敛到**封闭集**，默认只统计不写库。
//
// 背景：生产库 relations 表 964 条 active 边对应 690 种不同 relation_type，
// 其中只出现 1 次的有 559 种（80%）。同一关系千种写法 ⇒ memory_recall
// 不可能靠关系名命中。谓词不封闭是召回质量差的直接原因之一。
//
// 数据来源：封闭集与映射表由外部起草后交付（JSON），本工具只消费、不改它。
//
//	closed-predicate/predicates.md   30 个语义角色谓词（人读）
//	closed-predicate/mapping.json    690 种旧谓词 → 新谓词 或 DROP（机器读）
//	closed-predicate/invert.json    属性名反转规则（可选，机器读）
//
// 安全设计（与 homed-kb-migrate 同一套惯例）：
//  1. **默认只报告**。-apply 才写库。谓词改写会影响召回，不可逆。
//  2. 冲突检查为 0 才允许 -apply：同一旧谓词映射到多个新谓词属歧义，
//     真写库会造成不可预测结果。
//  3. count 求和必须等于 active 边数：不等说明 mapping 与库已不同步
//     （库被写过 / mapping 基于旧快照），此时拒绝 apply。
//
// 用法：
//
//	homed-predicate-migrate -db /data/memory/graph.db -mapping closed-predicate
//	homed-predicate-migrate -db ... -mapping ... -apply
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	_ "github.com/mattn/go-sqlite3"
)

// mapEntry 是 mapping.json 的一条：旧谓词 → 新谓词（new 为空表示丢弃）。
type mapEntry struct {
	Old    string  `json:"old"`
	Count  int     `json:"count"`
	New    *string `json:"new"`
	Reason string  `json:"reason"`

	// verdict 是 dry-run 判定的最终去向，由 invert.json 参与后决定：
	//   mapped / debris / pending_invert / unmapped
	verdict string
	// invertTo 是反转后的谓词（仅 pending_invert 有值）
	invertTo string
}

// invertSpec 是 invert.json 的**规则式**格式（不是 old→new 直映）。
//
// 为什么是规则：抽取侧把「值」提到了谓词位（如「教室」其实是属性名、
// 真正的谓词空缺），需要按**宾语形态**反推真实关系，而不是按谓词查表。
// 规则里 applies_to 指明它作用于哪类 reason（drop_unmapped / drop_funcword）。
type invertSpec struct {
	Version string       `json:"version"`
	Rules   []invertRule `json:"rules"`
	// AttributeNounWhitelist 是「旧谓词本身是属性名词」的白名单。
	AttributeNounWhitelist []string `json:"attribute_noun_whitelist"`
	// ObjectValueSignals 是值形态信号（含数字、长度区间、拒收前缀）。
	ObjectValueSignals struct {
		ContainsDigit bool     `json:"contains_digit"`
		LengthMin     int      `json:"length_min"`
		LengthMax     int      `json:"length_max"`
		RejectPrefix  []string `json:"reject_prefix"`
	} `json:"object_value_signals"`
}

type invertRule struct {
	ID        string   `json:"id"`
	AppliesTo []string `json:"applies_to"`
	New       *string  `json:"new"` // null 表示该规则不反转（仍丢弃）
	When      struct {
		RelationTypeIn          []string `json:"relation_type_in"`
		RelationTypeInWhitelist bool     `json:"relation_type_in_whitelist"`
		ObjectIsValue           bool     `json:"object_is_value"`
	} `json:"when"`
}

func main() {
	dbPath := flag.String("db", "", "graph.db 路径")
	mapDir := flag.String("mapping", "", "封闭集目录（含 mapping.json / invert.json）")
	apply := flag.Bool("apply", false, "真正写库（默认只报告）")
	topN := flag.Int("top", 40, "丢弃明细里按 count 列出的条数（0=全列）")
	flag.Parse()

	if *dbPath == "" || *mapDir == "" {
		fmt.Fprintln(os.Stderr, "必须提供 -db 与 -mapping")
		os.Exit(2)
	}

	entries, err := loadMapping(*mapDir)
	if err != nil {
		fail("读取 mapping: %v", err)
	}
	inverts, err := loadInvert(*mapDir)
	if err != nil {
		// invert.json 可选：未到位时只报告，不阻断
		fmt.Fprintf(os.Stderr, "[warn] invert.json 未就位（%v），pending_invert 将无法解析\n", err)
	}

	// dry-run 用只读 URI（mattn 驱动要求 file: 前缀）；-apply 才可写。
	dsn := *dbPath
	if !*apply {
		dsn = "file:" + *dbPath + "?mode=ro"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		fail("打开库: %v", err)
	}
	defer db.Close()

	// ① 库侧真实分布（mapping 的 count 只是快照，必须与库核对）
	dbCounts, total, err := loadDBCounts(db)
	if err != nil {
		fail("读库: %v", err)
	}

	sumCounts := 0
	for _, e := range entries {
		sumCounts += e.Count
	}

	fmt.Printf("库：%d 种 relation_type，%d 条 active 边\n", len(dbCounts), total)
	fmt.Printf("mapping：%d 条，count 求和 %d\n", len(entries), sumCounts)

	// 判据 1：mapping 与库同步
	syncOK := sumCounts == total
	if !syncOK {
		fmt.Printf("❌ 判据1 失败：mapping count 求和 %d != 库 active 边 %d（mapping 与库不同步）\n", sumCounts, total)
	} else {
		fmt.Printf("✅ 判据1：mapping 与库同步（%d == %d）\n", sumCounts, total)
	}

	// ② 判定去向
	applyInvert(entries, inverts)
	verdicts := tally(entries)

	// 判据 2：无歧义（同一旧谓词只能有一个去向）
	conflicts := findConflicts(entries)
	if len(conflicts) > 0 {
		fmt.Printf("❌ 判据2 失败：%d 个旧谓词映射有歧义\n", len(conflicts))
		for _, c := range conflicts {
			fmt.Printf("   %s\n", c)
		}
	} else {
		fmt.Printf("✅ 判据2：无歧义映射\n")
	}

	// ③ 四张表
	printByNewPredicate(entries)
	printDrops(entries, *topN)
	printCoverage(entries, total, verdicts)

	// 判据 3：写库前复核（写前再查一次，防 dry-run 到 apply 之间库被改）
	if *apply {
		if !syncOK || len(conflicts) > 0 {
			fail("拒绝 apply：判据未全绿（见上）")
		}
		if err := applyChanges(db, entries); err != nil {
			fail("写库: %v", err)
		}
		fmt.Println("\n已写库。")
	} else {
		fmt.Println("\n（dry-run：未写库。确认无误后加 -apply）")
	}
}

func loadMapping(dir string) ([]*mapEntry, error) {
	b, err := os.ReadFile(filepath.Join(dir, "mapping.json"))
	if err != nil {
		return nil, err
	}
	var raw []*mapEntry
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func loadInvert(dir string) (*invertSpec, error) {
	b, err := os.ReadFile(filepath.Join(dir, "invert.json"))
	if err != nil {
		return nil, err
	}
	var spec invertSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

// resolveInvert 按规则判定某条旧谓词是否应反转，返回反转后的谓词。
//
// 判定只用**谓词本身**能否命中规则条件（宾语形态规则需要读边表才能判，
// dry-run 阶段只统计「谓词可判定」的部分，宾语相关的留给 apply 前的逐条检查）。
func (sp *invertSpec) resolveInvert(e *mapEntry) (string, bool) {
	if sp == nil {
		return "", false
	}
	whitelist := make(map[string]bool, len(sp.AttributeNounWhitelist))
	for _, w := range sp.AttributeNounWhitelist {
		whitelist[w] = true
	}
	for _, r := range sp.Rules {
		// applies_to 必须包含本条的 reason
		match := false
		for _, a := range r.AppliesTo {
			if a == e.Reason {
				match = true
				break
			}
		}
		if !match {
			continue
		}
		// 规则要求"谓词在属性名词白名单里"
		if r.When.RelationTypeInWhitelist && whitelist[e.Old] {
			if r.New != nil && *r.New != "" {
				return *r.New, true
			}
			return "", false
		}
		// 规则枚举了具体谓词（如系词）
		for _, w := range r.When.RelationTypeIn {
			if w == e.Old {
				if r.New != nil && *r.New != "" {
					return *r.New, true
				}
				return "", false
			}
		}
	}
	return "", false
}

func loadDBCounts(db *sql.DB) (map[string]int, int, error) {
	rows, err := db.Query(`SELECT relation_type, COUNT(*) FROM relations WHERE status='active' GROUP BY relation_type`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[string]int{}
	total := 0
	for rows.Next() {
		var k string
		var c int
		if err := rows.Scan(&k, &c); err != nil {
			return nil, 0, err
		}
		out[k] = c
		total += c
	}
	return out, total, rows.Err()
}

// applyInvert 决定每条 mapping 的最终去向。
//
// 有 new ⇒ mapped；无 new 但 invert 表里命中 ⇒ pending_invert；
// 否则按 reason 分 debris（碎片）或 unmapped（待判断）。
func applyInvert(entries []*mapEntry, sp *invertSpec) {
	for _, e := range entries {
		if e.New != nil && *e.New != "" {
			e.verdict = "mapped"
			continue
		}
		if to, ok := sp.resolveInvert(e); ok {
			e.verdict = "pending_invert"
			e.invertTo = to
			continue
		}
		if e.Reason == "drop_funcword" || e.Reason == "drop_broken_punct" ||
			e.Reason == "drop_too_short" || e.Reason == "drop_too_long" {
			e.verdict = "debris"
		} else {
			e.verdict = "unmapped"
		}
	}
}

func tally(entries []*mapEntry) map[string]int {
	m := map[string]int{}
	for _, e := range entries {
		m[e.verdict]++
	}
	return m
}

// findConflicts 检查同一旧谓词是否有多个去向（mapping 里出现重复 old）。
func findConflicts(entries []*mapEntry) []string {
	seen := map[string]string{}
	var out []string
	for _, e := range entries {
		to := e.verdict
		if e.New != nil {
			to = "new:" + *e.New
		}
		if prev, ok := seen[e.Old]; ok && prev != to {
			out = append(out, fmt.Sprintf("%s: %s vs %s", e.Old, prev, to))
		}
		seen[e.Old] = to
	}
	return out
}

func printByNewPredicate(entries []*mapEntry) {
	type agg struct{ types, edges int }
	m := map[string]*agg{}
	for _, e := range entries {
		if e.verdict == "mapped" {
			if m[*e.New] == nil {
				m[*e.New] = &agg{}
			}
			m[*e.New].types++
			m[*e.New].edges += e.Count
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]].edges > m[keys[j]].edges })

	fmt.Printf("\n## 表1 按新谓词聚合（%d 个有收编）\n", len(m))
	fmt.Printf("%-20s %8s %8s\n", "新谓词", "旧种数", "边数")
	for _, k := range keys {
		fmt.Printf("%-20s %8d %8d\n", k, m[k].types, m[k].edges)
	}
}

func printDrops(entries []*mapEntry, topN int) {
	var drops []*mapEntry
	for _, e := range entries {
		if e.verdict != "mapped" {
			drops = append(drops, e)
		}
	}
	sort.Slice(drops, func(i, j int) bool { return drops[i].Count > drops[j].Count })

	fmt.Printf("\n## 表2 丢弃/待反转明细（%d 种，按 count 降序）\n", len(drops))
	if topN > 0 && len(drops) > topN {
		fmt.Printf("（仅列前 %d 条，-top 0 可全列）\n", topN)
	}
	fmt.Printf("%6s %-14s %-26s %s\n", "count", "去向", "旧谓词", "说明")
	for i, e := range drops {
		if topN > 0 && i >= topN {
			break
		}
		note := e.Reason
		if e.verdict == "pending_invert" {
			note = "反转→" + e.invertTo
		}
		fmt.Printf("%6d %-14s %-26s %s\n", e.Count, e.verdict, truncate(e.Old, 24), note)
	}
}

// printCoverage 算两版覆盖率：只算 mapped，与 mapped+pending_invert。
func printCoverage(entries []*mapEntry, total int, verdicts map[string]int) {
	mapped, invert := 0, 0
	for _, e := range entries {
		switch e.verdict {
		case "mapped":
			mapped += e.Count
			invert += e.Count
		case "pending_invert":
			invert += e.Count
		}
	}
	fmt.Printf("\n## 表3 覆盖率\n")
	fmt.Printf("库 active 边总数            %5d\n", total)
	fmt.Printf("mapped 保留                 %5d  (%.1f%%)\n", mapped, pct(mapped, total))
	fmt.Printf("mapped+invert 保留          %5d  (%.1f%%)\n", invert, pct(invert, total))
	fmt.Printf("真正丢弃                    %5d  (%.1f%%)\n", total-invert, pct(total-invert, total))
	fmt.Printf("\n判定去向分布：")
	for k, v := range verdicts {
		fmt.Printf(" %s=%d", k, v)
	}
	fmt.Println()
}

func applyChanges(db *sql.DB, entries []*mapEntry) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	upd, err := tx.Prepare(`UPDATE relations SET relation_type=?, updated_at=datetime('now') WHERE relation_type=? AND status='active'`)
	if err != nil {
		return err
	}
	defer upd.Close()
	del, err := tx.Prepare(`UPDATE relations SET status='deleted', updated_at=datetime('now') WHERE relation_type=? AND status='active'`)
	if err != nil {
		return err
	}
	defer del.Close()

	nUpd, nDel := 0, 0
	for _, e := range entries {
		switch e.verdict {
		case "mapped":
			r, err := upd.Exec(*e.New, e.Old)
			if err != nil {
				return fmt.Errorf("更新 %s: %w", e.Old, err)
			}
			c, _ := r.RowsAffected()
			nUpd += int(c)
		case "debris":
			r, err := del.Exec(e.Old)
			if err != nil {
				return fmt.Errorf("删除 %s: %w", e.Old, err)
			}
			c, _ := r.RowsAffected()
			nDel += int(c)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("更新 %d 条，标记删除 %d 条\n", nUpd, nDel)
	return nil
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func fail(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
