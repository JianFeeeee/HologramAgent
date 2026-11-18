package agent

import (
	"encoding/json"
	"fmt"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/meta"
	"github.com/JianFeeeee/HologramAgent/internal/nlp"
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
type MemoryStatus = sdk.MemoryStatus
type KnowledgeStatus = sdk.KnowledgeStatus
type DocumentStatus = sdk.DocumentStatus
type TextMemoryStatus = sdk.TextMemoryStatus
type SocialStatus = sdk.SocialStatus
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
	stageHost *kit.StageHostType,
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
	if stageHost != nil {
		// ★ 只给**摘要**，不给完整定义（2026-10-26 实测）。
		//
		// 原先直接塞 GetToolDefs()：173 个工具的完整 Description + Parameters
		// JSON ⇒ healthcheck_kernel 单条结果 **98,989 tokens**（预算 65,375 的 151%），
		// 一轮对话里它就把上下文塞满，还拖慢每一次后续往返。
		//
		// 摘要保留"有哪些工具、属于哪个插件、能干什么"，足够状态页与模型判断；
		// 需要完整参数表的场景走 get_plugin_tools(<plugin>)，那是按需的。
		defs := stageHost.GetToolDefs()
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

// registerPendingStatusSections 注册**句柄由 mainState 持有**的那六个段。
//
// ★ 为什么它们暂时住基座：`mainState` 还在基座包里（I 批才搬进 mainagent），
// 而"谁持有数据谁注册"要求注册点能读到持有者。搬注册点而不搬 mainState，
// 等于从别的包读基座私有字段 —— 那正是本仓不许的口子。
//
// I 批之后：本函数随 mainState 整体搬进 `mainagent`，基座里一行不剩。
func (a *Agent) registerPendingStatusSections() {
	m := a.MainState()
	if m == nil {
		return
	}
	if m.indexer != nil || a.GraphDB() != nil {
		a.RegisterStatusSection(StatusSection{Key: "memory", Payload: func() any {
			return memorySectionOf(a.GraphDB())
		}})
	}
	if m.coldcontextStore != nil {
		a.RegisterStatusSection(StatusSection{Key: "documents", Payload: func() any {
			return documentsSectionOf(m.coldcontextStore)
		}})
	}
	if m.textMem != nil {
		a.RegisterStatusSection(StatusSection{Key: "text_memory", Payload: func() any {
			return sdk.TextMemoryStatus{Available: true, FileCount: m.textMem.FileCount()}
		}})
	}
	if m.social != nil {
		a.RegisterStatusSection(StatusSection{Key: "social", Payload: func() any {
			st := sdk.SocialStatus{Available: true}
			if persons, err := m.social.ListPersons(); err == nil {
				st.PersonCount = len(persons)
			}
			return st
		}})
	}
	if m.tracker != nil {
		a.RegisterStatusSection(StatusSection{Key: "tracker", Payload: func() any {
			return sdk.TrackerStatus{Available: true, Dir: m.tracker.MergeDir()}
		}})
	}
	if m.multimodalSpace != nil || m.embeddingProvider != "" || m.embeddingError != "" {
		a.RegisterStatusSection(StatusSection{Key: "onnx", Payload: func() any {
			return a.onnxStatus()
		}})
	}
}

// memorySectionOf 把图记忆的 Introspect 结果转成状态段。
func memorySectionOf(db kernelapi.GraphMemory) sdk.MemoryStatus {
	if db == nil {
		return sdk.MemoryStatus{}
	}
	st := sdk.MemoryStatus{Available: true}
	if info, err := db.Introspect(); err == nil {
		if v, ok := info["entity_count"].(int); ok {
			st.EntityCount = v
		}
		if v, ok := info["relation_count"].(int); ok {
			st.RelationCount = v
		}
		if v, ok := info["entity_type_count"].(int); ok {
			st.EntityTypes = v
		}
	}
	return st
}

// documentsSectionOf 把冷文档归档的统计转成状态段。
func documentsSectionOf(store kernelapi.DocStore) sdk.DocumentStatus {
	if store == nil {
		return sdk.DocumentStatus{}
	}
	st := sdk.DocumentStatus{Available: true}
	stats := store.Stats()
	if v, ok := stats["doc_count"].(int); ok {
		st.DocCount = v
	}
	if v, ok := stats["vector_count"].(int); ok {
		st.VectorCount = v
	}
	return st
}

// GetKernelStatus 返回 Agent 驱动的内核状态快照。
func (a *Agent) GetKernelStatus() *KernelStatus {
	providerName := ""
	sourceCount := 0
	if a.providerManager != nil {
		sourceCount = len(a.providerManager.List())
	}
	if a.provider != nil {
		providerName = a.provider.Name()
	}

	var startTime time.Time
	stageHost := (*kit.StageHostType)(nil)
	pluginReg := kernelapi.PluginHost(nil)
	if m := a.MainState(); m != nil {
		startTime = m.startTime
		stageHost = m.stageHost
		pluginReg = m.pluginReg
	}

	ks := collectKernelStatus(
		startTime, string(a.id), providerName, sourceCount,
		stageHost, a.io, pluginReg,
	)
	ks.Usage = a.usageStatus()

	// ★ 基座注册的两个段（与任何 agent 类型无关）：
	//
	//	"runtime"  Go runtime 统计 —— 主 agent 挂了它照样存在
	//	"llm"      Provider* 是 AgentConfig 公有字段 —— 每个类型都有
	a.RegisterStatusSection(StatusSection{Key: "runtime", Payload: NewRuntimeSection()})
	a.RegisterStatusSection(StatusSection{Key: "llm", Payload: NewLLMSection(providerName, sourceCount)})

	// ★ 句柄在 mainState 的六个段（I 批随 mainState 搬进 mainagent）。
	a.registerPendingStatusSections()

	// ★ 类型注册的段：谁持有数据谁注册（主 agent 在它自己的 Initialize 里注册）。
	ks.Sections = a.CollectStatusSections()
	// ★ 插件注册的段（知识库条目数、索引状态…）：插件不是 agent，
	//   走 sdk 的全局段注册表，在这里合进同一份快照。
	for k, v := range sdk.CollectPluginStatusSections() {
		if ks.Sections == nil {
			ks.Sections = map[string]json.RawMessage{}
		}
		if _, dup := ks.Sections[k]; !dup {
			ks.Sections[k] = v
		}
	}
	return ks
}

// usageStatus 把 agent 的累计用量账目转成对外状态视图。
//
// 数据源就是 kit.Ledger（每次 LLM 调用后记账的那份），所以这里**不重算**：
// 重算等于给同一个事实造第二个来源，两边迟早会不一致。
func (a *Agent) usageStatus() sdk.UsageStatus {
	snap := a.usageLedger.Snapshot()
	out := sdk.UsageStatus{
		Calls:              snap.Calls,
		Prompt:             snap.Prompt,
		Completion:         snap.Completion,
		Total:              snap.Total,
		CacheRead:          snap.CacheRead,
		CacheMiss:          snap.CacheMiss,
		Reasoning:          snap.Reasoning,
		CacheReportedCalls: snap.CacheReportedCalls,
	}
	// ok=false（没有任何调用报过缓存）时**不带**这个字段，
	// 让消费方显示「—」而不是把「不知道」画成 0%。
	if rate, ok := snap.CacheHitRate(); ok {
		out.CacheHitRate = &rate
	}
	return out
}

// onnxStatus 汇总统一多模态向量空间（ONNX 模型）的启用状态。
//
// 判据是 Loaded()（provider 真正打开且元数据合法），**不是**「配置里写了 provider」——
// 后者在模型缺失 / 运行时缺失时也为真，拿它当判据就是假绿。
func (a *Agent) onnxStatus() sdk.ONNXStatus {
	st := sdk.ONNXStatus{Provider: a.MainState().embeddingProvider, DepParser: depParserStatus()}
	if a.MainState().multimodalSpace != nil && a.MainState().multimodalSpace.Loaded() {
		st.Enabled = true
		st.Dim = a.MainState().multimodalSpace.Dim()
		st.Fingerprint = a.MainState().multimodalSpace.Fingerprint()
		// 模态是可选能力：只有底层 provider 报出来时才带出。
		if mr, ok := a.MainState().multimodalSpace.(interface{ Modalities() []string }); ok {
			st.Modalities = mr.Modalities()
		}
		return st
	}
	switch {
	case a.MainState().embeddingError != "":
		st.Reason = "打开失败: " + a.MainState().embeddingError
	case a.MainState().embeddingProvider == "":
		st.Reason = "未配置统一向量空间 provider（走词嵌入/TF-IDF 回退路径）"
	default:
		st.Reason = "provider 未加载"
	}
	return st
}

// depParserStatus 汇总依存句法分析器的**实际**状态。
//
// 判据是「包级默认位上有解析器且能报出模型路径」，不是「配了 onnx_model_path」：
// 配置留空时用的是二进制内嵌模型（解到 <data>/nlp 后加载），只看配置会漏报。
func depParserStatus() sdk.DepParserStatus {
	p := nlp.GetDefaultParser()
	if p == nil {
		return sdk.DepParserStatus{}
	}
	st := sdk.DepParserStatus{Enabled: true}
	if mp, ok := p.(interface{ ModelPath() string }); ok {
		st.ModelPath = mp.ModelPath()
		if strings.Contains(filepath.ToSlash(st.ModelPath), "/nlp/dep_parser.onnx") {
			// 内嵌模型的解出位置固定为 <dataDir>/nlp/dep_parser.onnx；
			// 无法从路径反推来源时保留 false（未知不冒充内嵌）。
			st.Embedded = true
		}
	}
	return st
}

var _ StatusProvider = (*Agent)(nil)
var _ sdk.StatusAPI = (*Agent)(nil)

// residentStatuses 把内核的驻留子视图映射成 SDK DTO。
//
// 刻意**不带** ResidentInfo.Table（那是每个驻留子的 inputch 登记明细）：状态面
// 是给所有应用轮询的，把它塞进来会让每次 /status 都背上几十 KB。
// 只给表的大小（InputChTable），要看明细走专门的接口。
func residentStatuses(list []ResidentInfo) []ResidentStatus {
	out := make([]ResidentStatus, 0, len(list))
	for _, r := range list {
		st := ResidentStatus{
			ID:             r.ID,
			State:          r.State,
			Rounds:         r.Rounds,
			ContextFull:    r.ContextFull,
			InputChs:       r.InputChs,
			AllowedOutputs: r.AllowedOutputs,
			InputChTable:   r.TableSize,
			OffloadOwned:   r.OffloadOwned,
			// 子自己的调度器积压：per-agent 负载图靠这四项，缺了就只能画根。
			ReadyQueueDepth:   r.SchedReady,
			PendingInterrupts: r.SchedPending,
			SuspendStack:      r.SchedStack,
			InterruptQueues:   r.SchedQueues,
		}
		if !r.CreatedAt.IsZero() {
			st.CreatedAt = r.CreatedAt.Format(time.RFC3339)
		}
		out = append(out, st)
	}
	return out
}
