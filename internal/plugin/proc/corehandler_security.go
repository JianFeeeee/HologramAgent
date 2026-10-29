package proc

// 安全策略**声明面**的宿主侧实现（插件子进程 → 内核）。
//
// ## 为什么在 proc 包而不是让插件直连 security
//
// 插件是**子进程**，它唯一的通道就是本桥（stdio JSON-RPC + 共享内存）。
// 它拿不到 security.Runtime，也不该拿到 —— 那会让插件能直接改策略表。
//
// 而判定点必须在这里还有第二个原因：**Owner 要由内核填**。
// 插件自报的 owner 可以伪造（任何插件都能声明 `qq.owner` 并给
// AllowTools=["*"]，等于把 QQ 的 owner 档位改成全放行），
// 所以本包用 `h.sdk.PluginName()`（内核侧真实标识）作为 owner，
// 而不是采信参数里的值。
//
// ## 与 internal/sdk/security_impl.go 的关系
//
// 那里的 `securityImpl` 才是真正的实现（含 owner 校验与 Runtime 转发）。
// 本文件只做 JSON 解码 → 调它 → 把错误带回去，保持与其他
// corehandler_* 分部函数同构。

import (
	"encoding/json"
	"fmt"

	pubsdk "github.com/JianFeeeee/hologramagentsdk/sdk"
)

func (h *coreHandler) handleSecurity(method string, params json.RawMessage) (interface{}, error) {
	api := h.sdk.Security()
	if api == nil {
		// 未装配（独立工具/特殊宿主）时明确报错，而不是静默成功：
		// 静默成功会让插件以为档位已登记，直到注入时才暴露。
		return nil, fmt.Errorf("security: the declaration API is not wired in this host")
	}

	switch method {
	case MethodSecurityDeclareProfile:
		var p struct {
			Profile pubsdk.SecurityProfile `json:"profile"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := api.DeclareProfile(p.Profile); err != nil {
			return nil, err
		}
		return map[string]interface{}{"ok": true, "name": p.Profile.Name}, nil

	case MethodSecurityDeclareProfiles:
		var p struct {
			Profiles []pubsdk.SecurityProfile `json:"profiles"`
		}
		if err := unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := api.DeclareProfiles(p.Profiles); err != nil {
			return nil, err
		}
		names := make([]string, len(p.Profiles))
		for i, pr := range p.Profiles {
			names[i] = pr.Name
		}
		return map[string]interface{}{"ok": true, "names": names}, nil
	}

	// 组内不应到达：Handle 的分派表与 case 标签同源。
	return nil, fmt.Errorf("未知 method: %s", method)
}
