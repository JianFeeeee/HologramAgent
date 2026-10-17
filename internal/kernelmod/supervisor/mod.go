// Package supervisor 是 supervisor 模块：代理生命周期守护。
//
// 依赖 config：`initSupervisor(cfg)` 收 cfg。
//
// ★ 全仓唯一有 `log.Fatalf` 的 init*（其余都是记日志不中断）⇒ 它**必须成功**。
package supervisor

import (
	"log"

	"github.com/JianFeeeee/HomeAgent/internal/kernelboot"
	"github.com/JianFeeeee/HomeAgent/internal/kernelmod"
	"github.com/JianFeeeee/HomeAgent/internal/supervisor"
)

// Module 提供代理生命周期守护。
type Module struct{}

func (Module) Name() string       { return "supervisor" }
func (Module) Requires() []string { return []string{kernelmod.FacConfig} }
func (Module) Provides() []string { return []string{kernelmod.FacSupervisor} }

func (Module) Init(ks *kernelboot.KernelState) error {
	sup := supervisor.New(ks.Cfg)
	if err := sup.Start(); err != nil {
		log.Fatalf("start supervisor: %v", err)
	}
	ks.Supervisor = sup
	return nil
}

func init() { kernelmod.Register(Module{}) }
