// 知识库 store 模块。
package knowledge

import (
	"log"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
)

// StoreModule 提供知识库。
type StoreModule struct{}

func (StoreModule) Name() string { return "knowledge-store" }
func (StoreModule) Requires() []string {
	// ★ FacMigrated 是**刻意的顺序依赖**：迁移必须早于扫盘（见 migration.go）。
	return []string{kernelmod.FacConfig, kernelmod.FacMigrated}
}
func (StoreModule) Provides() []string { return []string{kernelmod.FacKnowledge} }

func (StoreModule) Init(ks *kernelboot.KernelState) error {
	ks.Knowledge = knowledge.NewStore(filepath.Join(ks.Cfg.Daemon.DataDir, "knowledge"))
	if err := ks.Knowledge.Start(); err != nil {
		log.Printf("[homed] warning: knowledge store: %v", err)
	} else {
		log.Printf("[homed] knowledge store active with %d items", len(ks.Knowledge.List()))
	}
	return nil
}

func init() { kernelmod.Register(StoreModule{}) }
