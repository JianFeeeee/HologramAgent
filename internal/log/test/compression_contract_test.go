package test

// internal/log 的模块职责判据：**用真实产物**验证压缩命名与保留清理。
//
// ## 为什么必须重写（2026-11-15）
//
// 此前本包 3 条判据只测「Start/Stop 不 panic」与「策略常量互不相同」——
// 而生产里有两个真实缺陷正在发生，判据一条都没抓到：
//
//  1. `compressMonthly` 产出 `month_2026-27`（ISO **周号前两位**当月份），
//     而 `cleanupBeforeMonths` 用 `time.Parse("2006-01", ...)` 解析
//     ⇒ month out of range ⇒ **一个都不删**。生产 17 个 `month_2026-27…43`
//     从 2026-08-01 起一直在，且因 `fileExists` 判重永远不会被重建。
//  2. 保留策略只处理 `month_` / `year_`，**完全不碰 `week_`**。
//
// 旧判据（`retention_test.go`，被 `53cdc7f` 删除）用**手写**的合规文件名
// `month_%04d-%02d` 造输入 —— 从没跑过 `compressMonthly` 的真实产物，
// 所以命名不一致这件事在判据里根本不存在。
//
// ⇒ 本文件的做法：**先跑真实压缩、再跑保留清理、断言产物消失**。
// 这是「判据的输入必须来自真实产物」那条教训（engineering-lessons §1.4）的落地。

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	logpkg "github.com/JianFeeeee/HologramAgent/internal/log"
)

type cfgMap map[string]string

func (c cfgMap) GetString(key, def string) string {
	if v, ok := c[key]; ok {
		return v
	}
	return def
}

func (c cfgMap) GetInt(key string, def int) int {
	if v, ok := c[key]; ok {
		var n int
		fmt.Sscanf(v, "%d", &n)
		return n
	}
	return def
}

// writeTarGz 造一个最小的 .tar.gz（内容无关，只要能被 os.Stat 看见）。
func writeTarGz(t *testing.T, dir, name string) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	gw := gzip.NewWriter(f)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: "inner.log", Size: 1, Mode: 0o644}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	tw.Close()
	gw.Close()
	f.Close()
}

