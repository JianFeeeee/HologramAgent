package skillmgr

// 内核自带 skill 的**描述必须能整条进索引**。
//
// ## 为什么这是判据而不是"注意一下"
//
// `SkillIndex()` 每行截断到 80 runes（见 truncateDesc）。内置 skill 的
// 描述一旦超过 80，尾巴会被切掉 —— 而尾巴恰好是"什么时候该读它"，
// 于是模型只看到半句机制名，不知道何时适用，也就不会去 skill_info。
//
// 实测（2026-10-26）：四条内置描述分别是 94/96/106/108 字符，
// **全部**被截断。这正是用户说的"要巧妙切分 skill 文本"——
// 索引行要自带"机制 + 最关键那条规则 + 何时展开"。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maxIndexRunes 与 truncateDesc 的上限保持一致（改了这里也必须改那里）。
const maxIndexRunes = 80

func TestBuiltinSkillDescriptionsFitSkillIndex(t *testing.T) {
	dirs, err := os.ReadDir(builtinSkillDir)
	if err != nil {
		t.Fatalf("读不到内置 skill 目录 %s: %v", builtinSkillDir, err)
	}
	checked := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		path := filepath.Join(builtinSkillDir, d.Name(), skillFileName)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读不到 %s: %v", path, err)
		}
		var desc string
		for _, ln := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(ln, "description:") {
				desc = strings.TrimSpace(strings.TrimPrefix(ln, "description:"))
				break
			}
		}
		if desc == "" {
			t.Fatalf("%s 没有 description（索引行会是空的，模型看不到它能干什么）", path)
		}
		if n := len([]rune(desc)); n > maxIndexRunes {
			t.Errorf("★ %s 的 description 有 %d 字符（上限 %d）—— "+
				"进索引会被截断，尾巴（何时该读）会丢：\n  %q",
				d.Name(), n, maxIndexRunes, desc)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("没有检查到任何内置 skill —— 目录结构变了？")
	}
}
