package pluginhost

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/supervisor"
)

// FacPlugins 是本模块提供的设施名。
//
// 意义：**"谁是最后一批被拉起的插件"** —— 声明 `plugins` 的插件要等所有
// 自动启动的插件都起完才起（如"插件总览"类插件）。
const FacPlugins = "plugins"

// Module 是插件宿主：扫元数据、按形态分流、按依赖时机拉起。
type Module struct{}

func (Module) Name() string { return "plugin-host" }

// Requires：插件**加载**所需的全部前置 —— **刻意不含 agent**。
//
// ★ 这是本模块存在的关键价值（用户方案 A 的核心收益）：
// 原先 wirePluginSDK 在 agent 模块里 ⇒ 插件加载被排在 agent 之后，
// 而 agent 层实测 **35 秒**（context dense backfill 750 events + 重建索引）。
//
// 而插件真正需要的只是 stage-host（Registry + StageHost）与几个底层设施。
// agent 相关的 StatusProvider 改为**后补注入**（见
// agent 模块），插件在 Start 阶段不用它们（唯一的 healthcheck 是 nil-guarded）。
func (Module) Requires() []string {
	return []string{
		kernelmod.FacConfig, kernelmod.FacKernelIO, kernelmod.FacMemory,
		kernelmod.FacLua, kernelmod.FacSupervisor, kernelmod.FacTracker,
		kernelmod.FacTextMem, kernelmod.FacDocStore, kernelmod.FacKnowledge,
		kernelmod.FacStageHost,
	}
}

func (Module) Provides() []string { return []string{FacPlugins} }

