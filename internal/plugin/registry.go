package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	internalConfig "github.com/JianFeeeee/HologramAgent/internal/config"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
	luaVM "github.com/JianFeeeee/HologramAgent/internal/lua"
	"github.com/JianFeeeee/HologramAgent/internal/memory"
	doc "github.com/JianFeeeee/HologramAgent/internal/memory/document"
	"github.com/JianFeeeee/HologramAgent/internal/memory/text"
	"github.com/JianFeeeee/HologramAgent/internal/plugin/proc"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	"github.com/JianFeeeee/HologramAgent/internal/tracker"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

type NativeFactory func(name string, config map[string]interface{}) (sdk.Plugin, error)

// PluginMeta 插件显示名称元数据，来源于 plg.json / RegisterPluginMeta。
// 与内置 SDK 中的 PluginMeta 保持一致，供 PluginManager 接口返回。
type PluginMeta = sdk.PluginMeta

var globalPluginMeta sync.Map // name -> PluginMeta

// RegisterPluginMeta 供插件包在 init() 中调用，注册显示名称。
func RegisterPluginMeta(name, nameZh, nameEn string) {
	globalPluginMeta.Store(name, sdk.PluginMeta{NameZh: nameZh, NameEn: nameEn})
}

// GetPluginMeta 查询插件的显示名称。
func GetPluginMeta(name string) (PluginMeta, bool) {
	v, ok := globalPluginMeta.Load(name)
	if !ok {
		return PluginMeta{}, false
	}
	return v.(PluginMeta), true
}

// globalFactories 是插件通过 init() 自注册的全局工厂表。
// Registry.RegisterNative() 写入此表；Registry.Load() 从中查找。
var globalFactories sync.Map

// RegisterFactory 供插件包在 init() 中调用，实现自注册。
// plugin.RegisterFactory("timer", func(name string, cfg map[string]interface{}) (sdk.Plugin, error) { ... })
func RegisterFactory(name string, factory NativeFactory) {
	globalFactories.Store(name, factory)
}

// PluginToolCleaner 定义插件注销接口，由 StageHost 实现。
type PluginToolCleaner interface {
	UnregisterPluginTools(pluginName string)
}

// PluginStageCleaner 摘除插件注册的 stage handler，由 StageHost 实现。
//
// 与 PluginToolCleaner 分开是为了向后兼容：旧 toolCleaner 实现（测试替身）
// 只有 UnregisterPluginTools，经类型断言取 stage 能力，取不到则跳过。
type PluginStageCleaner interface {
	UnregisterPluginStages(pluginName string) int
}

type Registry struct {
	// kernelCtl 是内核重启/关机通道（装配时注入，可 nil）。
	// 只对声明了该能力的内置插件可见（目前是 WebUI）。
	kernelCtl sdk.KernelControlAPI
	mu        sync.RWMutex
	plugins   map[string]sdk.Plugin
	instances []sdk.Plugin
	factories map[string]NativeFactory

	pluginAutoRestart map[string]bool
	sdkRefs           map[string]*sdk.PluginSDK

	iom      *agentIO.IOManager
	evBus    *events.Bus
	memDB    *memory.GraphDB
	textMem  *text.Memory
	docStore *doc.Store
	// CAS 已淘汰（块自带负载，见 plan.md）。本注册表不保留媒体存储字段：
	// 留着会诱导后来者再接线，走回头路。媒体字节走 sdk.Memory().MediaBytes。
	ks      *knowledge.Store
	mgr     *agentAPI.ProviderManager
	cfgReg  *internalConfig.ConfigRegistry
	plgDir  string
	dataDir string // 守护进程数据目录（注入给插件 SettingsAPI.DataDir）
	lua     *luaVM.VM
	baseKey string

	regTool  sdk.ToolRegistrar
	regStage sdk.StageRegistrar
	regAPI   sdk.APIRegistrar

	toolCleaner PluginToolCleaner

	status    sdk.StatusAPI
	sup       sdk.SupervisorAPI
	trk       *tracker.Tracker
	cfg       *types.Config
	stageHost sdk.ToolSource
	idx       *memory.Indexer

	// deviceAuth 回答"设备 deviceID 是否已授权给当前 agent"（D4）。
	//
	// 由 bootstrap 在 agent 建好之后注入（registry 本身早于 agent 构造，
	// 所以这里存的是**晚绑定**的闭包）。为 nil 时 ToolAPI.CanUse 对
	// 设备工具放行 —— 保持存量行为不变。
	deviceAuth func(deviceID string) bool

	knownDisabled map[string]bool
	allowlist     map[string]bool

	// runtimeStopped 是**运行态**关闭门（设计 §2.6.7.3）。
	//
	// ## 与 knownDisabled / disabled_plugins 的区别（关键）
	//
	//   disabled_plugins（配置）  = 持久禁用："是否**允许**加载"，跨重启保留
	//   runtimeStopped（本字段）  = 运行态关闭："这一次不想让它跑"，**不**持久
	//
	// ## 为什么必须是两个门而不是一个
	//
	// 实测缺陷：`Enable()` 会调 `cfgReg.RemoveDisabledPlugin`，
	// `DisablePlugin()` 会调 `cfgReg.AddDisabledPlugin` —— 全都是
	// "改运行态**顺带**改持久态"。于是：
	//   · agent 想临时关掉某插件 ⇒ 只能调 DisablePlugin ⇒ 顺带写了持久禁用
	//     ⇒ 用户发现"我只是关了一下，怎么以后开机都不自启了"
	//   · 想把刚才关的插件开回来 ⇒ EnablePlugin ⇒ 清了禁用标记
	//     ⇒ 若它本来是"不自启"的，这个动作改掉了用户设置
	//
	// 所以需要一条**只改运行态**的路径。清除条件也不同：
	//   · 持久禁用由"启用"清除（用户级决定）
	//   · 运行态关闭由"启动"清除（会话级决定，重启自然复位）
	//
	// ★ 不持久是本设计的**有意选择**：运行态关闭 = "这一次不跑"，
	// 不是"以后都不跑"。若它持久，agent 关掉一个插件后就再也没法让它
	// 随内核起来 —— 那正是要修的毛病。
	runtimeStopped map[string]bool

	// pluginHashes 记录各插件二进制(plugin.so/plugin.bin/main.lua)的 SHA256，
	// 供增量重载(Reload)对比：仅重载有变更的插件，避免全量 StopAll+Load 导致重复加载。
	pluginHashes map[string]string

	// procHost 是**全部子进程插件共享的那一块** StageContext 段（§3.3/§3.4）。
	//
	// 懒创建（首个 .bin 插件加载时），随内核存活。共享而非每插件一段是关键：
	// 每插件一段会让「内核 ctx → 段 → 插件改 → 回读 ctx」在多插件下退化成副本模型，
	// lost update 原样复现（§8.4 实测 35.8~36.8%）。
	procHostMu sync.Mutex
	procHost   *proc.Host

	// pluginChannels 记录每个插件注册过哪些 IO 通道（输出 device + 输入通道）。
	//
	// 不记的后果：子进程插件崩溃后它的 output device 仍在 IOManager 里，
	// 模型依旧看到 output_send__<ch> 并调用，只能拿到 ErrProcessExited；
	// 重启时 RegisterDevice 又因同名已存在而报 already registered，
	// 插件回来了但通道永久指向旧进程的死闭包。
	channelsMu     sync.Mutex
	pluginChannels map[string]*pluginChannelSet

	// procCrashes 记录子进程插件的崩溃频次，防止崩溃循环无休止重启。
	//
	// 与 agent 侧 plugin_health 并存而非重复：后者只能看到工具调用路径上的
	// panic，进程级退出（signal: killed / OOM / 自身 exit）根本不经那里。
	crashMu     sync.Mutex
	procCrashes map[string]*procCrashRecord

	// shuttingDown 在 StopAll 起始置位，用于冻结自动重启。
	shuttingDown atomic.Bool
}

// 子进程插件自动重启策略。
const (
	// procMaxRestarts 是窗口内允许的自动重启次数上限。
	// 超过则停手：再重启也只是重复同一个崩溃，得让人看日志。
	procMaxRestarts = 3
	// procCrashWindow 内无新崩溃则计数归零。
	procCrashWindow = 5 * time.Minute
	// procRestartBackoff 是线性退避步长（第 n 次重启前等 n × 此值）。
	procRestartBackoff = time.Second
)

// procCrashRecord 是单插件的崩溃计数。
type procCrashRecord struct {
	count int
	last  time.Time
}

// pluginChannelSet 是单个插件注册过的通道名集合。
type pluginChannelSet struct {
	outputs map[string]bool
	inputs  map[string]bool
}

