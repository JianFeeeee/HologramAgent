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
}

func NewManager(logDir string, cfg ConfigProvider) *Manager {
	return &Manager{logDir: logDir, cfg: cfg, done: make(chan struct{})}
}

func (m *Manager) Start(ctx context.Context) {
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
