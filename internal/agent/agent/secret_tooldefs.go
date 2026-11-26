package agent

// secret_tooldefs.go —— `secret_*` 工具的**声明**（schema）。
//
// ★ 声明与执行体一起在基座：它们读的是内核全局变量库（`SecurityGate.Store()`），
//   任何 agent 都有，不是某个类型的私有能力。
//
// 为什么留在基座而不是内核执行设施：设施只负责**执行**（路由/授权/边界），
// 工具声明属于"谁拥有这个工具"——那是基座（它拥有 io/stageHost/安全闸这几组内陆设施）。

import (
	"sort"

	sdk "github.com/JianFeeeee/HologramAgent/internal/sdk"
)

// SecretToolDefs 返回变量库工具的声明（按需暴露：安全模块未启用时不暴露）。
//
// ★ 只在**真的可用**时暴露：未启用时把工具给模型，只会让它反复撞到
//
//	"安全模块未启用"（无谓往返）。
func SecretToolDefs() []interface{} {
	return secretToolDefs()
}

func secretToolDefs() []interface{} {
	mk := func(name, desc string, props map[string]interface{}, required ...string) map[string]interface{} {
		return map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        name,
				"description": desc,
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": props,
					"required":   required,
				},
			},
		}
	}
	out := []interface{}{
		mk("secret_submit",
			"【凭据登记】把一条凭据登记进内核变量库，返回别名 @{sec_xxx}。之后在工具参数里用 @{别名} 引用即可，"+
				"内核会在调用插件前换回真实值（插件无感知）。**登记后不要再写明文。**",
			map[string]interface{}{
				"value": map[string]interface{}{"type": "string", "description": "凭据明文（仅此一次传入，之后不可读回）"},
				"note":  map[string]interface{}{"type": "string", "description": "简短备注，便于日后分辨（如 'github token'）"},
			}, "value"),
		mk("secret_list",
			"【凭据清单】列出变量库里已有的别名与元信息（**不含明文**，明文永远不可读回）。",
			map[string]interface{}{}),
		mk("secret_forget",
			"【凭据删除】按别名删除一条变量。删除后引用它的工具参数会失效。",
			map[string]interface{}{
				"alias": map[string]interface{}{"type": "string", "description": "要删除的别名（如 sec_ab12cd）"},
			}, "alias"),
	}
	_ = sort.Strings
	return out
}

// sdkSecretToolNames 是这几个工具的名字（登记归属与判据用）。
func sdkSecretToolNames() []string {
	return []string{"secret_submit", "secret_list", "secret_forget"}
}

var _ = sdk.ToolDef{}