func NewRegistry() *Registry {
	return &Registry{
		plugins:           make(map[string]sdk.Plugin),
		factories:         make(map[string]NativeFactory),
		pluginAutoRestart: make(map[string]bool),
		sdkRefs:           make(map[string]*sdk.PluginSDK),
		knownDisabled:     make(map[string]bool),
		runtimeStopped:    make(map[string]bool),
		pluginHashes:      make(map[string]string),
		pluginChannels:    make(map[string]*pluginChannelSet),
	}
}

func (r *Registry) SetIOManager(iom *agentIO.IOManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.iom = iom
}
func (r *Registry) SetEventBus(evBus *events.Bus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.evBus = evBus
}
func (r *Registry) SetMemory(memDB *memory.GraphDB) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.memDB = memDB
}
func (r *Registry) SetTextMemory(tm *text.Memory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.textMem = tm
}
func (r *Registry) SetDocStore(ds *doc.Store) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.docStore = ds
}
func (r *Registry) SetKnowledge(ks *knowledge.Store) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ks = ks
}
func (r *Registry) SetProviderManager(mgr *agentAPI.ProviderManager) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mgr = mgr
}
func (r *Registry) SetConfigRegistry(cfgReg *internalConfig.ConfigRegistry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfgReg = cfgReg
}
func (r *Registry) SetPluginDir(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plgDir = dir
}
func (r *Registry) SetDataDir(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dataDir = dir
}
func (r *Registry) SetLuaVM(vm *luaVM.VM) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lua = vm
}
func (r *Registry) SetBaseAPIKey(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.baseKey = key
}

// kernelControlAllowed 决定某个内置插件能否拿到内核重启/关机通道。
//
// ★ 为什么必须白名单，而不是"有就给"：
//
//	这条通道能让**整个服务停下来**（关机后不会自己回来）。
//	内置插件是编译进内核的，但它们的代码同样可能被改坏或有 bug；
//	把这种能力发给每一个内置插件，等于整个攻击面 = 全部内置插件。
//
// 目前只给 WebUI：它是有登录鉴权的管理界面，用户已经决定信任它。
// 将来要开放给别的插件，必须逐个评估（它有没有鉴权、会不会误触发）。
var kernelControlAllowed = map[string]bool{
	"webui": true,
}

// SetKernelControl 注入内核重启/关机通道（由 cmd/homed 装配时调用）。
//
// ★ 为何用 setter 而不是构造参数：internal/plugin 不能 import cmd/homed
//
//	（那是反向依赖，且 cmd 包不可被 import）。用接口 + setter 打破方向。
func (r *Registry) SetKernelControl(k sdk.KernelControlAPI) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kernelCtl = k
}

// kernelControlFor 按插件名决定是否授予内核控制通道。
//
// 未在白名单里的插件拿到 nil —— SDK 侧 KernelControl() 返回 nil，
// 调用方（插件自己）必须判 nil 并如实告知用户"当前环境不支持"。
func (r *Registry) kernelControlFor(name string) sdk.KernelControlAPI {
	r.mu.RLock()
	ctl := r.kernelCtl
	r.mu.RUnlock()
	return kernelControlForCtl(ctl, name)
}

// kernelControlForCtl 是 kernelControlFor 的**无锁内核**：给定已快照的 ctl 决定授权。
//
// 拆出来的原因：buildSDK 要在**一次** RLock 里取走整组依赖（见 snapshotDeps），
// 不能再回头单独取锁 —— RWMutex 不可重入，持锁再 RLock 在写者排队时会自锁。
func kernelControlForCtl(ctl sdk.KernelControlAPI, name string) sdk.KernelControlAPI {
	if ctl == nil || !kernelControlAllowed[name] {
		return nil
	}
	return ctl
}

func (r *Registry) SetToolRegistrar(fn sdk.ToolRegistrar) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.regTool = fn
}
func (r *Registry) SetStageRegistrar(fn sdk.StageRegistrar) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.regStage = fn
}
func (r *Registry) SetAPIRegistrar(fn sdk.APIRegistrar) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.regAPI = fn
}
func (r *Registry) SetToolCleaner(tc PluginToolCleaner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.toolCleaner = tc
}
func (r *Registry) SetStatusProvider(sp sdk.StatusAPI) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = sp
}
func (r *Registry) SetSupervisor(sup sdk.SupervisorAPI) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sup = sup
}
func (r *Registry) SetTracker(trk *tracker.Tracker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trk = trk
}
func (r *Registry) SetConfig(cfg *types.Config) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg = cfg
}
func (r *Registry) SetStageHost(sh sdk.ToolSource) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stageHost = sh
}

// SetDeviceAuthQuery 注入"设备是否已授权"的查询（D4）。
//
// 由 bootstrap 在 agent 构造完成后调用。注入后，ToolAPI.CanUse 才会
// 对未授权设备返回 false —— 此前 ToolAPI 路径**完全不过授权闸**。
func (r *Registry) SetDeviceAuthQuery(fn func(deviceID string) bool) {
	r.mu.Lock()
	r.deviceAuth = fn
	r.mu.Unlock()
	sdk.SetDeviceAuthQuery(fn)
}
func (r *Registry) SetIndexer(idx *memory.Indexer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.idx = idx
}

// SetLoadAllowlist 限制 Load 仅装载指定插件名（failback 受限启动用）。
// 空/未设置 = 装载全部。违反白名单的插件（含已注册工厂）一律跳过。
func (r *Registry) SetLoadAllowlist(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allowlist = nil
	if len(names) == 0 {
		return
	}
	r.allowlist = make(map[string]bool, len(names))
	for _, n := range names {
		r.allowlist[strings.TrimSpace(n)] = true
	}
}

func (r *Registry) allowlistAllows(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.allowlist == nil {
		return true
	}
	return r.allowlist[name]
}

func (r *Registry) RegisterNative(name string, factory NativeFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = factory
	globalFactories.Store(name, factory)
}

type channelDevice struct {
	name    string
	desc    string
	caps    agentIO.OutputCapability
	handler sdk.ToolHandler
	chDef   agentIO.ChannelDef
}

func (d *channelDevice) Name() string                                 { return d.name }
func (d *channelDevice) Type() agentIO.DeviceType                     { return agentIO.DeviceOutput }
func (d *channelDevice) Description() string                          { return d.desc }
func (d *channelDevice) Start() error                                 { return nil }
func (d *channelDevice) Stop() error                                  { return nil }
func (d *channelDevice) OutputCapabilities() agentIO.OutputCapability { return d.caps }
func (d *channelDevice) Tools() []agentIO.ToolDef                     { return nil }
func (d *channelDevice) Execute(tool string, args map[string]interface{}) (interface{}, error) {
	return d.handler(args)
}
func (d *channelDevice) ChannelDef() agentIO.ChannelDef { return d.chDef }

// sdkDeps 是 buildSDK 所需依赖的**一致性快照**（KERNEL-REGISTRY-SETLOCK）。
//
// 为什么要有这层：这批字段由 Set* 在启动图里注入，读侧若逐个裸读
// （原来的写法），每一处都是独立的竞争点；而且逐个"读一次锁一次"会让
// 同一次 buildSDK 里拿到**互相不一致**的组合（比如 lua 已是新的、cfg 还是旧的）。
// 一次 RLock 取齐既消灭竞争，也保证同一次 SDK 装配看到的是同一个状态。
type sdkDeps struct {
	plgDir    string
	dataDir   string
	cfgReg    *internalConfig.ConfigRegistry
	iom       *agentIO.IOManager
	evBus     *events.Bus
	memDB     *memory.GraphDB
	textMem   *text.Memory
	docStore  *doc.Store
	ks        *knowledge.Store
	mgr       *agentAPI.ProviderManager
	lua       *luaVM.VM
	baseKey   string
	regTool   sdk.ToolRegistrar
	regStage  sdk.StageRegistrar
	regAPI    sdk.APIRegistrar
	status    sdk.StatusAPI
	sup       sdk.SupervisorAPI
	trk       *tracker.Tracker
	cfg       *types.Config
	stageHost sdk.ToolSource
	idx       *memory.Indexer
	kernelCtl sdk.KernelControlAPI
}

