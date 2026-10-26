package skillmgr

// 内核自带的 skill：把**技术机制**写进 skill，由 skillmgr 注入，而不是塞进
// 系统提示词。
//
// ## 为什么（用户 2026-10-26 的两次裁定）
//
//  1. 「系统提示词把机制讲得太深太技术化，导致 agent 说话没人味」——
//     机制留在提示词里，模型会连**语气**一起模仿：回复变成说明书腔。
//  2. 「技术相关下沉到 skill，由 skillmgr 自行管理注入」——
//     提示词只留"必须做什么"，"怎么实现"由 skill 讲。
//
// 所以提示词里只剩一两句行为约束，机制在下面这四个 skill 正文里：
//
//	kernel-memory      记忆分层与 coldcontext_query 的回填语义
//	kernel-output      同步/异步通道怎么真正送达用户
//	kernel-tool-order  一次回复里多工具调用的调度与例外
//	kernel-plugins     按需插件怎么发现与拉起
//
// ## 为什么内嵌在二进制里，而不是让用户自己装
//
// 它们是**内核行为**的一部分：提示词引用了它们，就必须保证它们一定存在。
// 若依赖外部安装，用户删掉/改名后提示词会指向不存在的 skill —— 那比不写更糟。
//
// ## 落盘纪律：不覆盖用户版本
//
// 只在目标目录**不存在**时写入（与 adapters 的 `.bundled` 语义一致）。
// 用户改过的同名 skill 一律保留：内核不该覆盖人的修改。

import (
	"embed"
	"io/fs"
	"log"
	"os"
	"path/filepath"
)

//go:embed builtin
var builtinSkills embed.FS

// builtinSkillDir 是内嵌 skill 的根目录名。
const builtinSkillDir = "builtin"

// skillFileName 与 plugin.LoadSKILL 认的文件名一致。
const skillFileName = "SKILL.md"

// materializeBuiltinSkills 把内嵌 skill 落到 skillsDir，返回新写入的数量。
//
// 已存在的目录**不覆盖**：那是用户可能改过的版本。返回 0 表示无需写入。
func materializeBuiltinSkills(skillsDir string) int {
	if skillsDir == "" {
		return 0
	}
	entries, err := builtinSkills.ReadDir(builtinSkillDir)
	if err != nil {
		log.Printf("[skillmgr] read embedded skills: %v", err)
		return 0
	}
	written := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		dstDir := filepath.Join(skillsDir, name)
		if _, statErr := os.Stat(dstDir); statErr == nil {
			continue // 已存在（用户版本或上次落盘）：不覆盖
		}
		data, readErr := fs.ReadFile(builtinSkills, builtinSkillDir+"/"+name+"/"+skillFileName)
		if readErr != nil {
			log.Printf("[skillmgr] embedded skill %s: %v", name, readErr)
			continue
		}
		if mkErr := os.MkdirAll(dstDir, 0755); mkErr != nil {
			log.Printf("[skillmgr] create %s: %v", dstDir, mkErr)
			continue
		}
		if wErr := os.WriteFile(filepath.Join(dstDir, skillFileName), data, 0644); wErr != nil {
			log.Printf("[skillmgr] write %s: %v", dstDir, wErr)
			continue
		}
		written++
	}
	if written > 0 {
		log.Printf("[skillmgr] materialized %d kernel skills (mechanics live here, not in the system prompt)", written)
	}
	return written
}
