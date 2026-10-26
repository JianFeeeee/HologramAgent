// Package tracker 是 tracker 模块：变更追踪（overlayfs）。
//
// 依赖 config + AgentWorkDir（后者由 Group 0 的 ensureDataDirs 产出）。
package tracker

import (
	"errors"
	"log"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/tracker"
)

// Module 提供变更追踪。
type Module struct{}

func (Module) Name() string       { return "tracker" }
func (Module) Requires() []string { return []string{kernelmod.FacConfig} }
func (Module) Provides() []string { return []string{kernelmod.FacTracker} }

func (Module) Init(ks *kernelboot.KernelState) error {
	trk := tracker.NewTracker(ks.Cfg.Daemon.DataDir, ks.AgentWorkDir,
		tracker.WithKeepChangesets(100),
		tracker.WithMaxChangesetAge(30*24*time.Hour),
	)
	if err := trk.Init(); err != nil {
		log.Printf("[homed] warning: tracker init: %v", err)
	} else if err := trk.Start(); err != nil {
		if errors.Is(err, tracker.ErrOverlayNoPrivilege) {
			// 用户 2026-10-26 拍板方案 A：服务降权运行，接受变更追踪降级。
			// 日志必须说清"是什么、影响什么、怎么恢复"，而不是"可能不支持"。
			log.Printf("[homed] change tracker degraded: agentfs overlay needs mount privilege, " +
				"service runs as non-root (decision A, 2026-10-26). " +
				"Consequence: agentfs file-change tracking/rollback is disabled; chat, memory, " +
				"plugins and tools are unaffected. To restore: run as root, or grant mount capability.")
		} else {
			log.Printf("[homed] warning: tracker mount overlay: %v (non-fatal)", err)
		}
	} else {
		log.Printf("[homed] change tracker active at %s", trk.MergeDir())
	}
	ks.Tracker = trk
	return nil
}

func init() { kernelmod.Register(Module{}) }
