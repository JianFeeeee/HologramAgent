package mainagent

// state.go —— 主 agent 的**类型私有装配**。
//
// ## 为什么这些在这里而不在基座（用户裁定 2026-11-19）
//
// 它们不是内核设施，而是**主 agent 拿它做记忆整理的那一份装配**：
//
//	记忆整理面   indexer / tracker / coldcontextStore / social / textMem
//	嵌入与多模态 embedder / multimodalSpace / embeddingProvider
//	插件面       pluginReg / pluginDir / stageHost / pluginHealth
//	媒体与人格   personality / personaStore / pendingMedia* / noMergeMarkers
//	预算记账     toolResultWarnTokens / toolResultReporter
//	运行期       startTime / thinkingEnabled
//
// 驻留子与 rt 是轻量内核（记忆面只有 Recall/Commit），这些字段它们一条都用不到。
// 放基座等于让每个 agent 实例挂着一整套用不到的实现句柄。

import (
	"sync"
	"time"

	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi/model"
)

type state struct {
	indexer          kernelapi.MemoryIndexer
	tracker          kernelapi.Tracker
	coldcontextStore kernelapi.DocStore
	social           kernelapi.SocialStore
	textMem          kernelapi.TextMemory

	// injected 标记句柄是**外部显式注入**的（判据/探针），不是自己装配的。
	// installState 据此跳过 `services.Install` —— 否则会覆盖注入的那一份。
	injected bool

	// sem 是**基座句柄**（语义计算 syscall 的宿主）。
	//
	// ★ 为什么不直接持 *StaticEmbedder / MultimodalSpace：
	// 那是内核对象（docs §1 结论 5/12）。主 agent 只经基座方法要结果：
	// 「这 N 句的向量」「这两个向量的相似度」。持有实现对象等于让类型
	// 认识内核设施的形态，换 provider 就要回头改类型。
	sem               *agent.Agent
	embeddingProvider string
	embeddingError    string
	fusionCfg         CrossModalFusionConfig

	pluginReg    kernelapi.PluginHost
	pluginDir    string
	stageHost    *kit.StageHostType
	pluginHealth *kit.PluginHealthTracker

	personality        *agentPkg.Personality
	personaStore       PersonaStore
	pendingMedia       map[string]interface{}
	pendingMediaBlocks []model.MemoryBlock
	pendingToolBlocks  []interface{}
	noMergeMarkers     map[string]int
	noMergeMu          sync.Mutex

	toolResultWarnTokens int
	toolResultReporter   toolResultReporter

	startTime time.Time

	knowledge any
}
