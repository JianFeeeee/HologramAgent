// kernel_update 工具的 agent 侧实现。
//
// ## 三个入口共享一个底层（架构约束）
//
//	agent 工具 kernel_update  ┐
//	WebAPI /kernel/update     ├─ internal/update（CheckRelease / DoUpgrade）
//	CLI homed-upgrade         ┘
//
// 三份实现必然漂移（"agent 说没有新版、WebUI 却升了"），所以入口层只做
// 参数解析与结果呈现。
//
// ## 为什么实现放在 pluginmgr 而不是 agent/core
//
// 1. 与 plugin_update 对称（都在 pluginmgr，用户认知一致）
// 2. agent/core 拿不到 SDK 句柄，而升级完成后要 RequestKernelRestart
//    （那是 sdk.KernelControlAPI）
// 3. 与内核工具分派（toolcall.go）不同：这里走插件的 RegisterTool，
//    不需要改核心包
//
// ## 两阶段（用户口径）
//
//	apply=false（默认）→ 只查 GitHub Release 与本地版本比较，不改动任何东西
//	apply=true          → 下载/解包 → 校验 → 备份 → 原子替换 → 重启 → 版本复核
//	                     → 失败自动回滚（DoUpgrade 内部完成）

package pluginmgr

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/update"
)

// kernelUpdateCheck 是 kernel_update 的检查半边（apply=false）。
func (p *Plugin) kernelUpdateCheck() (interface{}, error) {
	cur := currentKernelVersion(p)
	info := update.CheckRelease(cur)
	return map[string]interface{}{
		"apply":     false,
		"current":   info.Current,
		"latest":    info.Latest,
		"has_update": info.HasUpdate,
		"asset":     info.Asset,
		"release":   info.LatestURL,
		"reason":    info.Reason,
		"hint":      "确认后传 apply=true 执行升级（会替换二进制并重启内核，连接断开数秒）",
	}, nil
}

// kernelUpdateApply 是 kernel_update 的执行半边（apply=true）。
//
// 只接受**本机已有的包**（pkg 参数）：从 URL 现场下载需要处理
// 校验/断点/重试，那是 CLI 的职责（homed-upgrade），agent 不应在
// 推理过程中发起长下载（会占满工具调用超时）。
func (p *Plugin) kernelUpdateApply(pkgPath string) (interface{}, error) {
	if pkgPath == "" {
		return map[string]interface{}{
			"error": "apply=true 需要 pkg（本机已存在的安装包路径；下载请跑 homed-upgrade -pkg <url>）",
		}, nil
	}
	st, err := os.Stat(pkgPath)
	if err != nil || st.IsDir() {
		return map[string]interface{}{
			"error": fmt.Sprintf("pkg 无效：%s", pkgPath),
		}, nil
	}

	tmp, err := os.MkdirTemp("", "kernel-upgrade-*")
	if err != nil {
		return nil, fmt.Errorf("建临时目录: %w", err)
	}
	defer os.RemoveAll(tmp)

	root, err := update.ExtractPkg(pkgPath, tmp)
	if err != nil {
		return nil, fmt.Errorf("解包: %w", err)
	}
	newBin := update.FindFile(root, "homed")
	newDG := update.FindFile(root, "depgraph.bin")
	if newBin == "" {
		return nil, fmt.Errorf("包内找不到 homed")
	}

	// ★ 升级会让本进程消失：先把"已受理"回给模型，再执行替换。
	//
	// 否则模型看到的是工具调用超时/断线，无法区分"升级成功"和"工具坏了"。
	// 返回后由本 goroutine 继续跑 DoUpgrade（替换+重启+回滚）。
	if kc := p.sdk.KernelControl(); kc != nil {
		_ = kc.RequestKernelRestart("kernel_update apply")
	}
	go func() {
		if err := update.DoUpgrade(root, kernelBinPath(p), newDG, p.pluginDir, kernelDataDir(p)); err != nil {
			// DoUpgrade 内部失败已自行回滚；这里只留一条诊断日志
			fmt.Fprintf(os.Stderr, "[pluginmgr] kernel_update upgrade failed (rolled back): %v\n", err)
		}
	}()
	return map[string]interface{}{
		"accepted": true,
		"message":  "升级已受理：正在替换二进制并重启。完成后用 kernel_update（apply=false）确认新版本。",
	}, nil
}

// kernelDataDir 推导数据目录（升级确认标记的落点）。
//
// 从 pluginDir 反推：<dataDir>/plugins ⇒ 上一级即 dataDir。
// 与内核 --data 的值一致，update 模块（第 0 层）才能读到同一个标记。
func kernelDataDir(p *Plugin) string {
	if p.pluginDir != "" {
		return filepath.Dir(filepath.Dir(p.pluginDir))
	}
	return "/home/newqqagent"
}

// currentKernelVersion 读本机 homed 自报版本（尽力而为）。
func currentKernelVersion(p *Plugin) *update.BinVersion {
	v, err := update.ReadBinVersion(kernelBinPath(p))
	if err != nil {
		return nil
	}
	return v
}

// kernelBinPath 是本机 homed 的路径。
//
// 由 pluginDir 推导（<data>/../homed 不总是成立），优先按**本进程**
// 的可执行路径：homed 与内核同二进制，这是最可靠的来源。
func kernelBinPath(p *Plugin) string {
	if exe, err := os.Executable(); err == nil && filepath.Base(exe) == "homed" {
		return exe
	}
	if _, err := os.Stat("/usr/local/bin/homed"); err == nil {
		return "/usr/local/bin/homed"
	}
	return "homed"
}
