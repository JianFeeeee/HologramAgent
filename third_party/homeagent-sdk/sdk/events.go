package sdk

// 受限事件总线（EventsAPI）。
//
// ## 为什么现在才敢暴露
//
// 事件总线是内核内部机制：它连接内核模块、状态面、WebUI 与插件。
// 早先不给插件的理由是**没有边界**——一给就是"任何插件能发任何事件、能订阅任何事件"，
// 等于把内核内部的同步面变成公共 API，且没有任何审查。
//
// 2026-10 之后内核侧有了 security 模块（凭据闸门 + 内容审查 + 审计），
// 于是可以只开**受限的一条缝**：
//
//	· 发布：只能用白名单事件类型，且只能在自己的命名空间下扩展
//	· 载荷：出向经 security 审查（明文凭据会被换成 @{别名}），并限制大小
//	· 订阅：只能订阅白名单里的只读事件
//
// ## 插件能做什么
//
//	Publish("skill_detected", {...})           // 交接类事件（兼容层 → skillmgr）
//	Publish("plugin.<自己名字>.<任意>", {...})  // 自己的命名空间，供插件间协作
//	Subscribe("tools_ready", handler)          // 等内核就绪再做事的入口
//
// 越界会**报错**而不是静默丢弃：静默丢弃会让插件以为发成功了。

// EventsAPI 是插件可见的事件总线。
//
// 它**不是** *events.Bus 的投影：这是一个有边界、有审查的接口。
type EventsAPI interface {
	// Publish 发布一个事件。
	//
	// eventType 必须在白名单内（见 AllowedPublish），或形如
	// "plugin.<本插件名>.<后缀>"。载荷大小与键数有上限。
	Publish(eventType string, payload map[string]interface{}) error

	// Subscribe 订阅一个白名单内的事件（只读面）。
	//
	// 返回的 cancel 在插件 Stop 时调用可解除订阅；宿主卸载插件时也会自动解除。
	// handler 的 panic 会被总线隔离，不影响其他订阅者。
	Subscribe(eventType string, handler func(payload map[string]interface{})) (cancel func(), err error)

	// AllowedPublish 返回当前可发布的事件类型（白名单 + 自己的命名空间前缀）。
	AllowedPublish() []string

	// AllowedSubscribe 返回当前可订阅的事件类型。
	AllowedSubscribe() []string
}

// 常用事件名（**只是名字，不是授权表**）。
//
// 授权表由内核 security 模块持有（`security.EventPolicy`）并可随配置调整；
// SDK 侧只声明"这些名字存在"，避免插件作者拼错字符串。
//
// 是否允许你用某个名字，以运行时 `Events().Publish/Subscribe` 的返回为准：
// 越界会**报错**，不会静默丢弃。
const (
	// EventNameSkillDetected：兼容层把发现的纯 SKILL 条目交给 skillmgr 接管。
	// （职责链交接，不是执行指令）
	EventNameSkillDetected = "skill_detected"
	// EventNameToolsReady：全部插件加载完、工具注册齐（只读订阅）。
	EventNameToolsReady = "tools_ready"
	// EventNameSecurityReady：安全闸门装配完成（只读订阅）。
	EventNameSecurityReady = "security_ready"
	// EventNameDeviceReady：工具/设备数据面就绪（只读订阅）。
	EventNameDeviceReady = "device_ready"
)
