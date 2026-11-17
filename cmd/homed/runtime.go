package main

// runtime.go —— **运行期起点**（装配完成后才执行）。
//
// ## 为什么单独一个文件（用户裁定 2026-11-17）
//
// > 「装配是装配，事件循环是事件循环，本来就是不相干的两件事」
//
// 在此之前，运行期的起点散落在**装配点**里：`agent.Start()`、supervisor 心跳、
// 日志管理器的 goroutine、定时任务、事件投递的订阅者启动……它们和 `Init`
// 混在同一个函数里，于是"跑起来"与"装起来"不可分辨。
//
// 现在：装配（`RunKernelModules` + 各模块 `Init`）在一个阶段里**结束**，
// 之后由本文件的 `startRuntime` 统一把已装好的东西推动起来。
// 装配那一侧**一行都不改**（分层、Provides/Requires、Init 只 Provide 照旧）。

import (
	"context"
	"log"
	"time"

	agentCore "github.com/JianFeeeee/HologramAgent/internal/agent/agent"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelloop"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// startRuntime 装配完成之后启动运行期事件循环。
//
// 它做四件事，且**只做这四件**（全部是"推动已装好的东西"，不是"装配"）：
//
//	① 挂投递驱动者：此后事件总线的投递由本循环推动，而不是发布者栈上同步跑完
//	② 启动循环 worker（投递 + 定时任务已注册的那些）
//	③ 心跳登记（内核还在转的证明，供 guard 判定）
//	④ 主 agent 启动（它的 agentloop 从这里开始转）
//
// 返回 stop 函数（与既有 cleanup 同序：先停 agent，再停循环）。
func startRuntime(ks *kstate.State, logDir string, ctx context.Context,
	heartbeatPath string, agentStarter func(), periodic []kernelloop.PeriodicTask) (func(), error) {

	// periodic 由调用方从**类型模块的声明面**转换而来（agent.PeriodicTask →
	// kernelloop.PeriodicTask）。转换留在装配点，内核循环不认识任何 agent 类型。

	loop := kernelloop.New(0) // 0 = 默认有界队列（背压而不丢事件）

	// ① 挂投递驱动者：经 security 提供的**注入点**（不是裸总线）。
	sink, _ := ks.Require(kernelapi.FacEventDispatch)
	dispatchSink, _ := sink.(kernelapi.EventDispatchSink)
	if err := loop.InstallDispatcher(dispatchSink); err != nil {
		// 装配缺陷：security 负责提供注入点，缺了就是顺序或声明错了。
		return nil, err
	}

	// ③ 心跳（内核存活证明）。
	if heartbeatPath != "" {
		loop.StartHeartbeat(kernelloop.Heartbeat{Path: heartbeatPath, Interval: 5 * time.Second})
	}

	// ② 各类型注册的定时任务（装配期已登记，这里统一刻度）。
	for _, t := range periodic {
		loop.RegisterPeriodic(t)
	}

	loop.Start()

	// ④ 主 agent 的 agentloop 从这一刻开始转（它与本循环天然并行）。
	if agentStarter != nil {
		agentStarter()
	}

	log.Printf("[homed] runtime loop started (event dispatch + %d periodic task(s))",
		len(loop.PeriodicTasks()))

	return func() {
		loop.Stop()
	}, nil
}

// periodicOf 把 agent 声明的定时任务转成内核循环的刻度项。
//
// ★ 转换点在**装配侧**（本文件）而不是内核循环里：循环不认识任何 agent 类型
// （否则"内核认识模块"就重新成立了）。agent 侧只负责声明"我要什么任务"。
func periodicOf(a *agentCore.Agent) []kernelloop.PeriodicTask {
	if a == nil {
		return nil
	}
	src := a.PeriodicForLoop()
	out := make([]kernelloop.PeriodicTask, 0, len(src))
	for _, t := range src {
		run := t.Run
		if run == nil {
			continue
		}
		out = append(out, kernelloop.PeriodicTask{
			Owner:    string(a.ID()),
			Name:     t.Name,
			Interval: t.Interval,
			Run:      func() { run(a) },
		})
	}
	return out
}
