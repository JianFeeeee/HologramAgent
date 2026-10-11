package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// worker 退出码协议：guard 通过 worker 退出码判定下一步动作。
// 该协议跨平台一致（Windows 下 guard 为 no-op，退出码仍保留）。
const (
	exitRestartRequested = 42 // worker 清理后请求重建（不计失败轮次）
	exitRecovered        = 43 // failback worker 恢复成功，guard 应重置失败轮次并交回主 agent
	exitRecoveryFailed   = 44 // failback worker 单轮恢复失败，guard 计入失败并进入下一轮/最后手段

	// exitGuardRestartRequested 是 guard 自身的退出码：L2 需要「重启本单元」但
	// restart_command 指向自己所在单元时（systemctl restart 会自杀），
	// guard 改为直接退出，由 systemd 的 Restart=always 重新拉起 —— 等价且无自杀窗口。
	//
	// 刻意取 41（小于 worker 的 42-44 区间），避免与 worker 退出码协议混淆。
	exitGuardRestartRequested = 41
)

// resolveDataDir 复用原有的数据目录探测逻辑。
func resolveDataDir(dataDir string) string {
	if dataDir != "" {
		return dataDir
	}
	exe, err := os.Executable()
	if err == nil {
		return filepath.Join(filepath.Dir(exe), "data")
	}
	if exe, err := exec.LookPath(os.Args[0]); err == nil {
		return filepath.Join(filepath.Dir(exe), "data")
	}
	return "./data"
}
