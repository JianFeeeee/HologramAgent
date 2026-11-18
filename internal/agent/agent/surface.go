package agent

// surface.go —— 基座提供给**类型实现**的构造面访问器。
//
// ## 为什么这层存在（且为什么它不违反洋葱）
//
// 内核持有 `*Agent`；类型模块（mainimpl / resident / rtchild）通过注册的回调
// 拿到它。回调里的代码需要读基座的构造面（id / io / provider / ctx / 安全闸…），
// 但 Go 的可见性让跨包读不到小写字段。
//
// ⇒ 基座把这些**构造面**开成只读访问器。它们不是"把内部状态暴露出去"：
//   每一个都是 Agent 构造时确定、运行期不再变的事实；写入口仍然只有
//   `Install*` 系列（类型初始化期用一次）。
//
// 纪律：这里只放**读**。出现"实现体要写某个基座字段"时，说明那个字段该搬家。

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
func (a *Agent) IDValue() types.AgentID { return a.id }

// Ctx 返回取消域。
func (a *Agent) Ctx() context.Context {
	if a == nil {
		return context.Background()
	}
	return a.ctx
}

// Cancel 返回取消函数。
func (a *Agent) Cancel() context.CancelFunc { return a.cancel }

// IO 返回 IOManager。
func (a *Agent) IO() *agentIO.IOManager { return a.io }

// Provider 返回默认模型源。
func (a *Agent) Provider() agentAPI.Provider { return a.provider }

// ProviderManager 返回多源管理器。
func (a *Agent) ProviderManager() *agentProvider.ProviderManager { return a.providerManager }

// PreferredProvider 返回优先模型源名。
func (a *Agent) PreferredProvider() string { return a.preferredProvider }

// Graph 返回图记忆共同面（可 nil）。
func (a *Agent) Graph() GraphMemory { return a.graph }

// SetGraph 运行期覆盖图记忆面（可选槽位）。
func (a *Agent) SetGraph(g GraphMemory) {
	if a != nil {
		a.graph = g
	}
}

// KernelSource 返回上级标识。
func (a *Agent) KernelSource() string { return a.kernelSource }

// ParentOf 返回父 agent 的 id（根为空）。
func (a *Agent) ParentOf() string { return a.parentID }

// TaskPrompt 返回任务提示词。
func (a *Agent) TaskPrompt() string { return a.taskPrompt }

// DataDir 返回数据目录。
func (a *Agent) DataDir() string { return a.dataDir }

// ContextOf 返回上下文实现（类型装的；未装时 nil）。
func (a *Agent) ContextOf() *RelevanceContext { return a.context }

// SystemPrompt 返回固定提示词。
func (a *Agent) SystemPrompt() string { return a.systemPrompt }

// AllowedOutputs 返回被授权的输出通道集合。
func (a *Agent) AllowedOutputs() []string { return a.allowedOutputs }

// MaxToolTurns 返回工具轮次硬上限。
func (a *Agent) MaxToolTurns() int { return a.maxToolTurns }

// UsageLedger 返回用量账本。
func (a *Agent) UsageLedger() *kit.Ledger { return &a.usageLedger }

// Sched 返回调度器（未安装时 nil）。
func (a *Agent) Sched() *scheduler { return a.sched }

// Sub 返回类型分区表（未安装时 nil）。
func (a *Agent) Sub() *subStates { return a.sub }

// HooksValue 返回当前槽位表（改写走 SetHooks）。
func (a *Agent) HooksValue() Hooks { return a.hooks }

// ChildrenOf 返回子 agent 登记表。
func (a *Agent) ChildrenOf() *child.Registry { return &a.children }

// CancelLLM 返回进行中的 LLM 取消槽。
func (a *Agent) CancelLLM() *context.CancelFunc { return &a.cancelLLM }

// LLMMu 返回 LLM 互斥锁。
func (a *Agent) LLMMu() *sync.Mutex { return &a.llmMu }

// InterruptInput 返回"本轮被中断"标志槽。
func (a *Agent) InterruptInput() *bool { return &a.interruptInput }

// LogIdentity 返回日志身份标签。
func (a *Agent) LogIdentity() child.LogIdentity { return a.logIdentity }

// Periodic 返回类型注册的定时任务（刻度由内核事件循环推动）。
func (a *Agent) Periodic() []PeriodicTask { return a.periodic }

// LastInputMu 返回最近输入时间表的锁。
func (a *Agent) LastInputMu() *sync.Mutex { return &a.lastInputMu }

// LastInput 返回最近输入时间表（去重窗口用）。
func (a *Agent) LastInput() map[string]time.Time { return a.lastInput }

// ToolMu 返回工具批的互斥锁。
func (a *Agent) ToolMu() *sync.Mutex { return &a.toolMu }

// ToolSeq 返回工具调用序号计数器。
func (a *Agent) ToolSeq() *uint64 { return &a.toolSeq }

// CancelTools 返回"取消当前工具批"的取消函数表。
func (a *Agent) CancelTools() *map[uint64]context.CancelFunc { return &a.cancelTools }

// Impl 返回类型实现体（未挂时 nil）。
func (a *Agent) Impl() any { return a.impl }

// SetImpl 挂上类型实现体（装配期一次）。
func (a *Agent) SetImpl(v any) {
	if a != nil {
		a.impl = v
	}
}

// AgentHooks 返回**载体 agent 的槽位表**（子 agent 类型用）。
//
// ★ 为什么需要它：判据与诊断要能看见"这个子 agent 注册了哪些槽位"，
//
//	而槽位表在载体（*Agent）上，不在 Entry 上。Entry 暴露 Agent() any，
//	外部拿到后仍需一个稳定的读取入口 —— 就是本方法。
func (rc *residentChild) AgentHooks() Hooks { return rc.agent.HooksValue() }

// AgentForTest 返回载体（判据用；生产路径不经过它）。
func (rc *residentChild) AgentForTest() *Agent { return rc.agent }

// AgentHooks 返回 rt 载体 agent 的槽位表。
func (rc *rtChild) AgentHooks() Hooks { return rc.agent.HooksValue() }

// AgentForTest 返回 rt 载体（判据用）。
func (rc *rtChild) AgentForTest() *Agent { return rc.agent }
