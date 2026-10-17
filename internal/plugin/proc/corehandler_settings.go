package proc

import (
	"encoding/json"
	"fmt"

	pubsdk "github.com/JianFeeeee/homeagentsdk/sdk"
)

// handleSettings 处理全部设置项读写（14 个 method 共用一条实现）。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleSettings(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- 设置（原 case 16/17/18/26~31/42~45/51）----
	case MethodSettingsGet, MethodSettingsSet, MethodSettingsRegisterDef,
		MethodSettingsSetSecret, MethodSettingsGetSecret,
		MethodSettingsGetCore, MethodSettingsSetCore, MethodSettingsListCore,
		MethodSettingsGetPlugin, MethodSettingsSetPlugin, MethodSettingsListPlugin,
		MethodSettingsList, MethodSettingsDefs, MethodSettingsDump,
		MethodSettingsPlugins, MethodSettingsDataDir:
		return h.settings(method, params)

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}

// handleLLM 处理 LLM 源查询与切换。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleLLM(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- LLM 源（原 case 19/20/37）----
	case MethodLLMListSources:
		llm := h.sdk.LLM()
		if llm == nil {
			return nil, errUnavailable("llm")
		}
		sources := llm.ListSources()
		if sources == nil {
			sources = []string{}
		}
		return map[string]interface{}{"sources": sources}, nil
	case MethodLLMSetSource:
		llm := h.sdk.LLM()
		if llm == nil {
			return nil, errUnavailable("llm")
		}
		var p struct {
			Name string `json:"name"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, llm.SetSource(p.Name)
	case MethodLLMCurrentSource:
		llm := h.sdk.LLM()
		if llm == nil {
			return nil, errUnavailable("llm")
		}
		return map[string]interface{}{"source": llm.CurrentSource()}, nil

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}

// handleSocial 处理社交图只读查询（人物/网络/特质/关系）。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleSocial(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- 社交图（只读，原 case 21/22/38/39/40）----
	case MethodSocialGetPerson, MethodSocialGetNetwork, MethodSocialGetTrait,
		MethodSocialGetRelation, MethodSocialListPersons:
		return h.social(method, params)

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}

// handleLifecycle 处理生命周期开关（当前仅自动重启）。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleLifecycle(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- 生命周期（原 case 8）----
	case MethodLifecycleAutoRestart:
		var p struct {
			Enabled bool `json:"enabled"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		h.sdk.SetAutoRestart(p.Enabled)
		return nil, nil

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}

// secretSettings 是 SettingsAPI 的**可选**加密能力。
//
// 为何用类型断言而不是把方法加进接口：proc 桥两侧（内核侧 settingsImpl、
// 模板侧 procSettings、以及各测试替身）实现面很宽，直接扩接口会让
// **所有实现者**同时编译失败 —— 包括已发布的 SDK 使用者。
// 用可选接口则：
//
//	· 支持者（内核 settingsImpl、新模板）→ 走加密路径
//	· 不支持者（旧测试替身）→ 返回明确错误，**不静默存明文**
type secretSettings interface {
	SetSecret(key string, value interface{}) error
	GetSecret(key string) (string, error)
}

// settingsSetSecret 强制加密写入。
//
// ★ 未实现该能力时**报错**而不是降级为 Set：插件以为存了密文，
// 实际磁盘上是明文 —— 这就是最危险的静默降级。
func settingsSetSecret(sett pubsdk.SettingsAPI, key string, value interface{}) error {
	ss, ok := sett.(secretSettings)
	if !ok {
		return fmt.Errorf("settings: 本内核不支持加密存储（settings.setSecret），" +
			"拒绝以明文写入敏感项；请升级内核")
	}
	return ss.SetSecret(key, value)
}

// settingsGetSecret 读取加密值。
func settingsGetSecret(sett pubsdk.SettingsAPI, key string) (string, error) {
	ss, ok := sett.(secretSettings)
	if !ok {
		return "", fmt.Errorf("settings: 本内核不支持加密存储（settings.getSecret）")
	}
	return ss.GetSecret(key)
}
