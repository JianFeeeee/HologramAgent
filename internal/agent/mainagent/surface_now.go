package mainagent

// surface_now.go —— 基座的面**现读**（不存快照）。
//
// ## 为什么不缓存
//
// `IO` / `StageHost` / `PluginDir` 都经 `MountSurface` 挂到基座，
// （`PluginHost` 已改为按 agentID 从 kstate 现取，不再是挂载的字段。）
//
// 挂载发生在 `Construct` 之后（装配点在 `New` 返回后调 MountSurface，
// 类型自己的 `Initialize` 又早于那次挂载 —— 实测顺序）。
// 缓存它们会在装配期留下 nil 快照，症状是"工具清单为空、插件面全死"，
// 且只在某些装配顺序下发生。
//
// ★ 每读一次都是一次 `semanticMu.RLock` + 字段取值（纳秒级），
// 不值得为它引入"快照 + 失效"这套东西。

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// stageHostNow 返回阶段管道（未挂载 ⇒ nil）。
func (m *Module) stageHostNow() *kit.StageHostType {
	if m == nil || m.a == nil {
		return nil
	}
	return m.a.StageHost()
}

// toolRegNow 返回**插件工具登记簿**（未挂载 ⇒ nil）。
//
// ★ 与 `stageHostNow()` 是两件事（用户裁定 2026-11-25）：
//
//	工具登记与阶段管道此前塞在同一个类型里。
func (m *Module) toolRegNow() *kit.ToolRegistry {
	if m == nil || m.a == nil {
		return nil
	}
	return m.a.ToolRegistry()
}

// pluginRegNow 返回插件注册表读面（未挂载 ⇒ nil）。
func (m *Module) pluginRegNow() kernelapi.PluginHost {
	if m == nil || m.a == nil {
		return nil
	}
	return m.a.PluginHost()
}

// pluginDirNow 返回插件目录（未挂载 ⇒ ""）。
func (m *Module) pluginDirNow() string {
	if m == nil || m.a == nil {
		return ""
	}
	return m.a.PluginDirPath()
}
