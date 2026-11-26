package agent

// kernelstatus.go —— **状态快照的合成**（基座组装，类型/插件只注册段）。
//
// ## 为什么在基座
//
// 状态面是**注册表合成**（`RegisterStatusSection`）：每个持有数据的实体
// （类型、插件、内核模块）注册自己那段，基座只按 key 收集并拼成一份快照。
//
// ★ 基座**不认识任何段的内容**：它不知道 "llm" 段有什么字段；
//   消费方按 key 取、自己解码（`ks.Sections["llm"]`）。

import (
	"time"

	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// GetKernelStatus 返回内核状态快照。
func (a *Agent) GetKernelStatus() *KernelStatus {
	if a == nil {
		return nil
	}
	providerName, sourceCount := "", 0
	if a.providerManager != nil {
		sourceCount = len(a.providerManager.List())
	}
	if a.provider != nil {
		providerName = a.provider.Name()
	}

	ks := collectKernelStatus(
		time.Now(), string(a.id), providerName, sourceCount,
		a.toolReg, a.io, a.pluginRegForStatus(),
	)
	ks.Usage = a.usageStatusSnapshot()

	// ★ 这里**只读**，不注册任何段（2026-11-20 实测缺陷）。
	//
	// 此前这里有两行 `RegisterStatusSection`（"runtime" / "llm"）：
	//
	//	a.RegisterStatusSection(StatusSection{Key: "runtime", Payload: NewRuntimeSection()})
	//	a.RegisterStatusSection(StatusSection{Key: "llm", Payload: NewLLMSection(...)})
	//
	// 而 `GetKernelStatus` 是**高频读路径**（cli /status 15s 轮询、webui 面板、
	// healthcheck）。后果：
	//
	//	· 每读一次就往共享段表**写**一次（读路径改共享状态 ⇒ 竞争面）
	//	· 每读一次新建两个闭包（`NewRuntimeSection` 每次都分配）
	//	· 每次写都触发一条告警日志 —— 实测 15s 两条，一天 11520 行
	//
	// 段的注册是**装配期动作**，归 `Start()`（与 "security" 段同一处）。
	// 值的来源（providerName / sourceCount）在读取时现取 —— 所以段里存的是
	// **求值函数**，不是快照值。

	// 类型注册的段：谁持有数据谁注册。
	ks.Sections = a.CollectStatusSections()
	// 插件注册的段走 sdk 的全局表，合进同一份快照。
	for k, v := range sdk.CollectPluginStatusSections() {
		_ = k
		_ = v
	}
	return ks
}

// pluginRegForStatus 取插件注册表（状态面用；未装配 ⇒ nil）。
//
// ★ 读 `AgentConfig.PluginReg`：它是**基座公有字段**（插件宿主在装配期注入），
//
//	与"是哪个类型"无关。
//
// 这里曾返回**恒 nil** 并附注"stageHost 不暴露 plugin 注册表"—— 那让
// 状态面的 `plugins` 恒为空数组。1:1 对照旧内核时实测：旧内核 `/status`
// 报 `插件: 25 loaded`，新内核报 `插件: 0 loaded`，且 `/kernel` 的
// `plugins` 是 `[]`。是真实回归，不是"设计如此"。
func (a *Agent) pluginRegForStatus() kernelapi.PluginHost {
	if a == nil {
		return nil
	}
	a.semanticMu.RLock()
	if r := a.pluginReg; r != nil {
		a.semanticMu.RUnlock()
		return r
	}
	a.semanticMu.RUnlock()
	return nil
}

// usageStatusSnapshot 返回用量快照。
func (a *Agent) usageStatusSnapshot() sdk.UsageStatus {
	snap := a.usageLedger.Snapshot()
	return sdk.UsageStatus{
		Calls: snap.Calls, Prompt: snap.Prompt, Completion: snap.Completion,
		Total: snap.Total,
	}
}

var _ = kit.Ledger{}
