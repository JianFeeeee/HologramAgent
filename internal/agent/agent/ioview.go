package agent

// ioview.go —— 基座对 **虚拟 IO 管理器** 的取用面（用户裁定 2026-11-27）。
//
// # 与 pluginvirtual.go 同构
//
//	真实 plghost（kstate.FacPluginRegistry）→ PluginHost()  按 a.id 过滤
//	真实 IOManager（kstate.FacIO）        → IOView()       按 a.id 过滤
//
// 两者都是「内核唯一事实 + 每实例视图」，都由基座一处现取 —— 类型不问、
// 不缓存、不继承父的那份。
//
// # 为什么不再用 SetParentIO 那套
//
// 改造前子实例的建法是「空 IOManager + 共享父的 ChannelRegistry + 父回退」，
// 两个口子都让子看见父的全部通道与设备。于是"通道归属 / 输出授权"的隔离
// 只剩最后的执行闸，可见面早已全开 —— 模型能看见一个必然被拒的通道。
//
// 现在视图按 `a.id` 过滤：入站看 `Owner`，出站看该实例的输出授权。

import (
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// IOView 返回本实例视角的 IO 视图（未装配 ⇒ nil）。
//
// 可见范围：
//
//	入站通道   Owner == 本实例（根实例可见全部 —— 它是通道的兜底持有者）
//	出站设备   该实例的输出授权（`allowedOutputs`，空 = 完整授权）
//
// ★ 每次调用**现算**，不做快照：设备随资源生灭（远程设备上下线以分钟计），
// 快照转瞬就过期。
func (a *Agent) IOView() *agentIO.IOView {
	if a == nil {
		return nil
	}
	// ★ 视图必须基于**真实的全局 io**（kstate 设施），不是 `a.io`：
	//   设备与通道都登记在内核那一份 io 上（插件注册在父的 io 上，而父的
	//   io 就是内核那份）。`a.io` 是实例自己的**队列**（输入入口 + 输出分发），
	//   不是资源登记表。
	real := a.kernelIO()
	if real == nil {
		// 未装配内核 io（单测/独立工具）：退回自己的队列视图，行为不变。
		real = a.io
	}
	if real == nil {
		return nil
	}
	// 根实例看得见全部通道：它是"未分配通道"的兜底持有者（见 InputChannel
	// 的 Owner 语义），也需要全局视野做诊断。
	return real.VirtualIO(string(a.id), !a.IsChildAgent(), a.allowedOutputs)
}

// kernelIO 取内核唯一的 IOManager（未装配 ⇒ nil）。
func (a *Agent) kernelIO() *agentIO.IOManager {
	ks := kstate.Get()
	if ks == nil {
		return nil
	}
	impl, ok := ks.Require(kernelapi.FacIO)
	if !ok || impl == nil {
		return nil
	}
	iom, _ := impl.(*agentIO.IOManager)
	return iom
}

// channelReg 取内核真实的通道登记表（未装配 ⇒ 退回自己的队列登记表）。
//
// ★ 基座内部所有"通道归属/状态"读写都走这里，不走 `a.io.ChannelRegistry()`：
//
//	后者是**实例自己那份**（单测里独立、生产里也可能与内核不同）。
//	归属是内核事实，必须读内核那份。
func (a *Agent) channelReg() *agentIO.ChannelRegistry {
	if a == nil {
		return nil
	}
	if real := a.kernelIO(); real != nil {
		return real.ChannelRegistry()
	}
	if a.io != nil {
		return a.io.ChannelRegistry()
	}
	return nil
}
