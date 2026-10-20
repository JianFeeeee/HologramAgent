package core

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/document"
	"github.com/JianFeeeee/HomeAgent/internal/memory/social"
	"github.com/JianFeeeee/HomeAgent/internal/memory/text"
	"github.com/JianFeeeee/HomeAgent/internal/meta"
	"github.com/JianFeeeee/HomeAgent/internal/nlp"
	"github.com/JianFeeeee/HomeAgent/internal/plugin"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	"github.com/JianFeeeee/HomeAgent/internal/tracker"
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
type ContextStatus = sdk.ContextStatus
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

// collectKernelStatus 聚合内核各子系统状态快照。
// 接收所有子系统引用（均为可选——nil 表示不可用），返回统一的状态报告。
func collectKernelStatus(
	startTime time.Time,
	agentID string,
	providerName string,
	sourceCount int,
	stageHost *StageHost,
	iom *agentIO.IOManager,
	pluginReg *plugin.Registry,
	memDB *memory.GraphDB,
	ks interface{ List() []string },
	docStore *document.Store,
	textMem *text.Memory,
	socialStore *social.SocialStore,
	trk *tracker.Tracker,
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
		Runtime: RuntimeStatus{
			Goroutines: runtime.NumGoroutine(),
			GoVersion:  runtime.Version(),
		},
		LLM: LLMStatus{
			Available: providerName != "",
			Provider:  providerName,
			Sources:   sourceCount,
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
		status.Tools = stageHost.GetToolDefs()
	}

	// Channels
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

	// Graph memory
	if memDB != nil {
		status.Memory.Available = true
		if info, err := memDB.Introspect(); err == nil {
			if ec, ok := info["entity_count"].(int); ok {
				status.Memory.EntityCount = ec
			}
			if rc, ok := info["relation_count"].(int); ok {
				status.Memory.RelationCount = rc
			}
			if et, ok := info["entity_type_count"].(int); ok {
				status.Memory.EntityTypes = et
			}
		}
	}

	// Knowledge
	if ks != nil {
		status.Knowledge.Available = true
		status.Knowledge.Items = ks.List()
		status.Knowledge.ItemCount = len(status.Knowledge.Items)
	}

	// Documents
	if docStore != nil {
		status.Documents.Available = true
		stats := docStore.Stats()
		if dc, ok := stats["doc_count"].(int); ok {
			status.Documents.DocCount = dc
		}
		if vc, ok := stats["vector_count"].(int); ok {
			status.Documents.VectorCount = vc
		}
	}

	// Text memory
	if textMem != nil {
		status.TextMemory.Available = true
		status.TextMemory.FileCount = textMem.FileCount()
	}

	// Social
	if socialStore != nil {
		status.Social.Available = true
		if persons, err := socialStore.ListPersons(); err == nil {
			status.Social.PersonCount = len(persons)
		}
	}

	// Tracker
	if trk != nil {
		status.Tracker.Available = true
		status.Tracker.Dir = trk.MergeDir()
	}

	// Memory
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	status.Runtime.MemoryMB = int64(m.Alloc / 1024 / 1024)

	return status
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

	var textMem *text.Memory
	if a.textMem != nil {
		textMem = a.textMem
	}

	var socialStore *social.SocialStore
	if a.social != nil {
		socialStore = a.social
	}

	var trk *tracker.Tracker
	if a.tracker != nil {
		trk = a.tracker
	}

	// 注意：knowledge 在 collectKernelStatus 里是**接口**参数，
	// 而 (*knowledge.Store)(nil) 塞进接口后 `ks != nil` 仍为真 → 调 List() 直接 panic。
	// 所以这里必须先判具体指针再进行接口赋值（healthcheck_kernel 会走到这条路径）。
	var knowledgeLister interface{ List() []string }
	if a.knowledge != nil {
		knowledgeLister = a.knowledge
	}

	ks := collectKernelStatus(
		a.startTime,
		string(a.id),
		providerName,
		sourceCount,
		a.stageHost,
		a.io,
		a.pluginReg,
		a.memory,
		knowledgeLister,
		a.docStore,
		textMem,
		socialStore,
		trk,
	)
	ks.ONNX = a.onnxStatus()
	ks.Scheduler = a.schedulerStatus()
	ks.Residents = residentStatuses(a.Residents())
	ks.Usage = a.usageStatus()

	return ks
}

// usageStatus 把 agent 的累计用量账目转成对外状态视图。
//
// 数据源就是 usageLedger（每次 LLM 调用后记账的那份），所以这里**不重算**：
// 重算等于给同一个事实造第二个来源，两边迟早会不一致。
func (a *Agent) usageStatus() sdk.UsageStatus {
	snap := a.usageLedger.snapshot()
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
	st := sdk.ONNXStatus{Provider: a.embeddingProvider, DepParser: depParserStatus()}
	if a.multimodalSpace != nil && a.multimodalSpace.Loaded() {
		st.Enabled = true
		st.Dim = a.multimodalSpace.Dim()
		st.Fingerprint = a.multimodalSpace.Fingerprint()
		// 模态是可选能力：只有底层 provider 报出来时才带出。
		if mr, ok := a.multimodalSpace.(interface{ Modalities() []string }); ok {
			st.Modalities = mr.Modalities()
		}
		return st
	}
	switch {
	case a.embeddingError != "":
		st.Reason = "打开失败: " + a.embeddingError
	case a.embeddingProvider == "":
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
