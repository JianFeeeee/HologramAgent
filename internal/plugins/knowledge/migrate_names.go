package knowledgeplugin

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// 存量目录名迁移。
//
// 背景：旧版 Add 对名字**整串** sanitize 却对路径**逐段** sanitize，
// 于是知识名（内存键 / LLM 可见的名字）与盘上目录从第一次落盘起就对不上。
// 典型残留：
//
//	tech/_go_/note   分类段内的空格未被 TrimSpace 掉
//	Tech/Upper       未小写化
//	a/b with space   空格未替换成下划线
//
// 修复后 normalizeName 要求二者逐字一致，故需要一次性把存量目录改名。
//
// 本文件的函数刻意**不依赖 Store 实例**：迁移要在 store 扫盘之前跑，
// 且必须能在不带任何索引/内存状态的前提下作用于任意知识根。

// MigrationItem 是一条待迁移（或已规范/非法）的知识条目。
type MigrationItem struct {
	OldName string
	NewName string // 空串表示已规范，无需改动
	Illegal bool   // 名称含 .. / 点段 / 隐藏段：拒绝读写，需人工处理
}

// PlanMigration 扫描知识根，给出规范名迁移清单。**只读，不修改任何文件。**
//
// 遍历口径与 scanDir 一致：含 content.md 的目录是条目，否则是分类目录、
// 继续递归。单个目录不可读时跳过该目录而不是整体失败——一个坏目录
// 不该让整次迁移计划落空。
func PlanMigration(root string) ([]MigrationItem, error) {
	root = filepath.Clean(root)
	var out []MigrationItem

	var walk func(dirName string)
	walk = func(dirName string) {
		dir := filepath.Join(root, filepath.FromSlash(dirName))
		if _, err := os.Stat(filepath.Join(dir, "content.md")); err == nil {
			it := MigrationItem{OldName: dirName}
			norm, err := normalizeName(dirName)
			switch {
			case err != nil:
				it.Illegal = true
			case norm != dirName:
				it.NewName = norm
			}
			out = append(out, it)
			return
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				walk(dirName + "/" + e.Name())
			}
		}
	}

	ents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			walk(e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OldName < out[j].OldName })
	return out, nil
}

// migrationConflicts 找出会互相覆盖的目标名（含目标已存在于盘上的情况）。
//
// 不改名是最好的选择：一旦"前一条改好了、后一条失败"就成了半迁移状态，
// 比完全没迁移更难收拾。所以检测到冲突就整批拒绝。
func migrationConflicts(root string, items []MigrationItem) []string {
	byTarget := map[string][]string{}
	for _, it := range items {
		if it.NewName != "" {
			byTarget[it.NewName] = append(byTarget[it.NewName], it.OldName)
		}
	}
	var conflicts []string
	for name, srcs := range byTarget {
		if len(srcs) > 1 {
			conflicts = append(conflicts, fmt.Sprintf("%s ← %s", name, strings.Join(srcs, ", ")))
		}
		// 目标已在盘上（既有条目或分类目录）也算冲突
		full := filepath.Join(root, filepath.FromSlash(name))
		if _, err := os.Stat(full); err == nil {
			conflicts = append(conflicts, name+" ← 盘上已存在同名路径")
		}
	}
	sort.Strings(conflicts)
	return conflicts
}

// ApplyMigration 执行迁移计划，返回成功/失败条数。
//
// limit <= 0 表示不限制。执行前会重新做一次冲突检测（计划生成与执行
// 之间可能有人改过盘上状态），有冲突则一条都不改。
func ApplyMigration(root string, items []MigrationItem, limit int) (applied, failed int) {
	root = filepath.Clean(root)
	if cs := migrationConflicts(root, items); len(cs) > 0 {
		return 0, len(items) // 全部算失败，调用方据 failed 判定中止
	}

	// 深的目标先改：父子目录同时重命名时，先动子避免父被移走导致子路径失效。
	todo := make([]MigrationItem, 0, len(items))
	for _, it := range items {
		if it.NewName != "" && !it.Illegal {
			todo = append(todo, it)
		}
	}
	sort.Slice(todo, func(i, j int) bool {
		di, dj := strings.Count(todo[i].NewName, "/"), strings.Count(todo[j].NewName, "/")
		if di != dj {
			return di > dj
		}
		// 同深度按旧名倒序：同层内避免「父先变子还在」的瞬时状态
		return todo[i].OldName > todo[j].OldName
	})

	for i, it := range todo {
		if limit > 0 && i >= limit {
			break
		}
		src := filepath.Join(root, filepath.FromSlash(it.OldName))
		dst := filepath.Join(root, filepath.FromSlash(it.NewName))
		// 硬保险：目标必须在知识根内
		if !strings.HasPrefix(filepath.Clean(dst), root+string(filepath.Separator)) {
			failed++
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			failed++
			continue
		}
		if err := os.Rename(src, dst); err != nil {
			failed++
			continue
		}
		applied++
	}
	return applied, failed
}
