// Package agent 是 agent 模块：主 Agent（需在插件加载前创建，
// 因为插件 Configure 需要 StatusProvider）。
//
// ## 依赖
//
// buildAgent 收 provider/providerMgr/iom/mem.db/mem.indexer/trk/coldcontextStore/
// knowledge/mem.social/textMem/personality/pluginReg/embedder/
// multimodalSpace/stageHost/evBus ⇒ 依赖除 supervisor 外的几乎全部下层模块。
//
// 人格设定（loadPersonality）在本模块内完成 —— 它只读配置文件，
// 没有独立产物的价值，不必单列模块。
package agent

import (
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	agentCore "github.com/JianFeeeee/HologramAgent/internal/agent/core"
	"github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/pkg/types"

	agentPkg "github.com/JianFeeeee/HologramAgent/internal/agent"
)

// Module 提供主 Agent。
type Module struct{}

func (Module) Name() string { return "agent" }
func (Module) Requires() []string {
	return []string{kernelmod.FacLLM, kernelmod.FacMemory, kernelmod.FacTracker,
		kernelmod.FacDocStore, kernelmod.FacKnowledge, kernelmod.FacTextMem,
		kernelmod.FacStageHost}
}
func (Module) Provides() []string { return []string{kernelmod.FacAgent} }

func (Module) Init(k *kernelboot.KernelState) error {
	if k.Memory == nil {
		return errMissing("agent", kernelmod.FacMemory)
	}
	if k.LLMManager == nil {
		return errMissing("agent", kernelmod.FacLLM)
	}
	if k.PluginRegistry == nil {
		return errMissing("agent", kernelmod.FacStageHost)
	}
	k.Agent = buildAgent(k)

	// 插件 SDK 接线（必须在 loadPlugins 之前）。
	wirePluginSDK(k)
	return nil
}

func init() { kernelmod.Register(Module{}) }

const defaultSystemPrompt = `You are HologramAgent, a continuously running personal assistant.

The system injects the available tools and installed skills on every turn:
- Each tool's description states when to use it, what its parameters mean, and known pitfalls. Read it before calling; never guess usage from the tool name.
- The skill index is only an index. Use skill_info to read a skill in full, and follow its documented steps for multi-step procedures.

Answer only from what the tools actually return. If something could not be retrieved, say so plainly. Never fill gaps by completing, guessing, or inventing.

Reply in natural language, like a person talking: do not enumerate which tools you called, their parameters, or timings unless the user explicitly asks. For how to address the user and what tone to use, see [Persona].`

