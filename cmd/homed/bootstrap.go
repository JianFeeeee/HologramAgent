package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	agentPkg "github.com/JianFeeeee/HomeAgent/internal/agent"
	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentCore "github.com/JianFeeeee/HomeAgent/internal/agent/core"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	internalConfig "github.com/JianFeeeee/HomeAgent/internal/config"
	"github.com/JianFeeeee/HomeAgent/internal/events"
	"github.com/JianFeeeee/HomeAgent/internal/ipc"
	"github.com/JianFeeeee/HomeAgent/internal/knowledge"
	logpkg "github.com/JianFeeeee/HomeAgent/internal/log"
	luapkg "github.com/JianFeeeee/HomeAgent/internal/lua"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/distill"
	"github.com/JianFeeeee/HomeAgent/internal/memory/document"
	"github.com/JianFeeeee/HomeAgent/internal/memory/pipeline"
	"github.com/JianFeeeee/HomeAgent/internal/memory/social"
	"github.com/JianFeeeee/HomeAgent/internal/memory/text"
	"github.com/JianFeeeee/HomeAgent/internal/memory/vector"
	"github.com/JianFeeeee/HomeAgent/internal/nlp"
	"github.com/JianFeeeee/HomeAgent/internal/plugin"
	"github.com/JianFeeeee/HomeAgent/internal/recovery"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	"github.com/JianFeeeee/HomeAgent/internal/supervisor"
	"github.com/JianFeeeee/HomeAgent/internal/tracker"
	"github.com/JianFeeeee/HomeAgent/pkg/embedding"
	"github.com/JianFeeeee/HomeAgent/pkg/generation"
	"github.com/JianFeeeee/HomeAgent/pkg/types"
)

// defaultSystemPrompt 是内置默认人格模板：不含版本号字面量，
// 被问版本时以运行时快照为准（历史上写死版本号导致实例自称旧版本）。
const defaultSystemPrompt = `你是 HomeAgent，一个持续运行的个人管家。
你的每次回复会自动发送到当前输出通道（默认=输入源），无需额外工具。
如需切换回复通道，使用 output_set_channel。
如需异步发送消息或通知，使用 output_send 指定通道和内容。
使用 output_list_channels 查看可用通道及其能力。

可用工具列表会由系统自动传入，按需使用即可。以下是你尤其需要关注的几类工具：
- memory_* — 图记忆（长期记忆，记录和查询个人信息/事实）
- knowledge_* — 知识库（查阅预设知识文档）
- doc_* — 文档记忆（近期对话的存档，查询后自动清除）
- person_* — 人物特质与社交关系网
- llm_* — LLM 源管理（列出/切换模型提供商）
- output_* — 输出通道管理（切换/发送消息）
- timer_set — 设置定时提醒
- plgreload — 热重载插件
- spawn_child — 生成子 Agent 异步执行独立任务（可传 max_turns 控制工具轮数，默认 5）

并行策略：遇到多个互不依赖的子任务时，优先并行 spawn 多个子 Agent 而非自己串行逐个执行；
长耗时任务（批量处理、多轮搜索汇总）也应交给子 Agent，避免阻塞当前对话。
- describe_image — 描述用户上传的图片
- transcribe_audio — 转写用户上传的音频
- ocr_image — 识别图片中的文字

命令与文件操作策略：
- cmd_run 经完整 shell（bash）执行，支持管道、分号、&&、命令替换、heredoc、重定向。
- 多步交互式程序（vim/top/ssh 会话、需要持续输入的进程）用 terminal_create 创建终端，
  terminal_write 发送输入、terminal_read 读输出——不要用 cmd_run 硬等交互程序退出。
- 写文件优先 files_write（原子+留档），生成多行内容时可用 heredoc 或 files_write，
  不要用 echo 拼接长文本。
- 读用户发来的文件用 files_read；向 webui 回传图片/文件用 output_send__webui(type=image/file)。

当用户上传图片或音频时，系统会自动附着媒体内容。如果模型不支持直接处理多媒体，请使用上述工具。

回复你的真实想法，用自然语言与用户交流。不要在回复中使用 emoji 表情。`

// setupLogging 初始化日志：行号前缀 + 同时输出到控制台与 <data>/log/ 下的本次启动文件。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func setupLogging(dataDir string) string {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)

	// 文件日志：同时输出到控制台和 data/log/ 目录
	logDir := filepath.Join(dataDir, "log")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		log.Printf("[homed] warning: cannot create log dir: %v", err)
	} else {
		logPath := filepath.Join(logDir, fmt.Sprintf("homed_%s.log", time.Now().Format("2006-01-02_15-04-05")))
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("[homed] warning: cannot open log file: %v", err)
		} else {
			log.SetOutput(io.MultiWriter(os.Stderr, logFile))
			log.Printf("[homed] logging to %s", logPath)
		}
	}

	return logDir
}

