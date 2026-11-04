package child

// host.go 定义 ChildKind 实现所需的**最小宿主能力**。
//
// # 为什么需要它
//
// 类型实现（rt / resident）必须能用载体的能力（system prompt、窗口大小、
// 输出设备、工具执行）。但如果它们直接调用 `*core.Agent`，就得反向 import core ⇒ 成环。
//
// 所以依赖方向固定为 **core → child**：core 实现 Host 接口，child 只认识接口。
//
// # 纪律：这个接口只增"骨架/类型共用的能力"
//
// 不要因为"某个类型顺手想用一下就加上"。每加一个方法，所有 Host 实现都要跟。
// 类型专属能力应当留在该类型的实现里（用类型断言或构造期注入闭包）。
// 判据 TestHostInterfaceIsMinimal 钉住方法集大小。

import (
	agentAPI "github.com/JianFeeeee/HologramAgent/internal/agent/api"
	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
)

// Host 是子 agent 可用的载体能力。
//
// core.Agent 是唯一的生产实现（见 core/child_host.go）。
type Host interface {
	// SystemPrompt 返回该载体的系统提示词（上下文装配用）。
	SystemPrompt() string

	// ContextWindowTokens 返回上下文窗口（token）。窗口决定裁剪水位。
	ContextWindowTokens() int

	// EmitText 把一段增量文本送到该子的输出通道。
	//
	// ★ 语义要求（用户裁定：rt 输出 = 标准异步输出路径）：
	// 实现必须**异步**——插件侧延迟不得节流调用方的生成循环。
	EmitText(c *Child, text string) error

	// RunToolBatch 执行一批工具，返回与入参**同序**的结果。
	//
	// ★ 实现必须让整批经过 before_toolcall / after_toolcall 插件钩子。
	// 历史上 rt 直调 executeToolCallOutcome，绕过了两个钩子 ⇒
	// 插件对 rt 的工具调用完全不可见（不能审计/拒绝/改参/脱敏）。
	RunToolBatch(c *Child, calls []agentAPI.ToolCall) []ToolResult

	// Logf 记一行日志（带该子的 id 前缀由实现负责）。
	Logf(format string, args ...interface{})
}

// Deliverer 由需要自定义输入投递的类型实现（骨架不需要，故不在 Host 上）。
//
// 单槽型（rt）用它在往槽里写值后取消当前轮（契约 1：新输入打断旧轮）。
type Deliverer interface {
	// AfterPush 在输入已写入槽之后调用。seq 是本次写入的序号。
	//
	// ★ 为什么拆成两步（先写槽、再取消）：只写槽不取消是错的 ——
	// 正在跑的轮次 block 在流里，不会回到循环顶部取新值，
	// 表现就是"等它说完"，正是契约 1 禁止的。
	AfterPush(c *Child, seq uint64)
}

// DeviceResolver 由能提供输出设备的 Host 实现（可选）。
//
// 单独成接口而不放进 Host：只有需要输出通道的类型才用它，
// 放进 Host 会让不需要的类型也被迫实现。
type DeviceResolver interface {
	// OutputDevice 按通道名解析设备。
	//
	// ★ 必须**每次重新解析**而不是缓存指针：插件崩溃时会被
	// UnregisterDevice，恢复后重新注册。缓存指针会一直指向死设备。
	OutputDevice(channel string) agentIO.Device
}
