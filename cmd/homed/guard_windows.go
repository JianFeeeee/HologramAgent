//go:build !linux

// L0/L1/L2 三层回退恢复机制为 Linux 专属（依赖 /etc 网络基线、overlayfs、
// syscall.Reboot、unix socket IPC、systemctl）。在 Windows 等非 Linux 平台
// 本文件提供 no-op 桩：guard/failback 角色不执行恢复，其余主 agent 功能照常。
package main

import (
	"log"
	"os"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	luaVM "github.com/JianFeeeee/HologramAgent/internal/lua"
)

// runGuard 桩：guard 父守护模式仅支持 Linux，其余平台打印提示并退出。
func runGuard(dataDir string) {
	log.Printf("[guard] guard mode is Linux-only (data=%s), exiting", dataDir)
	os.Exit(0)
}

// runFailbackRecovery 桩：failback 恢复为 Linux 专属，其余平台直接退出。
func runFailbackRecovery(dataDir string, cfgReg *internalConfig.ConfigRegistry, lua *luaVM.VM, providerMgr *agentAPI.ProviderManager, baseAPIKey string) {
	log.Printf("[failback] recovery is Linux-only (data=%s), exiting", dataDir)
	os.Exit(exitRecoveryFailed)
}

// lastDiagSummary 桩：无 recovery_result 时返回空自诊断。
func lastDiagSummary(dataDir string) string {
	return ""
}