// snapshotDeps 在一次 RLock 内取走 buildSDK 需要的全部依赖。
func (r *Registry) snapshotDeps() sdkDeps {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sdkDeps{
		plgDir:    r.plgDir,
		dataDir:   r.dataDir,
		cfgReg:    r.cfgReg,
		iom:       r.iom,
		evBus:     r.evBus,
		memDB:     r.memDB,
		textMem:   r.textMem,
		docStore:  r.docStore,
		ks:        r.ks,
		mgr:       r.mgr,
		lua:       r.lua,
		baseKey:   r.baseKey,
		regTool:   r.regTool,
		regStage:  r.regStage,
		regAPI:    r.regAPI,
		status:    r.status,
		sup:       r.sup,
		trk:       r.trk,
		cfg:       r.cfg,
		stageHost: r.stageHost,
		idx:       r.idx,
		kernelCtl: r.kernelCtl,
	}
}

// ioManagerOrNil 返回当前 IOManager 快照（无则 nil）。
//
// 给 buildSDK 里的**闭包**用：闭包在插件 Start 期间才被调用，那时不能持锁
// （会跨用户代码），所以每次都取一次新快照 —— 语义与原来的 r.iom 裸读一致，
// 只是变成有序读。
func (r *Registry) ioManagerOrNil() *agentIO.IOManager {
	r.mu.RLock()
	iom := r.iom
	r.mu.RUnlock()
	return iom
}

// pluginDirOrEmpty / cfgRegistryOrNil / toolCleanerOrNil / stageHostOrNil
// 是其余读点的同款访问器。集中在这里，避免各处自己拼锁走样。
func (r *Registry) pluginDirOrEmpty() string {
	r.mu.RLock()
	dir := r.plgDir
	r.mu.RUnlock()
	return dir
}

func (r *Registry) cfgRegistryOrNil() *internalConfig.ConfigRegistry {
	r.mu.RLock()
	cfgReg := r.cfgReg
	r.mu.RUnlock()
	return cfgReg
}

func (r *Registry) toolCleanerOrNil() PluginToolCleaner {
	r.mu.RLock()
	tc := r.toolCleaner
	r.mu.RUnlock()
	return tc
}

func (r *Registry) eventBusOrNil() *events.Bus {
	r.mu.RLock()
	bus := r.evBus
	r.mu.RUnlock()
	return bus
}

func (r *Registry) stageHostOrNil() sdk.ToolSource {
	r.mu.RLock()
	sh := r.stageHost
	r.mu.RUnlock()
	return sh
}

func (r *Registry) buildSDK(name string) *sdk.PluginSDK {
	// ★ 依赖快照（2026-10-24，KERNEL-REGISTRY-SETLOCK）。
	//
	// 为什么必须**一次取齐**而不是用 r.xxx 逐个裸读：
	// 这批字段由 Set* 在启动图第 4 层注入，而 buildSDK 在插件加载时同理并发执行；
	// 每个 r.xxx 各自裸读 = 每个都是一处数据竞争（实测 4% 概率报 7 条）。
	//
	// 为什么不在整个 buildSDK 外面套一把 RLock：下面的闭包（regOutput 等）
	// 只在**插件 Start 期间**才被调用，而 Start 本身可能在持锁区外跑很久；
	// 持着 registry 读锁跨用户代码要冒死锁风险（插件回调进内核注册面）。
	// 所以取到快照就立刻放锁，闭包里再用访问器晚绑定。
	deps := r.snapshotDeps()

	// 插件自己的安装目录：rawfile 资源与可写工作目录都挂在它下面。
	pluginDir := filepath.Join(deps.plgDir, name)

	sett := sdk.NewSettings(name, deps.cfgReg)
	if sd, ok := sett.(interface{ SetDataDir(string) }); ok {
		// 插件专属数据目录：<data>/plugin_data/<name>，保证存在
		dir := filepath.Join(deps.dataDir, "plugin_data", name)
		os.MkdirAll(dir, 0755)
		sd.SetDataDir(dir)
	}

	regTool := deps.regTool
	if regTool == nil {
		regTool = func(toolName string, def sdk.ToolDef, handler sdk.ToolHandler) error {
			return nil
		}
	}
	regStage := deps.regStage
	if regStage == nil {
		regStage = func(stage sdk.Stage, handler sdk.StageHandler) {}
	}
	// stage handler 注册时带上归属插件名，使卸载/崩溃时能成组摘除。
	// StageHost 实现了 RegisterStageFor；其他实现（测试替身）退回无归属注册。
	if h, ok := stageRegistrarForSource(deps.stageHost); ok {
		regStage = func(stage sdk.Stage, handler sdk.StageHandler) {
			h(name, stage, handler)
		}
	}
	regAPI := deps.regAPI
	if regAPI == nil {
		regAPI = func(name string) error { return nil }
	}

	regOutput := func(chName string, caps int, desc string, def sdk.ChannelDef, handler sdk.ToolHandler) error {
		iom := r.ioManagerOrNil()
		if iom == nil {
			return nil
		}
		if err := iom.RegisterDevice(&channelDevice{
			name:    chName,
			caps:    agentIO.OutputCapability(caps),
			desc:    desc,
			handler: handler,
			chDef:   agentIO.ChannelDef(def),
		}); err != nil {
			return err
		}
		// **兼容网**：插件应当显式 RegisterInputChannel 声明自己的入站通道；
		// 但历史插件常常只用 RegisterOutputChannel 声明（却用同一个名字注入输入，
		// 例：cli 只声明输出 "cli" 就用 InjectTextSync("cli", ...) 注入）。
		// 不兜底的话 inputch 登记表里没有它，"把 inputch 划给驻留子"直接失败
		// （实测报 `划入 inputch cli: inputch 未注册`）。
		//
		// ❗这里**不能**判"是否声明过入站通道"并告警：声明顺序是自由的，
		// 先 RegisterOutputChannel 再 RegisterInputChannel 是常见写法（qq 就是），
		// 按此刻的状态判会对它误报（实测：把 qq 报成"只声明了输出通道"）。
		// 真正该问的问题是"插件 Start 结束后，这个出站通道有没有对应的入站声明" ——
		// 那在 load 完成后统一判（见 warnOutputOnlyChannels）。
		if _, ok := iom.LookupInputChannel(chName); !ok {
			_ = iom.RegisterInputChannelFrom(name, chName, agentIO.ChannelDef(def))
		}
		r.noteChannel(name, chName, true)
		return nil
	}

	regOutputUnreg := func(chName string) error {
		iom := r.ioManagerOrNil()
		if iom == nil {
			return nil
		}
		iom.UnregisterDevice(chName)
		r.forgetChannel(name, chName)
		return nil
	}

	regInput := func(chName string, def sdk.ChannelDef) error {
		iom := r.ioManagerOrNil()
		if iom == nil {
			return nil
		}
		// inputch 是最基本的输入路由单位：登记**归属插件**，便于父 agent 看清
		// "哪个插件的哪个 inputch 划给了谁"（一个插件可注册多个 inputch）。
		_ = iom.RegisterInputChannelFrom(name, chName, agentIO.ChannelDef(def))
		r.noteChannel(name, chName, false)
		return nil
	}

	return sdk.New(name, sdk.SDKConfig{
		IOManager: deps.iom,
		EventBus:  deps.evBus,
		// 块自带负载（媒体字节在块的 Data），不再需要独立的媒体存储句柄。
		// 插件名仅用于日志溯源（哪个插件写的块）。
		Memory:     sdk.NewGraphMemoryWithPlugin(name, deps.memDB),
		TextMemory: sdk.NewTextMemoryWithPlugin(name, deps.textMem),
		DocMemory:  sdk.NewDocMemoryWithPlugin(name, deps.docStore),
		Knowledge:  sdk.NewKnowledge(deps.ks),
		LLM:        sdk.NewLLM(deps.mgr, deps.cfgReg, deps.lua, deps.baseKey),
		Settings:   sett,
		// ★ 变量库：桥接到进程级 security.Global()（**不另建存储**）。
		// 内核未启用安全模块时它 Available()==false（不是 nil）。
		Secrets: sdk.NewSecretsAPI(),
		// ★ 随包资源：读 <插件目录>/rawfile/，可写目录 <插件目录>/work/。
		// 内置插件（无独立目录）拿到的路径不存在，Read 会明确报错 ——
		// 那是对的：它们本来就没有随包资源。
		RawFiles:       sdk.NewRawFilesAPI(pluginDir),
		RegTool:        regTool,
		RegStage:       regStage,
		RegAPI:         regAPI,
		RegOutput:      regOutput,
		RegOutputUnreg: regOutputUnreg,
		RegInput:       regInput,
		PluginMgr:      r,
		KernelControl:  kernelControlForCtl(deps.kernelCtl, name),

		Status:     deps.status,
		Supervisor: deps.sup,
		Adapter:    sdk.NewAdapter(deps.lua),
		Tracker:    deps.trk,
		Config:     sdk.NewConfig(deps.cfg),
		Tool:       sdk.NewTool(deps.stageHost, deps.iom),
		Indexer:    sdk.NewIndexer(deps.idx),
	})
}

