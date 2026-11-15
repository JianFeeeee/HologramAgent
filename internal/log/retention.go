package log

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RetentionPolicy string

const (
	RetentionThisWeek  RetentionPolicy = "this_week"
	RetentionThisMonth RetentionPolicy = "this_month"
	RetentionNMonths   RetentionPolicy = "n_months"
	RetentionForever   RetentionPolicy = "forever"
)

func applyRetention(logDir string, policy RetentionPolicy, months int) {
	switch policy {
	case RetentionForever:
		return
	case RetentionThisWeek:
		cleanupExceptCurrentWeek(logDir)
	case RetentionThisMonth:
		cleanupExceptCurrentMonth(logDir)
	case RetentionNMonths:
		cleanupBeforeMonths(logDir, months)
	}
}

func cleanupExceptCurrentWeek(logDir string) {
	now := time.Now()
	keep := lastWeekKey(now)
	for _, f := range listArchives(logDir, "week_") {
		if !strings.HasPrefix(f, "week_"+keep) {
			os.Remove(filepath.Join(logDir, f))
		}
	}
	cleanupAll(logDir, "month_")
	cleanupAll(logDir, "year_")
}

func cleanupExceptCurrentMonth(logDir string) {
	now := time.Now()
	keep := lastMonthKey(now)
	for _, f := range listArchives(logDir, "month_") {
		if !strings.HasPrefix(f, "month_"+keep) {
			os.Remove(filepath.Join(logDir, f))
		}
	}
	cleanupAll(logDir, "year_")
}

// cleanupBeforeMonths 按「保留最近 months 个月」清理归档。
//
// ## ★ 覆盖全部四类归档（2026-11-15 重写）
//
// 原实现只处理 `month_` 与 `year_`，**完全不碰 `week_` 和裸 `homed_*.log`**。
// 而默认策略恰好是 `n_months` ⇒ 半年前的周归档与裸日志**永远留着**。
//
// ## ★ 解析口径必须容忍历史错误命名
//
// 磁盘上同时存在两代命名：
//   - `month_2026-27`（旧实现把 ISO **周号前两位**当月份，2026-08 前的产物）
//   - `month_2026-06`（新实现从日期算出的真月份）
//
// 旧代码对 `2026-27` 解析失败就 `continue` ⇒ **静默永不删除**。
// 现在把「非法月份」当成「该归档属于哪一年」来判：只要年份早于 cutoff 就删；
// 同年则用保守规则（保留），避免把上个月的归档误删。
func cleanupBeforeMonths(logDir string, months int) {
	cutoff := time.Now().AddDate(0, -months, 0)

	for _, f := range listArchives(logDir, "week_") {
		t, ok := archiveWeekStart(f)
		if !ok {
			continue
		}
		if t.Before(cutoff) {
			removeArchive(logDir, f)
		}
	}

	for _, f := range listArchives(logDir, "month_") {
		t, ok := archiveMonthStart(f)
		if !ok {
			continue
		}
		if t.Before(cutoff) {
			removeArchive(logDir, f)
		}
	}

	for _, f := range listArchives(logDir, "year_") {
		yearStr := strings.TrimSuffix(strings.TrimPrefix(f, "year_"), ".tar.gz")
		y, err := time.Parse("2006", yearStr)
		if err != nil {
			continue
		}
		if y.Before(cutoff) {
			removeArchive(logDir, f)
		}
	}

	// ★ 裸日志（`homed_*.log`）**刻意不归保留策略管**（2026-11-15 保持原边界）。
	//
	// 它们由 compressWeekly 收纳成 `week_*` 之后才消失 —— 那条路径上文件
	// 是「转移」而不是「丢弃」。保留策略直接删裸日志等于**丢数据**：
	// 一个还没被压缩的老日志会被无声抹掉，而它的内容从未进过任何归档。
	//
	// 已知遗留（未修，见 todo §8）：compressWeekly 遇到同名 week_ 归档
	// 已存在时直接 `continue`，于是该周后来产生的裸日志**永远不会被收纳**
	// （future-plan §4.2 记的「week_2026-W43 只含 1 个文件，应为 22 个」
	// 就是这个）。那是压缩侧的幂等缺陷，不该用「保留策略删掉它们」来兜底。
}

