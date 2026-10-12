package sdk

import (
	"log"
	"sync"

	agentIO "github.com/JianFeeeee/HomeAgent/internal/agent/io"
	"github.com/JianFeeeee/HomeAgent/internal/events"
	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
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
	settings SettingsAPI
	memory   MemoryAPI
	textMem  TextMemoryAPI
	docMem   DocMemoryAPI
	know     KnowledgeAPI
	llm      LLMAPI

	iom       *agentIO.IOManager
	eventBus  *events.Bus
	logger    *log.Logger
	pluginMgr PluginManager

	status     StatusAPI
	supervisor SupervisorAPI
	adapter    AdapterAPI
	tracker    TrackerAPI
	config     ConfigAPI
	tool       ToolAPI
	indexer    IndexerAPI
	terminal   TerminalAPI

	selftestMu sync.Mutex
	selftest   *VirtualInstance
}

func (s *PluginSDK) PluginMgr() PluginManager { return s.pluginMgr }

// 以下访问器遮蔽公共 SDK 的同名方法，返回内置插件可用的全量接口。

func (s *PluginSDK) Settings() SettingsAPI     { return s.settings }
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
	RegTool        ToolRegistrar
	RegStage       StageRegistrar
	RegAPI         APIRegistrar
	RegOutput      OutputChannelRegistrar
	RegOutputUnreg OutputChannelUnregistrar
	RegInput       InputChannelRegistrar
	PluginMgr      PluginManager

	Status     StatusAPI
	Supervisor SupervisorAPI
	Adapter    AdapterAPI
	Tracker    TrackerAPI
	Config     ConfigAPI
	Tool       ToolAPI
	Indexer    IndexerAPI
	Terminal   TerminalAPI
}

func New(name string, cfg SDKConfig) *PluginSDK {
	base := pubsdk.New(name, cfg.Settings, cfg.RegTool, cfg.RegStage, cfg.RegAPI, cfg.RegOutput)
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
	return &PluginSDK{
		PluginSDK: base,
		settings:  cfg.Settings,
		memory:    cfg.Memory,
		textMem:   cfg.TextMemory,
		docMem:    cfg.DocMemory,
		know:      cfg.Knowledge,
		llm:       cfg.LLM,

		iom:       cfg.IOManager,
		eventBus:  cfg.EventBus,
		logger:    log.Default(),
		pluginMgr: cfg.PluginMgr,

		status:     cfg.Status,
		supervisor: cfg.Supervisor,
		adapter:    cfg.Adapter,
		tracker:    cfg.Tracker,
		config:     cfg.Config,
		tool:       cfg.Tool,
		indexer:    cfg.Indexer,
		terminal:   cfg.Terminal,
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
func (s *PluginSDK) Terminal() TerminalAPI     { return s.terminal }

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

func (s *PluginSDK) Publish(evt *events.Event) {
	if s.eventBus != nil {
		s.eventBus.Publish(evt)
	}
}

func (s *PluginSDK) Subscribe(eventType events.EventType, handler events.Handler) func() {
	if s.eventBus != nil {
		return s.eventBus.Subscribe(eventType, handler)
	}
	return func() {}
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
