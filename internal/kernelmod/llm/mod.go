// Package llm 是 llm 模块：多源 Provider 管理（经 Lua 适配器做协议转换）。
//
// 依赖 config + lua：`initLLMProviders(cfg, luaVM, baseAPIKey)`。
package llm

import (
	"log"
	"os"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	luapkg "github.com/JianFeeeee/HologramAgent/internal/lua"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// Module 提供多源 provider 管理。
type Module struct{}

func (Module) Name() string       { return "llm" }
func (Module) Requires() []string { return []string{kernelmod.FacConfig, kernelmod.FacLua} }
func (Module) Provides() []string { return []string{kernelmod.FacLLM} }

func (Module) Init(ks *kernelboot.KernelState) error {
	// ★ 敏感信息防火模块的装配点。
	//
	// 原 main 注释："必须在任何 provider 注册之前装配"—— provider 的出向
	// 装饰器在 ProviderManager.Register 里读 security.Global()。装配晚了，
	// 先注册的那批 provider 就没被装饰（静默漏掉一条出网路径）。
	//
	// 实测：该装配**就在 initLLMProviders 内部**（不是 main 的独立语句）
	// ⇒ 声明式下它天然"紧邻"，不再依赖 main 里那两行的相对位置。
	ks.BaseAPIKey = resolveBaseAPIKey(ks.Cfg)
	ks.LLMManager = buildProviderManager(ks.Cfg, ks.LuaVM, ks.BaseAPIKey)
	ks.LLMProvider = ks.LLMManager.Default()
	return nil
}

// buildProviderManager 按配置注册全部 LLM 源（原 cmd/homed 的整块平移）。
func buildProviderManager(cfg *types.Config, luaVM *luapkg.VM, baseAPIKey string) *api.ProviderManager {
	providerMgr := api.NewProviderManager()
	adapterConcurrency := map[string]int{}
	for _, src := range cfg.LLM.Sources {
		if !api.IsValidSourceConfig(src.Name, src.BaseURL, src.Model, src.Adapter) {
			log.Printf("[homed] skip invalid llm source %q (base_url=%q model=%q adapter=%q)",
				src.Name, src.BaseURL, src.Model, src.Adapter)
			continue
		}
		key := src.APIKey
		if key == "" {
			key = baseAPIKey
		}
		luaProvider := api.NewLuaAdaptedProvider(api.BaseConfig{
			Model:         src.Model,
			BaseURL:       src.BaseURL,
			APIKey:        key,
			Temperature:   cfg.LLM.Temperature,
			MaxTokens:     cfg.LLM.MaxTokens,
			ContextWindow: src.ContextWindow,
			MaxConcurrent: src.MaxConcurrent,
			Priority:      src.Priority,
			Vision:        src.Vision,
			Audio:         src.Audio,
		}, luaVM, src.Name, src.Adapter)
		providerMgr.Register(src.Name, luaProvider)
		if src.Adapter != "" {
			adapterConcurrency[src.Adapter] += src.MaxConcurrent
		}
	}
	luaVM.ConfigureConcurrency(adapterConcurrency)
	if cfg.LLM.Provider != "" {
		providerMgr.SetDefault(cfg.LLM.Provider)
	}
	return providerMgr
}

// resolveBaseAPIKey 决定基础 key（配置 > LLM_API_KEY > DEEPSEEK_API_KEY）。
func resolveBaseAPIKey(cfg *types.Config) string {
	apiKey := cfg.LLM.APIKey
	if apiKey == "" {
		apiKey = os.Getenv("LLM_API_KEY")
	}
	if apiKey == "" {
		apiKey = os.Getenv("DEEPSEEK_API_KEY")
	}
	return apiKey
}

func init() { kernelmod.Register(Module{}) }
