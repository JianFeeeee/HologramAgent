package proc

import (
	"encoding/json"
	"fmt"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

// handleRegister 处理注册面：工具 / stage / 输出通道 / 插件 API / 入站通道。
//
// 本函数体是 corehandler.go 里 Handle 那一个大 switch 的**整块平移**：
// case 标签与 case 体逐字保留，只换了宿主函数（§3.2 的平移原则）。
func (h *coreHandler) handleRegister(method string, params json.RawMessage) (interface{}, error) {
	switch method {
	// ---- 注册面（原 case 1/2/3/4/46）----
	case MethodToolRegister:
		return h.toolRegister(params)
	case MethodStageRegister:
		return h.stageRegister(params)
	case MethodOutputRegister:
		return h.outputRegister(params)
	case MethodAPIRegister:
		var p struct {
			Name string `json:"name"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, h.sdk.RegisterPluginAPI(p.Name)
	case MethodInputRegister:
		var p struct {
			Name       string            `json:"name"`
			Def        pubsdk.ChannelDef `json:"def"`
			HasCleaner bool              `json:"has_cleaner"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if p.Name == "" {
			return nil, fmt.Errorf("input.register: 缺少 name")
		}
		cleaner, err := h.cleanerProxy(CleanerScopeInput, p.Name, p.HasCleaner)
		if err != nil {
			return nil, fmt.Errorf("input.register: %w", err)
		}
		if err := validateContextPolicy("input.register", p.Def.ContextPolicy); err != nil {
			return nil, err
		}
		if err := validateRecallPolicy("input.register", p.Def.RecallPolicy); err != nil {
			return nil, err
		}
		// 整体传 p.Def（只是把函数型的 Cleaner 换成代理），不要手写字段白名单：
		// 白名单会让新增字段静默丢失。
		def := p.Def
		def.Cleaner = cleaner
		return nil, h.sdk.RegisterInputChannel(p.Name, def)

	}

	// 组内不应到达：Handle 的分派表与本函数的 case 标签同源，
	// 出现即说明两处不同步。
	return nil, fmt.Errorf("未知 method: %s", method)
}