func (r *Registry) Load(dir string) error {
	if dir == "" {
		dir = r.pluginDirOrEmpty()
	}
	os.MkdirAll(dir, 0755)

	// 1) 扫描已有目录
	loaded := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			os.MkdirAll(dir, 0755)
			return nil
		}
		return err
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !r.allowlistAllows(name) {
			continue
		}
		plgDir := filepath.Join(dir, name)
		if r.loadOne(plgDir, name) {
			loaded[name] = true
		}
	}

	// 2) 对已注册但尚无目录的工厂，创建目录并加载
	r.mu.RLock()
	allFactories := make(map[string]NativeFactory)
	for name, f := range r.factories {
		allFactories[name] = f
	}
	r.mu.RUnlock()

	globalFactories.Range(func(key, val interface{}) bool {
		name := key.(string)
		if _, ok := allFactories[name]; !ok {
			allFactories[name] = val.(NativeFactory)
		}
		return true
	})

	for name, factory := range allFactories {
		if loaded[name] {
			continue
		}
		if !r.allowlistAllows(name) {
			continue
		}
		if r.isDisabled(name) {
			log.Printf("[plugin] %s is disabled, skipping", name)
			r.mu.Lock()
			r.knownDisabled[name] = true
			r.mu.Unlock()
			continue
		}
		plgDir := filepath.Join(dir, name)
		os.MkdirAll(plgDir, 0755)

		cfg := r.readConfig(plgDir)
		p, err := factory(name, cfg)
		if err != nil {
			log.Printf("[plugin] factory %s: %v", name, err)
			continue
		}
		if p == nil {
			continue
		}

		plgSDK := r.buildSDK(name)
		if err := p.Start(plgSDK); err != nil {
			log.Printf("[plugin] start %s: %v", name, err)
			continue
		}

		r.mu.Lock()
		r.plugins[name] = p
		r.pluginAutoRestart[name] = plgSDK.AutoRestart()
		r.instances = append(r.instances, p)
		r.mu.Unlock()
		r.warnOutputOnlyChannels(name)
		log.Printf("[plugin] loaded: %s", name)
	}

	return nil
}

func (r *Registry) isDisabled(name string) bool {
	cfgReg := r.cfgRegistryOrNil()
	if cfgReg == nil {
		return false
	}
	return cfgReg.IsPluginDisabled(name)
}

// pluginEntryHash 计算插件入口文件的 SHA256，用于增量重载对比。
// 无入口文件（内置纯工厂插件）返回空字符串（始终视为已加载）。
// plugin.bin 排在最前：与 detectEntryKind 保持一致的优先级，迁移期间同目录
// 两种产物共存时以子进程产物为准。
func pluginEntryHash(plgDir string) string {
	for _, candidate := range []string{binEntry, luaEntry, skillEntry} {
		path := filepath.Join(plgDir, candidate)
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			sum := sha256.Sum256(data)
			return hex.EncodeToString(sum[:])
		}
	}
	return ""
}

func (r *Registry) loadOne(plgDir, name string) bool {
	if r.isDisabled(name) {
		log.Printf("[plugin] %s is disabled, skipping", name)
		r.mu.Lock()
		r.knownDisabled[name] = true
		r.mu.Unlock()
		return false
	}
	// ② 运行态关闭门（设计 §2.6.7.3）。
	//
	// 与上面的持久禁用门**分开**：持久禁用由"启用"清除，运行态关闭由
	// "启动"清除。复用一个门就会让"临时关一下"变成"以后都不自启"。
	//
	// 注意这一条也挡住 loadPlugins 的启动扫描 —— 若插件在启动前就被
	// 运行态关闭（理论上不会，但保持语义一致），它不该被拉起。
	if r.isRuntimeStopped(name) {
		log.Printf("[plugin] %s 处于运行态关闭（runtime stop），跳过加载", name)
		return false
	}

	// 1) 查找工厂（init 自注册或 RegisterNative）
	r.mu.RLock()
	factory, hasFactory := r.factories[name]
	r.mu.RUnlock()
	if !hasFactory {
		v, ok := globalFactories.Load(name)
		if ok {
			factory = v.(NativeFactory)
			hasFactory = true
		}
	}

	var plg sdk.Plugin

	// 读取 plugin.json 以获取插件显示名称元数据（主要用于外部插件）
	if mft := readManifest(plgDir); mft != nil {
		if mft.NameZh != "" || mft.NameEn != "" {
			RegisterPluginMeta(name, mft.NameZh, mft.NameEn)
		}
	}

	if hasFactory {
		cfg := r.readConfig(plgDir)
		p, err := factory(name, cfg)
		if err != nil {
			log.Printf("[plugin] factory %s: %v", name, err)
			return false
		}
		if p == nil {
			return false
		}
		plg = p
	} else {
		// 2) 无工厂，尝试动态加载 .so / .lua
		dynCfg := r.readConfig(plgDir)
		p, err := r.tryDynamic(plgDir, name, dynCfg)
		if err != nil {
			log.Printf("[plugin] dynamic %s: %v", name, err)
		}
		if p == nil {
			return false
		}
		plg = p
	}

	plgSDK := r.buildSDK(name)
	if err := plg.Start(plgSDK); err != nil {
		log.Printf("[plugin] start %s: %v", name, err)
		return false
	}

	r.mu.Lock()
	r.plugins[name] = plg
	r.pluginAutoRestart[name] = plgSDK.AutoRestart()
	r.sdkRefs[name] = plgSDK
	r.instances = append(r.instances, plg)
	r.warnOutputOnlyChannels(name)
	if h := pluginEntryHash(plgDir); h != "" {
		r.pluginHashes[name] = h
	} else {
		delete(r.pluginHashes, name)
	}
	r.mu.Unlock()
	log.Printf("[plugin] loaded: %s", name)
	return true
}

// runStopHandlers 执行插件注册的停止清理回调（SDK 层），须在调用插件 Stop() 之前执行。
func (r *Registry) runStopHandlers(name string) {
	if sdk, ok := r.sdkRefs[name]; ok {
		sdk.RunStopHandlers()
	}
}

// stageRegistrarFor 取带归属的 stage 注册入口。
//
// r.regStage 是 cmd/homed 注入的闭包（无插件名参数），而 stageHost 本体同时
// 以 sdk.ToolSource 存在 r.stageHost 上。能取到 RegisterStageFor 时就直接用它，
// 否则退回无归属注册（测试替身、旧集成方）。
func (r *Registry) stageRegistrarFor() (func(plugin string, stage sdk.Stage, handler sdk.StageHandler), bool) {
	r.mu.RLock()
	sh := r.stageHost
	r.mu.RUnlock()
	return stageRegistrarForSource(sh)
}

// stageRegistrarForSource 是 stageRegistrarFor 的无锁内核（同样为了快照后复用）。
func stageRegistrarForSource(sh sdk.ToolSource) (func(plugin string, stage sdk.Stage, handler sdk.StageHandler), bool) {
	if sh == nil {
		return nil, false
	}
	if h, ok := sh.(interface {
		RegisterStageFor(plugin string, stage sdk.Stage, handler sdk.StageHandler)
	}); ok {
		return h.RegisterStageFor, true
	}
	return nil, false
}

// warnOutputOnlyChannels 在插件 Start 结束后，报告"只声明了出站、没有入站声明"的通道。
//
// 为什么放在 Start 之后：声明顺序自由（先出站后入站很常见），注册时刻的状态
// 判不出意图。这里看的是**插件最终声明了什么**，因此不会误报 qq 这种写法。
//
// 注：这类通道内核已兜底登记 inputch（功能可用），告警只是提醒插件作者把意图写明。
func (r *Registry) warnOutputOnlyChannels(plugin string) {
	r.channelsMu.Lock()
	set := r.pluginChannels[plugin]
	var only []string
	if set != nil {
		for ch := range set.outputs {
			if !set.inputs[ch] {
				only = append(only, ch)
			}
		}
	}
	r.channelsMu.Unlock()
	sort.Strings(only)
	for _, ch := range only {
		log.Printf("[plugin] %s 只声明了出站通道 %q（未 RegisterInputChannel）；"+
			"内核已兜底登记 inputch，若这是有意为之可忽略", plugin, ch)
	}
}

