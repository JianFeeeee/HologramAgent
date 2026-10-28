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
//	apply=true          → 解包 → 校验 → 备份 → 原子替换（**不重启**）
//	                     → 请求 guard 重拉 → 新内核启动时由 update 模块确认或回滚
//
// ★ 重启为什么不在 internal/update 里做：worker 是被守护方，只有 guard 知道
// 这次退出该不该重拉（exit 42 → 重拉且不计失败轮次）。详见
// cmd/homed/kernel_control.go。

package pluginmgr

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/update"
)

// kernelUpdateCheck 是 kernel_update 的检查半边（apply=false）。
func (p *Plugin) kernelUpdateCheck() (interface{}, error) {
	cur := currentKernelVersion(p)
	info := update.CheckRelease(cur)
	return map[string]interface{}{
		"apply":      false,
		"current":    info.Current,
		"latest":     info.Latest,
		"has_update": info.HasUpdate,
		"asset":      info.Asset,
		"release":    info.LatestURL,
		"reason":     info.Reason,
		"hint":       "确认后传 apply=true 执行升级（会替换二进制并重启内核，连接断开数秒）",
	}, nil
}

// kernelUpdateApply 是 kernel_update 的执行半边（apply=true）。
//
// 来源二选一（update.ParseSource）：
//
//	· pkg 非空        → 用本机已有的安装包
//	· release=true    → 从仓库最新 GitHub Release 自动下载
//
// ★ 顺序（此前是反的，会出真事故）：
//
//	先 Apply（备份→pending→替换），**再**请求重启。
//	反过来（先请求重启）会在新二进制还没就位时就重拉，
//	或重启恰好发生在替换中途 ⇒ 跑起来的是半新半旧的文件。
//
// ★ 重启走既有的 p.sdk.KernelControl()（restartCh → exit 42 → guard 重拉）。
// 它依赖 kernelControlAllowed 白名单 —— pluginmgr 已加入（否则该接口恒为 nil，
// 以前写在那里等于静默不重启）。
func (p *Plugin) kernelUpdateApply(pkgPath string, release bool) (interface{}, error) {
	src, err := update.ParseSource(pkgPath, release)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}

	tmp, err := os.MkdirTemp("", "kernel-upgrade-*")
	if err != nil {
		return nil, fmt.Errorf("建临时目录: %w", err)
	}
	defer os.RemoveAll(tmp)

	root, rel, err := src.Resolve(tmp)
	if err != nil {
		return map[string]interface{}{"error": "取包失败: " + err.Error()}, nil
	}
	newBin := update.FindFile(root, "homed")
	newDG := update.FindFile(root, "depgraph.bin")
	if newBin == "" {
		return map[string]interface{}{"error": "包内找不到 homed"}, nil
	}

	// 走事件总线：升级的每一步都让 WebUI 看得到。
	var bus *events.Bus
	if p.sdk != nil {
		bus = p.sdk.EventBus()
	}

	out, err := update.Apply(root, kernelBinPath(p), newDG, update.ResolveDataDir(kernelBinPath(p)), bus)
	if err != nil {
		return map[string]interface{}{"error": err.Error()}, nil
	}

	// 产物已就位 ⇒ 走**既有**重启通道（KernelControlAPI → restartCh → exit 42
	// → guard 用新二进制重拉）。不自己 systemctl，也不另开通道。
	//
	// ★ 顺序关键：Apply 完成**之后**才请求重启。反过来会在新二进制还没
	// 就位时重拉，或重启恰好落在替换中途。
	if kc := p.sdk.KernelControl(); kc != nil {
		if rerr := kc.RequestKernelRestart("kernel_update apply"); rerr != nil {
			return map[string]interface{}{
				"applied": true,
				"error":   "新内核已就位，但请求重启失败: " + rerr.Error(),
				"hint":    "手动重启服务即可；pending 标记会让首次启动自动确认或回滚",
			}, nil
		}
	} else {
		// 拿不到通道时**如实报告**，不要静默当成功：否则用户以为已升级，
		// 而进程还在跑旧镜像；pending 标记会在下次重启时被判"未确认"→ 回滚。
		return map[string]interface{}{
			"applied": true,
			"error":   "新内核已就位，但本插件没有被授予内核控制通道（kernelControlAllowed），无法请求重启",
			"hint":    "手动重启服务完成升级；或改用 WebUI 的 /api/v1/kernel/update",
		}, nil
	}

	res := map[string]interface{}{
		"applied": true,
		"from":    out.From.Version,
		"to":      out.To.Version,
		"backup":  out.BackupDir,
		"message": "新内核已就位，正在由 guard 重拉。重启后若新版本无法启动，第 0 层 update 模块会自动回滚。",
	}
	if rel != nil {
		res["release"] = rel.Latest
		res["asset"] = rel.Asset
	}
	return res, nil
}

// kernelDataDir 推导数据目录（升级确认标记 / 备份的落点）。
//
// ★ 三端共用 update.ResolveDataDir：CLI、WebUI、agent 工具必须算出**同一个**
// 路径，否则 pending 标记落在哪、第 0 层 update 就从哪读，两边一漂移，
// 升级启动失败时就静默不回滚（本文件此前反推 pluginDir 的做法就是这么
// 与 webui 侧走散的）。
func kernelDataDir(p *Plugin) string {
	return update.ResolveDataDir(kernelBinPath(p))
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

// onUpdateRequest 处理来自其他插件的更新请求（EventBus）。
//
// 请求方不必知道"怎么更新"，只需发一个事件：
//
//	scope:   "kernel" | "plugin"
//	name:    插件名（scope=plugin 时必填）
//	pkg:     本机安装包路径（可选）
//	release: bool，从仓库最新 Release 自动取包（可选）
//
// 为什么必须**异步**：升级会替换二进制并（对内核）请求重启，
// 本进程随后消失。事件处理器是同步投递的（见 events.Bus.Publish），
// 在处理器里同步做完会让发布方卡住直到进程被杀。
func (p *Plugin) onUpdateRequest(evt *events.Event) {
	if evt == nil || evt.Payload == nil {
		return
	}
	scope, _ := evt.Payload["scope"].(string)
	name, _ := evt.Payload["name"].(string)
	pkgPath, _ := evt.Payload["pkg"].(string)
	release, _ := evt.Payload["release"].(bool)

	go func() {
		var err error
		switch scope {
		case "kernel":
			_, err = p.kernelUpdateApply(pkgPath, release)
		case "plugin":
			if name == "" {
				err = fmt.Errorf("plugin 更新请求缺 name")
				break
			}
			// 复用既有 plugin_update 的 apply 通路（范围即该插件名）
			_, err = p.pluginUpdate(true, name)
		default:
			err = fmt.Errorf("未知更新范围 %q（来源 %s）", scope, evt.Source)
		}
		if err != nil {
			// 事件是单向的，没有返回值可取 ⇒ 失败必须留日志，
			// 否则请求方看到的就是"发了、没反应"。
			fmt.Fprintf(os.Stderr, "[pluginmgr] update_request (%s/%s) failed: %v\n", scope, name, err)
		}
	}()
}
