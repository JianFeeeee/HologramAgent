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

	agentCore "github.com/JianFeeeee/HologramAgent/internal/agent/core"
	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/ipc"
	logpkg "github.com/JianFeeeee/HologramAgent/internal/log"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	"github.com/JianFeeeee/HologramAgent/internal/memory/pipeline"
	"github.com/JianFeeeee/HologramAgent/internal/memory/social"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	"github.com/JianFeeeee/HologramAgent/internal/recovery"
	safeclose "github.com/JianFeeeee/HologramAgent/internal/safeclose"
	"github.com/JianFeeeee/HologramAgent/internal/supervisor"
	"github.com/JianFeeeee/HologramAgent/internal/tracker"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

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
	writeHB := func() {
		// ★ 若有待发的控制指令，**整文件重写**（保留 t= 行 + cmd= 行）。
		//
		// 为什么不能继续 append：guard 消费指令后会重写文件清除 cmd 行；
		// 若这里仍 append，历史 cmd 行会越积越多（每次读到的还是第一条）。
		// 无指令时保持原来的 append 语义（历史行为不变，只刷 mtime）。
		if content := nextHeartbeatWrite(hbPath, time.Now().Unix()); content != "" {
			if err := os.WriteFile(hbPath, []byte(content), 0644); err == nil {
				return
			}
		}
		// ★ 心跳文件必须**滚动**，不能无限 append（2026-10-17 实测：14.6MB）。
		//
		// heartbeat 的唯一职责是让 guard 看到 mtime 变新（见下面的注释）。
		// 历史行**没有任何读者** —— 唯一读内容的是 consumeHeartbeatCmd，
		// 而它只关心 cmd= 行。所以累加历史行纯属浪费：
		//   · 每 5s 12 字节 ⇒ 约 70 天长到 14.6MB（生产实测就是这个量级）
		//   · consumeHeartbeatCmd 要 ReadFile 整个文件 ⇒ 白白读 14.6MB
		//   · 长期跑下去还会一直涨（磁盘与读取成本都随天数线性增长）
		//
		// 改成截断重写：只保留**当前**这一行。mtime 语义完全不变
		// （文件仍在每次心跳被写），而体积恒定在十几字节。
		//
		// 用 O_TRUNC 而不是 O_APPEND：内容不再需要累积。
		if f, err := os.OpenFile(hbPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644); err == nil {
			fmt.Fprintf(f, "t=%d\n", time.Now().Unix())
			f.Close()
		}
	}
	// ★ 首次写入必须**同步**（返回前落盘），不能丢进 goroutine：
	//
	// heartbeat 文件在 worker 侧的唯一职责是「让 guard 看到 mtime 变新」。
	// 停机 >heartbeatTimeout 后重启时，文件 mtime 停在上一代 worker（已过期）；
	// 若首次写是异步的，它可能与重型初始化（多模态空间/知识库重建，冷启动
	// 5–8s）竞争调度，迟迟不落盘 —— guard 的 ticker 先到、拿旧 mtime 判 stale。
	// （2026-10-12 生产部署实测：6 连杀 + failback 降级。）
	// 同步首写保证：main() 继续往下做任何慢事之前，mtime 已经是新的。
	writeHB()
	hbStop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
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

	// ★★ 必须幂等（2026-10-17 实测 panic：close of closed channel）。
	//
	// 调用点有**两处**：
	//   · main.go:70  `defer stopHeartbeatEarly()`
	//   · bootstrap.go waitForShutdown 里的 `stopHeartbeat()`
	// 直接 `close(hbStop)` ⇒ 第二次关闭**必然 panic**：
	//
	//     panic: close of closed channel
	//     goroutine 1 [running]:
	//     main.startHeartbeat.func3()  bootstrap.go:259
	//     main.main()                  main.go:293
	//
	// 而且它发生在**退出路径**上：进程已经做完清理工作，
	// 却在最后一步崩溃 ⇒ 退出码变成 2（panic），
	// systemd/guard 会把它当成"异常退出"而不是"正常停止"，
	// 进而触发不必要的重启/告警。实测：SIGTERM 后退出但日志尾部是 panic 栈。
	//
	// 修法：`sync.Once` 保护。stop 函数做成幂等是本仓退出路径的通用要求
	// （它们往往既被 defer 调、又被显式调）。
	// 用 safeclose 统一入口（本仓 close 纪律：见 internal/safeclose）。
	return safeclose.SafeFunc(hbStop)
}

// waitForShutdown 阻塞至 SIGINT/SIGTERM 或 supervisor 请求重启，然后按原 main 的顺序清理，需要重建时以退出码交回 guard。
//
// 本函数体是 main() 里对应启动阶段的整块平移：语句、日志文本、错误语义不变，
// 只把「*dataDir」变成参数、把 defer 变成由调用点注册的 cleanup。
func waitForShutdown(ctx context.Context, dataDir string, restartCh chan struct{}, stopHeartbeat func(),
	pluginReg *plugin.Registry, trk *tracker.Tracker, cfgReg *internalConfig.ConfigRegistry, sup *supervisor.Daemon,
	stopKernelModules func()) {
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

	// ★★ 必须显式跑一次模块关闭，**不能只靠 defer**（2026-10-17 实测缺陷）。
	//
	// 两条退出路径原先**不对称**：
	//   路径 A（SIGTERM）：return 到 main → defer ks.CloseAll() ✓
	//   路径 B（exit 42）：下面的 os.Exit(42) **跳过所有 defer** ✗
	//
	// 而路径 B 是**每次热更新都走**的。后果是这些写者从不 flush：
	//   · docStore.Stop()  → flush()     文档索引可能陈旧
	//   · distiller.Stop() → flush()     待蒸馏规则丢失
	//   · textMem.Stop()   → Close()     文本记忆未落盘
	//   · luaVM/multimodal → 资源未释放（靠进程退出兜底）
	//
	// ⇒ 显式调用（CloseAll 幂等：第二次执行时 Closers 已空，无副作用）。
	stopKernelModules()
	log.Printf("[homed] stopped")

	if restartRequested {
		os.Exit(exitRestartRequested)
	}
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
		log.Printf("[homed] warning: config DB compaction failed: %v", err)
		return
	}
	if !done {
		return
	}
	after := before
	if st, err := os.Stat(cfgReg.DBPath()); err == nil {
		after = st.Size()
	}
	log.Printf("[homed] config DB compacted: %d -> %d bytes", before, after)
}
