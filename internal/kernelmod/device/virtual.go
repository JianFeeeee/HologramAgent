package device

// virtual.go —— **按实例裁剪的虚拟设备管理器**（用户裁定 2026-11-27）。
//
// # device 是什么
//
// device = **tool + ch 的抽象**：
//
//	IOManager.devices     → 出站设备表（tool 面）
//	IOManager.channelReg  → 入站通道表（ch 面）
//	plghost               → 插件注册的一切（工具 / stage / 段 / skill / API）
//
// 所以 device 是这三者的**统一管理者**，也是它们的**执行者**（工具执行链）。
//
// # 虚拟化语义
//
// 内核真实事实只有一份；每个 agent 实例看到的是**按自己裁剪的视图**：
//
//	真实（唯一）                   视图（每实例一个）
//	├ plghost            →  插件可见性（Grant 表运算）
//	├ IOManager.devices  →  设备可见性（输出授权）
//	└ IOManager.channel  →  通道归属（Owner）
//
// 三条纪律（与 plugin.virtualHost 同构）：
//
//	① real 是唯一事实 —— 视图不复制数据，只持指针
//	② 每条查询转给 real，再按该实例的归属/授权过滤
//	③ 事实层一行不改 —— 视图是另一个对象
//
// # 谁拉
//
// **agent 模块拉，device 不推**。此前 device 依赖 FacAgent 把数据面
// 推给 agent（`host.MountDevice`），而那个数据面全仓零消费 —— 倒挂且无效。
//
//	agent → Require(FacDevice) → VirtualDevice(agentID) → 自己的视图

import (
	"context"

	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
)

// DeviceView 是一个实例视角的设备管理器（tool + ch + 插件三面合一）。
//
// 它是**同接口对象**：调用方原本怎么用，仍然怎么用，只是每条查询已过滤。
type DeviceView struct {
	io      *agentIO.IOView
	plugins kernelapi.PluginHost
	tools   *kit.ToolRegistry
	exe     *Executor
	agentID string
}

// VirtualDevice 返回某实例视角的虚拟设备管理器。
//
// 每次调用返回新对象（只几个指针，无快照）⇒ 设备/通道/插件变化**立刻**反映。
func (d *Device) VirtualDevice(agentID string, isChild bool, allowedOutputs []string) *DeviceView {
	if d == nil {
		return nil
	}
	// 根实例看得见全部通道（它是"未分配通道"的兜底持有者）；
	// 插件视图的写权限只给主 agent（它是唯一调 admin_child 的实例）。
	var iov *agentIO.IOView
	if d.io != nil {
		iov = d.io.VirtualIO(agentID, !isChild, allowedOutputs)
	}
	var pv kernelapi.PluginHost
	if d.plugins != nil {
		pv = d.plugins.(*plugin.Registry).VirtualPlugins(agentID, !isChild)
	}
	return &DeviceView{
		io:      iov,
		plugins: pv,
		tools:   d.tools,
		exe:     d.exe,
		agentID: agentID,
	}
}

// ───────────────────────── tool 面 ─────────────────────────

// Tools 返回本实例可见的设备工具（已按输出授权与通道归属过滤）。
func (v *DeviceView) Tools() []agentIO.ToolDef {
	if v == nil || v.io == nil {
		return nil
	}
	return v.io.GetAllTools()
}

// ToolRegistry 返回插件工具表（执行链用；可见性由插件视图把关）。
func (v *DeviceView) ToolRegistry() *kit.ToolRegistry {
	if v == nil {
		return nil
	}
	return v.tools
}

// Execute 执行一批工具调用（走完整内核执行链）。
//
// ★ 参数与返回都是**纯数据类型**（`agentPkg.ToolCall` / `BatchResult`），
//
//	由 `internal/agent/agent` 定义。该包**零 kernelmod 依赖**，所以 device
//	import 它是单向的、不成环；而且只是用它的 struct，不碰实现。
func (v *DeviceView) Execute(ctx context.Context, h AgentHandle, calls []agentPkg.ToolCall) []agentPkg.BatchResult {
	if v == nil || v.exe == nil {
		return nil
	}
	return v.exe.Execute(ctx, h, calls)
}

// ───────────────────────── ch 面 ─────────────────────────

// Channels 返回本实例可见的出站设备清单。
func (v *DeviceView) Channels() []agentIO.ChannelInfo {
	if v == nil || v.io == nil {
		return nil
	}
	return v.io.ListChannels()
}

// InputChannels 返回本实例可见的入站通道清单（按 Owner 过滤）。
func (v *DeviceView) InputChannels() []agentIO.InputChannel {
	if v == nil || v.io == nil {
		return nil
	}
	return v.io.InputChannels()
}

// GetDevice 取本实例可见的设备（未授权 ⇒ nil）。
func (v *DeviceView) GetDevice(name string) agentIO.Device {
	if v == nil || v.io == nil {
		return nil
	}
	return v.io.GetDevice(name)
}

// OutputAllowed 报告本实例能否向该通道输出。
func (v *DeviceView) OutputAllowed(channel string) bool {
	if v == nil || v.io == nil {
		return false
	}
	return v.io.OutputAllowed(channel)
}

// ───────────────────────── 插件面 ─────────────────────────

// Plugins 返回本实例视角的插件管理器（可见性已按授权裁剪）。
func (v *DeviceView) Plugins() kernelapi.PluginHost {
	if v == nil {
		return nil
	}
	return v.plugins
}

// ───────────────────────── 管理器本体 ─────────────────────────

// Device 是 device 模块产出的**唯一管理者**（真实事实的持有者）。
//
// 它不持有任何 per-agent 状态：视图按 agentID 现算，所以一个实例服务全部 agent
// 而不会让"主 agent 的设置泄漏给子"。
type Device struct {
	io      *agentIO.IOManager
	plugins kernelapi.PluginHost
	tools   *kit.ToolRegistry
	exe     *Executor
}

// NewDevice 构造 device 管理者（装配期一次）。
func NewDevice(io *agentIO.IOManager, plugins kernelapi.PluginHost, tools *kit.ToolRegistry, exe *Executor) *Device {
	return &Device{io: io, plugins: plugins, tools: tools, exe: exe}
}

// Tools 返回**全量**设备工具（管理面/状态用；实例视角请走 VirtualDevice）。
func (d *Device) Tools() []agentIO.ToolDef {
	if d == nil || d.io == nil {
		return nil
	}
	return d.io.GetAllTools()
}

// Executor 返回工具执行设施（基座转发用）。
func (d *Device) Executor() *Executor {
	if d == nil {
		return nil
	}
	return d.exe
}
