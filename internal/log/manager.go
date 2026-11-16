package log

import (
	"context"
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
	"log"
	"sync"
	"time"
)

// ConfigProvider 是 log 包读取配置的最小接口，由 internalConfig.ConfigRegistry 实现。
type ConfigProvider interface {
	GetString(key, defaultVal string) string
	GetInt(key string, defaultVal int) int
}

type Manager struct {
	doneOnce sync.Once
	logDir   string
	cfg      ConfigProvider
	done     chan struct{}

	// wg 追踪 Start 里那个"跑一轮压缩 + 保留清理"的 goroutine。
	//
	// ## 为什么必须等它
	//
	// 那个 goroutine 不受 done 控制：Stop() 只关 done、不中断它。于是
	// `Stop()` 返回时压缩可能仍在写归档 —— 而调用方通常紧接着用**新的**
	// Manager 接管同一目录（热重载、测试的两段式）。新实例的 Start 会立刻
	// 跑 applyRetention，看见的是**半成品目录**：只压缩了一半的月归档，
	// 于是按 n_months 删掉它们，剩下那些还没生成的归档一个都没有。
	//
	// 实测（CI 覆盖率门偶发红，本机连跑 15 次全绿）：
	//
	//	--- FAIL: TestRetentionRemovesRealArchiveProducts
	//	    ★ 本月的月归档被误删
	//
	// 判据本身只是在"压缩完成"这个前提下才成立，而 Stop 不保证那个前提。
	wg sync.WaitGroup
}

func NewManager(logDir string, cfg ConfigProvider) *Manager {
	return &Manager{logDir: logDir, cfg: cfg, done: make(chan struct{})}
}

// 配置键：启动后立即补跑哪一层**压缩 + 保留清理**。
//
// ## 为什么需要它（不是测试专用开关）
//
// 三个循环的**首次 tick 是「下一个周一 01:00 / 下月 1 日 02:00 / 次年 1 月 1 日
// 03:00」**，不是「从现在起 24 小时」。实测（2026-11-15 周日 22:51 启动）：
//
//	weekly  首次 tick = 2h 后
//	monthly 首次 tick = **363h 后**（约 15 天）
//
// ⇒ 一个从旧数据目录接管的实例，最多要等半个月才第一次清理归档；
// 磁盘紧张时那半个月正好是最需要的窗口。
//
// ## 为什么是「层级」而不是布尔
//
// 三层会**逐级收敛**（裸日志→week_→month_→year_）。全跑一轮会把任何足够老的
// 日志一路塌到 `year_`，这对"补齐落后"是对的，但运维常常只想跑便宜的
// weekly（把裸日志收进周归档）而不想触发整个级联。层级取值把选择交给运维。
//
//	"false"（默认） 不补跑 —— 常规重启不重复扫描，行为与引入前逐字一致
//	"weekly"       只跑 compressWeekly
//	"monthly"      只跑 compressMonthly
//	"yearly"       只跑 compressYearly
//	"all"          三层依次全跑
//
// ## `all` 的固有语义（不是缺陷）
//
// 三层是**逐级收敛**的：裸日志→`week_`→`month_`→`year_`。跑完 `all` 之后
// 任何足够老的东西都塌进 `year_<年>.tar.gz`，于是按周/按月的保留粒度对它
// 不再适用。这是级联的固有语义（不是 bug），但要知道后果再选它。
//
// 保留清理**不由本配置控制**：`Start` 总是先跑一次（见 Start 的注释）。
const compressOnStartKey = "core.log.compress_on_start"

func (m *Manager) Start(ctx context.Context) {
	// 启动时**总是**跑一次保留清理（与 compress_on_start 无关）。
	//
	// ## 为什么不能只在压缩 tick 之后清理
	//
	// 三个循环的首次 tick 是「下一个周一 01:00 / 下月 1 日 02:00 / 次年 1 月 1 日
	// 03:00」，不是「从现在起 24 小时」。实测（2026-11-15 周日 22:51 启动）：
	//
	//	weekly  首次 tick = 2h 后
	//	monthly 首次 tick = **363h 后**（约 15 天）
	//
	// 而 `applyRetention` 只挂在压缩 tick 上 ⇒ 一个从旧数据目录接管的实例，
	// 最坏要等半个月才第一次清理归档 —— 磁盘紧张时那半个月正好是最需要的窗口。
	//
	// 保留清理本身极便宜（列目录 + stat + 删除），且完全由 `core.log.retention`
	// 决定行为：`forever` 什么都不删，`n_months` 只删按策略本就该删的。
	// 所以「启动即跑一次」不改变语义，只是把第一次清理提前到启动。
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.applyRetention()
		if level := m.cfg.GetString(compressOnStartKey, "false"); level != "false" && level != "" {
			m.compressOnStart(level)
		}
	}()
	go m.runLoop(ctx, "weekly", 24*time.Hour, m.weeklyTick, m.compressWeekly)
	go m.runLoop(ctx, "monthly", 24*time.Hour, m.monthlyTick, m.compressMonthly)
	go m.runLoop(ctx, "yearly", 24*time.Hour, m.yearlyTick, m.compressYearly)
}

