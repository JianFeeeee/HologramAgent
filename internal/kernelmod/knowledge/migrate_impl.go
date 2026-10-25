package knowledge

import (
	"log"

	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
)

// runKnowledgeMigration 是本迁移的真实实现（原 cmd/homed 的整块平移：
// 语句、日志文本、错误语义不变，只把 cfg 参数换成 root）。
func runKnowledgeMigration(root string) {
	const (
		// defaultApply = false ⇒ 启动时只扫描并报告，不改名。
		defaultApply = false
		// maxRenamePerRun 限制单次重命名数：给失控的目录规模设上限，
		// 避免启动阶段被一次大迁移拖住。
		maxRenamePerRun = 200
	)
	items, err := knowledge.PlanMigration(root)
	if err != nil {
		log.Printf("[homed] knowledge base migration scan failed (skipping): %v", err)
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
		log.Printf("[homed] knowledge base migration: %d illegal names (contain .. / dot segments / hidden segments), writes and deletes refused, manual handling required", illegal)
	}
	if need == 0 {
		return
	}
	log.Printf("[homed] knowledge base migration: %d/%d directory names need normalization (e.g. %s -> %s)", need, len(items),
		items[0].OldName, items[0].NewName)
	if !defaultApply {
		log.Printf("[homed] knowledge base migration: report-only mode. After reviewing the list run: homed-kb-migrate -root %s -apply", root)
		return
	}
	if need > maxRenamePerRun {
		log.Printf("[homed] knowledge base migration: %d renames exceed per-run limit %d, processing only the first %d",
			need, maxRenamePerRun, maxRenamePerRun)
	}
	applied, failed := knowledge.ApplyMigration(root, items, maxRenamePerRun)
	log.Printf("[homed] knowledge base migration complete: %d succeeded, %d failed", applied, failed)
}
