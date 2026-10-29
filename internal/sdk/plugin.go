package sdk

import (
	"log"
	"sync"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// SDKVersion 是对外 SDK 版本号，与核心 meta.Version 保持一致。
var SDKVersion = pubsdk.SDKVersion

type Plugin interface {
	Name() string
	Start(sdk *PluginSDK) error
	Stop() error
}

type ToolHandler = pubsdk.ToolHandler
type StageHandler = pubsdk.StageHandler

type Stage = pubsdk.Stage

const (
	StageOnInput        = pubsdk.StageOnInput
	StagePreAction      = pubsdk.StagePreAction
	StagePostAction     = pubsdk.StagePostAction
	StageBeforeToolcall = pubsdk.StageBeforeToolcall
	StageAfterToolcall  = pubsdk.StageAfterToolcall
	StageBeforeOutput   = pubsdk.StageBeforeOutput
	StageAfterOutput    = pubsdk.StageAfterOutput
)

type StageContext = pubsdk.StageContext
type MemItem = pubsdk.MemItem
type ToolCall = pubsdk.ToolCall
type ToolResult = pubsdk.ToolResult
type ToolError = pubsdk.ToolError
type ToolDef = pubsdk.ToolDef
type IOInjector = pubsdk.IOInjector
type ToolRegistrar = pubsdk.ToolRegistrar
type StageRegistrar = pubsdk.StageRegistrar
type StageScope = pubsdk.StageScope

const (
	StageScopeGlobal   = pubsdk.StageScopeGlobal
	StageScopeOwnTools = pubsdk.StageScopeOwnTools
)

type APIRegistrar = pubsdk.APIRegistrar
type OutputChannelRegistrar = pubsdk.OutputChannelRegistrar
type OutputChannelUnregistrar = pubsdk.OutputChannelUnregistrar
type InputChannelRegistrar = pubsdk.InputChannelRegistrar
type ChannelDef = pubsdk.ChannelDef

// 中断优先级的取值再导出：内置插件用 sdk.PriorityL4 声明“立即打断”，
// 外部插件同名常量会被内核夹到 L3（见 core.interruptLevel / proc 桥）。
const (
	PriorityL1 = pubsdk.PriorityL1
	PriorityL2 = pubsdk.PriorityL2
	PriorityL3 = pubsdk.PriorityL3
	PriorityL4 = pubsdk.PriorityL4
)

// InjectOptions / 上下文策略常量：内置插件与外部插件必须用同一套类型与取值，
// 否则内核要认两份，而漏认会静默丢失标志位。
type InjectOptions = pubsdk.InjectOptions

const (
	ContextPolicyNone  = pubsdk.ContextPolicyNone
	ContextPolicyPrune = pubsdk.ContextPolicyPrune
)

// 召回策略取值：与 ContextPolicy 正交（裁剪 vs 召回）。
const (
	RecallPolicyNone = pubsdk.RecallPolicyNone
	RecallPolicyAuto = pubsdk.RecallPolicyAuto
)

type DisabledPluginInfo struct {
	Name       string `json:"name"`
	DisabledAt string `json:"disabled_at"`
	DisabledBy string `json:"disabled_by"`
	// Reason 是禁用原因（可空）。
	//
	// 人工禁用通常为空；**内核自动禁用**必须填（如与内核版本不兼容），
	// 让用户在界面上直接看到"为什么没了"，而不是去翻日志。
	Reason string `json:"reason,omitempty"`
}

// PluginMeta is the display-name metadata for a plugin (from plg.json / RegisterPluginMeta).
type PluginMeta struct {
	NameZh string `json:"name_zh"`
	NameEn string `json:"name_en"`
}

// PluginRuntimeInfo 是插件的**运行期**状态，与 plugin.json 里的静态元数据相对。
//
// 为何需要：子进程插件的进程可能已经死了而注册表里还有条目（或反过来，
// 崩溃摘除后注册表已无条目但目录还在）。此前 plugin_list / GET /plugins
// 只读 plugin.json，无论插件死活都返回同一份内容——WebUI 与模型都看不出
// 「已安装」与「正在运行」的区别，插件被 kill 后只表现为工具静默失败。
type PluginRuntimeInfo struct {
	Name string `json:"name"`
	// Loaded 表示注册表中存在该插件实例。
	Loaded bool `json:"loaded"`
	// Disabled 表示插件被显式禁用（不该运行）。
	Disabled bool `json:"disabled"`
	// Builtin 表示编译期内置插件（无独立进程）。
	Builtin bool `json:"builtin"`
	// Channel 是加载通道：proc（子进程）/ lua / builtin。
	Channel string `json:"channel"`
	// PID 是子进程插件的进程号；非子进程或已退出为 0。
	PID int `json:"pid"`
	// Alive 表示子进程仍存活；非子进程插件与 Loaded 同值。
	Alive bool `json:"alive"`
	// CrashCount 是最近窗口内的崩溃次数（0 表示健康）。
	CrashCount int `json:"crash_count"`
	// AutoRestart 表示崩溃后内核是否会自动拉起。
	AutoRestart bool `json:"auto_restart"`
	// AutoStart 声明该插件是否随内核启动（plugin.json 的 auto_start，缺省 true）。
	//
	// 为什么必须暴露：`Loaded=false` 有三种完全不同的含义，只看 loaded 无法区分：
	//   · auto_start=false 且未运行 → **按需插件正常待命**（不是故障）
	//   · auto_start=true  且未运行 → 启动失败/被运行态 stop（需要关注）
	//   · disabled=true              → 用户显式禁用（有意如此）
	// 前端把三者都显示成「未加载」，用户会误以为按需插件坏了。
	AutoStart bool `json:"auto_start"`
	// RuntimeStopped 表示插件被 plugin_stop **运行态**停掉（不写持久禁用清单），
	// 与 Disabled 的持久禁用区分：重启后它会照常按 auto_start 决定是否拉起。
	RuntimeStopped bool `json:"runtime_stopped"`
	// Tools 是该插件当前注册在内核里的工具名。
	Tools []string `json:"tools,omitempty"`
}

type PluginManager interface {
	ListLoadedPlugins() []string
	ListDisabledPlugins() []DisabledPluginInfo
	IsPluginDisabled(name string) bool
	// IsBuiltinPlugin 判断插件是否为内置插件（编译期工厂，init() 自注册）。
	// 内置插件只能禁用/启用，不能卸载。
	IsBuiltinPlugin(name string) bool
	DisablePlugin(name, by string) error
	EnablePlugin(name string) error
	// StopPlugin/StartPlugin 是**运行态**启停（设计 §2.6.7.3/2.6.7.4）：
	// 只改运行状态，不写 disabled_plugins。与 Disable/Enable（持久态）分开。
	StopPlugin(name string) error
	StartPlugin(name string) error
	IsPluginRuntimeStopped(name string) bool
	// RemovePlugin 卸载插件：先停止（stop handlers + Stop），再执行插件注册的
	// onRemove 回调（RegisterOnRemoveHandler），最后从注册表移除并清理配置表。
	// 目录删除由调用方负责。
	RemovePlugin(name string) error
	// StopAndUnload 停止并从注册表移除插件但保留配置表，供更新/升级流程使用：
	// 换产物不动配置，重装后配置原样生效。不触发 onRemove 回调。
	StopAndUnload(name string) error
	ReloadPlugins() (string, error)
	// ReloadOne 重载单个插件（停止后重新加载，处理 dlclose/dynamic 句柄）。
	ReloadOne(name string) error
	PluginMetas() map[string]PluginMeta
	PluginDir() string
	// PluginRuntime 返回单个插件的运行期状态（进程存活 / PID / 崩溃计数）。
	// 未安装的插件返回零值 + false。
	PluginRuntime(name string) (PluginRuntimeInfo, bool)
	// ListPluginRuntimes 返回全部已加载插件的运行期状态。
	ListPluginRuntimes() []PluginRuntimeInfo
}

type PluginSDK struct {
	*pubsdk.PluginSDK

	// name 是本插件的名字（同时是事件发布者身份）。
	//
	// ★ 为何必须存下来：总线闸门要判断“这个插件能不能发这个事件”，
	// 而之前三条路径（Publish / Subscribe / EventBus）都不带身份 ——
	// 不带身份会被当成内核内部而一律放行，等于给所有插件开了伪造后门。
	name     string
	settings SettingsAPI
	memory   MemoryAPI
	textMem  TextMemoryAPI
	docMem   DocMemoryAPI
	know     KnowledgeAPI
	llm      LLMAPI

	iom       *agentIO.IOManager
	eventBus  *events.Bus
	events    pubsdk.EventsAPI
	logger    *log.Logger
	pluginMgr PluginManager

	status     StatusAPI
	supervisor SupervisorAPI
	adapter    AdapterAPI
	tracker    TrackerAPI
	config     ConfigAPI
	tool       ToolAPI
	indexer    IndexerAPI

	selftestMu sync.Mutex
	selftest   *VirtualInstance
}

func (s *PluginSDK) PluginMgr() PluginManager { return s.pluginMgr }

// 以下访问器遮蔽公共 SDK 的同名方法，返回内置插件可用的全量接口。

func (s *PluginSDK) Settings() SettingsAPI { return s.settings }

// Events 返回**受限**事件总线（可 nil：未接总线时）。
//
// 与直接拿 *events.Bus 的区别：只能发布白名单类型或自己的命名空间
// （`plugin.<name>.*`），载荷出向经 security 过闸，订阅只开只读事件。
func (s *PluginSDK) Events() pubsdk.EventsAPI  { return s.events }
func (s *PluginSDK) Memory() MemoryAPI         { return s.memory }
func (s *PluginSDK) TextMemory() TextMemoryAPI { return s.textMem }
func (s *PluginSDK) DocMemory() DocMemoryAPI   { return s.docMem }
func (s *PluginSDK) Knowledge() KnowledgeAPI   { return s.know }

// LLM 返回大模型源。媒体字节请走 Memory().MediaBytes(ref)（CAS 已淘汰）。
func (s *PluginSDK) LLM() LLMAPI { return s.llm }

// ioAdapter 桥接 IOManager 到公共 SDK 的 IOInjector 接口，
// 确保外部插件通过 s.InjectText() 等方法的调用能被路由到内核 IO 层。
type ioAdapter struct{ iom *agentIO.IOManager }

func (a ioAdapter) InjectInterruptText(source, channel, text string) {
	a.InjectInterruptTextOpts(source, channel, text, pubsdk.InjectOptions{})
}

// InjectInterruptTextOpts 注入可抢占当前处理的中断文本，并声明记忆/裁剪行为。
func (a ioAdapter) InjectInterruptTextOpts(source, channel, text string, opts pubsdk.InjectOptions) {
	if a.iom != nil {
		a.iom.InjectInterruptTextOpts(source, channel, text, opts)
	}
}

// InjectInputSync 同步注入输入并等待回复（阻塞直至 agent 处理完成），返回回复文本。
func (a ioAdapter) InjectInputSync(source, channel, text string) string {
	return a.InjectInputSyncOpts(source, channel, text, pubsdk.InjectOptions{})
}

// InjectInputSyncOpts 同步注入输入并声明记忆/裁剪行为。
func (a ioAdapter) InjectInputSyncOpts(source, channel, text string, opts pubsdk.InjectOptions) string {
	if a.iom == nil {
		return ""
	}
	out := a.iom.InjectInputSyncToOpts(source, channel, "text", map[string]interface{}{
		"content": text,
	}, opts)
	if out == nil {
		return ""
	}
	reply, _ := out.Payload["content"].(string)
	return reply
}

func (a ioAdapter) InjectText(source, channel, text string) {
	a.InjectTextOpts(source, channel, text, pubsdk.InjectOptions{})
}

// InjectTextOpts 注入排队文本，并声明记忆/裁剪行为。
func (a ioAdapter) InjectTextOpts(source, channel, text string, opts pubsdk.InjectOptions) {
	if a.iom != nil {
		a.iom.InjectTextOpts(source, channel, text, opts)
	}
}

func (a ioAdapter) InjectTextNoMemory(source, channel, text string) {
	a.InjectTextOpts(source, channel, text, pubsdk.InjectOptions{NoMemory: true})
}

// InjectInputMedia 注入带媒体内容块的输入。
//
// blocks 放在 payload 的 media_blocks 里，由 eventloop 取出转进
// stageCtx.Extra——与用户直接发图走的是同一条通道，因此自动获得
// CAS 落盘与媒体记忆绑定。与 SetToolBlocks 的区别：后者只能在工具
// 调用内部用，且媒体要等到下一条 tool message 才到模型手上。
func (a ioAdapter) InjectInputMedia(source, channel, text string, blocks []pubsdk.ContentBlock) {
	a.InjectInputMediaOpts(source, channel, text, blocks, pubsdk.InjectOptions{})
}

// InjectInputMediaOpts 注入带媒体块的输入，并声明记忆/裁剪行为。
func (a ioAdapter) InjectInputMediaOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts pubsdk.InjectOptions) {
	if a.iom != nil {
		a.iom.InjectInputMediaOpts(source, channel, text, blocks, opts)
	}
}

