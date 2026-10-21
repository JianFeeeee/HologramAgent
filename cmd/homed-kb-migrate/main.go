// Command homed-kb-migrate 迁移存量知识库的目录名到规范名。
//
// 背景：旧版 Add 对名字**整串** sanitize、对路径**逐段** sanitize，
// 于是知识名（内存键 / LLM 可见的名字）与盘上目录从第一次落盘起就对不上。
// 典型残留：
//
//	tech/_go_/note    分类段内的空格未被 TrimSpace 掉
//	Tech/Upper        未小写化
//	a/b with space    空格未替换成下划线
//
// 迁移把它们重命名到规范名，使三者一致。
//
// 安全设计：
//  1. **默认只报告**（-apply 才真改名）。批量 os.Rename 不可逆。
//  2. 检出目标名冲突则**整批拒绝**，不做部分迁移——半迁移状态比不迁移更难收拾。
//  3. 单条失败不中断整体，最后统一报告；执行前再查一次目标越界。
//
// 与 homed 启动时的关系：`homed` 启动会调同一个 PlanMigration 并**只报告**
// （见 cmd/homed/bootstrap.go 的 defaultApply）。本命令是人工确认后真正执行
// 的那一步。两者共用 internal/knowledge 里的同一份实现，避免口径漂移。
//
// 用法：
//
//	homed-kb-migrate -root /data/homeagent/knowledge           # 报告
//	homed-kb-migrate -root /data/homeagent/knowledge -apply    # 执行
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/JianFeeeee/HologramAgent/internal/knowledge"
)

func main() {
	root := flag.String("root", "", "知识库根目录（必填）")
	apply := flag.Bool("apply", false, "真正执行重命名（缺省只报告）")
	dryRun := flag.Bool("dry-run", false, "只报告（显式写法，与默认相同）")
	limit := flag.Int("limit", 0, "单次最多改名条数，0 = 不限")
	flag.Parse()

	if *root == "" {
		fmt.Fprintln(os.Stderr, "错误：必须指定 -root <知识库根目录>")
		flag.Usage()
		os.Exit(2)
	}
	if *apply && *dryRun {
		fmt.Fprintln(os.Stderr, "错误：-apply 与 -dry-run 互斥")
		os.Exit(2)
	}

	abs, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：%v\n", err)
		os.Exit(1)
	}
	abs = filepath.Clean(abs)

	items, err := knowledge.PlanMigration(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误：无法读取 %s：%v\n", abs, err)
		os.Exit(1)
	}
	if len(items) == 0 {
		fmt.Printf("未发现任何知识条目（%s）\n", abs)
		return
	}

	var need, illegal int
	for _, it := range items {
		switch {
		case it.Illegal:
			illegal++
			fmt.Printf("  [非法] %-40s 含 .. / 点段 / 隐藏段；写入与删除均已拒绝，需人工处理\n", it.OldName)
		case it.NewName != "":
			need++
			fmt.Printf("  [迁移] %-40s → %s\n", it.OldName, it.NewName)
		}
	}
	fmt.Printf("\n共 %d 条：需迁移 %d，已规范 %d，非法 %d\n",
		len(items), need, len(items)-need-illegal, illegal)

	if !*apply {
		if need == 0 {
			fmt.Println("\n无需迁移。加 -apply 不会改变任何东西。")
			return
		}
		fmt.Println("\n这是报告（未改动任何文件）。确认无误后加 -apply 执行。")
		return
	}

	applied, failed := knowledge.ApplyMigration(abs, items, *limit)
	fmt.Printf("\n迁移完成：成功 %d，失败 %d\n", applied, failed)
	if failed > 0 {
		fmt.Fprintln(os.Stderr, "存在失败项。若为名称冲突，请先人工处理冲突的目录再重跑。")
		os.Exit(1)
	}
}
