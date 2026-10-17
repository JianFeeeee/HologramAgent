// Package stagehost 是 stage-host 模块：阶段管道（StageHost）+ 插件注册表 + SDK 接线。
//
// ## 依赖
//
// 收 iom/evBus/mem.db/textMem/docStore/knowledge/providerMgr
// ⇒ 声明 kernel-io / memory / textmem / docmem / knowledge / llm。
//
// ★ 顺序上它在 llm 之后（providerMgr 是它的参数）—— 这不是"刻意安排"，
// 而是签名直接推出来的。
package stagehost

import (
	"log"
	"strings"

	"github.com/JianFeeeee/HomeAgent/internal/config"

	agentCore "github.com/JianFeeeee/HomeAgent/internal/agent/core"
	"github.com/JianFeeeee/HomeAgent/internal/kernelboot"
	"github.com/JianFeeeee/HomeAgent/internal/kernelmod"
	"github.com/JianFeeeee/HomeAgent/internal/plugin"
	cli "github.com/JianFeeeee/HomeAgent/internal/plugins/cli"
	webui "github.com/JianFeeeee/HomeAgent/internal/plugins/webui"
	"github.com/JianFeeeee/HomeAgent/internal/sdk"
)

// Module 提供阶段管道与插件注册表。
type Module struct{}

func (Module) Name() string { return "stage-host" }
func (Module) Requires() []string {
	return []string{kernelmod.FacKernelIO, kernelmod.FacMemory, kernelmod.FacTextMem,
		kernelmod.FacDocStore, kernelmod.FacKnowledge, kernelmod.FacLLM}
}
func (Module) Provides() []string { return []string{kernelmod.FacStageHost} }

func (Module) Init(ks *kernelboot.KernelState) error {
	stageHost := agentCore.NewStageHost()

	pluginReg := plugin.NewRegistry()
	pluginReg.SetIOManager(ks.IO)
	pluginReg.SetEventBus(ks.EventBus)
	pluginReg.SetMemory(ks.Memory.DB)
	pluginReg.SetTextMemory(ks.TextMemory)
	pluginReg.SetDocStore(ks.DocStore)
	pluginReg.SetKnowledge(ks.Knowledge)
	pluginReg.SetProviderManager(ks.LLMManager)
	pluginReg.SetConfigRegistry(ks.CfgReg)
	pluginReg.SetPluginDir(ks.Cfg.Plugin.Dir)
	pluginReg.SetDataDir(ks.DataDir) // 插件 SettingsAPI.DataDir() 的数据根目录

	// Wire registration callbacks: plugins' RegisterTool/RegisterStage → StageHost
	pluginReg.SetToolRegistrar(func(name string, def sdk.ToolDef, handler sdk.ToolHandler) error {
		log.Printf("[homed] SetToolRegistrar registering tool: %s (plugin=%s)", name, def.Plugin)
		return stageHost.RegisterTool(name, def, handler)
	})
	pluginReg.SetStageRegistrar(func(stage sdk.Stage, handler sdk.StageHandler) {
		stageHost.RegisterStage(stage, handler)
	})
	pluginReg.SetAPIRegistrar(func(name string) error { return nil })
	pluginReg.SetToolCleaner(stageHost)

	ks.StageHost = stageHost
	ks.PluginRegistry = pluginReg

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