// noteChannel 记住插件注册了哪个通道，供卸载/崩溃时摘除。
// forgetChannel 把某个通道从"本插件注册过哪些通道"的记账里摘掉（注销通道时用）。
//
// 不摘的话 status 的 channels 列表与 input_channels 视图会一直列着已死通道，
// 模型会以为它还在（远程设备掉线后尤其明显）。
func (r *Registry) forgetChannel(plugin, channel string) {
	if plugin == "" || channel == "" {
		return
	}
	r.channelsMu.Lock()
	defer r.channelsMu.Unlock()
	set := r.pluginChannels[plugin]
	if set == nil {
		return
	}
	delete(set.outputs, channel)
	delete(set.inputs, channel)
}

func (r *Registry) noteChannel(plugin, channel string, output bool) {
	if plugin == "" || channel == "" {
		return
	}
	r.channelsMu.Lock()
	defer r.channelsMu.Unlock()
	set := r.pluginChannels[plugin]
	if set == nil {
		set = &pluginChannelSet{outputs: map[string]bool{}, inputs: map[string]bool{}}
		r.pluginChannels[plugin] = set
	}
	if output {
		set.outputs[channel] = true
	} else {
		set.inputs[channel] = true
	}
}

// releasePluginChannels 摘除插件注册过的全部 IO 通道，返回摘除的通道名。
//
// 必须做：不摘除则 ① 模型仍看得到 output_send__<ch> 却永远失败；
// ② 插件重启时 RegisterDevice 报 already registered，新进程的通道注不上，
// 通道永久指向已死进程的闭包。
func (r *Registry) releasePluginChannels(plugin string) []string {
	if plugin == "" {
		return nil
	}
	r.channelsMu.Lock()
	set := r.pluginChannels[plugin]
	delete(r.pluginChannels, plugin)
	r.channelsMu.Unlock()
	if set == nil {
		return nil
	}
	iom := r.ioManagerOrNil()
	if iom == nil {
		return nil
	}
	var released []string
	for ch := range set.outputs {
		iom.UnregisterDevice(ch)
		released = append(released, ch)
	}
	for ch := range set.inputs {
		iom.UnregisterInputChannel(ch)
		if !set.outputs[ch] {
			released = append(released, ch)
		}
	}
	sort.Strings(released)
	return released
}

// detachPlugin 把插件在内核侧的全部注册面摸干净：工具 + stage handler + IO 通道。
//
// 这是「卸载一个插件」的完整含义。之前各路径（Disable/Reload/Remove/
// StopAndUnload）只调 UnregisterPluginTools，漏了 stage 与通道两项，
// 子进程崩溃路径更是三项都没做。
func (r *Registry) detachPlugin(name string) {
	if tc := r.toolCleanerOrNil(); tc != nil {
		tc.UnregisterPluginTools(name)
		if sc, ok := tc.(PluginStageCleaner); ok {
			if n := sc.UnregisterPluginStages(name); n > 0 {
				log.Printf("[plugin] %s: 摘除 %d 个 stage handler", name, n)
			}
		}
	}
	if chans := r.releasePluginChannels(name); len(chans) > 0 {
		log.Printf("[plugin] %s: 摘除 IO 通道 %v", name, chans)
	}
}

// runOnRemoveHandlers 执行插件注册的删除清理回调（SDK 层），插件 Stop() 之后、从注册表移除前执行。
func (r *Registry) runOnRemoveHandlers(name string) {
	if sdk, ok := r.sdkRefs[name]; ok {
		sdk.RunOnRemoveHandlers()
	}
}

// stopTarget 是 StopAll 并行化时的一个停止单元：插件本体 + 它的 SDK 句柄。
type stopTarget struct {
	plugin sdk.Plugin
	sdkRef *sdk.PluginSDK
}

func (r *Registry) StopAll() {
	// 关停开始即冻结自动重启：否则「Stop 触发退出 → 崩溃判定 → 重新 spawn」
	// 会在内核正在关停时把子进程又拉起来，段已拆而进程还在，直接 SIGBUS。
	r.shuttingDown.Store(true)

	// 取插件快照后**立即释放 registry 锁**，再并行停。
	//
	// 为何必须并行：串行时最坏耗时 = Σ(每个插件) = 5s(plugin.stop 调用)
	// + 5s(等退出) + 2s(收割) = 12s；线上有 23 个子进程插件，
	// 即 276s，而 systemd 只给 90s ⇒ 关停必然 timed out 然后 SIGKILL。
	// 实测确实每次都超时（线上日志里 23 个插件全退完了，
	// 最后那条 "[homed] stopped" 仍打不出来）。
	//
	// 为何先释放锁：p.Stop() 会触发 markExited → onExit → ReclaimOwner，
	// 那条链要读共享内存段。持着 registry 锁并行跑，若某插件的 onExit
	// 回调需要拿 registry 锁（如摘通道），就是自死锁。
	r.mu.Lock()
	snapshot := make([]sdk.Plugin, len(r.instances))
	copy(snapshot, r.instances)
	// stop handler 挂在 PluginSDK 上（r.sdkRefs），必须**在清空 sdkRefs 之前**
	// 把 handler 跑掉 —— 否则下面并行 goroutine 里就找不到它了。
	// runStopHandlers 自己不加锁（调用方持锁），这里正是持锁状态。
	stoppers := make([]stopTarget, 0, len(snapshot))
	for _, p := range snapshot {
		if p == nil {
			continue
		}
		stoppers = append(stoppers, stopTarget{plugin: p, sdkRef: r.sdkRefs[p.Name()]})
	}
	r.plugins = make(map[string]sdk.Plugin)
	r.instances = nil
	r.pluginAutoRestart = make(map[string]bool)
	r.sdkRefs = make(map[string]*sdk.PluginSDK)
	r.mu.Unlock()

	// 并行停：每个插件一个 goroutine，等全部完成。
	// 单个插件 panic 不带崩整个关停（那会让剩下的插件全停不掉），
	// 也不静默吞掉（留下日志）。
	var wg sync.WaitGroup
	for _, t := range stoppers {
		wg.Add(1)
		go func(tg stopTarget) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[plugin] stop %s panic: %v", tg.plugin.Name(), rec)
				}
			}()
			// stop handler（解绑通道等）必须先于 Stop：见 runStopHandlers 注释。
			if tg.sdkRef != nil {
				tg.sdkRef.RunStopHandlers()
			}
			if err := tg.plugin.Stop(); err != nil {
				log.Printf("[plugin] stop %s: %v", tg.plugin.Name(), err)
			}
		}(t)
	}
	wg.Wait()

	// 共享段在全部子进程退出后再释放：插件还持有映射时拆段，
	// 它们下一次访问就是 SIGBUS。在锁外调用：Close 不需 registry 锁，
	// 而持锁调它会与 onProcCrash 路径（子进程退出回调）产生锁序风险。
	r.closeProcHost()
}

func (r *Registry) Reload(dir string) (string, error) {
	if dir == "" {
		dir = r.pluginDirOrEmpty()
	}
	// 增量重载：扫描插件目录，对比入口文件 hash，仅 Stop+重载有变更的插件。
	// 未变更插件保持运行，避免 plgreload 触发全量 StopAll+Load 导致所有插件重复加载
	// 及内置插件(如 healthcheck)状态机错乱。
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	changed := 0
	remaining := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !r.allowlistAllows(name) {
			continue
		}
		plgDir := filepath.Join(dir, name)
		h := pluginEntryHash(plgDir)
		r.mu.RLock()
		old := r.pluginHashes[name]
		loaded := r.plugins[name] != nil
		r.mu.RUnlock()
		// 无入口文件（纯内置工厂插件）始终视为已加载；
		// 有变更或首次出现且未加载 → 需要重载。
		if !loaded {
			if r.loadOne(plgDir, name) {
				changed++
			}
			continue
		}
		if h == "" {
			remaining++
			continue
		}
		if old != h {
			if err := r.ReloadOne(name); err != nil {
				log.Printf("[plugin] reload %s: %v", name, err)
			} else {
				changed++
			}
		} else {
			remaining++
		}
	}
	return fmt.Sprintf("reloaded %d plugins, %d unchanged", changed, remaining), nil
}

// LoadOneDir 加载**单个**插件目录（供按 syncbus 分阶段启动的调度器使用）。
//
// 与 Load(dir) 的区别：Load 扫描整个目录并加载所有插件；
// 本方法只加载指定的那一个，让调用方（调度器）能控制**时机**。
//
// 复用 loadOne 的全部语义（禁用门、运行态门、工厂查找、握手超时……），
// **不绕过**任何既有机制 —— 分阶段启动不该是本设计的特权路径。
func (r *Registry) LoadOneDir(plgDir, name string) error {
	if !r.allowlistAllows(name) {
		return fmt.Errorf("plugin %s 不在本启动模式的允许清单内", name)
	}
	if r.loadOne(plgDir, name) {
		return nil
	}
	return fmt.Errorf("加载插件 %s 失败（见内核日志）", name)
}