func (Module) Init(ks *kernelboot.KernelState) error {
	// ── 前置检查（用户要求："nil 加读取前检查"）──
	if ks.PluginRegistry == nil || ks.StageHost == nil {
		return fmt.Errorf("plugin-host：ks.PluginRegistry/StageHost 为 nil —— " +
			"stage-host 模块必须先完成（插件加载的硬前置）")
	}
	if ks.Cfg == nil {
		return fmt.Errorf("plugin-host：ks.Cfg 为 nil —— config 模块必须先完成")
	}
	if ks.CfgReg == nil || ks.DataDir == "" {
		return fmt.Errorf("plugin-host：CfgReg/DataDir 为 nil/空 —— Group 0 必须先完成")
	}

	// ── ① 注入插件 SDK 的**底层依赖**（不含 agent 相关的两项）──
	injectBaseDeps(ks)

	// ── ② 扫一次目录 → 元数据缓存 ──
	dir := ks.Cfg.Plugin.Dir
	os.MkdirAll(dir, 0o755)
	catalog, noManifest := scanCatalog(dir, ks.CfgReg.IsPluginDisabled)
	ks.PluginCatalog = catalog
	// ★ 措辞必须准确（我第一版写成"跳过 N 个非插件目录" ⇒ **误导**）：
	// 那些目录大多是**内置插件**（编译期工厂，本就没有 plugin.json，
	// 由下面的 LoadSkipping 从工厂表加载）。只有既无 manifest
	// 又无工厂的才是真的非插件目录。
	//
	// 这条日志的价值在于：新增外部插件若忘写 plugin.json，会出现在这里
	// （那时"无 manifest ⇒ 只能走工厂 ⇒ 找不到 ⇒ 不加载"，可据此发现）。
	if len(noManifest) > 0 {
		log.Printf("[pluginhost] %d directories have no plugin.json (normal for built-in plugins, loaded by compile-time factories): %v",
			len(noManifest), noManifest)
	}
	log.Printf("[pluginhost] external plugin metadata cache ready: %d (dir %s)",
		catalog.Size(), dir)

	// ── ③ 按**三形态**分流（用户设计要求）──
	groups := classify(catalog)
	log.Printf("[pluginhost] form routing: auto-start %d / declared deps %d / "+
		"enabled-but-not-auto-start %d / persistently disabled %d",
		len(groups.autoStart), len(groups.declared),
		len(groups.manual), len(groups.disabled))

	// ── ④ 拉起：**全部自动启动的插件都交给 Scheduler** ──
	//
	// ★★ 我第一版在这里犯了一个**违反硬约束**的错，值得完整记录：
	//
	// 第一版按"本模块依赖是否覆盖插件声明"把插件分成 launchNow / waitLater，
	// 并在 Init 里**直接**把 launchNow 全加载了。
	// 但**未声明依赖**的插件，`RequiresFacilities` 是空 ⇒ "本模块依赖覆盖空集"
	// 恒真 ⇒ 它们被归入 launchNow ⇒ **在第 4 层就被加载**。
	//
	// 而 §2.6.5 的硬约束是：
	//
	//     未声明 ⇒ 启动时机**逐字不变**（内核**完全**初始化完成后）
	//
	// 实测证据（planA4.log）：内置插件在 `21:54:37`（第 4 层）被加载，
	// 而内核装配完成是 `21:55:12` —— **早了 35 秒**。
	// 约束原文写着"静默改变启动时机 = 静默引入竞态"，
	// 42 个存量插件里任何一个若假设"Start 时 llm 一定已就绪"就会中招。
	//
	// ⇒ 修法：**不再自己判时机**，全部交给 `kernelboot.Scheduler`
	// ——它已正确实现 §2.6.5（空声明 ⇒ 等 `KernelComplete()`）与
	// §2.6.6（`!AutoStart` ⇒ 不自动拉起），且有完整判据。
	// 我只需要提供"拉起一个插件"的动作。
	//
	// 这样声明的插件仍在依赖就绪时尽早启动（本步的核心收益不变），
	// 而未声明的**恢复原行为**（等内核完全初始化）。
	var bus kernelboot.SyncState
	var busConcrete *kernelboot.SyncBus // 保留具体类型以便订阅 OnUpdate
	if ks.SyncBus != nil {
		bus = ks.SyncBus
		busConcrete = ks.SyncBus
	} else {
		// ★★ typed-nil 陷阱：`var nb *SyncBus; var i SyncState = nb`
		// ⇒ i **非 nil** ⇒ 方法里解引用空指针 panic（实测复现）。
		// 所以这里必须显式判 `!= nil` 再赋给接口变量。
		log.Printf("[pluginhost] warning: SyncBus is nil -- non-production assembly path, " +
			"退化为立即加载（依赖未就绪的插件在 Start 时自行处理）")
		fallback := kernelboot.NewSyncBus()
		fallback.MarkKernelComplete()
		bus = fallback
		busConcrete = fallback
	}

	sched := kernelboot.NewScheduler(bus, func(spec kernelboot.PluginSpec) error {
		return ks.PluginRegistry.LoadOneDir(filepath.Join(dir, spec.Name), spec.Name)
	})
	sched.SetLaunchErrorHandler(func(spec kernelboot.PluginSpec, err error) {
		log.Printf("[pluginhost] warning: failed to start %s: %v", spec.Name, err)
	})
	ks.PluginScheduler = sched

	specs := buildSpecs(catalog, groups)
	// Submit 返回"此刻就能拉起"的：
	//   · 声明了依赖且已全就绪  ⇒ 立即拉起（**本步的核心收益**）
	//   · 未声明的              ⇒ 不进返回列表（等 KernelComplete，§2.6.5）
	//   · !AutoStart            ⇒ 不进（§2.6.6）
	// 同时 Submit 会做拼错校验（声明的设施名内核从未注册 ⇒ 立即报错）。
	for _, n := range sched.Submit(specs) {
		log.Printf("[pluginhost] plugin started (dependencies ready, before assembly finished): %s", n)
	}

	// ── ⑤ 加载**内置插件**（编译期工厂，**没有 plugin.json**）──
	//
	// ★★ 这一步是我第一版漏掉的（实测：19 个内置插件全部没加载 ——
	// webui / pluginmgr / cli / files / timer / mcp …，只剩 2 个外部插件起）。
	//
	// 原因：上面的 catalog 是**扫目录读 plugin.json** 得来的，
	// 而内置插件是**编译期注册的工厂**（`plugin.RegisterFactory` 在 init() 里），
	// 目录里可能连 plugin.json 都没有 ⇒ 表里根本没有它们。
	//
	// 走 `LoadSkipping(dir, skip)`：它内部既扫目录（外部插件）
	// 也遍历工厂（内置插件），是**唯一完整**的加载入口。
	// skip 集合 = 已由本模块调度处理的那些（按三形态分流的），
	// 避免重复加载。
	// ── ⑤ 其余插件（未声明的 + 内置）→ **等内核完全初始化**后加载 ──
	//
	// ★ 为什么不能在这里直接 LoadSkipping（我第一版的第二个错）：
	// plugin-host 在第 4 层，而 agent 与它**并行** ⇒ 此刻内核**尚未**
	// 完全初始化。直接加载同样违反 §2.6.5 的"未声明 ⇒ 逐字不变"。
	//
	// ⇒ 挂到 syncbus 的 `KernelComplete` 上（内核**推**状态，不轮询）。
	//
	// skip = 已由 Scheduler 接管的（declared ∪ manual ∪ disabled）。
	// 不在 skip 里的：未声明的外部插件 + **内置插件**（编译期工厂，
	// 不在 catalog 里，只有 LoadSkipping 的工厂分支能加载它们）。
	skip := buildSkipSet(catalog, groups)
	var loadOnce sync.Once
	loadRest := func(st kernelboot.SyncState) {
		if !st.KernelComplete() {
			return
		}
		loadOnce.Do(func() {
			if err := ks.PluginRegistry.LoadSkipping(dir, skip); err != nil {
				log.Printf("[pluginhost] warning: loading built-in/undeclared plugin: %v", err)
			}
			ks.PluginsReady = true
			log.Printf("[pluginhost] kernel fully initialized -> loaded remaining plugins (%d built-in)",
				len(noManifest))
		})
	}
	// 订阅 + 立即评估一次（若 bus 已 complete，比如非生产路径的退化 bus）
	// ★ 订阅用**具体类型**的 SyncBus（SyncState 是**只读接口**，
	// 故意没有 OnUpdate —— 设计 §2.6.3 "插件管理器对 syncbus 只读"）。
	// 本模块是内核模块，本就持有具体类型，无需放宽接口。
	if busConcrete != nil {
		busConcrete.OnUpdate(loadRest)
	}
	loadRest(bus)

	log.Printf("[pluginhost] this stage: %d with declared deps handled by scheduler "+
		"(those already ready were started), %d undeclared + %d built-in wait for kernel full init",
		len(groups.declared), len(groups.autoStart)-len(groups.declared), len(noManifest))
	return nil
}