// Stop 幂等（本仓 close 纪律：见 internal/safeclose）。
//
// 原先裸 `close(m.done)`：只在"单次停机"这一**隐式前提**下安全。
// 一旦将来多一条调用路径（重载、热更新、defer + 显式），就会
// `panic: close of closed channel` —— 而那种改动看起来与 close 无关。
func (m *Manager) Stop() {
	safeclose.Safe(&m.doneOnce, m.done)
	// 等启动期的压缩/清理 goroutine 收尾（理由见 Manager.wg 的注释）。
	// 它是有限时间的（列目录 + 压缩已有裸日志），且幂等。
	m.wg.Wait()
}

// compressOnStart 按配置的层级补跑一轮压缩，随后执行保留清理。
//
// 幂等：压缩遇同名归档跳过、删除遇不存在忽略 —— 重复启动不会破坏任何东西。
func (m *Manager) compressOnStart(level string) {
	log.Printf("[log manager] compress_on_start=%s: running one immediate pass", level)
	type step struct {
		name string
		work func(string) error
	}
	steps := map[string][]step{
		"weekly":  {{"weekly", m.compressWeekly}},
		"monthly": {{"monthly", m.compressMonthly}},
		"yearly":  {{"yearly", m.compressYearly}},
		"all": {
			{"weekly", m.compressWeekly},
			{"monthly", m.compressMonthly},
			{"yearly", m.compressYearly},
		},
	}
	list, ok := steps[level]
	if !ok {
		log.Printf("[log manager] unknown %s=%q (expected false|weekly|monthly|yearly|all); "+
			"falling back to all", compressOnStartKey, level)
		list = steps["all"]
	}
	for _, st := range list {
		if err := st.work(m.logDir); err != nil {
			log.Printf("[log manager] %s compression error: %v", st.name, err)
		}
	}
}

func (m *Manager) runLoop(ctx context.Context, name string, tickInterval time.Duration, nextFn func() time.Duration, work func(string) error) {
	first := nextFn()
	timer := time.NewTimer(first)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			log.Printf("[log manager] running %s compression", name)
			if err := work(m.logDir); err != nil {
				log.Printf("[log manager] %s compression error: %v", name, err)
			}
			m.applyRetention()
			timer.Reset(nextFn())
		}
	}
}

// applyRetention 执行一次归档清理。
//
// ## ★ 兜底值为什么必须是 n_months（2026-10-29 修正）
//
// `core.log.retention` 是用 `regMeta` 登记的（只登记、不播种），而
// `defDefault` 要求 `seeded[key]` 为真才回退 `ConfigDef.Default` ——
// 所以 **GetString 返回的永远是这里传的兜底值**，registry 里写的
// Default="n_months" 对实际行为没有任何作用。
//
// 此前这里写的是 "forever"，后果是：压缩照做、**永不清理**。
// 生产实测 /home/hologramagent/log 涨到 590MB / 504 个文件。
// 而本包另一处注释还写着"每天一个文件，从不自动清理" ——
// 注释描述了 bug，代码却没修。
//
// 现在兜底与 registry 的 Default 对齐（n_months / 3），两边由
// `internal/kernelmod/security` 的同型判据哲学钉住：默认值只允许有一处事实来源。
func (m *Manager) applyRetention() {
	policy := RetentionPolicy(m.cfg.GetString("core.log.retention", string(RetentionNMonths)))
	months := m.cfg.GetInt("core.log.retention_months", 3)
	// 未知策略值不静默当 forever 处理：那会让一个拼写错误（如 "weekly"）
	// 永久关闭清理，而运维以为已经生效。回退到默认并响亮记录。
	if !validRetentionPolicy(policy) {
		log.Printf("[log manager] unknown core.log.retention=%q (expected this_week|this_month|n_months|forever); "+
			"falling back to %s", string(policy), RetentionNMonths)
		policy = RetentionNMonths
	}
	if months <= 0 {
		months = 3
	}
	applyRetention(m.logDir, policy, months)
}

// validRetentionPolicy 报告策略值是否可识别。
func validRetentionPolicy(p RetentionPolicy) bool {
	switch p {
	case RetentionThisWeek, RetentionThisMonth, RetentionNMonths, RetentionForever:
		return true
	}
	return false
}

func (m *Manager) weeklyTick() time.Duration {
	return nextWeekdayTime(time.Monday, 1, 0)
}

func (m *Manager) monthlyTick() time.Duration {
	return nextMonthDayTime(1, 2, 0)
}

func (m *Manager) yearlyTick() time.Duration {
	return nextYearDayTime(1, 3, 0)
}

func (m *Manager) compressWeekly(logDir string) error {
	return compressWeekly(logDir)
}

func (m *Manager) compressMonthly(logDir string) error {
	return compressMonthly(logDir)
}

func (m *Manager) compressYearly(logDir string) error {
	return compressYearly(logDir)
}

func nextWeekdayTime(wd time.Weekday, hour, min int) time.Duration {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
	for next.Weekday() != wd || !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.Sub(now)
}

func nextMonthDayTime(day, hour, min int) time.Duration {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), day, hour, min, 0, 0, now.Location())
	if !next.After(now) {
		if now.Month() == 12 {
			next = time.Date(now.Year()+1, 1, day, hour, min, 0, 0, now.Location())
		} else {
			next = time.Date(now.Year(), now.Month()+1, day, hour, min, 0, 0, now.Location())
		}
	}
	return next.Sub(now)
}

func nextYearDayTime(day, hour, min int) time.Duration {
	now := time.Now()
	next := time.Date(now.Year(), 1, day, hour, min, 0, 0, now.Location())
	if !next.After(now) {
		next = time.Date(now.Year()+1, 1, day, hour, min, 0, 0, now.Location())
	}
	return next.Sub(now)
}
