package agent

// composition.go —— 类型模块**组合基座**时的句柄读入口。
//
// ## 形态（用户裁定 2026-11-17 / 2026-11-19）
//
// 类型 `type M struct{ A *agent.Agent }`，需要基座什么就直接调 `m.A.Xxx()`。
// 这里只有**字段读入口**：没有 `Host` 接口、没有 `a.X → h.X` 的映射层。
//
// ## 什么该在这里
//
// 只有**基座/内核设施**：IO、LLM 提供者、图记忆装配面、数据目录、
// 子 agent 骨架、错误分类。四类 agent 都要用，不属于任何类型私有。
//
// 与它们相对：主 agent 的**私有装配**（记忆整理面 / 媒体 / 插件 / 人格 /
// 预算记账）不是设施，整体归 `mainagent`，基座一格里都不留。
//
// ★ 这里**没有** `Scheduler()`：调度器是 agentloop 的内部实现，
//   基座只负责把输入交给 agent 实例的输入口（io 单通道 + priority 标签）。

import (
	"context"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	agentProvider "github.com/JianFeeeee/HologramAgent/internal/agent/provider"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// IO 返回本 agent 的 IO 管理器（输入入口 / 输出设备 / 通道登记表）。
func (a *Agent) IO() *agentIO.IOManager {
	if a == nil {
		return nil
	}
	return a.io
}

// Provider 返回本 agent 的 LLM 提供者。
func (a *Agent) Provider() agentAPI.Provider {
	if a == nil {
		return nil
	}
	return a.provider
}

// SetProvider 替换本 agent 的 LLM 提供者。
func (a *Agent) SetProvider(p agentAPI.Provider) {
	if a == nil {
		return
	}
	a.provider = p
}

// ProviderManager 返回提供者管理器（多源 / 分类别 / 重试）。
func (a *Agent) ProviderManager() *agentProvider.ProviderManager {
	if a == nil {
		return nil
	}
	return a.providerManager
}

// PreferredProvider 返回本 agent 优先使用的源名（空 = 继承父 / 默认）。
func (a *Agent) PreferredProvider() string {
	if a == nil {
		return ""
	}
	return a.preferredProvider
}

// SystemPrompt 返回系统提示词**原始素材**（子继承父的那份）。
//
// ★ 这是素材，不是渲染结果：渲染在类型自己的包里做，渲染好的**完整上下文**
//
//	才交回基座。
func (a *Agent) SystemPrompt() string {
	if a == nil {
		return ""
	}
	return a.systemPrompt
}

// GraphOpener 返回图记忆装配面（类型自己开记忆实例时用）。
func (a *Agent) GraphOpener() kernelapi.GraphMemoryOpener {
	if a == nil {
		return nil
	}
	return a.graphOpener
}

// CloseGraph 关闭一个图记忆句柄（经装配面；基座不认识实现类型）。
func (a *Agent) CloseGraph(m any) error {
	if a == nil {
		return nil
	}
	return a.closeGraph(m)
}

// StreamLLM 暴露流式补全。类型的 agentloop **阻塞地**等自己这一轮。
func (a *Agent) StreamLLM(ctx context.Context, req *agentAPI.CompletionRequest) (<-chan agentAPI.StreamChunk, error) {
	return a.streamLLM(ctx, req)
}

// CompleteLLM 暴露非流式补全（同样：类型阻塞等自己的结果）。
func (a *Agent) CompleteLLM(ctx context.Context, req *agentAPI.CompletionRequest, channel string) (*agentAPI.CompletionResponse, error) {
	return a.completeLLM(ctx, req, channel)
}

// IOBusDevice 返回注册在**本 agent io** 上的输出设备面（按通道名取）。
func (a *Agent) IOBusDevice(channel string) (agentIO.Device, bool) {
	if a == nil || a.io == nil {
		return nil, false
	}
	d := a.io.GetDevice(channel)
	return d, d != nil
}
