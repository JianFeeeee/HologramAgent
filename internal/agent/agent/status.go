package agent

import (
	"fmt"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"

	"runtime"

	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/meta"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// StatusProvider 内核状态查询接口。插件通过此接口查看内核运行动态。
type StatusProvider interface {
	GetKernelStatus() *KernelStatus
}

// 状态 DTO 使用内置 SDK 的中立类型，保证与插件层解耦。
type KernelStatus = sdk.KernelStatus
type PluginInfo = sdk.PluginInfo
type ChannelInfo = sdk.ChannelInfo

// InputChannelInfo 是 inputch 的登记与归属视图（见 internal/sdk/status.go）。
type InputChannelInfo = sdk.InputChannelInfo

// ResidentStatus 是驻留式子 agent 的运行时视图（见 internal/sdk/status.go）。
type ResidentStatus = sdk.ResidentStatus

// ★ 已删五个记忆/知识类状态别名（2026-11-25）：
//
//	MemoryStatus / TextMemoryStatus / KnowledgeStatus / DocumentStatus / SocialStatus
//
// 归**主 agent 的状态面**（它持有记忆实现）。基座不该有这些概念 ——
// 判据 §13「基座不认识图记忆」正是要 grep 到 0。主 agent 直接引用
// `sdk.XxxStatus`（它们本来就是 SDK 的公开类型，别名只是转发）。
type LLMStatus = sdk.LLMStatus
type RuntimeStatus = sdk.RuntimeStatus
type TrackerStatus = sdk.TrackerStatus
type BuildStatus = sdk.BuildStatus

func channelInfoFromIO(ch agentIO.ChannelInfo) ChannelInfo {
	tools := make([]string, 0, len(ch.Tools))
	for _, t := range ch.Tools {
		tools = append(tools, t.Name)
	}
	return ChannelInfo{
		Name:        ch.Name,
		Type:        fmt.Sprintf("%d", ch.Type),
		Direction:   channelDirection(ch.Type),
		Ready:       true,
		Description: ch.Description,
		Tools:       tools,
		OutputCaps:  int(ch.OutputCaps),
		CapsText:    ch.OutputCaps.String(),
	}
}

// channelDirection 把 DeviceType 翻成可读方向（前端画拓扑用）。
// 未知取值落到 "io"：宁可当作双向，也不要把它画成只进或只出。
func channelDirection(t agentIO.DeviceType) string {
	switch t {
	case agentIO.DeviceInput:
		return "in"
	case agentIO.DeviceOutput:
		return "out"
	default:
		return "io"
	}
}

// collectKernelStatus 收集**基座公共面**的状态（类型专属的段由持有者注册）。
//
// ## 这里只留真正公共的
//
//	Uptime / StartTime / Build / AgentID   内核身份
//	Plugins / Tools / Channels / InputChannels  已装配能力（每个 agent 都看得见）
//	Usage                                   累计用量账本
//	runtime / llm 两个 section              与任何 agent 类型无关的事实
//
// ## 这里**不**再填的（已改成注册段）
//
//	memory / knowledge / documents / text_memory / social / tracker / onnx
//	context / scheduler / residents
//
// 它们要么生产下恒为零值（句柄为 nil），要么装的是**类型专属结构**。
// 基座假设"所有 agent 都有这些面"就是把三形态塞进一个结构体。
func collectKernelStatus(
	startTime time.Time,
	agentID string,
	providerName string,
	sourceCount int,
	toolReg *kit.ToolRegistry,
	iom *agentIO.IOManager,
	pluginReg kernelapi.PluginHost,
) *KernelStatus {
	status := &KernelStatus{
		Uptime:    time.Since(startTime).Round(time.Second).String(),
		StartTime: startTime.Format(time.RFC3339),
		AgentID:   agentID,
		// 构建身份取自内核自己的 meta（-ldflags 注入点），
		// 而非 SDK 仓的硬编码版本。
		Build: BuildStatus{
			Version:       meta.Version,
			Commit:        meta.Commit,
			BuildTime:     meta.BuildTime,
			SDKCompatible: meta.SDKCompatibleVersion,
			KernelName:    meta.KernelName,
			// AGPL-3.0 §13：状态页向网络使用者展示取得源码的入口。
			SourceURL: meta.SourceURL,
			// 许可标识与全文：与源码链接一起构成「这是什么许可 + 怎么拿到源码」。
			License:    meta.License,
			LicenseURL: meta.LicenseURL,
		},
	}

	// Plugins
	if pluginReg != nil {
		names := pluginReg.List()
		for _, n := range names {
			status.Plugins = append(status.Plugins, PluginInfo{Name: n, Loaded: true})
		}
	}

	// Tools
	if toolReg != nil {
		// ★ 只给**摘要**，不给完整定义（2026-10-26 实测）。
		//
		// 原先直接塞 GetToolDefs()：173 个工具的完整 Description + Parameters
		// JSON ⇒ healthcheck_kernel 单条结果 **98,989 tokens**（预算 65,375 的 151%），
		// 一轮对话里它就把上下文塞满，还拖慢每一次后续往返。
		//
		// 摘要保留"有哪些工具、属于哪个插件、能干什么"，足够状态页与模型判断；
		// 需要完整参数表的场景走 get_plugin_tools(<plugin>)，那是按需的。
		defs := toolReg.GetToolDefs()
		summary := make([]sdk.ToolDef, 0, len(defs))
		for _, d := range defs {
			desc := d.Description
			if r := []rune(desc); len(r) > 120 {
				desc = string(r[:120]) + "…"
			}
			summary = append(summary, sdk.ToolDef{
				Name:        d.Name,
				Plugin:      d.Plugin,
				Description: desc,
			})
		}
		status.Tools = summary
	}

	// Channels + inputch 归属
	if iom != nil {
		for _, ch := range iom.ListChannels() {
			status.Channels = append(status.Channels, channelInfoFromIO(ch))
		}
		// inputch 登记与归属：与 Channels（设备能力）互补——那条回答「能做什么」，
		// 这条回答「这条输入归谁」。登记表是根 agent 与驻留子共用的，所以驻留子
		// 划走的 inputch（owner=子 id）也会出现在这里。
		for _, ic := range iom.InputChannels() {
			status.InputChannels = append(status.InputChannels, InputChannelInfo{
				Name:     ic.Name,
				Plugin:   ic.Plugin,
				Owner:    ic.Owner,
				Capacity: ic.Capacity,
				Output:   ic.Output,
			})
		}
	}

	return status
}

// NewRuntimeSection 是**基座注册**的 "runtime" 段（与任何 agent 无关）。
//
// 它凭什么归基座：Go runtime 统计在主 agent 挂掉之后依然存在，
// 与任何类型的上下文管理/记忆策略无关。
func NewRuntimeSection() RuntimeStatus {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return RuntimeStatus{
		Goroutines: runtime.NumGoroutine(),
		MemoryMB:   int64(m.Alloc / 1024 / 1024),
		GoVersion:  runtime.Version(),
	}
}

// NewLLMSection 是**基座注册**的 "llm" 段。
//
// 它凭什么归基座：`Provider` / `ProviderManager` / `PreferredProvider` 是
// `AgentConfig` 的**公有字段**，主/驻留/rt 每个实例都有；只由主 agent 注册，
// 驻留子与 rt 的源可用性就会在状态面完全消失。
func NewLLMSection(providerName string, sourceCount int) LLMStatus {
	return LLMStatus{
		Available: providerName != "",
		Provider:  providerName,
		Sources:   sourceCount,
	}
}

// ★ 已删 `residentStatuses`（2026-11-20）：它是**零调用者**的死转换函数，
//   而且入参是驻留子的私有类型（基座不认识它）。
//   驻留子的状态面由 `resident` 自己的 `SchedulerStatus()` / `ResidentInfo`
//   提供，经实例登记表读，不经过基座。
