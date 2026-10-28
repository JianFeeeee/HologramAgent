package update

// 更新进度/结果的事件发布。
//
// ## 为什么走 EventBus 而不是"日志 + WebUI 轮询"
//
// 升级是**长耗时且会让进程消失**的动作。轮询在重启瞬间必然断线，
// 而断线那一刻用户最需要知道"升级到哪一步/失败了/已回滚"。
// 事件总线是内核既有的发布订阅机制（同步投递、handler panic 隔离），
// 用它不需要新造一条通知通道。
//
// ## 为什么必须"先发事件再动作"
//
// 升级尾部几步（replace → restart）会让进程消失。顺序一反，
// 最后几步的事件永远发不出去。所以每个 publish* 都紧贴在其描述的
// 动作**之前**调用。

import (
	"fmt"

	"github.com/JianFeeeee/HologramAgent/internal/events"
)

// publishProgress 发布一个升级步骤（kernel 或 plugin）。
//
// scope: "kernel" | "plugin"; name: 插件名（kernel 时为空）
// step:  verify / backup / replace / restart / verify_version / rollback / done / failed
func publishProgress(bus *events.Bus, scope, name, step, detail string) {
	if bus == nil {
		return
	}
	bus.Publish(&events.Event{
		Type:   events.EventUpdateProgress,
		Source: "update",
		Payload: map[string]interface{}{
			"scope":  scope,
			"name":   name,
			"step":   step,
			"detail": detail,
		},
	})
}

// publishResult 发布升级最终结果（成功或失败，含回滚轨迹）。
//
// ★ error 字段**总是**出现在 payload 里（成功时为空串）。
//
// 此前失败路径把 errMsg 收进一个 `if errMsg != "" { _ = errMsg }` 分支
// 直接丢掉了 —— 于是订阅方只能看到 ok=false，**看不到失败原因**，
// 而"为什么失败"正是用户唯一想知道的。字段固定存在（即使为空）还让
// 订阅方不必做"字段有没有"的兼容分支。
//
// ★ 顺序约定：调用方必须在动作**之前**发事件（见文件头注释）。
// 成功路径也一样：result 发出后调用方可能立即重启，进程随即消失。
func publishResult(bus *events.Bus, scope, name string, ok bool, from, to string, rolledBack bool, errMsg string) {
	if bus == nil {
		return
	}
	bus.Publish(&events.Event{
		Type:   events.EventUpdateResult,
		Source: "update",
		Payload: map[string]interface{}{
			"scope":       scope,
			"name":        name,
			"ok":          ok,
			"from":        from,
			"to":          to,
			"rolled_back": rolledBack,
			"error":       errMsg,
		},
	})
}

// publishCheck 发布一次更新检查结果（只读，不改动任何东西）。
func publishCheck(bus *events.Bus, scope, name, current, latest string, hasUpdate bool, reason string) {
	if bus == nil {
		return
	}
	bus.Publish(&events.Event{
		Type:   events.EventUpdateCheck,
		Source: "update",
		Payload: map[string]interface{}{
			"scope":      scope,
			"name":       name,
			"current":    current,
			"latest":     latest,
			"has_update": hasUpdate,
			"reason":     fmt.Sprintf("%v", reason),
		},
	})
}
