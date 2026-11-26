package agent

// iotooldefs.go —— 通道类工具（output_list_channels / input_channels）的**历史注记**。
//
// ## 声明现在在注册表里（2026-11-19 收敛）
//
// 原先这两个工具的 schema 由本文件渲染（`OutputToolDefs()`），
// 而执行体在 `output.go` / `inputch.go` —— 声明与执行体分居两处，
// 且**没有登记点**，于是想"剥掉主动输出工具"的类型只能靠名字前缀猜。
//
// 现在：
//
//	声明 → `registry.Tool`（`DeclaredOnly` + `OutputGate`），登记点在
//	       主 agent 的类型装配（`mainagent/tools_register.go`）
//	执行 → 基座设施（`output.go` / `inputch.go`）
//	判定 → `IsOutputGateTool(name)`（读标记，不读前缀）
//
// `output_send__<通道>` 是**按通道动态生成**的（通道由插件登记，没有静态名字），
// 它仍由主 agent 渲染时生成 —— 那是唯一必须按前缀判的形态，且只在
// `registrytooldefs.go` 一处。