// LoadSkipping 加载目录下**除 skip 之外**的全部插件（供分阶段启动的"兜底"路径）。
//
// 语义与 Load(dir) 一致，只是把交给调度器的插件排除掉（它们由调度器按
// 依赖就绪情况拉起），避免"调度器还没轮到、Load 先把它们全拉了"。
//
// ★ 这个排除是分阶段启动能生效的关键：若不做，Load 的全量扫描会把
// 所有插件（含声明了依赖的）立刻加载，调度器就无事可做了。
func (r *Registry) LoadSkipping(dir string, skip map[string]bool) error {
	if dir == "" {
		dir = r.pluginDirOrEmpty()
	}
	os.MkdirAll(dir, 0755)

	loaded := map[string]bool{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			os.MkdirAll(dir, 0755)
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if skip[name] {
			continue // 交给调度器按依赖就绪情况拉起
		}
		if !r.allowlistAllows(name) {
			continue
		}
		if r.loadOne(filepath.Join(dir, name), name) {
			loaded[name] = true
		}
	}

	// 内置工厂（编译期注册、可能尚无目录）同样排除 skip 的那些
	r.mu.RLock()
	allFactories := make(map[string]NativeFactory, len(r.factories))
	for name, f := range r.factories {
		allFactories[name] = f
	}
	r.mu.RUnlock()
	globalFactories.Range(func(key, val interface{}) bool {
		name := key.(string)
		if _, ok := allFactories[name]; !ok {
			allFactories[name] = val.(NativeFactory)
		}
		return true
	})

	for name, factory := range allFactories {
		if loaded[name] || skip[name] || !r.allowlistAllows(name) {
			continue
		}
		if r.isDisabled(name) {
			log.Printf("[plugin] %s is disabled, skipping", name)
			r.mu.Lock()
			r.knownDisabled[name] = true
			r.mu.Unlock()
			continue
		}
		plgDir := filepath.Join(dir, name)
		os.MkdirAll(plgDir, 0755)
		cfg := r.readConfig(plgDir)
		p, err := factory(name, cfg)
		if err != nil {
			log.Printf("[plugin] factory %s: %v", name, err)
			continue
		}
		if p == nil {
			continue
		}
		plgSDK := r.buildSDK(name)
		if err := p.Start(plgSDK); err != nil {
			log.Printf("[plugin] start %s: %v", name, err)
			continue
		}
		r.mu.Lock()
		r.plugins[name] = p
		r.pluginAutoRestart[name] = plgSDK.AutoRestart()
		r.instances = append(r.instances, p)
		r.mu.Unlock()
		r.warnOutputOnlyChannels(name)
		log.Printf("[plugin] loaded: %s", name)
		loaded[name] = true
	}
	return nil
}

func (r *Registry) ReloadOne(name string) error {
	plgDir := filepath.Join(r.pluginDirOrEmpty(), name)

	r.mu.Lock()
	var removed sdk.Plugin
	if p, ok := r.plugins[name]; ok {
		r.runStopHandlers(name)
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s for reload: %v", name, err)
		}
		delete(r.plugins, name)
		delete(r.sdkRefs, name)
		for i, inst := range r.instances {
			if inst.Name() == name {
				r.instances = append(r.instances[:i], r.instances[i+1:]...)
				break
			}
		}
		removed = p
	}
	r.mu.Unlock()

	// 重载前必须把旧注册面摸干净。不做的后果：loadOne 重新 Start 时
	// RegisterTool 碰上同名旧工具直接报 already registered，新实例的工具一个都注不上；
	// stage handler 与 output device 同理——旧闭包指向已死进程，永不退场。
	// （此前只有 agent 的 autoReloadPlugins 在外层手动摸工具，plgreload 路径漏了。）
	r.detachPlugin(name)
	r.closeDynamic(removed)

	ok := r.loadOne(plgDir, name)
	if !ok {
		return fmt.Errorf("reload plugin %s failed", name)
	}
	log.Printf("[plugin] reloaded: %s", name)
	return nil
}

// closeDynamic 释放动态加载插件的共享库句柄（dlclose）。
// Linux dlopen 对同一路径返回已加载的旧句柄，若不释放，插件二进制更新后
// 重载/卸载仍会执行旧代码。Go plugin.Open 路径（dynamicPlugin）不可卸载，跳过。
func (r *Registry) closeDynamic(p sdk.Plugin) {
	if p == nil {
		return
	}
	if c, ok := p.(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			log.Printf("[plugin] close dynamic %s: %v", p.Name(), err)
		}
	}
}

func (r *Registry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := make([]string, 0, len(r.plugins))
	for name := range r.plugins {
		list = append(list, name)
	}
	sort.Strings(list)
	return list
}

func (r *Registry) Get(name string) sdk.Plugin {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.plugins[name]
}

func (r *Registry) AutoRestartEnabled(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	enabled, ok := r.pluginAutoRestart[name]
	if !ok {
		return true
	}
	return enabled
}

func (r *Registry) IsDisabled(name string) bool {
	return r.isDisabled(name)
}

func (r *Registry) Enable(name string) error {
	if cfgReg := r.cfgRegistryOrNil(); cfgReg != nil {
		cfgReg.RemoveDisabledPlugin(name)
	}
	r.mu.Lock()
	delete(r.knownDisabled, name)
	r.mu.Unlock()
	plgDir := filepath.Join(r.pluginDirOrEmpty(), name)
	if r.loadOne(plgDir, name) {
		log.Printf("[plugin] enabled: %s", name)
		return nil
	}
	return fmt.Errorf("enable plugin %s failed", name)
}

func (r *Registry) Disable(name string) error {
	r.mu.Lock()
	p, ok := r.plugins[name]
	if ok {
		r.runStopHandlers(name)
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s for disable: %v", name, err)
		}
		delete(r.plugins, name)
		delete(r.sdkRefs, name)
		for i, inst := range r.instances {
			if inst.Name() == name {
				r.instances = append(r.instances[:i], r.instances[i+1:]...)
				break
			}
		}
	}
	r.knownDisabled[name] = true
	r.mu.Unlock()

	r.detachPlugin(name)

	if cfgReg := r.cfgRegistryOrNil(); cfgReg != nil {
		cfgReg.AddDisabledPlugin(name, "system")
	}
	log.Printf("[plugin] disabled: %s", name)
	return nil
}

// ---- PluginManager interface ----

// PluginManager interface

// IsBuiltinPlugin 判断插件是否为内置插件（有编译期工厂,由 init() 注册）。
// 内置插件只能禁用/启用，不能卸载。
func (r *Registry) IsBuiltinPlugin(name string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	_, ok := r.factories[name]
	r.mu.RUnlock()
	if ok {
		return true
	}
	_, ok = globalFactories.Load(name)
	return ok
}

