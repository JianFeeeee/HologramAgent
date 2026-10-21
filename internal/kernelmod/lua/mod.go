// Package lua 是 lua 模块：Lua VM（LLM 协议适配）。
//
// 依赖 config：`initLuaVM(cfg)` 读 cfg.Daemon.DataDir（adapter 目录）。
package lua

import (
	"log"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	luapkg "github.com/JianFeeeee/HologramAgent/internal/lua"
)

// Module 提供 Lua 协议适配 VM。
type Module struct{}

func (Module) Name() string       { return "lua" }
func (Module) Requires() []string { return []string{kernelmod.FacConfig} }
func (Module) Provides() []string { return []string{kernelmod.FacLua} }

func (Module) Init(ks *kernelboot.KernelState) error {
	luaVM := luapkg.NewVM(filepath.Join(ks.Cfg.Daemon.DataDir, "adapters"))
	if err := luaVM.Start(); err != nil {
		log.Printf("[homed] warning: lua vm init failed: %v", err)
		ks.LuaVM = luaVM
		return nil // 与原语义一致：起不来只告警，仍交回实例
	}
	ks.LuaVM = luaVM
	ks.AddCloser(luaVM.Stop)
	return nil
}

func init() { kernelmod.Register(Module{}) }
