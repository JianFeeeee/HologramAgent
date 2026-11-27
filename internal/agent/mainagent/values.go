package mainagent

import "time"

// OffloadOptions 是**积压任务自动转投**的判定与执行参数。
//
// ★ 归主 agent：自动转投是它的一个决策（"我忙不过来，把活分出去"），
//
//	不是内核的通用能力。默认关闭（Enabled=false）—— 它让类型替父做决策，
//	必须由部署方显式打开。
//
// OffloadOptions 是自动转投的判定与执行参数。
type OffloadOptions struct {
	// BusyAfter：运行任务已持续多久算"长时间工作"（0 = 用默认）。
	BusyAfter time.Duration
	// MinPending：至少要积压多少条才值得拉起驻留子（0 = 用默认）。
	MinPending int
	// MaxResidents：为转投而拉起的驻留子上限（0 = 用默认）。
	MaxResidents int
	// Enabled 为 false 时完全关闭（默认关：见 DefaultOffloadOptions 的说明）。
	Enabled bool
}