// InjectInputMediaSync 注入带媒体内容块的输入并同步等待回复。
func (a ioAdapter) InjectInputMediaSync(source, channel, text string, blocks []pubsdk.ContentBlock) string {
	return a.InjectInputMediaSyncOpts(source, channel, text, blocks, pubsdk.InjectOptions{})
}

// InjectInputMediaSyncOpts 注入带媒体块的输入并同步等待回复，同时声明记忆/裁剪行为。
func (a ioAdapter) InjectInputMediaSyncOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts pubsdk.InjectOptions) string {
	if a.iom == nil {
		return ""
	}
	out := a.iom.InjectInputMediaSyncOpts(source, channel, text, blocks, opts)
	if out == nil {
		return ""
	}
	reply, _ := out.Payload["content"].(string)
	return reply
}

// InjectInterruptMedia 注入带媒体内容块的中断，可抢占当前 LLM 处理。
func (a ioAdapter) InjectInterruptMedia(source, channel, text string, blocks []pubsdk.ContentBlock) {
	a.InjectInterruptMediaOpts(source, channel, text, blocks, pubsdk.InjectOptions{})
}

// InjectInterruptMediaOpts 注入带媒体块的中断，并声明记忆/裁剪行为。
func (a ioAdapter) InjectInterruptMediaOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts pubsdk.InjectOptions) {
	if a.iom != nil {
		a.iom.InjectInterruptMediaOpts(source, channel, text, blocks, opts)
	}
}

