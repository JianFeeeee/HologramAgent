// Package knowledgereg 是**知识库插件的注册点**（装配侧适配）。
//
// ## 为什么单独一个包
//
// `internal/plugins/knowledge` 不能 import `internal/sdk`（`sdk.KnowledgeAPI`
// 指向它的 `Store` ⇒ 会成环），也不能 import `internal/plugin`（那会反过来
// 引到 sdk）。所以"把插件注册进工厂表"这件事必须住在**只依赖两者**的装配侧。
//
// 这就是"装配是装配"的落点：插件自己实现能力，装配侧负责把它接上宿主。
package knowledgereg

import (
	"github.com/JianFeeeee/HologramAgent/internal/plugin"
	knowledge "github.com/JianFeeeee/HologramAgent/internal/plugins/knowledge"
	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

func init() {
	plugin.RegisterPluginMeta(knowledge.PluginName, "知识库", "Knowledge Base")
	plugin.RegisterFactory(knowledge.PluginName, func(name string, config map[string]interface{}) (sdk.Plugin, error) {
		return &platformPlugin{p: knowledge.New(name)}, nil
	})
}

// platformPlugin 把知识库插件适配成 `sdk.Plugin`（Start/Stop 的宿主面）。
type platformPlugin struct {
	p    *knowledge.Plugin
	sdk  *sdk.PluginSDK
	name string
}

func (pp *platformPlugin) Name() string { return pp.p.Name() }

func (pp *platformPlugin) Start(s *sdk.PluginSDK) error {
	pp.sdk = s
	if err := pp.p.Start(hostAdapter{sdk: s}); err != nil {
		return err
	}
	// ★ 发布能力：知识库实现能力，内核不持有它的句柄。
	//
	// 别的插件（cli / webui / healthcheck / lua / proc 桥）仍要在进程内用它，
	// 所以需要一处"能力目录"。发布点在这里（装配侧），因为只有这里同时
	// 认识 SDK 与插件实现 —— 插件自己 publish 就得 import sdk（成环）。
	sdk.SetKnowledgeProvider(sdk.NewKnowledge(pp.p.Store()))
	return nil
}

func (pp *platformPlugin) Stop() error {
	sdk.ClearKnowledgeProvider()
	return pp.p.Stop()
}

// hostAdapter 把 `*sdk.PluginSDK` 适配成插件的最小宿主面。
type hostAdapter struct{ sdk *sdk.PluginSDK }

func (h hostAdapter) RegisterTool(def knowledge.ToolDef, handler knowledge.ToolHandler) error {
	return h.sdk.RegisterTool(def.Name, sdk.ToolDef{
		Name:         def.Name,
		Description:  def.Description,
		Parameters:   def.Parameters,
		NoMemory:     def.NoMemory,
		ParallelSafe: def.ParallelSafe,
	}, func(args map[string]interface{}) (interface{}, error) {
		return handler(args)
	})
}

func (h hostAdapter) Settings() knowledge.Settings { return settingsAdapter{st: h.sdk.Settings()} }

func (h hostAdapter) Status() knowledge.Status { return statusAdapter{st: h.sdk.Status()} }

func (h hostAdapter) Media() knowledge.MediaSource {
	mem := h.sdk.Memory()
	if mem == nil {
		return nil
	}
	return mem
}

func (h hostAdapter) Multimodal() knowledge.VectorSpace {
	mm := h.sdk.Multimodal()
	if mm == nil {
		return nil
	}
	return mm
}

// settingsAdapter 把 sdk.SettingsAPI 适配成插件的最小配置面。
type settingsAdapter struct{ st sdk.SettingsAPI }

func (a settingsAdapter) Get(key string) (interface{}, error) {
	if a.st == nil {
		return nil, nil
	}
	return a.st.Get(key)
}

func (a settingsAdapter) DataDir() string {
	if a.st == nil {
		return ""
	}
	return a.st.DataDir()
}

func (a settingsAdapter) RegisterDef(def knowledge.ConfigDef) {
	if a.st == nil {
		return
	}
	a.st.RegisterDef(sdk.ConfigDef{
		Key:         def.Key,
		Default:     def.Default,
		Type:        def.Type,
		DisplayName: def.DisplayName,
		Category:    def.Category,
		Description: def.Description,
		Secret:      def.Secret,
	})
}

// statusAdapter 把 sdk.StatusAPI 适配成插件的段注册面。
type statusAdapter struct{ st sdk.StatusAPI }

func (a statusAdapter) RegisterSection(key string, provider func() any) {
	if a.st == nil {
		return
	}
	a.st.RegisterSection(key, provider)
}

// 编译期确认：适配器满足插件的最小面。
var _ knowledge.Host = hostAdapter{}
