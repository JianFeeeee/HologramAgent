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
		},
	})
	if errMsg != "" {
		// 错误单独放，避免成功路径也带一个空 error 字段
		_ = errMsg
	}
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
