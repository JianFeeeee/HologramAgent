package knowledgeplugin

// migrate.go —— 知识库目录改名迁移（原 kernelmod/knowledge 的 migration 模块）。
//
// ## 为什么它现在就在 Start 里，而不是内核依赖图上的独立模块
//
// 原实现把它拆成独立内核模块，理由是"迁移必须先于扫盘"：
//
//	改名后扫盘一次到位，避免先以旧名建索引、再改名造成内存键与盘上目录短暂不一致。
//
// 那个约束是真的，但**内核依赖图**不是它的正确落点 —— 迁移是知识库自己的
// 数据目录整理（它知道自己的目录、自己的命名规则）。放在插件 Start 里，
// "先迁移再扫盘"就是两行顺序，不依赖任何跨模块排序。
//
// ★ 判据：删掉 kernelmod/knowledge 之后，`FacMigrated` 在仓库里零消费。
//   "顺序约束藏在依赖图里"被换成了"顺序约束在同一函数内" —— 后者才看得见。

import "log"

// migrateDefaultApply = false ⇒ 启动时只扫描并报告，不改名。
//
// 为什么不自动改：改名是不可逆的盘上动作，且"哪些名字算非法"取决于
// 命名规则演化。报告模式把决定权交给运维（homed-kb-migrate -apply）。
const migrateDefaultApply = false

// maxRenamePerRun 限制单次重命名数：避免启动阶段被一次大迁移拖住。
const maxRenamePerRun = 200

// migrateRoot 做一次迁移扫描（在 Store.Start **之前**调用）。
func migrateRoot(root string) {
	items, err := PlanMigration(root)
	if err != nil {
		log.Printf("[knowledge] migration scan failed (skipping): %v", err)
		return
	}
	need, illegal := 0, 0
	for _, it := range items {
		if it.Illegal {
			illegal++
		} else if it.NewName != "" {
			need++
		}
	}
	if need == 0 && illegal == 0 {
		return
	}
	if illegal > 0 {
		log.Printf("[knowledge] migration: %d illegal names (contain .. / dot segments / hidden segments), "+
			"writes and deletes refused, manual handling required", illegal)
	}
	if need == 0 {
		return
	}
	log.Printf("[knowledge] migration: %d/%d directory names need normalization (e.g. %s -> %s)",
		need, len(items), items[0].OldName, items[0].NewName)
	if !migrateDefaultApply {
		log.Printf("[knowledge] migration: report-only mode. After reviewing the list run: "+
			"homed-kb-migrate -root %s -apply", root)
		return
	}
	if need > maxRenamePerRun {
		log.Printf("[knowledge] migration: %d renames exceed per-run limit %d, processing only the first %d",
			need, maxRenamePerRun, maxRenamePerRun)
	}
	applied, failed := ApplyMigration(root, items, maxRenamePerRun)
	log.Printf("[knowledge] migration complete: %d succeeded, %d failed", applied, failed)
}
