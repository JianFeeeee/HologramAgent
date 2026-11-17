package agent

// access.go —— 基座**构造面**的访问器与实现体钩子（供 internal/agent/mainimpl）。
//
// ## 为什么允许这层 getter
//
// 实现体的方法原先是 `func (a *Agent)`。搬家后接收者变成 `*Main`（内嵌
// `*Agent`），而 Go 的嵌入只提升**方法**不提升**字段**（跨包不可见）。
// 所以需要一组只读访问器。它们暴露的是**基座构造面**（id/io/provider/sched…）。

import (
	"context"
	"sync"
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/agent/agent/child"
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	agentProvider "github.com/JianFeeeee/HologramAgent/internal/agent/provider"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// IDValue 返回本 agent 的标识。
func (a *Agent) IDValue() types.AgentID {
	if a == nil {
		return ""
	}
	return a.id
}

// Ctx 返回取消域。
func (a *Agent) Ctx() context.Context {
	if a == nil {
		return context.Background()
	}
	return a.ctx
}

// Cancel 返回取消函数。
func (a *Agent) Cancel() context.CancelFunc {
	if a == nil {
		return nil
	}
	return a.cancel
}

// IO 返回 IOManager。
func (a *Agent) IO() *agentIO.IOManager {
	if a == nil {
		return nil
	}
	return a.io
}

// Provider 返回默认模型源。
func (a *Agent) Provider() agentAPI.Provider {
	if a == nil {
		return nil
	}
	return a.provider
}

// ProviderManager 返回多源管理器。
func (a *Agent) ProviderManager() *agentProvider.ProviderManager {
	if a == nil {
		return nil
	}
	return a.providerManager
}

// PreferredProvider 返回优先模型源名。
func (a *Agent) PreferredProvider() string {
	if a == nil {
		return ""
	}
	return a.preferredProvider
}

// Graph 返回图记忆共同面（可 nil）。
func (a *Agent) Graph() GraphMemory {
	if a == nil {
		return nil
	}
	return a.graph
}

// SetGraph 运行期覆盖图记忆面（图记忆是可选配置）。
func (a *Agent) SetGraph(g GraphMemory) {
	if a != nil {
		a.graph = g
	}
}

// KernelSource 返回上级标识。
func (a *Agent) KernelSource() string {
	if a == nil {
		return ""
	}
	return a.kernelSource
}

// ParentOf 返回父 agent 的 id（根为空）。
func (a *Agent) ParentOf() string {
	if a == nil {
		return ""
	}
	return a.parentID
}

// TaskPrompt 返回任务提示词。
func (a *Agent) TaskPrompt() string {
	if a == nil {
		return ""
	}
	return a.taskPrompt
}

// DataDir 返回数据目录。
func (a *Agent) DataDir() string {
	if a == nil {
		return ""
	}
	return a.dataDir
}

// ContextOf 返回上下文实现。
func (a *Agent) ContextOf() *RelevanceContext {
	if a == nil {
		return nil
	}
	return a.context
}

// SetContextOf 由实现体注入自己的上下文实现。
func (a *Agent) SetContextOf(c *RelevanceContext) {
	if a != nil {
		a.context = c
	}
}

// SystemPrompt 返回固定提示词。
func (a *Agent) SystemPrompt() string {
	if a == nil {
		return ""
	}
	return a.systemPrompt
}

// AllowedOutputs 返回被授权的输出通道集合。
func (a *Agent) AllowedOutputs() []string {
	if a == nil {
		return nil
	}
	return a.allowedOutputs
}

// MaxToolTurns 返回工具轮次硬上限。
func (a *Agent) MaxToolTurns() int {
	if a == nil {
		return 0
	}
	return a.maxToolTurns
}

// UsageLedger 返回用量账本。
func (a *Agent) UsageLedger() *kit.Ledger {
	if a == nil {
		return nil
	}
	return &a.usageLedger
}

// Sched 返回调度器。
func (a *Agent) Sched() *scheduler {
	if a == nil {
		return nil
	}
	return a.sched
}

// Sub 返回类型分区表。
func (a *Agent) Sub() *subStates {
	if a == nil {
		return nil
	}
	return a.sub
}

// HooksValue 返回槽位表（改写走 SetHooks）。
func (a *Agent) HooksValue() Hooks {
	if a == nil {
		return Hooks{}
	}
	return a.hooks
}

// ChildrenOf 返回子 agent 登记表。
func (a *Agent) ChildrenOf() *child.Registry {
	if a == nil {
		return nil
	}
	return &a.children
}

// CancelLLM 返回进行中的 LLM 取消槽。
func (a *Agent) CancelLLM() *context.CancelFunc {
	if a == nil {
		return nil
	}
	return &a.cancelLLM
}

// LLMMu 返回 LLM 互斥锁。
func (a *Agent) LLMMu() *sync.Mutex {
	if a == nil {
		return nil
	}
	return &a.llmMu
}

// InterruptInput 返回"本轮被中断"标志槽。
func (a *Agent) InterruptInput() *bool {
	if a == nil {
		return nil
	}
	return &a.interruptInput
}

// LogIdentity 返回日志身份标签。
func (a *Agent) LogIdentity() child.LogIdentity {
	if a == nil {
		return child.LogIdentity{}
	}
	return a.logIdentity
}

// Periodic 返回类型注册的定时任务（刻度由内核事件循环推动）。
func (a *Agent) Periodic() []PeriodicTask {
	if a == nil {
		return nil
	}
	return a.periodic
}

// LastInputMu 返回最近输入时间表的锁。
func (a *Agent) LastInputMu() *sync.Mutex {
	if a == nil {
		return nil
	}
	return &a.lastInputMu
}

// LastInput 返回最近输入时间表（去重窗口用）。
func (a *Agent) LastInput() map[string]time.Time {
	if a == nil {
		return nil
	}
	return a.lastInput
}

// Impl 返回实现体（未挂时 nil）。
func (a *Agent) Impl() any {
	if a == nil {
		return nil
	}
	return a.impl
}

// SetImpl 挂上本类型的实现体（装配期一次）。
func (a *Agent) SetImpl(v any) {
	if a != nil {
		a.impl = v
	}
}