func init() { kernelmod.Register(Module{}) }

// buildSpecs 把"自动启动"的插件元数据转成调度器声明。
//
// ## ★ 为什么**未声明**的插件也要进来（我第一版漏了这个语义）
//
// 第一版把"未声明依赖"的插件排除在调度器之外、在 Init 里直接加载
// ⇒ 它们在第 4 层（agent 并行期间）就起了，而 §2.6.5 要求"等内核
// **完全**初始化"。实测后果：`healthcheck` 的 `s.Status()` 为 nil
// ⇒ **静默少注册一个工具** `healthcheck_kernel`（86 vs 87，无任何报错）。
//
// ⇒ 全部自动启动的插件都交给调度器；调度器对空声明返回
// `KernelComplete()`（§2.6.5），正是原行为。
func buildSpecs(c *PluginCatalog, g formGroups) []kernelboot.PluginSpec {
	var out []kernelboot.PluginSpec
	for _, n := range g.autoStart {
		m, ok := c.Get(n)
		if !ok {
			continue
		}
		out = append(out, kernelboot.PluginSpec{
			Name:               m.Name,
			RequiresFacilities: m.RequiresFacilities,
			AutoStart:          m.AutoStart,
		})
	}
	return out
}

// buildSkipSet 计算"不能让全量 LoadSkipping 碰"的插件名集合。
//
// ## ★ 为什么抽成独立函数（判据要求）
//
// 初版把这段逻辑内联在 Init 里 ⇒ 判据只能查源码字符串 ⇒
// 删掉代码但注释仍在时**判据不咬**（实测）。
// 抽出来后可**真的调用它**做行为断言（TestBuiltinsAreNotSkipped）。
//
// ## 集合语义（三形态 + 内置插件的边界）
//
//	② auto_start=false ⇒ 必须在集合里（否则会被全量加载 = 违背语义）
//	③ disabled        ⇒ 在集合里（显式跳过；registry 还有禁用门兜底）
//	① 声明的          ⇒ 由本模块按依赖时机拉起 ⇒ 必须在集合里（防重复）
//	**内置插件**      ⇒ **不在**集合里 ⇒ 走 LoadSkipping 的工厂分支被加载
//	                    ★ 这正是我第一版漏掉的关键：内置插件没有 plugin.json，
//	                      不在 catalog 里，只有 LoadSkipping 的工厂分支能加载它们。
func buildSkipSet(c *PluginCatalog, g formGroups) map[string]bool {
	skip := map[string]bool{}
	// ★★ 原则：**catalog 里登记过的插件，一律不交给 LoadSkipping**。
	//
	// ## 为什么不是"只跳过 declared/manual/disabled"（我第一版就是这么写的）
	//
	// 那会让**未声明的外部插件**同时被两条路加载：
	//   · 调度器（它管未声明的，等 KernelComplete 后拉起）
	//   · LoadSkipping（未声明的**不在** skip 里 ⇒ 也加载）
	//
	// 实测（变异 F，把 LoadSkipping 挪回 Init）：`healthcheck` 被 loaded **两次**，
	// 工具数 87 → **161**（重复注册）。planA5 侥幸未触发，只因测试目录里
	// 那 3 个外部插件恰好都声明了依赖或不自启 —— **不能依赖这种巧合**。
	//
	// ⇒ 改成**按构造**消除：catalog 里的全部（三形态之一）都由本模块/调度器
	// 负责；LoadSkipping 只处理 catalog **之外**的，也就是那些
	// **没有 plugin.json 的内置插件**（编译期工厂，只有它能加载）。
	//
	// 这样两条路**按定义**不相交。
	for _, n := range c.Names() {
		skip[n] = true
	}
	// 保底：三形态里若有 catalog 未收录的（理论上不会），也别漏
	for _, n := range g.autoStart {
		skip[n] = true
	}
	for _, n := range g.manual {
		skip[n] = true
	}
	for _, n := range g.disabled {
		skip[n] = true
	}
	return skip
}