// pluginInstalled 判断插件是否已安装（可用于禁用/启用等操作前的存在性校验）。
// 命中任一即视为已安装：
//  1. 已加载（plugins map 中）
//  2. 已注册工厂（内置插件，通过 init() 自注册，无需物理目录）
//  3. 插件目录 plgDir/<name> 存在（外部插件的安装目录）
func (r *Registry) pluginInstalled(name string) bool {
	if r == nil || name == "" {
		return false
	}
	r.mu.RLock()
	_, loaded := r.plugins[name]
	_, isFactory := r.factories[name]
	r.mu.RUnlock()
	if loaded || isFactory {
		return true
	}
	if _, ok := globalFactories.Load(name); ok {
		return true
	}
	if dir := r.pluginDirOrEmpty(); dir != "" {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

func (r *Registry) ListLoadedPlugins() []string { return r.List() }

func (r *Registry) ListDisabledPlugins() []sdk.DisabledPluginInfo {
	cfgReg := r.cfgRegistryOrNil()
	if cfgReg == nil {
		return nil
	}
	list, err := cfgReg.ListDisabledPlugins()
	if err != nil {
		return nil
	}
	result := make([]sdk.DisabledPluginInfo, len(list))
	for i, v := range list {
		result[i] = sdk.DisabledPluginInfo(v)
	}
	return result
}

func (r *Registry) IsPluginDisabled(name string) bool { return r.isDisabled(name) }

func (r *Registry) DisablePlugin(name, by string) error {
	// ★ 自锁守卫（与 StopPlugin 同一类，设计 §2.6.7.4）。
	//
	// 持久禁用 pluginmgr 比运行态关闭更糟：agent 从此**永远**失去插件管理能力
	// （重启也不恢复）。虽然 CLI/webui 可人工解禁，但 agent 侧是死锁。
	if err := r.refuseSelfLock(name, "禁用"); err != nil {
		return err
	}
	// 插件不存在（未安装）：拒绝并返回错误，避免把不存在的插件写进 disabled_plugins。
	// 判断标准：已加载 / 已注册工厂（内置）/ 插件目录存在，任一命中视为已安装。
	if !r.pluginInstalled(name) {
		return fmt.Errorf("plugin %s not installed", name)
	}
	// Check not disabling self if running
	if cfgReg := r.cfgRegistryOrNil(); cfgReg != nil {
		// If already disabled, no-op
		if cfgReg.IsPluginDisabled(name) {
			return fmt.Errorf("plugin %s already disabled", name)
		}
	}

	r.mu.Lock()
	p, ok := r.plugins[name]
	if ok {
		r.runStopHandlers(name)
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s for disable: %v", name, err)
		}
		delete(r.plugins, name)
		delete(r.sdkRefs, name)
		for i, inst := range r.instances {
			if inst.Name() == name {
				r.instances = append(r.instances[:i], r.instances[i+1:]...)
				break
			}
		}
	}
	r.knownDisabled[name] = true
	r.mu.Unlock()

	r.detachPlugin(name)

	if cfgReg := r.cfgRegistryOrNil(); cfgReg != nil {
		cfgReg.AddDisabledPlugin(name, by)
	}
	log.Printf("[plugin] disabled: %s (by %s)", name, by)
	return nil
}

func (r *Registry) EnablePlugin(name string) error { return r.Enable(name) }

// isRuntimeStopped 报告插件是否处于**运行态关闭**（设计 §2.6.7.3）。
func (r *Registry) isRuntimeStopped(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.runtimeStopped[name]
}

// IsRuntimeStopped 是 isRuntimeStopped 的公开形式（工具层与判据用）。
func (r *Registry) IsRuntimeStopped(name string) bool { return r.isRuntimeStopped(name) }

// IsPluginRuntimeStopped 实现 sdk.PluginManager（命名与该接口其他方法一致）。
func (r *Registry) IsPluginRuntimeStopped(name string) bool { return r.isRuntimeStopped(name) }

// StopPlugin 是**运行态**关闭：现在停掉，但**不改任何持久状态**（设计 §2.6.7.4）。
//
// ## 与 DisablePlugin 的区别（这是本方法存在的全部理由）
//
//	DisablePlugin  ⇒ 停掉 + 写 disabled_plugins（**持久**）
//	StopPlugin     ⇒ 停掉，**不写任何配置**（运行态，重启自然复位）
//
// 用户场景："我只是想临时关掉它看现象" —— 用 DisablePlugin 会顺带把
// 开机自启也关掉，用户下次开机会发现"怎么不启动了"。
//
// ## 为何拒绝关闭"插件管理自身"
//
// pluginmgr 被关掉后，agent 就**失去了插件管理能力**，而重新启动它
// **只能靠插件管理工具** —— 鸡生蛋，自锁且无解。
// 所以这条必须在**内核层面**拦死，不能靠 agent 自觉（agent 可以犯错）。
//
// 同理拒绝 webui：它是人机界面，关掉后人就看不见了（可被外部手段恢复，
// 但仍是坏体验，故一并拦）。
func (r *Registry) StopPlugin(name string) error {
	if err := r.refuseSelfLock(name, "关闭"); err != nil {
		return err
	}
	r.mu.Lock()
	p, ok := r.plugins[name]
	if !ok {
		r.mu.Unlock()
		// 不在运行：标记运行态关闭（幂等），使后续启动不会拉起它
		r.mu.Lock()
		r.runtimeStopped[name] = true
		r.mu.Unlock()
		return fmt.Errorf("plugin %s 未在运行（已标记为运行态关闭）", name)
	}
	r.runStopHandlers(name)
	stopErr := p.Stop()
	delete(r.plugins, name)
	delete(r.sdkRefs, name)
	for i, inst := range r.instances {
		if inst.Name() == name {
			r.instances = append(r.instances[:i], r.instances[i+1:]...)
			break
		}
	}
	r.runtimeStopped[name] = true
	r.mu.Unlock()

	// 摸掉注册面：否则旧闭包（工具/stage/通道）指向已死实例，
	// 模型仍能看到并调用它们，只能拿到 ErrProcessExited。
	// 与 ReloadOne/Disable 同一处置（那里不摸的后果见 ReloadOne 注释）。
	r.detachPlugin(name)

	if stopErr != nil {
		log.Printf("[plugin] runtime stop %s（Stop 报错）: %v", name, stopErr)
	}
	log.Printf("[plugin] runtime stopped: %s（不改持久状态）", name)
	return nil
}

// StartPlugin 是**运行态**启动：现在拉起，**不改持久状态**（设计 §2.6.7.4）。
//
// ## 为何不清除 disabled_plugins
//
// 与 EnablePlugin 的区别：EnablePlugin 会调 RemoveDisabledPlugin
// ⇒ 若插件本来被持久禁用，这个动作把它**解禁**了（改了用户决定）。
// StartPlugin 只清运行态门，持久禁用仍然生效：
//
//	· 被持久禁用的插件 ⇒ StartPlugin **拒绝**（提示先启用）
//	  理由：持久禁用是用户的明确决定，绕过它加载等于违背用户意图
//	· 只是运行态关闭的插件 ⇒ StartPlugin 正常拉起
//
// ## 重启后的语义
//
// runtimeStopped **不持久** ⇒ 重启后插件按 auto_start + disabled_plugins
// 正常决定。这正是"关闭 ≠ 以后都不启动"的落点。
func (r *Registry) StartPlugin(name string) error {
	// 持久禁用优先：用户的明确决定不能被运行态操作绕过
	if r.isDisabled(name) {
		return fmt.Errorf("plugin %s 已被**持久禁用**，请先用 plugin_enable 启用（运行态启动不会解除持久禁用）", name)
	}
	// 清运行态门（必须在 loadOne 之前，否则会被门挡住）
	r.mu.Lock()
	wasStopped := r.runtimeStopped[name]
	delete(r.runtimeStopped, name)
	r.mu.Unlock()

	// 已在运行 ⇒ 幂等成功（不重复加载）。
	//
	// ★ 判据必须看 **r.plugins**（真的加载了），不能看 PluginRuntime：
	// 后者在"工厂已注册但插件并未加载"时也返回 ok=true（它答的是
	// "这个插件**已安装**吗"，不是"**正在运行**吗"）。
	// 用它会让 StartPlugin 在插件其实已停止时误返回成功 —— 实测抓到
	// （TestStopThenStartRoundTrip：停止后启动，插件其实没被重新加载）。
	r.mu.RLock()
	_, running := r.plugins[name]
	r.mu.RUnlock()
	if running {
		return nil
	}

	plgDir := filepath.Join(r.pluginDirOrEmpty(), name)
	if r.loadOne(plgDir, name) {
		if wasStopped {
			log.Printf("[plugin] runtime started: %s（已清运行态关闭）", name)
		} else {
			log.Printf("[plugin] runtime started: %s", name)
		}
		return nil
	}
	// 加载失败 ⇒ 恢复运行态门，避免"启动失败但门被清了"导致状态不一致
	r.mu.Lock()
	if wasStopped {
		r.runtimeStopped[name] = true
	}
	r.mu.Unlock()
	return fmt.Errorf("启动插件 %s 失败（本身加载出错，或插件不存在）", name)
}

// refuseSelfLock 拦住会自锁的关闭/禁用操作（设计 §2.6.7.4）。
//
// 为何在内核层面拦而不是靠工具层校验：agent 可以犯错，
// 而这几个操作一旦执行就**无法从 agent 侧恢复**（自锁）。
// 内核拒绝是唯一可靠的位置。
func (r *Registry) refuseSelfLock(name, action string) error {
	switch name {
	case "pluginmgr":
		return fmt.Errorf("拒绝%s pluginmgr：它是 agent 的插件管理能力本身，"+
			"%s后 agent 再也无法启动任何插件（只能靠外部手段恢复）", action, action)
	case "webui":
		return fmt.Errorf("拒绝%s webui：它是人机界面，%s后用户看不到系统状态"+
			"（如需临时停用请用 plugin_disable，或从外部恢复）", action, action)
	}
	return nil
}

// StopAndUnload 停止并从注册表移除插件，但保留其配置表（config_<name>）。
// 供插件更新/升级流程使用：换 so/文件不动配置，重装后配置原样生效。
// 不执行 onRemove 回调（那是删除专用语义）。目录由调用方管理。
func (r *Registry) StopAndUnload(name string) error {
	r.mu.Lock()
	var unloaded sdk.Plugin
	p, ok := r.plugins[name]
	if ok {
		r.runStopHandlers(name)
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s for unload: %v", name, err)
		}
		delete(r.plugins, name)
		delete(r.sdkRefs, name)
		for i, inst := range r.instances {
			if inst.Name() == name {
				r.instances = append(r.instances[:i], r.instances[i+1:]...)
				break
			}
		}
		unloaded = p
	}
	r.mu.Unlock()

	r.detachPlugin(name)
	r.closeDynamic(unloaded)
	log.Printf("[plugin] unloaded (config kept): %s", name)
	return nil
}