// ensureDataDirs 建好启动期需要的全部目录，返回 agent 的 overlayfs 工作目录。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func ensureDataDirs(dataDir string) string {
	agentWorkDir := filepath.Join(dataDir, "agentfs")
	dirs := []string{
		dataDir,
		filepath.Join(dataDir, "snapshots"),
		filepath.Join(dataDir, "plugins"),
		filepath.Join(dataDir, "changesets"),
		filepath.Join(dataDir, "memory"),
		filepath.Join(dataDir, "memory", "raw"),
		filepath.Join(dataDir, "adapters"),
		agentWorkDir,
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			log.Fatalf("create dir %s: %v", d, err)
		}
	}

	return agentWorkDir
}

// memoryStack 聚合记忆侧组件：图库、索引器、社交图、蒸馏器。
type memoryStack struct {
	db        *memory.GraphDB
	indexer   *memory.Indexer
	social    *social.SocialStore
	distiller *pipeline.Distiller
}

// initMemoryStack 初始化图记忆 / 索引 / 社交图 / 蒸馏管线。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func initMemoryStack(dataDir string, cfgReg *internalConfig.ConfigRegistry) (*memoryStack, func()) {
	memDB, err := memory.NewGraphDB(filepath.Join(dataDir, "memory", "graph.db"))
	if err != nil {
		log.Printf("[homed] warning: memory init failed: %v", err)
		memDB = nil
	} else {
		log.Printf("[homed] graph memory initialized")
	}
	memIdx := memory.NewIndexer(memDB)
	memIdx.Sync() // 启动时立即同步，避免前30分钟空窗
	socialStore := social.New(memDB)

	distiller := pipeline.NewDistiller(memDB, dataDir, pipeline.DistillerConfig{
		Interval:      10 * time.Minute,
		RetentionDays: 7,
		BatchSize:     50,
	})
	// ★ 必须在 distiller.Start() **之前**注入：Start 会起 distillLoop，
	// 而第一次 tick 在 Interval 之后。若先 Start 再注入，头一个周期
	// （实测配置 10 分钟）会白跑一次 jieba 路 —— 而那条路实测产出 0 条。
	if memDB != nil {
		distiller.SetSplitter(initDistillSplitter(cfgReg))
	}

	if memDB != nil {
		// 这里**故意不写 defer distiller.Stop()**：本函数在 return 时即触发
		// defer，而 Stop() → cancel() 会让刚启动的 distillLoop 立刻退出，
		// 规则蒸馏管线启动即死、10min 心跳从不运行（旧 main() 拆分时的残留）。
		// 停机由调用点注册的 cleanup 负责（见下方返回值）。
		distiller.Start()
	}

	return &memoryStack{db: memDB, indexer: memIdx, social: socialStore, distiller: distiller},
		func() {
			// 与原 main 的两个 defer 同序（LIFO）：先停蒸馏器，再关图库。
			if memDB != nil {
				distiller.Stop()
			}
			if memDB != nil {
				memDB.Close()
			}
		}
}

// startMemoryCandidateConsumer 起一个常驻 goroutine：把 eventbus 上的 memory_candidate 事件写进文本记忆并喂给蒸馏管线。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func startMemoryCandidateConsumer(ctx context.Context, iom *agentIO.IOManager, textMem *text.Memory, memDB *memory.GraphDB, distiller *pipeline.Distiller) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-iom.OutputChan():
				if !ok {
					return
				}
				if evt.Target == "memory" && evt.Type == "memory_candidate" {
					source, _ := evt.Payload["source"].(string)
					input, _ := evt.Payload["input"].(string)
					response, _ := evt.Payload["response"].(string)
					toolsUsed, _ := evt.Payload["tools_used"].([]string)
					toolResults, _ := evt.Payload["tool_results"].([]interface{})
					agentID, _ := evt.Payload["agent_id"].(string)

					if input != "" && textMem != nil {
						te := text.Event{
							Timestamp: time.Now().Unix(),
							Source:    source,
							Input:     input,
							Response:  response,
							ToolsUsed: toolsUsed,
							AgentID:   agentID,
						}
						if err := textMem.Append(te); err != nil {
							log.Printf("[homed] text memory append: %v", err)
						}
					}

					if input != "" && memDB != nil {
						distiller.Append("agent", "user", input)
					}
					if response != "" && memDB != nil {
						distiller.Append("agent", "assistant", response)
					}

					// 工具输出接入蒸馏管线
					for _, tr := range toolResults {
						if trMap, ok := tr.(map[string]interface{}); ok {
							if text, ok := trMap["output"].(string); ok && text != "" && memDB != nil {
								distiller.Append("agent", "tool", text)
							}
						}
					}
				}
			}
		}
	}()
}

