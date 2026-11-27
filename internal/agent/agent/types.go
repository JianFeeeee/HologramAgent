package agent

// types.go —— 类型名常量（表本体在 `internal/agent/registry`）。
//
// ## 历史（2026-11-20 收敛）
//
// 本文件曾有第二张类型表：`typeRegistry map[string]Factory` +
// `RegisterType` / `Instantiate` / `Types` + 两个 any 适配器。
//
// 它违反 docs/zh/target-architecture.md §7.1：
//
// > 「`internal/agent/registry` 解决的是「**基座不预设清单**」」
// > 「一张表管五件事」
//
// 唯一那张表是 `registry.Descriptor`（`Actions` / `Reports` / `Sections` /
// `Timers` / `Construct` / `Initialize` / `Admin` / `Help`）。
// 主 agent 也是表里的一项，与 resident / rt / worker / 插件类型同构。
//
// **造实例**的唯一入口是 `agent.NewInstance(InstanceSpec)`：内核填好
// `AgentConfig`（配置透传），类型的 `Install` 挂自己的回调。

// TypeNameMain 是**主 agent** 的类型名（内核按配置选它拉起）。
const TypeNameMain = "main"
