// Package kernelapi 是 kernel-api 模块：IOManager + EventBus。
//
// ## 依赖：**真无依赖**
//
// `initKernelAPI()` 不收任何参数 ⇒ 它与 config 并列第 0 层。
// 这条"真无依赖"是有意的：让"最先初始化的 core 不依赖任何模块"
// 在代码里**字面成立**。
package kernelapi

import (
	"log"

	agentIO "github.com/JianFeeeee/HologramAgent/internal/agent/io"
	"github.com/JianFeeeee/HologramAgent/internal/events"
	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
)

// Module 提供 IO 抽象层与事件总线。
type Module struct{}

func (Module) Name() string       { return "kernel-api" }
func (Module) Requires() []string { return nil }
func (Module) Provides() []string { return []string{kernelmod.FacKernelIO} }

func (Module) Init(ks *kernelboot.KernelState) error {
	iom := agentIO.NewIOManager()
	evBus := events.NewBus()
	log.Printf("[homed] kernel API ready: IOManager + EventBus")
	ks.IO = iom
	ks.EventBus = evBus
	return nil
}

func init() { kernelmod.Register(Module{}) }