// RemovePlugin 卸载插件：先停止（stop handlers + Stop），再执行插件注册的 onRemove
// 回调（删除专用，重载不触发），最后从注册表移除并清理禁用/工具注册/配置。
// 插件目录的物理删除由调用方（pluginmgr）负责。更新场景请用 StopAndUnload。
func (r *Registry) RemovePlugin(name string) error {
	r.mu.Lock()
	var removed sdk.Plugin
	p, ok := r.plugins[name]
	if ok {
		r.runStopHandlers(name)
		if err := p.Stop(); err != nil {
			log.Printf("[plugin] stop %s for remove: %v", name, err)
		}
		delete(r.plugins, name)
		delete(r.sdkRefs, name)
		for i, inst := range r.instances {
			if inst.Name() == name {
				r.instances = append(r.instances[:i], r.instances[i+1:]...)
				break
			}
		}
		removed = p
	}
	r.runOnRemoveHandlers(name)
	r.mu.Unlock()

	r.detachPlugin(name)
	if cfgReg := r.cfgRegistryOrNil(); cfgReg != nil {
		cfgReg.RemoveDisabledPlugin(name)
		cfgReg.RemovePlugin(name)
	}
	r.closeDynamic(removed)
	log.Printf("[plugin] removed: %s", name)
	return nil
}

func (r *Registry) ReloadPlugins() (string, error) { return r.Reload(r.pluginDirOrEmpty()) }

// PluginRuntime 返回单个插件的运行期状态。
//
// 这是「插件管理器能看到真实死活」的数据源。子进程模型下，
// 「注册表里有条目」不等于「进程还活着」；只读 plugin.json 的旧实现
// 无法区分两者，插件被 kill 后 WebUI 仍显示“正常”。
func (r *Registry) PluginRuntime(name string) (sdk.PluginRuntimeInfo, bool) {
	if name == "" {
		return sdk.PluginRuntimeInfo{}, false
	}

	r.mu.RLock()
	plg, loaded := r.plugins[name]
	_, hasFactory := r.factories[name]
	r.mu.RUnlock()
	if !hasFactory {
		_, hasFactory = globalFactories.Load(name)
	}

	installed := loaded || hasFactory
	var dirExists bool
	if dir := r.pluginDirOrEmpty(); dir != "" {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && st.IsDir() {
			dirExists = true
			installed = true
		}
	}
	if !installed {
		return sdk.PluginRuntimeInfo{}, false
	}

	info := sdk.PluginRuntimeInfo{
		Name:           name,
		Loaded:         loaded,
		Disabled:       r.isDisabled(name),
		Builtin:        hasFactory,
		AutoRestart:    r.AutoRestartEnabled(name),
		AutoStart:      true, // 缺省 true；下面按清单覆盖
		RuntimeStopped: r.IsPluginRuntimeStopped(name),
		CrashCount:     r.crashCount(name),
	}
	// auto_start 以清单为准（plugin.json 显式 false 才是按需）。
	// 目录不存在（纯内置）时保持缺省 true —— 内置插件本就随内核加载。
	if dirExists {
		if m, err := ReadManifest(filepath.Join(r.pluginDirOrEmpty(), name)); err == nil {
			info.AutoStart = m.ShouldAutoStart()
		}
	}

	switch {
	case hasFactory:
		info.Channel = "builtin"
	case dirExists:
		info.Channel = detectEntryKind(filepath.Join(r.pluginDirOrEmpty(), name)).String()
	}

	// 子进程插件报真实 PID 与存活；其余形态与 Loaded 同值（无独立进程）。
	type procStatus interface {
		PID() int
		Alive() bool
	}
	if ps, ok := plg.(procStatus); ok && loaded {
		info.PID = ps.PID()
		info.Alive = ps.Alive()
	} else {
		info.Alive = loaded
	}

	if sh := r.stageHostOrNil(); sh != nil {
		for _, def := range sh.GetToolDefs() {
			if def.Plugin == name {
				info.Tools = append(info.Tools, def.Name)
			}
		}
		sort.Strings(info.Tools)
	}
	return info, true
}

// ListPluginRuntimes 返回全部已知插件的运行期状态（含已安装未加载者）。
func (r *Registry) ListPluginRuntimes() []sdk.PluginRuntimeInfo {
	names := r.ListKnown()
	out := make([]sdk.PluginRuntimeInfo, 0, len(names))
	for _, name := range names {
		if info, ok := r.PluginRuntime(name); ok {
			out = append(out, info)
		}
	}
	return out
}

// crashCount 读取窗口内的崩溃计数（过期视为 0）。
func (r *Registry) crashCount(name string) int {
	r.crashMu.Lock()
	defer r.crashMu.Unlock()
	rec := r.procCrashes[name]
	if rec == nil || time.Since(rec.last) > procCrashWindow {
		return 0
	}
	return rec.count
}

// ListKnown 返回所有已知插件（已加载 + 已禁用 + 已安装但未加载）。
func (r *Registry) ListKnown() []string {
	r.mu.RLock()
	known := make(map[string]bool)
	for name := range r.plugins {
		known[name] = true
	}
	for name := range r.knownDisabled {
		known[name] = true
	}
	r.mu.RUnlock()

	if dir := r.pluginDirOrEmpty(); dir != "" {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() {
				known[e.Name()] = true
			}
		}
	}

	r.mu.RLock()
	for name := range r.factories {
		known[name] = true
	}
	r.mu.RUnlock()

	globalFactories.Range(func(key, val interface{}) bool {
		known[key.(string)] = true
		return true
	})

	list := make([]string, 0, len(known))
	for name := range known {
		list = append(list, name)
	}
	sort.Strings(list)
	return list
}

func (r *Registry) PluginMetas() map[string]sdk.PluginMeta {
	metas := make(map[string]sdk.PluginMeta)
	globalPluginMeta.Range(func(key, val interface{}) bool {
		metas[key.(string)] = val.(sdk.PluginMeta)
		return true
	})
	return metas
}

func (r *Registry) PluginDir() string {
	return r.pluginDirOrEmpty()
}

func (r *Registry) tryDynamic(plgDir, name string, config map[string]interface{}) (sdk.Plugin, error) {
	// 按 manifest entry 分派加载通道。
	//
	// C ABI 通道（.so/.dll/.dylib）已整体删除：外部插件统一走子进程，
	// 三套独立 ABI 实现收敛为单一 RPC 实现（§9.2）。
	if detectEntryKind(plgDir) == entryProc {
		plg, err := r.loadProc(plgDir, name, config)
		if err != nil {
			return nil, err
		}
		if plg != nil {
			log.Printf("[plugin] %s: 经 proc 通道加载（子进程）", name)
			return plg, nil
		}
		return nil, fmt.Errorf("plugin %s: entry 声明 %s 但未找到可用二进制", name, binEntry)
	}

	// 旧 .so/.dll 插件给明确错误，不静默跳过。
	// 静默跳过会让「插件目录在但没加载」看起来像配置问题，
	// 而实际原因是需要用新 hmapdev 重编。
	if hasLegacyCABIEntry(plgDir) {
		return nil, fmt.Errorf(
			"plugin %s: 检测到旧 C ABI 产物（plugin.so/.dll/.dylib）。"+
				"外部插件已改为子进程模式，请用新版 hmapdev 重编产出 %s"+
				"（业务代码无需修改）", name, binEntry)
	}

	// Lua 插件仍走解释器
	return tryLoadLua(plgDir, name, config)
}

func (r *Registry) readConfig(plgDir string) map[string]interface{} {
	cfg := map[string]interface{}{}
	data, err := os.ReadFile(filepath.Join(plgDir, "skill.json"))
	if err != nil {
		return cfg
	}
	var meta map[string]interface{}
	if err := json.Unmarshal(data, &meta); err == nil {
		return meta
	}
	return cfg
}
