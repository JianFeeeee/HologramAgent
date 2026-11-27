// Package tool 是 tool-reg 模块：**插件工具登记簿**。
//
// ## 为什么它自成模块
//
// 用户裁定 2026-11-25：
//
//	「tool 应该是一个独立的模块，与 stagehost 在内核通过 kstate 协作」
//	「stagehost 是工具啊？你好好看看 stagehost 到底是什么」
//
// 阶段管道和工具登记此前是**同一个对象**（`kit.StageHostType`），
// 它们唯一的共同点是"插件注册进来的"。后果：
//
//	· "工具表由谁产"讲不清 —— 想查工具来源要先知道它还兼职阶段
//	· 一次误改（渲染面重复拼 `ToolsFor`）之后，责任归属也讲不清
//	· 插件卸载要同时清两样东西，容易漏（本仓实测过漏 stage handler）
//
// 现在：
//
//	tool-reg   名字 + schema + handler（本模块，FacToolReg）
//	stage-host 阶段管道（FacStageHost）
//	tool-exec  按名字取 handler 执行（FacToolExec）
//
// 三者在内核经 `kstate` 协作，谁也不持有谁。
package tool

import (
	"github.com/JianFeeeee/HologramAgent/internal/agent/kit"
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// Module 提供插件工具登记簿。
type Module struct{}

func (Module) Name() string { return "tool-reg" }

// Requires 无（登记簿本身不依赖别的模块：它是空的容器，谁来登记都行）。
func (Module) Requires() []string { return nil }

func (Module) Provides() []string { return []string{kernelapi.FacToolReg} }

func (Module) Init(ks *kstate.State) error {
	ks.Provide(kernelapi.FacToolReg, kit.NewToolRegistry())
	return nil
}

func init() { kernelmod.Register(Module{}) }