// ContentBlock / ImageURL / AudioURL 是多模态内容块在插件边界上的类型。
//
// 别名到公共 SDK 而非另建一套：内置插件（webui/multimodal 等）与外部插件必须
// 用同一套结构，否则 resolveInput 的类型分支要认第三种类型，而漏认的后果是
// 媒体被静默丢弃。
type ContentBlock = pubsdk.ContentBlock
type ImageURL = pubsdk.ImageURL
type AudioURL = pubsdk.AudioURL

// SDKConfig holds all dependencies for creating a PluginSDK.
type SDKConfig struct {
	IOManager      *agentIO.IOManager
	EventBus       *events.Bus
	Memory         MemoryAPI
	TextMemory     TextMemoryAPI
	DocMemory      DocMemoryAPI
	Knowledge      KnowledgeAPI
	LLM            LLMAPI
	Settings       SettingsAPI
	Secrets        pubsdk.SecretsAPI
	RawFiles       pubsdk.RawFilesAPI
	RegTool        ToolRegistrar
	RegStage       StageRegistrar
	RegAPI         APIRegistrar
	RegOutput      OutputChannelRegistrar
	RegOutputUnreg OutputChannelUnregistrar
	RegInput       InputChannelRegistrar
	PluginMgr      PluginManager
	// KernelControl 是内核重启/关机通道（WebUI 用）。nil = 未接该能力。
	KernelControl KernelControlAPI

	Status     StatusAPI
	Supervisor SupervisorAPI
	Adapter    AdapterAPI
	Tracker    TrackerAPI
	Config     ConfigAPI
	Tool       ToolAPI
	Indexer    IndexerAPI
}

