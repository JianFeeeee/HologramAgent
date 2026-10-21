// 知识库目录改名迁移（幂等）。**独立模块**，见下。
//
// ## ★ 为什么迁移必须是独立模块而不是 store 里的一行
//
// 原 main 注释："迁移必须在 store 扫盘**之前**：改名后扫盘一次到位，
// 避免先以旧名建索引、再改名造成内存键与盘上目录短暂不一致"。
//
// 这是**真实的顺序约束**（不是副作用顺序，是"必须先改名再扫盘"）⇒
// 在依赖图上是知识库 store 的前置。声明成依赖 ⇒ 分层器保证它们**不同层**
// （同层就是并行了，那会破坏约束）。
//
// ★ 若把迁移并进 store 的 Init 里，这个约束就"藏在函数体内"了 ——
// 那正是"加模块必须调顺序"的病根形态。
package knowledge

import (
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/kernelboot"
	"github.com/JianFeeeee/HologramAgent/internal/kernelmod"
)

// MigrationModule 做知识库目录改名迁移。
type MigrationModule struct{}

func (MigrationModule) Name() string       { return "knowledge-migration" }
func (MigrationModule) Requires() []string { return []string{kernelmod.FacConfig} }
func (MigrationModule) Provides() []string { return []string{kernelmod.FacMigrated} }

func (MigrationModule) Init(ks *kernelboot.KernelState) error {
	root := filepath.Join(ks.Cfg.Daemon.DataDir, "knowledge")
	runKnowledgeMigration(root)
	return nil
}

func init() { kernelmod.Register(MigrationModule{}) }