// initLLMProviders 按配置注册全部 LLM 源（每个源经 Lua 适配器协议转换），并把各适配器的并发额度汇总回 Lua VM。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func initLLMProviders(cfg *types.Config, luaVM *luapkg.VM, baseAPIKey string) *agentAPI.ProviderManager {
	providerMgr := agentAPI.NewProviderManager()
	adapterConcurrency := map[string]int{}
	for _, src := range cfg.LLM.Sources {
		if !agentAPI.IsValidSourceConfig(src.Name, src.BaseURL, src.Model, src.Adapter) {
			log.Printf("[homed] skip invalid llm source %q (base_url=%q model=%q adapter=%q)", src.Name, src.BaseURL, src.Model, src.Adapter)
			continue
		}
		key := src.APIKey
		if key == "" {
			key = baseAPIKey
		}
		luaProvider := agentAPI.NewLuaAdaptedProvider(agentAPI.BaseConfig{
			Model:         src.Model,
			BaseURL:       src.BaseURL,
			APIKey:        key,
			Temperature:   cfg.LLM.Temperature,
			MaxTokens:     cfg.LLM.MaxTokens,
			ContextWindow: src.ContextWindow,
			MaxConcurrent: src.MaxConcurrent,
			Priority:      src.Priority,
			Vision:        src.Vision,
			Audio:         src.Audio,
		}, luaVM, src.Name, src.Adapter)
		providerMgr.Register(src.Name, luaProvider)
		if src.Adapter != "" {
			adapterConcurrency[src.Adapter] += src.MaxConcurrent
		}
	}
	luaVM.ConfigureConcurrency(adapterConcurrency)
	if cfg.LLM.Provider != "" {
		providerMgr.SetDefault(cfg.LLM.Provider)
	}

	return providerMgr
}

// initDocStore 启动文档记忆。flush 的唯一入口是 Stop()，所以关停时必须调用它。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func initDocStore(cfg *types.Config) (*document.Store, func()) {
	docStore := document.NewStore(filepath.Join(cfg.Daemon.DataDir, "memory", "documents"), memory.TokenizeWords)
	if err := docStore.Start(); err != nil {
		log.Printf("[homed] warning: document store: %v", err)
	}
	// 关停时落盘。文档记忆的内存态变更（迁移结果、访问计数等）只在 flush
	// 里写盘，而 flush 的唯一入口是 Stop()——此前全仓无人调用它，
	// 于是迁移结果永不落盘、每次启动白算一遍。

	return docStore, func() { docStore.Stop() }
}

// initDistillSplitter 按配置打开蒸馏用的生成侧 provider 并组装拆分器。
//
// 返回 nil 表示不启用（未配置 provider、或打开失败）—— 此时蒸馏回退既有的
// jieba/ONNX 路。**不因为模型不可用而让蒸馏停摆**：那条路虽然实测产出 0 条，
// 但它至少不会让对话记忆丢失。
//
// 配置形态与多模态向量侧对称（core.memory.multimodal_space.*）：
//
//	core.memory.distill.generation.provider = ollama
//	core.memory.distill.generation.options.model = qwen3:1.7b
//	core.memory.distill.generation.options.num_thread = 10
func initDistillSplitter(cfgReg *internalConfig.ConfigRegistry) pipeline.RecordSplitter {
	if cfgReg == nil {
		return nil
	}
	providerName := cfgReg.GetString("core.memory.distill.generation.provider", "")
	if providerName == "" {
		return nil
	}
	opts := map[string]string{}
	const optPrefix = "core.memory.distill.generation.options."
	for _, key := range cfgReg.List(optPrefix) {
		opts[strings.TrimPrefix(key, optPrefix)] = cfgReg.GetString(key, "")
	}
	provider, err := generation.Open(providerName, generation.Config{Options: opts})
	if err != nil {
		log.Printf("[homed] warning: 蒸馏生成 provider %q 打开失败: %v（蒸馏回退 jieba 路；已注册: %s）",
			providerName, err, strings.Join(generation.Names(), ", "))
		return nil
	}
	if !provider.Info().SupportsJSONSchema {
		// 拆分器的零幻觉完全依赖 schema 约束。不支持 schema 的 provider
		// 会拿到自由文本，解析必炸 —— 与其让它在运行时反复失败，不如现在就拒。
		log.Printf("[homed] warning: 蒸馏生成 provider %q 不支持 JSON Schema，拆分不可靠（蒸馏回退 jieba 路）",
			providerName)
		provider.Close()
		return nil
	}
	log.Printf("[homed] distill generation active: provider=%s model=%s",
		providerName, provider.Info().Model)
	return distill.NewExtractor(provider, nil)
}

