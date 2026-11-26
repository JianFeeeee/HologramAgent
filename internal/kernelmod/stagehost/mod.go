// Package stagehost 是 stage-host 模块：**阶段管道** + 插件注册表 + SDK 接线。
//
// ## 工具登记不在这里（2026-11-25 拆分）
//
// 用户裁定：「tool 应该是一个独立的模块，与 stagehost 在内核通过 kstate 协作」。
// 工具登记簿（名字/schema/handler）已搬到 `kernelmod/tool`（`FacToolReg`）；
// 本模块只保留阶段管道。两者在**本模块的 Init 里接线**：插件注册回调
// 分别把这些东西送到各自的家。
//
// ## 依赖
//
// 收 iom/evBus/providerMgr —— **记忆不再由内核分发**（归主 agent 自己装配）
// ⇒ 声明 kernel-io / memory / textmem / docmem / knowledge / llm。
//
// ★ 顺序上它在 llm 之后（providerMgr 是它的参数）—— 这不是"刻意安排"，
// 而是签名直接推出来的。
package stagehost

import (
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	agentProvider "github.com/JianFeeeee/HologramAgent/internal/agent/provider"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"log"
	"strings"

	"github.com/JianFeeeee/HologramAgent/internal/config"

	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	cli "github.com/JianFeeeee/HologramAgent/internal/plugins/cli"
	webui "github.com/JianFeeeee/HologramAgent/internal/plugins/webui"
	"github.com/JianFeeeee/HologramAgent/internal/promptsec"
	"github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// Module 提供阶段管道与插件注册表。
type Module struct{}

func (Module) Name() string { return "stage-host" }
func (Module) Requires() []string {
	return []string{kernelmod.FacKernelIO, kernelmod.FacLLM}
}
func (Module) Provides() []string { return []string{kernelmod.FacStageHost} }

func (Module) Init(ks *kstate.State) error {
	// 依赖的产物从挂载点取（编译期只认 kernelapi 的接口/具名设施，不 import 实现）：
	// 这里的断言是**装配契约**——缺了就响亮失败，不许静默降级。
	iom := kernelmod.MustValue[*agentIO.IOManager](ks, kernelapi.FacKernelIO)
	// ★ 取的是**代理面**（FacEventBus），不是裸总线（第⑥次裁定）。
	evBus, _ := kernelmod.MustValueAny(ks, kernelapi.FacEventBus).(kernelapi.EventBus)
	providerMgr := kernelmod.MustValue[*agentProvider.ProviderManager](ks, kernelapi.FacLLM)

	stageHost := kit.NewStageHost()
	// ★ 工具登记簿是**独立模块**产出的设施（`FacToolReg`），本模块只接线。
	//
	// 此前它住在这个模块里、和阶段管道是同一个对象 —— 于是"工具表由谁产"
	// 永远讲不清（用户裁定 2026-11-25）。
	toolReg := kernelmod.MustValue[*kit.ToolRegistry](ks, kernelapi.FacToolReg)

	// 插件声明式系统提示词段的注册表：本模块产出，挂到内核挂载点。
	//
	// ★ 不往下传参：读方（agent 模块、SDK）自己去 ks 取 ——
	// 与 StageHost / PluginRegistry 完全同型。往构造点传指针会让
	// "记得接这一根线"成为每种 agent 类型的义务（那正是之前的老毛病）。
	promptSections := promptsec.NewRegistry()
	ks.Provide(kernelapi.FacPromptSections, promptSections)

	pluginReg := plugin.NewRegistry()
	pluginReg.SetIOManager(iom)
	pluginReg.SetEventBus(evBus)
	// 多模态向量空间：知识库插件要用它建稠密索引（经 s.Multimodal()）。
	// 未启用时 Require 拿不到 ⇒ 传 nil ⇒ 插件回落到词法路（行为不变）。
	if impl, ok := ks.Require(kernelapi.FacMultimodal); ok {
		if mm, _ := impl.(kernelapi.MultimodalSpace); mm != nil {
			pluginReg.SetMultimodal(mm)
		}
	}
	pluginReg.SetProviderManager(providerMgr)
	pluginReg.SetConfigRegistry(ks.CfgReg)
	pluginReg.SetPluginDir(ks.Cfg.Plugin.Dir)
	pluginReg.SetDataDir(ks.DataDir) // 插件 SettingsAPI.DataDir() 的数据根目录

	// Wire registration callbacks: plugins' RegisterTool/RegisterStage → StageHost
	pluginReg.SetToolRegistrar(func(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
		log.Printf("[homed] SetToolRegistrar registering tool: %s (plugin=%s)", name, def.Plugin)
		return toolReg.RegisterTool(name, def, handler)
	})
	pluginReg.SetStageRegistrar(func(stage sdk.Stage, handler sdk.StageHandler) {
		stageHost.RegisterStage(stage, handler)
	})
	pluginReg.SetAPIRegistrar(func(name string) error { return nil })
	pluginReg.SetToolCleaner(toolReg)

	ks.Provide(kernelapi.FacStageHost, stageHost)
	ks.Provide(kernelapi.FacPluginRegistry, pluginReg)

	// ★ 这两行必须在插件 Start（loadPlugins）**之前**设置：
	// CLI 插件的 unix socket 路径，与 webui 的监听地址覆盖。
	//
	// 归入本模块的理由：它们都是**插件读的配置**，与插件系统同生共死。
	// （原先它们在 main 里紧邻 loadPlugins —— 声明式下"同模块"是等价保证。）
	cli.DefaultSocket = ks.CLISocket
	webui.SetListenOverride(resolveWebUIOverride(ks.CfgReg, ks.HTTPAddr))
	return nil
}

// resolveWebUIOverride 解析 webui 监听地址的覆盖值，空串表示不覆盖。
//
// 优先级：CLI --webui > 核心配置 webui.listen_addr（仅当被改成非内置默认值）。
// 两者都不给时由 webui 插件自己的 settings["addr"] 决定。
//
// 为什么不写成"内核在插件加载前 Set 插件 settings['addr']"：那时
// config_webui 表还没建（表只在插件注册 def 时创建），PluginSettings.Set 的
// INSERT 会失败而错误被忽略，随后插件 Start 里 RegisterDef 才建表并写入默认
// :8080 —— 于是 CLI --webui 与 webui.listen_addr **一直是死配置**。
func resolveWebUIOverride(cfgReg *config.ConfigRegistry, httpAddr string) string {
	if strings.TrimSpace(httpAddr) != "" {
		return strings.TrimSpace(httpAddr)
	}
	// webui.listen_addr 的播种默认值就是 ":8080"；与默认值相同视为"未配置"，
	// 否则会把用户在设置页里改过的插件 addr 顶掉。
	if v := strings.TrimSpace(cfgReg.GetString("webui.listen_addr", ":8080")); v != "" && v != ":8080" {
		return v
	}
	return ""
}

func init() { kernelmod.Register(Module{}) }
