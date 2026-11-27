package agent

// protocolsurface.go —— 父子交互的**唯一协议面**（基座 ↔ 类型的公共入口）。
//
// ## 两条协议，没有第三条（用户裁定 2026-11-18 / 2026-11-19）
//
//	父 → 子   admin_child(type, action, meta, arg)   ← 本文件的 AdminChild
//	子 → 父   CallParent(level, kind, text, meta)
//
// ★ 类型（**含主 agent**）不得直接看 `children` 表、不得调
//   `InstanceManager.DestroyChild`、不得调某个类型的方法 —— 那些都是绕过协议。
//   凡是要动子，构造一个 admin_child 请求走 `registry.Dispatch`。
//
// ★ `Channels()` / `ListChildren` 是**只读简报**（`registry.ChildBrief`）：
//   看得到 id/type/state/inputchs/extra，看不到任何类型的内部字段。

import (
	"fmt"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/registry"
)

// AdminChild 执行一次 admin_child 请求（与模型发起的是同一条路）。
func (a *Agent) AdminChild(typeName, action string, arg map[string]interface{}) (string, error) {
	im := a.EnsureInstanceManager()
	if im == nil {
		return "", fmt.Errorf("实例管理器未就绪（内核装配错误）")
	}
	return registry.Dispatch(im, typeName, action, arg)
}

// ListChildren 返回存活实例的**只读简报**（按 type 过滤，空 = 全部）。
func (a *Agent) ListChildren(typeName string) []registry.ChildBrief {
	im := a.EnsureInstanceManager()
	if im == nil {
		return nil
	}
	return im.ListChildren(typeName)
}

// RouteInputTo 把一条输入投给另一个 agent（走内核路由，不碰对方内部）。
//
// ★ 复用内核的 `RouteInput(owner, evt)`：目标 agent 自己注册的投递口接住它。
//
//	事件本体原样传递 —— 带 ResponseCh 的同步调用方靠它拿回执。
func (a *Agent) RouteInputTo(agentID string, evt *agentIO.InputEvent) bool {
	if a == nil || agentID == "" || evt == nil {
		return false
	}
	return delivered(agentID, evt)
}