func removeArchive(logDir, name string) {
	if err := os.Remove(filepath.Join(logDir, name)); err != nil && !os.IsNotExist(err) {
		log.Printf("[log retention] remove %s: %v", name, err)
	}
}

// archiveWeekStart 从 `week_2026-W27.tar.gz` 解出该周的周一。
func archiveWeekStart(f string) (time.Time, bool) {
	m := weekRe.FindStringSubmatch(f)
	if m == nil {
		return time.Time{}, false
	}
	t, err := parseWeekKey(m[1] + "-W" + m[2])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// archiveMonthStart 从文件名解出该归档覆盖的**起点**（用于与 cutoff 比较）。
//
// ## 必须容忍两代命名，且不能退化成「按年一刀切」
//
// 磁盘上同时存在：
//   - `month_2026-06.tar.gz` —— 新实现：真月份
//   - `month_2026-27.tar.gz` —— 旧实现：把 ISO **周号前两位**当月份
//
// 两者用同一个正则 `month_(\d{4}-\d{2})` 都能匹配上，**只有 `time.Parse`
// 能区分**（第 27 月不存在 ⇒ month out of range）。
//
// ## 歧义消解（1..12 既可读成月份、也可读成周号）
//
//   · 1..12  ⇒ 按**月份**解释（新实现的含义；且对 `2026-06` 而言
//               「6 月 1 日」晚于「第 6 周（2 月 2 日）」，取较晚者更保守）
//   · 13..53 ⇒ 月份不存在，按 **ISO 周号**还原成该周周一
//
// ★ 为什么不能退化成「按年判断（该年 1 月 1 日）」：那会把**本年内**的
//   历史归档一并删掉。实测 `month_2026-43`（ISO 第 43 周 = 10 月 19 日）
//   在 months=3（cutoff=08-15）下**应当保留**，按年判断会误删它。

// archiveMonthStart 从文件名解出该归档覆盖的**起点**（用于与 cutoff 比较）。
//
// ## 必须容忍两代命名，且不能退化成「按年一刀切」
//
// 磁盘上同时存在：
//   - `month_2026-06.tar.gz` —— 新实现：真月份
//   - `month_2026-27.tar.gz` —— 旧实现：把 ISO **周号前两位**当月份
//
// 两者用同一个正则 `month_(\d{4}-\d{2})` 都能匹配上，**只有 `time.Parse`
// 能区分**（第 27 月不存在 ⇒ month out of range）。上一版在 `Parse` 失败时
// 直接 return false ⇒ 2025 年的历史脏名依然永久占盘（判据当场红了）。
//
// ## 歧义消解（1..12 既可读成月份、也可读成周号）
//
//	· 1..12  ⇒ 按**月份**解释。对 `2026-06` 而言「6 月 1 日」晚于
//	            「第 6 周（2 月 2 日）」，取较晚者更保守（不容易误删）。
//	· 13..53 ⇒ 月份不存在，按 **ISO 周号**还原成该周周一。
//
// ★ 为什么不能退化成「该年 1 月 1 日」：那会把**本年内**的历史归档一并删掉。
//
//	实测 `month_2026-43`（ISO 第 43 周 = 10 月 19 日）在 months=3
//	（cutoff=08-15）下**应当保留**，按年判断会误删它。
func archiveMonthStart(f string) (time.Time, bool) {
	m := monthRe.FindStringSubmatch(f)
	if m == nil {
		return time.Time{}, false
	}
	num := 0
	if _, err := fmt.Sscanf(m[1][5:7], "%02d", &num); err != nil {
		return time.Time{}, false
	}
	if num >= 1 && num <= 12 {
		if t, err := time.Parse("2006-01", m[1]); err == nil {
			return t, true
		}
		return time.Time{}, false
	}
	if num <= 53 {
		if t, err := parseWeekKey(m[1][:4] + "-W" + m[1][5:7]); err == nil {
			return t, true
		}
	}
	// >53 只可能是坏数据：不删（不猜）。
	return time.Time{}, false
}

func cleanupAll(logDir, prefix string) {
	for _, f := range listArchives(logDir, prefix) {
		removeArchive(logDir, f)
	}
}
