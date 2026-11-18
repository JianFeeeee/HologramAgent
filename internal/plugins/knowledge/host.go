package knowledgeplugin

import "github.com/JianFeeeee/HologramAgent/internal/kernelapi"

// host.go —— 插件所需**宿主能力的最小面**（不 import internal/sdk）。
//
// ## 为什么不是直接收 `*sdk.PluginSDK`
//
// 两个理由，都是结构性的：
//
//  1. **循环依赖**：`internal/sdk` 的 `KnowledgeAPI` 指向本包的 `Store` 类型
//     （内置插件用的全量接口），所以本包不能 import SDK。
//  2. **归属**：插件只需要"配置 / 状态段 / 工具注册 / 向量能力"这四件事。
//     把需求收成最小接口，装配点（`internal/plugin/builtin/knowledgereg`）
//     用适配器接上真 SDK ⇒ 插件实现与宿主形状解耦。
//
// ★ 这不是为测试留后门：接口是**生产界面的形状**，装配点传的是真 SDK。

// Settings 是插件自己的配置表（`s.Settings()`）。
//
// ★ 为什么是 Settings 而不是内核配置：listen_addr / token 是**本插件的**配置，
//
//	住在 config_<name> 表里。读内核全局配置会让插件知道与自己无关的东西。
type Settings interface {
	Get(key string) (interface{}, error)
	// DataDir 是本实例的数据根目录（插件自己的文件放这里）。
	//
	// ★ 为什么不是内核路径常量：数据根目录由运维/宿主决定（`-data` 参数、
	//   容器挂载点），插件只该知道"我的根在它下面"。
	DataDir() string
	// RegisterDef 让设置页显示本插件的配置项（名字/类型/说明）。
	RegisterDef(def ConfigDef)
}

// ConfigDef 是配置项声明（与 sdk.ConfigDef 同形状）。
type ConfigDef struct {
	Key         string
	Default     interface{}
	Type        string
	DisplayName string
	Category    string
	Description string
	Secret      bool
}

// Status 是状态段注册面（`s.Status()`）。
//
// ★ 谁持有数据谁注册：知识库的统计由插件报进内核状态快照，
//
//	主 agent 一行都不参与（A-1 已把 KernelStatus 收成 Sections map）。
type Status interface {
	RegisterSection(key string, provider func() any)
}

// VectorSpace 是多模态向量空间（与 memory 共用同一份实例）。
//
// ★ 直接复用叶子层的 `kernelapi.MultimodalSpace`：向量空间是**共享基础设施**
//
//	（memory 与 knowledge 用同一份数据与同一套坐标），不是某个模块的私有实现。
//	它是"契约层"的类型，与"内核内部接口"不是一回事 —— 插件用一个共享向量
//	空间不需要知道内核怎么装配它。
type VectorSpace = kernelapi.MultimodalSpace

// Host 是插件需要的**全部**宿主能力。
type Host interface {
	ToolRegistrar
	Settings() Settings
	Status() Status
	// Multimodal 返回统一多模态向量空间（可能 nil）。
	Multimodal() VectorSpace
	// Media 返回媒体字节取回面（可能 nil ⇒ 媒体关联降级为只记 digest）。
	Media() MediaSource
}

// MediaSource 是媒体字节的取回面（`s.Memory().MediaBytes`）。
//
// ★ 为什么知识库需要它：`knowledge_create` 的 media_digests 是**块引用**
//
//	（模型给的短前缀），媒体字节住在图记忆的块里。知识条目自带一份副本
//	（CAS 淘汰不影响它），所以必须取一次原始字节。
type MediaSource interface {
	MediaBytes(ref string) (data []byte, mime string, ok bool, err error)
}
