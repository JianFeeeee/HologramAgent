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

	agentAPI "github.com/JianFeeeee/HomeAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	internalConfig "github.com/JianFeeeee/HomeAgent/internal/config"
	"github.com/JianFeeeee/HomeAgent/internal/events"
	"github.com/JianFeeeee/HomeAgent/internal/knowledge"
	luaVM "github.com/JianFeeeee/HomeAgent/internal/lua"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	doc "github.com/JianFeeeee/HomeAgent/internal/memory/document"
	"github.com/JianFeeeee/HomeAgent/internal/memory/text"
	"github.com/JianFeeeee/HomeAgent/internal/plugin/proc"
	sdk "github.com/JianFeeeee/HomeAgent/internal/sdk"
	"github.com/JianFeeeee/HomeAgent/internal/tracker"
	"github.com/JianFeeeee/HomeAgent/pkg/types"
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
	ks         *knowledge.Store
	mgr        *agentAPI.ProviderManager
	cfgReg     *internalConfig.ConfigRegistry
	plgDir     string
	dataDir    string // 守护进程数据目录（注入给插件 SettingsAPI.DataDir）
	lua        *luaVM.VM
	baseKey    string

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
	termAPI   sdk.TerminalAPI

	// deviceAuth 回答"设备 deviceID 是否已授权给当前 agent"（D4）。
	//
	// 由 bootstrap 在 agent 建好之后注入（registry 本身早于 agent 构造，
	// 所以这里存的是**晚绑定**的闭包）。为 nil 时 ToolAPI.CanUse 对
	// 设备工具放行 —— 保持存量行为不变。
	deviceAuth func(deviceID string) bool

	knownDisabled map[string]bool
	allowlist     map[string]bool

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
		pluginHashes:      make(map[string]string),
		pluginChannels:    make(map[string]*pluginChannelSet),
	}
}

func (r *Registry) SetIOManager(iom *agentIO.IOManager)                     { r.iom = iom }
func (r *Registry) SetEventBus(evBus *events.Bus)                           { r.evBus = evBus }
func (r *Registry) SetMemory(memDB *memory.GraphDB)                         { r.memDB = memDB }
func (r *Registry) SetTextMemory(tm *text.Memory)                           { r.textMem = tm }
func (r *Registry) SetDocStore(ds *doc.Store)                               { r.docStore = ds }
func (r *Registry) SetKnowledge(ks *knowledge.Store)                        { r.ks = ks }
func (r *Registry) SetProviderManager(mgr *agentAPI.ProviderManager)        { r.mgr = mgr }
func (r *Registry) SetConfigRegistry(cfgReg *internalConfig.ConfigRegistry) { r.cfgReg = cfgReg }
func (r *Registry) SetPluginDir(dir string)                                 { r.plgDir = dir }
func (r *Registry) SetDataDir(dir string)                                   { r.dataDir = dir }
func (r *Registry) SetLuaVM(vm *luaVM.VM)                                   { r.lua = vm }
func (r *Registry) SetBaseAPIKey(key string)                                { r.baseKey = key }
func (r *Registry) SetToolRegistrar(fn sdk.ToolRegistrar)                   { r.regTool = fn }
func (r *Registry) SetStageRegistrar(fn sdk.StageRegistrar)                 { r.regStage = fn }
func (r *Registry) SetAPIRegistrar(fn sdk.APIRegistrar)                     { r.regAPI = fn }
func (r *Registry) SetToolCleaner(tc PluginToolCleaner)                     { r.toolCleaner = tc }
func (r *Registry) SetStatusProvider(sp sdk.StatusAPI)                      { r.status = sp }
func (r *Registry) SetSupervisor(sup sdk.SupervisorAPI)                     { r.sup = sup }
func (r *Registry) SetTracker(trk *tracker.Tracker)                         { r.trk = trk }
func (r *Registry) SetConfig(cfg *types.Config)                             { r.cfg = cfg }
func (r *Registry) SetStageHost(sh sdk.ToolSource)                          { r.stageHost = sh }

