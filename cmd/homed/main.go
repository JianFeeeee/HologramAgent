package main

import (
	"context"
	"log"
	"path/filepath"

	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod/pluginhost"
	"github.com/JianFeeeee/HologramAgent/internal/meta"
	// ★ 内置插件统一走 internal/plugins 的清单（all.go），**不要**在这里
	// 单独导入某个插件：
	//   · 重复导入虽无害（同一包只 init 一次），但会让 all.go 不再是唯一清单
	//     —— 按它盘点的工具/文档会漏项（kbtree 曾因此长期游离在清单之外）；
	//   · 想增删内置插件请改 all.go。
	_ "github.com/JianFeeeee/HologramAgent/internal/plugins"
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"github.com/JianFeeeee/HologramAgent/internal/systemd"

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

	// ---- 配置中心（SQLite 持久化，唯一配置源） ----
	//
	// 必须早于 initMemoryStack：蒸馏的生成侧 provider 由配置决定
	//（core.memory.distill.generation.provider），且要在 distiller.Start()
	// 之前注入 —— 否则第一个 tick 会白跑一次产出为 0 的 jieba 路。
	cfgReg := internalConfig.NewConfigRegistry(filepath.Join(opt.dataDir, "config.db"))
	defer cfgReg.Close()
	cfgReg.SeedDefaults(opt.dataDir)
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
	if cfgReg.GetBool("core.security.enabled", true) {
		// ★ 先收敛数据目录与敏感文件权限（幂等），再读密钥。
		//
		// 顺序有意为之：内嵌主密钥把密钥放进了 0755 的二进制里，
		// 而 config.db 出厂是 0644 —— 「能读二进制就能解密数据库」。
		// 收紧到 0600/0750 是本机缓解手段，必须在服务开始读写之前生效。
		//
		// 失败不 Fatal（权限问题不该让服务不可用），但会逐条响亮记录。
		if hr := security.HardenDataDir(opt.dataDir); len(hr.Changed) > 0 {
			log.Printf("[homed] hardened %d file permissions (necessary mitigation for embedded-key scenario): %v",
				len(hr.Changed), hr.Changed)
		}
		key, keySrc, kerr := security.LoadOrCreateKey(opt.dataDir)
		if kerr != nil {
			log.Fatalf("[homed] security key init failed (refusing to start in degraded mode): %v", kerr)
		}
		// E-1：若存在密钥环，装入历史钥（只读旧密文；当前钥仍由
		// systemd credential/env/security.key 提供）。密钥环损坏或 current 不一致
		// 必须响亮失败，不能猜一把钥继续启动。
		oldKeys, rerr := security.LoadKeyring(opt.dataDir, key)
		if rerr != nil {
			log.Fatalf("[homed] security keyring load failed (refusing to silently pick wrong key): %v", rerr)
		}
		// ★ 内嵌钥上台时，磁盘钥必须继续参与**解密**。
		//
		// 否则一台用磁盘钥加密过的老机器升级到内嵌了另一把钥的二进制后，
		// 旧密文会全部解不开 ⇒ 服务起不来；用户此时若选择"重建库"，
		// 凭据就**一次性全丢** —— 那恰好与内嵌的初衷（防丢）相反。
		//
		// 与 E-1 同一套语义：当前钥负责 Seal，旧钥只参与 Open。
		if extra := security.AdditionalDecryptKeys(opt.dataDir, key); len(extra) > 0 {
			oldKeys = append(oldKeys, extra...)
		}
		if len(oldKeys) > 0 {
			log.Printf("[homed] security module loaded %d historical/extra key(s) (E-1 smooth rotation + embedded migration)", len(oldKeys))
		}
		if _, serr := security.InitGlobal(security.Options{
			Path:    filepath.Join(opt.dataDir, "security", "variables.json"),
			KeyFn:   func() ([]byte, error) { return key, nil },
			OldKeys: oldKeys,
			Enabled: true,
		}); serr != nil {
			log.Fatalf("[homed] security variable store load failed (will not silently rebuild): %v", serr)
		}
		// 配置落盘用的子密钥（域隔离：与别名密钥不共用）。
		// 必须早于 EncryptExistingSecrets，后者依赖它。
		if _, cerr := security.InitConfigCodecWithOldKeys(key, oldKeys); cerr != nil {
			log.Fatalf("[homed] config-encryption subkey derivation failed: %v", cerr)
		}
		// 把库里仍是明文的敏感项就地加密（幂等；升级路径必须跑）。
		if n, merr := cfgReg.EncryptExistingSecrets(); merr != nil {
			// 不 Fatal：库可能是只读/被锁，而“部分为明文”不等于服务不可用。
			// 但要**响亮告警**：这是用户必须知道的安全事实。
			log.Printf("[homed] existing sensitive config encryption incomplete (some entries still plaintext): %v", merr)
		} else if n > 0 {
			log.Printf("[homed] encrypted %d existing sensitive config entries", n)
		}
		log.Printf("[homed] security module enabled (key source %s, %d variables, config-at-rest encryption on)",
			keySrc, security.Global().Count())

		// ★ E-2：备份跨机器恢复的诊断。
		//
		// 场景：把 config.db 备份恢复到另一台机器，而主密钥没一起搬过来
		// ⇒ 库里密文是**旧机器密钥**加的，当前钥解不开。
		// 此前这种状态只在“读某一条配置”时才暴露成“某个功能坏了”，
		// 用户看到的是症状而不是原因。这里一次性说清事实与修复动作。
		if okN, badN, aerr := cfgReg.AuditSecrets(); aerr == nil && badN > 0 {
			log.Printf("[homed] WARNING: %d ciphertext entries cannot be decrypted with current master key (%d decryptable) — " +
				"most common cause: this config.db came from **another machine / another key**. " +
				"Fix: bring the original machine's key over (systemd credential / %s / " +
				"%s), or start once with the old key and run the re-encryption migration.",
				badN, okN, security.EnvKeyName, security.KeyringFileName)
		}
	} else {
		log.Printf("[homed] security module explicitly disabled (core.security.enabled=false) — data at rest is plaintext and relay is unprotected")
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