// initMultimodalSpace 从公共注册表打开多模态向量 provider。返回 (空间, provider 名, 失败原因, cleanup)：后两个值只用于状态报告。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func initMultimodalSpace(cfgReg *internalConfig.ConfigRegistry) (vector.MultimodalEmbedder, string, string, func()) {
	var multimodalSpace vector.MultimodalEmbedder
	// 这两个值只用于状态报告（healthcheck_kernel 的 onnx 段）：
	// 「配了哪个 provider」与「为什么没启用」，避免只能看到 false 却不知原因。
	var mmProviderName, mmErr string
	var closeAdapted func()
	if mmProvider := cfgReg.GetString("core.memory.multimodal_space.provider", ""); mmProvider != "" {
		mmProviderName = mmProvider
		opts := map[string]string{}
		const optPrefix = "core.memory.multimodal_space.options."
		for _, key := range cfgReg.List("core.memory.multimodal_space.options.") {
			opts[strings.TrimPrefix(key, optPrefix)] = cfgReg.GetString(key, "")
		}
		provider, err := embedding.Open(mmProvider, embedding.Config{Options: opts})
		if err != nil {
			mmErr = err.Error()
			log.Printf("[homed] warning: 多模态向量 provider %q 打开失败: %v（多模态向量检索已禁用；已注册: %s）",
				mmProvider, err, strings.Join(embedding.Names(), ", "))
		} else if adapted, err := vector.AdaptProvider(provider); err != nil {
			provider.Close()
			mmErr = err.Error()
			log.Printf("[homed] warning: 多模态向量 provider %q 元数据不合法: %v（多模态向量检索已禁用）", mmProvider, err)
		} else {
			multimodalSpace = adapted
			info := provider.Info()
			// 指纹可能很长（模型文件哈希），日志里只取前 12 个字符便于对照。
			shortFP := info.Fingerprint
			if len(shortFP) > 12 {
				shortFP = shortFP[:12]
			}
			log.Printf("[homed] multimodal space active: provider=%s dim=%d fp=%s modalities=%v",
				mmProvider, info.Dimension, shortFP, info.Modalities)
		}
	}

	if closeAdapted == nil {
		closeAdapted = func() {}
	}
	return multimodalSpace, mmProviderName, mmErr, closeAdapted
}

// initKnowledgeStore 启动知识库。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func initKnowledgeStore(cfg *types.Config) *knowledge.Store {
	ks := knowledge.NewStore(filepath.Join(cfg.Daemon.DataDir, "knowledge"))
	if err := ks.Start(); err != nil {
		log.Printf("[homed] warning: knowledge store: %v", err)
	} else {
		log.Printf("[homed] knowledge store active with %d items", len(ks.List()))
	}

	return ks
}

// initKnowledgeMigration 在知识库扫盘**之前**把存量目录名规范化。
//
// 为何不靠 Store 内部自己做：规范名是「内存键 + 盘上目录 + LLM 可见名字」
// 三者必须逐字一致，而磁盘重命名属于有破坏性的副作用，应该在 store 扫盘
// 之前、在明确的边界上一次性做完，而不是散在 Store 的初始化路径里。
//
// 为何默认只报告：os.Rename 不可逆，批量重命名生产数据必须由人确认。
// 需要真正迁移时用 homed-kb-migrate -apply（或把下面 defaultApply 打开）。
//
// 本函数体同样遵守 bootstrap 的平移原则。
func initKnowledgeMigration(cfg *types.Config) {
	root := filepath.Join(cfg.Daemon.DataDir, "knowledge")
	const (
		// defaultApply = false ⇒ 启动时只扫描并报告，不改名。
		defaultApply = false
		// maxRenamePerRun 限制单次重命名数：给失控的目录规模设一个上限，
		// 避免启动阶段被一次大迁移拖住。
		maxRenamePerRun = 200
	)
	items, err := knowledge.PlanMigration(root)
	if err != nil {
		log.Printf("[homed] 知识库迁移扫描失败（跳过）: %v", err)
		return
	}
	need, illegal := 0, 0
	for _, it := range items {
		if it.Illegal {
			illegal++
		} else if it.NewName != "" {
			need++
		}
	}
	if need == 0 && illegal == 0 {
		return
	}
	if illegal > 0 {
		log.Printf("[homed] 知识库迁移：%d 条名称非法（含 .. / 点段 / 隐藏段），写入与删除均已拒绝，需人工处理", illegal)
	}
	if need == 0 {
		return
	}
	log.Printf("[homed] 知识库迁移：%d/%d 条目录名待规范化（例：%s → %s）", need, len(items),
		items[0].OldName, items[0].NewName)
	if !defaultApply {
		log.Printf("[homed] 知识库迁移：当前为只报告模式。确认清单后执行：homed-kb-migrate -root %s -apply", root)
		return
	}
	if need > maxRenamePerRun {
		log.Printf("[homed] 知识库迁移：需改名 %d 条超过单次上限 %d，本次只处理前 %d 条",
			need, maxRenamePerRun, maxRenamePerRun)
	}
	applied, failed := knowledge.ApplyMigration(root, items, maxRenamePerRun)
	log.Printf("[homed] 知识库迁移完成：成功 %d，失败 %d", applied, failed)
}

