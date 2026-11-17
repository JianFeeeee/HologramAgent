// Package iobus 是 kernel-io 模块：IOManager + EventBus。
//
// ## 依赖：**真无依赖**
//
// `initKernelAPI()` 不收任何参数 ⇒ 它与 config 并列第 0 层。
// 这条"真无依赖"是有意的：让"最先初始化的 core 不依赖任何模块"
// 在代码里**字面成立**。
package iobus

import (
	"github.com/JianFeeeee/HologramAgent/internal/kernelapi"
	"log"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/kstate"
)

// Module 提供 IO 抽象层与事件总线。
type Module struct{}

func (Module) Name() string       { return "kernel-api" }
func (Module) Requires() []string { return nil }
func (Module) Provides() []string {
	// FacEventBusRaw 是本模块**创建**的裸总线（第⑥次裁定：它只给 security 取，
	// 由 security 劫持后把代理面注册到 FacEventBus）。
	return []string{kernelmod.FacKernelIO, kernelapi.FacEventBusRaw}
}

func (Module) Init(ks *kstate.State) error {
	iom := agentIO.NewIOManager()
	evBus := events.NewBus()
	log.Printf("[homed] kernel API ready: IOManager + EventBus")
	ks.Provide(kernelapi.FacIO, iom)
	// ★ 裸总线只挂在 **Raw** 名下（2026-11-17 第⑥次裁定）：
	//   `FacEventBus` 留给安全模块注册的**代理面**，消费方只能取到代理。
	//   本模块是**创建者**，装配侧的 security 是**劫持者**，两者是仅有的
	//   两个能碰到裸总线的地方。
	ks.Provide(kernelapi.FacEventBusRaw, evBus)
	return nil
}

func init() { kernelmod.Register(Module{}) }
