package main

import (
	"context"
	"log"
	"path/filepath"
	"strings"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod/pluginhost"
	securitymod "github.com/JianFeeeee/HologramAgent/internal/kernelmod/security"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/meta"
	// ★ 内置插件统一走 internal/plugins 的清单（all.go），**不要**在这里
	// 单独导入某个插件：
	//   · 重复导入虽无害（同一包只 init 一次），但会让 all.go 不再是唯一清单
	//     —— 按它盘点的工具/文档会漏项（kbtree 曾因此长期游离在清单之外）；
	//   · 想增删内置插件请改 all.go。
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins"
	"github.com/JianFeeeee/HologramAgent/internal/systemd"
	updatepkg "github.com/JianFeeeee/HologramAgent/internal/update"

	// 空白导入内置 provider：它们各自在 init 里注册到 pkg/embedding。
	// 想把核心换成自己的模型，只需替换这一行（或另建一个发行版 main）。
	_ "github.com/JianFeeeee/HologramAgent/providers/chineseclip"
	_ "github.com/JianFeeeee/HologramAgent/providers/qwen3vl"
	// 生成侧 provider（蒸馏拆三元组）。与上面两个向量 provider 各自独立注册：
	// 核心只依赖 pkg/generation 的接口，按配置 Open("ollama")。
	_ "github.com/JianFeeeee/HologramAgent/providers/ollama"
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

	// ---- 心跳：dataDir 一确定就启动（★ 启动竞态的 worker 侧根修）----
	//
	// 心跳文件是 guard 判 worker 存活的唯一依据（mtime 新鲜度 + IPC PING）。
	// 必须在**任何重型初始化之前**首写同步落盘：停机 >heartbeatTimeout 后重启，
	// 文件 mtime 停在上一代 worker（已过期）；若等所有初始化完才首写（原位置在
	// main 尾部），guard 的 5s ticker 先到、拿旧 mtime 判 stale → 把还在加载
	// 多模态空间/知识库的 worker SIGKILL（2026-10-12 生产实测：6 连杀 +
	// failback 降级，详见 562ba6bd）。
	//
	// guard 侧另有启动宽限期兜底（shouldKillStaleHeartbeat），两层独立生效：
	// worker 侧让 mtime 尽快变新（治本），guard 侧容忍旧 mtime（防御）。
	//
	// 用 context.Background()（而非 main 的 ctx）：心跳生命周期 = 进程，
	// 与 agent 的 ctx 解耦；停止靠本函数返回的 hbStop（waitForShutdown 调它）。
	stopHeartbeatEarly := startHeartbeat(opt.dataDir, context.Background())
	defer stopHeartbeatEarly()

	if opt.cliSocket == "" {
		opt.cliSocket = filepath.Join(opt.dataDir, "cli.sock")
	}

	logDir := setupLogging(opt.dataDir)

	log.Printf("[homed] starting %s", meta.FullVersion())

	agentWorkDir := ensureDataDirs(opt.dataDir)

	// ★ 数据外置接线（第 4 项）：告诉分词器去哪里找外置词库。
	//
	// 词库 12MB + ONNX 模型 47MB 是**数据**而不是代码，却 //go:embed 进了
	// 二进制（88MB 里 59MB 是它们）。外置后：
	//   · 词库/模型可独立发版，不必重下整个二进制
	//   · 而 patch 版本之间这两个目录实际从未变过
	//
	// 必须在 GetJieba() 首次调用之前设：首次调用会初始化单例并固定目录。
	memory.SetJiebaDataDir(opt.dataDir)

	// ---- 配置中心（SQLite 持久化，唯一配置源） ----
	//
	// 必须早于 initMemoryStack：蒸馏的生成侧 provider 由配置决定
	//（core.memory.distill.generation.provider），且要在 distiller.Start()
	// 之前注入 —— 否则第一个 tick 会白跑一次产出为 0 的 jieba 路。
	cfgReg := internalConfig.NewConfigRegistry(filepath.Join(opt.dataDir, "config.db"))
	defer cfgReg.Close()
	cfgReg.SeedDefaults(opt.dataDir)

	// ========================================================================
	// ★ 数据目录**权威源**：数据库优先，命令行次之
	//
	// 用户 2026-10-28 裁定：`core.daemon.data_dir` 有值时以它为准，没有时
	// 才用 `-data`。理由是这个键是**运维可改的**（WebUI 就能改），而 `-data`
	// 写在 systemd 单元里、改它要碰部署配置。让可改的那个赢，才符合直觉。
	//
	// ## 为什么必须在 cfgReg 之后、一切重型初始化之前
	//
	// 它决定 logDir / cli.sock / 心跳文件 / 各存储的落点。晚一步就出现
	// "日志已经写进 A、数据写进 B"的分裂。
	//
	// ## 为什么只做一次
	//
	// cfgReg 是从 <cliDataDir>/config.db 打开的，而它读出的 data_dir 可能是
	// 另一个目录 —— 那个目录下**也有** config.db。若再次打开去读它，就成了
	// 自我引用的无限链。所以：库只开一次，键只读一次。
	// ========================================================================
	if dbDir := strings.TrimSpace(cfgReg.GetString("core.daemon.data_dir", "")); dbDir != "" {
		if clean := filepath.Clean(dbDir); clean != filepath.Clean(opt.dataDir) {
			log.Printf("[homed] data dir: 以数据库 core.daemon.data_dir 为准 → %s（原 -data %s）", clean, opt.dataDir)
		}
		opt.dataDir = filepath.Clean(dbDir)
	}
	if opt.cliSocket == "" || !filepath.IsAbs(opt.cliSocket) {
		opt.cliSocket = filepath.Join(opt.dataDir, "cli.sock")
	}
	// LLM 配置写前留档（config_set 写 core.llm.* 前自动快照），guard 恢复用基线
	// ★ 锚点：本块必须在 SetLLMSnapshotFile / ToConfig 之前（两者都会读 core.llm.*）。
	// ========================================================================
	// ★ 安全模块必须在**任何读 core.llm.* 的操作之前**装配。
	//
	// 实测故障（2026-10-16，自己引入的）：安全块原先放在 ToConfig() 之后。
	// 于是 ToConfig() 在**没有主密钥**的情况下读 core.llm.*.api_key ——
	// 密文解不开，key 变成空串，而 initLLMProviders(cfg,...) 用的正是这份
	// 早期快照 ⇒ 所有 LLM 调用 401 invalid api key。
	//
	// 症状极具误导性：日志里只有一行 `Dump 解密 ... 失败: security: 无可用主密钥`，
	// 而 401 看起来像“key 配错了”。实际是**启动顺序**问题。
	//
	// 约束：安全块只依赖 cfgReg（不依赖 cfg），所以可以安全前移。
	// ========================================================================
	// ★ 安全模块的装配已迁到 internal/kernelmod/security（正式内核模块）。
	//
	// 为什么仍在这里调：密钥与变量库必须早于 `cfgReg.ToConfig()`
	// （否则读 core.llm.* 的密文解不开 ⇒ 全部 provider 401 invalid api key，
	// 症状极具误导性）。那是**时序**要求，依赖图表达不了 —— 与 Group 0
	// 其他动作同性质。模块侧提供幂等的 Prepare()，调度时 Init 会复用同一实例。
	if _, serr := securitymod.Prepare(opt.dataDir, cfgReg); serr != nil {
		log.Fatalf("[homed] security module init failed (refusing to start in degraded mode): %v", serr)
	}

	// ========================================================================
	// ★ 内核装配：公共结构体 + 模块注册表（用户设计）
	//
	// Group 0（上面到这里的部分：平台门/心跳/日志/配置库/安全块）是
	// **副作用顺序**要求，不进调度器 —— 依赖图表达不了"此动作必须在
	// 那个动作之前生效"（图表达的是"我产出数据给他"）。见 kmod_group0 说明
	// 与 bootstrap_order_test.go 的结构性判据。
	//
	// 从 cfg 起全部改为**模块自声明依赖 + 注册表 + BFS 分层**：
	//   · 公共数据统一在 kernelboot.KernelState（不再散落各包包级变量）
	//   · 每个模块在 internal/kernelmod/<name>/ 里**自己声明** Requires/Provides
	//   · 注册表是乱序的（顺序无意义，仅供查询"内核有哪些模块"）
	ks := kernelboot.NewKernelState()
	ks.CfgReg = cfgReg
	ks.DataDir = opt.dataDir
	ks.AgentWorkDir = agentWorkDir
	ks.CLISocket = opt.cliSocket
	ks.HTTPAddr = opt.httpAddr
	ks.Ctx, ks.Stop = context.WithCancel(context.Background())

	// ★ Group 0 的最后两步（读 core.llm.*，必须在安全块之后）：
	//   它们归 config 模块的产物路径，但**执行点**在这里 —— 阶段依赖。
	cfgReg.SetLLMSnapshotFile(filepath.Join(opt.dataDir, "llm_snapshot.json"))
	ks.Cfg = cfgReg.ToConfig()

	// ★ 数据目录一致性检查（2026-10-18 接线）。
	//
	// 内核有**两处**数据目录来源，且不等价：
	//   -data <dir>              ← systemd 单元，决定 ks.DataDir
	//   core.daemon.data_dir     ← 数据库，决定 ks.Cfg.Daemon.DataDir
	//
	// 不一致时数据会**分裂**：图记忆（memory/graph.db）在 -data 那个目录，
	// 而文本/知识/文档在 DB 那个目录 ⇒ 检索跨目录、备份会漏。
	// 生产当前两者一致，所以从没暴露；但 WebUI 允许改 core.daemon.data_dir
	// 而**没有任何提示**，用户改完重启就会发现"数据丢了"。
	//
	// 这里只告警 + 给出可直接粘贴的修复命令，**不改任何东西**：
	// 静默改 systemd 单元比不提示更危险（改错了服务起不来，而用户不知道
	// 是谁改的）。修复由人执行 —— 命令里已内联 sed 替换。
	if cdir := ks.Cfg.Daemon.DataDir; cdir != "" && cdir != opt.dataDir {
		st := systemd.State{RuntimeDataDir: opt.dataDir, ConfigDataDir: cdir}
		log.Printf("[homed] WARNING: data directories mismatch (data will split):")
		log.Printf("[homed]    -data        = %s (graph memory, plugin Settings.DataDir)", opt.dataDir)
		log.Printf("[homed]    DB data_dir  = %s (text/knowledge/doc)", cdir)
		log.Printf("[homed]    fix command (do not hand-edit the unit; use a drop-in to override ExecStart in production):")
		log.Printf("[homed]    %s", st.SyncCommand(cdir))
	}

	// 装配全部内核模块（缺依赖/环/重名 ⇒ 这里就报错，不必等运行期）。
	//
	// ★ 插件调度已**归位到内核模块**（`internal/kernelmod/pluginhost`，
	// 用户方案 A）：它负责扫元数据缓存、按三形态分流、按依赖时机拉起。
	// main 这里不再有插件调度逻辑 —— 只保留装配后的一点点收尾。
	// ★★ failback 的插件**白名单必须在装配之前**设置。
	//
	// 原因：插件拉起已归位到 plugin-host 模块，而它在**装配期**执行
	// ⇒ 白名单若在装配后才设，插件早已按全量拉起了（受限启动就失效了）。
	//
	// 这是把调度搬进模块后引入的**新时序要求** —— 原先调度在装配后，
	// 所以把 allowlist 放在装配后也无害。迁移时必须重新核对这类顺序。
	//
	// 注意：白名单要的是 plugin.Registry，而它是 stage-host 的产物
	// ⇒ 装配前拿不到。所以这里传 nil 的 registry 只算白名单名（logged），
	// 真正的 SetLoadAllowlist 由装配期的钩子在 registry 就绪后执行。
	pendingAllowlist := resolveFailbackAllowlist(cfgReg, opt.boot, opt.dataDir)

	// 装配全部内核模块（缺依赖/环/重名 ⇒ 这里就报错，不必等运行期）。
	//
	// ★ 插件调度已**归位到内核模块**（`internal/kernelmod/pluginhost`，
	// 用户方案 A）：它负责扫元数据缓存、按三形态分流、按依赖时机拉起。
	// main 这里不再有插件调度逻辑 —— 只保留装配后的一点点收尾。
	if _, err := RunKernelModules(ks, kernelAssembleHooks{
		// 在 stage-host 就绪、插件真正拉起**之前**把白名单设上。
		OnLayerDone: func(_ int, _ []kernelboot.Module) {
			if pendingAllowlist != nil && ks.PluginRegistry != nil {
				ks.PluginRegistry.SetLoadAllowlist(pendingAllowlist)
			}
		},
	}); err != nil {
		log.Fatalf("[homed] kernel assembly failed: %v", err)
	}
	defer ks.CloseAll()

	// ★ 挂载内核侧升级执行器（第 3 项：执行权归内核）。
	//
	// 为何在这里而不是在 update 模块的 Init 里：update 模块必须在第 0 层
	// （回滚判定要早于一切模块），而事件总线由 kernelapi 层产出 ——
	// 第 0 层时它还不存在。所以分两段：Init 做回滚与探测，此处订阅
	// update_request 并接管“scope=kernel”的执行。
	//
	// 为何执行权必须在内核而不是插件：插件可以被 disable / auto_start:false，
	// 而升级内核这件事不能依赖一个可被禁用的部件。
	// ★ 注入事件总线（升级进度/结果都要发出去）。
	//
	// 注入后 internal/update 以内核身份发事件，**插件不再需要持有 *events.Bus**
	// —— 那正是“安全监控可被绕过”的根源（拿裸总线就能伪造任意事件）。
	updatepkg.SetEventBus(ks.EventBus)
	defer updatepkg.AttachExecutor(ks.EventBus, ks.DataDir)()

	cfg := ks.Cfg
	luaVM := ks.LuaVM
	sup := ks.Supervisor
	trk := ks.Tracker
	ctx := ks.Ctx
	pluginReg := ks.PluginRegistry
	agent := ks.Agent

	// 候选消费管线：常驻 goroutine，把 eventbus 的 memory_candidate 写进
	// 文本记忆并喂给蒸馏管线。原顺序里它在 textmem 之后、provider 之前。
	startMemoryCandidateConsumer(ctx, ks.IO, ks.TextMemory, ks.Memory.DB, ks.Memory.Distiller)

	// L1 failback：受限 worker 启动即跑恢复梯子，结果以退出码交回 guard。
	if opt.boot == "failback" {
		runFailbackRecovery(opt.dataDir, cfgReg, luaVM, ks.LLMManager, ks.BaseAPIKey)
	}

	// ---- 插件已完成调度（plugin-host 模块做的事）----
	//
	// 装配期 plugin-host 已：
	//   ① 扫目录建元数据缓存（ks.PluginCatalog）
	//   ② 按三形态分流（自动启动 / 不自动启动 / 禁用）
	//   ③ 拉起"声明设施已就绪"的插件；挂起"等更晚设施（如 agent）"的
	//      （挂起项经 syncbus 在 agent 就绪时自动拉起，见 pluginhost/waiter.go）
	if cat, ok := ks.PluginCatalog.(*pluginhost.PluginCatalog); ok {
		log.Printf("[homed] plugin scheduling done: %d metadata cached, %d loaded",
			cat.Size(), len(pluginReg.ListLoadedPlugins()))
	} else {
		log.Printf("[homed] plugin scheduling done: %d loaded", len(pluginReg.ListLoadedPlugins()))
	}

	// 插件加载完成后再回收空闲页：大值（如老版聊天记录）可能在这一步被搬走/删除，
	// 而 SQLite 的 DELETE 不会缩小文件。
	compactConfigDB(cfgReg)

	stopRuntime := startAgentRuntime(cfgReg, pluginReg, agent, logDir, ctx)
	defer stopRuntime()

	_, closeIPC := startIPCServer(opt.dataDir, opt.boot, agent)
	defer closeIPC()

	restartCh := startSupervisorRuntime(sup, trk, agent)

	// 内核控制通道（WebUI 的重启/关机按钮走这里）。
	//
	// ★ 必须在插件加载**之前**注入：WebUI 在 Start 里取
	//   s.KernelControl()，注入晚了它拿到 nil ⇒ 按钮永远报"不可用"。
	//   装配顺序在模块化内核里由依赖图决定，而本行位于 agent 模块之后、
	//   plugin-host 拉起插件之前 —— 正是需要的位置。
	pluginReg.SetKernelControl(&kernelControlImpl{restartCh: restartCh})

	log.Printf("[homed] main agent started, model=%s base=%s sources=%d adapters=%d",
		cfg.LLM.Model, cfg.LLM.BaseURL, len(cfg.LLM.Sources), len(luaVM.ListAdapters()))
	log.Printf("[homed] kernel ready, waiting for plugin IO...")

	// ---- 等待退出信号 ----
	//
	// 心跳已在 main 早期启动（见文件头部 startHeartbeatEarly），此处不再重复启动。
	// stopKernelModules：热更新路径（os.Exit 跳过 defer）下也要跑模块关闭。
	// CloseAll 幂等（第二次 Closers 已空）⇒ 与 defer 并存是安全的。
	waitForShutdown(ctx, opt.dataDir, restartCh, stopHeartbeatEarly, pluginReg, trk, cfgReg, sup,
		func() {
			if err := ks.CloseAll(); err != nil {
				log.Printf("[homed] module shutdown: %v", err)
			}
		})
}
