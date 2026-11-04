// Package llm 是 llm 模块：多源 Provider 管理（经 Lua 适配器做协议转换）。
//
// 依赖 config + lua：`initLLMProviders(cfg, luaVM, baseAPIKey)`。
package llm

import (
	"github.com/JianFeeeee/HologramAgent/internal/security"
	"log"
	"os"

	"github.com/JianFeeeee/HologramAgent/internal/agent/api"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
	luapkg "github.com/JianFeeeee/HologramAgent/internal/lua"
	"github.com/JianFeeeee/HologramAgent/pkg/types"
)

// Module 提供多源 provider 管理。
type Module struct{}

func (Module) Name() string { return "llm" }
func (Module) Requires() []string {
	// ★ FacSecurity（2026-11-04 补）：这是**真实存在**的依赖。
	//
	// ProviderManager.Register 在注册每个 provider 时套出向装饰器（明文→别名），
	// 而装饰器需要变量库。此前那份变量库来自进程级单例 security.Global() ——
	// 装配晚了也"恰好"能读到（因为单例先装了），于是顺序错误被掩盖。
	//
	// 单例删除后，变量库必须**在注册前**由 SetSecretStore 注入。声明依赖
	// FacSecurity 让依赖图保证 security 先于本模块完成，而不是靠"碰巧"。
	return []string{kernelmod.FacConfig, kernelmod.FacLua, kernelmod.FacSecurity}
}
func (Module) Provides() []string { return []string{kernelmod.FacLLM} }

func (Module) Init(ks *kstate.State) error {
	// ★ 敏感信息防火模块的装配点。
	//
	// 原 main 注释："必须在任何 provider 注册之前装配"—— provider 的出向
	// 装饰器在 ProviderManager.Register 里读 security.Global()。装配晚了，
	// 先注册的那批 provider 就没被装饰（静默漏掉一条出网路径）。
	//
	// 实测：该装配**就在 initLLMProviders 内部**（不是 main 的独立语句）
	// ⇒ 声明式下它天然"紧邻"，不再依赖 main 里那两行的相对位置。
	// ★ 注入必须**早于任何 provider 注册**：Register 时才会套装饰器，
	// 注入晚了先注册的那批就没被装饰（静默漏掉一条出网路径）。
	//
	// 依赖图（Requires 里的 FacSecurity）保证 ks.Security 此时非 nil。
	ks.BaseAPIKey = resolveBaseAPIKey(ks.Cfg)
	ks.LLMManager = buildProviderManager(ks.Cfg, ks.LuaVM, ks.BaseAPIKey, ks.Security)
	ks.LLMProvider = ks.LLMManager.Default()
	return nil
}

// buildProviderManager 按配置注册全部 LLM 源（原 cmd/homed 的整块平移）。
func buildProviderManager(cfg *types.Config, luaVM *luapkg.VM, baseAPIKey string,
	secRT *security.Runtime) *api.ProviderManager {
	providerMgr := api.NewProviderManager()
	// 先注入变量库，再注册 —— 顺序反了会静默漏装饰（见 Init 的注释）。
	if secRT != nil {
		providerMgr.SetSecretStore(secRT.Store())
	}
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
		// ★ 能力是**配置事实**，由装配层登记；不让内核靠 adapter 上的
		// 类型断言去猜（断言穿过 adapter + 安全装饰器，任一层漏实现方法
		// 就会静默变成"没有视觉"—— 这是反复出现的老 bug）。
		providerMgr.SetCapabilities(src.Name, api.ProviderCaps{
			Vision: src.Vision,
			Audio:  src.Audio,
		})
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
