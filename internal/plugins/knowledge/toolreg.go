package knowledgeplugin

// toolreg.go —— 插件侧**工具注册所需的最小面**（不 import internal/sdk）。
//
// ## 为什么不是直接收 `*sdk.PluginSDK`
//
// `internal/sdk` 里有 `KnowledgeAPI`，而它的实现指向本插件 ⇒ 直接收 SDK
// 会形成 import 循环（sdk → knowledge → sdk）。
//
// 更本质的理由：插件**不需要知道 SDK 的整体**。它只需要"注册一个工具"这一
// 件事。把需求收成最小接口，好处是：
//
//	· 装配点可以把 sdk.PluginSDK 直接传进来（方法集兼容，无需适配器）
//	· 判据可以用一个三行的假实现驱动本文件（不需要整机装配）
//
// ★ 这不是"为测试留口子"：接口是**生产界面的形状**，装配点传的是真 SDK。

// ToolDef 是工具定义的**最小形状**（与 sdk.ToolDef 的同名字段一一对应）。
//
// 用最小形状而不是别名：插件不该依赖 SDK 的字段演化（SDK 加字段时
// 插件不需要重新编译就能继续工作）。
type ToolDef struct {
	Name         string
	Description  string
	Parameters   map[string]interface{}
	NoMemory     bool
	ParallelSafe bool
}

// ToolHandler 是工具执行体。
type ToolHandler func(args map[string]interface{}) (any, error)

// ToolRegistrar 是注册一个工具的能力。
//
// ★ 装配点用一行适配器把 `sdk.PluginSDK` 接上来（见 plugin.go 的
//
//	sdkToolRegistrar）—— 适配留**装配侧**，插件不认识 SDK 的具体类型。
type ToolRegistrar interface {
	RegisterTool(def ToolDef, handler ToolHandler) error
}