func New(name string, cfg SDKConfig) *PluginSDK {
	base := pubsdk.New(name, cfg.Settings, cfg.RegTool, cfg.RegStage, cfg.RegAPI, cfg.RegOutput)
	if cfg.Secrets != nil {
		base.SetSecretsAPI(cfg.Secrets)
	}
	if cfg.RawFiles != nil {
		base.SetRawFilesAPI(cfg.RawFiles)
	}
	// 反代声明收集：内置插件（无 plugin.json）在 Start 里用 RegisterProxy
	// 声明自己的服务，落到本包的登记表；外部插件走 plugin.json 自动发现。
	base.SetProxyRegistrar(func(svc string, d pubsdk.ProxyDef) { RegisterBuiltinProxy(name, svc, d) })
	if cfg.IOManager != nil {
		base.SetIOInjector(ioAdapter{iom: cfg.IOManager})
	}
	if cfg.RegInput != nil {
		base.SetInputChannelRegistrar(cfg.RegInput)
	}
	if cfg.RegOutputUnreg != nil {
		base.SetOutputChannelUnregistrar(cfg.RegOutputUnreg)
	}
	base.SetMemoryAPI(cfg.Memory)
	base.SetTextMemoryAPI(cfg.TextMemory)
	base.SetDocMemoryAPI(cfg.DocMemory)
	base.SetKnowledgeAPI(cfg.Knowledge)
	base.SetLLMAPI(cfg.LLM)
	if cfg.KernelControl != nil {
		base.SetKernelControlAPI(cfg.KernelControl)
	}
	return &PluginSDK{
		PluginSDK: base,
		name:      name,
		settings:  cfg.Settings,
		memory:    cfg.Memory,
		textMem:   cfg.TextMemory,
		docMem:    cfg.DocMemory,
		know:      cfg.Knowledge,
		llm:       cfg.LLM,

		iom:       cfg.IOManager,
		eventBus:  cfg.EventBus,
		events:    newEventsAPI(name, cfg.EventBus),
		logger:    log.Default(),
		pluginMgr: cfg.PluginMgr,

		status:     cfg.Status,
		supervisor: cfg.Supervisor,
		adapter:    cfg.Adapter,
		tracker:    cfg.Tracker,
		config:     cfg.Config,
		tool:       cfg.Tool,
		indexer:    cfg.Indexer,
	}
}