func buildAgent(k *kernelboot.KernelState) *agentCore.Agent {
	cfg := k.Cfg
	cfgReg := k.CfgReg
	provider := k.LLMProvider
	providerMgr := k.LLMManager
	iom := k.IO
	memDB := k.Memory.DB
	memIdx := k.Memory.Indexer
	trk := k.Tracker
	coldcontextStore := k.DocStore
	ks := k.Knowledge
	socialStore := k.Memory.Social
	textMem := k.TextMemory
	pluginReg := k.PluginRegistry
	embedder := k.Embedder
	multimodalSpace := k.MultimodalSpace
	mmProviderName := k.MultimodalProviderName
	mmErr := k.MultimodalErr
	stageHost := k.StageHost
	evBus := k.EventBus

	personality := loadPersonality(cfg, cfgReg)

	sysPrompt := cfgReg.GetString("core.agent.system_prompt", defaultSystemPrompt)
	if sysPrompt == "" {
		sysPrompt = defaultSystemPrompt
	}

	// D4：把"设备是否已授权"的判据注入插件侧（toolImpl.CanUse 用）。
	// ⚠️ 必须放在 agent 构造**之后**——判据要用 agent 的 allowedOutputs，
	// 而 registry 早于 agent 构造（故这里传的是晚绑定闭包）。
	// 未注入时 ToolAPI 路径对设备放行：那是"授权可被绕过"的既成缺口。
	// 注入后，cli 的 /terminal、seq 序列等一切走 ToolAPI 的调用都受同一道闸。
	agent := agentCore.New(agentCore.AgentConfig{
		ID:              "main",
		SystemPrompt:    sysPrompt,
		Provider:        provider,
		ProviderManager: providerMgr,
		IO:              iom,
		Memory:          memDB,
		Indexer:         memIdx,
		Tracker:         trk,
		DocStore:        coldcontextStore,
		Knowledge:       ks,
		SocialStore:     socialStore,
		TextMemory:      textMem,
		// MediaStore 已随 CAS 淘汰：块自带负载（见 plan.md）。
		Personality: personality,
		// 人格落库面：首启门禁（任何通道都问一次）与 persona_set 工具用。
		// 与 WebUI 向导共用 internal/config 的同一份落库逻辑。
		PersonaStore: config.RegistryPersonaStore{Reg: cfgReg},
		PluginReg:    pluginReg,
		PluginDir:    cfg.Plugin.Dir,
		// DataDir：驻留子的 temp 图库锚点（<data>/residents/<id>/graph.db）。
		// 漏接时的现象是"工具存在、可调用、但创建必失败"——只有真实二进制才看得出来。
		DataDir:         cfg.Daemon.DataDir,
		DistillInterval: cfgReg.GetDuration("core.agent.distill_interval", 30*time.Minute),
		ArchiveInterval: cfgReg.GetDuration("core.agent.archive_interval", 60*time.Minute),
		ReviewInterval:  cfgReg.GetDuration("core.agent.review_interval", 120*time.Minute),
		MergeInterval:   cfgReg.GetDuration("core.agent.merge_interval", 120*time.Minute),
		MaxToolTurns:    cfgReg.GetInt("core.agent.max_tool_turns", 10),
		// 上下文管理的可调阈值。此前这些值全部硬编码在 ComputeTokenBudget 里，
		// 界面改不了、不同窗口的实例也没法各自调优。
		CtxTuning: agentCore.ContextTuning{
			UtilizationPercent: cfgReg.GetInt("core.agent.context.utilization_percent", 80),
			MaxTargetTokens:    cfgReg.GetInt("core.agent.context.max_target_tokens", 600000),
			MemoryRatioPercent: cfgReg.GetInt("core.agent.context.memory_ratio_percent", 0),
			ProtectedCount:     cfgReg.GetInt("core.agent.context.protected_count", 10),
		},
		// ⚠️ 这条键此前被注册进配置面板、也有默认值，但**从未被读取** ——
		// 界面上改它没有任何效果，且不报任何错。同一类「死配置」正是
		// 本次把阈值接入配置系统时要一并修掉的。
		MaxContextSize: cfgReg.GetInt("core.agent.max_context_size", 30),
		// 默认值**不在这里写**：ConfigRegistry 的 GetXxx 会从已注册的
		// ConfigDef.Default 取（见 registry.go 的 defDefault）。这里传的
		// 字面量只是「连 ConfigDef 都没注册」时的兜底，正常路径不会用到。
		//
		// 2026-10-18 的教训：把 busy_after 从 5m 改成 1m 时，同一个默认值
		// 散落在 seedDBValues/sedCoreDefs/读取点 三处，得手工对齐一遍，
		// 漏一处就是静默的行为漂移。现在只剩 ConfigDef 一处。
		Offload: agentCore.OffloadOptions{
			Enabled:      cfgReg.GetBool("core.agent.offload_enabled", false),
			BusyAfter:    cfgReg.GetDuration("core.agent.offload_busy_after", 0),
			MinPending:   cfgReg.GetInt("core.agent.offload_min_pending", 0),
			MaxResidents: cfgReg.GetInt("core.agent.offload_max_residents", 0),
		},
		ContextSavePath:    filepath.Join(cfg.Daemon.DataDir, "memory", "context.json"),
		EmbeddingModelPath: cfgReg.GetString("core.agent.embedding_model_path", ""),
		Embedder:           embedder,
		MultimodalSpace:    multimodalSpace,
		EmbeddingProvider:  mmProviderName,
		EmbeddingError:     mmErr,
		StageHost:          stageHost,
		EventBus:           evBus,
		ThinkingEnabled:    cfg.LLM.ThinkingEnabled,
		InputProcessing:    cfg.InputProcessing,
	})

	// 图记忆读写钩子 → 事件总线。
	//
	// ★ 为什么在这里接（而不是让各调用方自己发事件）：
	//   图读写散在 26 个方法与 180+ 个 db 调用点上，各调用方自报必漏发；
	//   而**图库自己就知道它碰了哪些块 ID**。星图此前只能靠工具名猜
	//   节点（把 "stablemem_recall" 拆词去匹配中文实体名，实测命中 0），
	//   于是每次都点亮同一批无关节点——这正是「星图没有读写反馈」的真身。
	//
	// ★ 只在 evBus 非 nil 时接：它把「内核进程」与「独立 CLI 工具」区分开
	//   （homed-graph-migrate / memgc 等也用 NewGraphDB，但没有事件总线，
	//   也不该为一次批量迁移向不存在的订阅者发几万条事件）。
	//
	// ★ 钩子**必须非阻塞**：它在记忆读写的同步路径上，发布到总线
	//   是内存操作（各订阅者回调也都很短），实测开销可忽略。
	if memDB != nil && evBus != nil {
		memDB.SetAccessHook(func(ev memory.AccessEvent) {
			payload := map[string]interface{}{
				"op": ev.Op,
			}
			// 只在非空时带上字段：空数组会让前端白白重算一次。
			if len(ev.Blocks) > 0 {
				payload["blocks"] = ev.Blocks
			}
			if len(ev.Created) > 0 {
				payload["created"] = ev.Created
			}
			if len(ev.Removed) > 0 {
				payload["removed"] = ev.Removed
			}
			if len(ev.Merged) > 0 {
				merged := make([]map[string]string, 0, len(ev.Merged))
				for _, m := range ev.Merged {
					merged = append(merged, map[string]string{"from": m.From, "to": m.To})
				}
				payload["merged"] = merged
			}
			if ev.Tool != "" {
				payload["tool"] = ev.Tool
			}
			evBus.Publish(&events.Event{
				Type:      events.EventMemoryAccess,
				Source:    "main",
				Payload:   payload,
				Timestamp: time.Now().Unix(),
			})
		})
	}

	// D4：把「设备是否已授权」的判据注入插件侧（toolImpl.CanUse 消费它）。
	//
	// 为什么必须在这里：判据要用 agent 自己的 allowedOutputs，而
	// pluginReg 早于 agent 构造（newStageAndRegistry 在 main() 里先跑），
	// 所以 registry 存的是**晚绑定**闭包，注入点必须在 agent 建好之后。
	//
	// 不注入的后果（已实测的真实缺口）：设备授权闸只存在于
	// core.executeToolCallInner，即「agent 收到模型 tool_call」那条路径；
	// 而 ToolAPI.ExecuteTool 是**另一条**独立入口，不经那道闸 ⇒
	// 凡是走 ToolAPI 的调用都能绕过 AllowedOutputs。实测范围不止序列：
	// cli 的 /terminal 就直接经 ToolAPI 调 agentcli 的终端工具。
	pluginReg.SetDeviceAuthQuery(func(deviceID string) bool {
		return agent.IsOutputAllowed("device/" + deviceID)
	})

	// 方案 B：把 agent 的**内置工具面**注入 ToolAPI。
	//
	// 缺口背景：`memory_*` / `knowledge_*` / `doc_*` / `person_*` 这 20+ 个
	// 在 executeToolCallInner 里按前缀分派，从不进 ToolAPI ⇒ 插件经 ToolAPI
	// 既查不到也调不了。真机实跑实证：seq_run 报「工具 knowledge_list
	// 不存在或未注册」，而同一轮模型直接调它是成功的。
	//
	// 注入必须在此处（agent 构造之后）：内置工具的可见性由运行期状态门控
	// （memory/knowledge 是否就绪），而 provider 持有的是 agent。
	agent.InstallBuiltinToolProvider()

	return agent
}

