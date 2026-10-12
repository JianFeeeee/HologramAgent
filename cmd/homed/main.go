package main

import (
	"context"
	"log"
	"path/filepath"
	"strings"

	internalConfig "github.com/JianFeeeee/HomeAgent/internal/config"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/meta"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/clawhubadapter"
	cli "github.com/JianFeeeee/HomeAgent/internal/plugins/cli"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/healthcheck"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/kbtree"
	_ "github.com/JianFeeeee/HomeAgent/internal/plugins/pluginmgr"
	webui "github.com/JianFeeeee/HomeAgent/internal/plugins/webui"

	// 空白导入内置 provider：它们各自在 init 里注册到 pkg/embedding。
	// 想把核心换成自己的模型，只需替换这一行（或另建一个发行版 main）。
	_ "github.com/JianFeeeee/HomeAgent/providers/chineseclip"
	_ "github.com/JianFeeeee/HomeAgent/providers/qwen3vl"
	// 生成侧 provider（蒸馏拆三元组）。与上面两个向量 provider 各自独立注册：
	// 核心只依赖 pkg/generation 的接口，按配置 Open("ollama")。
	_ "github.com/JianFeeeee/HomeAgent/providers/ollama"
)

// main 是 worker 进程的启动序列。
//
// 形状约定：本函数只保留「顺序编排 + 就地交接」——
//   - 每个阶段一行调用，参数即该阶段的全部依赖（依赖顺序即调用顺序）；
//   - 阶段实现体在同包 bootstrap.go，与这里的调用一一对应；
//   - 需要逆序释放的资源由阶段函数返回 cleanup，在**原位** defer 注册，
//     因此释放顺序与拆分前完全一致。
func main() {
	// 平台门放在最前面：比 flag 解析还早，因为原生 Windows 上根本不应进入任何
	// 初始化路径（会去建共享段、拉插件进程）。理由与 WSL 指引见
	// platform_windows.go。
	requireSupportedPlatform()

	opt := parseFlags()

	// 父守护模式：只负责拉起/守护 worker，不初始化 agent 内核
	if opt.role == "guard" {
		runGuard(resolveDataDir(opt.dataDir))
		return
	}

	log.Printf("[homed] role=agent boot=%s", opt.boot)

	if opt.dataDir == "" {
		opt.dataDir = resolveDataDir(opt.dataDir)
	}

	if opt.cliSocket == "" {
		opt.cliSocket = filepath.Join(opt.dataDir, "cli.sock")
	}

	logDir := setupLogging(opt.dataDir)

	log.Printf("[homed] starting %s", meta.FullVersion())

	agentWorkDir := ensureDataDirs(opt.dataDir)

	// ---- 配置中心（SQLite 持久化，唯一配置源） ----
	//
	// 必须早于 initMemoryStack：蒸馏的生成侧 provider 由配置决定
	//（core.memory.distill.generation.provider），且要在 distiller.Start()
	// 之前注入 —— 否则第一个 tick 会白跑一次产出为 0 的 jieba 路。
	cfgReg := internalConfig.NewConfigRegistry(filepath.Join(opt.dataDir, "config.db"))
	defer cfgReg.Close()
	cfgReg.SeedDefaults(opt.dataDir)
	// LLM 配置写前留档（config_set 写 core.llm.* 前自动快照），guard 恢复用基线
	cfgReg.SetLLMSnapshotFile(filepath.Join(opt.dataDir, "llm_snapshot.json"))
	cfg := cfgReg.ToConfig()

	// ---- 基础设施层：记忆、技能 ----

	mem, closeMem := initMemoryStack(opt.dataDir, cfgReg)
	defer closeMem()

	// 共享词嵌入：蒸馏提取（Phase 3 TransE 验证）与 Agent 上下文复用同一实例，
	// 避免同一模型被二次加载（约 200k×300 维 ≈ 数百 MB 内存）。
	embedder := memory.NewStaticEmbedder(strings.Split(cfgReg.GetString("core.agent.embedding_model_path", ""), ",")...)
	mem.distiller.SetEmbedder(embedder)

	// ---- Lua VM（LLM 协议适配） ----

	luaVM, closeLuaVM := initLuaVM(cfg)
	defer closeLuaVM()

	// ---- 守护管理（代理生命周期管理） ----

	sup := initSupervisor(cfg)

	// ---- 变更追踪（overlayfs） ----

	trk := initTracker(cfg, agentWorkDir)

	// ========================================================================
	// 内核 API：IOManager（IO 抽象层） + EventBus（事件总线）
	// 所有插件通过这两个通道与核心交互
	// ========================================================================

	iom, evBus := initKernelAPI()

	// ---- 文本记忆 + 记忆蒸馏管线 ----

	textMem, closeTextMem := initTextMemory(cfg)
	defer closeTextMem()

	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	startMemoryCandidateConsumer(ctx, iom, textMem, mem.db, mem.distiller)

	// ---- LLM Provider 管理（多源，通过 Lua 适配器协议转换） ----

	baseAPIKey := resolveBaseAPIKey(cfg)

	providerMgr := initLLMProviders(cfg, luaVM, baseAPIKey)
	provider := providerMgr.Default()

	// L1 failback：受限 worker 启动即跑恢复梯子（probe→还原DNS/proxy→还原config+ReloadFromConfig→probe），
	// 结果以退出码 exitRecovered=43 / exitRecoveryFailed=44 交回 guard，不进入主 agent 循环。
	if opt.boot == "failback" {
		runFailbackRecovery(opt.dataDir, cfgReg, luaVM, providerMgr, baseAPIKey)
	}

	// ---- 文档记忆 + 知识库 ----

	docStore, closeDocStore := initDocStore(cfg)
	defer closeDocStore()

	multimodalSpace, mmProviderName, mmErr, closeMultimodal := initMultimodalSpace(cfgReg)
	defer closeMultimodal()

	// 迁移必须在 store 扫盘**之前**：改名后扫盘一次到位，
	// 避免先以旧名建索引、再改名造成内存键与盘上目录短暂不一致。
	initKnowledgeMigration(cfg)
	ks := initKnowledgeStore(cfg)

	// ---- 人格设定 ----

	personality := loadPersonality(cfg, cfgReg)

	// ---- 阶段管道（StageHost）+ 插件系统（Registry） ----

	stageHost, pluginReg := newStageAndRegistry(cfg, cfgReg, iom, evBus, mem.db, textMem,
		docStore, ks, providerMgr, opt.dataDir)

	// ---- Agent Core (需在插件加载前创建，因为插件 Configure 需要 StatusProvider) ----

	agent := newMainAgent(cfg, cfgReg, provider, providerMgr, iom, mem.db, mem.indexer, trk,
		docStore, ks, mem.social, textMem, personality, pluginReg, embedder,
		multimodalSpace, mmProviderName, mmErr, stageHost, evBus)

	wirePluginSDK(pluginReg, luaVM, baseAPIKey, sup, trk, cfg, stageHost, mem.indexer, agent)

	// 为内置插件注入内核依赖（各插件通过 init() 自注册工厂）
	cli.DefaultSocket = opt.cliSocket
	// webui 监听地址覆盖：必须在 loadPlugins 之前设置，插件 Start 时会读它。
	webui.SetListenOverride(resolveWebUIOverride(cfgReg, opt.httpAddr))

	// ---- 依存句法分析器（内嵌 ONNX 模型 / 规则引擎） ----

	initONNXParser(cfg, cfgReg)

	loadPlugins(cfg, cfgReg, pluginReg, stageHost, opt.boot, opt.dataDir)

	// 插件加载完成后再回收空闲页：大值（如老版聊天记录）可能在这一步被搬走/删除，
	// 而 SQLite 的 DELETE 不会缩小文件。
	compactConfigDB(cfgReg)

	stopRuntime := startAgentRuntime(cfgReg, pluginReg, agent, logDir, ctx)
	defer stopRuntime()

	_, closeIPC := startIPCServer(opt.dataDir, opt.boot, agent)
	defer closeIPC()

	restartCh := startSupervisorRuntime(sup, trk, agent)

	log.Printf("[homed] main agent started, model=%s base=%s sources=%d adapters=%d",
		cfg.LLM.Model, cfg.LLM.BaseURL, len(cfg.LLM.Sources), len(luaVM.ListAdapters()))
	log.Printf("[homed] kernel ready, waiting for plugin IO...")

	// ---- 等待退出信号 ----

	stopHeartbeat := startHeartbeat(opt.dataDir, ctx)
	waitForShutdown(ctx, opt.dataDir, restartCh, stopHeartbeat, pluginReg, trk, cfgReg, sup)
}
