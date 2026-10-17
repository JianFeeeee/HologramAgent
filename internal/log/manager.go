package log

import (
	"context"
	safeclose "github.com/JianFeeeee/HomeAgent/internal/safeclose"
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

func (m *Manager) applyRetention() {
	policy := RetentionPolicy(m.cfg.GetString("core.log.retention", "forever"))
	months := m.cfg.GetInt("core.log.retention_months", 3)
	applyRetention(m.logDir, policy, months)
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
