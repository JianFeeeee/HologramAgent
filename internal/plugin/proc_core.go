package plugin

import (
	"github.com/JianFeeeee/HologramAgent/internal/plugin/proc"
	isdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// procCore 把内核的 *internal/sdk.PluginSDK 收窄成子进程插件可见的能力面。
//
// ❗ **必须用命名字段，不能嵌入** `*isdk.PluginSDK`：嵌入会让全部方法被提升，
// 外部插件通道就能经类型断言拿到 Supervisor()/Tracker()/Adapter()/Indexer()
// 这些内核内部机制——权限梯度退化成纸面约定。命名字段下只有下面显式写出的
// 方法存在，这才是 §3.8 说的「从 C ABI 表达能力的意外产物变成显式声明并强制的策略」。
//
// 另一个必要性：internal/sdk 的接口是公开 SDK 的**超集**（isdk.KnowledgeAPI
// 内嵌 pubsdk.KnowledgeAPI 再加 Stats()/Remove()，isdk.MemoryAPI 加 GraphData()，
// isdk.LLMAPI 加 Chat()/ReloadFromConfig()），Go 方法签名精确匹配下
// *isdk.PluginSDK 本就不满足 proc.CoreSDK。
//
// 内置插件走的仍是原路径（直接持 *isdk.PluginSDK，拿到全量接口），不受影响。
type procCore struct {
	sdk *isdk.PluginSDK
}

// newProcCore 包装内核 SDK 供子进程插件使用。
func newProcCore(s *isdk.PluginSDK) procCore { return procCore{sdk: s} }

func (c procCore) PluginName() string { return c.sdk.PluginName() }

// ---- 能力访问器：内部超集接口 → 公开 SDK 接口 ----
//
// nil 保护是必要的：corehandler 用 `if xxx == nil` 判断能力不可用并返回
// errUnavailable，若把「类型化的 nil」透过去，判空会失效——插件收到的是
// panic 而不是"能力不可用"。

// Secrets 转发到内核的**全局**变量库（进程级单例，不是 per-plugin 配置）。
func (c procCore) Secrets() pubsdk.SecretsAPI {
	if v := c.sdk.Secrets(); v != nil {
		return v
	}
	return nil
}

func (c procCore) Settings() pubsdk.SettingsAPI {
	if s := c.sdk.Settings(); s != nil {
		return s
	}
	return nil
}

func (c procCore) Memory() pubsdk.MemoryAPI {
	if m := c.sdk.Memory(); m != nil {
		return m
	}
	return nil
}

func (c procCore) TextMemory() pubsdk.TextMemoryAPI {
	if m := c.sdk.TextMemory(); m != nil {
		return m
	}
	return nil
}

func (c procCore) DocMemory() pubsdk.DocMemoryAPI {
	if m := c.sdk.DocMemory(); m != nil {
		return m
	}
	return nil
}

func (c procCore) Knowledge() pubsdk.KnowledgeAPI {
	if k := c.sdk.Knowledge(); k != nil {
		return k
	}
	return nil
}

func (c procCore) LLM() pubsdk.LLMAPI {
	if l := c.sdk.LLM(); l != nil {
		return l
	}
	return nil
}

func (c procCore) Social() pubsdk.SocialAPI {
	if s := c.sdk.Social(); s != nil {
		return s
	}
	return nil
}

func (c procCore) PluginMgr() pubsdk.PluginMgrAPI {
	if m := c.sdk.PluginMgr(); m != nil {
		return m
	}
	return nil
}

// ---- 注册面 ----

func (c procCore) RegisterTool(name string, def pubsdk.ToolDef, handler pubsdk.ToolHandler) error {
	return c.sdk.RegisterTool(name, def, handler)
}

func (c procCore) RegisterStage(stage pubsdk.Stage, handler pubsdk.StageHandler, scope ...pubsdk.StageScope) {
	c.sdk.RegisterStage(stage, handler, scope...)
}

func (c procCore) RegisterPluginAPI(name string) error {
	return c.sdk.RegisterPluginAPI(name)
}

func (c procCore) RegisterOutputChannel(name string, caps int, desc string, def pubsdk.ChannelDef, handler pubsdk.ToolHandler) error {
	return c.sdk.RegisterOutputChannel(name, caps, desc, def, handler)
}

func (c procCore) RegisterInputChannel(name string, def pubsdk.ChannelDef) error {
	return c.sdk.RegisterInputChannel(name, def)
}

// ---- IO 注入 ----

func (c procCore) InjectText(source, channel, text string) {
	c.sdk.InjectText(source, channel, text)
}

func (c procCore) InjectInterruptText(source, channel, text string) {
	c.sdk.InjectInterruptText(source, channel, text)
}

func (c procCore) InjectTextNoMemory(source, channel, text string) {
	c.sdk.InjectTextNoMemory(source, channel, text)
}

// ---- 带注入标志位（记忆/裁剪行为由插件在调用点声明）----

func (c procCore) InjectTextOpts(source, channel, text string, opts pubsdk.InjectOptions) {
	c.sdk.InjectTextOpts(source, channel, text, opts)
}

func (c procCore) InjectInterruptTextOpts(source, channel, text string, opts pubsdk.InjectOptions) {
	c.sdk.InjectInterruptTextOpts(source, channel, text, opts)
}

func (c procCore) InjectInputSyncOpts(source, channel, text string, opts pubsdk.InjectOptions) string {
	// 这里用公共 SDK 的三参数 + opts 形态（返回回复文本），
	// 不用内核内部那个 (eventType, payload) → *OutputEvent 的全量签名：
	// 它会把内核 IO 事件结构暴露给外部插件。
	return c.sdk.InjectInputSyncOpts(source, channel, text, opts)
}

// InjectInputSync 收窄为公开 SDK 的三参数文本形态。
//
// internal/sdk.PluginSDK 的同名方法是 (source, channel, eventType, payload)
// → *agentIO.OutputEvent，暴露了内核 IO 事件结构；外部插件只该看到回复文本。
// 取值方式与 C ABI 路径一致（internal/plugin/cabi/loader.go 的 case 47）。
func (c procCore) InjectInputSync(source, channel, text string) string {
	out := c.sdk.InjectInputSync(source, channel, "text", map[string]interface{}{
		"content": text,
	})
	if out == nil {
		return ""
	}
	reply, _ := out.Payload["content"].(string)
	return reply
}

// ---- 带媒体的 IO 注入 ----
//
// 三个方法都直接转调 internal/sdk 的同名方法：那一层已经是三参数 + blocks
// 的公开形态，不像 InjectInputSync 需要收窄。

func (c procCore) InjectInputMedia(source, channel, text string, blocks []pubsdk.ContentBlock) {
	c.sdk.InjectInputMedia(source, channel, text, blocks)
}

func (c procCore) InjectInputMediaSync(source, channel, text string, blocks []pubsdk.ContentBlock) string {
	return c.sdk.InjectInputMediaSync(source, channel, text, blocks)
}

func (c procCore) InjectInterruptMedia(source, channel, text string, blocks []pubsdk.ContentBlock) {
	c.sdk.InjectInterruptMedia(source, channel, text, blocks)
}

func (c procCore) InjectInputMediaOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts pubsdk.InjectOptions) {
	c.sdk.InjectInputMediaOpts(source, channel, text, blocks, opts)
}

func (c procCore) InjectInputMediaSyncOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts pubsdk.InjectOptions) string {
	return c.sdk.InjectInputMediaSyncOpts(source, channel, text, blocks, opts)
}

func (c procCore) InjectInterruptMediaOpts(source, channel, text string, blocks []pubsdk.ContentBlock, opts pubsdk.InjectOptions) {
	c.sdk.InjectInterruptMediaOpts(source, channel, text, blocks, opts)
}

// SetToolBlocks 转调 internal/sdk：插件工具注入的媒体块，内核在下一轮
// tool message 携带。
func (c procCore) SetToolBlocks(blocks []pubsdk.ContentBlock) {
	c.sdk.SetToolBlocks(blocks)
}

// ---- 生命周期 ----

func (c procCore) SetAutoRestart(enabled bool) { c.sdk.SetAutoRestart(enabled) }

// 编译期确认收窄面正好满足子进程插件的能力契约。
var _ proc.CoreSDK = procCore{}

// procPluginAdapter 把 *proc.Plugin 适配到 registry 的 sdk.Plugin 接口。
//
// 两者只差 Start 的参数类型：registry 传 *isdk.PluginSDK（全量能力），
// 而子进程插件只该拿到收窄后的 proc.CoreSDK。转接在此发生，
// 权限收窄就成了**类型系统强制**的事，而不是约定（§3.8）。
//
// Name/Stop/Close 经嵌入指针提升；Close 对 registry.closeDynamic 可见，
// 故重载时能真正 kill 子进程——对比 cabi 路径的 Close 只做 dlclose，
// 而 dlclose 对 Go c-shared 是 no-op（§1.1，热重载静默失效的根因）。
type procPluginAdapter struct {
	*proc.Plugin
}

// Start 把内核全量 SDK 收窄成子进程可见的能力面后启动进程。
func (a procPluginAdapter) Start(s *isdk.PluginSDK) error {
	return a.Plugin.Start(newProcCore(s))
}

// 编译期确认适配器满足 registry 的插件接口。
var _ isdk.Plugin = procPluginAdapter{}
