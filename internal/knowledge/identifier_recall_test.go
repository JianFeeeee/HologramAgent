package knowledge

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIdentifierQueryBeatsVolume 复现并钉住 2026-10-31 报的那个故障形态：
//
//	库里 79% 的条目是某一种文档（实测 196 条里 154 条 cangjie），
//	查一个**只在目标条目里出现一次**的英文标识符，结果被那批文档淹没，
//	目标条目一次都进不了前五。
//
// 这里用退化路径（不注入稠密空间）来验，因为它只依赖词法路 —— 而词法路
// 正是当年被误判为"对标识符失效"的那条路。若分词器退回 jieba 单字符切分，
// 本判据会红（噪声文档里全是英文字母，token 重叠率足以盖过目标）。
func TestIdentifierQueryBeatsVolume(t *testing.T) {
	root := t.TempDir()

	// 目标条目：标识符只出现一次，符合真实文档形态（2872 字节里出现一次）。
	target := "# 内核装配\n\n回滚前的清理由 `PruneOldBackups` 负责，保留最近 3 个备份。\n"
	writeKB(t, root, "hologramagent_kernel_assembly_upgrade", target)

	// 噪声：60 篇英文词密集的文档，模拟 cangjie 那批（含大量 old/path/strip 之类
	// 普通英文词，正是误命中来源）。
	filler := strings.Repeat("The old path is stripped and the old file path is cleaned. "+
		"Use the strip command with the old path argument. ", 8)
	for i := 0; i < 60; i++ {
		writeKB(t, root, fmt.Sprintf("cangjie/dev-guide/noise_%02d", i), filler)
	}

	s := NewStore(root)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	for _, q := range []string{"PruneOldBackups", "pruneoldbackups"} {
		res := s.Search(q, 5)
		if len(res) == 0 {
			t.Fatalf("查询 %q 无任何结果", q)
		}
		if res[0].Name != "hologramagent_kernel_assembly_upgrade" {
			var names []string
			for _, r := range res {
				names = append(names, r.Name)
			}
			t.Errorf("查询 %q 的 top1 是 %q，应为目标条目。\n  实际前五: %v\n"+
				"  常见原因：分词器把标识符切成单字符（见 identtoken.go）",
				q, res[0].Name, names)
		}
	}
}

// TestSeparatorIdentifierNotBuried 钉住带分隔符的标识符（`strip_path`）。
//
// 与上一条分开的原因：带 `_` 的标识符会被拆成部件（strip / path），
// 而这两个都是普通英文词 —— 部件拆分既是命中来源，也是误命中来源。
// 所以这条单独验：整词命中必须压过部件噪声。
func TestSeparatorIdentifierNotBuried(t *testing.T) {
	root := t.TempDir()
	writeKB(t, root, "changelog_v1.4.0",
		"# 更新日志\n\n- 通用反向代理：路径挂载 `strip_path`，按子域反代出去。\n")
	filler := strings.Repeat("Path handling: strip the path prefix, then strip again. ", 10)
	for i := 0; i < 60; i++ {
		writeKB(t, root, fmt.Sprintf("cangjie/deploy/page_%02d", i), filler)
	}

	s := NewStore(root)
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	res := s.Search("strip_path", 5)
	if len(res) == 0 {
		t.Fatal("无结果")
	}
	if res[0].Name != "changelog_v1.4.0" {
		var names []string
		for _, r := range res {
			names = append(names, r.Name)
		}
		t.Errorf("top1 是 %q，应为 changelog_v1.4.0（实际前五 %v）", res[0].Name, names)
	}
}

// writeKB 在 root/<name>/content.md 写一条知识。
func writeKB(t *testing.T, root, name, content string) {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "content.md"), []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}