// Selftest 返回一个隔离的虚拟自检实例（healthcheck 等内置插件用），
// 完全独立于生产存储，不产生任何污染。首次调用创建，复用已存在实例；
// 每轮自检前调用 SelftestReset 重建以清空上轮测试数据。
func (s *PluginSDK) Selftest(scope string) (*VirtualInstance, error) {
	s.selftestMu.Lock()
	defer s.selftestMu.Unlock()
	if s.selftest == nil {
		vi, err := NewVirtualInstance(scope)
		if err != nil {
			return nil, err
		}
		s.selftest = vi
	}
	return s.selftest, nil
}

// SelftestReset 清理并重建隔离自检实例，用于每轮健康检查前重置状态。
func (s *PluginSDK) SelftestReset(scope string) error {
	s.selftestMu.Lock()
	defer s.selftestMu.Unlock()
	if s.selftest == nil {
		vi, err := NewVirtualInstance(scope)
		if err != nil {
			return err
		}
		s.selftest = vi
		return nil
	}
	vi, err := s.selftest.Reset(scope)
	if err != nil {
		return err
	}
	s.selftest = vi
	return nil
}

func (s *PluginSDK) Status() StatusAPI         { return s.status }
func (s *PluginSDK) Supervisor() SupervisorAPI { return s.supervisor }
func (s *PluginSDK) Adapter() AdapterAPI       { return s.adapter }
func (s *PluginSDK) Tracker() TrackerAPI       { return s.tracker }
func (s *PluginSDK) Config() ConfigAPI         { return s.config }
func (s *PluginSDK) Tool() ToolAPI             { return s.tool }
func (s *PluginSDK) Indexer() IndexerAPI       { return s.indexer }

func (s *PluginSDK) InjectInput(source, channel, eventType string, payload map[string]interface{}) {
	if s.iom != nil {
		s.iom.InjectInputTo(source, channel, eventType, payload)
	}
}

func (s *PluginSDK) InjectInputSync(source, channel, eventType string, payload map[string]interface{}) *agentIO.OutputEvent {
	if s.iom != nil {
		return s.iom.InjectInputSyncTo(source, channel, eventType, payload)
	}
	return nil
}

func (s *PluginSDK) InjectInterrupt(source, channel, eventType string, payload map[string]interface{}) {
	if s.iom != nil {
		if payload == nil {
			payload = map[string]interface{}{}
		}
		payload["type"] = eventType
		s.iom.InjectInterrupt(source, channel, payload)
	}
}

func (s *PluginSDK) InjectTextSync(source, channel, text string) *agentIO.OutputEvent {
	return s.InjectInputSync(source, channel, "text", map[string]interface{}{"content": text})
}

func (s *PluginSDK) InjectTextSyncNoMemory(source, channel, text string) *agentIO.OutputEvent {
	return s.InjectInputSync(source, channel, "text", map[string]interface{}{"content": text, "no_memory": true})
}

func (s *PluginSDK) OutputChan() <-chan *agentIO.OutputEvent {
	if s.iom != nil {
		return s.iom.OutputChan()
	}
	return nil
}

func (s *PluginSDK) RegisterChannel(name string, dev agentIO.Device) error {
	if s.iom != nil {
		return s.iom.RegisterDevice(dev)
	}
	return nil
}

