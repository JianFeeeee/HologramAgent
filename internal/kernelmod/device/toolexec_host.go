package device

// toolexec_host.go —— 执行链需要的**能力面**（全部由内核设施提供）。
//
// # 为什么不再是「宿主」
//
// 改造前这里是 `hostFace`：15 个方法、由调用方以 `any` 传进来，实际装的是
// `*Agent`。那是**反向依赖**——内核执行链向基座索取"我自己的安全闸 / 我的
// 工具表 / 我的 io"。后果：
//
//	· device 必须依赖 agent（倒挂），而 Facilities 里 FacDevice 零消费方
//	· 每个类型都要记得传 Host（mainagent 还专门有个转发层补它）
//	· `any` + 运行期断言，类型安全丢失
//
// 现在按**能力**拆成四组窄接口，由装配期注入。执行时只多一个「我是谁」。
//
// # 只保留三项 agent 状态
//
// 身份（agentID / isChild）、取消登记、工具阶段需要填的身份字段 ——
// 这三样是**实例自己的运行态**，不属任何设施。

import (
	"context"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
)

// IOAccess 是设备与通道（device 自己的资源池）。
type IOAccess interface {
	// IO 返回内核唯一的 IOManager（设备表 + 通道登记表）。
	IO() *agentIO.IOManager
}

// SecurityAccess 是安全闸门（工具授权 / 路径 / 网络 / 产出物 / 内容审查）。
type SecurityAccess interface {
	// SecurityGate 返回闸门（未启用时是 Disabled 实现，不是 nil）。
	SecurityGate() kernelapi.SecurityGate
}

// ToolAccess 是插件工具表（含声明里的 ParallelSafe / Serial）。
type ToolAccess interface {
	ToolRegistry() *kit.ToolRegistry
}

// CancelRegistry 是按实例的工具取消登记（运行态，非设施）。
type CancelRegistry interface {
	Register(agentID string, cancel context.CancelFunc) uint64
	Unregister(agentID string, id uint64)
	// CancelAll 取消某实例的全部在跑工具（停机时）。
	CancelAll(agentID string)
}