// injectBaseDeps 注入插件 SDK 的底层依赖（原 wirePluginSDK 的 7 项）。
//
// ★ 这里**刻意不做** SetStatusProvider —— 它依赖 agent，
// 由 agent 模块**后补注入**（见其注释）。这不是遗漏，是有意的分层：
// 让插件加载不必等 agent 那 35 秒。
func injectBaseDeps(ks *kernelboot.KernelState) {
	reg := ks.PluginRegistry
	reg.SetLuaVM(ks.LuaVM)
	reg.SetBaseAPIKey(ks.BaseAPIKey)
	if ks.Supervisor != nil {
		reg.SetSupervisor(supervisor.NewSDKAdapter(ks.Supervisor))
	}
	reg.SetTracker(ks.Tracker)
	reg.SetConfig(ks.Cfg)
	reg.SetStageHost(ks.StageHost)

	if ks.Memory != nil {
		reg.SetIndexer(ks.Memory.Indexer)
	}
}

// formGroups 是三形态分流结果。
//
// ★ 用户明确要求的三种形态（§2.6.6）：
//
//	① autoStart ：自动启动（缺省/true）
//	② manual    ：**启用但不自动启动**（auto_start=false）⇒ 等显式启用
//	③ disabled  ：禁用（disabled_plugins 配置）⇒ 不加载
//
// 外加 `declared`：自动启动**且声明了依赖**的子集（决定"何时"拉）。
type formGroups struct {
	autoStart []string // ① 自动启动的全部（含 declared）
	declared  []string // ① 中声明了 requires_facilities 的
	manual    []string // ② 不自动启动
	disabled  []string // ③ 持久禁用
}

func classify(c *PluginCatalog) formGroups {
	var g formGroups
	for _, m := range c.All() {
		switch {
		case m.Disabled:
			// ③ 禁用：最高优先级（不加载）
			g.disabled = append(g.disabled, m.Name)
		case !m.AutoStart:
			// ② 启用但不自动启动：**不自动加载**，等 agent 显式启用
			g.manual = append(g.manual, m.Name)
		default:
			// ① 自动启动
			g.autoStart = append(g.autoStart, m.Name)
			if len(m.RequiresFacilities) > 0 {
				g.declared = append(g.declared, m.Name)
			}
		}
	}
	return g
}

// loadOne 加载单个插件（走既有 LoadOneDir 路径，不绕过任何机制）。
func loadOne(ks *kernelboot.KernelState, dir, name string) (bool, error) {
	if err := ks.PluginRegistry.LoadOneDir(filepath.Join(dir, name), name); err != nil {
		return false, err
	}
	return true, nil
}

// planCatalog 取元数据缓存（类型断言集中一处，便于排查"缓存没建起来"）。
func planCatalog(ks *kernelboot.KernelState) *PluginCatalog {
	c, _ := ks.PluginCatalog.(*PluginCatalog)
	return c
}