func (s *PluginSDK) UnregisterChannel(name string) {
	if s.iom != nil {
		s.iom.UnregisterDevice(name)
	}
}

func (s *PluginSDK) ListChannels() []agentIO.ChannelInfo {
	if s.iom != nil {
		return s.iom.ListChannels()
	}
	return nil
}

// Publish 以**本插件身份**发布事件。
//
// ★ 必须带身份（pluginName）：总线的闸门要判断“这个插件能不能发这个
// 事件”。不带身份会被当成内核内部而一律放行 —— 那等于给所有插件
// 开了伪造系统事件的后门。
func (s *PluginSDK) Publish(evt *events.Event) {
	if s.eventBus == nil {
		return
	}
	if err := s.eventBus.PublishAs(events.Publisher{Name: s.name}, evt); err != nil {
		// 被拒时不能静默：插件否则以为“发了、没反应”
		log.Printf("[sdk] %s publish %q denied: %v", s.name, evt.Type, err)
	}
}

// EventBus 返回内核事件总线（nil 表示该部署未接总线）。
//
// ## ★ 为什么它仍然返回裸总线（但不再是一个漏洞）
//
// 有些能力要**把总线本身传下去**（例如升级闭环在替换二进制期间持续
// 发进度事件）。但裸总线本身不再能绕过策略：
//
//	闸门挂在 Bus.Publish / Bus.Subscribe 上（见 internal/events/bus.go）。
//	拿到 *Bus 的调用方直接 Publish，仍然会被当作**内核身份**吗？
//	不会——因为他们拿到的是同一个 *Bus，而 Bus.Publish 用空身份。
//
// ★★ 所以这里返回的是一个**带身份的视图**：订阅/发布都绑定本插件名。
//
//	之前返回 *events.Bus 的后果是任何插件都能调 bus.Publish(伪造事件)
//	而不经任何授权 —— 那正是“安全监控可被绕过”的根源。
type BusHandle struct {
	bus  *events.Bus
	name string
}

// Publish 以插件身份发布。
func (h BusHandle) Publish(evt *events.Event) error {
	if h.bus == nil {
		return nil
	}
	return h.bus.PublishAs(events.Publisher{Name: h.name}, evt)
}

// Subscribe 以插件身份订阅（被拒时返回 error）。
func (h BusHandle) Subscribe(eventType events.EventType, handler events.Handler) (func(), error) {
	if h.bus == nil {
		return func() {}, nil
	}
	return h.bus.SubscribeAs(events.Publisher{Name: h.name}, eventType, handler)
}

// EventBus 返回**带本插件身份**的总线视图。
//
// 拿到它也不能绕过闸门：Publish/Subscribe 都会带上插件名去过策略。
func (s *PluginSDK) EventBus() BusHandle { return BusHandle{bus: s.eventBus, name: s.name} }

// Subscribe 以**本插件身份**订阅事件。
//
// ★ 之前这里调的是裸 Bus.Subscribe —— 不查策略、不报错。
// 现在走 SubscribeAs，被拒时会记日志（不能用 error：旧调用点习惯
// 只拿一个 unsub 函数，改签名会波及几十处）。
func (s *PluginSDK) Subscribe(eventType events.EventType, handler events.Handler) func() {
	if s.eventBus == nil {
		return func() {}
	}
	unsub, err := s.eventBus.SubscribeAs(events.Publisher{Name: s.name}, eventType, handler)
	if err != nil {
		log.Printf("[sdk] %s subscribe %q denied: %v", s.name, eventType, err)
		return func() {}
	}
	return unsub
}

// SetToolBlocks 桥接到 IOManager：插件工具注入多模态块，process.go 消费。
func (a ioAdapter) SetToolBlocks(blocks []pubsdk.ContentBlock) {
	if a.iom == nil {
		return
	}
	ifaces := make([]interface{}, len(blocks))
	for i, b := range blocks {
		ifaces[i] = b
	}
	a.iom.SetToolBlocks(ifaces)
}

// SetToolBlocks 注入多模态内容块（图片/音频），内核在下一条 tool message
// 的 content 数组里带上这些块，让模型在后续轮次看到图/听到音频。
func (s *PluginSDK) SetToolBlocks(blocks []pubsdk.ContentBlock) {
	if s.iom != nil {
		ifaces := make([]interface{}, len(blocks))
		for i, b := range blocks {
			ifaces[i] = b
		}
		s.iom.SetToolBlocks(ifaces)
	}
}