// loadPersonality 按「个人文件 > 配置项」的优先级解析人格内容，并对腐坏内容告警。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func loadPersonality(cfg *types.Config, cfgReg *internalConfig.ConfigRegistry) *agentPkg.Personality {
	personalPath := filepath.Join(cfg.Daemon.DataDir, "personal", "personal.md")
	personality, err := agentPkg.LoadPersonality(personalPath)
	if err != nil {
		log.Printf("[homed] warning: load personality: %v", err)
	}
	if personality != nil && personality.Content != "" {
		log.Printf("[homed] 人格来源=文件 %s（优先于配置项），%d 字节", personalPath, len(personality.Content))
		if hints := agentPkg.PersonaStaleHints(personality.Content); len(hints) > 0 {
			log.Printf("[homed] warning: 人格文件含会腐坏的内容 %v — 建议迁到配置项 core.agent.personal_prompt"+
				"（默认模板不含版本号，被问版本时以运行时快照为准）", hints)
		}
	} else if pv := cfgReg.GetString("core.agent.personal_prompt", internalConfig.DefaultPersonaPrompt); strings.TrimSpace(pv) != "" {
		personality = &agentPkg.Personality{Content: pv, Path: "(core.agent.personal_prompt)"}
		log.Printf("[homed] 人格来源=配置项 core.agent.personal_prompt，%d 字节", len(pv))
	} else {
		log.Printf("[homed] 人格来源=无（配置项为空且无人格文件）")
	}

	return personality
}

// newStageAndRegistry 建阶段管道与插件注册表，把内核依赖接到注册表上。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func newStageAndRegistry(cfg *types.Config, cfgReg *internalConfig.ConfigRegistry, iom *agentIO.IOManager,
	evBus *events.Bus, memDB *memory.GraphDB, textMem *text.Memory, docStore *document.Store,
	ks *knowledge.Store, providerMgr *agentAPI.ProviderManager,
	dataDir string) (*agentCore.StageHost, *plugin.Registry) {
	stageHost := agentCore.NewStageHost()

	pluginReg := plugin.NewRegistry()
	pluginReg.SetIOManager(iom)
	pluginReg.SetEventBus(evBus)
	pluginReg.SetMemory(memDB)
	pluginReg.SetTextMemory(textMem)
	pluginReg.SetDocStore(docStore)
	pluginReg.SetKnowledge(ks)
	pluginReg.SetProviderManager(providerMgr)
	pluginReg.SetConfigRegistry(cfgReg)
	pluginReg.SetPluginDir(cfg.Plugin.Dir)
	pluginReg.SetDataDir(dataDir) // 插件 SettingsAPI.DataDir() 的数据根目录

	// Wire registration callbacks: plugins' RegisterTool/RegisterStage → StageHost
	pluginReg.SetToolRegistrar(func(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
		log.Printf("[homed] SetToolRegistrar registering tool: %s (plugin=%s)", name, def.Plugin)
		return stageHost.RegisterTool(name, def, handler)
	})
	pluginReg.SetStageRegistrar(func(stage sdk.Stage, handler sdk.StageHandler) {
		stageHost.RegisterStage(stage, handler)
	})
	pluginReg.SetAPIRegistrar(func(name string) error {
		return nil
	})
	pluginReg.SetToolCleaner(stageHost)

	return stageHost, pluginReg
}