func writeRawLog(t *testing.T, dir, date string) {
	t.Helper()
	name := fmt.Sprintf("homed_%s_10-00-00.log", date)
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

// TestCompressMonthlyProducesRealMonthNames 钉住产物的**命名口径**。
//
// 这是本轮两个生产缺陷的共同根因：产物名必须是 `month_YYYY-MM`
// （一个能被 `2006-01` 解析的真月份），不能是 ISO 周号。
//
// 变异验证：把 `monthKey` 改回 `fmt.Sprintf("%s-%s", m[1], m[2][:2])`
// ⇒ 产出 `month_2026-27` ⇒ 本判据红。
//
// ⚠️ 这条判据需要触发真实的 compressMonthly，而它不在导出面上。
// 因此通过配置 `core.log.compress_on_start=true` + Start 走生产路径。
// 这是**唯一**能只用导出接口跑到真实压缩的方式 —— 也是把它做成
// 一个真实配置项（而非测试专用钩子）的原因。
func TestCompressMonthlyProducesRealMonthNames(t *testing.T) {
	dir := t.TempDir()
	// 2026-06-29 是 ISO 2026-W27 的周一 ⇒ 应归入 month_2026-06。
	writeRawLog(t, dir, "2026-06-29")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 先 weekly 收进周归档，再 monthly 才能产出月归档 —— 两级都要跑，
	// 但**不能**到 yearly（见函数末尾的说明）。
	m := logpkg.NewManager(dir, cfgMap{
		"core.log.compress_on_start": "monthly",
		"core.log.retention":         "forever", // 只看命名，不清理
	})
	_ = m
	mw := logpkg.NewManager(dir, cfgMap{
		"core.log.compress_on_start": "weekly",
		"core.log.retention":         "forever",
	})
	mw.Start(ctx)
	waitFor(t, dir, func() bool { return countArchives(dir, "week_") > 0 }, 5*time.Second)
	mw.Stop()

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	m.Start(ctx2)
	waitFor(t, dir, func() bool { return countArchives(dir, "month_") > 0 }, 5*time.Second)
	m.Stop()

	names := archives(dir, "month_")
	if len(names) != 1 {
		t.Fatalf("★ 期望产出 1 个 month_ 归档，实际 %d 个: %v", len(names), names)
	}
	got := names[0]
	// 必须是 month_YYYY-MM.tar.gz（月份 2 位、范围 01..12）
	if !strings.HasPrefix(got, "month_2026-") || !strings.HasSuffix(got, ".tar.gz") {
		t.Fatalf("★ 产物名不符: %q", got)
	}
	month := strings.TrimSuffix(strings.TrimPrefix(got, "month_2026-"), ".tar.gz")
	if len(month) != 2 || month[0] > '1' || (month[0] == '1' && month[1] > '2') || month[0] == '0' && month[1] == '0' {
		t.Fatalf("★ 产物名里的月份不是合法月份（1..12）: %q（旧实现会把 ISO 周号 27 塞进来）", got)
	}
	if got != "month_2026-06.tar.gz" {
		t.Fatalf("★ 2026-W27 的周一在 6 月，期望 month_2026-06.tar.gz，实际 %q", got)
	}
	// ★ 顺带钉住一件重要的事：**不能** cascade 到 yearly。
	//
	// 三层逐级收敛（裸日志→week→month→year）。全跑会把一切都塌进
	// `year_2026.tar.gz`，于是任何按月/周的保留粒度都失效 ——
	// 实测（本判据的另一半）：一个 2026-11-02 的裸日志跑完三层只剩
	// `year_2026.tar.gz`。所以 `compress_on_start` 选层而不是布尔。
	// 关键：这个产物必须能被 retention 的解析口径认出来。
	if _, err := time.Parse("2006-01", "2026-"+month); err != nil {
		t.Fatalf("★ 产物名无法被 time.Parse(\"2006-01\") 解析（这会静默导致永不清理）: %q", got)
	}
}

// TestRetentionRemovesRealArchiveProducts 是本轮的核心判据：
// **压缩 → 保留清理** 整条链要在真实产物上工作。
//
// 变异验证：
//   - 把 `compressMonthly` 的 monthKey 改回周号 ⇒ 命名非法 ⇒ 清理逻辑收不到
//     （新实现退化为「按年判断」，2026 年不早于 cutoff ⇒ 仍保留）⇒ 红
//   - 删掉 cleanupBeforeMonths 里的 `week_` 分支 ⇒ 第二段红
func TestRetentionRemovesRealArchiveProducts(t *testing.T) {
	dir := t.TempDir()
	// 两个半年前的周（2026-W27 → 06-29，W33 → 08-10），
	// 加一个上周（2026-W45 → 11-02，应保留）。
	writeRawLog(t, dir, "2026-06-29")
	writeRawLog(t, dir, "2026-08-10")
	writeRawLog(t, dir, "2026-11-02")

	// 第一段：只跑 weekly + monthly，产出**真实月归档**。
	// retention=forever 防止这一阶段顺手清理，破坏后面的断言。
	compressLevel(t, dir, "weekly", "forever")
	compressLevel(t, dir, "monthly", "forever")

	months := archives(dir, "month_")
	if len(months) == 0 {
		t.Fatalf("★ 压缩没有产出任何 month_ 归档")
	}

	// 第二段：按 n_months=3 清理。半年前的必须被删，上周的必须留下。
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := logpkg.NewManager(dir, cfgMap{
		"core.log.retention":        "n_months",
		"core.log.retention_months": "3",
	})
	m.Start(ctx)
	waitFor(t, dir, func() bool { return len(archives(dir, "month_")) < len(months) }, 3*time.Second)
	m.Stop()

	// 半年前那批（6 月 / 8 月）必须消失；11 月那个必须还在。
	left := archives(dir, "month_")
	for _, f := range left {
		if strings.Contains(f, "2026-06") || strings.Contains(f, "2026-08") {
			t.Fatalf("★ 半年前的月归档未被清理（真实产物在，说明解析口径又不匹配了）: %v", left)
		}
	}
	foundNov := false
	for _, f := range left {
		if strings.Contains(f, "2026-11") {
			foundNov = true
		}
	}
	if !foundNov {
		t.Fatalf("★ 本月的月归档被误删: %v", left)
	}
}

// compressLevel 用真实 Manager 跑指定层的压缩（retention 由参数控制）。
func compressLevel(t *testing.T, dir, level, retention string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := logpkg.NewManager(dir, cfgMap{
		"core.log.compress_on_start": level,
		"core.log.retention":         retention,
	})
	m.Start(ctx)
	// 压缩是**异步 goroutine**，且后续的 monthly 依赖 weekly 的产物。
	// 这里按 level 等**它自己那一层**的产物，而不是「有任何归档」——
	// 后者在第一次（weekly）就为真，会让 monthly 阶段过早退出。
	wait := func() int {
		switch level {
		case "weekly":
			return countArchives(dir, "week_")
		case "monthly":
			return countArchives(dir, "month_")
		default:
			return countArchives(dir, "year_")
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if wait() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	m.Stop()
}

// TestRetentionCoversWeekArchives 单独钉住「保留策略必须碰 week_」。
//
// 原实现只处理 month_ / year_，而默认策略是 n_months ⇒
// 半年前的周归档**永远留着**。
//
// 变异验证：删掉 cleanupBeforeMonths 里的 week_ 循环 ⇒ 红。
func TestRetentionCoversWeekArchives(t *testing.T) {
	dir := t.TempDir()
	writeTarGz(t, dir, "week_2026-W27.tar.gz") // 2026-06-29，半年前
	writeTarGz(t, dir, "week_2026-W45.tar.gz") // 2026-11-02，上周

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// ★ 不设 compress_on_start：`Start` 现在**总是**先跑一次保留清理
	// （三个压缩循环的首次 tick 是「下一个周一 / 下月 1 日」，最坏要等 363h）。
	// 设 all 会触发级联塌缩（week_→month_→year_），把被测对象本身搬走。
	m := logpkg.NewManager(dir, cfgMap{})
	m.Start(ctx)
	waitFor(t, dir, func() bool { return !exists(dir, "week_2026-W27.tar.gz") }, 5*time.Second)
	m.Stop()

	if exists(dir, "week_2026-W27.tar.gz") {
		t.Fatal("★ 半年前的 week 归档未被清理 —— 保留策略完全不碰 week_")
	}
	if !exists(dir, "week_2026-W45.tar.gz") {
		t.Fatal("★ 上周的 week 归档被误删了")
	}
}

// TestRetentionRemovesLegacyWeekNamedMonth 钉住历史脏名字的收敛。
//
// 生产里现存 17 个 `month_2026-27`（旧实现产物）。它们不能被
// `time.Parse("2006-01")` 解析 —— 旧代码因此 `continue` ⇒ 永久占盘。
// 新实现退化为「按年判断」：整年早于 cutoff 就删。
//
// 变异验证：让 archiveMonthStart 对非法月份返回 false ⇒ 红。
func TestRetentionRemovesLegacyWeekNamedMonth(t *testing.T) {
	dir := t.TempDir()
	writeTarGz(t, dir, "month_2025-27.tar.gz") // 2025 年的历史脏名
	writeTarGz(t, dir, "month_2026-43.tar.gz") // 2026 年的历史脏名（本年内，应保留）

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 不设 compress_on_start：设 all 会把 month_ 级联压成 year_，
	// 被测的历史脏名自己就先被搬走了（那样测的就不是保留了）。
	m := logpkg.NewManager(dir, cfgMap{})
	m.Start(ctx)
	waitFor(t, dir, func() bool { return !exists(dir, "month_2025-27.tar.gz") }, 5*time.Second)
	m.Stop()

	if exists(dir, "month_2025-27.tar.gz") {
		t.Fatal("★ 2025 年的历史脏名未被清理（解析失败 ⇒ 静默 continue）")
	}
	if !exists(dir, "month_2026-43.tar.gz") {
		t.Fatal("★ 本年内（2026）的归档被误删 —— 退化判定必须保守")
	}
}

// TestRawLogsNotRemovedByRetention 保持既有语义边界：裸日志不归保留策略管。
//
// 它们是「转移」进 week_ 归档，不是「丢弃」。保留策略直接删裸日志
// 等于丢数据（内容从未进任何归档）。
func TestRawLogsNotRemovedByRetention(t *testing.T) {
	dir := t.TempDir()
	writeRawLog(t, dir, "2026-01-01")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 不设 compress_on_start：设了就先把裸日志收进 week_（那测的就不是保留了）。
	m := logpkg.NewManager(dir, cfgMap{"core.log.retention": "this_week"})
	m.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	m.Stop()

	if !exists(dir, "homed_2026-01-01_10-00-00.log") {
		t.Fatal("★ 裸 homed_*.log 被保留策略直接删了（应先压缩成 week_，否则丢数据）")
	}
}

// TestCompressOnStartIsOffByDefault：默认不补跑（常规重启不重复扫描）。
//
// 变异验证：把默认值改成 "true" ⇒ 老归档会被立刻处理 ⇒ 本判据红。
func TestCompressOnStartIsOffByDefault(t *testing.T) {
	dir := t.TempDir()
	writeRawLog(t, dir, "2026-06-29")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := logpkg.NewManager(dir, cfgMap{}) // 不配 compress_on_start
	m.Start(ctx)
	time.Sleep(300 * time.Millisecond)
	m.Stop()

	if countArchives(dir, "month_") > 0 {
		t.Fatal("★ 默认行为发生了变化：未配置 compress_on_start 却补跑了压缩")
	}
}

func countArchives(dir, prefix string) int { return len(archives(dir, prefix)) }

func archives(dir, prefix string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func waitFor(t *testing.T, dir string, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
