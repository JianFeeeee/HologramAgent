package update

import (
	"os"
	"path/filepath"
)

// State 是更新子系统的公共数据（挂在 KernelState.Update 上，由 update 模块填充）。
//
// 只放**别人要读**的东西：版本、是否回滚过、二进制路径。
// 更新实现（Check/Upgrade/DoUpgrade）是函数，不需要常驻状态。
//
// ★ 定义在 internal/update 而不是 kernelmod：三端（CLI/WebAPI/agent 工具）
// 都要用，而 KernelState 只有内核启动一条路径。模块通过
// kernelboot.UpdateStateProvider 接口把它暴露给内核，避免 import 循环。
type State struct {
	BinaryPath   string
	DepGraphPath string
	Version      string
	Commit       string
	SDKVersion   string
	ProbeErr     error

	// RolledBack 表示本次启动是否回滚了上一次升级。
	RolledBack bool
	RolledFrom string
	RolledTo   string
}

// Summary 返回给 healthcheck / 状态面的摘要。
func (s *State) Summary() map[string]interface{} {
	if s == nil {
		return nil
	}
	out := map[string]interface{}{
		"version":     s.Version,
		"commit":      ShortCommit(s.Commit),
		"sdk_version": s.SDKVersion,
		"binary":      s.BinaryPath,
		"rolled_back": s.RolledBack,
	}
	if s.ProbeErr != nil {
		out["probe_error"] = s.ProbeErr.Error()
	}
	if s.RolledBack {
		out["rolled_from"], out["rolled_to"] = s.RolledFrom, s.RolledTo
	}
	return out
}

// KernelBinaryPath 返回本进程的内核二进制路径。
//
// 用 os.Executable 而不是硬编码 /usr/local/bin/homed：部署路径可变
// （install.sh 支持 PREFIX），而"我自己是谁"永远可靠。
func KernelBinaryPath() string {
	if exe, err := os.Executable(); err == nil {
		if resolved, err2 := filepath.EvalSymlinks(exe); err2 == nil {
			return resolved
		}
		return exe
	}
	return "homed"
}
