// Package docstore 是 doc-store 模块：文档记忆（文本块 + 向量）。
//
// 依赖 config：`initDocStore(cfg)` 读 cfg.Daemon.DataDir。
package docstore

import (
	"log"
	"path/filepath"

	"github.com/JianFeeeee/HomeAgent/internal/kernelboot"
	"github.com/JianFeeeee/HomeAgent/internal/kernelmod"
	"github.com/JianFeeeee/HomeAgent/internal/memory"
	"github.com/JianFeeeee/HomeAgent/internal/memory/document"
)

// Module 提供文档记忆。
type Module struct{}

func (Module) Name() string       { return "doc-store" }
func (Module) Requires() []string { return []string{kernelmod.FacConfig} }
func (Module) Provides() []string { return []string{kernelmod.FacDocStore} }

func (Module) Init(ks *kernelboot.KernelState) error {
	docStore := document.NewStore(
		filepath.Join(ks.Cfg.Daemon.DataDir, "memory", "documents"), memory.TokenizeWords)
	if err := docStore.Start(); err != nil {
		log.Printf("[homed] warning: document store: %v", err)
	}
	// 关停时落盘：文档记忆的内存态变更（迁移结果、访问计数等）只在 flush 里写盘，
	// 而 flush 的唯一入口是 Stop() —— 此前全仓无人调用它，
	// 于是迁移结果永不落盘、每次启动白算一遍。
	ks.DocStore = docStore
	ks.AddCloser(docStore.Stop)
	return nil
}

func init() { kernelmod.Register(Module{}) }
