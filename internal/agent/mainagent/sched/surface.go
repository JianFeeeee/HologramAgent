package sched

// surface.go —— 调度器需要的两个**最小取面**（不含任何帧字段结构）。
//
// ★ 关键区别：这里不是"帧的公共定义"，而是"调度器对帧的最低要求"。
//   主 agent 的 TaskFrame 满足它；驻留子的帧也满足它自己那份 sched 的同名取面。

import agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"

// Suspended 是能被压进中断栈的**现场**（调度器只存不读）。
//
// ★ 任何值都能满足（空接口）—— 因为调度器确实对它零知识。
//
//	保留具名接口是为了让"调度器要的只是一个盒子"这件事在类型上写清楚。
type Suspended interface{}

// EventOf 从现场里取输入事件（只用于日志归因；取不到返回 nil）。
//
// 帧类型由调用方（本类型的帧推进）实现这个函数并传进来，调度器不 import
// 任何帧类型。
type EventOf func(site Suspended) *agentIO.InputEvent