// wirePluginSDK 注入**只有 agent 才知道**的那两项（KERNEL-REGISTRY-SETLOCK 去重后）。
//
// ## 为什么这里不再注入那 7 项底层依赖（2026-10-24 去重）
//
// 原先本函数与 pluginhost.injectBaseDeps 各自把 LuaVM/BaseAPIKey/Supervisor/
// Tracker/Config/StageHost/Indexer 写一遍，而两个模块同处启动图第 4 层、
// **并行执行** ⇒ 同一批字段被两个 goroutine 无锁写（实测 4% 概率报 DATA RACE）。
//
// 判掉哪一侧：留 pluginhost，砍这边。理由是**必要性不对称**：
//
//	· pluginhost 的注入是**必需**的 —— 它自己紧接着就 LoadSkipping 拉插件，
//	  而它刻意 `Requires()` 不含 agent（核心收益：插件不必等 agent 那 35 秒）。
//	  砍掉它 ⇒ 第 4 层加载的插件拿到全 nil 依赖。
//	· agent 这边注入的这批值是**同一批 ks.* 字段的同一份值**，写第二次
//	  没有新增信息；砍掉它 ⇒ 语义不变（pluginhost 侧的 nil 守卫更严，
//	  Supervisor 只因 FacSupervisor 已在 Requires 里而恒非 nil）。
//
// ## 注意：去重**不能**替代加锁
//
// 去重后本函数仍有 SetStatusProvider / SetDeviceAuthQuery 两项要写，而
// pluginhost 同时可能正在加载插件（buildSDK 读 status）⇒ 写读依然并发。
// 所以 registry 侧 Set*/读侧的锁是**真正的修复**，去重只是消除了一处
// 无意义的重复写。两件事都做，缺一不可。
func wirePluginSDK(k *kernelboot.KernelState) {
	k.PluginRegistry.SetStatusProvider(k.Agent)
}