// newMainAgent 组装主 Agent：把内核各面（IO/记忆/文档/知识/媒体/社交/文本/插件/状态）接进 AgentConfig。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func newMainAgent(cfg *types.Config, cfgReg *internalConfig.ConfigRegistry, provider agentAPI.Provider,
	providerMgr *agentAPI.ProviderManager, iom *agentIO.IOManager, memDB *memory.GraphDB,
	memIdx *memory.Indexer, trk *tracker.Tracker, docStore *document.Store, ks *knowledge.Store,
	socialStore *social.SocialStore, textMem *text.Memory,
	personality *agentPkg.Personality, pluginReg *plugin.Registry, embedder *memory.StaticEmbedder,
	multimodalSpace vector.MultimodalEmbedder, mmProviderName, mmErr string,
	stageHost *agentCore.StageHost, evBus *events.Bus) *agentCore.Agent {
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
		DocStore:        docStore,
		Knowledge:       ks,
		SocialStore:     socialStore,
		TextMemory:      textMem,
		// MediaStore 已随 CAS 淘汰：块自带负载（见 plan.md）。
		Personality:     personality,
		// 人格落库面：首启门禁（任何通道都问一次）与 persona_set 工具用。
		// 与 WebUI 向导共用 internal/config 的同一份落库逻辑。
		PersonaStore: internalConfig.RegistryPersonaStore{Reg: cfgReg},
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
		Offload: agentCore.OffloadOptions{
			Enabled:      cfgReg.GetBool("core.agent.offload_enabled", false),
			BusyAfter:    cfgReg.GetDuration("core.agent.offload_busy_after", 5*time.Minute),
			MinPending:   cfgReg.GetInt("core.agent.offload_min_pending", 3),
			MaxResidents: cfgReg.GetInt("core.agent.offload_max_residents", 2),
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
	//   节点（把 "memory_recall" 拆词去匹配中文实体名，实测命中 0），
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

// initONNXParser 初始化依存句法分析器（内嵌 ONNX 模型，失败则退回规则引擎）。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func initONNXParser(cfg *types.Config, cfgReg *internalConfig.ConfigRegistry) {
	modelPath := cfgReg.GetString("core.agent.onnx_model_path", "")
	onnxParser, err := nlp.NewONNXParser(nlp.ONNXConfig{
		ModelPath: modelPath,
		DataDir:   filepath.Join(cfg.Daemon.DataDir, "nlp"),
	})
	if err != nil {
		log.Printf("[homed] warn: ONNX parser init: %v, using fallback", err)
	} else {
		nlp.SetDefaultParser(onnxParser)
		log.Printf("[homed] dep parser initialized (model: %s)", modelPath)
	}
}

// loadPlugins 建插件目录、按启动模式决定 allowlist，然后加载全部插件。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func loadPlugins(cfg *types.Config, cfgReg *internalConfig.ConfigRegistry, pluginReg *plugin.Registry,
	stageHost *agentCore.StageHost, bootMode, dataDir string) {
	// Auto-create plugins directory (without hardcoding plugin names)
	os.MkdirAll(cfg.Plugin.Dir, 0755)

	// failback 受限启动：仅装载 failback 插件集（webfetch/files/cmd 为内核内置，
	// 此处仅控制外部插件，默认含 recoverydiag 以便直接在受限态产出恢复结论）
	if bootMode == "failback" {
		list := cfgReg.GetString("core.agent.failback_plugins", "webui,pluginmgr,recoverydiag")
		// 优先使用 guard.yaml 经过 recovery 任务下发的插件集（guard 是 failback 权威）
		if task, terr := recovery.LoadTask(recovery.TaskPath(dataDir)); terr == nil && len(task.Plugins()) > 0 {
			list = strings.Join(task.Plugins(), ",")
		}
		var names []string
		for _, s := range strings.Split(list, ",") {
			if s = strings.TrimSpace(s); s != "" {
				names = append(names, s)
			}
		}
		pluginReg.SetLoadAllowlist(names)
		log.Printf("[homed] failback boot: plugin allowlist = %v", names)
	}

	// Load all plugins — each scans its own dir and is loaded via factory or .so
	if err := pluginReg.Load(cfg.Plugin.Dir); err != nil {
		log.Printf("[homed] warning: load plugins: %v", err)
	}
	log.Printf("[homed] stage host ready with %d registered tools", stageHost.ToolCount())
}

// startAgentRuntime 接线技能索引、起日志管理、启动 agent，返回逆序关停的 cleanup。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func startAgentRuntime(cfgReg *internalConfig.ConfigRegistry, pluginReg *plugin.Registry,
	agent *agentCore.Agent, logDir string, ctx context.Context) func() {
	// 技能索引接线：skillmgr 插件实现 SkillIndexProvider 时注入 agent（方案B prompt 注入）
	if sp := pluginReg.Get("skillmgr"); sp != nil {
		if prov, ok := sp.(agentCore.SkillIndexProvider); ok {
			agent.SetSkillIndexProvider(prov)
			log.Printf("[homed] skill index wired from skillmgr plugin")
		}
	}

	// 日志管理：层级压缩 + 保留策略
	logManager := logpkg.NewManager(logDir, cfgReg)
	go logManager.Start(ctx)

	agent.Start()

	return func() {
		// 与原 main 的两个 defer 同序（LIFO）：先停 agent，再停日志管理。
		agent.Stop()
		logManager.Stop()
	}
}

// startIPCServer 起 PING/ACK 心跳服务（含 kernel 状态快照），返回仅在启动成功后生效的 cleanup。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func startIPCServer(dataDir, bootMode string, agent *agentCore.Agent) (*ipc.Server, func()) {
	started := false
	ipcServer := ipc.NewServer(dataDir, func() *ipc.Status {
		st := agent.GetKernelStatus()
		llmOK := st != nil && st.LLM.Available
		tools := 0
		if st != nil {
			tools = len(st.Tools)
		}
		uptime := int64(0)
		if st != nil {
			if d, err := time.ParseDuration(st.Uptime); err == nil {
				uptime = int64(d.Seconds())
			}
		}
		return &ipc.Status{
			PID:       os.Getpid(),
			Boot:      bootMode,
			UptimeSec: uptime,
			LLMOK:     &llmOK,
			Tools:     tools,
			LastDiag:  lastDiagSummary(dataDir),
		}
	})
	if err := ipcServer.Start(); err != nil {
		log.Printf("[homed] warning: ipc heartbeat server: %v", err)
	} else {
	}

	return ipcServer, func() {
		if started {
			ipcServer.Stop()
		}
	}
}