// SetDeviceAuthQuery 注入"设备是否已授权"的查询（D4）。
//
// 由 bootstrap 在 agent 构造完成后调用。注入后，ToolAPI.CanUse 才会
// 对未授权设备返回 false —— 此前 ToolAPI 路径**完全不过授权闸**。
func (r *Registry) SetDeviceAuthQuery(fn func(deviceID string) bool) {
	r.deviceAuth = fn
	sdk.SetDeviceAuthQuery(fn)
}
func (r *Registry) SetIndexer(idx *memory.Indexer)   { r.idx = idx }
func (r *Registry) SetTerminalAPI(t sdk.TerminalAPI) { r.termAPI = t }

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

func (r *Registry) buildSDK(name string) *sdk.PluginSDK {
	sett := sdk.NewSettings(name, r.cfgReg)
	if sd, ok := sett.(interface{ SetDataDir(string) }); ok {
		// 插件专属数据目录：<data>/plugin_data/<name>，保证存在
		dir := filepath.Join(r.dataDir, "plugin_data", name)
		os.MkdirAll(dir, 0755)
		sd.SetDataDir(dir)
	}

	regTool := r.regTool
	if regTool == nil {
		regTool = func(toolName string, def sdk.ToolDef, handler sdk.ToolHandler) error {
			return nil
		}
	}
	regStage := r.regStage
	if regStage == nil {
		regStage = func(stage sdk.Stage, handler sdk.StageHandler) {}
	}
	// stage handler 注册时带上归属插件名，使卸载/崩溃时能成组摘除。
	// StageHost 实现了 RegisterStageFor；其他实现（测试替身）退回无归属注册。
	if h, ok := r.stageRegistrarFor(); ok {
		regStage = func(stage sdk.Stage, handler sdk.StageHandler) {
			h(name, stage, handler)
		}
	}
	regAPI := r.regAPI
	if regAPI == nil {
		regAPI = func(name string) error { return nil }
	}

	regOutput := func(chName string, caps int, desc string, def sdk.ChannelDef, handler sdk.ToolHandler) error {
		if r.iom == nil {
			return nil
		}
		if err := r.iom.RegisterDevice(&channelDevice{
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
		if _, ok := r.iom.LookupInputChannel(chName); !ok {
			_ = r.iom.RegisterInputChannelFrom(name, chName, agentIO.ChannelDef(def))
		}
		r.noteChannel(name, chName, true)
		return nil
	}

	regOutputUnreg := func(chName string) error {
		if r.iom == nil {
			return nil
		}
		r.iom.UnregisterDevice(chName)
		r.forgetChannel(name, chName)
		return nil
	}

	regInput := func(chName string, def sdk.ChannelDef) error {
		if r.iom == nil {
			return nil
		}
		// inputch 是最基本的输入路由单位：登记**归属插件**，便于父 agent 看清
		// "哪个插件的哪个 inputch 划给了谁"（一个插件可注册多个 inputch）。
		_ = r.iom.RegisterInputChannelFrom(name, chName, agentIO.ChannelDef(def))
		r.noteChannel(name, chName, false)
		return nil
	}

	return sdk.New(name, sdk.SDKConfig{
		IOManager: r.iom,
		EventBus:  r.evBus,
		// 块自带负载（媒体字节在块的 Data），不再需要独立的媒体存储句柄。
		// 插件名仅用于日志溯源（哪个插件写的块）。
		Memory:         sdk.NewGraphMemoryWithPlugin(name, r.memDB),
		TextMemory:     sdk.NewTextMemoryWithPlugin(name, r.textMem),
		DocMemory:      sdk.NewDocMemoryWithPlugin(name, r.docStore),
		Knowledge:      sdk.NewKnowledge(r.ks),
		LLM:            sdk.NewLLM(r.mgr, r.cfgReg, r.lua, r.baseKey),
		Settings:       sett,
		RegTool:        regTool,
		RegStage:       regStage,
		RegAPI:         regAPI,
		RegOutput:      regOutput,
		RegOutputUnreg: regOutputUnreg,
		RegInput:       regInput,
		PluginMgr:      r,

		Status:     r.status,
		Supervisor: r.sup,
		Adapter:    sdk.NewAdapter(r.lua),
		Tracker:    r.trk,
		Config:     sdk.NewConfig(r.cfg),
		Tool:       sdk.NewTool(r.stageHost, r.iom),
		Indexer:    sdk.NewIndexer(r.idx),
		Terminal:   r.termAPI,
	})
}

func (r *Registry) Load(dir string) error {
	if dir == "" {
		dir = r.plgDir
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
	if r.cfgReg == nil {
		return false
	}
	return r.cfgReg.IsPluginDisabled(name)
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
	if r.stageHost == nil {
		return nil, false
	}
	if h, ok := r.stageHost.(interface {
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
	if set == nil || r.iom == nil {
		return nil
	}
	var released []string
	for ch := range set.outputs {
		r.iom.UnregisterDevice(ch)
		released = append(released, ch)
	}
	for ch := range set.inputs {
		r.iom.UnregisterInputChannel(ch)
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
	if r.toolCleaner != nil {
		r.toolCleaner.UnregisterPluginTools(name)
		if sc, ok := r.toolCleaner.(PluginStageCleaner); ok {
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
		dir = r.plgDir
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

func (r *Registry) ReloadOne(name string) error {
	plgDir := filepath.Join(r.plgDir, name)

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
	if r.cfgReg != nil {
		r.cfgReg.RemoveDisabledPlugin(name)
	}
	r.mu.Lock()
	delete(r.knownDisabled, name)
	r.mu.Unlock()
	plgDir := filepath.Join(r.plgDir, name)
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

	if r.cfgReg != nil {
		r.cfgReg.AddDisabledPlugin(name, "system")
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
	if r.plgDir != "" {
		if fi, err := os.Stat(filepath.Join(r.plgDir, name)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

func (r *Registry) ListLoadedPlugins() []string { return r.List() }

func (r *Registry) ListDisabledPlugins() []sdk.DisabledPluginInfo {
	if r.cfgReg == nil {
		return nil
	}
	list, err := r.cfgReg.ListDisabledPlugins()
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
	// 插件不存在（未安装）：拒绝并返回错误，避免把不存在的插件写进 disabled_plugins。
	// 判断标准：已加载 / 已注册工厂（内置）/ 插件目录存在，任一命中视为已安装。
	if !r.pluginInstalled(name) {
		return fmt.Errorf("plugin %s not installed", name)
	}
	// Check not disabling self if running
	if r.cfgReg != nil {
		// If already disabled, no-op
		if r.cfgReg.IsPluginDisabled(name) {
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

	if r.cfgReg != nil {
		r.cfgReg.AddDisabledPlugin(name, by)
	}
	log.Printf("[plugin] disabled: %s (by %s)", name, by)
	return nil
}

func (r *Registry) EnablePlugin(name string) error { return r.Enable(name) }

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
	if r.cfgReg != nil {
		r.cfgReg.RemoveDisabledPlugin(name)
		r.cfgReg.RemovePlugin(name)
	}
	r.closeDynamic(removed)
	log.Printf("[plugin] removed: %s", name)
	return nil
}

func (r *Registry) ReloadPlugins() (string, error) { return r.Reload(r.plgDir) }

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
	if r.plgDir != "" {
		if st, err := os.Stat(filepath.Join(r.plgDir, name)); err == nil && st.IsDir() {
			dirExists = true
			installed = true
		}
	}
	if !installed {
		return sdk.PluginRuntimeInfo{}, false
	}

	info := sdk.PluginRuntimeInfo{
		Name:        name,
		Loaded:      loaded,
		Disabled:    r.isDisabled(name),
		Builtin:     hasFactory,
		AutoRestart: r.AutoRestartEnabled(name),
		CrashCount:  r.crashCount(name),
	}

	switch {
	case hasFactory:
		info.Channel = "builtin"
	case dirExists:
		info.Channel = detectEntryKind(filepath.Join(r.plgDir, name)).String()
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

	if r.stageHost != nil {
		for _, def := range r.stageHost.GetToolDefs() {
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

	if r.plgDir != "" {
		entries, _ := os.ReadDir(r.plgDir)
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
	return r.plgDir
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