// loadPersonality 载入人格（原 cmd/homed 的整块平移）。
func loadPersonality(cfg *types.Config, cfgReg *config.ConfigRegistry) *agentPkg.Personality {
	personalPath := filepath.Join(cfg.Daemon.DataDir, "personal", "personal.md")
	personality, err := agentPkg.LoadPersonality(personalPath)
	if err != nil {
		log.Printf("[homed] warning: load personality: %v", err)
	}
	if personality != nil && personality.Content != "" {
		log.Printf("[homed] persona source=file %s (takes precedence over config), %d bytes", personalPath, len(personality.Content))
		if hints := agentPkg.PersonaStaleHints(personality.Content); len(hints) > 0 {
			log.Printf("[homed] warning: persona file contains content that will rot %v -- migrate to config entry core.agent.personal_prompt "+
				"(the default template has no version number; when asked for the version, use the runtime snapshot)", hints)
		}
	} else if pv := cfgReg.GetString("core.agent.personal_prompt", config.DefaultPersonaPrompt); strings.TrimSpace(pv) != "" {
		personality = &agentPkg.Personality{Content: pv, Path: "(core.agent.personal_prompt)"}
		log.Printf("[homed] persona source=config entry core.agent.personal_prompt, %d bytes", len(pv))
	} else {
		log.Printf("[homed] persona source=none (config entry empty and no persona file)")
	}
	return personality
}

// errMissing 报告"依赖的模块产物为 nil"。
//
// ★ 为什么立刻报错而不是让 nil 传下去：nil 读出零值后 continue 会在**很远
// 的地方**变成模糊症状（如"模型没有配置"）。这里点出**是哪个模块的哪个
// 产物缺失**，把排查成本从"追几小时"降到"看一行"。
//
// 措辞刻意点出"依赖图本应保证它非 nil" —— 若真出现，说明
// ① 模块忘了声明依赖，或 ② Requires/Provides 设施名不一致（拼写漂移）。
func errMissing(moduleName, facility string) error {
	return fmt.Errorf("模块 %s 依赖的设施 %q 对应的数据为 nil —— "+
		"依赖图本应保证它已就绪。两种可能：① 该模块没在 Requires 里声明这个依赖；"+
		"② Provides/Requires 的设施名写得不一致（拼写漂移）", moduleName, facility)
}

var _ = events.EventMemoryAccess
var _ = memory.AccessEvent{}