// startSupervisorRuntime 把真实存活源与重启通道接到 supervisor 上，返回重启请求通道。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func startSupervisorRuntime(sup *supervisor.Daemon, trk *tracker.Tracker, agent *agentCore.Agent) chan struct{} {
	sup.SetTracker(trk)
	sup.RegisterAgent("main")

	// 真实存活源 + 重启通道：daemon 心跳语义由此修正（lastHB 只在确认存活时更新），
	// 重启动作不再空转——清理后以特殊退出码交给 guard/systemd 重建。
	restartCh := make(chan struct{}, 1)
	sup.SetHeartbeatSource(func(id types.AgentID) (time.Time, types.HealthStatus, error) {
		st := agent.GetKernelStatus()
		if st == nil {
			return time.Time{}, types.HealthDown, fmt.Errorf("no kernel status")
		}
		h := types.HealthHealthy
		if !st.LLM.Available {
			h = types.HealthDegraded
		}
		return time.Now(), h, nil
	})
	sup.SetRestartHandler(func(id types.AgentID) {
		select {
		case restartCh <- struct{}{}:
		default:
		}
	})

	return restartCh
}

// startHeartbeat 每 5s 触碰 <data>/heartbeat（guard 据此判定 worker 存活/卡死），返回停止函数。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func startHeartbeat(dataDir string, ctx context.Context) func() {
	// 心跳：每 5s 触碰 <data>/heartbeat，guard 据此判定工作进程是否存活/卡死
	hbPath := filepath.Join(dataDir, "heartbeat")
	hbStop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		writeHB := func() {
			if f, err := os.OpenFile(hbPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
				fmt.Fprintf(f, "t=%d\n", time.Now().Unix())
				f.Close()
			}
		}
		writeHB()
		for {
			select {
			case <-t.C:
				writeHB()
			case <-hbStop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	return func() { close(hbStop) }
}

// waitForShutdown 阻塞至 SIGINT/SIGTERM 或 supervisor 请求重启，然后按原 main 的顺序清理，需要重建时以退出码交回 guard。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func waitForShutdown(ctx context.Context, dataDir string, restartCh chan struct{}, stopHeartbeat func(),
	pluginReg *plugin.Registry, trk *tracker.Tracker, cfgReg *internalConfig.ConfigRegistry, sup *supervisor.Daemon) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	restartRequested := false
	select {
	case <-sigCh:
		log.Printf("[homed] shutting down...")
	case <-restartCh:
		restartRequested = true
		log.Printf("[homed] restart requested, shutting down cleanly then exiting with code %d", exitRestartRequested)
	}

	stopHeartbeat()
	pluginReg.StopAll()
	if trk != nil {
		trk.Stop()
	}
	if err := cfgReg.Flush(); err != nil {
		log.Printf("[homed] flush config: %v", err)
	}
	sup.Shutdown()
	log.Printf("[homed] stopped")

	if restartRequested {
		os.Exit(exitRestartRequested)
	}
}

// initLuaVM 起 Lua VM（LLM 协议适配）；启动失败只告警，cleanup 为 no-op。
func initLuaVM(cfg *types.Config) (*luapkg.VM, func()) {
	luaVM := luapkg.NewVM(filepath.Join(cfg.Daemon.DataDir, "adapters"))
	if err := luaVM.Start(); err != nil {
		log.Printf("[homed] warning: lua vm init failed: %v", err)
		return luaVM, func() {}
	}
	return luaVM, luaVM.Stop
}

// initSupervisor 起守护管理（代理生命周期管理）；起不来是致命错误。
func initSupervisor(cfg *types.Config) *supervisor.Daemon {
	sup := supervisor.New(cfg)
	if err := sup.Start(); err != nil {
		log.Fatalf("start supervisor: %v", err)
	}
	return sup
}

// initTracker 起 overlayfs 变更追踪；无 overlayfs 支持时降级为非致命告警。
func initTracker(cfg *types.Config, agentWorkDir string) *tracker.Tracker {
	trk := tracker.NewTracker(cfg.Daemon.DataDir, agentWorkDir,
		tracker.WithKeepChangesets(100),
		tracker.WithMaxChangesetAge(30*24*time.Hour),
	)
	if err := trk.Init(); err != nil {
		log.Printf("[homed] warning: tracker init: %v", err)
	} else {
		if err := trk.Start(); err != nil {
			log.Printf("[homed] warning: tracker mount overlay: %v (non-fatal: no overlayfs support?)", err)
		} else {
			log.Printf("[homed] change tracker active at %s", trk.MergeDir())
		}
	}
	return trk
}

// initKernelAPI 建内核与插件之间的两个通道：IOManager（IO 抽象层）+ EventBus（事件总线）。
func initKernelAPI() (*agentIO.IOManager, *events.Bus) {
	iom := agentIO.NewIOManager()
	evBus := events.NewBus()
	log.Printf("[homed] kernel API ready: IOManager + EventBus")
	return iom, evBus
}

