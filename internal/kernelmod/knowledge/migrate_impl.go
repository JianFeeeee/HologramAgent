package knowledge

import (
	"log"

	"github.com/JianFeeeee/HomeAgent/internal/knowledge"
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
		log.Printf("[homed] 知识库迁移扫描失败（跳过）: %v", err)
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
		log.Printf("[homed] 知识库迁移：%d 条名称非法（含 .. / 点段 / 隐藏段），写入与删除均已拒绝，需人工处理", illegal)
	}
	if need == 0 {
		return
	}
	log.Printf("[homed] 知识库迁移：%d/%d 条目录名待规范化（例：%s → %s）", need, len(items),
		items[0].OldName, items[0].NewName)
	if !defaultApply {
		log.Printf("[homed] 知识库迁移：当前为只报告模式。确认清单后执行：homed-kb-migrate -root %s -apply", root)
		return
	}
	if need > maxRenamePerRun {
		log.Printf("[homed] 知识库迁移：需改名 %d 条超过单次上限 %d，本次只处理前 %d 条",
			need, maxRenamePerRun, maxRenamePerRun)
	}
	applied, failed := knowledge.ApplyMigration(root, items, maxRenamePerRun)
	log.Printf("[homed] 知识库迁移完成：成功 %d，失败 %d", applied, failed)
}