// initTextMemory 起文本记忆；启动失败只告警，cleanup 为 no-op。
func initTextMemory(cfg *types.Config) (*text.Memory, func()) {
	textMem := text.New(filepath.Join(cfg.Daemon.DataDir, "memory", "text"))
	if err := textMem.Start(); err != nil {
		log.Printf("[homed] warning: text memory start: %v", err)
		return textMem, func() {}
	}
	log.Printf("[homed] text memory active at %s", filepath.Join(cfg.Daemon.DataDir, "memory", "text"))
	return textMem, textMem.Stop
}

// resolveBaseAPIKey 解析兜底 API key：配置项 > LLM_API_KEY > DEEPSEEK_API_KEY。
func resolveBaseAPIKey(cfg *types.Config) string {
	apiKey := cfg.LLM.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("LLM_API_KEY")
	}
	if apiKey == "" {
		apiKey = os.Getenv("DEEPSEEK_API_KEY")
	}
	return apiKey
}

// wirePluginSDK 把内核各面注入每个插件的 PluginSDK（阶段6 将替换遗留的 util.Configure）。
func wirePluginSDK(pluginReg *plugin.Registry, luaVM *luapkg.VM, baseAPIKey string, sup *supervisor.Daemon,
	trk *tracker.Tracker, cfg *types.Config, stageHost *agentCore.StageHost, memIdx *memory.Indexer,
	agent *agentCore.Agent) {
	pluginReg.SetLuaVM(luaVM)
	pluginReg.SetBaseAPIKey(baseAPIKey)
	pluginReg.SetSupervisor(supervisor.NewSDKAdapter(sup))
	pluginReg.SetTracker(trk)
	pluginReg.SetConfig(cfg)
	pluginReg.SetStageHost(stageHost)
	pluginReg.SetIndexer(memIdx)
	pluginReg.SetStatusProvider(agent)
	pluginReg.SetTerminalAPI(agent)
}

// resolveWebUIOverride 解析 webui 监听地址的覆盖值，空串表示不覆盖。
//
// 优先级：CLI --webui > 核心配置 webui.listen_addr（仅当它被改成非内置默认值）。
// 两者都不给时由 webui 插件自己的 settings["addr"] 决定。
//
// 为什么不写成“内核在插件加载前 Set 插件 settings['addr']”：那时
// config_webui 表还没建（表只在插件注册 def 时创建），PluginSettings.Set 的
// INSERT 会失败而错误被忽略，随后插件 Start 里 RegisterDef 才建表并写入默认
// :8080 —— 于是 CLI --webui 与 webui.listen_addr **一直是死配置**，
// 无论怎么传都监听 :8080。覆盖值改由插件自己接收（webui.SetListenOverride）。
func resolveWebUIOverride(cfgReg *internalConfig.ConfigRegistry, httpAddr string) string {
	if strings.TrimSpace(httpAddr) != "" {
		return strings.TrimSpace(httpAddr)
	}
	// webui.listen_addr 的播种默认值就是 ":8080"；与默认值相同视为“未配置”，
	// 否则会把用户在设置页里改过的插件 addr 顶掉。
	if v := strings.TrimSpace(cfgReg.GetString("webui.listen_addr", ":8080")); v != "" && v != ":8080" {
		return v
	}
	return ""
}

// options 是 worker 的命令行参数。
type options struct {
	dataDir   string
	httpAddr  string
	cliSocket string
	role      string
	boot      string
}

// parseFlags 解析命令行参数。
func parseFlags() options {
	dataDir := flag.String("data", "", "data directory (default: auto-detect next to binary)")
	httpAddr := flag.String("webui", "", "webui listen address (default: webui.listen_addr from config)")
	cliSocket := flag.String("socket", "", "cli unix socket path (default: <data>/cli.sock)")
	role := flag.String("role", "agent", "process role: guard (父守护) | agent (工作进程)")
	boot := flag.String("boot", "normal", "agent boot mode: normal | failback (受限启动，仅 failback 插件集)")
	flag.Parse()
	return options{dataDir: *dataDir, httpAddr: *httpAddr, cliSocket: *cliSocket, role: *role, boot: *boot}
}

// compactConfigDB 在空闲页够多时压缩配置库；失败只告警（不影响启动）。
//
// 触发条件（见 internal/config.MaybeCompact）：空闲页 >= 1MB 且占页数 >= 25%。
// 放在插件加载之后调用——迁移/清理大值发生在插件 Start 里，之前调用没有意义。
func compactConfigDB(cfgReg *internalConfig.ConfigRegistry) {
	before := int64(-1)
	if st, err := os.Stat(cfgReg.DBPath()); err == nil {
		before = st.Size()
	}
	done, err := cfgReg.MaybeCompact(1<<20, 0.25)
	if err != nil {
		log.Printf("[homed] warning: 配置库压缩失败: %v", err)
		return
	}
	if !done {
		return
	}
	after := before
	if st, err := os.Stat(cfgReg.DBPath()); err == nil {
		after = st.Size()
	}
	log.Printf("[homed] 配置库已压缩: %d -> %d 字节", before, after)
}
